// admin_workflow_checkpoints.go — S9.2 per-step workflow snapshot
// + resume admin endpoints.
//
// Endpoints
//
//   GET  /api/admin/workflow-checkpoints[?fund_id=X&trading_date=YYYY-MM-DD&run_id=Z]
//        Cross-fund or scoped per-step timeline view. Exactly one
//        of {fund_id+trading_date, run_id} must be present.
//
//   POST /api/admin/workflow-checkpoints/resume
//        Body: {"run_id":"...", "step":"..."} (step optional —
//        defaults to "the latest failed / paused step").
//        Synchronously re-fires the named step via the per-fund
//        orchestrator's TriggerStep path.
//
// Reads are audit-logged at debug level; writes (resume) are
// audit-logged at info level via auditLogger.LogMutation.

package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/fundai/server/internal/api"
	"github.com/fundai/server/internal/audit"
	"github.com/fundai/server/internal/repository"
)

// workflowCheckpointResumeSink is the narrow contract the admin
// handler needs from the orchestrator layer: given a (fund, date,
// step) tuple, re-fire that step synchronously and return the
// resulting step status.
//
// Production wiring satisfies this via workflowServiceAdapter's
// existing TriggerStep flow; tests can substitute a stub.
type workflowCheckpointResumeSink interface {
	ResumeStep(ctx context.Context, fundID string, tradingDate time.Time, step string) (*api.WorkflowStatus, error)
}

func (h *adminHandler) registerWorkflowCheckpointAdminRoutes(mux *http.ServeMux) {
	if h == nil || mux == nil || h.workflowCheckpointRepo == nil {
		return
	}
	mux.HandleFunc("GET /api/admin/workflow-checkpoints", h.handleAdminListWorkflowCheckpoints)
	mux.HandleFunc("POST /api/admin/workflow-checkpoints/resume", h.handleAdminResumeWorkflowCheckpoint)
}

