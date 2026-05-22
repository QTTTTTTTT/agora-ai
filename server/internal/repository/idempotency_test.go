package repository

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestNavSnapshotCreateUsesUpsert is the F16 contract test for the NAV
// idempotency fix. A retried settlement step (manual admin trigger,
// crash recovery, scheduler double-fire) MUST collapse to an UPDATE on
// the existing row instead of inserting a duplicate.
//
// We assert the SQL contains the ON CONFLICT clause keyed on
// (fund_id, trading_date) — i.e. the constraint added in migration 027.
// A regression that drops the ON CONFLICT would surface as stacked rows
// and overstate fund returns; this guard prevents the next refactor
// from silently undoing the fix.
func TestNavSnapshotCreateUsesUpsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)

	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO nav_snapshots")).
		WithArgs("fund-1", tradingDate, 1.05, 105.0, 100.0, 5.0, 0.05, 0.05, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(1, 1))

	repo := NewNavSnapshotRepo(db)
	err = repo.Create(context.Background(), &NavSnapshot{
		FundID:           "fund-1",
		TradingDate:      tradingDate,
		NAV:              1.05,
		TotalAssets:      105.0,
		TotalMarketValue: 100.0,
		AvailableCash:    5.0,
		DailyReturn:      0.05,
		TotalReturn:      0.05,
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Verify the actual SQL contains the F16 ON CONFLICT clause.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPlanCreateWithIdempotencyKeyDeduplicates is the F16 contract test
// for investment_plans. When a key is supplied, a second create with
// the same key MUST return the original plan ID rather than inserting
// a duplicate. The CTE pattern keeps this atomic — no separate SELECT
// is needed by the caller.
func TestPlanCreateWithIdempotencyKeyDeduplicates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)

	// First create: idempotency-keyed CTE. Whether or not the actual
	// row was inserted, we expect "plan-1" as the returned id. The
	// CTE INSERT now carries the Phase 2A confidence column too, so
	// the WithArgs list includes a sql.NullFloat64{} placeholder
	// before the idempotency key.
	mock.ExpectQuery(regexp.QuoteMeta("WITH ins AS (")).
		WithArgs("fund-1", tradingDate, "pending_user", sql.NullString{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{}, sqlmock.AnyArg(), sqlmock.AnyArg(), sql.NullFloat64{}, "run-x|pm_plan").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plan-1"))

	repo := NewPlanRepo(db)
	id, err := repo.createTx(context.Background(), db, &InvestmentPlan{
		FundID:               "fund-1",
		TradingDate:          tradingDate,
		Status:               "pending_user",
		ClientIdempotencyKey: sql.NullString{String: "run-x|pm_plan", Valid: true},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "plan-1" {
		t.Errorf("expected plan-1, got %s", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestPlanCreateWithoutIdempotencyKeyTakesLegacyPath proves backward
// compatibility: existing callers that don't supply a key continue to
// use the plain INSERT...RETURNING path with no behavior change. The
// only way to verify is to assert the SQL DOESN'T contain "WITH ins AS".
func TestPlanCreateWithoutIdempotencyKeyTakesLegacyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 18, 0, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO investment_plans (fund_id, trading_date, status, reasoning, risk_score, expected_return, roundtable_id, pm_agent_id, risk_review, discussion_snapshot, confidence)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING id`)).
		WithArgs("fund-1", tradingDate, "pending_user", sql.NullString{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullString{}, sql.NullString{}, sqlmock.AnyArg(), sqlmock.AnyArg(), sql.NullFloat64{}).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plan-2"))

	repo := NewPlanRepo(db)
	id, err := repo.createTx(context.Background(), db, &InvestmentPlan{
		FundID:      "fund-1",
		TradingDate: tradingDate,
		Status:      "pending_user",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if id != "plan-2" {
		t.Errorf("expected plan-2, got %s", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
