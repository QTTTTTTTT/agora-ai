package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/quota"
)

// registerQuotaRoutes mounts the F28 quota admin surface. Routes are
// intentionally kebab-case in URLs (consistent with other admin
// resources) and snake_case in JSON payloads (matches the existing
// admin convention).
func (h *adminHandler) registerQuotaRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/quotas/{fundId}", h.handleGetFundQuota)
	mux.HandleFunc("PUT /api/admin/quotas/{fundId}", h.handleUpsertFundQuota)
	mux.HandleFunc("DELETE /api/admin/quotas/{fundId}", h.handleDeleteFundQuota)
	// "_default_" is the platform-default sentinel — chosen over an
	// empty path segment so the routing pattern stays unambiguous.
	mux.HandleFunc("GET /api/admin/quotas/_default_", h.handleGetDefaultQuota)
	mux.HandleFunc("PUT /api/admin/quotas/_default_", h.handleUpsertDefaultQuota)
}

type quotaPayload struct {
	MaxActiveAgents        *int64  `json:"max_active_agents"`
	MaxConcurrentWorkflows *int64  `json:"max_concurrent_workflows"`
	DailyLLMTokenLimit     *int64  `json:"daily_llm_token_limit"`
	MonthlyLLMTokenLimit   *int64  `json:"monthly_llm_token_limit"`
	Notes                  *string `json:"notes"`
}

func (h *adminHandler) handleGetFundQuota(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.quotaService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "quota service unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId is required"})
		return
	}
	q, usage, err := h.quotaService.Snapshot(r.Context(), fundID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"quota": marshalQuota(q),
		"usage": marshalUsage(usage),
	})
}

func (h *adminHandler) handleUpsertFundQuota(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.quotaService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "quota service unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId is required"})
		return
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var payload quotaPayload
	if err := dec.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": err.Error()})
		return
	}
	q, err := h.quotaService.UpsertQuota(r.Context(), quota.UpsertQuotaInput{
		FundID:                 fundID,
		MaxActiveAgents:        payload.MaxActiveAgents,
		MaxConcurrentWorkflows: payload.MaxConcurrentWorkflows,
		DailyLLMTokenLimit:     payload.DailyLLMTokenLimit,
		MonthlyLLMTokenLimit:   payload.MonthlyLLMTokenLimit,
		Notes:                  payload.Notes,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": marshalQuota(q)})
}

func (h *adminHandler) handleDeleteFundQuota(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.quotaService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "quota service unavailable"})
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "fundId is required"})
		return
	}
	if err := h.quotaService.DeleteQuota(r.Context(), fundID); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusNoContent, nil)
}

func (h *adminHandler) handleGetDefaultQuota(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.quotaService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "quota service unavailable"})
		return
	}
	q, err := h.quotaService.EffectiveQuota(r.Context(), "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": marshalQuota(q)})
}

func (h *adminHandler) handleUpsertDefaultQuota(w http.ResponseWriter, r *http.Request) {
	if !requireSuperAdmin(w, r) {
		return
	}
	if h.quotaService == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "quota service unavailable"})
		return
	}
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	var payload quotaPayload
	if err := dec.Decode(&payload); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid request body", "detail": err.Error()})
		return
	}
	q, err := h.quotaService.UpsertQuota(r.Context(), quota.UpsertQuotaInput{
		FundID:                 "",
		MaxActiveAgents:        payload.MaxActiveAgents,
		MaxConcurrentWorkflows: payload.MaxConcurrentWorkflows,
		DailyLLMTokenLimit:     payload.DailyLLMTokenLimit,
		MonthlyLLMTokenLimit:   payload.MonthlyLLMTokenLimit,
		Notes:                  payload.Notes,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"quota": marshalQuota(q)})
}

// marshalQuota converts FundQuota to a JSON-friendly map. Null limit
// fields render as JSON null so callers can distinguish "unlimited"
// from "limit of 0".
func marshalQuota(q *quota.FundQuota) map[string]any {
	if q == nil {
		return nil
	}
	out := map[string]any{
		"fund_id":                  q.FundID,
		"max_active_agents":        nullInt64ToAny(q.MaxActiveAgents),
		"max_concurrent_workflows": nullInt64ToAny(q.MaxConcurrentWorkflows),
		"daily_llm_token_limit":    nullInt64ToAny(q.DailyLLMTokenLimit),
		"monthly_llm_token_limit":  nullInt64ToAny(q.MonthlyLLMTokenLimit),
		"updated_at":               q.UpdatedAt,
	}
	if q.Notes != "" {
		out["notes"] = q.Notes
	}
	return out
}

func marshalUsage(u *quota.Usage) map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"fund_id":              u.FundID,
		"date":                 u.Date,
		"active_agents":        u.ActiveAgents,
		"concurrent_workflows": u.ConcurrentWorkflows,
		"daily_tokens":         u.DailyTokens,
		"monthly_tokens":       u.MonthlyTokens,
	}
}

func nullInt64ToAny(v sql.NullInt64) any {
	if !v.Valid {
		return nil
	}
	return v.Int64
}
