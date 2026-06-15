// advisor_handler.go — HTTP surface for the /advisor consultation
// mode introduced in migration 098.
//
// Five endpoints, all scoped to the authenticated user (no fund_id
// because /advisor is intentionally isolated from the fund/team
// subsystem):
//
//   GET    /api/advisor/presets            list of available persona presets
//   POST   /api/advisor/consult            run one consultation for symbol+preset
//   GET    /api/advisor/history            list of the user's past consultations
//   GET    /api/advisor/consultations/{id} fetch one consultation + every child report
//   GET    /api/advisor/health             cheap "is the surface wired?" probe
//
// The advisor_mode feature flag (registered in migration 098 with
// enforce_server_gate=TRUE) is enforced upstream of this handler
// by featureGateMiddleware, so the handler itself only worries
// about auth + input validation + service errors.
//
// All wire shapes are intentionally explicit (no embedding of
// internal types) so the API surface can evolve independently
// from the service-package structs.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/advisor"
	"github.com/fundai/server/internal/advisorbilling"
	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/compliance"
	"github.com/fundai/server/internal/i18nmsg"
	"github.com/fundai/server/internal/repository"
)

type advisorHandler struct {
	svc           *advisor.Service
	repRepo       *agentreputation.Repo
	gate          *advisorbilling.Gate
	complianceRepo *repository.ComplianceRepo
	complianceMode compliance.Mode
	now           func() time.Time
}

// newAdvisorHandler returns nil when the service isn't wired so
// the router keeps the routes unregistered in degraded boots —
// hitting them then resolves to the SPA 404 page rather than a
// 500 that confuses tests.
//
// repRepo (Phase 5) is optional — when nil the /track-record
// endpoint returns an empty list rather than 500ing, so the UI
// can degrade gracefully before the reputation loop has produced
// any rows.
func newAdvisorHandler(svc *Services) *advisorHandler {
	if svc == nil || svc.AdvisorService == nil {
		return nil
	}
	return &advisorHandler{
		svc:            svc.AdvisorService,
		repRepo:        svc.AgentReputationRepo,
		gate:           svc.AdvisorBillingGate,
		complianceRepo: svc.ComplianceRepo,
		complianceMode: svc.ComplianceMode,
		now:            time.Now,
	}
}

func (h *advisorHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/advisor/health", h.handleHealth)
	mux.HandleFunc("GET /api/advisor/presets", h.handleListPresets)
	mux.HandleFunc("POST /api/advisor/consult", h.handleConsult)
	mux.HandleFunc("GET /api/advisor/history", h.handleHistory)
	mux.HandleFunc("GET /api/advisor/consultations/{id}", h.handleGetConsultation)
	mux.HandleFunc("GET /api/advisor/track-record", h.handleTrackRecord)
	mux.HandleFunc("GET /api/advisor/billing/summary", h.handleBillingSummary)
	mux.HandleFunc("GET /api/advisor/billing/calls", h.handleBillingCalls)
}

// --- Wire shapes -----------------------------------------------------------

type advisorPresetWire struct {
	Key           string   `json:"preset_key"`
	LabelZh       string   `json:"label_zh"`
	LabelEn       string   `json:"label_en"`
	DescriptionZh string   `json:"description_zh"`
	DescriptionEn string   `json:"description_en"`
	MasterKeys    []string `json:"master_keys"`
	TacticKeys    []string `json:"tactic_keys"`
	Kind          string   `json:"kind"`
	SortOrder     int      `json:"sort_order"`
}

type advisorConsultRequest struct {
	Symbol           string   `json:"symbol"`
	Market           string   `json:"market,omitempty"`
	AssetClass       string   `json:"asset_class,omitempty"`
	PresetKey        string   `json:"preset_key"`
	CustomMasterKeys []string `json:"custom_master_keys,omitempty"`
	CustomTacticKeys []string `json:"custom_tactic_keys,omitempty"`
	Notes            string   `json:"notes,omitempty"`
	PriceLast        float64  `json:"price_last,omitempty"`
	PriceChange      float64  `json:"price_change,omitempty"`
	Currency         string   `json:"currency,omitempty"`
}

