package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// WorkflowActivityEvent mirrors workflow.ActivityEvent but lives in the
// repository package so the persistence layer doesn't take a dependency
// on the workflow package (which would force a circular import). The
// fields are 1:1 with the ring-buffer projection so the bus can translate
// in either direction with a trivial copy.
type WorkflowActivityEvent struct {
	FundID       string    `json:"fund_id"`
	Seq          uint64    `json:"seq"`
	Type         string    `json:"type"`
	Role         string    `json:"role"`
	Step         string    `json:"step,omitempty"`
	RunID        string    `json:"run_id,omitempty"`
	TradingDate  string    `json:"trading_date,omitempty"`
	Message      string    `json:"message"`
	ErrorMessage string    `json:"error,omitempty"`
	EventAt      time.Time `json:"event_at"`
}

// WorkflowActivityRepo persists the projected ActivityEvent stream so the
// Team Live Activity panel survives container restarts and exposes a
// proper "load earlier" surface. The repository is intentionally tiny:
// the bus owns concurrency, batching and seq ordering — we only do raw
// inserts, time-bounded queries, and bulk delete for the retention cron.
type WorkflowActivityRepo struct {
	db *sql.DB
}

// NewWorkflowActivityRepo constructs the repo. db must outlive the repo.
func NewWorkflowActivityRepo(db *sql.DB) *WorkflowActivityRepo {
	return &WorkflowActivityRepo{db: db}
}

// BulkInsert appends one batch of events to workflow_activity_events.
// The (fund_id, seq) UNIQUE index guarantees idempotency if the async
// writer's flush ever retries the same batch: rows that already exist
// are silently kept on `ON CONFLICT DO NOTHING`.
//
// Empty input is a no-op (returns nil) so the async-writer can call
// BulkInsert unconditionally on flush.
func (r *WorkflowActivityRepo) BulkInsert(ctx context.Context, events []WorkflowActivityEvent) error {
	if r == nil || r.db == nil {
		return errors.New("workflow_activity_repo: nil repo")
	}
	if len(events) == 0 {
		return nil
	}

	// Multi-row INSERT with ON CONFLICT for idempotency. Generated with
	// fixed columns so we can use a single prepared statement on the
	// driver side (Postgres caches the plan).
	const columns = `fund_id, seq, type, role, step, run_id, trading_date, message, error_message, event_at`
	var sb strings.Builder
	sb.WriteString("INSERT INTO workflow_activity_events (")
	sb.WriteString(columns)
	sb.WriteString(") VALUES ")
	args := make([]any, 0, len(events)*10)
	for i, evt := range events {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i * 10
		fmt.Fprintf(&sb, "($%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d,$%d)",
			base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8, base+9, base+10,
		)
		args = append(args,
			nullStringIfEmpty(evt.FundID),
			int64(evt.Seq),
			nonEmpty(evt.Type),
			defaultString(evt.Role, "system"),
			nullStringIfEmpty(evt.Step),
			nullStringIfEmpty(evt.RunID),
			nullStringIfEmpty(evt.TradingDate),
			nonEmpty(evt.Message),
			nullStringIfEmpty(evt.ErrorMessage),
			evt.EventAt.UTC(),
		)
	}
	sb.WriteString(" ON CONFLICT (fund_id, seq) DO NOTHING")

	if _, err := r.db.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("workflow_activity_repo: bulk insert: %w", err)
	}
	return nil
}

