package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// TestWorkflowEventPersistsStateClassification confirms the allow-list of
// event types that should write to workflow_runs. If you add a new event
// type to the orchestrator, update this list deliberately.
func TestWorkflowEventPersistsStateClassification(t *testing.T) {
	persistTypes := []string{
		"run_started",
		"run_completed",
		"run_failed",
		"run_rejected",
		"run_resumed",
		"step_started",
		"step_completed",
		"step_failed",
		"step_paused",
		"step_skipped",
		"awaiting_user",
	}
	for _, typ := range persistTypes {
		if !workflowEventPersistsState(typ) {
			t.Errorf("expected event type %q to trigger persistence", typ)
		}
	}

	nonPersistTypes := []string{
		"",
		"heartbeat",
		"info",
		"unknown",
		"researcher_speak",
	}
	for _, typ := range nonPersistTypes {
		if workflowEventPersistsState(typ) {
			t.Errorf("expected event type %q to NOT trigger persistence", typ)
		}
	}
}

// fakeDelegateBus records all events forwarded to it from the persisting bus.
type fakeDelegateBus struct {
	mu     sync.Mutex
	events []workflow.WorkflowEvent
}

func (b *fakeDelegateBus) Publish(_ context.Context, evt workflow.WorkflowEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, evt)
	return nil
}

func (b *fakeDelegateBus) seen() []workflow.WorkflowEvent {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]workflow.WorkflowEvent, len(b.events))
	copy(out, b.events)
	return out
}

// TestPersistingEventBusDelegatesEveryEvent ensures the wrapper does not
// swallow or filter events on the delegate path — every event must be
// forwarded to the activity bus regardless of whether it triggers DB writes.
func TestPersistingEventBusDelegatesEveryEvent(t *testing.T) {
	delegate := &fakeDelegateBus{}
	bus := newPersistingEventBus(nil, "fund-x", time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC), delegate)

	events := []workflow.WorkflowEvent{
		{Type: "run_started", FundID: "fund-x"},
		{Type: "step_started", FundID: "fund-x"},
		{Type: "heartbeat", FundID: "fund-x"},
		{Type: "awaiting_user", FundID: "fund-x"},
	}
	for _, e := range events {
		if err := bus.Publish(context.Background(), e); err != nil {
			t.Fatalf("publish %s: %v", e.Type, err)
		}
	}

	got := delegate.seen()
	if len(got) != len(events) {
		t.Fatalf("delegate received %d events, want %d", len(got), len(events))
	}
	for i, evt := range got {
		if evt.Type != events[i].Type {
			t.Errorf("delegate event[%d]=%s want %s", i, evt.Type, events[i].Type)
		}
	}
}

// TestPersistingEventBusSkipsWhenNoSnapshot verifies the bus is a graceful
// no-op when an event lacks Snapshot, even if its type would normally persist.
// Prevents NPEs from test or upstream code that builds events manually.
func TestPersistingEventBusSkipsWhenNoSnapshot(t *testing.T) {
	delegate := &fakeDelegateBus{}
	bus := newPersistingEventBus(nil, "fund-x", time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC), delegate)

	if err := bus.Publish(context.Background(), workflow.WorkflowEvent{Type: "step_started", FundID: "fund-x"}); err != nil {
		t.Fatalf("publish without snapshot: %v", err)
	}
	if len(delegate.seen()) != 1 {
		t.Fatalf("expected delegate to still receive event, got %d", len(delegate.seen()))
	}
}

// TestPersistingEventBusWritesStepProgressToRepo is the critical integration
// test: it proves that emitting a step_started event with a state snapshot
// triggers an INSERT into workflow_runs even before RunFull returns.
// This is the exact scenario that caused workflow_runs to stall at
// macro_brief during the F8 smoke test.
func TestPersistingEventBusWritesStepProgressToRepo(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)
	startedAt := time.Date(2026, time.May, 18, 9, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, fund_id, trading_date")).
		WithArgs("fund-x", tradingDate).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO workflow_runs")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "trading_date", "status", "current_step",
			"step_results", "started_at", "completed_at", "created_at",
		}).AddRow(
			"run-1", "fund-x", tradingDate, "running", "research_parallel",
			[]byte(`{}`), startedAt, nil, startedAt,
		))
	mock.ExpectCommit()

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}
	bus := newPersistingEventBus(adapter, "fund-x", tradingDate, nil)

	snap := &workflow.WorkflowState{
		RunID:       "run-1",
		FundID:      "fund-x",
		TradingDate: "2026-05-18",
		Status:      workflow.RunStatusRunning,
		CurrentStep: workflow.StepResearchParallel,
		StartedAt:   startedAt,
	}
	evt := workflow.WorkflowEvent{
		Type:        "step_started",
		RunID:       snap.RunID,
		FundID:      snap.FundID,
		Step:        workflow.StepResearchParallel,
		TradingDate: snap.TradingDate,
		Timestamp:   startedAt,
		Snapshot:    snap,
	}
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestPersistingEventBusContinuesOnDBError ensures a DB write failure does
// not abort the event delivery or leak the error to the orchestrator. The
// activity bus must still see the event (UX must not break because DB is
// flaky), and persistence simply skips this round.
func TestPersistingEventBusContinuesOnDBError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin().WillReturnError(errors.New("db down"))

	adapter := &workflowServiceAdapter{
		workflowRepo: repository.NewWorkflowRunRepo(db),
		runtimes:     make(map[string]*workflowRuntime),
	}
	delegate := &fakeDelegateBus{}
	bus := newPersistingEventBus(adapter, "fund-x", tradingDate, delegate)

	snap := &workflow.WorkflowState{
		RunID:       "run-2",
		FundID:      "fund-x",
		TradingDate: "2026-05-18",
		Status:      workflow.RunStatusRunning,
		CurrentStep: workflow.StepMacroBrief,
		StartedAt:   time.Date(2026, time.May, 18, 9, 0, 0, 0, time.UTC),
	}
	evt := workflow.WorkflowEvent{
		Type:     "step_started",
		FundID:   "fund-x",
		Snapshot: snap,
	}
	if err := bus.Publish(context.Background(), evt); err != nil {
		t.Fatalf("publish should swallow DB error, got %v", err)
	}
	if len(delegate.seen()) != 1 {
		t.Fatalf("delegate must still see event when DB write fails, got %d", len(delegate.seen()))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
