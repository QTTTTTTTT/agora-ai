// admin_marketstatus.go — admin REST surface for the S6.1
// market-status gate.
//
// Endpoints
//
//   GET    /api/admin/marketstatus/instruments           list rows (?market, ?status, ?symbol)
//   GET    /api/admin/marketstatus/instruments/{key}     one row
//   PUT    /api/admin/marketstatus/instruments/{key}     upsert row
//   POST   /api/admin/marketstatus/instruments/{key}/halt    convenience: status=halted with reason+until
//   POST   /api/admin/marketstatus/instruments/{key}/unhalt  convenience: status=trading
//   POST   /api/admin/marketstatus/instruments/{key}/limits  convenience: set lower/upper
//
//   GET    /api/admin/marketstatus/calendar              ?market=...&from=...&to=...
//   PUT    /api/admin/marketstatus/calendar/{market}/{date}  upsert one day
//
//   GET    /api/admin/marketstatus/events                ?fund_id, ?instrument_key, ?rule_code, ?decision
//
// All endpoints require admin and audit-log mutations.

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/marketstatus"
)

// instrumentStatusWire is the on-wire shape for one instrument
// status row. Pointer fields render as omitempty so the UI can
// tell "no override set" apart from "override = 0".
type instrumentStatusWire struct {
	InstrumentKey       string   `json:"instrument_key"`
	Symbol              string   `json:"symbol"`
	Market              string   `json:"market"`
	Status              string   `json:"status"`
	HaltReason          string   `json:"halt_reason,omitempty"`
	HaltStartedAt       string   `json:"halt_started_at,omitempty"`
	HaltUntil           string   `json:"halt_until,omitempty"`
	LowerLimit          *float64 `json:"lower_limit,omitempty"`
	UpperLimit          *float64 `json:"upper_limit,omitempty"`
	LastQuoteAt         string   `json:"last_quote_at,omitempty"`
	LastQuotePrice      *float64 `json:"last_quote_price,omitempty"`
	AssetClass          string   `json:"asset_class"`
	StalenessBudgetSecs *int     `json:"staleness_budget_seconds,omitempty"`
	Note                string   `json:"note,omitempty"`
	UpdatedAt           string   `json:"updated_at"`
}

func projectStatus(s marketstatus.InstrumentStatus) instrumentStatusWire {
	out := instrumentStatusWire{
		InstrumentKey: s.InstrumentKey,
		Symbol:        s.Symbol,
		Market:        s.Market,
		Status:        s.Status,
		HaltReason:    s.HaltReason,
		LowerLimit:    s.LowerLimit,
		UpperLimit:    s.UpperLimit,
		LastQuotePrice: s.LastQuotePrice,
		AssetClass:    s.AssetClass,
		Note:          s.Note,
		UpdatedAt:     s.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if s.HaltStartedAt != nil {
		out.HaltStartedAt = s.HaltStartedAt.UTC().Format(time.RFC3339Nano)
	}
	if s.HaltUntil != nil {
		out.HaltUntil = s.HaltUntil.UTC().Format(time.RFC3339Nano)
	}
	if s.LastQuoteAt != nil {
		out.LastQuoteAt = s.LastQuoteAt.UTC().Format(time.RFC3339Nano)
	}
	if s.StalenessBudget != nil {
		secs := int(s.StalenessBudget.Seconds())
		out.StalenessBudgetSecs = &secs
	}
	return out
}

type calendarDayWire struct {
	Market      string `json:"market"`
	TradingDate string `json:"trading_date"`
	IsOpen      bool   `json:"is_open"`
	OpenLocal   string `json:"open_local"`
	CloseLocal  string `json:"close_local"`
	MarketTZ    string `json:"market_tz"`
	HalfDay     bool   `json:"half_day"`
	Note        string `json:"note,omitempty"`
}

func projectCalendarDay(d marketstatus.CalendarDay) calendarDayWire {
	return calendarDayWire{
		Market:      d.Market,
		TradingDate: d.TradingDate.Format("2006-01-02"),
		IsOpen:      d.IsOpen,
		OpenLocal:   d.OpenLocal,
		CloseLocal:  d.CloseLocal,
		MarketTZ:    d.MarketTZ,
		HalfDay:     d.HalfDay,
		Note:        d.Note,
	}
}

type marketStatusEventWire struct {
	ID            string                 `json:"id"`
	FundID        string                 `json:"fund_id,omitempty"`
	InstrumentKey string                 `json:"instrument_key"`
	Symbol        string                 `json:"symbol,omitempty"`
	Decision      string                 `json:"decision"`
	RuleCode      string                 `json:"rule_code"`
	Summary       string                 `json:"summary,omitempty"`
	Metadata      map[string]any         `json:"metadata,omitempty"`
	ClientOrderID string                 `json:"client_order_id,omitempty"`
	DetectedAt    string                 `json:"detected_at"`
}

func projectMarketStatusEvent(e marketstatus.EventDetail) marketStatusEventWire {
	return marketStatusEventWire{
		ID:            e.ID,
		FundID:        e.FundID,
		InstrumentKey: e.InstrumentKey,
		Symbol:        e.Symbol,
		Decision:      string(e.Decision),
		RuleCode:      string(e.RuleCode),
		Summary:       e.Summary,
		Metadata:      e.Metadata,
		ClientOrderID: e.ClientOrderID,
		DetectedAt:    e.DetectedAt.UTC().Format(time.RFC3339Nano),
	}
}

// registerMarketStatusAdminRoutes wires the routes. Called from
// registerAdminRoutes.
func (h *adminHandler) registerMarketStatusAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.db == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/marketstatus/instruments", h.handleListMarketStatusInstruments)
	mux.HandleFunc("GET /api/admin/marketstatus/instruments/{key}", h.handleGetMarketStatusInstrument)
	mux.HandleFunc("PUT /api/admin/marketstatus/instruments/{key}", h.handleUpsertMarketStatusInstrument)
	mux.HandleFunc("POST /api/admin/marketstatus/instruments/{key}/halt", h.handleHaltMarketStatusInstrument)
	mux.HandleFunc("POST /api/admin/marketstatus/instruments/{key}/unhalt", h.handleUnhaltMarketStatusInstrument)
	mux.HandleFunc("POST /api/admin/marketstatus/instruments/{key}/limits", h.handleSetMarketStatusLimits)
	mux.HandleFunc("GET /api/admin/marketstatus/calendar", h.handleListMarketStatusCalendar)
	mux.HandleFunc("PUT /api/admin/marketstatus/calendar/{market}/{date}", h.handleUpsertMarketStatusCalendar)
	mux.HandleFunc("GET /api/admin/marketstatus/events", h.handleListMarketStatusEvents)
}

