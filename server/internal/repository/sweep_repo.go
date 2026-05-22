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

// SweepRepo persists parameter-sweep headers (the 1-to-N "group"
// that a sweep submission expands into). The N child jobs
// themselves still live in backtest_jobs, joined back via the
// sweep_id foreign key.
//
// Reads + writes are independent of BacktestRepo so the two
// concerns stay decoupled — the adapter wires them together.
type SweepRepo struct {
	db *sql.DB
}

func NewSweepRepo(db *sql.DB) *SweepRepo {
	return &SweepRepo{db: db}
}

// SweepRow is the on-disk header for one sweep submission.
type SweepRow struct {
	ID          string
	FundID      string
	UserID      string
	Name        string
	BaseRequest json.RawMessage
	Axes        json.RawMessage
	TotalCells  int
	CreatedAt   time.Time
}

// Insert persists a freshly-fanned sweep. Idempotent on PK
// conflict only in the trivial "same UUID" sense — callers
// should never reuse IDs.
func (r *SweepRepo) Insert(ctx context.Context, row *SweepRow) error {
	if row == nil {
		return errors.New("sweep_repo: nil row")
	}
	if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.FundID) == "" {
		return errors.New("sweep_repo: id and fund_id required")
	}
	base, err := normaliseJSON(row.BaseRequest)
	if err != nil {
		return fmt.Errorf("sweep_repo: marshal base: %w", err)
	}
	axes, err := normaliseJSON(row.Axes)
	if err != nil {
		return fmt.Errorf("sweep_repo: marshal axes: %w", err)
	}
	if _, err := r.db.ExecContext(ctx,
		`INSERT INTO backtest_sweeps (id, fund_id, user_id, name, base_request, axes, total_cells, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		row.ID, row.FundID, row.UserID, row.Name, base, axes, row.TotalCells, row.CreatedAt,
	); err != nil {
		return fmt.Errorf("sweep_repo: insert: %w", err)
	}
	return nil
}

// Get returns the sweep header by ID; ErrNotFound on miss.
func (r *SweepRepo) Get(ctx context.Context, id string) (*SweepRow, error) {
	out, err := scanSweep(r.db.QueryRowContext(ctx, sweepSelect+` WHERE id = $1`, id))
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByFund returns sweep headers for one fund in newest-first
// order. The limit caps at 200 to avoid runaway pulls.
func (r *SweepRepo) ListByFund(ctx context.Context, fundID string, limit int) ([]SweepRow, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		sweepSelect+` WHERE fund_id = $1 ORDER BY created_at DESC LIMIT $2`,
		fundID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("sweep_repo: list: %w", err)
	}
	defer rows.Close()
	out := make([]SweepRow, 0, 16)
	for rows.Next() {
		s, err := scanSweep(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *s)
	}
	return out, rows.Err()
}

const sweepSelect = `
SELECT id, fund_id, user_id, name, base_request, axes, total_cells, created_at
  FROM backtest_sweeps`

func scanSweep(s rowScanner) (*SweepRow, error) {
	var row SweepRow
	var base, axes []byte
	if err := s.Scan(&row.ID, &row.FundID, &row.UserID, &row.Name, &base, &axes, &row.TotalCells, &row.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("sweep_repo: scan: %w", err)
	}
	if len(base) > 0 {
		row.BaseRequest = json.RawMessage(base)
	}
	if len(axes) > 0 {
		row.Axes = json.RawMessage(axes)
	}
	return &row, nil
}
