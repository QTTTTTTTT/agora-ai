package fx

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestAggregator_NilRepo_Identity(t *testing.T) {
	a := NewAggregator(nil)
	res, err := a.Aggregate(context.Background(), []ValueBucket{
		{Amount: 100, Currency: "USD"},
		{Amount: 50, Currency: ""},
	}, "USD", time.Time{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 150 {
		t.Errorf("total = %v", res.Total)
	}
	if res.Stale {
		t.Error("expected stale = false")
	}
}

func TestAggregator_NilRepo_StaleOnCrossCcy(t *testing.T) {
	a := NewAggregator(nil)
	res, err := a.Aggregate(context.Background(), []ValueBucket{
		{Amount: 100, Currency: "USD"},
		{Amount: 720, Currency: "CNY"},
	}, "USD", time.Time{})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !res.Stale {
		t.Error("expected stale = true")
	}
	if len(res.MissingPairs) != 1 || res.MissingPairs[0] != "CNY/USD" {
		t.Errorf("missing = %+v", res.MissingPairs)
	}
}

func TestAggregator_HappyPath(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	// CNY → USD: USD/CNY = 7.20 (direct)
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("CNY", "USD", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("CNY", "USD", 1.0/7.20, now, "yahoo"))

	a := NewAggregator(NewRepo(db))
	res, err := a.Aggregate(context.Background(), []ValueBucket{
		{Amount: 100, Currency: "USD"},
		{Amount: 720, Currency: "CNY"}, // → 100 USD
	}, "USD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Stale {
		t.Errorf("stale = true; missing = %+v", res.MissingPairs)
	}
	got := res.Total
	want := 100.0 + (720 * (1.0 / 7.20))
	if roundFloat(got, 4) != roundFloat(want, 4) {
		t.Errorf("total = %v want %v", got, want)
	}
}

func TestAggregator_PartialMiss_FlagStale(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	now := time.Now().UTC()
	// CNY → USD: hit
	mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).
		WithArgs("CNY", "USD", sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"base_currency", "quote_currency", "rate", "rate_at", "source",
		}).AddRow("CNY", "USD", 1.0/7.20, now, "yahoo"))
	// HKD → USD: miss in 5 lookups (direct + tri + inverse)
	for i := 0; i < 5; i++ {
		mock.ExpectQuery(regexp.QuoteMeta("FROM fx_rates")).WillReturnError(sql.ErrNoRows)
	}

	a := NewAggregator(NewRepo(db))
	res, err := a.Aggregate(context.Background(), []ValueBucket{
		{Amount: 720, Currency: "CNY"},
		{Amount: 780, Currency: "HKD"},
	}, "USD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !res.Stale {
		t.Error("expected stale = true")
	}
	if len(res.MissingPairs) != 1 || res.MissingPairs[0] != "HKD/USD" {
		t.Errorf("missing = %+v", res.MissingPairs)
	}
}

func TestAggregator_ZeroBucketsSkipped(t *testing.T) {
	a := NewAggregator(nil)
	res, err := a.Aggregate(context.Background(), []ValueBucket{
		{Amount: 0, Currency: "USD"},
	}, "USD", time.Now())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if res.Total != 0 {
		t.Errorf("total = %v", res.Total)
	}
}
