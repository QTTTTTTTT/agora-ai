package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkflowCheckpoint captures the outcome of a single step inside a
// daily workflow run. The orchestrator upserts one row per
// (run_id, step) so a row reflects the latest attempt; the optional
// payload column carries the small handful of identifiers / counts
// the next step needs to resume from this checkpoint.
type WorkflowCheckpoint struct {
	ID          string
	RunID       string
	FundID      string
	TradingDate time.Time
	Step        string
	Status      string
	Attempts    int
	StartedAt   time.Time
	EndedAt     time.Time
	DurationMs  int64
	ErrorText   string
	Payload     json.RawMessage
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// CheckpointPayload is a typed view of WorkflowCheckpoint.Payload.
// Used by the resume path to decide whether the downstream
// dependency (plan_id, report counts) is actually present.
type CheckpointPayload struct {
	PlanID      string `json:"planId,omitempty"`
	RoundtableID string `json:"roundtableId,omitempty"`
	ReportCount int    `json:"reportCount,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

// WorkflowCheckpointRepo persists step-level snapshots of a daily
// workflow run.
type WorkflowCheckpointRepo struct {
	db *sql.DB
}

func NewWorkflowCheckpointRepo(db *sql.DB) *WorkflowCheckpointRepo {
	return &WorkflowCheckpointRepo{db: db}
}

// Upsert writes the checkpoint, replacing any earlier row for the
// same (run_id, step). The row's updated_at is bumped to NOW() on
// every replacement so the API can show the freshest attempt.
func (r *WorkflowCheckpointRepo) Upsert(ctx context.Context, cp *WorkflowCheckpoint) (*WorkflowCheckpoint, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workflow_checkpoint_repo: not initialised")
	}
	if cp == nil {
		return nil, errors.New("workflow_checkpoint_repo: nil checkpoint")
	}
	if strings.TrimSpace(cp.RunID) == "" || strings.TrimSpace(cp.FundID) == "" {
		return nil, errors.New("workflow_checkpoint_repo: run_id and fund_id required")
	}
	if strings.TrimSpace(cp.Step) == "" {
		return nil, errors.New("workflow_checkpoint_repo: step required")
	}
	if !isCheckpointStatus(cp.Status) {
		return nil, fmt.Errorf("workflow_checkpoint_repo: invalid status %q", cp.Status)
	}
	if cp.Attempts < 1 {
		cp.Attempts = 1
	}
	payload := cp.Payload
	if len(payload) == 0 {
		payload = json.RawMessage(`{}`)
	}
	now := time.Now().UTC()
	if cp.StartedAt.IsZero() {
		cp.StartedAt = now
	}
	if cp.EndedAt.IsZero() {
		cp.EndedAt = now
	}
	if cp.DurationMs == 0 && !cp.EndedAt.Before(cp.StartedAt) {
		cp.DurationMs = cp.EndedAt.Sub(cp.StartedAt).Milliseconds()
	}
	errText := sql.NullString{}
	if strings.TrimSpace(cp.ErrorText) != "" {
		errText = sql.NullString{String: cp.ErrorText, Valid: true}
	}
	out := &WorkflowCheckpoint{}
	var errOut sql.NullString
	err := r.db.QueryRowContext(ctx,
		`INSERT INTO workflow_checkpoints
			(run_id, fund_id, trading_date, step, status, attempts,
			 started_at, ended_at, duration_ms, error_text, payload)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 ON CONFLICT (run_id, step) DO UPDATE
		 SET status      = EXCLUDED.status,
		     attempts    = EXCLUDED.attempts,
		     started_at  = EXCLUDED.started_at,
		     ended_at    = EXCLUDED.ended_at,
		     duration_ms = EXCLUDED.duration_ms,
		     error_text  = EXCLUDED.error_text,
		     payload     = EXCLUDED.payload,
		     updated_at  = NOW()
		 RETURNING id, run_id, fund_id, trading_date, step, status, attempts,
		           started_at, ended_at, duration_ms, error_text, payload,
		           created_at, updated_at`,
		cp.RunID, cp.FundID, cp.TradingDate, cp.Step, cp.Status, cp.Attempts,
		cp.StartedAt, cp.EndedAt, cp.DurationMs, errText, payload,
	).Scan(
		&out.ID, &out.RunID, &out.FundID, &out.TradingDate, &out.Step, &out.Status, &out.Attempts,
		&out.StartedAt, &out.EndedAt, &out.DurationMs, &errOut, &out.Payload,
		&out.CreatedAt, &out.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_checkpoint_repo: upsert: %w", err)
	}
	if errOut.Valid {
		out.ErrorText = errOut.String
	}
	return out, nil
}

// ListByRun returns every checkpoint for a run, ordered by ended_at
// ascending so the caller can render the timeline directly.
func (r *WorkflowCheckpointRepo) ListByRun(ctx context.Context, runID string) ([]WorkflowCheckpoint, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workflow_checkpoint_repo: not initialised")
	}
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("workflow_checkpoint_repo: run_id required")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, fund_id, trading_date, step, status, attempts,
		        started_at, ended_at, duration_ms, error_text, payload,
		        created_at, updated_at
		 FROM workflow_checkpoints
		 WHERE run_id = $1
		 ORDER BY ended_at ASC, created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_checkpoint_repo: list by run: %w", err)
	}
	defer rows.Close()
	return scanCheckpointRows(rows)
}

// ListByFundAndDate returns every checkpoint for a (fund, trading
// date) pair. Useful for the Admin UI's "show me the last failed
// step for this fund on this day" view.
func (r *WorkflowCheckpointRepo) ListByFundAndDate(ctx context.Context, fundID string, tradingDate time.Time) ([]WorkflowCheckpoint, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workflow_checkpoint_repo: not initialised")
	}
	if strings.TrimSpace(fundID) == "" {
		return nil, errors.New("workflow_checkpoint_repo: fund_id required")
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, run_id, fund_id, trading_date, step, status, attempts,
		        started_at, ended_at, duration_ms, error_text, payload,
		        created_at, updated_at
		 FROM workflow_checkpoints
		 WHERE fund_id = $1 AND trading_date = $2
		 ORDER BY ended_at ASC, created_at ASC`,
		fundID, tradingDate,
	)
	if err != nil {
		return nil, fmt.Errorf("workflow_checkpoint_repo: list by fund/date: %w", err)
	}
	defer rows.Close()
	return scanCheckpointRows(rows)
}