type advisorConsultResponse struct {
	ConsultationID      string                 `json:"consultation_id"`
	Symbol              string                 `json:"symbol"`
	// SymbolName is the issuer's short Chinese / English name.
	// Omitted when the upstream data provider didn't resolve a
	// name — frontends fall back to displaying the bare symbol.
	SymbolName          string                 `json:"symbol_name,omitempty"`
	PresetKey           string                 `json:"preset_key"`
	AggregateVerdict    string                 `json:"aggregate_verdict"`
	AggregateConfidence int                    `json:"aggregate_confidence"`
	ConsensusScore      float64                `json:"consensus_score"`
	MasterReports       []advisorMasterWire    `json:"master_reports"`
	TacticReports       []advisorTacticWire    `json:"tactic_reports"`
	CreatedAt           time.Time              `json:"created_at"`
}

type advisorMasterWire struct {
	MasterKey      string         `json:"master_key"`
	MasterNameZh   string         `json:"master_name_zh"`
	MasterNameEn   string         `json:"master_name_en"`
	Verdict        string         `json:"verdict"`
	Confidence     int            `json:"confidence"`
	Thesis         string         `json:"thesis"`
	KeyReasons     []string       `json:"key_reasons"`
	KeyRisks       []string       `json:"key_risks"`
	MasterSpecific map[string]any `json:"master_specific,omitempty"`
	RedLinesHit    []string       `json:"red_lines_hit,omitempty"`
	LLMModel       string         `json:"llm_model,omitempty"`
	GeneratedAt    time.Time      `json:"generated_at"`
}

type advisorTacticWire struct {
	TacticKey           string    `json:"tactic_key"`
	TacticNameZh        string    `json:"tactic_name_zh"`
	TacticNameEn        string    `json:"tactic_name_en"`
	Verdict             string    `json:"verdict"`
	Confidence          int       `json:"confidence"`
	Thesis              string    `json:"thesis"`
	EntryPriceLow       *float64  `json:"entry_price_low,omitempty"`
	EntryPriceHigh      *float64  `json:"entry_price_high,omitempty"`
	StopLossPrice       *float64  `json:"stop_loss_price,omitempty"`
	TargetT1            *float64  `json:"target_t1,omitempty"`
	TargetT3            *float64  `json:"target_t3,omitempty"`
	ExpectedHoldingDays *int      `json:"expected_holding_days,omitempty"`
	Score               float64   `json:"score"`
	KeyReasons          []string  `json:"key_reasons"`
	KeyRisks            []string  `json:"key_risks"`
	RedLinesHit         []string  `json:"red_lines_hit,omitempty"`
	MarketRegimePass    bool      `json:"market_regime_pass"`
	MarketRegimeReason  string    `json:"market_regime_reason,omitempty"`
	GeneratedAt         time.Time `json:"generated_at"`
}

type advisorConsultationSummaryWire struct {
	ID                  string    `json:"id"`
	Symbol              string    `json:"symbol"`
	// SymbolName mirrors advisorConsultResponse.SymbolName.
	SymbolName          string    `json:"symbol_name,omitempty"`
	Market              string    `json:"market,omitempty"`
	PresetKey           string    `json:"preset_key"`
	AggregateVerdict    string    `json:"aggregate_verdict"`
	AggregateConfidence int       `json:"aggregate_confidence"`
	ConsensusScore      float64   `json:"consensus_score"`
	MasterCount         int       `json:"master_count"`
	TacticCount         int       `json:"tactic_count"`
	CreatedAt           time.Time `json:"created_at"`
}

// --- Handlers --------------------------------------------------------------

func (h *advisorHandler) handleHealth(w http.ResponseWriter, r *http.Request) {
	// No auth required — the SPA polls this from /welcome to
	// decide whether to render the "大师团队咨询" card.
	tacticsLoaded := false
	if personas, err := agent.LoadTacticPersonas(); err == nil && len(personas) > 0 {
		tacticsLoaded = true
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":         "ok",
		"masters_loaded": h.svc != nil,
		"tactics_loaded": tacticsLoaded,
		"server_time":    h.now().Format(time.RFC3339),
	})
}

