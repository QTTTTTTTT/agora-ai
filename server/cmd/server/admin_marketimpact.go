// admin_marketimpact.go — admin REST surface for the S6.2
// market-impact / size-aware slippage calibration.
//
// Endpoints
//
//   GET    /api/admin/marketimpact/instruments           list rows (?market, ?asset_class, ?limit, ?offset)
//   GET    /api/admin/marketimpact/instruments/{key}     one row
//   PUT    /api/admin/marketimpact/instruments/{key}     upsert row
//   DELETE /api/admin/marketimpact/instruments/{key}     remove row
//   POST   /api/admin/marketimpact/preview               run engine on a probe (no DB write)
//   GET    /api/admin/marketimpact/cache                 return cache stats (size, last refresh)
//   POST   /api/admin/marketimpact/cache/refresh         force a Refresh from DB
//
// Conventions match S6.1: writes go through h.requireAdmin,
// invalidate the in-memory cache so the simulator picks up the
// change immediately, audit-log the mutation, and bump
// fundai_marketimpact_events_total{event="admin_*"}.

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/marketimpact"
)

// liquidityWire is the on-wire shape for one calibration row.
// Pointer fields render as omitempty so the UI can tell "no
// calibration set" apart from "calibration = 0".
type liquidityWire struct {
	InstrumentKey      string   `json:"instrument_key"`
	Symbol             string   `json:"symbol"`
	Market             string   `json:"market"`
	AssetClass         string   `json:"asset_class"`
	ADVShares          *float64 `json:"adv_shares,omitempty"`
	ADVNotional        *float64 `json:"adv_notional,omitempty"`
	ADVWindowDays      int      `json:"adv_window_days"`
	DailyVolatility    *float64 `json:"daily_volatility,omitempty"`
	ImpactCoefficient  float64  `json:"impact_coefficient"`
	ImpactExponent     float64  `json:"impact_exponent"`
	MinSlippageBps     float64  `json:"min_slippage_bps"`
	MaxSlippageBps     float64  `json:"max_slippage_bps"`
	LastCalibratedAt   string   `json:"last_calibrated_at,omitempty"`
	CalibrationSource  string   `json:"calibration_source"`
	Note               string   `json:"note,omitempty"`
	UpdatedAt          string   `json:"updated_at"`
}

func projectLiquidity(l marketimpact.Liquidity) liquidityWire {
	out := liquidityWire{
		InstrumentKey:     l.InstrumentKey,
		Symbol:            l.Symbol,
		Market:            l.Market,
		AssetClass:        l.AssetClass,
		ADVShares:         l.ADVShares,
		ADVNotional:       l.ADVNotional,
		ADVWindowDays:     l.ADVWindowDays,
		DailyVolatility:   l.DailyVolatility,
		ImpactCoefficient: l.ImpactCoefficient,
		ImpactExponent:    l.ImpactExponent,
		MinSlippageBps:    l.MinSlippageBps,
		MaxSlippageBps:    l.MaxSlippageBps,
		CalibrationSource: l.CalibrationSource,
		Note:              l.Note,
		UpdatedAt:         l.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if l.LastCalibratedAt != nil {
		out.LastCalibratedAt = l.LastCalibratedAt.UTC().Format(time.RFC3339Nano)
	}
	return out
}

// estimateWire is the engine output for the preview endpoint.
type estimateWire struct {
	AdverseBps      float64 `json:"adverse_bps"`
	TempImpactBps   float64 `json:"temp_impact_bps"`
	PermImpactBps   float64 `json:"perm_impact_bps,omitempty"`
	UsedDefaults    bool    `json:"used_defaults"`
	UsedADVFallback bool    `json:"used_adv_fallback"`
	Reason          string  `json:"reason,omitempty"`
	DetectorVersion string  `json:"detector_version,omitempty"`
	AppliedAt       string  `json:"applied_at,omitempty"`
}

func projectEstimate(e marketimpact.Estimate) estimateWire {
	return estimateWire{
		AdverseBps:      e.AdverseBps,
		TempImpactBps:   e.TempImpactBps,
		PermImpactBps:   e.PermImpactBps,
		UsedDefaults:    e.UsedDefaults,
		UsedADVFallback: e.UsedADVFallback,
		Reason:          e.Reason,
		DetectorVersion: e.DetectorVersion,
		AppliedAt:       e.AppliedAt.UTC().Format(time.RFC3339Nano),
	}
}

// registerMarketImpactAdminRoutes wires the routes. Called
// from registerAdminRoutes.
func (h *adminHandler) registerMarketImpactAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/marketimpact/instruments", h.handleListMarketImpactInstruments)
	mux.HandleFunc("GET /api/admin/marketimpact/instruments/{key}", h.handleGetMarketImpactInstrument)
	mux.HandleFunc("PUT /api/admin/marketimpact/instruments/{key}", h.handleUpsertMarketImpactInstrument)
	mux.HandleFunc("DELETE /api/admin/marketimpact/instruments/{key}", h.handleDeleteMarketImpactInstrument)
	mux.HandleFunc("POST /api/admin/marketimpact/preview", h.handleMarketImpactPreview)
	mux.HandleFunc("GET /api/admin/marketimpact/cache", h.handleMarketImpactCacheStats)
	mux.HandleFunc("POST /api/admin/marketimpact/cache/refresh", h.handleMarketImpactCacheRefresh)
}