type workflowCheckpointWire struct {
	ID          string          `json:"id"`
	RunID       string          `json:"run_id"`
	FundID      string          `json:"fund_id"`
	TradingDate string          `json:"trading_date"`
	Step        string          `json:"step"`
	Status      string          `json:"status"`
	Attempts    int             `json:"attempts"`
	StartedAt   string          `json:"started_at"`
	EndedAt     string          `json:"ended_at"`
	DurationMs  int64           `json:"duration_ms"`
	ErrorText   string          `json:"error_text,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   string          `json:"created_at"`
	UpdatedAt   string          `json:"updated_at"`
}

func wireCheckpoint(cp repository.WorkflowCheckpoint) workflowCheckpointWire {
	return workflowCheckpointWire{
		ID:          cp.ID,
		RunID:       cp.RunID,
		FundID:      cp.FundID,
		TradingDate: cp.TradingDate.UTC().Format("2006-01-02"),
		Step:        cp.Step,
		Status:      cp.Status,
		Attempts:    cp.Attempts,
		StartedAt:   cp.StartedAt.UTC().Format(time.RFC3339),
		EndedAt:     cp.EndedAt.UTC().Format(time.RFC3339),
		DurationMs:  cp.DurationMs,
		ErrorText:   cp.ErrorText,
		Payload:     cp.Payload,
		CreatedAt:   cp.CreatedAt.UTC().Format(time.RFC3339),
		UpdatedAt:   cp.UpdatedAt.UTC().Format(time.RFC3339),
	}
}

func (h *adminHandler) handleAdminListWorkflowCheckpoints(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	q := r.URL.Query()
	runID := strings.TrimSpace(q.Get("run_id"))
	fundID := strings.TrimSpace(q.Get("fund_id"))
	tradingDateStr := strings.TrimSpace(q.Get("trading_date"))
	switch {
	case runID != "":
		rows, err := h.workflowCheckpointRepo.ListByRun(r.Context(), runID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"checkpoints": wireCheckpoints(rows)})
	case fundID != "" && tradingDateStr != "":
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
	default:
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_filter", "either run_id, or both fund_id + trading_date, is required"))
	}
}

func wireCheckpoints(rows []repository.WorkflowCheckpoint) []workflowCheckpointWire {
	out := make([]workflowCheckpointWire, 0, len(rows))
	for _, cp := range rows {
		out = append(out, wireCheckpoint(cp))
	}
	return out
}

type adminResumeCheckpointRequest struct {
	RunID string `json:"run_id"`
	Step  string `json:"step,omitempty"`
}

type adminResumeCheckpointResponse struct {
	RunID  string `json:"run_id"`
	Step   string `json:"step"`
	Status string `json:"status"`
}

func (h *adminHandler) handleAdminResumeWorkflowCheckpoint(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdmin(w, r) {
		return
	}
	if h.workflowCheckpointResumeSink == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorPayload("resume_unavailable", "no resume sink wired"))
		return
	}
	var req adminResumeCheckpointRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, errorPayload("invalid_body", err.Error()))
			return
		}
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		writeJSON(w, http.StatusBadRequest, errorPayload("missing_run_id", "run_id is required"))
		return
	}
	step := strings.TrimSpace(req.Step)
	target, err := h.resolveResumeTarget(r.Context(), runID, step)
	if err != nil {
		if errors.Is(err, errResumeNoFailedCheckpoint) {
			writeJSON(w, http.StatusConflict, errorPayload("no_failed_step", err.Error()))
			return
		}
		writeJSON(w, http.StatusInternalServerError, errorPayload("internal", err.Error()))
		return
	}
	status, err := h.workflowCheckpointResumeSink.ResumeStep(r.Context(), target.fundID, target.tradingDate, target.step)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, errorPayload("resume_failed", err.Error()))
		return
	}
	userID, _ := api.AuthenticatedUserID(r)
	if h.auditLogger != nil && userID != "" {
		_ = h.auditLogger.LogMutation(r.Context(), audit.MutationEvent{
			ActorUserID: userID,
			Action:      "workflow_checkpoint.resume",
			TargetType:  "workflow_run",
			TargetID:    runID,
			After: map[string]any{
				"run_id":       runID,
				"fund_id":      target.fundID,
				"trading_date": target.tradingDate.Format("2006-01-02"),
				"step":         target.step,
			},
		})
	}
	resp := adminResumeCheckpointResponse{
		RunID:  runID,
		Step:   target.step,
		Status: "triggered",
	}
	if status != nil && strings.TrimSpace(status.State) != "" {
		resp.Status = status.State
	}
	writeJSON(w, http.StatusOK, resp)
}

type resumeTarget struct {
	fundID      string
	tradingDate time.Time
	step        string
}

var errResumeNoFailedCheckpoint = errors.New("no failed or paused checkpoint to resume")

// resolveResumeTarget figures out which (fund, date, step) the
// operator wants re-fired. Step is optional in the request body
// — when empty we resume from the most recent failed/paused
// checkpoint, which is the common "the run died at PM, re-fire
// it" UX. When step is supplied explicitly we still pull the
// existing checkpoint to get the fund/date pair so the API never
// trusts caller-supplied scoping.
func (h *adminHandler) resolveResumeTarget(ctx context.Context, runID, step string) (resumeTarget, error) {
	if step != "" {
		rows, err := h.workflowCheckpointRepo.ListByRun(ctx, runID)
		if err != nil {
			return resumeTarget{}, fmt.Errorf("list by run: %w", err)
		}
		for _, cp := range rows {
			if cp.Step == step {
				return resumeTarget{fundID: cp.FundID, tradingDate: cp.TradingDate, step: cp.Step}, nil
			}
		}
		return resumeTarget{}, fmt.Errorf("step %q not found in run %s", step, runID)
	}
	cp, err := h.workflowCheckpointRepo.GetLatestFailedOrPaused(ctx, runID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return resumeTarget{}, errResumeNoFailedCheckpoint
		}
		return resumeTarget{}, fmt.Errorf("get latest failed: %w", err)
	}
	return resumeTarget{fundID: cp.FundID, tradingDate: cp.TradingDate, step: cp.Step}, nil
}