func (h *advisorHandler) handleListPresets(w http.ResponseWriter, r *http.Request) {
	if _, ok := api.AuthenticatedUserID(r); !ok {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.svc == nil || h.svc.Presets() == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("advisor_unavailable", "advisor service not wired"))
		return
	}
	presets, err := h.svc.Presets().List(r.Context(), true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	// Filter cn_tactic-bound presets out of the en-US response so
	// the picker UI only shows what the user can actually run. The
	// per-Consult locale guard remains in place as defence in depth
	// in case a stale frontend caches a now-blocked preset key.
	hideTactic := i18nmsg.FromCtx(r.Context()) == i18nmsg.LocaleEN
	out := make([]advisorPresetWire, 0, len(presets))
	for _, p := range presets {
		if hideTactic && len(p.TacticKeys) > 0 {
			continue
		}
		out = append(out, advisorPresetWire{
			Key:           p.Key,
			LabelZh:       p.LabelZh,
			LabelEn:       p.LabelEn,
			DescriptionZh: p.DescriptionZh,
			DescriptionEn: p.DescriptionEn,
			MasterKeys:    p.MasterKeys,
			TacticKeys:    p.TacticKeys,
			Kind:          string(p.Kind()),
			SortOrder:     p.SortOrder,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"presets": out})
}

func (h *advisorHandler) handleConsult(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	var req advisorConsultRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	if strings.TrimSpace(req.Symbol) == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("symbol_required", "symbol is required"))
		return
	}
	if strings.TrimSpace(req.PresetKey) == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("preset_required", "preset_key is required"))
		return
	}
	if h.svc == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("advisor_unavailable", "advisor service not wired"))
		return
	}

	// SEC Publishers' Exclusion gate (Path A, before RIA
	// registration): the user must have an on-file
	// acknowledgment that they understand the impersonal-
	// analysis disclosure. We do this BEFORE the billing /
	// LLM call so a not-yet-acknowledged user doesn't burn
	// credits on a request we'd refuse to deliver anyway.
	//
	// 'global' surface covers all advisor surfaces; the
	// per-surface check is for users who want fine-grained
	// audit. RIA mode skips the gate because once Form ADV is
	// on file, the Form CRS delivery is the per-user consent.
	if !h.disclaimerOK(r.Context(), userID) {
		writeJSON(w, http.StatusPreconditionRequired, errorPayload(
			"disclaimer_required",
			"user must acknowledge the impersonal-analysis disclosure before requesting an advisor consultation; POST /api/compliance/acknowledgments first",
		))
		return
	}

	// Phase A — classify the consultation BEFORE running the
	// panel so we can charge the right bucket. Done by reading
	// the resolved preset (so a custom preset with one master
	// gets quick-priced even though preset.MasterKeys is empty
	// in DB).
	kind, preGateErr := h.classifyConsult(r, req)
	if preGateErr != nil {
		// Preset lookup failures here surface the same way as
		// the service-level branch below: 404 / 503 etc.
		writeConsultLookupError(w, preGateErr)
		return
	}

	// Gate.Check is best-effort gated. When the gate isn't wired
	// (test boot / DB-less degraded mode) we skip both Check and
	// Consume so the existing flow keeps working.
	var preCheckSource advisorbilling.UnitSource
	if h.gate != nil {
		dec, err := h.gate.Check(r.Context(), userID, kind)
		if err != nil {
			if advisorbilling.IsQuotaExceeded(err) {
				writeAdvisorQuotaExceeded(w, err)
				return
			}
			writeJSON(w, http.StatusInternalServerError, errorPayload("billing_check_failed", err.Error()))
			return
		}
		preCheckSource = dec.Source
		writeBillingHeaders(w, dec)
	}

	resp, err := h.svc.Consult(r.Context(), advisor.ConsultRequest{
		UserID:           userID,
		Symbol:           req.Symbol,
		Market:           req.Market,
		AssetClass:       req.AssetClass,
		PresetKey:        req.PresetKey,
		CustomMasterKeys: req.CustomMasterKeys,
		CustomTacticKeys: req.CustomTacticKeys,
		Notes:            req.Notes,
		PriceLast:        req.PriceLast,
		PriceChange:      req.PriceChange,
		Currency:         req.Currency,
	})
	if err != nil {
		switch {
		case errors.Is(err, advisor.ErrPresetNotFound):
			writeJSON(w, http.StatusNotFound, errorPayload("preset_not_found", err.Error()))
		case errors.Is(err, advisor.ErrPresetLocaleBlocked):
			writeJSON(w, http.StatusBadRequest, errorPayload(
				"preset_locale_blocked",
				i18nmsg.T(i18nmsg.FromCtx(r.Context()), i18nmsg.KeyAdvisorPresetLocaleBlocked),
			))
		case errors.Is(err, advisor.ErrUnsupportedPreset):
			writeJSON(w, http.StatusNotImplemented, errorPayload("preset_not_supported", "this preset requires a panel not yet wired (e.g. A-share short-term tactics in Phase 4)"))
		case errors.Is(err, advisor.ErrNotReady):
			writeJSON(w, http.StatusServiceUnavailable, errorPayload("advisor_unavailable", err.Error()))
		default:
			writeJSON(w, http.StatusInternalServerError, errorPayload("consult_failed", err.Error()))
		}
		return
	}

	// Consume AFTER a successful Consult: failed consults must
	// not burn quota, otherwise a flaky upstream LLM eats free
	// users alive. The Consume call is best-effort logged but
	// non-fatal — we'd rather under-charge once than 500 a user
	// who just paid for compute.
	if h.gate != nil {
		if dec, cerr := h.gate.Consume(r.Context(), userID, kind, preCheckSource); cerr == nil && dec != nil {
			writeBillingHeaders(w, dec)
		}
	}
	writeJSON(w, http.StatusOK, projectConsultResponse(resp))
}

