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

// BacktestRepo is the persistence layer for Phase 2F: backtest
// jobs + their NAV curves + their trade event logs. Reads and
// writes are independent of the in-memory backtest.JobStore — the
// repo holds long-term history while the JobStore handles live
// progress.
//
// Three storage shapes:
//
//   - BacktestJobRow: one row per submitted run; metrics +
//     status mirror what's exposed via the api.BacktestJob view.
//   - BacktestNavPoint: per-day NAV snapshot.
//   - BacktestTradeEvent: per-execution / per-skip entry.
//
// Writes happen at two moments in a run's lifetime:
//   1. Submit — Insert a row with status='queued'.
//   2. Final  — Update status + metrics + bulk-insert NAV +
//      Trades. Wrapped in one TX so a crash mid-write doesn't
//      leave a half-persisted run.
//
// Reads return the same row shape regardless of where the job
// came from (DB-only vs. DB-after-restart vs. DB-after-completion).
type BacktestRepo struct {
	db *sql.DB
}

func NewBacktestRepo(db *sql.DB) *BacktestRepo {
	return &BacktestRepo{db: db}
}

// DB exposes the underlying handle for tests that need to wire up
// transactional fixtures alongside the repo. Not part of the
// public contract — use at your own risk.
func (r *BacktestRepo) DB() *sql.DB { return r.db }

// BacktestJobRow is the on-disk representation of a job. We
// deliberately keep this distinct from api.BacktestJob /
// backtest.Job — the public view embeds in-memory progress info
// that doesn't make sense for a persisted row.
type BacktestJobRow struct {
	ID                string
	FundID            string
	UserID            string
	Name              string
	EngineKind        string
	Status            string
	Request           json.RawMessage
	Error             sql.NullString
	WindowStart       time.Time
	WindowEnd         time.Time
	InitialCash       sql.NullFloat64
	FinalNav          sql.NullFloat64
	CumulativeReturn  sql.NullFloat64
	AnnualizedReturn  sql.NullFloat64
	Volatility        sql.NullFloat64
	SharpeRatio       sql.NullFloat64
	MaxDrawdown       sql.NullFloat64
	WinRate           sql.NullFloat64
	TradeCount        int
	WinningTradeCount int
	LosingTradeCount  int
	TotalDays         int
	DoneDays          int
	SubmittedAt       time.Time
	StartedAt         sql.NullTime
	CompletedAt       sql.NullTime
	// SweepID is non-empty when this job was spawned as part of a
	// parameter sweep (Phase 2H). Nil for one-off backtests.
	SweepID sql.NullString
	// SweepCell is the axis-name → value-string map identifying
	// which cell of the sweep this job covers. Nil when SweepID
	// is unset.
	SweepCell json.RawMessage
	// WalkForward is the JSON-serialised per-fold breakdown for
	// walk-forward runs. Nil for plain backtests. We store it
	// here (rather than its own table) because it's at most ~12
	// folds per job and the UI consumes it as one structure.
	WalkForward json.RawMessage
}

// BacktestNavPoint mirrors backtest.NavPoint for persistence. We
// use float64 for the numeric scalar columns because pgx maps
// NUMERIC to string by default and we don't want to drag the
// shopspring/decimal dependency in just for backtest plots.
type BacktestNavPoint struct {
	Seq           int
	Date          time.Time
	Nav           float64
	Cash          float64
	PositionValue float64
	DrawdownPct   float64
	Positions     json.RawMessage // symbol → quantity
}

// BacktestTradeEvent mirrors backtest.TradeEvent for persistence.
type BacktestTradeEvent struct {
	Seq        int
	Date       time.Time
	Symbol     string
	Action     string
	Status     string
	Quantity   float64
	FillPrice  float64
	Notional   float64
	Reason     sql.NullString
	Confidence sql.NullFloat64
}

