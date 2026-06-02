// Order Cancel / Replace API (P0-5).
//
// This handler exposes:
//
//	POST /api/funds/{fundId}/orders/{tradeId}/cancel
//	POST /api/funds/{fundId}/orders/{tradeId}/replace
//
// They mutate live trades persisted in trade_executions. Both endpoints:
//
//   - require an authenticated user;
//   - verify the user owns the fund (via authorizeFundAccess →
//     fund_companies.owner_user_id);
//   - reject terminal trades (status in 'filled','cancelled','rejected',
//     'expired');
//   - emit an audit log entry to the hash-chained data_access_log so
//     a post-incident audit can reconstruct who cancelled / replaced
//     what, when, and from which IP;
//   - update the broker.Simulator's in-memory order when one is wired,
//     so a working limit replaced to a marketable price fills on the
//     spot.
//
// Why a separate handler
//
// FundHandler.TradeService is read-only by design — it surfaces
// historical fills for the dashboard. Cancel / Replace are state
// transitions and need different authz, validation, and side effects
// (audit + broker fan-out). Putting them in their own file keeps the
// concern legible and lets us evolve the API (TIF, stop edits, OCO
// cancels) without churning the read path.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/repository"
)

// orderActionsHandler is the HTTP surface of P0-5. Construct via
// newOrderActionsHandler from the wired Services.
type orderActionsHandler struct {
	tradeRepo   *repository.TradeRepo
	fundRepo    *repository.FundRepo
	companyRepo *repository.FundCompanyRepo
	auditLogger audit.Logger
	simulator   *broker.Simulator
	cfg         *Config // P0-7: needed by verifyStepUpToken; nil-safe in tests
	// P0-9: live-trading hard gate. Nil ⇒ legacy behaviour
	// (cancel/replace pass-through regardless of trading_mode).
	// Production wiring goes through newOrderActionsHandlerWithGate.
	gate *liveTradingGate
	log  *slog.Logger
}

func newOrderActionsHandler(svc *Services) *orderActionsHandler {
	return newOrderActionsHandlerWithConfig(svc, nil)
}

// newOrderActionsHandlerWithConfig is the production constructor —
// cfg is required to verify the X-Step-Up-Token header (P0-7).
// The legacy newOrderActionsHandler is preserved for unit tests
// that don't exercise step-up; they get nil cfg, which the
// verifier treats as "config unavailable" and reports as
// {Valid:false, Reason:"config unavailable"}, so audit metadata
// degrades to step_up=false rather than crashing.
func newOrderActionsHandlerWithConfig(svc *Services, cfg *Config) *orderActionsHandler {
	return newOrderActionsHandlerWithGate(svc, cfg, nil)
}

// newOrderActionsHandlerWithGate is the P0-9 production
// constructor. Same shape as the cfg variant plus a wired
// liveTradingGate. main.go uses this; existing tests call the
// nil-gate constructors and continue to exercise the legacy
// behaviour (gate not enforced).
func newOrderActionsHandlerWithGate(svc *Services, cfg *Config, gate *liveTradingGate) *orderActionsHandler {
	if svc == nil || svc.DB == nil {
		return nil
	}
	return &orderActionsHandler{
		tradeRepo:   repository.NewTradeRepo(svc.DB),
		fundRepo:    repository.NewFundRepo(svc.DB),
		companyRepo: repository.NewFundCompanyRepo(svc.DB),
		auditLogger: audit.NewDBLogger(svc.DB),
		simulator:   svc.BrokerSimulator,
		cfg:         cfg,
		gate:        gate,
		log:         slog.Default(),
	}
}