// classifyConsult resolves the preset (via Service.Presets) and
// classifies the consultation as deep or quick. Centralised so
// the Gate sees the same kind the billing UI will report later.
//
// When the preset can't be resolved the caller surfaces the
// error the same way the Consult path would.
func (h *advisorHandler) classifyConsult(r *http.Request, req advisorConsultRequest) (advisorbilling.ConsultKind, error) {
	if h.svc == nil || h.svc.Presets() == nil {
		// Without a preset lookup we can't classify — assume
		// deep so the conservative quota gets charged. The
		// downstream Consult will likely error too.
		return advisorbilling.KindDeep, nil
	}
	preset, err := h.svc.Presets().Get(r.Context(), req.PresetKey)
	if err != nil {
		return "", err
	}
	masterKeys, tacticKeys := preset.MasterKeys, preset.TacticKeys
	if preset.Kind() == advisor.PresetKindEmpty {
		masterKeys, tacticKeys = req.CustomMasterKeys, req.CustomTacticKeys
	}
	return advisorbilling.ClassifyPreset(preset, masterKeys, tacticKeys), nil
}

// writeConsultLookupError surfaces preset-lookup errors that
// surface before the service.Consult call (i.e. inside the gate
// pre-check) with the same status codes the service path uses.
func writeConsultLookupError(w http.ResponseWriter, err error) {
	if errors.Is(err, advisor.ErrPresetNotFound) {
		writeJSON(w, http.StatusNotFound, errorPayload("preset_not_found", err.Error()))
		return
	}
	writeJSON(w, http.StatusInternalServerError, errorPayload("preset_lookup_failed", err.Error()))
}

// writeAdvisorQuotaExceeded surfaces a 402 Payment Required with
// a structured payload the SPA can show "you've used 5/5 deep
// consults this month — upgrade to Pro for 100" against.
func writeAdvisorQuotaExceeded(w http.ResponseWriter, err error) {
	var qe *advisorbilling.QuotaExceededError
	if !errors.As(err, &qe) {
		writeJSON(w, http.StatusInternalServerError, errorPayload("quota_check_failed", err.Error()))
		return
	}
	payload := map[string]any{
		"error":            "advisor_quota_exceeded",
		"kind":             string(qe.Kind),
		"plan_tier":        string(qe.PlanTier),
		"limit":            qe.Limit,
		"used":             qe.Used,
		"next_reset_at":    qe.NextResetAt.UTC().Format(time.RFC3339),
		"upgrade_suggested": string(qe.UpgradeSuggested),
		"message":          qe.Error(),
	}
	writeJSON(w, http.StatusPaymentRequired, payload)
}

