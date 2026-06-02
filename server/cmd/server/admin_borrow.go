// admin_borrow.go — admin REST surface for the S6.4
// securities-borrow stack.
//
// Endpoints
//
//   GET    /api/admin/borrow/rates                  list rate calibrations
//   GET    /api/admin/borrow/rates/{key}            one row
//   POST   /api/admin/borrow/rates                  upsert (or new)
//   DELETE /api/admin/borrow/rates/{key}            hard delete
//   POST   /api/admin/borrow/locate/preview         dry-run a locate decision
//   GET    /api/admin/borrow/locate/events          locate audit log
//   GET    /api/admin/borrow/ledger                 daily fee ledger
//   GET    /api/admin/borrow/cache                  cache stats
//   POST   /api/admin/borrow/cache/refresh          force a cache reload

package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/securitiesborrow"
)

// borrowRateWire is the on-wire shape for one rate.
type borrowRateWire struct {
	InstrumentKey       string  `json:"instrument_key"`
	Symbol              string  `json:"symbol"`
	Market              string  `json:"market"`
	AssetClass          string  `json:"asset_class"`
	BorrowRateBpsAnnual float64 `json:"borrow_rate_bps_annual"`
	LocateFeeBps        float64 `json:"locate_fee_bps"`
	Availability        string  `json:"availability"`
	AvailableShares     *int64  `json:"available_shares,omitempty"`
	MinLocateQty        *int64  `json:"min_locate_qty,omitempty"`
	MaxLocateQty        *int64  `json:"max_locate_qty,omitempty"`
	Source              string  `json:"source"`
	LastCalibratedAt    string  `json:"last_calibrated_at"`
	Note                string  `json:"note,omitempty"`
	UpdatedAt           string  `json:"updated_at"`
}

