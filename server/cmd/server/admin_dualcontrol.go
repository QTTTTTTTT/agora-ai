package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/subscription"
)

// registerDualControlActions installs handlers for every super_admin
// mutation that must go through two-person approval. ADD ALL NEW
// SENSITIVE ACTIONS HERE — wiring an action to the dual-control queue
// is opt-in. Until an action is registered, /api/admin/requests will
// reject submissions with ErrUnknownAdminAction.
//
// Action naming convention: snake_case verb_object, matching the
// audit.MutationEvent.Action of the underlying handler so log readers
// can trivially join admin_change_log ↔ admin_requests by action.
//
// Each handler receives an *sql.Tx so its writes commit atomically
// with the admin_requests status flip. Audit rows MUST be recorded
// with LogMutationTx for the same reason.
func (h *adminHandler) registerDualControlActions() {
	if h == nil || h.dualControl == nil {
		return
	}
	h.dualControl.Register("update_platform_settings", h.executeUpdatePlatformSettings)
	h.dualControl.Register("upsert_llm_budget", h.executeUpsertLLMBudget)
}

// adminRequestRoutes wires the dual-control HTTP surface. Kept in a
// dedicated method so RegisterRoutes stays scannable and tests can
// mount this subset onto a stripped-down mux when they want to focus
// on the approval flow.
func (h *adminHandler) registerDualControlRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("POST /api/admin/requests", h.handleSubmitAdminRequest)
	mux.HandleFunc("GET /api/admin/requests", h.handleListAdminRequests)
	mux.HandleFunc("GET /api/admin/requests/{id}", h.handleGetAdminRequest)
	mux.HandleFunc("POST /api/admin/requests/{id}/approve", h.handleApproveAdminRequest)
	mux.HandleFunc("POST /api/admin/requests/{id}/reject", h.handleRejectAdminRequest)
}

type submitAdminRequestPayload struct {
	Action     string         `json:"action"`
	TargetType string         `json:"target_type"`
	TargetID   string         `json:"target_id"`
	Payload    map[string]any `json:"payload"`
	Reason     string         `json:"reason"`
	TTLSeconds int            `json:"ttl_seconds,omitempty"`
}

func (h *adminHandler) handleSubmitAdminRequest(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.dualControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dual control not configured"})
		return
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var payload submitAdminRequestPayload
	if err := dec.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": err.Error()})
		return
	}
	if strings.TrimSpace(payload.Action) == "" || strings.TrimSpace(payload.TargetType) == "" || strings.TrimSpace(payload.TargetID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "action, target_type and target_id are required"})
		return
	}

	requesterID, _ := api.AuthenticatedUserID(r)
	req, err := h.dualControl.Submit(r.Context(), audit.SubmitInput{
		RequesterUserID: requesterID,
		Action:          payload.Action,
		TargetType:      payload.TargetType,
		TargetID:        payload.TargetID,
		Payload:         payload.Payload,
		Reason:          payload.Reason,
		TTL:             ttlFromSeconds(payload.TTLSeconds),
	})
	if err != nil {
		writeJSON(w, statusForDualControlError(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, marshalAdminRequest(req))
}

func (h *adminHandler) handleListAdminRequests(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.dualControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dual control not configured"})
		return
	}
	status := r.URL.Query().Get("status")
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	rows, err := h.dualControl.List(r.Context(), status, limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		out = append(out, marshalAdminRequest(row))
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (h *adminHandler) handleGetAdminRequest(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.dualControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dual control not configured"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	req, err := h.dualControl.Get(r.Context(), id)
	if err != nil {
		writeJSON(w, statusForDualControlError(err), map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, marshalAdminRequest(req))
}

func (h *adminHandler) handleApproveAdminRequest(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.dualControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dual control not configured"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	approverID, _ := api.AuthenticatedUserID(r)
	req, details, err := h.dualControl.ApproveAndExecute(r.Context(), id, approverID)
	if err != nil {
		writeJSON(w, statusForDualControlError(err), map[string]any{"error": err.Error(), "request": maybeMarshalAdminRequest(req)})
		return
	}
	resp := marshalAdminRequest(req)
	if details != nil {
		resp["result"] = details
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *adminHandler) handleRejectAdminRequest(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.dualControl == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "dual control not configured"})
		return
	}
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "id is required"})
		return
	}
	defer r.Body.Close()
	var body struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // reason is optional
	approverID, _ := api.AuthenticatedUserID(r)
	req, err := h.dualControl.Reject(r.Context(), id, approverID, body.Reason)
	if err != nil {
		writeJSON(w, statusForDualControlError(err), map[string]any{"error": err.Error(), "request": maybeMarshalAdminRequest(req)})
		return
	}
	writeJSON(w, http.StatusOK, marshalAdminRequest(req))
}

