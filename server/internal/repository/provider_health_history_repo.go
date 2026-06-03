// Package repository — platform_llm_provider_health_history access
// layer (S14.A).
//
// The probe loop inserts one row per 5-minute tick per active
// provider; the admin dashboard reads a rolling window per
// provider; the retention sweep drops rows older than 30 days.
// All three operations are simple SQL — no caching, no batching.
// The probe loop already coalesces 5-minute ticks so write volume
// is bounded.

package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProviderHealthRow mirrors a row in platform_llm_provider_health_history.
type ProviderHealthRow struct {
	ID         uuid.UUID
	ProviderID uuid.UUID
	Provider   string
	Label      string
	CheckedAt  time.Time
	OK         bool
	LatencyMS  int
	HTTPStatus int
	Message    sql.NullString
	ModelName  sql.NullString
}

// ProviderHealthHistoryRepo wraps the platform_llm_provider_health_history
// table with insert / list / cleanup helpers.
type ProviderHealthHistoryRepo struct {
	db *sql.DB
}

// NewProviderHealthHistoryRepo wires a *sql.DB. Returns nil on nil
// db so the wiring layer can degrade without panicking.
func NewProviderHealthHistoryRepo(db *sql.DB) *ProviderHealthHistoryRepo {
	if db == nil {
		return nil
	}
	return &ProviderHealthHistoryRepo{db: db}
}

// Insert records a single ping result. Errors are surfaced but
// callers (the probe loop) typically log-and-continue so one DB
// hiccup doesn't kill the loop.
func (r *ProviderHealthHistoryRepo) Insert(ctx context.Context, row ProviderHealthRow) error {
	if r == nil || r.db == nil {
		return errors.New("provider_health_history_repo: nil db")
	}
	if row.ProviderID == uuid.Nil {
		return errors.New("provider_health_history_repo: provider_id required")
	}
	if row.CheckedAt.IsZero() {
		row.CheckedAt = time.Now().UTC()
	}
	if row.LatencyMS < 0 {
		row.LatencyMS = 0
	}
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO platform_llm_provider_health_history
		    (provider_id, provider, label, checked_at,
		     ok, latency_ms, http_status, message, model_name)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		row.ProviderID, strings.ToLower(strings.TrimSpace(row.Provider)),
		strings.TrimSpace(row.Label),
		row.CheckedAt, row.OK, row.LatencyMS, row.HTTPStatus,
		row.Message, row.ModelName,
	)
	if err != nil {
		return fmt.Errorf("provider_health_history_repo: insert: %w", err)
	}
	return nil
}

// ListRecent returns rows for one provider, newest-first, capped
// by limit. The dashboard uses this for the per-provider sparkline.
// providerID == uuid.Nil returns rows across ALL providers (used
// by the global "top offenders" view).
func (r *ProviderHealthHistoryRepo) ListRecent(ctx context.Context, providerID uuid.UUID, since time.Time, limit int) ([]ProviderHealthRow, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("provider_health_history_repo: nil db")
	}
	if limit <= 0 || limit > 5000 {
		limit = 1000
	}
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	var (
		rows *sql.Rows
		err  error
	)
	if providerID == uuid.Nil {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, provider_id, provider, label, checked_at,
			        ok, latency_ms, http_status, message, model_name
			   FROM platform_llm_provider_health_history
			  WHERE checked_at >= $1
			  ORDER BY checked_at DESC
			  LIMIT $2`, since, limit)
	} else {
		rows, err = r.db.QueryContext(ctx,
			`SELECT id, provider_id, provider, label, checked_at,
			        ok, latency_ms, http_status, message, model_name
			   FROM platform_llm_provider_health_history
			  WHERE provider_id = $1 AND checked_at >= $2
			  ORDER BY checked_at DESC
			  LIMIT $3`, providerID, since, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("provider_health_history_repo: list: %w", err)
	}
	defer rows.Close()
	out := []ProviderHealthRow{}
	for rows.Next() {
		var row ProviderHealthRow
		if err := rows.Scan(
			&row.ID, &row.ProviderID, &row.Provider, &row.Label, &row.CheckedAt,
			&row.OK, &row.LatencyMS, &row.HTTPStatus, &row.Message, &row.ModelName,
		); err != nil {
			return nil, fmt.Errorf("provider_health_history_repo: scan: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider_health_history_repo: rows: %w", err)
	}
	return out, nil
}

// ProviderHealthSummary is the aggregated shape the dashboard
// shows in the top row of the observability tab. One row per
// provider over the requested window.
type ProviderHealthSummary struct {
	ProviderID    uuid.UUID
	Provider      string
	Label         string
	Checks        int
	Successes     int
	Failures      int
	LatencyP50    int
	LatencyP95    int
	LatencyMax    int
	LastCheckedAt time.Time
	LastOK        sql.NullBool
}

// SuccessRate returns successes / checks as a 0..1 float, or 0
// when there were no checks in the window.
func (s ProviderHealthSummary) SuccessRate() float64 {
	if s.Checks == 0 {
		return 0
	}
	return float64(s.Successes) / float64(s.Checks)
}

// SummariseByProvider returns one ProviderHealthSummary per
// provider seen in the window. Single SQL query — uses Postgres
// percentile_cont for p50/p95 so the math stays out of Go.
func (r *ProviderHealthHistoryRepo) SummariseByProvider(ctx context.Context, since time.Time) ([]ProviderHealthSummary, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("provider_health_history_repo: nil db")
	}
	if since.IsZero() {
		since = time.Now().Add(-24 * time.Hour)
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT
		    provider_id,
		    MAX(provider)                                                   AS provider,
		    MAX(label)                                                      AS label,
		    COUNT(*)::int                                                   AS checks,
		    COUNT(*) FILTER (WHERE ok)::int                                 AS successes,
		    COUNT(*) FILTER (WHERE NOT ok)::int                             AS failures,
		    COALESCE(PERCENTILE_CONT(0.50) WITHIN GROUP (ORDER BY latency_ms), 0)::int AS p50,
		    COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY latency_ms), 0)::int AS p95,
		    COALESCE(MAX(latency_ms), 0)                                    AS p_max,
		    MAX(checked_at)                                                 AS last_checked_at,
		    (ARRAY_AGG(ok ORDER BY checked_at DESC))[1]                     AS last_ok
		FROM platform_llm_provider_health_history
		WHERE checked_at >= $1
		GROUP BY provider_id
		ORDER BY provider, label
	`, since)
	if err != nil {
		return nil, fmt.Errorf("provider_health_history_repo: summarise: %w", err)
	}
	defer rows.Close()
	out := []ProviderHealthSummary{}
	for rows.Next() {
		var s ProviderHealthSummary
		if err := rows.Scan(
			&s.ProviderID, &s.Provider, &s.Label,
			&s.Checks, &s.Successes, &s.Failures,
			&s.LatencyP50, &s.LatencyP95, &s.LatencyMax,
			&s.LastCheckedAt, &s.LastOK,
		); err != nil {
			return nil, fmt.Errorf("provider_health_history_repo: summarise scan: %w", err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("provider_health_history_repo: summarise rows: %w", err)
	}
	return out, nil
}

// DeleteOlderThan drops rows beyond the retention boundary. The
// probe loop calls this once per startup AND once per day so a
// long-running pod also stays bounded. Returns the number of
// deleted rows for observability.
func (r *ProviderHealthHistoryRepo) DeleteOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	if r == nil || r.db == nil {
		return 0, errors.New("provider_health_history_repo: nil db")
	}
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM platform_llm_provider_health_history WHERE checked_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, fmt.Errorf("provider_health_history_repo: cleanup: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
