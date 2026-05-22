package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrLLMBudgetExceeded is returned by BudgetService.Check when the user
// has spent at or above their configured daily / monthly cents cap.
// llm.MultiProviderClient wraps this as a permanent error so the
// workflow retry policy short-circuits and the orchestrator can pause
// the run with an explicit "out of budget" reason.
var ErrLLMBudgetExceeded = errors.New("subscription: llm dollar budget exceeded")

// BudgetLimit is the resolved cap applied to a Check() call. Helpful
// for the admin API + workflow error messages so the operator can see
// exactly which window tripped the gate.
type BudgetLimit struct {
	// Scope describes where the limit came from. Always set.
	//   "fund"   — explicit (user_id, fund_id) row matched
	//   "user"   — fell back to the user-wide (fund_id IS NULL) row
	//   "none"   — no row found; Check returns nil
	Scope string

	// Daily / Monthly are the limit values applied (cents). Zero means
	// "no cap on that window". DailySpend / MonthlySpend hold the
	// rolling spend at the moment of the check.
	DailyLimitCents   float64
	MonthlyLimitCents float64
	DailySpendCents   float64
	MonthlySpendCents float64
}

// BudgetRow mirrors a row in llm_budgets. Pointer fields express
// "unset" so the admin UI can distinguish "no cap" from "cap = 0".
type BudgetRow struct {
	UserID            string     `json:"user_id"`
	FundID            *string    `json:"fund_id,omitempty"`
	DailyLimitCents   *float64   `json:"daily_limit_cents,omitempty"`
	MonthlyLimitCents *float64   `json:"monthly_limit_cents,omitempty"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// BudgetService implements the F14 dollar-budget hard gate.
//
// Design constraints:
//   - Check() is on every LLM call hot path → MUST be fast. We do at most
//     one limits-table SELECT + one usage SUM per call. A future
//     optimization can cache the limit row with TTL (the spend lookup
//     is already a single indexed scan).
//   - Spend uses price_cents (what the user owes), not cost_cents (what
//     the platform pays the provider). The user-facing budget should
//     be denominated in what the user is billed.
//   - The window boundary is calendar day / calendar month in the
//     server's timezone. Acceptable for v1; a per-user timezone column
//     can be added later if a billing dispute warrants it.
type BudgetService struct {
	db *sql.DB
}

func NewBudgetService(db *sql.DB) *BudgetService {
	return &BudgetService{db: db}
}

// Check returns nil if the (user, fund) combo is allowed to make another
// LLM call. Returns ErrLLMBudgetExceeded wrapped with a window descriptor
// when either the daily or monthly cap is reached.
//
// fundID may be empty when the call is not associated with a specific
// fund (e.g., user-driven admin chat); in that case only the user-wide
// row is consulted.
func (s *BudgetService) Check(ctx context.Context, userID, fundID string) error {
	limit, err := s.resolveLimit(ctx, userID, fundID)
	if err != nil {
		return err
	}
	if limit == nil || limit.Scope == "none" {
		return nil
	}

	// Daily window first (more likely to trip on a bursty workflow).
	if limit.DailyLimitCents > 0 {
		dailySpend, err := s.getSpend(ctx, userID, fundID, limit.Scope, dailyWindow(time.Now().UTC()))
		if err != nil {
			return fmt.Errorf("budget service: query daily spend: %w", err)
		}
		limit.DailySpendCents = dailySpend
		if dailySpend >= limit.DailyLimitCents {
			return fmt.Errorf("%w: scope=%s window=daily spent=%.2f limit=%.2f",
				ErrLLMBudgetExceeded, limit.Scope, dailySpend, limit.DailyLimitCents)
		}
	}

	if limit.MonthlyLimitCents > 0 {
		monthlySpend, err := s.getSpend(ctx, userID, fundID, limit.Scope, monthlyWindow(time.Now().UTC()))
		if err != nil {
			return fmt.Errorf("budget service: query monthly spend: %w", err)
		}
		limit.MonthlySpendCents = monthlySpend
		if monthlySpend >= limit.MonthlyLimitCents {
			return fmt.Errorf("%w: scope=%s window=monthly spent=%.2f limit=%.2f",
				ErrLLMBudgetExceeded, limit.Scope, monthlySpend, limit.MonthlyLimitCents)
		}
	}
	return nil
}

// Snapshot returns a BudgetLimit describing the current state for a
// (user, fund) without enforcing the cap. Useful for admin dashboards
// and for the workflow pause UI ("you've used 92¢ of your $1.00 daily
// budget on fund X — bump or wait until reset").
func (s *BudgetService) Snapshot(ctx context.Context, userID, fundID string) (*BudgetLimit, error) {
	limit, err := s.resolveLimit(ctx, userID, fundID)
	if err != nil {
		return nil, err
	}
	if limit == nil {
		return &BudgetLimit{Scope: "none"}, nil
	}
	if limit.DailyLimitCents > 0 {
		spend, err := s.getSpend(ctx, userID, fundID, limit.Scope, dailyWindow(time.Now().UTC()))
		if err != nil {
			return nil, err
		}
		limit.DailySpendCents = spend
	}
	if limit.MonthlyLimitCents > 0 {
		spend, err := s.getSpend(ctx, userID, fundID, limit.Scope, monthlyWindow(time.Now().UTC()))
		if err != nil {
			return nil, err
		}
		limit.MonthlySpendCents = spend
	}
	return limit, nil
}

// GetBudget returns the raw row for (user, fund) — fund may be empty
// to fetch the user-wide row. Returns nil + nil error when no row.
func (s *BudgetService) GetBudget(ctx context.Context, userID, fundID string) (*BudgetRow, error) {
	row := &BudgetRow{}
	var daily, monthly sql.NullFloat64
	var fund sql.NullString
	var query string
	var args []any
	userID = strings.TrimSpace(userID)
	fundID = strings.TrimSpace(fundID)
	if fundID == "" {
		query = `SELECT user_id, fund_id, daily_limit_cents, monthly_limit_cents, created_at, updated_at
		         FROM llm_budgets WHERE user_id = $1 AND fund_id IS NULL`
		args = []any{userID}
	} else {
		query = `SELECT user_id, fund_id, daily_limit_cents, monthly_limit_cents, created_at, updated_at
		         FROM llm_budgets WHERE user_id = $1 AND fund_id = $2`
		args = []any{userID, fundID}
	}
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&row.UserID, &fund, &daily, &monthly, &row.CreatedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("budget service: get budget: %w", err)
	}
	if fund.Valid {
		v := fund.String
		row.FundID = &v
	}
	if daily.Valid {
		v := daily.Float64
		row.DailyLimitCents = &v
	}
	if monthly.Valid {
		v := monthly.Float64
		row.MonthlyLimitCents = &v
	}
	return row, nil
}

// ListByUser returns all budget rows for a user (both user-wide and
// per-fund). Sorted with the user-wide row first, then funds by ID for
// stable admin UI output.
func (s *BudgetService) ListByUser(ctx context.Context, userID string) ([]BudgetRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT user_id, fund_id, daily_limit_cents, monthly_limit_cents, created_at, updated_at
		 FROM llm_budgets WHERE user_id = $1
		 ORDER BY (fund_id IS NOT NULL), fund_id`, userID)
	if err != nil {
		return nil, fmt.Errorf("budget service: list by user: %w", err)
	}
	defer rows.Close()
	out := make([]BudgetRow, 0, 4)
	for rows.Next() {
		var r BudgetRow
		var daily, monthly sql.NullFloat64
		var fund sql.NullString
		if err := rows.Scan(&r.UserID, &fund, &daily, &monthly, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("budget service: scan: %w", err)
		}
		if fund.Valid {
			v := fund.String
			r.FundID = &v
		}
		if daily.Valid {
			v := daily.Float64
			r.DailyLimitCents = &v
		}
		if monthly.Valid {
			v := monthly.Float64
			r.MonthlyLimitCents = &v
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpsertBudget creates or updates a budget row. Pass nil for either
// limit to leave that window uncapped. Validates at least one limit is
// set so the DB constraint catches double-NULL early in the API path.
func (s *BudgetService) UpsertBudget(ctx context.Context, userID, fundID string, dailyCents, monthlyCents *float64) (*BudgetRow, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, fmt.Errorf("budget service: user_id required")
	}
	if dailyCents == nil && monthlyCents == nil {
		return nil, fmt.Errorf("budget service: at least one of daily / monthly must be set")
	}
	if dailyCents != nil && *dailyCents < 0 {
		return nil, fmt.Errorf("budget service: daily limit must be non-negative")
	}
	if monthlyCents != nil && *monthlyCents < 0 {
		return nil, fmt.Errorf("budget service: monthly limit must be non-negative")
	}

	var fundArg any
	fundID = strings.TrimSpace(fundID)
	if fundID != "" {
		fundArg = fundID
	}

	// Two upserts because the unique index splits on fund_id IS NULL vs.
	// NOT NULL. Using "ON CONFLICT (user_id, fund_id)" doesn't work for
	// the NULL case in vanilla Postgres without the WHERE clause.
	var query string
	if fundArg == nil {
		query = `
			INSERT INTO llm_budgets (user_id, fund_id, daily_limit_cents, monthly_limit_cents, updated_at)
			VALUES ($1, NULL, $2, $3, NOW())
			ON CONFLICT (user_id) WHERE fund_id IS NULL DO UPDATE
			SET daily_limit_cents = EXCLUDED.daily_limit_cents,
			    monthly_limit_cents = EXCLUDED.monthly_limit_cents,
			    updated_at = NOW()
		`
	} else {
		query = `
			INSERT INTO llm_budgets (user_id, fund_id, daily_limit_cents, monthly_limit_cents, updated_at)
			VALUES ($1, $2, $3, $4, NOW())
			ON CONFLICT (user_id, fund_id) WHERE fund_id IS NOT NULL DO UPDATE
			SET daily_limit_cents = EXCLUDED.daily_limit_cents,
			    monthly_limit_cents = EXCLUDED.monthly_limit_cents,
			    updated_at = NOW()
		`
	}

	var execErr error
	if fundArg == nil {
		_, execErr = s.db.ExecContext(ctx, query, userID, nullableFloat(dailyCents), nullableFloat(monthlyCents))
	} else {
		_, execErr = s.db.ExecContext(ctx, query, userID, fundArg, nullableFloat(dailyCents), nullableFloat(monthlyCents))
	}
	if execErr != nil {
		return nil, fmt.Errorf("budget service: upsert: %w", execErr)
	}
	return s.GetBudget(ctx, userID, fundID)
}

// DeleteBudget removes a budget row. No-op when no row exists.
func (s *BudgetService) DeleteBudget(ctx context.Context, userID, fundID string) error {
	fundID = strings.TrimSpace(fundID)
	var (
		err error
	)
	if fundID == "" {
		_, err = s.db.ExecContext(ctx, `DELETE FROM llm_budgets WHERE user_id = $1 AND fund_id IS NULL`, userID)
	} else {
		_, err = s.db.ExecContext(ctx, `DELETE FROM llm_budgets WHERE user_id = $1 AND fund_id = $2`, userID, fundID)
	}
	if err != nil {
		return fmt.Errorf("budget service: delete: %w", err)
	}
	return nil
}

// IsLLMBudgetExceeded is the canonical predicate for callers wanting to
// special-case the budget error (e.g., workflow pause logic).
func IsLLMBudgetExceeded(err error) bool {
	return errors.Is(err, ErrLLMBudgetExceeded)
}

// ---- internals --------------------------------------------------------

type spendWindow struct {
	from time.Time
	to   time.Time
}

func dailyWindow(now time.Time) spendWindow {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return spendWindow{from: day, to: day.Add(24 * time.Hour)}
}

func monthlyWindow(now time.Time) spendWindow {
	month := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	return spendWindow{from: month, to: month.AddDate(0, 1, 0)}
}

// resolveLimit looks up the most-specific budget row for (user, fund).
// Returns Scope="none" when no row applies — caller treats as no cap.
func (s *BudgetService) resolveLimit(ctx context.Context, userID, fundID string) (*BudgetLimit, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, nil
	}
	fundID = strings.TrimSpace(fundID)

	if fundID != "" {
		row, err := s.GetBudget(ctx, userID, fundID)
		if err != nil {
			return nil, err
		}
		if row != nil {
			return budgetRowToLimit("fund", row), nil
		}
	}
	row, err := s.GetBudget(ctx, userID, "")
	if err != nil {
		return nil, err
	}
	if row != nil {
		return budgetRowToLimit("user", row), nil
	}
	return &BudgetLimit{Scope: "none"}, nil
}