// RegisterRoutes wires the cancel + replace endpoints. Idempotent on
// nil receiver / mux to match the rest of the handler-registration
// pattern in main.go.
func (h *orderActionsHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/funds/{fundId}/orders/{tradeId}/cancel", h.handleCancel)
	mux.HandleFunc("POST /api/funds/{fundId}/orders/{tradeId}/replace", h.handleReplace)
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

// cancelOrderRequest is the JSON body for cancel. Both fields are
// optional — a bodyless request is treated as
// reason="user_requested".
type cancelOrderRequest struct {
	// Reason is one of the canonical short tags: "user_requested",
	// "ttl", "risk_breach", "system". Anything else is rewritten
	// to "user_requested" at the boundary so the audit aggregator
	// has a closed vocabulary.
	Reason string `json:"reason"`
	// Note is free-text rationale captured into the audit metadata
	// (NOT into trade_executions.cancel_reason — that column has
	// length and shape constraints).
	Note string `json:"note"`
}

// orderResponse is the trim wire shape returned by both endpoints.
// It is intentionally narrower than the full TradeExecution row so
// the API surface is forward-compatible with future broker fields
// without exposing internal state to the UI.
type orderResponse struct {
	ID           string  `json:"id"`
	FundID       string  `json:"fundId"`
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	OrderType    string  `json:"orderType"`
	Status       string  `json:"status"`
	Quantity     float64 `json:"quantity"`
	FilledQty    float64 `json:"filledQty"`
	LimitPrice   float64 `json:"limitPrice,omitempty"`
	StopPrice    float64 `json:"stopPrice,omitempty"`
	TrailAmount  float64 `json:"trailAmount,omitempty"`
	TrailPercent float64 `json:"trailPercent,omitempty"`
	DisplayQty   float64 `json:"displayQty,omitempty"`
	CancelReason string  `json:"cancelReason,omitempty"`
	ReplaceCount int     `json:"replaceCount"`
}

func (h *orderActionsHandler) handleCancel(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	tradeID := strings.TrimSpace(r.PathValue("tradeId"))
	if fundID == "" || tradeID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and tradeId required"))
		return
	}
	// Body is optional — empty body ⇒ user_requested.
	var body cancelOrderRequest
	if r.ContentLength > 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}
	reason := normaliseCancelReason(body.Reason)

	ctx := r.Context()
	fund, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID)
	if err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	// P0-9 live-trading hard gate. We compute readiness even when
	// the gate is nil (legacy / test wiring) — checkLiveGate
	// returns Ready=true / GateEnforced=false in that case so the
	// audit metadata still records the (degraded) state.
	readiness := h.checkLiveGate(ctx, fund, userID, r)
	if readiness.GateEnforced && !readiness.Ready {
		writeOrderActionJSON(w, http.StatusForbidden, errorPayload(string(readiness.FirstFailing),
			"live trading prerequisite not met: "+string(readiness.FirstFailing)))
		return
	}

	// Snapshot the pre-mutation state so the audit row carries a
	// before/after diff.
	before, err := h.tradeRepo.GetByIDForFund(ctx, fundID, tradeID)
	if err != nil {
		writeOrderActionFromRepoError(w, err)
		return
	}

	if err := h.tradeRepo.CancelOrder(ctx, fundID, tradeID, reason); err != nil {
		writeOrderActionFromRepoError(w, err)
		return
	}

	// Best-effort cancel on the broker simulator — if the order was
	// also booked there (broker_order_id is set), match the DB
	// state. We deliberately ignore "not found / already
	// terminal" errors so a DB-only order doesn't produce a false
	// 500.
	if h.simulator != nil && before.BrokerOrderID.Valid && before.BrokerOrderID.String != "" {
		if simErr := h.simulator.CancelOrder(ctx, broker.CancelOrderRequest{
			FundID:        fundID,
			BrokerOrderID: before.BrokerOrderID.String,
		}); simErr != nil {
			if !errors.Is(simErr, broker.ErrOrderNotFound) && !errors.Is(simErr, broker.ErrOrderTerminal) {
				h.log.Warn("simulator cancel failed (db state already cancelled)",
					"fund_id", fundID, "trade_id", tradeID, "err", simErr.Error())
			}
		}
	}

	after, err := h.tradeRepo.GetByIDForFund(ctx, fundID, tradeID)
	if err != nil {
		writeOrderActionFromRepoError(w, err)
		return
	}

	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "trade.cancel",
		TargetType:  "trade_execution",
		TargetID:    tradeID,
		Before:      tradeAuditSnapshot(before),
		After:       tradeAuditSnapshot(after),
		Metadata: mergeAuditMetadata(map[string]any{
			"fund_id":     fundID,
			"reason":      reason,
			"note":        strings.TrimSpace(body.Note),
			"client_addr": clientIP(r),
		},
			stepUpAuditMetadata(verifyStepUpToken(r, h.cfg)),
			liveGateAuditMetadata(readiness)),
	})

	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"order": tradeToOrderResponse(after),
	})
}

