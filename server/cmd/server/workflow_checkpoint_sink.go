package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// workflowCheckpointSink bridges workflow.CheckpointStore (the
// orchestrator's narrow persistence contract) to the
// repository.WorkflowCheckpointRepo (PostgreSQL upserts).
//
// All errors are returned so the orchestrator can log them; the
// orchestrator never lets a checkpoint persistence failure block
// the run — see DailyOrchestrator.persistCheckpoint.
type workflowCheckpointSink struct {
	repo *repository.WorkflowCheckpointRepo
}

func newWorkflowCheckpointSink(repo *repository.WorkflowCheckpointRepo) *workflowCheckpointSink {
	if repo == nil {
		return nil
	}
	return &workflowCheckpointSink{repo: repo}
}

// Save satisfies workflow.CheckpointStore. The snapshot's TradingDate
// arrives as a YYYY-MM-DD string (the orchestrator works in date
// strings to stay independent of timezone); parseTradingDateOrNow
// turns it into a time.Time the repo can persist. Bogus dates fall
// back to the current UTC midnight rather than failing the call so
// a misconfigured schedule still produces a row the operator can see.
func (s *workflowCheckpointSink) Save(ctx context.Context, snap workflow.CheckpointSnapshot) error {
	if s == nil || s.repo == nil {
		return errors.New("workflow_checkpoint_sink: not initialised")
	}
	if strings.TrimSpace(snap.RunID) == "" || strings.TrimSpace(snap.FundID) == "" {
		return errors.New("workflow_checkpoint_sink: run_id and fund_id required")
	}
	tradingDate := parseTradingDateOrNow(snap.TradingDate)
	payload := snap.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	cp := &repository.WorkflowCheckpoint{
		RunID:       snap.RunID,
		FundID:      snap.FundID,
		TradingDate: tradingDate,
		Step:        snap.Step,
		Status:      snap.Status,
		Attempts:    snap.Attempts,
		StartedAt:   snap.StartedAt,
		EndedAt:     snap.EndedAt,
		ErrorText:   snap.ErrorText,
		Payload:     payload,
	}
	if cp.StartedAt.IsZero() {
		cp.StartedAt = time.Now().UTC()
	}
	if cp.EndedAt.IsZero() {
		cp.EndedAt = cp.StartedAt
	}
	if _, err := s.repo.Upsert(ctx, cp); err != nil {
		return fmt.Errorf("workflow_checkpoint_sink: upsert: %w", err)
	}
	return nil
}
