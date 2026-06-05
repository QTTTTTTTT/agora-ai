// fund_workflow_checkpoints_handler.go — S9.2 per-step workflow
// snapshot read-only view for fund owners.
//
//	GET /api/funds/{fundId}/workflow-checkpoints?trading_date=YYYY-MM-DD
//
// Mirrors the data exposed by /api/admin/workflow-checkpoints, but
// scoped by path so:
//
//   - Auth uses authorizeFundAccess (the fund's company owner)
//     instead of requireAdmin. Platform admins still have access
//     because they pass the company-owner check at the wiring
//     layer.
//   - The repo call is exclusively ListByFundAndDate(fundID, date),
//     which is hard-bounded to the path's fundId so a malicious
//     query string cannot leak rows from other funds.
//   - Resume is intentionally NOT exposed here. Re-firing a step
//     can re-execute paid LLM calls and external broker
//     instructions; that decision stays with platform operators
//     via /api/admin/workflow-checkpoints/resume. Fund owners who
//     need a re-run contact support, who can authorise via the
//     admin route while leaving the audit trail in place.
//
// The handler returns the same workflowCheckpointWire shape used
// by the admin handler (defined in admin_workflow_checkpoints.go),
// so the web client can render both views with one component.

package main

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/repository"
)

type fundWorkflowCheckpointsHandler struct {
	workflowCheckpointRepo *repository.WorkflowCheckpointRepo
	fundRepo               *repository.FundRepo
	companyRepo            *repository.FundCompanyRepo
}

// newFundWorkflowCheckpointsHandler wires from Services. Returns
// nil when the checkpoint repo is missing — the routes then stay
// unregistered and the web view degrades to "feature unavailable"
// the same way fund_llm_overrides_handler does. This keeps boot
// resilient when the optional S9.2 schema slice has not been
// migrated (e.g. legacy DR rebuilds, downstream forks).
func newFundWorkflowCheckpointsHandler(svc *Services) *fundWorkflowCheckpointsHandler {
	if svc == nil || svc.WorkflowCheckpointRepo == nil || svc.DB == nil {
		return nil
	}
	return &fundWorkflowCheckpointsHandler{
		workflowCheckpointRepo: svc.WorkflowCheckpointRepo,
		fundRepo:               repository.NewFundRepo(svc.DB),
		companyRepo:            repository.NewFundCompanyRepo(svc.DB),
	}
}

func (h *fundWorkflowCheckpointsHandler) RegisterRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil {
		return
	}
	mux.HandleFunc("GET /api/funds/{fundId}/workflow-checkpoints", h.handleList)
}

func (h *fundWorkflowCheckpointsHandler) handleList(w http.ResponseWriter, r *http.Request) {
	userID, ok := api.AuthenticatedUserID(r)
	if !ok || strings.TrimSpace(userID) == "" {
		writeJSON(w, http.StatusUnauthorized, errorPayload("unauthorized", "authentication required"))
		return
	}
	fundID := strings.TrimSpace(r.PathValue("fundId"))
	if fundID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_path", "fundId required"))
		return
	}
	if _, err := authorizeFundAccess(r.Context(), h.fundRepo, h.companyRepo, userID, fundID); err != nil {
		switch {
		case errors.Is(err, api.ErrForbidden):
			writeJSON(w, http.StatusForbidden, errorPayload("forbidden", "you do not own this fund"))
		case errors.Is(err, api.ErrNotFound):
			writeJSON(w, http.StatusNotFound, errorPayload("not_found", "fund not found"))
		default:
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		}
		return
	}
	tradingDateStr := strings.TrimSpace(r.URL.Query().Get("trading_date"))
	if tradingDateStr == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_trading_date", "trading_date is required (YYYY-MM-DD)"))
		return
	}
	td, err := time.Parse("2006-01-02", tradingDateStr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorPayload("invalid_trading_date", err.Error()))
		return
	}
	rows, err := h.workflowCheckpointRepo.ListByFundAndDate(r.Context(), fundID, td)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"checkpoints": wireCheckpoints(rows)})
}
