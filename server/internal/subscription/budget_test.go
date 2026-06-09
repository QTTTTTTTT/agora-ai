package subscription

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

func newBudgetMock(t *testing.T) (*BudgetService, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	return NewBudgetService(db), mock, func() { db.Close() }
}

// TestCheckNoBudgetRowIsAllowed proves the absence of a budget row =
// "no cap configured", not "block by default". A default-deny stance
// would break every existing tenant on rollout.
func TestCheckNoBudgetRowIsAllowed(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a").
		WillReturnError(sql.ErrNoRows)

	if err := svc.Check(context.Background(), "user-a", "fund-x"); err != nil {
		t.Fatalf("expected allow, got %v", err)
	}
}

// TestCheckDailyLimitBlocks is the core hard-gate test: spend at limit
// must return ErrLLMBudgetExceeded so the workflow can pause.
func TestCheckDailyLimitBlocks(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	limitRows := sqlmock.NewRows([]string{"user_id", "fund_id", "daily_limit_cents", "monthly_limit_cents", "created_at", "updated_at"}).
		AddRow("user-a", "fund-x", 100.0, nil, time.Now(), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnRows(limitRows)

	// Spend lookup for daily window — return at-limit so the gate trips.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(price_cents), 0)")).
		WithArgs("user-a", "fund-x", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(100.0))

	err := svc.Check(context.Background(), "user-a", "fund-x")
	if !IsLLMBudgetExceeded(err) {
		t.Fatalf("expected ErrLLMBudgetExceeded, got %v", err)
	}
}

// TestCheckMonthlyLimitBlocks confirms the monthly window is enforced
// independently of the daily window.
func TestCheckMonthlyLimitBlocks(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	limitRows := sqlmock.NewRows([]string{"user_id", "fund_id", "daily_limit_cents", "monthly_limit_cents", "created_at", "updated_at"}).
		AddRow("user-a", "fund-x", nil, 5000.0, time.Now(), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnRows(limitRows)

	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(SUM(price_cents), 0)")).
		WithArgs("user-a", "fund-x", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(5000.0))

	err := svc.Check(context.Background(), "user-a", "fund-x")
	if !IsLLMBudgetExceeded(err) {
		t.Fatalf("expected ErrLLMBudgetExceeded for monthly, got %v", err)
	}
}

// TestCheckUserWideFallbackApplies proves the fallback resolution
// works: no fund-specific row → consult user-wide cap → apply user-wide
// scope (so spend is summed across all funds).
func TestCheckUserWideFallbackApplies(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnError(sql.ErrNoRows)
	userWide := sqlmock.NewRows([]string{"user_id", "fund_id", "daily_limit_cents", "monthly_limit_cents", "created_at", "updated_at"}).
		AddRow("user-a", nil, 100.0, nil, time.Now(), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a").
		WillReturnRows(userWide)
	// User-wide scope: spend query has no fund_id filter.
	mock.ExpectQuery(regexp.QuoteMeta("WHERE user_id = $1 AND created_at >= $2 AND created_at < $3")).
		WithArgs("user-a", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(50.0))

	if err := svc.Check(context.Background(), "user-a", "fund-x"); err != nil {
		t.Fatalf("expected allow (50 < 100), got %v", err)
	}
}

// TestCheckEmptyUserIDIsAllowed is the safety valve: anonymous /
// internal calls without a user_id (admin LLM panels?) must not crash
// or block.
func TestCheckEmptyUserIDIsAllowed(t *testing.T) {
	svc, _, done := newBudgetMock(t)
	defer done()
	if err := svc.Check(context.Background(), "", "fund-x"); err != nil {
		t.Fatalf("expected nil for empty user_id, got %v", err)
	}
}

// TestUpsertBudgetRejectsAllNil proves we don't accidentally insert a
// useless row with no caps set.
func TestUpsertBudgetRejectsAllNil(t *testing.T) {
	svc, _, done := newBudgetMock(t)
	defer done()
	_, err := svc.UpsertBudget(context.Background(), "user-a", "", nil, nil)
	if err == nil {
		t.Fatal("expected error for all-nil limits")
	}
}

// TestUpsertBudgetRejectsNegative proves a typo-bug "-100" doesn't
// silently become a giant unsigned-style budget after the DB cast.
func TestUpsertBudgetRejectsNegative(t *testing.T) {
	svc, _, done := newBudgetMock(t)
	defer done()
	neg := -1.0
	_, err := svc.UpsertBudget(context.Background(), "user-a", "", &neg, nil)
	if err == nil {
		t.Fatal("expected error for negative daily limit")
	}
}

// TestUpsertBudgetUserScopeWritesNullFundID confirms the user-wide path
// emits the right INSERT (NULL fund_id, user-wide ON CONFLICT).
func TestUpsertBudgetUserScopeWritesNullFundID(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	daily := 250.0
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO llm_budgets")).
		WithArgs("user-a", daily, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	// GetBudget readback for the response body.
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a").
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "fund_id", "daily_limit_cents", "monthly_limit_cents", "created_at", "updated_at"}).
			AddRow("user-a", nil, daily, nil, time.Now(), time.Now()))

	row, err := svc.UpsertBudget(context.Background(), "user-a", "", &daily, nil)
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if row.UserID != "user-a" || row.FundID != nil {
		t.Errorf("expected user-wide row, got fund_id=%v", row.FundID)
	}
	if row.DailyLimitCents == nil || *row.DailyLimitCents != daily {
		t.Errorf("expected daily=%v, got %v", daily, row.DailyLimitCents)
	}
}

// TestSnapshotReturnsCurrentSpend exercises the read-only snapshot
// path (used by admin UI + workflow pause messages).
func TestSnapshotReturnsCurrentSpend(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	limitRows := sqlmock.NewRows([]string{"user_id", "fund_id", "daily_limit_cents", "monthly_limit_cents", "created_at", "updated_at"}).
		AddRow("user-a", "fund-x", 100.0, 5000.0, time.Now(), time.Now())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnRows(limitRows)
	mock.ExpectQuery(regexp.QuoteMeta("WHERE user_id = $1 AND fund_id = $2")).
		WithArgs("user-a", "fund-x", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(42.5))
	mock.ExpectQuery(regexp.QuoteMeta("WHERE user_id = $1 AND fund_id = $2")).
		WithArgs("user-a", "fund-x", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(1234.0))

	snap, err := svc.Snapshot(context.Background(), "user-a", "fund-x")
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Scope != "fund" {
		t.Errorf("expected scope=fund, got %s", snap.Scope)
	}
	if snap.DailySpendCents != 42.5 || snap.MonthlySpendCents != 1234.0 {
		t.Errorf("unexpected spend: %+v", snap)
	}
}

// TestCheckFallsThroughWhenFundIDNotUUID is the regression test for
// the advisor-mode breakage where the LLM client was stamping a
// string sentinel ("advisor") into ChatRequest.FundID. Postgres
// rejected the bind on llm_budgets.fund_id (UUID) with SQLSTATE 22P02
// and the resulting opaque DB error bricked every advisor consultation
// with "llm_error:budget service: get budget: pq: invalid input
// syntax for type uuid". GetBudget must now swallow 22P02 like a
// missing row so resolveLimit can fall through to the user-wide cap.
func TestCheckFallsThroughWhenFundIDNotUUID(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "advisor").
		WillReturnError(&pq.Error{Code: "22P02", Message: `invalid input syntax for type uuid: "advisor"`})
	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a").
		WillReturnError(sql.ErrNoRows)

	if err := svc.Check(context.Background(), "user-a", "advisor"); err != nil {
		t.Fatalf("expected fall-through to no-cap, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestCheckPropagatesNon22P02DBErrors confirms we only swallow the
// specific invalid-UUID case — every other DB-level failure must
// still surface so the LLM gate can fail closed.
func TestCheckPropagatesNon22P02DBErrors(t *testing.T) {
	svc, mock, done := newBudgetMock(t)
	defer done()

	mock.ExpectQuery(regexp.QuoteMeta("SELECT user_id, fund_id, daily_limit_cents")).
		WithArgs("user-a", "fund-x").
		WillReturnError(&pq.Error{Code: "53300", Message: "too many connections"})

	err := svc.Check(context.Background(), "user-a", "fund-x")
	if err == nil {
		t.Fatal("expected pool exhaustion to propagate, got nil")
	}
}

// TestIsLLMBudgetExceededOnWrappedError protects the public predicate
// from regressions caused by error wrapping in callers.
func TestIsLLMBudgetExceededOnWrappedError(t *testing.T) {
	wrapped := errors.New("downstream: " + ErrLLMBudgetExceeded.Error())
	if IsLLMBudgetExceeded(wrapped) {
		t.Fatal("plain string-formatted error must NOT match (no Unwrap chain)")
	}
	properly := ErrLLMBudgetExceeded
	if !IsLLMBudgetExceeded(properly) {
		t.Fatal("plain sentinel must match")
	}
}
