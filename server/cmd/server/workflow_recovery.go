package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// recoveryDecision is the per-row outcome from the startup triage. Used
// only inside this file for logging + the F12 test assertions.
type recoveryDecision string

const (
	recoveryResumed     recoveryDecision = "resumed"      // paused + plan approved → resumed from trade execution
	recoveryRehydrated  recoveryDecision = "rehydrated"   // paused awaiting user → runtime registered, no goroutine
	recoveryInterrupted recoveryDecision = "interrupted"  // running mid-step → marked failed
	recoverySkippedRun  recoveryDecision = "skipped"      // unrecognized / already terminal — left alone
)

// recoveryReport is the aggregate outcome of one recovery sweep. Useful
// for the startup log line ("recovered N runs") and for unit tests that
// drive the triage matrix.
type recoveryReport struct {
	Total       int
	Resumed     int
	Rehydrated  int
	Interrupted int
	Skipped     int
	Errors      int
}

// RecoverIncompleteWorkflows is the F12 entry point. It scans the
// workflow_runs table for runs that were still "in flight" when the
// process last died and triages each one:
//
//   - status='running'  → server crashed mid-step. Mark as failed with an
//     explicit "interrupted" reason. Admin / scheduler can re-trigger from
//     a clean slate. We refuse to auto-resume here because we don't know
//     which side effects already happened (LLM call counted? trade row
//     inserted? wallet held?) and double-execution risk outweighs the
//     convenience of auto-resume for mid-step crashes.
//
//   - status='paused' AND current_step='user_approval':
//       · plan.status='approved' → user approved before crash; the workflow
//         was supposed to continue into trade execution. Resume by launching
//         resumeApprovedPlan in the background, exactly like a fresh
//         ApprovePlan call would have done.
//       · plan.status='pending_user' → user hasn't decided yet. Register
//         the runtime so admin / scheduler observability sees it, but do
//         not launch any goroutine — the lazy path through ApprovePlan
//         will rehydrate and run when the user clicks.
//       · plan.status='rejected' / any other → leave as-is, will be
//         cleaned up by the next workflow_run sweep.
//
//   - status='pending' → run was claimed but never actually started
//     (extremely rare; usually a race where the claim succeeded but the
//     orchestrator goroutine never launched). Mark as failed with the
//     "interrupted" reason so the next scheduler tick re-claims cleanly.
//
// Safe to call multiple times — already-recovered runs are excluded by
// the next ListIncomplete query because their status is now 'failed' /
// 'running' / 'paused' (the latter only when the runtime registry now
// owns them, which the rehydrate branches set up).
func (s *workflowServiceAdapter) RecoverIncompleteWorkflows(ctx context.Context) (*recoveryReport, error) {
	if s == nil || s.workflowRepo == nil || s.fundRepo == nil {
		return &recoveryReport{}, nil
	}
	report := &recoveryReport{}

	runs, err := s.workflowRepo.ListIncomplete(ctx, 500)
	if err != nil {
		return report, fmt.Errorf("workflow recovery: list incomplete runs: %w", err)
	}
	report.Total = len(runs)
	if report.Total == 0 {
		return report, nil
	}

	slog.Info("workflow recovery: scanning incomplete runs", "count", report.Total)

	for i := range runs {
		run := runs[i]
		decision, err := s.recoverOneRun(ctx, &run)
		if err != nil {
			report.Errors++
			slog.Error("workflow recovery: failed to recover run",
				"fund_id", run.FundID,
				"trading_date", run.TradingDate.Format("2006-01-02"),
				"status", run.Status,
				"current_step", run.CurrentStep.String,
				"err", err,
			)
			continue
		}
		switch decision {
		case recoveryResumed:
			report.Resumed++
		case recoveryRehydrated:
			report.Rehydrated++
		case recoveryInterrupted:
			report.Interrupted++
		default:
			report.Skipped++
		}
	}

	slog.Info("workflow recovery: complete",
		"total", report.Total,
		"resumed", report.Resumed,
		"rehydrated", report.Rehydrated,
		"interrupted", report.Interrupted,
		"skipped", report.Skipped,
		"errors", report.Errors,
	)
	return report, nil
}

func (s *workflowServiceAdapter) recoverOneRun(ctx context.Context, run *repository.WorkflowRun) (recoveryDecision, error) {
	fund, err := s.fundRepo.GetByID(ctx, run.FundID)
	if err != nil {
		// Fund deleted while the run was alive — mark it failed so the
		// row no longer shows up in scheduler / admin views.
		if errors.Is(err, repository.ErrNotFound) {
			s.markRunInterrupted(ctx, run, "fund deleted before recovery")
			return recoveryInterrupted, nil
		}
		return recoverySkippedRun, fmt.Errorf("load fund: %w", err)
	}

	status := strings.ToLower(strings.TrimSpace(run.Status))
	switch status {
	case "running":
		// Server crashed mid-step. We have no safe way to know which
		// side effects already committed for the in-flight step, so we
		// fail closed.
		s.markRunInterrupted(ctx, run, "server restart interrupted in-flight step")
		return recoveryInterrupted, nil

	case "pending":
		// Claimed but never started (orchestrator goroutine never
		// launched). Same fail-closed treatment so next scheduler tick
		// can re-claim cleanly via ClaimStart.
		s.markRunInterrupted(ctx, run, "server restart before orchestrator launch")
		return recoveryInterrupted, nil

	case "paused":
		return s.recoverPausedRun(ctx, fund, run)
	}

	return recoverySkippedRun, nil
}

