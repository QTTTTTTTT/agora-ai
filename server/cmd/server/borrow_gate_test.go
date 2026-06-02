package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/securitiesborrow"
)

func newBorrowGateEnv(t *testing.T, cache *securitiesborrow.Cache) (*borrowGate, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	repo := securitiesborrow.NewRepo(db)
	if cache == nil {
		cache = securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	}
	g := newBorrowGate(db, repo, cache, newServerMetrics(), nil)
	return g, mock, func() { _ = db.Close() }
}

func TestBorrowGate_BuyShortCircuits(t *testing.T) {
	g, _, cleanup := newBorrowGateEnv(t, nil)
	defer cleanup()
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Side: "buy", Quantity: 1000,
	})
	if v.Rejected {
		t.Error("buy must never be rejected")
	}
	if g.metrics.(*serverMetrics).borrowEvents["check_allow_non_sell"] != 1 {
		t.Error("expected check_allow_non_sell metric")
	}
}

func TestBorrowGate_LongClose_NoBorrowNeeded(t *testing.T) {
	g, mock, cleanup := newBorrowGateEnv(t, nil)
	defer cleanup()
	// position = 1000 long; order = sell 500 → no short borrow needed.
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(1000)))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Side: "sell", Quantity: 500,
	})
	if v.Rejected {
		t.Errorf("expected allow, got %+v", v)
	}
}

func TestBorrowGate_PartialShort_BorrowsSurplus(t *testing.T) {
	// position = 100 long; order = sell 1000 → must borrow 900.
	// Cache has an "easy" rate, so allow with no warning.
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	cache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US", Symbol: "TSLA", Availability: securitiesborrow.AvailabilityEasy, BorrowRateBpsAnnual: 50},
	})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(100)))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Symbol: "TSLA",
		Side: "sell", Quantity: 1000, IntendedPrice: 200,
	})
	if v.Rejected {
		t.Errorf("expected allow, got %+v", v)
	}
}

func TestBorrowGate_FullShort_HardToBorrow(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	cache.SetRows([]securitiesborrow.BorrowRate{
		{
			InstrumentKey: "TSLA.US", Symbol: "TSLA",
			Availability:        securitiesborrow.AvailabilityHard,
			BorrowRateBpsAnnual: 3000, // 30%/yr
			LocateFeeBps:        25,
			AvailableShares:     ptrInt64Test(50000),
		},
	})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Symbol: "TSLA",
		Side: "sell", Quantity: 1000, IntendedPrice: 200,
	})
	if v.Rejected {
		t.Errorf("expected allow, got %+v", v)
	}
	if v.LocateFee <= 0 {
		t.Errorf("expected locate fee, got %v", v.LocateFee)
	}
	if len(v.Warnings) < 2 {
		t.Errorf("expected HTB warning + locate fee warning, got %v", v.Warnings)
	}
}

func TestBorrowGate_Unavailable_Reject(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	cache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "NOPE.US", Symbol: "NOPE", Availability: securitiesborrow.AvailabilityUnavailable},
	})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "NOPE.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "NOPE.US", Symbol: "NOPE",
		Side: "sell", Quantity: 100, IntendedPrice: 50,
	})
	if !v.Rejected {
		t.Fatalf("expected reject, got %+v", v)
	}
	if !contains(v.RejectReason, "unavailable") {
		t.Errorf("reason missing 'unavailable': %s", v.RejectReason)
	}
}

func TestBorrowGate_NoCalibration_AllowDefault(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Side: "sell", Quantity: 100,
	})
	if v.Rejected {
		t.Errorf("expected fail-open allow, got %+v", v)
	}
	if len(v.Warnings) == 0 {
		t.Errorf("expected no-calibration warning")
	}
}

func TestBorrowGate_NoCalibration_FailClosed(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	g.rejectOnNoCalibration = true
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(0)))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Side: "sell", Quantity: 100,
	})
	if !v.Rejected {
		t.Errorf("expected reject under fail-closed, got %+v", v)
	}
}

func TestBorrowGate_PositionLookupErr_FailOpen(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WillReturnError(errors.New("db down"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Side: "sell", Quantity: 100,
	})
	if v.Rejected {
		t.Errorf("expected fail-open on DB err, got %+v", v)
	}
	if len(v.Warnings) == 0 {
		t.Errorf("expected warning on fail-open")
	}
}

func TestBorrowGate_NoPositionRow_TreatsAsZero(t *testing.T) {
	cache := securitiesborrow.NewCache(securitiesborrow.CacheConfig{})
	cache.SetRows([]securitiesborrow.BorrowRate{
		{InstrumentKey: "TSLA.US", Availability: securitiesborrow.AvailabilityEasy, BorrowRateBpsAnnual: 50},
	})
	g, mock, cleanup := newBorrowGateEnv(t, cache)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "TSLA.US").
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{
		FundID: "f1", InstrumentKey: "TSLA.US", Symbol: "TSLA",
		Side: "sell", Quantity: 100, IntendedPrice: 50,
	})
	if v.Rejected {
		t.Errorf("expected allow with easy rate, got %+v", v)
	}
}

func TestBorrowGate_NilSafe(t *testing.T) {
	var g *borrowGate
	v := g.CheckOrder(context.Background(), broker.BorrowProbe{Side: "sell"})
	if v.Rejected {
		t.Error("nil gate must not reject")
	}
}

// ----- helpers -----

func ptrInt64Test(v int64) *int64 { return &v }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
