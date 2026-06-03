package repository

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newRollupMockRepo(t *testing.T) (*ProviderDailyRollupRepo, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewProviderDailyRollupRepo(db), mock
}

func TestProviderDailyRollupRepo_RecomputeWindow_HappyPath(t *testing.T) {
	repo, mock := newRollupMockRepo(t)
	from := time.Now().Add(-1 * time.Hour)
	to := time.Now()
	mock.ExpectExec(regexp.QuoteMeta("WITH affected AS")).
		WithArgs(from, to).
		WillReturnResult(sqlmock.NewResult(0, 5))
	n, err := repo.RecomputeWindow(context.Background(), from, to)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if n != 5 {
		t.Fatalf("expected 5 buckets, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderDailyRollupRepo_RecomputeWindow_RejectsInvertedWindow(t *testing.T) {
	repo, _ := newRollupMockRepo(t)
	to := time.Now().Add(-1 * time.Hour)
	from := time.Now()
	_, err := repo.RecomputeWindow(context.Background(), from, to)
	if err == nil {
		t.Fatalf("expected error on inverted window")
	}
}

func TestProviderDailyRollupRepo_ListByDayRange_AllProviders(t *testing.T) {
	repo, mock := newRollupMockRepo(t)
	fromDay := time.Now().Add(-7 * 24 * time.Hour)
	toDay := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE day >=")).
		WithArgs(fromDay, toDay).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider", "model_name", "day", "calls",
			"input_tokens", "output_tokens", "total_tokens",
			"cost_cents", "custom_key_calls", "last_rolled_at",
		}).
			AddRow("openai", "gpt-4o", toDay, int64(120),
				int64(50000), int64(15000), int64(65000),
				123.4567, int64(10), time.Now()).
			AddRow("claude", "claude-3-5", toDay, int64(60),
				int64(30000), int64(10000), int64(40000),
				90.1234, int64(0), time.Now()))
	rows, err := repo.ListByDayRange(context.Background(), "", fromDay, toDay)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("want 2 rows, got %d", len(rows))
	}
	if rows[0].CostCents <= 100 || rows[1].CostCents <= 50 {
		t.Fatalf("unexpected costs: %+v", rows)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderDailyRollupRepo_ListByDayRange_FilterByProvider(t *testing.T) {
	repo, mock := newRollupMockRepo(t)
	fromDay := time.Now().Add(-7 * 24 * time.Hour)
	toDay := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("WHERE provider = $1")).
		WithArgs("openai", fromDay, toDay).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider", "model_name", "day", "calls",
			"input_tokens", "output_tokens", "total_tokens",
			"cost_cents", "custom_key_calls", "last_rolled_at",
		}))
	rows, err := repo.ListByDayRange(context.Background(), "openai", fromDay, toDay)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("want 0 rows, got %d", len(rows))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestProviderDailyRollupRepo_ListByDayRange_RejectsInverted(t *testing.T) {
	repo, _ := newRollupMockRepo(t)
	fromDay := time.Now()
	toDay := time.Now().Add(-7 * 24 * time.Hour)
	_, err := repo.ListByDayRange(context.Background(), "", fromDay, toDay)
	if err == nil {
		t.Fatalf("expected error on inverted day range")
	}
}

func TestProviderDailyRollupRepo_SumByProvider(t *testing.T) {
	repo, mock := newRollupMockRepo(t)
	fromDay := time.Now().Add(-30 * 24 * time.Hour)
	toDay := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta("ORDER BY cost_cents DESC")).
		WithArgs(fromDay, toDay).
		WillReturnRows(sqlmock.NewRows([]string{
			"provider", "calls", "total_tokens", "cost_cents", "days_in_window",
		}).
			AddRow("openai", int64(3000), int64(1500000), 4567.89, 30).
			AddRow("claude", int64(2000), int64(1000000), 3210.50, 28))
	out, err := repo.SumByProvider(context.Background(), fromDay, toDay)
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("want 2 totals, got %d", len(out))
	}
	if out[0].Provider != "openai" || out[0].CostCents <= 4500 {
		t.Fatalf("unexpected top provider: %+v", out[0])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestNewProviderDailyRollupRepo_NilDB(t *testing.T) {
	if NewProviderDailyRollupRepo(nil) != nil {
		t.Fatalf("expected nil repo")
	}
}