// statusForDualControlError translates audit sentinels into the HTTP
// codes our clients expect. Keep this exhaustive: a missing mapping
// silently surfaces as 500 which is worse than wrong-but-explicit.
func statusForDualControlError(err error) int {
	switch {
	case errors.Is(err, audit.ErrRequestNotFound):
		return http.StatusNotFound
	case errors.Is(err, audit.ErrRequestSelfApproval),
		errors.Is(err, audit.ErrRequesterNotSuperAdmin),
		errors.Is(err, audit.ErrApproverNotSuperAdmin):
		return http.StatusForbidden
	case errors.Is(err, audit.ErrRequestAlreadyFinal),
		errors.Is(err, audit.ErrRequestExpired):
		return http.StatusConflict
	case errors.Is(err, audit.ErrUnknownAdminAction):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func ttlFromSeconds(seconds int) time.Duration {
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

// maybeMarshalAdminRequest is a nil-safe wrapper for error responses
// where the request may or may not have been loaded successfully.
func maybeMarshalAdminRequest(req *audit.AdminRequest) any {
	if req == nil {
		return nil
	}
	return marshalAdminRequest(req)
}

func marshalAdminRequest(req *audit.AdminRequest) map[string]any {
	out := map[string]any{
		"id":                req.ID,
		"requester_user_id": req.RequesterUserID,
		"action":            req.Action,
		"target_type":       req.TargetType,
		"target_id":         req.TargetID,
		"status":            req.Status,
		"expires_at":        req.ExpiresAt,
		"created_at":        req.CreatedAt,
		"updated_at":        req.UpdatedAt,
	}
	if len(req.Payload) > 0 {
		out["payload"] = json.RawMessage(req.Payload)
	}
	if req.Reason != "" {
		out["reason"] = req.Reason
	}
	if req.ApproverUserID.Valid {
		out["approver_user_id"] = req.ApproverUserID.String
	}
	if req.ApprovedAt.Valid {
		out["approved_at"] = req.ApprovedAt.Time
	}
	if req.ExecutedAt.Valid {
		out["executed_at"] = req.ExecutedAt.Time
	}
	if req.ExecutionError.Valid {
		out["execution_error"] = req.ExecutionError.String
	}
	return out
}

// --- registered action executors -------------------------------------------

// executeUpdatePlatformSettings is the dual-control replay of
// PUT /api/admin/platform-settings. It expects the same payload shape
// as the direct endpoint so a future migration to "approval-only" can
// be done by simply rejecting the direct route. Snapshots the prior
// settings into admin_change_log so reviewers see exactly what changed.
func (h *adminHandler) executeUpdatePlatformSettings(ctx context.Context, tx *sql.Tx, req audit.AdminRequest) (map[string]any, error) {
	var payload struct {
		AccessMode                 string `json:"access_mode"`
		DefaultTeamIntervalMinutes int    `json:"default_team_interval_minutes"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}

	before, err := h.subscriptionService.GetPlatformSettings(ctx)
	if err != nil {
		return nil, fmt.Errorf("load current settings: %w", err)
	}
	after, err := h.subscriptionService.UpdatePlatformSettings(ctx, &subscription.PlatformSettings{
		AccessMode:                 payload.AccessMode,
		DefaultTeamIntervalMinutes: payload.DefaultTeamIntervalMinutes,
	})
	if err != nil {
		return nil, fmt.Errorf("apply settings: %w", err)
	}
	approver := ""
	if req.ApproverUserID.Valid {
		approver = req.ApproverUserID.String
	}
	if err := h.auditLogger.LogMutationTx(ctx, tx, audit.MutationEvent{
		ActorUserID: approver,
		Action:      "update_platform_settings",
		TargetType:  req.TargetType,
		TargetID:    req.TargetID,
		RequestID:   req.ID,
		Before:      before,
		After:       after,
		Metadata:    map[string]any{"requester": req.RequesterUserID},
	}); err != nil {
		return nil, fmt.Errorf("audit mutation: %w", err)
	}
	return map[string]any{"settings": after}, nil
}

// executeUpsertLLMBudget replays PUT /api/admin/llm-budgets/{userId}
// through the dual-control queue. Budget raises are the canonical
// "small input that costs real money" change, so this is the most
// valuable action to gate on two-person approval. Payload shape mirrors
// upsertLLMBudgetPayload in admin_handler.go.
func (h *adminHandler) executeUpsertLLMBudget(ctx context.Context, tx *sql.Tx, req audit.AdminRequest) (map[string]any, error) {
	if h.budgetService == nil {
		return nil, errors.New("budget service unavailable")
	}
	var payload struct {
		UserID            string   `json:"user_id"`
		FundID            string   `json:"fund_id"`
		DailyLimitCents   *float64 `json:"daily_limit_cents"`
		MonthlyLimitCents *float64 `json:"monthly_limit_cents"`
	}
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		return nil, fmt.Errorf("decode payload: %w", err)
	}
	if strings.TrimSpace(payload.UserID) == "" {
		payload.UserID = req.TargetID
	}

	beforeRow, _ := h.budgetService.GetBudget(ctx, payload.UserID, payload.FundID)
	after, err := h.budgetService.UpsertBudget(ctx, payload.UserID, payload.FundID, payload.DailyLimitCents, payload.MonthlyLimitCents)
	if err != nil {
		return nil, fmt.Errorf("upsert budget: %w", err)
	}
	approver := ""
	if req.ApproverUserID.Valid {
		approver = req.ApproverUserID.String
	}
	if err := h.auditLogger.LogMutationTx(ctx, tx, audit.MutationEvent{
		ActorUserID: approver,
		Action:      "upsert_llm_budget",
		TargetType:  "user_llm_budget",
		TargetID:    payload.UserID,
		RequestID:   req.ID,
		Before:      beforeRow,
		After:       after,
		Metadata:    map[string]any{"requester": req.RequesterUserID, "fund_id": payload.FundID},
	}); err != nil {
		return nil, fmt.Errorf("audit mutation: %w", err)
	}
	return map[string]any{"budget": after}, nil
}