// BacktestJobFull is the read-side shape returned by GetWithDetails.
// Pages that show a single backtest (web's /backtests/:id) pull
// this in one round trip.
type BacktestJobFull struct {
	Job    BacktestJobRow
	Nav    []BacktestNavPoint
	Trades []BacktestTradeEvent
}

// InsertQueued writes the initial row for a freshly-submitted
// job. Returns ErrAlreadyExists when the (id) PK conflicts — the
// caller shouldn't retry; in practice IDs are UUIDs from the
// JobStore so collisions are theoretical.
func (r *BacktestRepo) InsertQueued(ctx context.Context, row *BacktestJobRow) error {
	if row == nil {
		return errors.New("backtest_repo: nil row")
	}
	if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.FundID) == "" {
		return errors.New("backtest_repo: id and fund_id required")
	}
	req, err := normaliseJSON(row.Request)
	if err != nil {
		return fmt.Errorf("backtest_repo: marshal request: %w", err)
	}
	// sweep_cell may be nil/empty for one-off jobs; normaliseJSON
	// returns 'null' which Postgres accepts as JSONB NULL.
	sweepCell, err := normaliseJSON(row.SweepCell)
	if err != nil {
		return fmt.Errorf("backtest_repo: marshal sweep_cell: %w", err)
	}
	_, err = r.db.ExecContext(ctx,
		`INSERT INTO backtest_jobs
		    (id, fund_id, user_id, name, engine_kind, status, request,
		     window_start, window_end, submitted_at, total_days, done_days,
		     sweep_id, sweep_cell)
		 VALUES
		    ($1, $2, $3, $4, $5, $6, $7,
		     $8, $9, $10, $11, $12,
		     $13, $14)`,
		row.ID, row.FundID, row.UserID, row.Name, row.EngineKind, row.Status, req,
		row.WindowStart, row.WindowEnd, row.SubmittedAt, row.TotalDays, row.DoneDays,
		row.SweepID, sweepCell,
	)
	if err != nil {
		return fmt.Errorf("backtest_repo: insert: %w", err)
	}
	return nil
}

// UpdateFinal flips a queued/running row to a terminal state and
// writes the denormalised metrics + bulk-inserts NAV points +
// Trades. All in one TX so partial persistence is impossible.
//
// `row` only needs the fields that change between submit and
// final: Status, Error, started/completed timestamps, metrics +
// total/done days. The PK is row.ID; FundID is not re-validated
// (the row was authorised at submit time).
func (r *BacktestRepo) UpdateFinal(ctx context.Context, row *BacktestJobRow, nav []BacktestNavPoint, trades []BacktestTradeEvent) error {
	if row == nil {
		return errors.New("backtest_repo: nil row")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("backtest_repo: begin tx: %w", err)
	}
	defer tx.Rollback()

	walkForward, err := normaliseJSON(row.WalkForward)
	if err != nil {
		return fmt.Errorf("backtest_repo: marshal walk_forward: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE backtest_jobs SET
		     status = $1,
		     error = $2,
		     started_at = $3,
		     completed_at = $4,
		     initial_cash = $5,
		     final_nav = $6,
		     cumulative_return = $7,
		     annualized_return = $8,
		     volatility = $9,
		     sharpe_ratio = $10,
		     max_drawdown = $11,
		     win_rate = $12,
		     trade_count = $13,
		     winning_trade_count = $14,
		     losing_trade_count = $15,
		     total_days = $16,
		     done_days = $17,
		     walk_forward = $18
		 WHERE id = $19`,
		row.Status, row.Error, row.StartedAt, row.CompletedAt,
		row.InitialCash, row.FinalNav,
		row.CumulativeReturn, row.AnnualizedReturn,
		row.Volatility, row.SharpeRatio,
		row.MaxDrawdown, row.WinRate,
		row.TradeCount, row.WinningTradeCount, row.LosingTradeCount,
		row.TotalDays, row.DoneDays,
		walkForward,
		row.ID,
	); err != nil {
		return fmt.Errorf("backtest_repo: update: %w", err)
	}

	// Delete-then-insert for nav/trades so a retried UpdateFinal
	// (e.g., a flaky DB rolled back the first commit) doesn't
	// produce duplicate rows. The job-level UPDATE above already
	// scoped the writes to one job by ID.
	if _, err := tx.ExecContext(ctx, `DELETE FROM backtest_nav_points WHERE job_id = $1`, row.ID); err != nil {
		return fmt.Errorf("backtest_repo: clear nav: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM backtest_trade_events WHERE job_id = $1`, row.ID); err != nil {
		return fmt.Errorf("backtest_repo: clear trades: %w", err)
	}

	if err := insertNavBatch(ctx, tx, row.ID, nav); err != nil {
		return err
	}
	if err := insertTradesBatch(ctx, tx, row.ID, trades); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("backtest_repo: commit: %w", err)
	}
	return nil
}

