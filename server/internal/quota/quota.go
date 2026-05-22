// Package quota implements F28 per-fund resource quotas.
//
// Three quota types are enforced:
//
//   - ActiveAgents:         max simultaneously-active agents per fund
//   - ConcurrentWorkflows:  max workflow_runs in {pending, running, paused} per fund
//   - LLMTokens:            daily + monthly token budget per fund
//
// Quotas come from fund_quotas. A row with fund_id IS NULL is the
// platform default; per-fund rows override individual fields. NULL on a
// limit field means "no cap on this dimension" (a row with all NULLs
// effectively disables enforcement for that fund).
//
// All Check* methods return a typed sentinel (ErrQuotaExceeded) so
// callers can detect the quota path and surface a 429 or pause the
// workflow without parsing the message.
package quota

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrQuotaExceeded is the sentinel returned when a Check* call rejects
// a request. The wrapper carries the resource name and the offending
// limit so HTTP / workflow layers can render a useful error message.
var ErrQuotaExceeded = errors.New("quota exceeded")

// Resource enumerates the quota dimensions. Stringly-typed so it can
// be embedded in error messages and Prometheus labels without a switch.
type Resource string

const (
	ResourceActiveAgents        Resource = "active_agents"
	ResourceConcurrentWorkflows Resource = "concurrent_workflows"
	ResourceLLMTokensDaily      Resource = "llm_tokens_daily"
	ResourceLLMTokensMonthly    Resource = "llm_tokens_monthly"
)

// ExceededError is returned wrapping ErrQuotaExceeded. We attach the
// fund / resource / observed / limit so JSON responses and audit logs
// can be deterministic without parsing strings.
type ExceededError struct {
	FundID   string
	Resource Resource
	Observed int64
	Limit    int64
}

func (e *ExceededError) Error() string {
	return fmt.Sprintf("quota exceeded: fund=%s resource=%s observed=%d limit=%d", e.FundID, e.Resource, e.Observed, e.Limit)
}

func (e *ExceededError) Is(target error) bool {
	return target == ErrQuotaExceeded
}

// FundQuota describes the effective limits for a single fund after
// merging fund-specific overrides on top of the platform default.
type FundQuota struct {
	FundID                 string
	MaxActiveAgents        sql.NullInt64
	MaxConcurrentWorkflows sql.NullInt64
	DailyLLMTokenLimit     sql.NullInt64
	MonthlyLLMTokenLimit   sql.NullInt64
	Notes                  string
	UpdatedAt              time.Time
}

// Usage is the operational counter snapshot used in Check*.
type Usage struct {
	FundID              string
	Date                time.Time
	ActiveAgents        int64
	ConcurrentWorkflows int64
	DailyTokens         int64
	MonthlyTokens       int64
}

// Service is the gateway used by HTTP, workflow and LLM layers. It is
// safe for concurrent use; all state lives in postgres.
type Service struct {
	db  *sql.DB
	now func() time.Time
}

// NewService constructs a quota service. Pass nil db only in tests
// that exclusively exercise the helpers; production wiring must
// always provide a real *sql.DB.
func NewService(db *sql.DB) *Service {
	return &Service{db: db, now: time.Now}
}

// EffectiveQuota returns the merged platform-default + per-fund row.
// When neither exists a zero-value FundQuota is returned, which the
// Check* methods interpret as "unbounded".
func (s *Service) EffectiveQuota(ctx context.Context, fundID string) (*FundQuota, error) {
	if s == nil || s.db == nil {
		return &FundQuota{FundID: fundID}, nil
	}
	defaults, err := s.loadRow(ctx, "")
	if err != nil {
		return nil, err
	}
	override, err := s.loadRow(ctx, fundID)
	if err != nil {
		return nil, err
	}
	merged := &FundQuota{FundID: fundID, UpdatedAt: s.now()}
	if defaults != nil {
		merged.MaxActiveAgents = defaults.MaxActiveAgents
		merged.MaxConcurrentWorkflows = defaults.MaxConcurrentWorkflows
		merged.DailyLLMTokenLimit = defaults.DailyLLMTokenLimit
		merged.MonthlyLLMTokenLimit = defaults.MonthlyLLMTokenLimit
		merged.UpdatedAt = defaults.UpdatedAt
		merged.Notes = defaults.Notes
	}
	if override != nil {
		if override.MaxActiveAgents.Valid {
			merged.MaxActiveAgents = override.MaxActiveAgents
		}
		if override.MaxConcurrentWorkflows.Valid {
			merged.MaxConcurrentWorkflows = override.MaxConcurrentWorkflows
		}
		if override.DailyLLMTokenLimit.Valid {
			merged.DailyLLMTokenLimit = override.DailyLLMTokenLimit
		}
		if override.MonthlyLLMTokenLimit.Valid {
			merged.MonthlyLLMTokenLimit = override.MonthlyLLMTokenLimit
		}
		if override.UpdatedAt.After(merged.UpdatedAt) {
			merged.UpdatedAt = override.UpdatedAt
		}
		if strings.TrimSpace(override.Notes) != "" {
			merged.Notes = override.Notes
		}
	}
	return merged, nil
}