// ---------------------------------------------------------------------------
// Replace
// ---------------------------------------------------------------------------

// replaceOrderRequest is the JSON body for replace. Every field is
// pointer-as-no-change so a request can update a single attribute
// (most commonly limit_price).
type replaceOrderRequest struct {
	Quantity     *float64 `json:"quantity"`
	LimitPrice   *float64 `json:"limitPrice"`
	StopPrice    *float64 `json:"stopPrice"`
	TrailAmount  *float64 `json:"trailAmount"`
	TrailPercent *float64 `json:"trailPercent"`
	DisplayQty   *float64 `json:"displayQty"`
	Note         string   `json:"note"`
}

func (h *orderActionsHandler) handleReplace(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeOrderActionJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "missing or invalid bearer token"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	tradeID := strings.TrimSpace(r.PathValue("tradeId"))
	if fundID == "" || tradeID == "" {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId and tradeId required"))
		return
	}
	if r.ContentLength <= 0 {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "replace requires a JSON body"))
		return
	}
	var body replaceOrderRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}
	fields := repository.ReplaceTradeFields{
		Quantity:     body.Quantity,
		LimitPrice:   body.LimitPrice,
		StopPrice:    body.StopPrice,
		TrailAmount:  body.TrailAmount,
		TrailPercent: body.TrailPercent,
		DisplayQty:   body.DisplayQty,
	}
	if !fields.HasChanges() {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", "replace requires at least one of: quantity, limitPrice, stopPrice, trailAmount, trailPercent, displayQty"))
		return
	}
	if err := validateReplaceFields(fields); err != nil {
		writeOrderActionJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
		return
	}

	ctx := r.Context()
	fund, err := authorizeFundAccess(ctx, h.fundRepo, h.companyRepo, userID, fundID)
	if err != nil {
		writeOrderActionFromAuthError(w, err)
		return
	}

	// P0-9 live-trading hard gate (replace path). Same wiring as
	// handleCancel — see the long-form comment there.
	readiness := h.checkLiveGate(ctx, fund, userID, r)
	if readiness.GateEnforced && !readiness.Ready {
		writeOrderActionJSON(w, http.StatusForbidden, errorPayload(string(readiness.FirstFailing),
			"live trading prerequisite not met: "+string(readiness.FirstFailing)))
		return
	}

	before, err := h.tradeRepo.GetByIDForFund(ctx, fundID, tradeID)
	if err != nil {
		writeOrderActionFromRepoError(w, err)
		return
	}

	updated, err := h.tradeRepo.ReplaceOrderFields(ctx, fundID, tradeID, fields)
	if err != nil {
		writeOrderActionFromRepoError(w, err)
		return
	}

	if h.simulator != nil && before.BrokerOrderID.Valid && before.BrokerOrderID.String != "" {
		if _, simErr := h.simulator.ReplaceOrder(ctx, broker.ReplaceOrderRequest{
			FundID:          fundID,
			BrokerOrderID:   before.BrokerOrderID.String,
			NewQuantity:     fields.Quantity,
			NewLimitPrice:   fields.LimitPrice,
			NewStopPrice:    fields.StopPrice,
			NewTrailAmount:  fields.TrailAmount,
			NewTrailPercent: fields.TrailPercent,
			NewDisplayQty:   fields.DisplayQty,
		}); simErr != nil {
			if !errors.Is(simErr, broker.ErrOrderNotFound) && !errors.Is(simErr, broker.ErrOrderTerminal) {
				h.log.Warn("simulator replace failed (db state already updated)",
					"fund_id", fundID, "trade_id", tradeID, "err", simErr.Error())
			}
		}
	}

	h.logAudit(ctx, audit.MutationEvent{
		ActorUserID: userID,
		Action:      "trade.replace",
		TargetType:  "trade_execution",
		TargetID:    tradeID,
		Before:      tradeAuditSnapshot(before),
		After:       tradeAuditSnapshot(updated),
		Metadata: mergeAuditMetadata(map[string]any{
			"fund_id":      fundID,
			"changes":      replaceFieldsAuditMap(fields),
			"note":         strings.TrimSpace(body.Note),
			"replace_count": updated.ReplaceCount,
			"client_addr":  clientIP(r),
		},
			stepUpAuditMetadata(verifyStepUpToken(r, h.cfg)),
			liveGateAuditMetadata(readiness)),
	})

	writeOrderActionJSON(w, http.StatusOK, map[string]any{
		"order": tradeToOrderResponse(updated),
	})
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// checkLiveGate is the per-handler entry point to the P0-9 hard
// gate. We load the user's KYC info on demand here (not on every
// request) so legacy callers that never reach a live fund pay
// nothing extra. Errors loading the user fall back to a deny:
// the gate's KYC pillar will refuse the action, which is the
// correct posture for an unrecoverable lookup failure.
//
// When h.gate is nil (test wiring) we still build a synthetic
// LiveReadiness so audit metadata stays well-shaped — the
// returned readiness has GateEnforced=false, so the calling
// handler treats it as pass-through.
func (h *orderActionsHandler) checkLiveGate(ctx context.Context, fund *repository.Fund, userID string, r *http.Request) LiveReadiness {
	if h == nil || h.gate == nil {
		// Legacy / test wiring — no gate. Audit log records
		// trading_mode but flags GateEnforced=false so a future
		// audit query can detect bypassed prod traffic.
		mode := ""
		if fund != nil {
			mode = fund.TradingMode
		}
		return LiveReadiness{Ready: true, TradingMode: mode, GateEnforced: false}
	}
	user, loadErr := loadActiveUserByID(ctx, h.gate.db, userID)
	if loadErr != nil {
		// We synthesize a deny-by-default user for live funds:
		// KYCStatus is left empty, which fails the KYC pillar.
		// For non-live funds the gate would have bypassed anyway.
		user = &authenticatedUser{ID: userID}
	}
	return h.gate.check(ctx, fund, user, r)
}