// ----- list instruments -----

func (h *adminHandler) handleListMarketStatusInstruments(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	repo := marketstatus.NewRepo(h.db)
	rows, err := repo.ListStatus(r.Context(), marketstatus.ListStatusParams{
		Market: strings.TrimSpace(q.Get("market")),
		Status: strings.TrimSpace(q.Get("status")),
		Symbol: strings.TrimSpace(q.Get("symbol")),
		Limit:  limit, Offset: offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]instrumentStatusWire, 0, len(rows))
	for _, s := range rows {
		out = append(out, projectStatus(s))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instruments": out})
}

func (h *adminHandler) handleGetMarketStatusInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	repo := marketstatus.NewRepo(h.db)
	got, err := repo.GetByKey(r.Context(), key)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if got == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "instrument not configured"))
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectStatus(*got)})
}

// ----- upsert instrument -----

type upsertMarketStatusRequest struct {
	Symbol              string   `json:"symbol"`
	Market              string   `json:"market"`
	Status              string   `json:"status"`
	HaltReason          string   `json:"halt_reason,omitempty"`
	HaltStartedAt       string   `json:"halt_started_at,omitempty"`
	HaltUntil           string   `json:"halt_until,omitempty"`
	LowerLimit          *float64 `json:"lower_limit,omitempty"`
	UpperLimit          *float64 `json:"upper_limit,omitempty"`
	AssetClass          string   `json:"asset_class,omitempty"`
	StalenessBudgetSecs *int     `json:"staleness_budget_seconds,omitempty"`
	Note                string   `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertMarketStatusInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	var req upsertMarketStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	params := marketstatus.UpsertStatusParams{
		InstrumentKey:       key,
		Symbol:              req.Symbol,
		Market:              req.Market,
		Status:              strings.ToLower(strings.TrimSpace(req.Status)),
		HaltReason:          req.HaltReason,
		LowerLimit:          req.LowerLimit,
		UpperLimit:          req.UpperLimit,
		AssetClass:          req.AssetClass,
		StalenessBudgetSecs: req.StalenessBudgetSecs,
		Note:                req.Note,
		UpdatedBy:           userID,
	}
	if t, ok := parseTimestampPtr(req.HaltStartedAt); ok {
		params.HaltStartedAt = t
	}
	if t, ok := parseTimestampPtr(req.HaltUntil); ok {
		params.HaltUntil = t
	}
	repo := marketstatus.NewRepo(h.db)
	if err := repo.UpsertStatus(r.Context(), params); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	got, _ := repo.GetByKey(r.Context(), key)
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketstatus.upsert",
			TargetType:  "marketstatus_instrument",
			TargetID:    key,
			After: map[string]any{
				"status":       params.Status,
				"halt_reason":  params.HaltReason,
				"lower_limit":  params.LowerLimit,
				"upper_limit":  params.UpperLimit,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketStatusEvent("admin_upsert")
	}
	if got == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectStatus(*got)})
}

// ----- convenience: halt -----

type haltMarketStatusRequest struct {
	Reason    string `json:"reason"`
	HaltUntil string `json:"halt_until,omitempty"`
}

func (h *adminHandler) handleHaltMarketStatusInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	var req haltMarketStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := marketstatus.NewRepo(h.db)
	existing, _ := repo.GetByKey(r.Context(), key)
	now := time.Now().UTC()
	params := marketstatus.UpsertStatusParams{
		InstrumentKey: key,
		Status:        "halted",
		HaltReason:    req.Reason,
		HaltStartedAt: &now,
		UpdatedBy:     userID,
	}
	if existing != nil {
		params.Symbol = existing.Symbol
		params.Market = existing.Market
		params.AssetClass = existing.AssetClass
		params.LowerLimit = existing.LowerLimit
		params.UpperLimit = existing.UpperLimit
		params.Note = existing.Note
		if existing.StalenessBudget != nil {
			secs := int(existing.StalenessBudget.Seconds())
			params.StalenessBudgetSecs = &secs
		}
	}
	if t, ok := parseTimestampPtr(req.HaltUntil); ok {
		params.HaltUntil = t
	}
	if err := repo.UpsertStatus(r.Context(), params); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketstatus.halt",
			TargetType:  "marketstatus_instrument",
			TargetID:    key,
			After: map[string]any{
				"reason":     req.Reason,
				"halt_until": req.HaltUntil,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketStatusEvent("admin_halt")
	}
	got, _ := repo.GetByKey(r.Context(), key)
	if got == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectStatus(*got)})
}

func (h *adminHandler) handleUnhaltMarketStatusInstrument(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := marketstatus.NewRepo(h.db)
	existing, _ := repo.GetByKey(r.Context(), key)
	if existing == nil {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "instrument not configured"))
		return
	}
	params := marketstatus.UpsertStatusParams{
		InstrumentKey: key,
		Symbol:        existing.Symbol,
		Market:        existing.Market,
		Status:        "trading",
		AssetClass:    existing.AssetClass,
		LowerLimit:    existing.LowerLimit,
		UpperLimit:    existing.UpperLimit,
		Note:          existing.Note,
		UpdatedBy:     userID,
	}
	if existing.StalenessBudget != nil {
		secs := int(existing.StalenessBudget.Seconds())
		params.StalenessBudgetSecs = &secs
	}
	if err := repo.UpsertStatus(r.Context(), params); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketstatus.unhalt",
			TargetType:  "marketstatus_instrument",
			TargetID:    key,
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketStatusEvent("admin_unhalt")
	}
	got, _ := repo.GetByKey(r.Context(), key)
	if got == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectStatus(*got)})
}

// ----- convenience: set price limits -----

type setLimitsMarketStatusRequest struct {
	LowerLimit *float64 `json:"lower_limit"`
	UpperLimit *float64 `json:"upper_limit"`
}

func (h *adminHandler) handleSetMarketStatusLimits(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	key := decodePath(r.PathValue("key"))
	if key == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_key", "instrument_key required"))
		return
	}
	var req setLimitsMarketStatusRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := marketstatus.NewRepo(h.db)
	existing, _ := repo.GetByKey(r.Context(), key)
	params := marketstatus.UpsertStatusParams{
		InstrumentKey: key,
		Status:        "trading",
		LowerLimit:    req.LowerLimit,
		UpperLimit:    req.UpperLimit,
		UpdatedBy:     userID,
	}
	if existing != nil {
		params.Symbol = existing.Symbol
		params.Market = existing.Market
		params.Status = existing.Status
		params.HaltReason = existing.HaltReason
		params.HaltStartedAt = existing.HaltStartedAt
		params.HaltUntil = existing.HaltUntil
		params.AssetClass = existing.AssetClass
		params.Note = existing.Note
		if existing.StalenessBudget != nil {
			secs := int(existing.StalenessBudget.Seconds())
			params.StalenessBudgetSecs = &secs
		}
	}
	if err := repo.UpsertStatus(r.Context(), params); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketstatus.set_limits",
			TargetType:  "marketstatus_instrument",
			TargetID:    key,
			After: map[string]any{
				"lower_limit": req.LowerLimit,
				"upper_limit": req.UpperLimit,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketStatusEvent("admin_set_limits")
	}
	got, _ := repo.GetByKey(r.Context(), key)
	if got == nil {
		writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"instrument": projectStatus(*got)})
}

// ----- calendar -----

func (h *adminHandler) handleListMarketStatusCalendar(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	market := strings.TrimSpace(q.Get("market"))
	if market == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_market", "market required"))
		return
	}
	now := time.Now().UTC()
	from := now.AddDate(0, 0, -7)
	to := now.AddDate(0, 1, 0)
	if v, ok := parseDateOnly(q.Get("from")); ok {
		from = v
	}
	if v, ok := parseDateOnly(q.Get("to")); ok {
		to = v
	}
	repo := marketstatus.NewRepo(h.db)
	rows, err := repo.ListCalendarDays(r.Context(), market, from, to)
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]calendarDayWire, 0, len(rows))
	for _, d := range rows {
		out = append(out, projectCalendarDay(d))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"days": out})
}

type upsertCalendarDayRequest struct {
	IsOpen     bool   `json:"is_open"`
	OpenLocal  string `json:"open_local"`
	CloseLocal string `json:"close_local"`
	MarketTZ   string `json:"market_tz"`
	HalfDay    bool   `json:"half_day"`
	Note       string `json:"note,omitempty"`
}

func (h *adminHandler) handleUpsertMarketStatusCalendar(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	market := strings.TrimSpace(r.PathValue("market"))
	dateStr := strings.TrimSpace(r.PathValue("date"))
	if market == "" || dateStr == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "market + date required"))
		return
	}
	date, ok := parseDateOnly(dateStr)
	if !ok {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_date", "date must be YYYY-MM-DD"))
		return
	}
	var req upsertCalendarDayRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	repo := marketstatus.NewRepo(h.db)
	if err := repo.UpsertCalendarDay(r.Context(), marketstatus.UpsertCalendarDayParams{
		Market: market, TradingDate: date,
		IsOpen: req.IsOpen, OpenLocal: req.OpenLocal, CloseLocal: req.CloseLocal,
		MarketTZ: req.MarketTZ, HalfDay: req.HalfDay, Note: req.Note,
	}); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("upsert_failed", err.Error()))
		return
	}
	if h.auditLogger != nil {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "marketstatus.calendar_upsert",
			TargetType:  "marketstatus_calendar",
			TargetID:    fmt.Sprintf("%s:%s", market, dateStr),
			After: map[string]any{
				"is_open":  req.IsOpen,
				"half_day": req.HalfDay,
			},
		})
	}
	if h.metrics != nil {
		h.metrics.RecordMarketStatusEvent("admin_calendar_upsert")
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ----- events -----

func (h *adminHandler) handleListMarketStatusEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	repo := marketstatus.NewRepo(h.db)
	rows, err := repo.ListEvents(r.Context(), marketstatus.ListEventsParams{
		FundID:        strings.TrimSpace(q.Get("fund_id")),
		InstrumentKey: strings.TrimSpace(q.Get("instrument_key")),
		RuleCode:      strings.TrimSpace(q.Get("rule_code")),
		Decision:      strings.TrimSpace(q.Get("decision")),
		Limit:         limit, Offset: offset,
	})
	if err != nil {
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	out := make([]marketStatusEventWire, 0, len(rows))
	for _, e := range rows {
		out = append(out, projectMarketStatusEvent(e))
	}
	writeOrderActionJSON(w, http.StatusOK, map[string]any{"events": out})
}

// ----- helpers -----

// parseTimestampPtr accepts RFC3339 and returns (ptr, ok). Empty
// string returns (nil, false) so the caller can leave the field
// alone.
func parseTimestampPtr(s string) (*time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, false
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		// Fall back to YYYY-MM-DDThh:mm:ssZ (no nanos) is RFC3339;
		// other shapes don't parse. We deliberately don't try
		// dozens of formats — operator should send RFC3339.
		return nil, false
	}
	t = t.UTC()
	return &t, true
}

func parseDateOnly(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// decodePath URL-decodes a single path segment. Instrument keys
// can contain `.` (e.g. AAPL.US) which is fine, but if the
// upstream client URL-encodes for safety we still need to undo
// it.
func decodePath(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if dec, err := url.PathUnescape(s); err == nil {
		return strings.TrimSpace(dec)
	}
	return s
}
