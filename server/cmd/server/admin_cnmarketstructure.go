// admin_cnmarketstructure.go — admin-only probe + health endpoint
// for the A-share market structure provider chain. Lets operators
// verify the akshare-MCP is reachable and inspect circuit health
// without tailing logs.
//
// Endpoints (all gated by requireAdmin):
//
//   GET /api/admin/cnmarketstructure/health
//     Returns per-provider counters from the registry's health
//     tracker (success / failure counts, last error, open-until,
//     EMA latency).
//
//   GET /api/admin/cnmarketstructure/probe?symbol=600519
//     Triggers a live probe through the cache (so it doubles as a
//     cache warmer) and returns the full IntradaySnapshot +
//     MarketRegime + DragonTiger entries for the supplied symbol.
//     Useful for "does akshare even know about 600519 today?"
//     debugging.

package main

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/cnmarketstructure"
)

// registerCNMarketStructureAdminRoutes mounts the two admin endpoints
// on the supplied mux. No-op when no provider is wired so a degraded
// boot (CN_MARKETSTRUCTURE_DISABLED=1) won't 500 on probe attempts.
func (h *adminHandler) registerCNMarketStructureAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/cnmarketstructure/health", h.handleCNStructHealth)
	mux.HandleFunc("GET /api/admin/cnmarketstructure/probe", h.handleCNStructProbe)
}

type cnstructHealthResponse struct {
	Configured     bool                                             `json:"configured"`
	ProviderNames  []string                                         `json:"provider_names,omitempty"`
	ProviderHealth map[string]cnmarketstructure.ProviderHealthStats `json:"provider_health,omitempty"`
	Note           string                                           `json:"note,omitempty"`
}

func (h *adminHandler) handleCNStructHealth(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	resp := cnstructHealthResponse{}
	if h.cnStructRegistry == nil {
		resp.Configured = false
		resp.Note = "CN_MARKETSTRUCTURE_DISABLED=1 or no AKSHARE_CNSTRUCT_URL / AKSHARE_OHLC_URL set"
		writeJSON(w, http.StatusOK, resp)
		return
	}
	resp.Configured = true
	resp.ProviderNames = h.cnStructRegistry.Names()
	resp.ProviderHealth = h.cnStructRegistry.HealthStats()
	writeJSON(w, http.StatusOK, resp)
}

type cnstructProbeResponse struct {
	Symbol         string                               `json:"symbol"`
	AsOf           time.Time                            `json:"asof"`
	Intraday       *cnmarketstructure.IntradaySnapshot  `json:"intraday,omitempty"`
	IntradayError  string                               `json:"intraday_error,omitempty"`
	MarketRegime   *cnmarketstructure.MarketRegime      `json:"market_regime,omitempty"`
	MarketError    string                               `json:"market_error,omitempty"`
	DragonTiger    []cnmarketstructure.DragonTigerEntry `json:"dragon_tiger,omitempty"`
	DragonError    string                               `json:"dragon_error,omitempty"`
	SectorStrength []cnmarketstructure.SectorStrength   `json:"sector_strength,omitempty"`
	SectorError    string                               `json:"sector_error,omitempty"`
	ElapsedMillis  int64                                `json:"elapsed_ms"`
}

func (h *adminHandler) handleCNStructProbe(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.cnStructProvider == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "cn market structure not configured",
		})
		return
	}
	symbol := strings.TrimSpace(r.URL.Query().Get("symbol"))
	if symbol == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "symbol query param required",
		})
		return
	}
	lookback := 30
	if raw := r.URL.Query().Get("lhb_lookback"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 365 {
			lookback = n
		}
	}
	sectorTopN := 10
	if raw := r.URL.Query().Get("sector_top_n"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 100 {
			sectorTopN = n
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	started := time.Now()
	resp := cnstructProbeResponse{Symbol: symbol, AsOf: started}

	intraday, err := h.cnStructProvider.FetchIntraday(ctx, symbol)
	if err != nil {
		if errors.Is(err, cnmarketstructure.ErrNoData) {
			resp.IntradayError = "no data (symbol may not be in today's pool)"
		} else {
			resp.IntradayError = err.Error()
		}
	} else {
		resp.Intraday = intraday
	}

	regime, err := h.cnStructProvider.FetchMarketRegime(ctx)
	if err != nil {
		resp.MarketError = err.Error()
	} else {
		resp.MarketRegime = regime
	}

	dragon, err := h.cnStructProvider.FetchDragonTiger(ctx, symbol, lookback)
	if err != nil {
		if errors.Is(err, cnmarketstructure.ErrNoData) {
			resp.DragonError = "no billboard appearance in lookback window"
		} else {
			resp.DragonError = err.Error()
		}
	} else {
		resp.DragonTiger = dragon
	}

	sectors, err := h.cnStructProvider.FetchSectorStrength(ctx, sectorTopN)
	if err != nil {
		resp.SectorError = err.Error()
	} else {
		resp.SectorStrength = sectors
	}

	resp.ElapsedMillis = time.Since(started).Milliseconds()
	writeJSON(w, http.StatusOK, resp)
}
