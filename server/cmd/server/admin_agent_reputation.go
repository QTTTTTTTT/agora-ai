// admin_agent_reputation.go — S8.4 cross-fund admin view +
// rebuild trigger for the agent reputation ledger.
//
// Endpoints
//
//   GET  /api/admin/agent-reputation/stats[?fund_id=X&kind=Y&limit=N]
//        Cross-fund per-agent stats view.
//
//   POST /api/admin/agent-reputation/rebuild
//        Body: {"fund_id": "..."} (optional — empty = all funds)
//        Synchronously kicks one backfill wave. Returns the
//        number of outcome rows it wrote.
//
// Writes (rebuild) are audit-logged.

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
)

// agentReputationRebuildSink is the narrow interface the admin
// handler needs from the backfill loop. The loop satisfies it
// via RunOnce + RetargetFund; tests can substitute a stub.
type agentReputationRebuildSink interface {
	RebuildForFund(ctx context.Context, fundID string) (int, error)
}

func (h *adminHandler) registerAgentReputationAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.agentReputationRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/agent-reputation/stats", h.handleAdminListAgentReputationStats)
	mux.HandleFunc("POST /api/admin/agent-reputation/rebuild", h.handleAdminRebuildAgentReputation)
}

func (h *adminHandler) handleAdminListAgentReputationStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	fundID := strings.TrimSpace(q.Get("fund_id"))
	params := buildStatsParams(r, fundID)
	rows, err := h.agentReputationRepo.ListStats(r.Context(), params)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": projectAgentReputationStats(rows)})
}

type adminRebuildAgentReputationRequest struct {
	FundID string `json:"fund_id,omitempty"`
}

type adminRebuildAgentReputationResponse struct {
	OutcomesWritten int    `json:"outcomes_written"`
	Status          string `json:"status"`
}

func (h *adminHandler) handleAdminRebuildAgentReputation(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.agentReputationRebuildSink == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("backfill_unavailable", "no backfill loop wired"))
		return
	}
	var req adminRebuildAgentReputationRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}
	userID, _ := api.AuthenticatedUserID(r)
	fundID := strings.TrimSpace(req.FundID)
	n, err := h.agentReputationRebuildSink.RebuildForFund(r.Context(), fundID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "agent_reputation.rebuild",
			TargetType:  "agent_reputation",
			TargetID:    fundID,
			After: map[string]any{
				"fund_id":          fundID,
				"outcomes_written": n,
			},
		})
	}
	writeJSON(w, http.StatusOK, adminRebuildAgentReputationResponse{
		OutcomesWritten: n,
		Status:          "ok",
	})
}