// recoverPausedRun handles the most delicate case: a workflow that was
// waiting for user approval when the process died. We need to look at the
// plan to know whether the user already decided.
func (s *workflowServiceAdapter) recoverPausedRun(ctx context.Context, fund *repository.Fund, run *repository.WorkflowRun) (recoveryDecision, error) {
	currentStep := strings.ToLower(strings.TrimSpace(run.CurrentStep.String))
	if currentStep != workflow.StepUserApproval.String() {
		// Paused at any step other than user_approval is unexpected
		// (no other step pauses in the current orchestrator). Treat as
		// interrupted so the row doesn't sit "paused" forever.
		s.markRunInterrupted(ctx, run, fmt.Sprintf("paused at unexpected step %q", run.CurrentStep.String))
		return recoveryInterrupted, nil
	}

	plan, err := s.findPendingPlanForRun(ctx, fund.ID, run.TradingDate)
	if err != nil {
		// Couldn't tell what state the plan is in — leave the run paused
		// and let the lazy ApprovePlan path handle it when the user
		// clicks. We don't want to fail-close here because that would
		// destroy a pending approval the user can still answer.
		slog.Warn("workflow recovery: paused run with no readable plan, leaving in place",
			"fund_id", fund.ID,
			"trading_date", run.TradingDate.Format("2006-01-02"),
			"err", err,
		)
		return recoverySkippedRun, nil
	}

	planStatus := strings.ToLower(strings.TrimSpace(plan.Status))
	switch planStatus {
	case "approved", "executing":
		// Race: user clicked approve right before the crash. The
		// workflow MUST resume from trade execution; otherwise the
		// approved-but-unexecuted plan sits forever.
		if err := s.launchResumeForRecovery(ctx, fund, run, plan); err != nil {
			return recoverySkippedRun, fmt.Errorf("launch resume: %w", err)
		}
		return recoveryResumed, nil

	case "pending_user", "pending":
		// User hasn't decided. Register the runtime so observability
		// surfaces show the paused fund. We deliberately do NOT launch
		// the orchestrator goroutine — the lazy ApprovePlan / RejectPlan
		// path already rebuilds the runtime correctly when the user
		// actually decides, and double-launching would double-bill the
		// next step.
		if err := s.rehydrateRuntimeForRecovery(fund, run); err != nil {
			return recoverySkippedRun, fmt.Errorf("rehydrate runtime: %w", err)
		}
		return recoveryRehydrated, nil

	case "rejected":
		// User rejected before the crash but the workflow_run wasn't
		// flipped to rejected. Reconcile by marking the run rejected
		// (mirrors what RejectAwaitingPlan would have done).
		s.markRunRejected(ctx, run, "user rejected plan before recovery")
		return recoveryInterrupted, nil

	default:
		// completed / unknown / weird — leave alone.
		return recoverySkippedRun, nil
	}
}

// findPendingPlanForRun returns the plan associated with a paused run.
// Returns ErrNotFound if no plan exists for that fund + trading date,
// which is itself a recovery decision the caller handles.
func (s *workflowServiceAdapter) findPendingPlanForRun(ctx context.Context, fundID string, tradingDate time.Time) (*repository.InvestmentPlan, error) {
	if s.planRepo == nil {
		return nil, errors.New("plan repo not wired")
	}
	return s.planRepo.GetLatestByFundAndDate(ctx, fundID, normalizeTradingDate(tradingDate))
}

// markRunInterrupted writes a terminal "failed" status with an explicit
// step_results entry naming the interruption reason. We use 'failed'
// rather than introducing a new 'interrupted' enum so existing dashboards
// and rules don't need updates; the reason is preserved in step_results.
func (s *workflowServiceAdapter) markRunInterrupted(ctx context.Context, run *repository.WorkflowRun, reason string) {
	stepResults := decodeWorkflowStepResults(run.StepResults)
	stepKey := strings.TrimSpace(run.CurrentStep.String)
	if stepKey == "" {
		stepKey = workflow.StepMacroBrief.String()
	}
	step := stepResults[stepKey]
	step.Step = stepKey
	step.Status = "failed"
	step.Error = "recovery: " + reason
	step.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	stepResults[stepKey] = step

	encoded, err := json.Marshal(stepResults)
	if err != nil {
		slog.Warn("workflow recovery: failed to encode step results",
			"fund_id", run.FundID,
			"err", err,
		)
		return
	}
	completedAt := time.Now().UTC()

	updated := *run
	updated.Status = "failed"
	updated.StepResults = json.RawMessage(encoded)
	updated.CompletedAt = sql.NullTime{Time: completedAt, Valid: true}

	if err := s.workflowRepo.Update(ctx, &updated); err != nil {
		slog.Warn("workflow recovery: failed to mark run interrupted",
			"fund_id", run.FundID,
			"trading_date", run.TradingDate.Format("2006-01-02"),
			"err", err,
		)
		return
	}
	if s.metrics != nil {
		s.metrics.ObserveWorkflow(run.FundID, updated.Status, updated.CurrentStep.String)
	}
}