// CheckActiveAgents rejects when the proposed activation would push
// the live agent count above the configured cap. proposedDelta is
// usually 1 (creating one agent), but is parameterised so bulk
// activations from /api/agents/{id}/activate can be checked atomically.
func (s *Service) CheckActiveAgents(ctx context.Context, fundID string, proposedDelta int) error {
	if s == nil || s.db == nil || proposedDelta <= 0 {
		return nil
	}
	quota, err := s.EffectiveQuota(ctx, fundID)
	if err != nil {
		return err
	}
	if !quota.MaxActiveAgents.Valid {
		return nil
	}
	current, err := s.countActiveAgents(ctx, fundID)
	if err != nil {
		return err
	}
	if current+int64(proposedDelta) > quota.MaxActiveAgents.Int64 {
		return &ExceededError{FundID: fundID, Resource: ResourceActiveAgents, Observed: current, Limit: quota.MaxActiveAgents.Int64}
	}
	return nil
}

// CheckConcurrentWorkflows rejects when the proposed workflow start
// would exceed the cap. Schedulers and admin manual-triggers MUST call
// this before claiming a workflow_run row to avoid a "started then
// rejected" inconsistent state.
func (s *Service) CheckConcurrentWorkflows(ctx context.Context, fundID string) error {
	if s == nil || s.db == nil {
		return nil
	}
	quota, err := s.EffectiveQuota(ctx, fundID)
	if err != nil {
		return err
	}
	if !quota.MaxConcurrentWorkflows.Valid {
		return nil
	}
	current, err := s.countActiveWorkflows(ctx, fundID)
	if err != nil {
		return err
	}
	if current+1 > quota.MaxConcurrentWorkflows.Int64 {
		return &ExceededError{FundID: fundID, Resource: ResourceConcurrentWorkflows, Observed: current, Limit: quota.MaxConcurrentWorkflows.Int64}
	}
	return nil
}

// CheckLLMTokens validates that a planned LLM call would not exceed
// the daily or monthly token cap. We look at both windows in a single
// call so the LLM client only pays one round-trip per request.
//
// requestedTokens is an *upper-bound estimate* the caller projects
// based on prompt size + max_tokens. Conservative pessimism here is
// the right trade — better to gate a possibly-OK call than overshoot.
func (s *Service) CheckLLMTokens(ctx context.Context, fundID string, requestedTokens int64) error {
	if s == nil || s.db == nil || requestedTokens <= 0 || strings.TrimSpace(fundID) == "" {
		return nil
	}
	quota, err := s.EffectiveQuota(ctx, fundID)
	if err != nil {
		return err
	}
	if !quota.DailyLLMTokenLimit.Valid && !quota.MonthlyLLMTokenLimit.Valid {
		return nil
	}

	now := s.now().UTC()
	if quota.DailyLLMTokenLimit.Valid {
		daily, err := s.tokenUsageSum(ctx, fundID, startOfDay(now), now)
		if err != nil {
			return err
		}
		if daily+requestedTokens > quota.DailyLLMTokenLimit.Int64 {
			return &ExceededError{FundID: fundID, Resource: ResourceLLMTokensDaily, Observed: daily, Limit: quota.DailyLLMTokenLimit.Int64}
		}
	}
	if quota.MonthlyLLMTokenLimit.Valid {
		monthly, err := s.tokenUsageSum(ctx, fundID, startOfMonth(now), now)
		if err != nil {
			return err
		}
		if monthly+requestedTokens > quota.MonthlyLLMTokenLimit.Int64 {
			return &ExceededError{FundID: fundID, Resource: ResourceLLMTokensMonthly, Observed: monthly, Limit: quota.MonthlyLLMTokenLimit.Int64}
		}
	}
	return nil
}

