package varisk

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func mustMock(t *testing.T) (*Repo, sqlmock.Sqlmock, func()) {
	t.Helper()
	// Default regexp matcher: we assert against representative
	// fragments of the SQL rather than exact whitespace.
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewRepo(db), mock, func() { _ = db.Close() }
}

func TestRepo_DailyReturns_HappyPath(t *testing.T) {
	r, mock, done := mustMock(t)
	defer done()
	asOf := time.Date(2026, 1, 31, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"trading_date", "daily_return"}).
		AddRow(time.Date(2026, 1, 30, 0, 0, 0, 0, time.UTC), -0.002).
		AddRow(time.Date(2026, 1, 29, 0, 0, 0, 0, time.UTC), 0.001)
	mock.ExpectQuery(`SELECT trading_date, daily_return.*FROM nav_snapshots`).
		WithArgs("fund-1", asOf, 60).WillReturnRows(rows)

	got, err := r.DailyReturns(context.Background(), DailyReturnsParams{
		FundID:       "fund-1",
		LookbackDays: 60,
		AsOf:         asOf,
	})
	if err != nil {
		t.Fatalf("DailyReturns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	// Reversed to ascending chronological order.
	if !got[0].Date.Before(got[1].Date) {
		t.Errorf("expected chronological order, got %v then %v", got[0].Date, got[1].Date)
	}
	if got[0].Value != 0.001 || got[1].Value != -0.002 {
		t.Errorf("values out of order: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_DailyReturns_RejectsInvalidArgs(t *testing.T) {
	r, _, done := mustMock(t)
	defer done()
	if _, err := r.DailyReturns(context.Background(), DailyReturnsParams{FundID: "", LookbackDays: 30}); err == nil {
		t.Error("expected error for empty fund_id")
	}
	if _, err := r.DailyReturns(context.Background(), DailyReturnsParams{FundID: "fund-1", LookbackDays: 0}); err == nil {
		t.Error("expected error for zero lookback")
	}
}

func TestRepo_AppendSnapshot_WritesAllRowsInTx(t *testing.T) {
	r, mock, done := mustMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectPrepare(`INSERT INTO portfolio_var_snapshots`)
	gen := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	ws := time.Date(2025, 12, 1, 0, 0, 0, 0, time.UTC)
	we := time.Date(2026, 5, 31, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`INSERT INTO portfolio_var_snapshots`).
		WithArgs("fund-1", gen, "historical", 0.95, 1,
			-0.02, -0.025,
			ws, we, 252, 252,
			0.0005, 0.012,
			nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	seedVal := int64(20260601)
	pathsVal := 50000
	mock.ExpectExec(`INSERT INTO portfolio_var_snapshots`).
		WithArgs("fund-1", gen, "monte_carlo", 0.99, 1,
			-0.035, -0.04,
			ws, we, 252, 252,
			0.0005, 0.012,
			seedVal, pathsVal).
		WillReturnResult(sqlmock.NewResult(2, 1))
	mock.ExpectCommit()

	err := r.AppendSnapshot(context.Background(), Snapshot{
		FundID:       "fund-1",
		GeneratedAt:  gen,
		Horizon:      1,
		LookbackDays: 252,
		SampleSize:   252,
		Results: []Result{
			{
				Method: MethodHistorical, Confidence: Confidence95, Horizon: 1,
				Var: -0.02, CVar: -0.025, Mean: 0.0005, Std: 0.012,
				SampleSize:        252,
				SampleWindowStart: ws,
				SampleWindowEnd:   we,
			},
			{
				Method: MethodMonteCarlo, Confidence: Confidence99, Horizon: 1,
				Var: -0.035, CVar: -0.04, Mean: 0.0005, Std: 0.012,
				SampleSize:        252,
				SampleWindowStart: ws,
				SampleWindowEnd:   we,
				MonteCarloSeed:    &seedVal,
				MonteCarloPaths:   &pathsVal,
			},
		},
	})
	if err != nil {
		t.Fatalf("AppendSnapshot: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_AppendSnapshot_RollbackOnInsertError(t *testing.T) {
	r, mock, done := mustMock(t)
	defer done()
	mock.ExpectBegin()
	mock.ExpectPrepare(`INSERT INTO portfolio_var_snapshots`)
	mock.ExpectExec(`INSERT INTO portfolio_var_snapshots`).
		WillReturnError(errors.New("constraint violation"))
	mock.ExpectRollback()

	err := r.AppendSnapshot(context.Background(), Snapshot{
		FundID:      "fund-1",
		GeneratedAt: time.Now(),
		Results: []Result{
			{Method: MethodHistorical, Confidence: Confidence95, Horizon: 1, Var: -0.02, CVar: -0.025, SampleSize: 100},
		},
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("expectations: %v", err)
	}
}

func TestRepo_AppendSnapshot_SkipsEmptyResults(t *testing.T) {
	r, _, done := mustMock(t)
	defer done()
	if err := r.AppendSnapshot(context.Background(), Snapshot{FundID: "fund-1"}); err != nil {
		t.Fatalf("unexpected error for empty results: %v", err)
	}
}

func TestRepo_ListSnapshots_HappyPath(t *testing.T) {
	r, mock, done := mustMock(t)
	defer done()
	t0 := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 5, 30, 12, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"id", "fund_id", "calculated_at", "method", "confidence", "horizon_days",
		"var_pct", "cvar_pct", "sample_size", "lookback_days",
	}).
		AddRow(int64(2), "fund-1", t0, "historical", 0.95, 1, -0.021, -0.026, 252, 252).
		AddRow(int64(1), "fund-1", t1, "historical", 0.95, 1, -0.020, -0.025, 252, 252)
	mock.ExpectQuery(`SELECT id, fund_id, calculated_at, method, confidence, horizon_days.*FROM portfolio_var_snapshots`).
		WithArgs("fund-1", "historical", 0.95, 1, 90).WillReturnRows(rows)

	got, err := r.ListSnapshots(context.Background(), ListSnapshotsParams{
		FundID:      "fund-1",
		Method:      MethodHistorical,
		Confidence:  Confidence95,
		HorizonDays: 1,
	})
	if err != nil {
		t.Fatalf("ListSnapshots: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows, got %d", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 1 {
		t.Errorf("expected newest-first ordering, got %+v / %+v", got[0], got[1])
	}
}

func TestRepo_ListSnapshots_RejectsBadArgs(t *testing.T) {
	r, _, done := mustMock(t)
	defer done()
	ctx := context.Background()
	if _, err := r.ListSnapshots(ctx, ListSnapshotsParams{FundID: ""}); err == nil {
		t.Error("expected error for empty fund_id")
	}
	if _, err := r.ListSnapshots(ctx, ListSnapshotsParams{FundID: "f", Method: "bogus"}); err == nil {
		t.Error("expected error for bogus method")
	}
	if _, err := r.ListSnapshots(ctx, ListSnapshotsParams{FundID: "f", Method: MethodHistorical, Confidence: 0.5}); err == nil {
		t.Error("expected error for bogus confidence")
	}
	if _, err := r.ListSnapshots(ctx, ListSnapshotsParams{FundID: "f", Method: MethodHistorical, Confidence: Confidence95, HorizonDays: 0}); err == nil {
		t.Error("expected error for zero horizon")
	}
}