func projectBorrowRate(r securitiesborrow.BorrowRate) borrowRateWire {
	return borrowRateWire{
		InstrumentKey:       r.InstrumentKey,
		Symbol:              r.Symbol,
		Market:              r.Market,
		AssetClass:          r.AssetClass,
		BorrowRateBpsAnnual: r.BorrowRateBpsAnnual,
		LocateFeeBps:        r.LocateFeeBps,
		Availability:        string(r.Availability),
		AvailableShares:     r.AvailableShares,
		MinLocateQty:        r.MinLocateQty,
		MaxLocateQty:        r.MaxLocateQty,
		Source:              string(r.Source),
		LastCalibratedAt:    r.LastCalibratedAt.UTC().Format(time.RFC3339Nano),
		Note:                r.Note,
		UpdatedAt:           r.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// locateEventWire is the on-wire shape for one audit row.
type locateEventWire struct {
	ID              string   `json:"id"`
	FundID          string   `json:"fund_id"`
	InstrumentKey   string   `json:"instrument_key"`
	Symbol          string   `json:"symbol"`
	RequestedQty    float64  `json:"requested_qty"`
	Decision        string   `json:"decision"`
	RateBpsAnnual   *float64 `json:"rate_bps_annual,omitempty"`
	LocateFeeBps    *float64 `json:"locate_fee_bps,omitempty"`
	LocateFeeAmount *float64 `json:"locate_fee_amount,omitempty"`
	IntendedPrice   *float64 `json:"intended_price,omitempty"`
	Notional        *float64 `json:"notional,omitempty"`
	Reason          string   `json:"reason,omitempty"`
	ClientOrderID   string   `json:"client_order_id,omitempty"`
	CreatedAt       string   `json:"created_at"`
}

func projectLocateEvent(e securitiesborrow.LocateEvent) locateEventWire {
	return locateEventWire{
		ID:              e.ID,
		FundID:          e.FundID,
		InstrumentKey:   e.InstrumentKey,
		Symbol:          e.Symbol,
		RequestedQty:    e.RequestedQty,
		Decision:        string(e.Decision),
		RateBpsAnnual:   e.RateBpsAnnual,
		LocateFeeBps:    e.LocateFeeBps,
		LocateFeeAmount: e.LocateFeeAmount,
		IntendedPrice:   e.IntendedPrice,
		Notional:        e.Notional,
		Reason:          e.Reason,
		ClientOrderID:   e.ClientOrderID,
		CreatedAt:       e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// ledgerWire is the on-wire shape for one ledger row.
type ledgerWire struct {
	ID                string  `json:"id"`
	FundID            string  `json:"fund_id"`
	InstrumentKey     string  `json:"instrument_key"`
	Symbol            string  `json:"symbol"`
	AccrualDate       string  `json:"accrual_date"`
	ShortQty          float64 `json:"short_qty"`
	MarketPrice       float64 `json:"market_price"`
	Notional          float64 `json:"notional"`
	RateBpsAnnual     float64 `json:"rate_bps_annual"`
	DayCountBasis     int     `json:"day_count_basis"`
	FeeAmount         float64 `json:"fee_amount"`
	CashLedgerEntryID string  `json:"cash_ledger_entry_id,omitempty"`
	CreatedAt         string  `json:"created_at"`
}

func projectLedger(e securitiesborrow.BorrowLedgerEntry) ledgerWire {
	return ledgerWire{
		ID:                e.ID,
		FundID:            e.FundID,
		InstrumentKey:     e.InstrumentKey,
		Symbol:            e.Symbol,
		AccrualDate:       e.AccrualDate.UTC().Format("2006-01-02"),
		ShortQty:          e.ShortQty,
		MarketPrice:       e.MarketPrice,
		Notional:          e.Notional,
		RateBpsAnnual:     e.RateBpsAnnual,
		DayCountBasis:     e.DayCountBasis,
		FeeAmount:         e.FeeAmount,
		CashLedgerEntryID: e.CashLedgerEntryID,
		CreatedAt:         e.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
}

// registerBorrowAdminRoutes wires the routes.
func (h *adminHandler) registerBorrowAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/borrow/rates", h.handleListBorrowRates)
	mux.HandleFunc("GET /api/admin/borrow/rates/{key}", h.handleGetBorrowRate)
	mux.HandleFunc("POST /api/admin/borrow/rates", h.handleUpsertBorrowRate)
	mux.HandleFunc("DELETE /api/admin/borrow/rates/{key}", h.handleDeleteBorrowRate)
	mux.HandleFunc("POST /api/admin/borrow/locate/preview", h.handleBorrowLocatePreview)
	mux.HandleFunc("GET /api/admin/borrow/locate/events", h.handleListLocateEvents)
	mux.HandleFunc("GET /api/admin/borrow/ledger", h.handleListBorrowLedger)
	mux.HandleFunc("GET /api/admin/borrow/cache", h.handleBorrowCacheStats)
	mux.HandleFunc("POST /api/admin/borrow/cache/refresh", h.handleBorrowCacheRefresh)
}

// ----- rates: list -----

func (h *adminHandler) handleListBorrowRates(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, err := h.borrowRepo.ListRates(r.Context(), securitiesborrow.ListRateFilter{
		Market:       strings.TrimSpace(q.Get("market")),
		AssetClass:   strings.TrimSpace(q.Get("asset_class")),
		Availability: strings.TrimSpace(q.Get("availability")),
		Limit:        limit,
		Offset:       offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]borrowRateWire, 0, len(rows))
	for _, rec := range rows {
		out = append(out, projectBorrowRate(rec))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"rates": out,
		"total": len(out),
	})
}

// ----- rates: get -----

func (h *adminHandler) handleGetBorrowRate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	rec, err := h.borrowRepo.GetRateByKey(r.Context(), key)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if rec == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no borrow-rate row"))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"rate": projectBorrowRate(*rec)})
}

// ----- rates: upsert -----

type upsertBorrowRateRequest struct {
	InstrumentKey       string   `json:"instrument_key"`
	Symbol              string   `json:"symbol"`
	Market              string   `json:"market,omitempty"`
	AssetClass          string   `json:"asset_class,omitempty"`
	BorrowRateBpsAnnual *float64 `json:"borrow_rate_bps_annual,omitempty"`
	LocateFeeBps        *float64 `json:"locate_fee_bps,omitempty"`
	Availability        string   `json:"availability,omitempty"`
	AvailableShares     *int64   `json:"available_shares,omitempty"`
	MinLocateQty        *int64   `json:"min_locate_qty,omitempty"`
	MaxLocateQty        *int64   `json:"max_locate_qty,omitempty"`
	Source              string   `json:"source,omitempty"`
	Note                string   `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertBorrowRate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	var req upsertBorrowRateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	now := time.Now().UTC()
	rec, err := h.borrowRepo.UpsertRate(r.Context(), securitiesborrow.UpsertRateParams{
		InstrumentKey:       req.InstrumentKey,
		Symbol:              req.Symbol,
		Market:              req.Market,
		AssetClass:          req.AssetClass,
		BorrowRateBpsAnnual: req.BorrowRateBpsAnnual,
		LocateFeeBps:        req.LocateFeeBps,
		Availability:        req.Availability,
		AvailableShares:     req.AvailableShares,
		MinLocateQty:        req.MinLocateQty,
		MaxLocateQty:        req.MaxLocateQty,
		Source:              req.Source,
		LastCalibratedAt:    &now,
		Note:                req.Note,
		UpdatedBy:           userID,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	// Push the fresh row into the cache so the next short order
	// sees it without waiting for the refresh tick.
	if h.borrowCache != nil {
		h.borrowCache.ApplyChange(rec.InstrumentKey, rec)
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "borrow.rate.upsert",
			TargetType:  "security_borrow_rate",
			TargetID:    rec.InstrumentKey,
			After: map[string]any{
				"availability":           rec.Availability,
				"borrow_rate_bps_annual": rec.BorrowRateBpsAnnual,
				"locate_fee_bps":         rec.LocateFeeBps,
				"available_shares":       rec.AvailableShares,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordBorrowEvent("admin_upsert_rate")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"rate": projectBorrowRate(*rec)})
}

// ----- rates: delete -----

func (h *adminHandler) handleDeleteBorrowRate(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	key := strings.TrimSpace(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if err := h.borrowRepo.DeleteRate(r.Context(), key); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "no borrow-rate row"))
			return
		}
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.borrowCache != nil {
		h.borrowCache.ApplyChange(key, nil)
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "borrow.rate.delete",
			TargetType:  "security_borrow_rate",
			TargetID:    key,
		})
	}
	if h.metrics != nil {
		h.metrics.RecordBorrowEvent("admin_delete_rate")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----- locate preview -----

type borrowLocatePreviewRequest struct {
	FundID        string  `json:"fund_id"`
	InstrumentKey string  `json:"instrument_key"`
	RequestedQty  float64 `json:"requested_qty"`
	IntendedPrice float64 `json:"intended_price"`
}

func (h *adminHandler) handleBorrowLocatePreview(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil || h.borrowCache == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	var req borrowLocatePreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.InstrumentKey) == "" || req.RequestedQty <= 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "instrument_key + positive requested_qty required"))
		return
	}
	rate := h.borrowCache.Lookup(req.InstrumentKey)
	d := securitiesborrow.NewLocateEngine().Evaluate(securitiesborrow.LocateProbe{
		FundID:        req.FundID,
		InstrumentKey: req.InstrumentKey,
		RequestedQty:  req.RequestedQty,
		IntendedPrice: req.IntendedPrice,
	}, rate)
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"decision":         string(d.Kind),
		"allowed":          d.Allowed,
		"requested_qty":    d.RequestedQty,
		"intended_price":   d.IntendedPrice,
		"notional":         d.Notional,
		"borrow_rate_bps":  d.BorrowRateBps,
		"locate_fee_bps":   d.LocateFeeBps,
		"locate_fee_amount": d.LocateFeeAmount,
		"available_shares": d.AvailableShares,
		"reason":           d.Reason,
		"source":           string(d.Source),
	})
}

// ----- locate audit -----

func (h *adminHandler) handleListLocateEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var since *time.Time
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			since = &t
		}
	}
	rows, err := h.borrowRepo.ListLocateEvents(r.Context(), securitiesborrow.ListLocateFilter{
		FundID:        strings.TrimSpace(q.Get("fund_id")),
		InstrumentKey: strings.TrimSpace(q.Get("instrument_key")),
		Decision:      strings.TrimSpace(q.Get("decision")),
		Since:         since,
		Limit:         limit,
		Offset:        offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]locateEventWire, 0, len(rows))
	for _, e := range rows {
		out = append(out, projectLocateEvent(e))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"events": out,
		"total":  len(out),
	})
}

// ----- ledger -----

func (h *adminHandler) handleListBorrowLedger(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowRepo == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "borrow not wired"))
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	var since, until *time.Time
	if s := strings.TrimSpace(q.Get("since")); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			since = &t
		}
	}
	if s := strings.TrimSpace(q.Get("until")); s != "" {
		if t, err := time.Parse("2006-01-02", s); err == nil {
			until = &t
		}
	}
	rows, err := h.borrowRepo.ListLedger(r.Context(), securitiesborrow.ListLedgerFilter{
		FundID:        strings.TrimSpace(q.Get("fund_id")),
		InstrumentKey: strings.TrimSpace(q.Get("instrument_key")),
		Since:         since,
		Until:         until,
		Limit:         limit,
		Offset:        offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]ledgerWire, 0, len(rows))
	for _, e := range rows {
		out = append(out, projectLedger(e))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"entries": out,
		"total":   len(out),
	})
}

// ----- cache stats / refresh -----

func (h *adminHandler) handleBorrowCacheStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowCache == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "cache not wired"))
		return
	}
	last := h.borrowCache.LastRefresh()
	out := map[string]any{
		"size": h.borrowCache.Size(),
	}
	if !last.IsZero() {
		out["last_refresh"] = last.UTC().Format(time.RFC3339Nano)
	}
	writeOrderActionJSON(w, http.StatusOK, out)
}

func (h *adminHandler) handleBorrowCacheRefresh(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.borrowCache == nil {
		writeOrderActionJSON(w, http.StatusServiceUnavailable, errorPayload("unavailable", "cache not wired"))
		return
	}
	if err := h.borrowCache.Refresh(r.Context()); err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.metrics != nil {
		h.metrics.RecordBorrowEvent("admin_cache_refresh")
	}
	last := h.borrowCache.LastRefresh()
	out := map[string]any{
		"ok":   true,
		"size": h.borrowCache.Size(),
	}
	if !last.IsZero() {
		out["last_refresh"] = last.UTC().Format(time.RFC3339Nano)
	}
	writeOrderActionJSON(w, http.StatusOK, out)
}