// RecordLLMTokens upserts the day's token usage. Called by the LLM
// client after a successful provider response (cache hits do NOT
// consume budget — they're free re-issues of an already-paid call).
func (s *Service) RecordLLMTokens(ctx context.Context, fundID string, promptTokens, completionTokens int64) error {
	if s == nil || s.db == nil || strings.TrimSpace(fundID) == "" {
		return nil
	}
	if promptTokens < 0 || completionTokens < 0 {
		return fmt.Errorf("quota: negative token count")
	}
	today := s.now().UTC().Format("2006-01-02")
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO fund_llm_token_usage (fund_id, trading_date, prompt_tokens, completion_tokens, total_tokens, updated_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())
		 ON CONFLICT (fund_id, trading_date) DO UPDATE SET
		    prompt_tokens     = fund_llm_token_usage.prompt_tokens + EXCLUDED.prompt_tokens,
		    completion_tokens = fund_llm_token_usage.completion_tokens + EXCLUDED.completion_tokens,
		    total_tokens      = fund_llm_token_usage.total_tokens + EXCLUDED.total_tokens,
		    updated_at        = NOW()`,
		fundID, today, promptTokens, completionTokens, promptTokens+completionTokens,
	)
	if err != nil {
		return fmt.Errorf("quota: record token usage: %w", err)
	}
	return nil
}

// UpsertQuotaInput is the admin API surface for setting / clearing
// limits. A nil pointer for a field means "leave unchanged" on update,
// or "no cap" on initial insert. To clear an existing cap, callers
// should explicitly send 0 and the admin endpoint should translate
// 0 to sql.NullInt64{Valid:false}.
type UpsertQuotaInput struct {
	FundID                 string         // empty = platform default
	MaxActiveAgents        *int64
	MaxConcurrentWorkflows *int64
	DailyLLMTokenLimit     *int64
	MonthlyLLMTokenLimit   *int64
	Notes                  *string
}

// UpsertQuota installs or updates a per-fund / platform-default row.
func (s *Service) UpsertQuota(ctx context.Context, input UpsertQuotaInput) (*FundQuota, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("quota: service not configured")
	}
	args := []any{
		nullableUUID(input.FundID),
		nullableInt(input.MaxActiveAgents),
		nullableInt(input.MaxConcurrentWorkflows),
		nullableInt(input.DailyLLMTokenLimit),
		nullableInt(input.MonthlyLLMTokenLimit),
		nullableString(input.Notes),
	}
	if strings.TrimSpace(input.FundID) == "" {
		// Default row → upsert by partial unique index on (NULL fund_id).
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO fund_quotas (fund_id, max_active_agents, max_concurrent_workflows, daily_llm_token_limit, monthly_llm_token_limit, notes)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT ((TRUE)) WHERE fund_id IS NULL DO UPDATE SET
			    max_active_agents        = EXCLUDED.max_active_agents,
			    max_concurrent_workflows = EXCLUDED.max_concurrent_workflows,
			    daily_llm_token_limit    = EXCLUDED.daily_llm_token_limit,
			    monthly_llm_token_limit  = EXCLUDED.monthly_llm_token_limit,
			    notes                    = COALESCE(EXCLUDED.notes, fund_quotas.notes),
			    updated_at               = NOW()`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("quota: upsert default row: %w", err)
		}
	} else {
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO fund_quotas (fund_id, max_active_agents, max_concurrent_workflows, daily_llm_token_limit, monthly_llm_token_limit, notes)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (fund_id) WHERE fund_id IS NOT NULL DO UPDATE SET
			    max_active_agents        = EXCLUDED.max_active_agents,
			    max_concurrent_workflows = EXCLUDED.max_concurrent_workflows,
			    daily_llm_token_limit    = EXCLUDED.daily_llm_token_limit,
			    monthly_llm_token_limit  = EXCLUDED.monthly_llm_token_limit,
			    notes                    = COALESCE(EXCLUDED.notes, fund_quotas.notes),
			    updated_at               = NOW()`,
			args...,
		)
		if err != nil {
			return nil, fmt.Errorf("quota: upsert fund row: %w", err)
		}
	}
	return s.loadRow(ctx, input.FundID)
}

// DeleteQuota removes a fund-specific row, restoring platform-default behaviour.
func (s *Service) DeleteQuota(ctx context.Context, fundID string) error {
	if s == nil || s.db == nil || strings.TrimSpace(fundID) == "" {
		return errors.New("quota: fundID required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM fund_quotas WHERE fund_id=$1`, fundID)
	if err != nil {
		return fmt.Errorf("quota: delete fund row: %w", err)
	}
	return nil
}