// writeBillingHeaders stamps RateLimit-style headers on the
// response so the SPA can read remaining quota without a
// follow-up Summary call. Mirrors the well-known
// X-RateLimit-* conventions.
func writeBillingHeaders(w http.ResponseWriter, dec *advisorbilling.Decision) {
	if dec == nil {
		return
	}
	w.Header().Set("X-Advisor-Plan", string(dec.PlanTier))
	w.Header().Set("X-Advisor-Kind", string(dec.Kind))
	if dec.Limit >= 0 {
		w.Header().Set("X-Advisor-Limit", strconv.Itoa(dec.Limit))
		w.Header().Set("X-Advisor-Used", strconv.Itoa(dec.Used))
		w.Header().Set("X-Advisor-Remaining", strconv.Itoa(dec.Remaining))
	} else {
		w.Header().Set("X-Advisor-Limit", "unlimited")
	}
	if !dec.NextResetAt.IsZero() {
		w.Header().Set("X-Advisor-Reset", dec.NextResetAt.UTC().Format(time.RFC3339))
	}
}

func (h *advisorHandler) handleHistory(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.svc == nil || h.svc.Repo() == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("advisor_unavailable", "advisor service not wired"))
		return
	}
	q := r.URL.Query()
	limit := 50
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	includeChildren := q.Get("include") == "children"
	rows, err := h.svc.Repo().ListConsultations(r.Context(), advisor.ListConsultationsParams{
		UserID:          userID,
		Symbol:          q.Get("symbol"),
		PresetKey:       q.Get("preset_key"),
		Limit:           limit,
		IncludeChildren: includeChildren,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("list_failed", err.Error()))
		return
	}
	out := make([]advisorConsultationSummaryWire, 0, len(rows))
	full := make([]map[string]any, 0, len(rows))
	for _, c := range rows {
		out = append(out, advisorConsultationSummaryWire{
			ID:                  c.ID,
			Symbol:              c.Symbol,
			SymbolName:          c.SymbolName,
			Market:              c.Market,
			PresetKey:           c.PresetKey,
			AggregateVerdict:    c.AggregateVerdict,
			AggregateConfidence: c.AggregateConfidence,
			ConsensusScore:      c.ConsensusScore,
			MasterCount:         len(c.MasterReports),
			TacticCount:         len(c.TacticReports),
			CreatedAt:           c.CreatedAt,
		})
		if includeChildren {
			full = append(full, projectConsultationDetail(c))
		}
	}
	body := map[string]any{"consultations": out}
	if includeChildren {
		body["details"] = full
	}
	writeJSON(w, http.StatusOK, body)
}

// --- Phase 5 public track record --------------------------------------------

type advisorTrackRecordRowWire struct {
	AgentID        string  `json:"agent_id"`
	AgentName      string  `json:"agent_name"`
	AgentKind      string  `json:"agent_kind"`
	Category       string  `json:"category"`
	DecisionsCount int64   `json:"decisions_count"`
	HitsCount      int64   `json:"hits_count"`
	MissesCount    int64   `json:"misses_count"`
	HitRate        float64 `json:"hit_rate"`
	AvgAlpha       float64 `json:"avg_alpha"`
	AvgConfidence  float64 `json:"avg_confidence"`
	LastDecisionAt string  `json:"last_decision_at,omitempty"`
	UpdatedAt      string  `json:"updated_at"`
}

type advisorTrackRecordResponse struct {
	Masters []advisorTrackRecordRowWire `json:"masters"`
	Tactics []advisorTrackRecordRowWire `json:"tactics"`
}

