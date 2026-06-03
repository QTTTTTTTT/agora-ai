// Sprint 11.4 — LLMHealthRepo aggregates investment_plans by
// decision_source / fallback category over an admin-supplied window.
// Kept in its own file (rather than shoved into PlanRepo) because the
// query shape is read-only / dashboard-only and we don't want to
// inflate the very broad PlanRepo surface with another row type.
//
// Concurrency: stateless; safe for concurrent callers.

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// LLMHealthRepo is the admin-only reader for decision-provenance
// aggregates. It is intentionally NOT exposed to fund-scoped APIs —
// the data leaked here includes the raw fallback_reason JSONB which
// must stay admin-only.
type LLMHealthRepo struct {
	db *sql.DB
}

func NewLLMHealthRepo(db *sql.DB) *LLMHealthRepo {
	return &LLMHealthRepo{db: db}
}

// SourceCount is one row of the per-source aggregate.
type SourceCount struct {
	Source string `json:"source"`
	Count  int64  `json:"count"`
}

// FallbackCategoryCount is one row of the per-category aggregate.
// Provider is the coarse vendor tag from the JSONB blob; empty when
// the failure happened before a provider was selected (auth / budget
// rejections).
type FallbackCategoryCount struct {
	Category string `json:"category"`
	Provider string `json:"provider,omitempty"`
	Count    int64  `json:"count"`
}

// FallbackPlan is one recent fallback row surfaced for the admin
// LLM-health board. The Summary field is the raw provider message —
// the admin board is the ONE surface in the product that displays it.
type FallbackPlan struct {
	PlanID    string    `json:"planId"`
	FundID    string    `json:"fundId"`
	Source    string    `json:"source"`
	Category  string    `json:"category,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	Summary   string    `json:"summary,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

// AggregateBySource returns one row per decision_source covering plans
// created within the last `window`. window is clamped to [1h, 30d] —
// outside that range the query becomes either useless (too short) or
// expensive (too long). 0 means "use default" (24h).
func (r *LLMHealthRepo) AggregateBySource(ctx context.Context, window time.Duration) ([]SourceCount, error) {
	since := clampHealthWindow(window)
	rows, err := r.db.QueryContext(ctx,
		`SELECT COALESCE(decision_source, 'legacy') AS src, COUNT(*)
		   FROM investment_plans
		  WHERE created_at >= NOW() - $1::interval
		  GROUP BY 1
		  ORDER BY 2 DESC`,
		intervalLiteral(since),
	)
	if err != nil {
		return nil, fmt.Errorf("llm_health_repo: aggregate by source: %w", err)
	}
	defer rows.Close()
	var out []SourceCount
	for rows.Next() {
		var sc SourceCount
		if err := rows.Scan(&sc.Source, &sc.Count); err != nil {
			return nil, fmt.Errorf("llm_health_repo: scan source row: %w", err)
		}
		out = append(out, sc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llm_health_repo: iterate source rows: %w", err)
	}
	return out, nil
}

// AggregateByCategory returns one row per (category, provider) over
// the recent window — only fallback_* rows are considered. Used for
// the "what's breaking" panel and the SRE alert query.
func (r *LLMHealthRepo) AggregateByCategory(ctx context.Context, window time.Duration) ([]FallbackCategoryCount, error) {
	since := clampHealthWindow(window)
	rows, err := r.db.QueryContext(ctx,
		`SELECT COALESCE(fallback_reason->>'category', 'unknown') AS category,
		        COALESCE(fallback_reason->>'provider', '')        AS provider,
		        COUNT(*)
		   FROM investment_plans
		  WHERE created_at >= NOW() - $1::interval
		    AND decision_source LIKE 'fallback_%'
		  GROUP BY 1, 2
		  ORDER BY 3 DESC`,
		intervalLiteral(since),
	)
	if err != nil {
		return nil, fmt.Errorf("llm_health_repo: aggregate by category: %w", err)
	}
	defer rows.Close()
	var out []FallbackCategoryCount
	for rows.Next() {
		var row FallbackCategoryCount
		if err := rows.Scan(&row.Category, &row.Provider, &row.Count); err != nil {
			return nil, fmt.Errorf("llm_health_repo: scan category row: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llm_health_repo: iterate category rows: %w", err)
	}
	return out, nil
}

// RecentFallbacks lists the N most recent fallback plans within the
// admin-supplied window. limit is clamped to [1, 200].
func (r *LLMHealthRepo) RecentFallbacks(ctx context.Context, window time.Duration, limit int) ([]FallbackPlan, error) {
	since := clampHealthWindow(window)
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, fund_id,
		        COALESCE(decision_source, 'legacy'),
		        COALESCE(fallback_reason->>'category', ''),
		        COALESCE(fallback_reason->>'provider', ''),
		        COALESCE(fallback_reason->>'model',    ''),
		        COALESCE(fallback_reason->>'summary',  ''),
		        created_at
		   FROM investment_plans
		  WHERE created_at >= NOW() - $1::interval
		    AND decision_source LIKE 'fallback_%'
		  ORDER BY created_at DESC
		  LIMIT $2`,
		intervalLiteral(since),
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("llm_health_repo: recent fallbacks: %w", err)
	}
	defer rows.Close()
	var out []FallbackPlan
	for rows.Next() {
		var p FallbackPlan
		if err := rows.Scan(&p.PlanID, &p.FundID, &p.Source, &p.Category, &p.Provider, &p.Model, &p.Summary, &p.CreatedAt); err != nil {
			return nil, fmt.Errorf("llm_health_repo: scan recent: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("llm_health_repo: iterate recent: %w", err)
	}
	return out, nil
}

// clampHealthWindow keeps the admin-supplied window within a sane
// band. 0 / negative → 24h default; > 30d → 30d ceiling to bound
// query cost.
func clampHealthWindow(window time.Duration) time.Duration {
	const (
		defaultWindow = 24 * time.Hour
		maxWindow     = 30 * 24 * time.Hour
		minWindow     = time.Hour
	)
	if window <= 0 {
		return defaultWindow
	}
	if window < minWindow {
		return minWindow
	}
	if window > maxWindow {
		return maxWindow
	}
	return window
}

// intervalLiteral renders a duration as a PostgreSQL INTERVAL string.
// We pass it as $1::interval rather than building the SQL with
// string concatenation to keep the prepared-statement boundary clean.
func intervalLiteral(d time.Duration) string {
	hours := int(d / time.Hour)
	if hours <= 0 {
		hours = 1
	}
	return strings.TrimSpace(fmt.Sprintf("%d hours", hours))
}
