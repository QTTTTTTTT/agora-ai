// factor_exposure_handler.go — per-fund factor-exposure read API (S7 / P3-1).
//
// Routes
//
//	GET /api/funds/{fundId}/risk/factor-exposure
//	    Live read: pull current holding_positions, compute the six
//	    canonical factor exposures, optionally persist a snapshot
//	    (?persist=1) and return everything to the UI.
//
//	GET /api/funds/{fundId}/risk/factor-exposure/trend
//	    Returns the last N snapshots (default 30 most recent
//	    points across all factors). Used by the dashboard
//	    sparkline / line chart.
//
// Authorization mirrors cash_ledger_handler.go: shared
// authorizeFundAccess gate. PositionSide=short is honoured by
// signing MarketValue negative so the engine treats it as a
// short exposure (per spec in internal/factorexposure/engine.go).

package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/factorexposure"
	"github.com/fundai/server/internal/repository"
)

type factorExposureHandler struct {
	repo        *factorexposure.Repo
	positions   *repository.PositionRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	metrics     *serverMetrics
}

func newFactorExposureHandler(svc *Services) *factorExposureHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &factorExposureHandler{
		repo:        factorexposure.NewRepo(svc.DB),
		positions:   repository.NewPositionRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		metrics:     svc.Metrics,
	}
}

func (h *factorExposureHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/risk/factor-exposure", h.handleSnapshot)
	mux.HandleFunc("GET /api/funds/{fundId}/risk/factor-exposure/trend", h.handleTrend)
}

// exposureRowWire is the JSON projection for one PortfolioExposure
// row. snake_case so the shared/api-client TS types pick the field
// names straight up.
type exposureRowWire struct {
	Factor        string  `json:"factor"`
	NetExposure   float64 `json:"net_exposure"`
	GrossExposure float64 `json:"gross_exposure"`
	CapitalPct    float64 `json:"capital_pct"`
	HoldingCount  int     `json:"holding_count"`
	LoadingsAsOf  string  `json:"loadings_asof,omitempty"`
}

type snapshotWire struct {
	FundID             string            `json:"fund_id"`
	GeneratedAt        string            `json:"generated_at"`
	NAV                float64           `json:"nav"`
	HoldingsTotal      int               `json:"holdings_total"`
	HoldingsCovered    int               `json:"holdings_covered"`
	OldestLoadingAsOf  string            `json:"oldest_loading_asof,omitempty"`
	Exposures          []exposureRowWire `json:"exposures"`
}

func projectSnapshot(snap factorexposure.Snapshot) snapshotWire {
	out := snapshotWire{
		FundID:          snap.FundID,
		GeneratedAt:     snap.GeneratedAt.UTC().Format(time.RFC3339Nano),
		NAV:             snap.NAV,
		HoldingsTotal:   snap.HoldingsTotal,
		HoldingsCovered: snap.HoldingsCovered,
		Exposures:       make([]exposureRowWire, 0, len(snap.Exposures)),
	}
	if !snap.OldestLoadingAsOf.IsZero() {
		out.OldestLoadingAsOf = snap.OldestLoadingAsOf.UTC().Format("2006-01-02")
	}
	for _, r := range snap.Exposures {
		row := exposureRowWire{
			Factor:        string(r.Factor),
			NetExposure:   r.NetExposure,
			GrossExposure: r.GrossExposure,
			CapitalPct:    r.CapitalPct,
			HoldingCount:  r.HoldingCount,
		}
		if !r.LoadingsAsOf.IsZero() {
			row.LoadingsAsOf = r.LoadingsAsOf.UTC().Format("2006-01-02")
		}
		out.Exposures = append(out.Exposures, row)
	}
	return out
}