// liveGateAuditMetadata returns the gate-relevant fields to merge
// into a mutation event. Even when the gate isn't enforced we
// log trading_mode + gate_enforced=false so post-incident queries
// can answer "how many live mutations bypassed the gate?".
func liveGateAuditMetadata(rd LiveReadiness) map[string]any {
	out := map[string]any{
		"trading_mode":      rd.TradingMode,
		"live_gate_enforced": rd.GateEnforced,
		"live_gate_ready":   rd.Ready,
	}
	if rd.GateEnforced {
		out["live_gate_kyc_ok"] = rd.KYCOK
		out["live_gate_broker_link_ok"] = rd.BrokerLinkOK
		out["live_gate_2fa_ok"] = rd.TwoFAOK
		out["live_gate_step_up_ok"] = rd.StepUpOK
		if rd.FirstFailing != "" {
			out["live_gate_first_failing"] = string(rd.FirstFailing)
		}
		if rd.BrokerLinkID != "" {
			out["live_gate_broker_link_id"] = rd.BrokerLinkID
		}
	}
	return out
}

// canonicalCancelReasons is the closed vocabulary the audit aggregator
// expects. Anything outside is rewritten to user_requested at the
// boundary.
var canonicalCancelReasons = map[string]bool{
	"user_requested":        true,
	"superseded_by_replace": true,
	"ttl":                   true,
	"risk_breach":           true,
	"system":                true,
}

