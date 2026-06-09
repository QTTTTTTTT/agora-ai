// admin_advisor_reputation.go — Phase 5 admin surface for the
// /advisor reputation ledger.
//
// Endpoints
//
//   GET  /api/admin/advisor-reputation/stats[?kind=master|tactic&limit=N]
//        Lists every advisor-scoped per-agent stats row (fund_id IS NULL).
//        Filterable by AgentKind so admins can split master vs tactic
//        leaderboards without paging through both.
//
//   POST /api/admin/advisor-reputation/rebuild
//        Synchronously kicks one advisor backfill wave. Returns the
//        number of outcome rows that were written.
//
// Writes are audit-logged identically to the fund-scoped rebuild.

package main

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/fundai/server/internal/agentreputation"
	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
)

// advisorReputationRebuildSink is the narrow interface the admin
// handler needs from the advisor backfill loop.
// *advisorReputationLoop satisfies it; tests can substitute a stub.
type advisorReputationRebuildSink interface {
	RunOnce(ctx context.Context) (int, error)
}

func (h *adminHandler) registerAdvisorReputationAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.agentReputationRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/advisor-reputation/stats", h.handleAdminListAdvisorReputationStats)
	mux.HandleFunc("POST /api/admin/advisor-reputation/rebuild", h.handleAdminRebuildAdvisorReputation)
	mux.HandleFunc("POST /api/admin/advisor-reputation/coldstart", h.handleAdminAdvisorColdStart)
}

func (h *adminHandler) handleAdminListAdvisorReputationStats(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	p := agentreputation.ListStatsParams{AdvisorOnly: true}
	if v := strings.TrimSpace(q.Get("kind")); v != "" {
		p.AgentKind = agentreputation.AgentKind(v)
	}
	if v := strings.TrimSpace(q.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			p.Limit = n
		}
	}
	rows, err := h.agentReputationRepo.ListStats(r.Context(), p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"stats": projectAgentReputationStats(rows)})
}

type adminRebuildAdvisorReputationResponse struct {
	OutcomesWritten int    `json:"outcomes_written"`
	Status          string `json:"status"`
}

func (h *adminHandler) handleAdminRebuildAdvisorReputation(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.advisorReputationRebuildSink == nil {
		writeJSON(w, http.StatusServiceUnavailable,
			errorPayload("backfill_unavailable", "advisor reputation loop not wired"))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	n, err := h.advisorReputationRebuildSink.RunOnce(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "advisor_reputation.rebuild",
			TargetType:  "advisor_reputation",
			TargetID:    "",
			After: map[string]any{
				"outcomes_written": n,
			},
		})
	}
	writeJSON(w, http.StatusOK, adminRebuildAdvisorReputationResponse{
		OutcomesWritten: n,
		Status:          "ok",
	})
}
