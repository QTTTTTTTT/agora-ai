package varisk

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Repo persists VaR / CVaR snapshots and reads the daily-return
// time series that feeds the engine.
//
// We intentionally do NOT depend on internal/repository — the
// varisk package needs a narrow, focused query for the
// nav_snapshots.daily_return column and that's it. The handler
// layer is welcome to fetch full NavSnapshot rows from
// repository and pass DailyReturn into the engine; this Repo
// is only here so the handler can write back snapshots and
// read the trend.
type Repo struct {
	db *sql.DB
}

func NewRepo(db *sql.DB) *Repo {
	return &Repo{db: db}
}

// DailyReturnsParams scopes the time-series read.
type DailyReturnsParams struct {
	FundID       string
	LookbackDays int
	// AsOf is the latest day to include. Pass zero value for
	// "today" (server time). Useful in tests so they don't
	// drift with wall clock.
	AsOf time.Time
}

// DailyReturns reads up to LookbackDays consecutive
// nav_snapshots rows ending at AsOf and returns the
// daily_return column. Missing trading days (holidays,
// weekends, gaps) are silently dropped because nav_snapshots
// only stores days that actually had a NAV computation.
//
// Returned in chronological order, oldest first.
func (r *Repo) DailyReturns(ctx context.Context, p DailyReturnsParams) ([]DailyReturn, error) {
	if p.FundID == "" {
		return nil, errors.New("varisk: fund_id required")
	}
	if p.LookbackDays <= 0 {
		return nil, fmt.Errorf("varisk: lookback_days %d must be > 0", p.LookbackDays)
	}
	asOf := p.AsOf
	if asOf.IsZero() {
		asOf = time.Now().UTC()
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT trading_date, daily_return
		   FROM nav_snapshots
		  WHERE fund_id = $1
		    AND trading_date <= $2::date
		  ORDER BY trading_date DESC
		  LIMIT $3`,
		p.FundID, asOf, p.LookbackDays,
	)
	if err != nil {
		return nil, fmt.Errorf("varisk: query daily_return: %w", err)
	}
	defer rows.Close()

	var out []DailyReturn
	for rows.Next() {
		var dr DailyReturn
		if err := rows.Scan(&dr.Date, &dr.Value); err != nil {
			return nil, fmt.Errorf("varisk: scan daily_return: %w", err)
		}
		out = append(out, dr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("varisk: iter daily_return: %w", err)
	}
	// We selected DESC; reverse so the engine sees chronological order.
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// AppendSnapshot writes every Result in the Snapshot as one
// row. All rows share GeneratedAt so the dashboard can group
// them into a coherent "tile set".
//
// Append-only by design: no UPDATE path. If you need to
// recompute, write a fresh snapshot.
func (r *Repo) AppendSnapshot(ctx context.Context, s Snapshot) error {
	if s.FundID == "" {
		return errors.New("varisk: fund_id required")
	}
	if len(s.Results) == 0 {
		return nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("varisk: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO portfolio_var_snapshots (
			fund_id, calculated_at, method, confidence, horizon_days,
			var_pct, cvar_pct,
			sample_window_start, sample_window_end, sample_size, lookback_days,
			mean_daily_return, stdev_daily_return,
			monte_carlo_seed, monte_carlo_paths
		) VALUES (
			$1, $2, $3, $4, $5,
			$6, $7,
			$8, $9, $10, $11,
			$12, $13,
			$14, $15
		)`,
	)
	if err != nil {
		return fmt.Errorf("varisk: prepare insert: %w", err)
	}
	defer stmt.Close()

	for _, res := range s.Results {
		var seedArg interface{}
		var pathsArg interface{}
		if res.MonteCarloSeed != nil {
			seedArg = *res.MonteCarloSeed
		}
		if res.MonteCarloPaths != nil {
			pathsArg = *res.MonteCarloPaths
		}
		var meanArg, stdArg interface{}
		if s.SampleSize >= 2 {
			meanArg = res.Mean
			stdArg = res.Std
		}
		var winStart, winEnd interface{}
		if !res.SampleWindowStart.IsZero() {
			winStart = res.SampleWindowStart
		}
		if !res.SampleWindowEnd.IsZero() {
			winEnd = res.SampleWindowEnd
		}
		if _, err := stmt.ExecContext(ctx,
			s.FundID, s.GeneratedAt, string(res.Method), float64(res.Confidence), res.Horizon,
			res.Var, res.CVar,
			winStart, winEnd, res.SampleSize, s.LookbackDays,
			meanArg, stdArg,
			seedArg, pathsArg,
		); err != nil {
			return fmt.Errorf("varisk: insert snapshot row: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("varisk: commit snapshot: %w", err)
	}
	return nil
}

// TrendRow is one historical archive row used to render the
// "VaR over time" sparkline.
type TrendRow struct {
	ID            int64
	FundID        string
	CalculatedAt  time.Time
	Method        Method
	Confidence    Confidence
	HorizonDays   int
	Var           float64
	CVar          float64
	SampleSize    int
	LookbackDays  int
}

// ListSnapshotsParams filters the trend query. Method /
// Confidence / HorizonDays are required so the trend renders a
// single consistent line; Limit caps the result count.
type ListSnapshotsParams struct {
	FundID      string
	Method      Method
	Confidence  Confidence
	HorizonDays int
	Limit       int
}

// ListSnapshots returns up to Limit rows for the matching
// (fund, method, confidence, horizon) combo, newest first.
func (r *Repo) ListSnapshots(ctx context.Context, p ListSnapshotsParams) ([]TrendRow, error) {
	if p.FundID == "" {
		return nil, errors.New("varisk: fund_id required")
	}
	if !p.Method.IsValid() {
		return nil, fmt.Errorf("varisk: invalid method %q", p.Method)
	}
	if !p.Confidence.IsValid() {
		return nil, formatErrInvalidConf(p.Confidence)
	}
	if p.HorizonDays < 1 || p.HorizonDays > 20 {
		return nil, fmt.Errorf("varisk: horizon_days %d out of range", p.HorizonDays)
	}
	limit := p.Limit
	if limit <= 0 || limit > 365 {
		limit = 90
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id, calculated_at, method, confidence, horizon_days,
		        var_pct, cvar_pct, sample_size, lookback_days
		   FROM portfolio_var_snapshots
		  WHERE fund_id = $1
		    AND method = $2
		    AND confidence = $3
		    AND horizon_days = $4
		  ORDER BY calculated_at DESC
		  LIMIT $5`,
		p.FundID, string(p.Method), float64(p.Confidence), p.HorizonDays, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("varisk: query trend: %w", err)
	}
	defer rows.Close()
	var out []TrendRow
	for rows.Next() {
		var tr TrendRow
		var method string
		var conf float64
		if err := rows.Scan(&tr.ID, &tr.FundID, &tr.CalculatedAt, &method, &conf, &tr.HorizonDays,
			&tr.Var, &tr.CVar, &tr.SampleSize, &tr.LookbackDays); err != nil {
			return nil, fmt.Errorf("varisk: scan trend row: %w", err)
		}
		tr.Method = Method(method)
		tr.Confidence = Confidence(conf)
		out = append(out, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("varisk: iter trend rows: %w", err)
	}
	return out, nil
}
