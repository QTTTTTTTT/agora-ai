package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/repository"
)

// fakeRecoveryLeader is a leaderChecker that flips to leader after a
// configurable number of polls. Used to assert the polling behaviour of
// runRecoveryWhenLeader.
type fakeRecoveryLeader struct {
	pollsBeforeLeader int32
	current           atomic.Int32
}

func (f *fakeRecoveryLeader) IsLeader(name string) bool {
	if name != SchedulerLeaseName {
		return false
	}
	return f.current.Add(1) > f.pollsBeforeLeader
}

func sqlmockColumnsForRun() []string {
	return []string{"id", "fund_id", "trading_date", "status", "current_step", "step_results", "started_at", "completed_at", "created_at"}
}

// TestRecoverIncompleteWorkflowsEmpty proves the recovery sweep is a
// no-op when no in-flight runs exist. This is the steady-state case for
// every restart on a healthy system, and it MUST not write anything.
func TestRecoverIncompleteWorkflowsEmpty(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, fund_id, trading_date, status, current_step, step_results, started_at, completed_at, created_at\n\t\t FROM workflow_runs\n\t\t WHERE status IN ('running', 'paused', 'pending')")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(sqlmockColumnsForRun()))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	report, err := adapter.RecoverIncompleteWorkflows(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Total != 0 || report.Resumed != 0 || report.Rehydrated != 0 || report.Interrupted != 0 {
		t.Fatalf("expected all-zero report on empty input, got %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestRecoverIncompleteWorkflowsMarksRunningAsInterrupted is the most
// important safety test: a server crash mid-step must NOT auto-resume
// (double-execution risk on LLM calls / trades). Instead it must be
// marked failed so the next scheduler tick can reclaim cleanly.
func TestRecoverIncompleteWorkflowsMarksRunningAsInterrupted(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 18, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_runs")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(sqlmockColumnsForRun()).
			AddRow("run-crash", "fund-c", tradingDate, "running", "research_parallel", []byte(`{}`), startedAt, nil, startedAt))

	// fundRepo.GetByID is called to verify the fund still exists. We
	// return a minimal row so the recovery can proceed without
	// short-circuiting on ErrNotFound.
	mock.ExpectQuery(regexp.QuoteMeta("FROM funds")).
		WithArgs("fund-c").
		WillReturnRows(fundsRowFor("fund-c"))

	// The terminal write: mark this run as failed with the interrupted
	// reason embedded in step_results.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_runs")).
		WithArgs(
			"failed",
			sqlmock.AnyArg(), // current_step preserved
			sqlmock.AnyArg(), // step_results JSON
			sqlmock.AnyArg(), // started_at preserved
			sqlmock.AnyArg(), // completed_at = now
			"run-crash",
		).
		WillReturnResult(sqlmock.NewResult(0, 1))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		planRepo:     repository.NewPlanRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	report, err := adapter.RecoverIncompleteWorkflows(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Interrupted != 1 {
		t.Fatalf("expected 1 interrupted, got %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestRecoverIncompleteWorkflowsHandlesMissingFund proves recovery
// gracefully handles orphan workflow_runs (fund deleted while run was
// alive). They must be marked failed so they stop polluting admin views.
func TestRecoverIncompleteWorkflowsHandlesMissingFund(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 18, 9, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_runs")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(sqlmockColumnsForRun()).
			AddRow("run-orphan", "fund-gone", tradingDate, "running", "macro_brief", []byte(`{}`), startedAt, nil, startedAt))

	mock.ExpectQuery(regexp.QuoteMeta("FROM funds")).
		WithArgs("fund-gone").
		WillReturnError(sql.ErrNoRows)

	mock.ExpectExec(regexp.QuoteMeta("UPDATE workflow_runs")).
		WithArgs("failed", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), "run-orphan").
		WillReturnResult(sqlmock.NewResult(0, 1))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	report, err := adapter.RecoverIncompleteWorkflows(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if report.Interrupted != 1 {
		t.Fatalf("expected 1 interrupted for orphan, got %+v", report)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestRecoverIncompleteWorkflowsListError surfaces the error from the
// initial scan. A failing scan means we can't reason about state and
// shouldn't pretend recovery succeeded.
func TestRecoverIncompleteWorkflowsListError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_runs")).
		WithArgs(500).
		WillReturnError(errors.New("conn refused"))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	_, err = adapter.RecoverIncompleteWorkflows(context.Background())
	if err == nil {
		t.Fatal("expected error from list, got nil")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestRecoverNilAdapter proves the function is safe on a nil / partially
// wired adapter. Useful for tests + early-startup invocation paths.
func TestRecoverNilAdapter(t *testing.T) {
	var s *workflowServiceAdapter
	report, err := s.RecoverIncompleteWorkflows(context.Background())
	if err != nil {
		t.Fatalf("nil adapter: %v", err)
	}
	if report == nil {
		t.Fatal("expected non-nil empty report")
	}
}

// TestRunRecoveryWhenLeaderPollsUntilLeader proves the polling loop
// waits for leadership before invoking recovery. A non-leader replica
// must not run recovery (would race the actual leader).
func TestRunRecoveryWhenLeaderPollsUntilLeader(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	// On the 3rd poll, the leader check returns true. Before that, no
	// SQL must be issued.
	leader := &fakeRecoveryLeader{pollsBeforeLeader: 2}

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_runs")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(sqlmockColumnsForRun()))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan struct{})
	go func() {
		adapter.runRecoveryWhenLeader(ctx, leader, 10*time.Millisecond)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runRecoveryWhenLeader did not exit within timeout")
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
	if leader.current.Load() < 3 {
		t.Fatalf("expected at least 3 leader checks, got %d", leader.current.Load())
	}
}

// TestRunRecoveryWhenLeaderRunsImmediatelyWhenNilChecker proves the
// no-leader-gating fast path (used in dev / unit tests) executes
// recovery exactly once and returns.
func TestRunRecoveryWhenLeaderRunsImmediatelyWhenNilChecker(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	mock.ExpectQuery(regexp.QuoteMeta("FROM workflow_runs")).
		WithArgs(500).
		WillReturnRows(sqlmock.NewRows(sqlmockColumnsForRun()))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}

	adapter.runRecoveryWhenLeader(context.Background(), nil, time.Millisecond)

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// fundsRowFor returns the minimum fund row needed by FundRepo.GetByID.
// Mirrors the column list in FundRepo.GetByID; keep in sync if columns
// are added there.
func fundsRowFor(id string) *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "company_id", "name", "description", "trading_mode",
		"initial_capital", "current_capital", "total_assets", "nav",
		"status", "config", "created_at", "updated_at",
	}).AddRow(
		id, "company-1", fmt.Sprintf("Fund %s", id), nil, "simulation",
		"100000", "100000", "100000", "1.0",
		"active", []byte(`{}`), time.Now(), time.Now(),
	)
}

// Compile-time check that sync is imported (used in deeper tests below).
var _ = sync.Mutex{}