// GetLatestFailedOrPaused returns the most recent failed / paused
// checkpoint for a run, or sql.ErrNoRows when every step completed
// cleanly. The resume API consults this to decide which step the
// operator should restart from.
func (r *WorkflowCheckpointRepo) GetLatestFailedOrPaused(ctx context.Context, runID string) (*WorkflowCheckpoint, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workflow_checkpoint_repo: not initialised")
	}
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("workflow_checkpoint_repo: run_id required")
	}
	row := r.db.QueryRowContext(ctx,
		`SELECT id, run_id, fund_id, trading_date, step, status, attempts,
		        started_at, ended_at, duration_ms, error_text, payload,
		        created_at, updated_at
		 FROM workflow_checkpoints
		 WHERE run_id = $1 AND status IN ('failed','paused','pending')
		 ORDER BY ended_at DESC
		 LIMIT 1`,
		runID,
	)
	out := &WorkflowCheckpoint{}
	var errText sql.NullString
	if err := row.Scan(
		&out.ID, &out.RunID, &out.FundID, &out.TradingDate, &out.Step, &out.Status, &out.Attempts,
		&out.StartedAt, &out.EndedAt, &out.DurationMs, &errText, &out.Payload,
		&out.CreatedAt, &out.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if errText.Valid {
		out.ErrorText = errText.String
	}
	return out, nil
}

func scanCheckpointRows(rows *sql.Rows) ([]WorkflowCheckpoint, error) {
	var out []WorkflowCheckpoint
	for rows.Next() {
		var cp WorkflowCheckpoint
		var errText sql.NullString
		if err := rows.Scan(
			&cp.ID, &cp.RunID, &cp.FundID, &cp.TradingDate, &cp.Step, &cp.Status, &cp.Attempts,
			&cp.StartedAt, &cp.EndedAt, &cp.DurationMs, &errText, &cp.Payload,
			&cp.CreatedAt, &cp.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("workflow_checkpoint_repo: scan row: %w", err)
		}
		if errText.Valid {
			cp.ErrorText = errText.String
		}
		out = append(out, cp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflow_checkpoint_repo: iterate rows: %w", err)
	}
	return out, nil
}

func isCheckpointStatus(s string) bool {
	switch s {
	case "success", "failed", "skipped", "pending", "paused":
		return true
	default:
		return false
	}
}