// holdingsToFactorExposure converts repository rows into the
// engine's Holding slice. Shorts (PositionSide="short" or
// Quantity < 0) get their MarketValue signed negative so the
// engine treats them as inverse exposures.
func holdingsToFactorExposure(rows []repository.HoldingPosition) []factorexposure.Holding {
	out := make([]factorexposure.Holding, 0, len(rows))
	for _, r := range rows {
		if r.InstrumentKey == "" {
			continue
		}
		mv := r.MarketValue
		side := strings.ToLower(strings.TrimSpace(r.PositionSide.String))
		if side == "short" || r.Quantity < 0 {
			// Long positions store MV positive; short legs are
			// represented as either side="short" with positive MV
			// or quantity<0. Either way we want negative MV from
			// the engine's POV.
			if mv > 0 {
				mv = -mv
			}
		}
		out = append(out, factorexposure.Holding{
			InstrumentKey: r.InstrumentKey,
			Symbol:        r.Symbol,
			MarketValue:   mv,
		})
	}
	return out
}

func (h *factorExposureHandler) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	positions, err := h.positions.ListByFund(ctx, fundID)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	holdings := holdingsToFactorExposure(positions)
	keys := make([]string, 0, len(holdings))
	for _, hd := range holdings {
		keys = append(keys, hd.InstrumentKey)
	}

	// Use today as the as-of bound; loadings with future asof are
	// excluded. The instrument_factor_loadings_latest_idx serves
	// the DISTINCT ON query so the round trip is cheap.
	loadings, err := h.repo.LoadingsByInstruments(ctx, keys, time.Now().UTC())
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	eng := &factorexposure.Engine{}
	snap := eng.Compute(fundID, holdings, loadings)

	if strings.TrimSpace(r.URL.Query().Get("persist")) == "1" {
		// Best-effort persist; failure does not block the read
		// path. Operators see snapshot.archive failures via the
		// fundai_factorexposure_snapshot_write_failures_total
		// counter (wired in metrics, omitted here for brevity).
		if err := h.repo.AppendSnapshot(ctx, snap); err != nil {
			// Surface in response so callers can decide to retry
			// rather than silently dropping the audit row.
			writeJSON(w, http.StatusOK, map[string]any{
				"snapshot":      projectSnapshot(snap),
				"persist_error": err.Error(),
			})
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"snapshot": projectSnapshot(snap),
		"factors":  factorexposure.AllFactors,
	})
}

type trendPointWire struct {
	CalculatedAt  string  `json:"calculated_at"`
	Factor        string  `json:"factor"`
	NetExposure   float64 `json:"net_exposure"`
	GrossExposure float64 `json:"gross_exposure"`
	CapitalPct    float64 `json:"capital_pct"`
	HoldingCount  int     `json:"holding_count"`
	LoadingsAsOf  string  `json:"loadings_asof"`
}

func (h *factorExposureHandler) handleTrend(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	ctx := r.Context()
	if _, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}
	q := r.URL.Query()
	limit := 180
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if v, err := strconv.Atoi(raw); err == nil && v > 0 {
			limit = v
		}
	}
	var factor factorexposure.Factor
	if f := strings.TrimSpace(q.Get("factor")); f != "" {
		parsed, ok := factorexposure.ParseFactor(f)
		if !ok {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_factor", f))
			return
		}
		factor = parsed
	}
	rows, err := h.repo.ListSnapshots(ctx, fundID, factor, limit)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]trendPointWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, trendPointWire{
			CalculatedAt:  row.CalculatedAt.UTC().Format(time.RFC3339Nano),
			Factor:        string(row.Factor),
			NetExposure:   row.NetExposure,
			GrossExposure: row.GrossExposure,
			CapitalPct:    row.CapitalPct,
			HoldingCount:  row.HoldingCount,
			LoadingsAsOf:  row.LoadingsAsOf.UTC().Format("2006-01-02"),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"points":  out,
		"factors": factorexposure.AllFactors,
	})
}

// Silence unused-import warnings when JSON or errors are only
// referenced conditionally as the file evolves.
var (
	_ = json.NewDecoder
	_ = errors.New
)