// Snapshot returns the merged quota + current usage. Useful for the
// admin dashboard "how close are we to the cap?" view.
func (s *Service) Snapshot(ctx context.Context, fundID string) (*FundQuota, *Usage, error) {
	q, err := s.EffectiveQuota(ctx, fundID)
	if err != nil {
		return nil, nil, err
	}
	u := &Usage{FundID: fundID, Date: s.now().UTC()}
	if s.db != nil && strings.TrimSpace(fundID) != "" {
		if u.ActiveAgents, err = s.countActiveAgents(ctx, fundID); err != nil {
			return q, nil, err
		}
		if u.ConcurrentWorkflows, err = s.countActiveWorkflows(ctx, fundID); err != nil {
			return q, nil, err
		}
		now := u.Date
		if u.DailyTokens, err = s.tokenUsageSum(ctx, fundID, startOfDay(now), now); err != nil {
			return q, nil, err
		}
		if u.MonthlyTokens, err = s.tokenUsageSum(ctx, fundID, startOfMonth(now), now); err != nil {
			return q, nil, err
		}
	}
	return q, u, nil
}

// --- private helpers -------------------------------------------------------

func (s *Service) loadRow(ctx context.Context, fundID string) (*FundQuota, error) {
	var (
		row *sql.Row
		out FundQuota
	)
	if strings.TrimSpace(fundID) == "" {
		row = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(fund_id::text, ''), max_active_agents, max_concurrent_workflows, daily_llm_token_limit, monthly_llm_token_limit, COALESCE(notes,''), updated_at
			 FROM fund_quotas WHERE fund_id IS NULL`)
	} else {
		row = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(fund_id::text, ''), max_active_agents, max_concurrent_workflows, daily_llm_token_limit, monthly_llm_token_limit, COALESCE(notes,''), updated_at
			 FROM fund_quotas WHERE fund_id=$1`, fundID)
	}
	err := row.Scan(&out.FundID, &out.MaxActiveAgents, &out.MaxConcurrentWorkflows, &out.DailyLLMTokenLimit, &out.MonthlyLLMTokenLimit, &out.Notes, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("quota: load row: %w", err)
	}
	return &out, nil
}

func (s *Service) countActiveAgents(ctx context.Context, fundID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM agents
		 WHERE fund_id=$1 AND status IN ('active','running','idle')`,
		fundID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("quota: count active agents: %w", err)
	}
	return n, nil
}

func (s *Service) countActiveWorkflows(ctx context.Context, fundID string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM workflow_runs
		 WHERE fund_id=$1 AND status IN ('pending','running','paused')`,
		fundID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("quota: count active workflows: %w", err)
	}
	return n, nil
}

func (s *Service) tokenUsageSum(ctx context.Context, fundID string, from, to time.Time) (int64, error) {
	var n sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT SUM(total_tokens) FROM fund_llm_token_usage
		 WHERE fund_id=$1 AND trading_date >= $2 AND trading_date <= $3`,
		fundID, from.Format("2006-01-02"), to.Format("2006-01-02"),
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("quota: token usage sum: %w", err)
	}
	if !n.Valid {
		return 0, nil
	}
	return n.Int64, nil
}

func startOfDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func startOfMonth(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, t.Location())
}

func nullableInt(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func nullableString(v *string) sql.NullString {
	if v == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *v, Valid: true}
}

func nullableUUID(v string) sql.NullString {
	if strings.TrimSpace(v) == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}
