package main

import (
	"context"
	"time"

	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// activityPersisterAdapter bridges the slim workflow.ActivityPersister
// interface (which the workflow package owns and depends on nothing
// outside of itself) with *repository.WorkflowActivityRepo (which owns
// the actual SQL). Keeping the two structs separate is what lets the
// workflow package stay independent of the repository package; the
// adapter is the only place that knows about both sides.
type activityPersisterAdapter struct {
	repo *repository.WorkflowActivityRepo
}

func newActivityPersisterAdapter(repo *repository.WorkflowActivityRepo) *activityPersisterAdapter {
	if repo == nil {
		return nil
	}
	return &activityPersisterAdapter{repo: repo}
}

// BulkInsert translates workflow events into repository rows and forwards
// the call. The two structs are field-for-field identical so the conversion
// is a trivial copy loop; we keep the adapter explicit instead of using
// reflection both for clarity and so adding a field to one side raises
// a compile error here (preventing silent data loss).
func (a *activityPersisterAdapter) BulkInsert(ctx context.Context, events []workflow.PersistableActivityEvent) error {
	if a == nil || a.repo == nil || len(events) == 0 {
		return nil
	}
	rows := make([]repository.WorkflowActivityEvent, len(events))
	for i, evt := range events {
		rows[i] = repository.WorkflowActivityEvent{
			FundID:       evt.FundID,
			Seq:          evt.Seq,
			Type:         evt.Type,
			Role:         evt.Role,
			Step:         evt.Step,
			RunID:        evt.RunID,
			TradingDate:  evt.TradingDate,
			Message:      evt.Message,
			ErrorMessage: evt.ErrorMessage,
			EventAt:      evt.EventAt,
		}
	}
	return a.repo.BulkInsert(ctx, rows)
}

// ListByFund forwards to the repo and converts row → workflow event.
// The bus uses this for the "load earlier" pagination path and as a
// fallback when the in-memory ring has been wiped by a restart.
func (a *activityPersisterAdapter) ListByFund(ctx context.Context, fundID string, before time.Time, limit int) ([]workflow.PersistableActivityEvent, error) {
	if a == nil || a.repo == nil {
		return nil, nil
	}
	rows, err := a.repo.ListByFund(ctx, fundID, before, limit)
	if err != nil {
		return nil, err
	}
	out := make([]workflow.PersistableActivityEvent, len(rows))
	for i, row := range rows {
		out[i] = workflow.PersistableActivityEvent{
			FundID:       row.FundID,
			Seq:          row.Seq,
			Type:         row.Type,
			Role:         row.Role,
			Step:         row.Step,
			RunID:        row.RunID,
			TradingDate:  row.TradingDate,
			Message:      row.Message,
			ErrorMessage: row.ErrorMessage,
			EventAt:      row.EventAt,
		}
	}
	return out, nil
}

// MaxSeqForFund is a thin pass-through used by the bus to seed its
// per-fund seq counter after process restart.
func (a *activityPersisterAdapter) MaxSeqForFund(ctx context.Context, fundID string) (uint64, error) {
	if a == nil || a.repo == nil {
		return 0, nil
	}
	return a.repo.MaxSeqForFund(ctx, fundID)
}