// handleTrackRecord returns the rolling per-master + per-tactic
// stats for the /advisor surface. Read by AdvisorTrackRecordPanel
// and by the Welcome page header. Authenticated but not
// fund-scoped — every signed-in user sees the same global
// leaderboard.
//
// Degraded behaviour: when AgentReputationRepo is nil (test boot,
// rolled-back migration), returns an empty {"masters":[],
// "tactics":[]} with HTTP 200 so the UI never has to special-case.
func (h *advisorHandler) handleTrackRecord(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.repRepo == nil {
		writeJSON(w, http.StatusOK, advisorTrackRecordResponse{
			Masters: []advisorTrackRecordRowWire{},
			Tactics: []advisorTrackRecordRowWire{},
		})
		return
	}
	limit := 50
	if v := strings.TrimSpace(r.URL.Query().Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 200 {
			limit = n
		}
	}
	masters, err := h.repRepo.ListStats(r.Context(), agentreputation.ListStatsParams{
		AdvisorOnly: true,
		AgentKind:   agentreputation.KindMaster,
		Limit:       limit,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("track_record_failed", err.Error()))
		return
	}
	tactics, err := h.repRepo.ListStats(r.Context(), agentreputation.ListStatsParams{
		AdvisorOnly: true,
		AgentKind:   agentreputation.KindTactic,
		Limit:       limit,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("track_record_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, advisorTrackRecordResponse{
		Masters: projectAdvisorTrackRecordRows(masters),
		Tactics: projectAdvisorTrackRecordRows(tactics),
	})
}

func projectAdvisorTrackRecordRows(rows []agentreputation.Stats) []advisorTrackRecordRowWire {
	out := make([]advisorTrackRecordRowWire, 0, len(rows))
	for _, s := range rows {
		row := advisorTrackRecordRowWire{
			AgentID:        s.AgentID,
			AgentName:      s.AgentName,
			AgentKind:      string(s.AgentKind),
			Category:       s.Category,
			DecisionsCount: s.DecisionsCount,
			HitsCount:      s.HitsCount,
			MissesCount:    s.MissesCount,
			HitRate:        s.HitRate(),
			AvgAlpha:       s.AvgAlpha,
			AvgConfidence:  s.AvgConfidence,
			UpdatedAt:      s.UpdatedAt.UTC().Format(time.RFC3339Nano),
		}
		if s.LastDecisionAt.Valid {
			row.LastDecisionAt = s.LastDecisionAt.Time.UTC().Format(time.RFC3339Nano)
		}
		out = append(out, row)
	}
	return out
}

func (h *advisorHandler) handleGetConsultation(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.svc == nil || h.svc.Repo() == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("advisor_unavailable", "advisor service not wired"))
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "consultation id required"))
		return
	}
	row, err := h.svc.Repo().GetConsultation(r.Context(), userID, id)
	if err != nil {
		if errors.Is(err, advisor.ErrNotFound) {
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "consultation not found"))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("fetch_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, projectConsultationDetail(row))
}

// --- Projection helpers ----------------------------------------------------

func projectConsultResponse(in advisor.ConsultResponse) advisorConsultResponse {
	return advisorConsultResponse{
		ConsultationID:      in.ConsultationID,
		Symbol:              in.Symbol,
		SymbolName:          in.SymbolName,
		PresetKey:           in.PresetKey,
		AggregateVerdict:    in.AggregateVerdict,
		AggregateConfidence: in.AggregateConfidence,
		ConsensusScore:      in.ConsensusScore,
		MasterReports:       projectMasterRows(in.MasterReports),
		TacticReports:       projectTacticRows(in.TacticReports),
		CreatedAt:           in.CreatedAt,
	}
}

func projectMasterRows(in []advisor.MasterReportRow) []advisorMasterWire {
	out := make([]advisorMasterWire, 0, len(in))
	for _, r := range in {
		out = append(out, advisorMasterWire{
			MasterKey:      r.MasterKey,
			MasterNameZh:   r.MasterNameZh,
			MasterNameEn:   r.MasterNameEn,
			Verdict:        r.Verdict,
			Confidence:     r.Confidence,
			Thesis:         r.Thesis,
			KeyReasons:     r.KeyReasons,
			KeyRisks:       r.KeyRisks,
			MasterSpecific: r.MasterSpecific,
			RedLinesHit:    r.RedLinesHit,
			LLMModel:       r.LLMModel,
			GeneratedAt:    r.GeneratedAt,
		})
	}
	return out
}

