package main

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/securitiesborrow"
)

func newAccrualLoopEnv(t *testing.T, cache *securitiesborrow.Cache) (*borrowAccrualLoop, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := securitiesborrow.NewRepo(db)
	if cache == nil {
		cache = securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	}
	cashRepo := repository.NewCashLedgerRepo(db)
	l := newBorrowAccrualLoop(borrowAccrualConfig{
		DB:         db,
		BorrowRepo: repo,
		Cache:      cache,
		CashRepo:   cashRepo,
		Metrics:    newServerMetrics(),
		Interval:   1 * time.Minute,
		HourOfDay:  23,
		DayCount:   365,
	})
	return l, mock, func() { _ = db.Close() }
}

func TestAccrualLoop_OneShort_BooksFeeAndLedger(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	cache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US", BorrowRateBpsAnnual: 3000, Availability: securitiesborrow.AvailabilityHard},
	})
	l, mock, cleanup := newAccrualLoopEnv(t, cache)
	defer cleanup()

	asOf := time.Date(2026, 6, 1, 23, 30, 0, 0, time.UTC)
	// Scan returns one short.
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "instrument_key", "symbol", "quantity", "current_price",
		}).AddRow("f1", "TSLA.US", "TSLA", float64(-1000), float64(200)))
	// Expect a cash_ledger Append (regex against the actual INSERT
	// the cash_ledger_repo emits — uses idempotency_key).
	mock.ExpectQuery("INSERT INTO cash_ledger").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("cle-1"))
	// Expect the borrow ledger upsert.
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO short_position_borrow_ledger")).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol",
			"accrual_date", "short_qty", "market_price", "notional",
			"rate_bps_annual", "day_count_basis", "fee_amount",
			"cash_ledger_entry_id", "created_at",
		}).AddRow(
			"led-1", "f1", "TSLA.US", "TSLA",
			time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
			float64(1000), float64(200), float64(200000),
			float64(3000), 365, float64(164.38),
			"cle-1", asOf,
		))
	if err := l.AccrueOnce(context.Background(), asOf); err != nil {
		t.Fatalf("err = %v", err)
	}
	m := l.metrics.(*serverMetrics)
	if m.borrowEvents["accrual_booked"] != 1 {
		t.Errorf("expected accrual_booked, got %+v", m.borrowEvents)
	}
}

func TestAccrualLoop_NoShortRow_NoOp(t *testing.T) {
	l, mock, cleanup := newAccrualLoopEnv(t, nil)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "instrument_key", "symbol", "quantity", "current_price",
		})) // empty result
	if err := l.AccrueOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestAccrualLoop_NoCalibration_Skipped(t *testing.T) {
	l, mock, cleanup := newAccrualLoopEnv(t, nil)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnRows(sqlmock.NewRows([]string{
			"fund_id", "instrument_key", "symbol", "quantity", "current_price",
		}).AddRow("f1", "TSLA.US", "TSLA", float64(-100), float64(200)))
	if err := l.AccrueOnce(context.Background(), time.Now()); err != nil {
		t.Fatalf("err = %v", err)
	}
	m := l.metrics.(*serverMetrics)
	if m.borrowEvents["accrual_skipped_zero borrow cost"] != 1 {
		t.Errorf("expected zero-cost skip metric, got %+v", m.borrowEvents)
	}
}

func TestAccrualLoop_ScanErr_Returns(t *testing.T) {
	l, mock, cleanup := newAccrualLoopEnv(t, nil)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnError(errors.New("db down"))
	err := l.AccrueOnce(context.Background(), time.Now())
	if err == nil {
		t.Error("expected scan err")
	}
	m := l.metrics.(*serverMetrics)
	if m.borrowEvents["scan_failed"] != 1 {
		t.Error("expected scan_failed metric")
	}
}

func TestAccrualLoop_NotLeader_NoOp(t *testing.T) {
	// notLeader struct returns false; tickOnce must skip the
	// whole pipeline (no DB calls expected → mock not strict).
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	l := newBorrowAccrualLoop(borrowAccrualConfig{
		DB:         db,
		BorrowRepo: securitiesborrow.NewRepo(db),
		Metrics:    newServerMetrics(),
		Leader:     notLeader{},
		Interval:   1 * time.Minute,
		HourOfDay:  23,
	})
	l.tickOnce(context.Background()) // must not call DB
}

func TestAccrualLoop_LastRunIdempotency(t *testing.T) {
	l, _, cleanup := newAccrualLoopEnv(t, nil)
	defer cleanup()
	now := time.Date(2026, 6, 1, 23, 30, 0, 0, time.UTC)
	l.lastRun = now
	l.now = func() time.Time { return now.Add(10 * time.Minute) } // still same day, after hour
	// tickOnce should skip (already ran today). No DB expectations set.
	l.tickOnce(context.Background())
}

type notLeader struct{}

func (notLeader) IsLeader() bool { return false }