func normaliseCancelReason(in string) string {
	in = strings.TrimSpace(strings.ToLower(in))
	if canonicalCancelReasons[in] {
		return in
	}
	return "user_requested"
}

// validateReplaceFields enforces the same per-field positivity guards
// the simulator and repo enforce, but at the HTTP edge so we return
// 400 instead of 500 on bad input.
func validateReplaceFields(f repository.ReplaceTradeFields) error {
	if f.Quantity != nil && *f.Quantity <= 0 {
		return fmt.Errorf("quantity must be > 0")
	}
	if f.LimitPrice != nil && *f.LimitPrice <= 0 {
		return fmt.Errorf("limitPrice must be > 0")
	}
	if f.StopPrice != nil && *f.StopPrice <= 0 {
		return fmt.Errorf("stopPrice must be > 0")
	}
	if f.TrailAmount != nil && *f.TrailAmount <= 0 {
		return fmt.Errorf("trailAmount must be > 0")
	}
	if f.TrailPercent != nil && (*f.TrailPercent <= 0 || *f.TrailPercent >= 1) {
		return fmt.Errorf("trailPercent must be in (0, 1)")
	}
	if f.DisplayQty != nil && *f.DisplayQty <= 0 {
		return fmt.Errorf("displayQty must be > 0")
	}
	return nil
}

func tradeToOrderResponse(t *repository.TradeExecution) orderResponse {
	if t == nil {
		return orderResponse{}
	}
	resp := orderResponse{
		ID:           t.ID,
		FundID:       t.FundID,
		Symbol:       t.Symbol,
		Side:         t.Side,
		OrderType:    t.OrderType,
		Status:       t.Status,
		Quantity:     t.Quantity,
		FilledQty:    t.FilledQty,
		ReplaceCount: t.ReplaceCount,
	}
	if t.Price.Valid {
		resp.LimitPrice = t.Price.Float64
	}
	if t.StopPrice.Valid {
		resp.StopPrice = t.StopPrice.Float64
	}
	if t.TrailAmount.Valid {
		resp.TrailAmount = t.TrailAmount.Float64
	}
	if t.TrailPercent.Valid {
		resp.TrailPercent = t.TrailPercent.Float64
	}
	if t.DisplayQty.Valid {
		resp.DisplayQty = t.DisplayQty.Float64
	}
	if t.CancelReason.Valid {
		resp.CancelReason = t.CancelReason.String
	}
	return resp
}

