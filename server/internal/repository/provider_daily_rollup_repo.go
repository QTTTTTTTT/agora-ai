// Package repository — platform_llm_provider_daily_rollups access
// layer (S14.A).
//
// The rollup loop reads recent usage_entries (last hour, optionally
// last few days for catch-up) and upserts per-(provider, model, day)
// buckets. Reads serve the admin cost dashboard.
//
// We deliberately compute totals inside Postgres (GROUP BY) instead
// of streaming entries into Go: usage_entries is already on the hot
// write path of the LLM router, and reading it twice in a rollup
// without an index-only scan over a wide range would be unfortunate.
// The (model_name, created_at) index keeps the GROUP BY cheap.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProviderDailyRollupRow mirrors a row in platform_llm_provider_daily_rollups.
type ProviderDailyRollupRow struct {
	Provider       string
	ModelName      string
	Day            time.Time
	Calls          int64
	InputTokens    int64
	OutputTokens   int64
	TotalTokens    int64
	CostCents      float64
	CustomKeyCalls int64
	LastRolledAt   time.Time
}

// ProviderDailyRollupRepo manages per-day cost / token buckets.
type ProviderDailyRollupRepo struct {
	db *sql.DB
}

// NewProviderDailyRollupRepo returns nil on nil db.
func NewProviderDailyRollupRepo(db *sql.DB) *ProviderDailyRollupRepo {
	if db == nil {
		return nil
	}
	return &ProviderDailyRollupRepo{db: db}
}