func projectTacticRows(in []advisor.TacticReportRow) []advisorTacticWire {
	out := make([]advisorTacticWire, 0, len(in))
	for _, r := range in {
		out = append(out, advisorTacticWire{
			TacticKey:           r.TacticKey,
			TacticNameZh:        r.TacticNameZh,
			TacticNameEn:        r.TacticNameEn,
			Verdict:             r.Verdict,
			Confidence:          r.Confidence,
			Thesis:              r.Thesis,
			EntryPriceLow:       r.EntryPriceLow,
			EntryPriceHigh:      r.EntryPriceHigh,
			StopLossPrice:       r.StopLossPrice,
			TargetT1:            r.TargetT1,
			TargetT3:            r.TargetT3,
			ExpectedHoldingDays: r.ExpectedHoldingDays,
			Score:               r.Score,
			KeyReasons:          r.KeyReasons,
			KeyRisks:            r.KeyRisks,
			RedLinesHit:         r.RedLinesHit,
			MarketRegimePass:    r.MarketRegimePass,
			MarketRegimeReason:  r.MarketRegimeReason,
			GeneratedAt:         r.GeneratedAt,
		})
	}
	return out
}

// handleBillingSummary serves the per-user advisor monthly
// quota snapshot the SPA renders in AdvisorBillingHeader.
//
// Degraded behaviour: when the Gate isn't wired, returns a zeroed
// "Free / 5 deep / 15 quick" payload so the front-end never has
// to special-case missing data — the user can still try a
// consultation; the actual gate happens at request time.
type advisorBillingSummaryWire struct {
	PlanTier            string `json:"plan_tier"`
	YearMonth           string `json:"year_month"`
	DeepLimit           int    `json:"deep_limit"`
	DeepUsed            int    `json:"deep_used"`
	DeepRemaining       int    `json:"deep_remaining"`
	QuickLimit          int    `json:"quick_limit"`
	QuickUsed           int    `json:"quick_used"`
	QuickRemaining      int    `json:"quick_remaining"`
	NextResetAt         string `json:"next_reset_at"`
	AllowAdvisorBYOK    bool   `json:"allow_advisor_byok"`
	UpgradeSuggested    string `json:"upgrade_suggested,omitempty"`
	CreditDeepBalance   int    `json:"credit_deep_balance,omitempty"`
	CreditQuickBalance  int    `json:"credit_quick_balance,omitempty"`
	TotalPurchasedCents int64  `json:"total_purchased_cents,omitempty"`
}