func budgetRowToLimit(scope string, row *BudgetRow) *BudgetLimit {
	limit := &BudgetLimit{Scope: scope}
	if row.DailyLimitCents != nil {
		limit.DailyLimitCents = *row.DailyLimitCents
	}
	if row.MonthlyLimitCents != nil {
		limit.MonthlyLimitCents = *row.MonthlyLimitCents
	}
	return limit
}

// getSpend sums price_cents over the window. The scope chooses between
// "all entries for this user" (user scope) vs. "entries for this fund"
// (fund scope) so a per-fund budget caps the fund alone, not the user.
func (s *BudgetService) getSpend(ctx context.Context, userID, fundID, scope string, window spendWindow) (float64, error) {
	var (
		spend sql.NullFloat64
		err   error
	)
	if scope == "fund" && fundID != "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(price_cents), 0)
			 FROM usage_entries
			 WHERE user_id = $1 AND fund_id = $2 AND created_at >= $3 AND created_at < $4`,
			userID, fundID, window.from, window.to,
		).Scan(&spend)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(price_cents), 0)
			 FROM usage_entries
			 WHERE user_id = $1 AND created_at >= $2 AND created_at < $3`,
			userID, window.from, window.to,
		).Scan(&spend)
	}
	if err != nil {
		return 0, err
	}
	if !spend.Valid {
		return 0, nil
	}
	return spend.Float64, nil
}

func nullableFloat(v *float64) any {
	if v == nil {
		return nil
	}
	return *v
}