// RecomputeWindow scans usage_entries for [from, to) and rewrites
// the affected (provider, model_name, day) buckets via a single
// INSERT ... SELECT ... ON CONFLICT DO UPDATE. The loop usually
// calls this with from = NOW()-1h, to = NOW(), but startup catch-up
// passes a wider window. Returns the number of rolled buckets so
// the loop can log "rolled N buckets in M ms".
func (r *ProviderDailyRollupRepo) RecomputeWindow(ctx context.Context, from, to time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("provider_daily_rollup_repo: nil db")
	}
	if !from.Before(to) {
		return 0, errors.New("provider_daily_rollup_repo: from must be < to")
	}
	// Find which (provider, model, day) buckets contain rows in the
	// window. Re-roll the FULL day for each such bucket — partial
	// rolls would produce stale numbers because cost_cents would
	// double-count entries already in the bucket.
	res, err := r.db.ExecContext(ctx, `
		WITH affected AS (
		    SELECT DISTINCT
		        LOWER(model_provider) AS provider,
		        model_name,
		        DATE(created_at AT TIME ZONE 'UTC') AS day
		    FROM usage_entries
		    WHERE created_at >= $1 AND created_at < $2
		),
		fresh AS (
		    SELECT
		        LOWER(ue.model_provider)                 AS provider,
		        ue.model_name                            AS model_name,
		        DATE(ue.created_at AT TIME ZONE 'UTC')   AS day,
		        COUNT(*)                                 AS calls,
		        COALESCE(SUM(ue.input_tokens), 0)::bigint  AS input_tokens,
		        COALESCE(SUM(ue.output_tokens), 0)::bigint AS output_tokens,
		        COALESCE(SUM(ue.input_tokens + ue.output_tokens), 0)::bigint AS total_tokens,
		        COALESCE(SUM(ue.cost_cents), 0)::numeric(14,4) AS cost_cents,
		        COUNT(*) FILTER (WHERE ue.is_custom_key) AS custom_key_calls
		    FROM usage_entries ue
		    INNER JOIN affected a
		        ON LOWER(ue.model_provider) = a.provider
		       AND ue.model_name = a.model_name
		       AND DATE(ue.created_at AT TIME ZONE 'UTC') = a.day
		    GROUP BY LOWER(ue.model_provider), ue.model_name, DATE(ue.created_at AT TIME ZONE 'UTC')
		)
		INSERT INTO platform_llm_provider_daily_rollups
		    (provider, model_name, day, calls,
		     input_tokens, output_tokens, total_tokens,
		     cost_cents, custom_key_calls, last_rolled_at)
		SELECT provider, model_name, day, calls,
		       input_tokens, output_tokens, total_tokens,
		       cost_cents, custom_key_calls, NOW()
		FROM fresh
		ON CONFLICT (provider, model_name, day) DO UPDATE
		SET calls            = EXCLUDED.calls,
		    input_tokens     = EXCLUDED.input_tokens,
		    output_tokens    = EXCLUDED.output_tokens,
		    total_tokens     = EXCLUDED.total_tokens,
		    cost_cents       = EXCLUDED.cost_cents,
		    custom_key_calls = EXCLUDED.custom_key_calls,
		    last_rolled_at   = NOW()
	`, from, to)
	if err != nil {
		return 0, fmt.Errorf("provider_daily_rollup_repo: recompute: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListByDayRange returns rollups within [fromDay, toDay] inclusive,
// optionally filtered by a single provider. Used by the cost
// dashboard. Caller-side aggregation keeps SQL simple and lets the
// UI re-pivot freely (by provider vs by day vs by model).
func (r *ProviderDailyRollupRepo) ListByDayRange(ctx context.Context, provider string, fromDay, toDay time.Time) ([]ProviderDailyRollupRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("provider_daily_rollup_repo: nil db")
	}
	if toDay.Before(fromDay) {
		return nil, errors.New("provider_daily_rollup_repo: toDay before fromDay")
	}
	var (
		rows *sql.Rows
		err  error
	)
	if provider == "" {
		rows, err = r.db.QueryContext(ctx, `
			SELECT provider, model_name, day, calls,
			       input_tokens, output_tokens, total_tokens,
			       cost_cents, custom_key_calls, last_rolled_at
			  FROM platform_llm_provider_daily_rollups
			 WHERE day >= $1 AND day <= $2
			 ORDER BY day DESC, provider, model_name`,
			fromDay, toDay)
	} else {
		rows, err = r.db.QueryContext(ctx, `
			SELECT provider, model_name, day, calls,
			       input_tokens, output_tokens, total_tokens,
			       cost_cents, custom_key_calls, last_rolled_at
			  FROM platform_llm_provider_daily_rollups
			 WHERE provider = $1 AND day >= $2 AND day <= $3
			 ORDER BY day DESC, model_name`,
			provider, fromDay, toDay)
	}
	if err != nil {
		return nil, fmt.Errorf("provider_daily_rollup_repo: list: %w", err)
	}
	defer rows.Close()
	out := []ProviderDailyRollupRow{}
	for rows.Next() {
		var row ProviderDailyRollupRow
		if err := rows.Scan(
			&row.Provider, &row.ModelName, &row.Day, &row.Calls,
			&row.InputTokens, &row.OutputTokens, &row.TotalTokens,
			&row.CostCents, &row.CustomKeyCalls, &row.LastRolledAt,
		); err != nil {
			return nil, fmt.Errorf("provider_daily_rollup_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider_daily_rollup_repo: rows: %w", err)
	}
	return out, nil
}

// ProviderCostTotal is the per-provider aggregate used by the
// "top spenders this week" widget.
type ProviderCostTotal struct {
	Provider     string
	Calls        int64
	TotalTokens  int64
	CostCents    float64
	DaysInWindow int
}

// SumByProvider rolls (provider, day) → provider totals across the
// window. Single SQL query.
func (r *ProviderDailyRollupRepo) SumByProvider(ctx context.Context, fromDay, toDay time.Time) ([]ProviderCostTotal, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("provider_daily_rollup_repo: nil db")
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT provider,
		       COALESCE(SUM(calls), 0)::bigint                 AS calls,
		       COALESCE(SUM(total_tokens), 0)::bigint          AS total_tokens,
		       COALESCE(SUM(cost_cents), 0)::numeric(14,4)     AS cost_cents,
		       COUNT(DISTINCT day)::int                        AS days_in_window
		  FROM platform_llm_provider_daily_rollups
		 WHERE day >= $1 AND day <= $2
		 GROUP BY provider
		 ORDER BY cost_cents DESC
	`, fromDay, toDay)
	if err != nil {
		return nil, fmt.Errorf("provider_daily_rollup_repo: sum: %w", err)
	}
	defer rows.Close()
	out := []ProviderCostTotal{}
	for rows.Next() {
		var t ProviderCostTotal
		if err := rows.Scan(&t.Provider, &t.Calls, &t.TotalTokens, &t.CostCents, &t.DaysInWindow); err != nil {
			return nil, fmt.Errorf("provider_daily_rollup_repo: sum scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider_daily_rollup_repo: sum rows: %w", err)
	}
	return out, nil
}