// ----- list -----

func (h *adminHandler) handleListMarketImpactInstruments(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact not wired"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, err := h.marketImpactRepo.List(r.Context(), marketimpact.ListFilter{
		Market:     strings.TrimSpace(q.Get("market")),
		AssetClass: strings.TrimSpace(q.Get("asset_class")),
		Limit:      limit,
		Offset:     offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]liquidityWire, 0, len(rows))
	for _, l := range rows {
		out = append(out, projectLiquidity(l))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"instruments": out,
		"total":       len(out),
	})
}

func (h *adminHandler) handleGetMarketImpactInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact not wired"))
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	got, err := h.marketImpactRepo.GetByKey(r.Context(), key)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if got == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no calibration row"))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectLiquidity(*got)})
}

// ----- upsert -----

type upsertMarketImpactRequest struct {
	Symbol             string   `json:"symbol"`
	Market             string   `json:"market"`
	AssetClass         string   `json:"asset_class,omitempty"`
	ADVShares          *float64 `json:"adv_shares,omitempty"`
	ADVNotional        *float64 `json:"adv_notional,omitempty"`
	ADVWindowDays      *int     `json:"adv_window_days,omitempty"`
	DailyVolatility    *float64 `json:"daily_volatility,omitempty"`
	ImpactCoefficient  *float64 `json:"impact_coefficient,omitempty"`
	ImpactExponent     *float64 `json:"impact_exponent,omitempty"`
	MinSlippageBps     *float64 `json:"min_slippage_bps,omitempty"`
	MaxSlippageBps     *float64 `json:"max_slippage_bps,omitempty"`
	LastCalibratedAt   string   `json:"last_calibrated_at,omitempty"`
	CalibrationSource  string   `json:"calibration_source,omitempty"`
	Note               string   `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertMarketImpactInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact not wired"))
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	var req upsertMarketImpactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	params := marketimpact.UpsertParams{
		InstrumentKey:     key,
		Symbol:            strings.TrimSpace(req.Symbol),
		Market:            strings.TrimSpace(req.Market),
		AssetClass:        strings.ToLower(strings.TrimSpace(req.AssetClass)),
		ADVShares:         req.ADVShares,
		ADVNotional:       req.ADVNotional,
		ADVWindowDays:     req.ADVWindowDays,
		DailyVolatility:   req.DailyVolatility,
		ImpactCoefficient: req.ImpactCoefficient,
		ImpactExponent:    req.ImpactExponent,
		MinSlippageBps:    req.MinSlippageBps,
		MaxSlippageBps:    req.MaxSlippageBps,
		CalibrationSource: strings.TrimSpace(req.CalibrationSource),
		Note:              strings.TrimSpace(req.Note),
		UpdatedBy:         userID,
	}
	if t, ok := parseTimestampPtr(req.LastCalibratedAt); ok {
		params.LastCalibratedAt = t
	}
	got, err := h.marketImpactRepo.Upsert(r.Context(), params)
	if err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	// Apply to the in-memory cache so the simulator's slippage
	// engine picks up the change without waiting for the next
	// periodic Refresh.
	if h.marketImpactCache != nil && got != nil {
		h.marketImpactCache.ApplyChange(key, got)
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketimpact.upsert",
			TargetType:  "instrument_liquidity",
			TargetID:    key,
			After: map[string]any{
				"adv_shares":         params.ADVShares,
				"daily_volatility":   params.DailyVolatility,
				"impact_coefficient": params.ImpactCoefficient,
				"impact_exponent":    params.ImpactExponent,
				"min_slippage_bps":   params.MinSlippageBps,
				"max_slippage_bps":   params.MaxSlippageBps,
				"calibration_source": params.CalibrationSource,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketImpactEvent("admin_upsert")
	}
	if got == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectLiquidity(*got)})
}

// ----- delete -----

func (h *adminHandler) handleDeleteMarketImpactInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact not wired"))
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.marketImpactRepo.Delete(r.Context(), key); err != nil {
		// sql.ErrNoRows comes back if the row never existed; we
		// return 404 instead of a generic 500.
		if errors.Is(err, errMarketImpactNotFound) || strings.Contains(err.Error(), "no rows") {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no calibration row"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.marketImpactCache != nil {
		h.marketImpactCache.ApplyChange(key, nil)
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketimpact.delete",
			TargetType:  "instrument_liquidity",
			TargetID:    key,
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketImpactEvent("admin_delete")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----- preview -----
//
// Operator types in instrument + side + quantity + reference
// price; we run the same engine the simulator runs and return
// the bps. Lets risk teams sanity-check an instrument's
// calibration without placing a real order.

type previewMarketImpactRequest struct {
	InstrumentKey string  `json:"instrument_key"`
	Symbol        string  `json:"symbol,omitempty"`
	AssetClass    string  `json:"asset_class,omitempty"`
	Side          string  `json:"side"`
	Quantity      float64 `json:"quantity"`
	ReferencePx   float64 `json:"reference_price"`
}

func (h *adminHandler) handleMarketImpactPreview(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactAdapter == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact not wired"))
		return
	}
	var req previewMarketImpactRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.InstrumentKey) == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "instrument_key required"))
		return
	}
	if req.Quantity <= 0 || req.ReferencePx <= 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "quantity and reference_price must be > 0"))
		return
	}
	side := strings.ToLower(strings.TrimSpace(req.Side))
	if side != "buy" && side != "sell" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "side must be buy or sell"))
		return
	}
	symbol := strings.TrimSpace(req.Symbol)
	if symbol == "" {
		symbol = req.InstrumentKey
	}
	asset := strings.ToLower(strings.TrimSpace(req.AssetClass))
	if asset == "" {
		asset = "equity"
	}
	probe := marketimpact.OrderProbe{
		InstrumentKey: strings.TrimSpace(req.InstrumentKey),
		Symbol:        symbol,
		AssetClass:    asset,
		Side:          side,
		Quantity:      req.Quantity,
		ReferencePx:   req.ReferencePx,
	}
	est := h.marketImpactAdapter.EstimateForProbe(probe)
	// Show the operator the implied fill price too — easier
	// gut-check than reading bps in isolation.
	implied := marketimpact.ApplyAdverse(req.ReferencePx, side, est.AdverseBps)
	notional := req.Quantity * req.ReferencePx
	notionalCost := req.Quantity * (implied - req.ReferencePx)
	if side == "sell" {
		notionalCost = -notionalCost
	}
	if h.metrics != nil {
		h.metrics.RecordMarketImpactEvent("admin_preview")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"estimate":       projectEstimate(est),
		"reference_px":   req.ReferencePx,
		"implied_fill":   implied,
		"notional":       notional,
		"impact_cost":    notionalCost,
		"impact_cost_pct": pctOrZero(notionalCost, notional),
	})
}

// pctOrZero is a /-by-zero-safe percentage helper.
func pctOrZero(num, denom float64) float64 {
	if denom == 0 {
		return 0
	}
	return num / denom * 100
}

// ----- cache stats / refresh -----

func (h *adminHandler) handleMarketImpactCacheStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactCache == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact cache not wired"))
		return
	}
	last := h.marketImpactCache.LastRefresh()
	out := map[string]any{
		"size": h.marketImpactCache.Size(),
	}
	if !last.IsZero() {
		out["last_refresh"] = last.UTC().Format(time.RFC3339Nano)
	}
	writeOrderActionJSON(w, http.StatusOK, out)
}

func (h *adminHandler) handleMarketImpactCacheRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.marketImpactCache == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "marketimpact cache not wired"))
		return
	}
	if err := h.marketImpactCache.Refresh(r.Context()); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("refresh_failed", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordMarketImpactEvent("admin_cache_refresh")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"ok":   true,
		"size": h.marketImpactCache.Size(),
	})
}

// errMarketImpactNotFound is a sentinel returned by helpers
// when the underlying repo signals "no rows". We compare with
// errors.Is so the caller can branch without depending on
// database/sql.
var errMarketImpactNotFound = fmt.Errorf("marketimpact: not found")