// MarkInterruptedActive is the startup sweep: any row in a
// non-terminal state belongs to a process that crashed. Flip it
// to 'failed' with a friendly error.
//
// Returns the number of rows touched so the boot logs can report
// "swept N interrupted backtest jobs".
func (r *BacktestRepo) MarkInterruptedActive(ctx context.Context, now time.Time) (int, error) {
	res, err := r.db.ExecContext(ctx,
		`UPDATE backtest_jobs
		    SET status = 'failed',
		        error  = COALESCE(NULLIF(error, ''), 'server restart before completion'),
		        completed_at = COALESCE(completed_at, $1)
		  WHERE status IN ('queued', 'running')`,
		now,
	)
	if err != nil {
		return 0, fmt.Errorf("backtest_repo: sweep: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// GetWithDetails returns the full job + nav + trades by id. Used
// by the api handler when the in-memory store doesn't have the
// job. Returns ErrNotFound on a missing row.
func (r *BacktestRepo) GetWithDetails(ctx context.Context, id string) (*BacktestJobFull, error) {
	job, err := r.GetJob(ctx, id)
	if err != nil {
		return nil, err
	}
	nav, err := r.ListNav(ctx, id)
	if err != nil {
		return nil, err
	}
	trades, err := r.ListTrades(ctx, id)
	if err != nil {
		return nil, err
	}
	return &BacktestJobFull{Job: *job, Nav: nav, Trades: trades}, nil
}

// GetJob fetches just the job row, no children. Used for cheap
// listing operations.
func (r *BacktestRepo) GetJob(ctx context.Context, id string) (*BacktestJobRow, error) {
	row := r.db.QueryRowContext(ctx, backtestJobSelect+` WHERE id = $1`, id)
	out, err := scanBacktestJob(row)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListByFund returns the newest-first job ledger for one fund. We
// cap the limit at 500 so a misbehaving client doesn't drain the
// whole table.
func (r *BacktestRepo) ListByFund(ctx context.Context, fundID string, limit int) ([]BacktestJobRow, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx,
		backtestJobSelect+` WHERE fund_id = $1 ORDER BY submitted_at DESC LIMIT $2`,
		fundID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("backtest_repo: list by fund: %w", err)
	}
	defer rows.Close()
	out := make([]BacktestJobRow, 0, 16)
	for rows.Next() {
		j, err := scanBacktestJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("backtest_repo: list by fund rows: %w", err)
	}
	return out, nil
}

// ListBySweep returns all child jobs of a sweep in submit order
// (oldest first — sweeps are typically rendered by axis index,
// which matches submit order).
func (r *BacktestRepo) ListBySweep(ctx context.Context, sweepID string) ([]BacktestJobRow, error) {
	rows, err := r.db.QueryContext(ctx,
		backtestJobSelect+` WHERE sweep_id = $1 ORDER BY submitted_at ASC`,
		sweepID,
	)
	if err != nil {
		return nil, fmt.Errorf("backtest_repo: list by sweep: %w", err)
	}
	defer rows.Close()
	out := make([]BacktestJobRow, 0, 16)
	for rows.Next() {
		j, err := scanBacktestJob(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *j)
	}
	return out, rows.Err()
}

// ListNav returns nav points for a job in seq order.
func (r *BacktestRepo) ListNav(ctx context.Context, jobID string) ([]BacktestNavPoint, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT seq, date, nav, cash, position_value, drawdown_pct, positions
		   FROM backtest_nav_points WHERE job_id = $1 ORDER BY seq ASC`,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("backtest_repo: list nav: %w", err)
	}
	defer rows.Close()
	out := make([]BacktestNavPoint, 0, 64)
	for rows.Next() {
		var p BacktestNavPoint
		var positions []byte
		if err := rows.Scan(&p.Seq, &p.Date, &p.Nav, &p.Cash, &p.PositionValue, &p.DrawdownPct, &positions); err != nil {
			return nil, fmt.Errorf("backtest_repo: scan nav: %w", err)
		}
		if len(positions) > 0 {
			// Defensive copy: sql.Rows reuses the backing slice
			// across iterations.
			p.Positions = append(json.RawMessage(nil), positions...)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListTrades returns trade events for a job in seq order.
func (r *BacktestRepo) ListTrades(ctx context.Context, jobID string) ([]BacktestTradeEvent, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT seq, date, symbol, action, status, quantity, fill_price, notional, reason, confidence
		   FROM backtest_trade_events WHERE job_id = $1 ORDER BY seq ASC`,
		jobID,
	)
	if err != nil {
		return nil, fmt.Errorf("backtest_repo: list trades: %w", err)
	}
	defer rows.Close()
	out := make([]BacktestTradeEvent, 0, 64)
	for rows.Next() {
		var t BacktestTradeEvent
		if err := rows.Scan(&t.Seq, &t.Date, &t.Symbol, &t.Action, &t.Status, &t.Quantity, &t.FillPrice, &t.Notional, &t.Reason, &t.Confidence); err != nil {
			return nil, fmt.Errorf("backtest_repo: scan trade: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// CancelByID flips a queued/running job to 'cancelled' on the
// repo side. The in-memory cancellation still happens via
// JobStore.Cancel — this is the audit-trail write.
func (r *BacktestRepo) CancelByID(ctx context.Context, id string, now time.Time) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE backtest_jobs
		    SET status = 'cancelled', completed_at = $1
		  WHERE id = $2 AND status IN ('queued','running')`,
		now, id,
	)
	if err != nil {
		return fmt.Errorf("backtest_repo: cancel: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the row doesn't exist or it's already in a
		// terminal state. Either way the caller's intent
		// (cancellation) is no longer meaningful; return
		// ErrNotFound so the adapter can map to 409.
		return ErrNotFound
	}
	return nil
}

// -------------------- helpers --------------------

// backtestJobSelect is the column list shared by every job-row
// query so adding a column only requires one edit.
const backtestJobSelect = `
SELECT id, fund_id, user_id, name, engine_kind, status, request, error,
       window_start, window_end,
       initial_cash, final_nav, cumulative_return, annualized_return,
       volatility, sharpe_ratio, max_drawdown, win_rate,
       trade_count, winning_trade_count, losing_trade_count,
       total_days, done_days,
       submitted_at, started_at, completed_at,
       sweep_id, sweep_cell, walk_forward
  FROM backtest_jobs`

// rowScanner is the minimal interface implemented by both *sql.Row
// and *sql.Rows so a single Scan helper handles both code paths.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanBacktestJob(s rowScanner) (*BacktestJobRow, error) {
	var j BacktestJobRow
	// []byte instead of sql.RawBytes because RawBytes is rejected
	// by *sql.Row.Scan (RawBytes is only valid while *sql.Rows
	// hasn't advanced — single-row Scan has no such anchor).
	var request []byte
	var sweepCell []byte
	var walkForward []byte
	err := s.Scan(
		&j.ID, &j.FundID, &j.UserID, &j.Name, &j.EngineKind, &j.Status, &request, &j.Error,
		&j.WindowStart, &j.WindowEnd,
		&j.InitialCash, &j.FinalNav, &j.CumulativeReturn, &j.AnnualizedReturn,
		&j.Volatility, &j.SharpeRatio, &j.MaxDrawdown, &j.WinRate,
		&j.TradeCount, &j.WinningTradeCount, &j.LosingTradeCount,
		&j.TotalDays, &j.DoneDays,
		&j.SubmittedAt, &j.StartedAt, &j.CompletedAt,
		&j.SweepID, &sweepCell, &walkForward,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("backtest_repo: scan job: %w", err)
	}
	if len(request) > 0 {
		j.Request = json.RawMessage(request)
	}
	// "null" JSONB sentinels come back as 4 literal bytes; collapse
	// them to nil so the API layer doesn't echo a "null" string.
	if len(sweepCell) > 0 && string(sweepCell) != "null" {
		j.SweepCell = json.RawMessage(sweepCell)
	}
	if len(walkForward) > 0 && string(walkForward) != "null" {
		j.WalkForward = json.RawMessage(walkForward)
	}
	return &j, nil
}

// insertNavBatch / insertTradesBatch use one INSERT with multiple
// VALUES tuples. Postgres caps that at ~65535 parameters; even
// the chunkiest backtest (10 years × 250 days) only needs ~2500
// rows, well under the limit.
func insertNavBatch(ctx context.Context, tx *sql.Tx, jobID string, points []BacktestNavPoint) error {
	if len(points) == 0 {
		return nil
	}
	const cols = 7
	args := make([]any, 0, len(points)*cols+1)
	args = append(args, jobID)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO backtest_nav_points (job_id, seq, date, nav, cash, position_value, drawdown_pct, positions) VALUES `)
	for i, p := range points {
		if i > 0 {
			sb.WriteString(",")
		}
		base := i*cols + 2
		fmt.Fprintf(&sb, "($1, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6,
		)
		positions, err := normaliseJSON(p.Positions)
		if err != nil {
			return fmt.Errorf("backtest_repo: positions json: %w", err)
		}
		args = append(args, p.Seq, p.Date, p.Nav, p.Cash, p.PositionValue, p.DrawdownPct, positions)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("backtest_repo: insert nav: %w", err)
	}
	return nil
}

func insertTradesBatch(ctx context.Context, tx *sql.Tx, jobID string, trades []BacktestTradeEvent) error {
	if len(trades) == 0 {
		return nil
	}
	const cols = 9
	args := make([]any, 0, len(trades)*cols+1)
	args = append(args, jobID)
	var sb strings.Builder
	sb.WriteString(`INSERT INTO backtest_trade_events (job_id, seq, date, symbol, action, status, quantity, fill_price, notional, reason, confidence) VALUES `)
	for i, t := range trades {
		if i > 0 {
			sb.WriteString(",")
		}
		base := i*cols + 2
		fmt.Fprintf(&sb, "($1, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			base, base+1, base+2, base+3, base+4, base+5, base+6, base+7, base+8,
		)
		args = append(args, t.Seq, t.Date, t.Symbol, t.Action, t.Status, t.Quantity, t.FillPrice, t.Notional, t.Reason, t.Confidence)
	}
	if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
		return fmt.Errorf("backtest_repo: insert trades: %w", err)
	}
	return nil
}

// normaliseJSON returns a JSONB-friendly []byte. Empty / nil
// payloads become 'null' so Postgres accepts them; invalid JSON
// causes an early error rather than a confusing DB-side reject.
func normaliseJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return []byte("null"), nil
	}
	if !json.Valid(raw) {
		return nil, errors.New("invalid JSON payload")
	}
	return []byte(raw), nil
}