// ListByFund returns up to `limit` events for fundID, newest first.
// When `before` is non-zero only events strictly older than `before`
// are returned, which is exactly the pagination semantics the UI
// needs for "load earlier" ("show me the page before what I already
// have"). `id` is used as a tiebreaker so equal timestamps still
// produce a stable sort and don't loop the cursor.
//
// `limit` is clamped to a defensive range [1, 500].
func (r *WorkflowActivityRepo) ListByFund(ctx context.Context, fundID string, before time.Time, limit int) ([]WorkflowActivityEvent, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("workflow_activity_repo: nil repo")
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return nil, errors.New("workflow_activity_repo: fund id required")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}

	const baseQuery = `SELECT fund_id, seq, type, role,
		COALESCE(step, ''), COALESCE(run_id, ''), COALESCE(trading_date, ''),
		message, COALESCE(error_message, ''), event_at
		FROM workflow_activity_events
		WHERE fund_id = $1`

	var rows *sql.Rows
	var err error
	if before.IsZero() {
		rows, err = r.db.QueryContext(ctx,
			baseQuery+` ORDER BY event_at DESC, id DESC LIMIT $2`,
			fundID, limit,
		)
	} else {
		rows, err = r.db.QueryContext(ctx,
			baseQuery+` AND event_at < $2 ORDER BY event_at DESC, id DESC LIMIT $3`,
			fundID, before.UTC(), limit,
		)
	}
	if err != nil {
		return nil, fmt.Errorf("workflow_activity_repo: list: %w", err)
	}
	defer rows.Close()

	out := make([]WorkflowActivityEvent, 0, limit)
	for rows.Next() {
		var evt WorkflowActivityEvent
		var seq int64
		if err := rows.Scan(
			&evt.FundID, &seq, &evt.Type, &evt.Role,
			&evt.Step, &evt.RunID, &evt.TradingDate,
			&evt.Message, &evt.ErrorMessage, &evt.EventAt,
		); err != nil {
			return nil, fmt.Errorf("workflow_activity_repo: scan: %w", err)
		}
		if seq < 0 {
			seq = 0
		}
		evt.Seq = uint64(seq)
		out = append(out, evt)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("workflow_activity_repo: rows: %w", err)
	}
	return out, nil
}

// MaxSeqForFund returns the largest seq we've ever persisted for the
// fund. The bus uses this on first publish after restart to seed its
// in-memory totalSeq counter so per-fund Seq remains monotonic across
// process boundaries (essential for SSE sinceSeq backfill).
//
// Returns 0 with err=nil when the fund has no rows yet (fresh fund or
// table truncated by the retention cron); the bus then starts at seq=1
// as it did pre-persistence.
func (r *WorkflowActivityRepo) MaxSeqForFund(ctx context.Context, fundID string) (uint64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("workflow_activity_repo: nil repo")
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return 0, errors.New("workflow_activity_repo: fund id required")
	}
	var seq sql.NullInt64
	if err := r.db.QueryRowContext(ctx,
		`SELECT MAX(seq) FROM workflow_activity_events WHERE fund_id = $1`,
		fundID,
	).Scan(&seq); err != nil {
		return 0, fmt.Errorf("workflow_activity_repo: max seq: %w", err)
	}
	if !seq.Valid || seq.Int64 < 0 {
		return 0, nil
	}
	return uint64(seq.Int64), nil
}

// DeleteOlderThan removes events for `fundID` whose event_at is strictly
// older than `cutoff`. Returns the number of rows deleted (best-effort —
// some drivers don't fill RowsAffected, in which case we return 0).
// The retention cron calls this once per fund with cutoff = now - retentionDays.
func (r *WorkflowActivityRepo) DeleteOlderThan(ctx context.Context, fundID string, cutoff time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("workflow_activity_repo: nil repo")
	}
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		return 0, errors.New("workflow_activity_repo: fund id required")
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM workflow_activity_events WHERE fund_id = $1 AND event_at < $2`,
		fundID, cutoff.UTC(),
	)
	if err != nil {
		return 0, fmt.Errorf("workflow_activity_repo: delete older: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// Driver doesn't support RowsAffected; treat as "we don't know"
		// rather than failing the cron.
		return 0, nil
	}
	return n, nil
}

// nullStringIfEmpty returns sql.NullString. Empty string → NULL so
// queries with `COALESCE(col, '')` keep working and IS NULL filters
// still match the "no value" semantic.
func nullStringIfEmpty(v string) sql.NullString {
	v = strings.TrimSpace(v)
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// nonEmpty returns the trimmed value; empty becomes a literal empty
// string (the column is NOT NULL).
func nonEmpty(v string) string {
	return strings.TrimSpace(v)
}

// defaultString returns fallback when v is empty (after trim).
func defaultString(v, fallback string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return fallback
	}
	return v
}
