package workflow

import (
	"context"
	"sync"
	"testing"
	"time"
)

type captureCheckpointStore struct {
	mu    sync.Mutex
	saves []CheckpointSnapshot
	err   error
}

func (s *captureCheckpointStore) Save(_ context.Context, snap CheckpointSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.saves = append(s.saves, snap)
	return s.err
}

// TestPersistCheckpoint_NilStoreNoOp covers the most common path:
// no store is wired (the orchestrator was constructed without
// WithCheckpointStore). persistCheckpoint must short-circuit so
// the in-process state path is untouched.
func TestPersistCheckpoint_NilStoreNoOp(t *testing.T) {
	o := &DailyOrchestrator{
		fundID: "f1",
		state:  newWorkflowState("f1", "2026-06-01"),
	}
	o.persistCheckpoint(context.Background(), StepPMPlan, StepResult{
		Step:      StepPMPlan,
		Status:    "success",
		StartedAt: time.Now(),
		EndedAt:   time.Now(),
	}, 1, nil)
}

// TestPersistCheckpoint_NilOrchestratorNoOp covers the symmetric
// nil-receiver guard.
func TestPersistCheckpoint_NilOrchestratorNoOp(t *testing.T) {
	var o *DailyOrchestrator
	o.persistCheckpoint(context.Background(), StepPMPlan, StepResult{Status: "success"}, 1, nil)
}

// TestPersistCheckpoint_HappyPath verifies that a success snapshot
// is forwarded with the correct fields populated.
func TestPersistCheckpoint_HappyPath(t *testing.T) {
	store := &captureCheckpointStore{}
	o := &DailyOrchestrator{
		fundID:          "f1",
		state:           newWorkflowState("f1", "2026-06-01"),
		checkpointStore: store,
	}
	o.state.RunID = "run-1"
	now := time.Now()
	o.persistCheckpoint(context.Background(), StepPMPlan, StepResult{
		Step:      StepPMPlan,
		Status:    "success",
		StartedAt: now,
		EndedAt:   now.Add(time.Second),
	}, 1, []byte(`{"planId":"plan-9"}`))
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 1 {
		t.Fatalf("expected 1 save, got %d", len(store.saves))
	}
	snap := store.saves[0]
	if snap.RunID != "run-1" || snap.Step != "pm_plan" || snap.Status != "success" {
		t.Errorf("unexpected snap: %+v", snap)
	}
	if string(snap.Payload) != `{"planId":"plan-9"}` {
		t.Errorf("payload mismatch: %s", string(snap.Payload))
	}
}

// TestPersistCheckpoint_FailureCapturesErrText verifies that the
// error text is forwarded so the admin UI can show it without a
// second round-trip.
func TestPersistCheckpoint_FailureCapturesErrText(t *testing.T) {
	store := &captureCheckpointStore{}
	o := &DailyOrchestrator{
		fundID:          "f1",
		state:           newWorkflowState("f1", "2026-06-01"),
		checkpointStore: store,
	}
	o.state.RunID = "run-1"
	o.persistCheckpoint(context.Background(), StepPMPlan, StepResult{
		Step:   StepPMPlan,
		Status: "failed",
		Error:  context.DeadlineExceeded,
	}, 3, nil)
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.saves) != 1 {
		t.Fatalf("expected 1 save, got %d", len(store.saves))
	}
	if store.saves[0].ErrorText == "" {
		t.Error("expected error text to be captured")
	}
	if store.saves[0].Attempts != 3 {
		t.Errorf("expected attempts=3, got %d", store.saves[0].Attempts)
	}
}