func (h *advisorHandler) handleBillingSummary(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.gate == nil {
		writeJSON(w, http.StatusOK, advisorBillingSummaryWire{
			PlanTier:       "free",
			YearMonth:      h.now().UTC().Format("2006-01"),
			DeepLimit:      5,
			QuickLimit:     15,
			DeepRemaining:  5,
			QuickRemaining: 15,
			NextResetAt:    h.now().UTC().AddDate(0, 1, 0).Format(time.RFC3339),
		})
		return
	}
	sum, err := h.gate.Summary(r.Context(), userID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("billing_summary_failed", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, advisorBillingSummaryWire{
		PlanTier:            string(sum.PlanTier),
		YearMonth:           sum.YearMonth,
		DeepLimit:           sum.DeepLimit,
		DeepUsed:            sum.DeepUsed,
		DeepRemaining:       sum.DeepRemaining,
		QuickLimit:          sum.QuickLimit,
		QuickUsed:           sum.QuickUsed,
		QuickRemaining:      sum.QuickRemaining,
		NextResetAt:         sum.NextResetAt.UTC().Format(time.RFC3339),
		AllowAdvisorBYOK:    sum.AllowAdvisorBYOK,
		UpgradeSuggested:    string(sum.UpgradeSuggested),
		CreditDeepBalance:   sum.CreditDeepBalance,
		CreditQuickBalance:  sum.CreditQuickBalance,
		TotalPurchasedCents: sum.TotalPurchasedCents,
	})
}

// handleBillingCalls powers the /settings/byok call-log panel.
// One row per /consult call with the model used, the service-unit
// source (plan / credit / unmetered) and a derived `byok_used`
// boolean so the SPA can hide "free LLM" calls when the user
// toggles "BYOK only".
type advisorBillingCallWire struct {
	ID                string    `json:"id"`
	Symbol            string    `json:"symbol"`
	PresetKey         string    `json:"preset_key"`
	AggregateVerdict  string    `json:"aggregate_verdict"`
	ServiceUnitSource string    `json:"service_unit_source"`
	ServiceUnitCost   int       `json:"service_unit_cost"`
	ModelsUsed        []string  `json:"models_used"`
	BYOKUsed          bool      `json:"byok_used"`
	CreatedAt         time.Time `json:"created_at"`
}

type advisorBillingCallsResponse struct {
	Calls []advisorBillingCallWire `json:"calls"`
}

func (h *advisorHandler) handleBillingCalls(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	if h.svc == nil {
		writeJSON(w, http.StatusOK, advisorBillingCallsResponse{Calls: []advisorBillingCallWire{}})
		return
	}
	limit := 30
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	byokOnly := false
	if v := r.URL.Query().Get("byok"); v == "1" || strings.EqualFold(v, "true") {
		byokOnly = true
	}
	rows, err := h.svc.Repo().ListBillingCalls(r.Context(), advisor.ListBillingCallsParams{
		UserID:   userID,
		Limit:    limit,
		BYOKOnly: byokOnly,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("billing_calls_failed", err.Error()))
		return
	}
	out := make([]advisorBillingCallWire, 0, len(rows))
	for _, row := range rows {
		out = append(out, advisorBillingCallWire{
			ID:                row.ID,
			Symbol:            row.Symbol,
			PresetKey:         row.PresetKey,
			AggregateVerdict:  row.AggregateVerdict,
			ServiceUnitSource: row.ServiceUnitSource,
			ServiceUnitCost:   row.ServiceUnitCost,
			ModelsUsed:        row.ModelsUsed,
			BYOKUsed:          row.BYOKUsed,
			CreatedAt:         row.CreatedAt,
		})
	}
	writeJSON(w, http.StatusOK, advisorBillingCallsResponse{Calls: out})
}

func projectConsultationDetail(c advisor.ConsultationRow) map[string]any {
	out := map[string]any{
		"id":                   c.ID,
		"symbol":               c.Symbol,
		"market":               c.Market,
		"asset_class":          c.AssetClass,
		"preset_key":           c.PresetKey,
		"aggregate_verdict":    c.AggregateVerdict,
		"aggregate_confidence": c.AggregateConfidence,
		"consensus_score":      c.ConsensusScore,
		"notes":                c.Notes,
		"price_at_consult":     c.PriceAtConsult,
		"master_reports":       projectMasterRows(c.MasterReports),
		"tactic_reports":       projectTacticRows(c.TacticReports),
		"created_at":           c.CreatedAt,
	}
	if c.SymbolName != "" {
		out["symbol_name"] = c.SymbolName
	}
	return out
}

// disclaimerOK is the SEC Publishers' Exclusion gate. Returns
// true if:
//
//   - the server is running in RIA mode (Form ADV + Form CRS
//     are the per-user consent path; we don't gate per-request
//     in that mode); OR
//   - the compliance repo isn't wired (degraded boot / tests —
//     gate is open); OR
//   - the user has at least one ack row for surface 'advisor'
//     or 'global' at text_version >= 1.
//
// We deliberately fail OPEN when the repo errors. The
// alternative (fail closed on a transient DB error) would
// 500 every advisor consult during a brief DB blip, which is
// worse for users than the small risk of letting one extra
// consult through during the outage.
func (h *advisorHandler) disclaimerOK(ctx context.Context, userID string) bool {
	if h == nil {
		return true
	}
	if h.complianceMode == compliance.ModeRIARegistered {
		return true
	}
	if h.complianceRepo == nil {
		return true
	}
	gateCtx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
	defer cancel()
	ok, err := h.complianceRepo.HasAcknowledged(gateCtx, userID, "advisor", string(compliance.ModePublisher), 1)
	if err != nil {
		// Fail OPEN — see function docstring.
		return true
	}
	return ok
}