// tradeAuditSnapshot returns a stable, JSON-serialisable subset of the
// trade row. We deliberately drop noisy / volatile fields (created_at,
// idempotency key) so the audit chain hash is dominated by the
// fields the user actually touched.
func tradeAuditSnapshot(t *repository.TradeExecution) map[string]any {
	if t == nil {
		return nil
	}
	out := map[string]any{
		"id":            t.ID,
		"fund_id":       t.FundID,
		"symbol":        t.Symbol,
		"side":          t.Side,
		"order_type":    t.OrderType,
		"status":        t.Status,
		"quantity":      t.Quantity,
		"filled_qty":    t.FilledQty,
		"replace_count": t.ReplaceCount,
	}
	if t.Price.Valid {
		out["price"] = t.Price.Float64
	}
	if t.StopPrice.Valid {
		out["stop_price"] = t.StopPrice.Float64
	}
	if t.TrailAmount.Valid {
		out["trail_amount"] = t.TrailAmount.Float64
	}
	if t.TrailPercent.Valid {
		out["trail_percent"] = t.TrailPercent.Float64
	}
	if t.DisplayQty.Valid {
		out["display_qty"] = t.DisplayQty.Float64
	}
	if t.CancelReason.Valid {
		out["cancel_reason"] = t.CancelReason.String
	}
	if t.CancelledAt.Valid {
		out["cancelled_at"] = t.CancelledAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if t.ReplacedAt.Valid {
		out["replaced_at"] = t.ReplacedAt.Time.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

func replaceFieldsAuditMap(f repository.ReplaceTradeFields) map[string]any {
	out := map[string]any{}
	if f.Quantity != nil {
		out["quantity"] = *f.Quantity
	}
	if f.LimitPrice != nil {
		out["limit_price"] = *f.LimitPrice
	}
	if f.StopPrice != nil {
		out["stop_price"] = *f.StopPrice
	}
	if f.TrailAmount != nil {
		out["trail_amount"] = *f.TrailAmount
	}
	if f.TrailPercent != nil {
		out["trail_percent"] = *f.TrailPercent
	}
	if f.DisplayQty != nil {
		out["display_qty"] = *f.DisplayQty
	}
	return out
}

// logAudit forwards a mutation event to the audit logger, swallowing
// errors. We deliberately do NOT roll back the cancel/replace on an
// audit-write failure — losing one audit row is preferable to a
// half-cancelled order that the user thinks succeeded. The chain
// verifier (P0-8 audit_verify_handler) surfaces gaps post hoc.
func (h *orderActionsHandler) logAudit(ctx context.Context, ev audit.MutationEvent) {
	if h == nil || h.auditLogger == nil {
		return
	}
	if err := h.auditLogger.LogMutation(ctx, ev); err != nil {
		h.log.Warn("order action audit write failed",
			"action", ev.Action, "target_id", ev.TargetID, "err", err.Error())
	}
}

func writeOrderActionJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func errorPayload(code, detail string) map[string]any {
	return map[string]any{"error": code, "detail": detail}
}

// writeOrderActionFromAuthError maps authorizeFundAccess errors to
// the right HTTP code without leaking which arm of the check failed.
func writeOrderActionFromAuthError(w http.ResponseWriter, err error) {
	if errors.Is(err, api.ErrForbidden) {
		writeOrderActionJSON(w, http.StatusForbidden, errorPayload("forbidden", "you do not have access to this fund"))
		return
	}
	if errors.Is(err, api.ErrNotFound) {
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "fund not found"))
		return
	}
	writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
}

// writeOrderActionFromRepoError maps trade-repo errors to the right
// HTTP codes. We collapse "not cancellable" / "not replaceable" to
// 409 so the UI can disable the button and show "已成交，无法取消" /
// similar without parsing a free-text body.
func writeOrderActionFromRepoError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeOrderActionJSON(w, http.StatusNotFound, errorPayload("not_found", "order not found"))
	case errors.Is(err, repository.ErrTradeNotCancellable):
		writeOrderActionJSON(w, http.StatusConflict, errorPayload("not_cancellable", "order is in a terminal state"))
	case errors.Is(err, repository.ErrTradeNotReplaceable):
		writeOrderActionJSON(w, http.StatusConflict, errorPayload("not_replaceable", "order is terminal or has reached the replace cap"))
	default:
		writeOrderActionJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
	}
}

// clientIP returns the best-effort client address for audit metadata.
// Honours X-Forwarded-For when present (single hop only — proxies
// further out should be handled at the LB layer).
func clientIP(r *http.Request) string {
	if r == nil {
		return ""
	}
	if xff := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); xff != "" {
		if comma := strings.IndexByte(xff, ','); comma > 0 {
			return strings.TrimSpace(xff[:comma])
		}
		return xff
	}
	return r.RemoteAddr
}