func (s *workflowServiceAdapter) markRunRejected(ctx context.Context, run *repository.WorkflowRun, reason string) {
	stepResults := decodeWorkflowStepResults(run.StepResults)
	stepKey := workflow.StepUserApproval.String()
	step := stepResults[stepKey]
	step.Step = stepKey
	step.Status = "rejected"
	step.Error = "recovery: " + reason
	step.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	stepResults[stepKey] = step

	encoded, err := json.Marshal(stepResults)
	if err != nil {
		slog.Warn("workflow recovery: failed to encode step results",
			"fund_id", run.FundID,
			"err", err,
		)
		return
	}
	completedAt := time.Now().UTC()

	updated := *run
	updated.Status = "rejected"
	updated.StepResults = json.RawMessage(encoded)
	updated.CompletedAt = sql.NullTime{Time: completedAt, Valid: true}

	if err := s.workflowRepo.Update(ctx, &updated); err != nil {
		slog.Warn("workflow recovery: failed to mark run rejected",
			"fund_id", run.FundID,
			"trading_date", run.TradingDate.Format("2006-01-02"),
			"err", err,
		)
		return
	}
	if s.metrics != nil {
		s.metrics.ObserveWorkflow(run.FundID, updated.Status, updated.CurrentStep.String)
	}
}

// rehydrateRuntimeForRecovery creates a workflow runtime entry that
// reflects the paused state of the DB row, without launching any
// orchestrator goroutine. Purpose: scheduler snapshot + admin views
// can show the fund as 'paused awaiting user' immediately, instead of
// waiting for the next user click to lazy-load the runtime.
func (s *workflowServiceAdapter) rehydrateRuntimeForRecovery(fund *repository.Fund, run *repository.WorkflowRun) error {
	tradingDate := normalizeTradingDate(run.TradingDate)
	s.cancelRuntime(s.takeRuntime(fund.ID, tradingDate))
	runtime := s.getRuntime(fund, tradingDate, time.Now(), false)
	if runtime == nil || runtime.orchestrator == nil {
		return errors.New("getRuntime returned nil runtime")
	}
	s.restoreRuntimeFromRun(runtime, run)
	return nil
}

// launchResumeForRecovery is the auto-resume branch: user approved before
// the crash, so we need to launch the trade-execution-onward goroutine
// just like ResumeApprovedPlan would have.
func (s *workflowServiceAdapter) launchResumeForRecovery(ctx context.Context, fund *repository.Fund, run *repository.WorkflowRun, plan *repository.InvestmentPlan) error {
	tradingDate := normalizeTradingDate(run.TradingDate)
	s.cancelRuntime(s.takeRuntime(fund.ID, tradingDate))
	runtime := s.getRuntime(fund, tradingDate, time.Now(), false)
	if runtime == nil || runtime.orchestrator == nil {
		return errors.New("getRuntime returned nil runtime")
	}
	s.restoreRuntimeFromRun(runtime, run)
	runtime.orchestrator.RestoreState(workflow.WorkflowState{
		RunID:       run.ID,
		FundID:      run.FundID,
		TradingDate: tradingDate.Format("2006-01-02"),
		Status:      workflow.RunStatusPaused,
		CurrentStep: workflow.StepUserApproval,
		PlanID:      strings.TrimSpace(plan.ID),
		StartedAt:   nullTimeValue(run.StartedAt),
	})
	go s.resumeApprovedPlan(fund.ID, tradingDate.Format("2006-01-02"), runtime, plan.ID)
	return nil
}

// runRecoveryWhenLeader is the startup hook plumbed in by main.go. It
// polls leadership every interval; once this replica becomes leader,
// runs RecoverIncompleteWorkflows exactly once and exits. If the process
// loses leadership before becoming leader, it keeps polling until ctx
// is cancelled (typical shutdown).
//
// Why poll instead of running unconditionally? Multi-replica deployments
// must run recovery exactly once per crash, on whichever replica wins the
// scheduler lease. Single-replica deployments still work because they
// become leader within ~one ttl after start.
func (s *workflowServiceAdapter) runRecoveryWhenLeader(ctx context.Context, checker leaderChecker, interval time.Duration) {
	if s == nil {
		return
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	if checker == nil {
		// No leader gating wired (test / single-process dev). Run
		// recovery immediately.
		if _, err := s.RecoverIncompleteWorkflows(ctx); err != nil {
			slog.Error("workflow recovery: error", "err", err)
		}
		return
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if checker.IsLeader(SchedulerLeaseName) {
			if _, err := s.RecoverIncompleteWorkflows(ctx); err != nil {
				slog.Error("workflow recovery: error", "err", err)
			}
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
