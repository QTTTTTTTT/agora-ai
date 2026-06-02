package securitiesborrow

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newMockedRepo(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

// ----- rate -----

func rateRowsCols() []string {
	return []string{
		"id", "instrument_key", "symbol", "market", "asset_class",
		"borrow_rate_bps_annual", "locate_fee_bps",
		"availability", "available_shares", "min_locate_qty", "max_locate_qty",
		"source", "last_calibrated_at", "note", "updated_by",
		"created_at", "updated_at",
	}
}

func TestRepo_GetRateByKey_NotFoundReturnsNil(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery("FROM security_borrow_rates").
		WithArgs("MISSING").
		WillReturnError(sql.ErrNoRows)
	got, err := repo.GetRateByKey(context.Background(), "MISSING")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("expected nil")
	}
}

func TestRepo_UpsertRate_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_borrow_rates")).
		WillReturnRows(sqlmock.NewRows(rateRowsCols()).AddRow(
			"id1", "TSLA.US", "TSLA", "US", "equity",
			float64(3000), float64(25),
			"hard", int64(50000), int64(100), int64(100000),
			"manual", now, "", nil,
			now, now,
		))
	rate := 3000.0
	fee := 25.0
	r, err := repo.UpsertRate(context.Background(), UpsertRateParams{
		InstrumentKey:       "TSLA.US",
		Symbol:              "TSLA",
		Market:              "US",
		AssetClass:          "equity",
		BorrowRateBpsAnnual: &rate,
		LocateFeeBps:        &fee,
		Availability:        "hard",
		AvailableShares:     ptrInt64(50000),
		MinLocateQty:        ptrInt64(100),
		MaxLocateQty:        ptrInt64(100000),
		Source:              "manual",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if r == nil || r.Availability != AvailabilityHard {
		t.Errorf("got %+v", r)
	}
	if r.AvailableShares == nil || *r.AvailableShares != 50000 {
		t.Errorf("avail = %v", r.AvailableShares)
	}
}

func TestRepo_UpsertRate_Validates(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	cases := []struct {
		name string
		p    UpsertRateParams
	}{
		{"missing key", UpsertRateParams{Symbol: "X"}},
		{"missing symbol", UpsertRateParams{InstrumentKey: "X"}},
		{"negative rate", UpsertRateParams{InstrumentKey: "X", Symbol: "X", BorrowRateBpsAnnual: ptrFloat64(-1)}},
		{"bad availability", UpsertRateParams{InstrumentKey: "X", Symbol: "X", Availability: "wat"}},
		{"bad source", UpsertRateParams{InstrumentKey: "X", Symbol: "X", Source: "wat"}},
	}
	for _, c := range cases {
		if _, err := repo.UpsertRate(context.Background(), c.p); err == nil {
			t.Errorf("%s: expected validation err", c.name)
		}
	}
}

func ptrFloat64(f float64) *float64 { return &f }

func TestRepo_DeleteRate_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("DELETE FROM security_borrow_rates")).
		WithArgs("missing").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.DeleteRate(context.Background(), "missing"); err == nil {
		t.Error("expected sql.ErrNoRows")
	}
}

// ----- locate audit -----

func TestRepo_LogLocateEvent_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO security_locate_events")).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("evt-1"))
	id, err := repo.LogLocateEvent(context.Background(), LogLocateEventParams{
		FundID: "f1", InstrumentKey: "TSLA.US", Symbol: "TSLA",
		RequestedQty: 1000, Decision: LocateAllow, Reason: "ok",
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if id != "evt-1" {
		t.Errorf("id = %s", id)
	}
}

func TestRepo_LogLocateEvent_Requires(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.LogLocateEvent(context.Background(), LogLocateEventParams{}); err == nil {
		t.Error("expected fund/instrument required err")
	}
	if _, err := repo.LogLocateEvent(context.Background(), LogLocateEventParams{
		FundID: "f", InstrumentKey: "X",
	}); err == nil {
		t.Error("expected decision required err")
	}
}

func TestRepo_ListLocateEvents_FilterByFund(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	mock.ExpectQuery("FROM security_locate_events").
		WithArgs("f1", 200, 0).
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol",
			"requested_qty", "decision",
			"rate_bps_annual", "locate_fee_bps", "locate_fee_amount",
			"intended_price", "notional",
			"reason", "client_order_id", "created_at",
		}).AddRow(
			"evt-1", "f1", "TSLA.US", "TSLA",
			float64(1000), "allow",
			float64(3000), float64(25), float64(250),
			float64(100), float64(100000),
			"ok", "co-1", now,
		))
	got, err := repo.ListLocateEvents(context.Background(), ListLocateFilter{FundID: "f1"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 1 || got[0].Decision != LocateAllow {
		t.Errorf("got %+v", got)
	}
}

// ----- ledger -----

func ledgerCols() []string {
	return []string{
		"id", "fund_id", "instrument_key", "symbol",
		"accrual_date", "short_qty", "market_price", "notional",
		"rate_bps_annual", "day_count_basis", "fee_amount",
		"cash_ledger_entry_id", "created_at",
	}
}

func TestRepo_UpsertLedgerEntry_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Now().UTC()
	day := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO short_position_borrow_ledger")).
		WillReturnRows(sqlmock.NewRows(ledgerCols()).AddRow(
			"led-1", "f1", "TSLA.US", "TSLA",
			day, float64(1000), float64(200), float64(200000),
			float64(3000), 365, float64(164.38),
			nil, now,
		))
	got, err := repo.UpsertLedgerEntry(context.Background(), UpsertLedgerParams{
		FundID: "f1", InstrumentKey: "TSLA.US", Symbol: "TSLA",
		AccrualDate: day,
		ShortQty:    1000, MarketPrice: 200, Notional: 200000,
		RateBpsAnnual: 3000, DayCountBasis: 365, FeeAmount: 164.38,
	})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.FeeAmount < 164 {
		t.Errorf("got %+v", got)
	}
}

func TestRepo_UpsertLedger_Validates(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if _, err := repo.UpsertLedgerEntry(context.Background(), UpsertLedgerParams{}); err == nil {
		t.Error("expected fund/instrument required err")
	}
	if _, err := repo.UpsertLedgerEntry(context.Background(), UpsertLedgerParams{
		FundID: "f", InstrumentKey: "X", ShortQty: 0, FeeAmount: 1,
	}); err == nil {
		t.Error("expected qty > 0 err")
	}
	if _, err := repo.UpsertLedgerEntry(context.Background(), UpsertLedgerParams{
		FundID: "f", InstrumentKey: "X", ShortQty: 1, FeeAmount: -1,
	}); err == nil {
		t.Error("expected fee >= 0 err")
	}
}
