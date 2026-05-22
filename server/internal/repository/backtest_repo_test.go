package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newBacktestRepoTest(t *testing.T) (*BacktestRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	return NewBacktestRepo(db), mock, func() { db.Close() }
}

func TestBacktestRepoInsertQueued(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()

	row := &BacktestJobRow{
		ID:          "00000000-0000-0000-0000-000000000001",
		FundID:      "fund-1",
		UserID:      "user-1",
		Name:        "trial",
		EngineKind:  "fallback",
		Status:      "queued",
		Request:     json.RawMessage(`{"symbols":["AAPL"]}`),
		WindowStart: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC),
		WindowEnd:   time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
		SubmittedAt: time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC),
		TotalDays:   100,
		DoneDays:    0,
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_jobs`)).
		WithArgs(row.ID, row.FundID, row.UserID, row.Name, row.EngineKind, row.Status, sqlmock.AnyArg(),
			row.WindowStart, row.WindowEnd, row.SubmittedAt, row.TotalDays, row.DoneDays,
			row.SweepID, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.InsertQueued(context.Background(), row); err != nil {
		t.Fatalf("InsertQueued: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBacktestRepoInsertQueuedRejectsEmptyID(t *testing.T) {
	repo, _, cleanup := newBacktestRepoTest(t)
	defer cleanup()
	if err := repo.InsertQueued(context.Background(), &BacktestJobRow{FundID: "fund-1"}); err == nil {
		t.Errorf("expected error on empty id")
	}
}

func TestBacktestRepoUpdateFinalWithNavAndTrades(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()

	row := &BacktestJobRow{
		ID:               "job-1",
		Status:           "completed",
		Error:            sql.NullString{},
		StartedAt:        sql.NullTime{Time: time.Date(2026, 5, 20, 12, 1, 0, 0, time.UTC), Valid: true},
		CompletedAt:      sql.NullTime{Time: time.Date(2026, 5, 20, 12, 2, 0, 0, time.UTC), Valid: true},
		InitialCash:      sql.NullFloat64{Float64: 100000, Valid: true},
		FinalNav:         sql.NullFloat64{Float64: 110000, Valid: true},
		CumulativeReturn: sql.NullFloat64{Float64: 0.10, Valid: true},
		TradeCount:       2,
		TotalDays:        10,
		DoneDays:         10,
	}
	nav := []BacktestNavPoint{
		{Seq: 0, Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Nav: 100000, Cash: 100000, Positions: json.RawMessage(`{}`)},
		{Seq: 1, Date: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC), Nav: 105000, Cash: 90000, PositionValue: 15000, Positions: json.RawMessage(`{"AAPL":100}`)},
	}
	trades := []BacktestTradeEvent{
		{Seq: 0, Date: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), Symbol: "AAPL", Action: "buy", Status: "filled", Quantity: 100, FillPrice: 150, Notional: 15000},
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs SET`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_nav_points WHERE job_id`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_trade_events WHERE job_id`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, int64(len(nav))))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, int64(len(trades))))
	mock.ExpectCommit()

	if err := repo.UpdateFinal(context.Background(), row, nav, trades); err != nil {
		t.Fatalf("UpdateFinal: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBacktestRepoUpdateFinalEmptyChildrenSkipsInserts(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs SET`)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_nav_points`)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM backtest_trade_events`)).WillReturnResult(sqlmock.NewResult(0, 0))
	// No INSERT statements because both slices are empty.
	mock.ExpectCommit()

	if err := repo.UpdateFinal(context.Background(), &BacktestJobRow{ID: "job-x", Status: "failed"}, nil, nil); err != nil {
		t.Fatalf("UpdateFinal: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

func TestBacktestRepoMarkInterruptedActive(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()

	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE backtest_jobs`)).
		WithArgs(now).
		WillReturnResult(sqlmock.NewResult(0, 3))

	n, err := repo.MarkInterruptedActive(context.Background(), now)
	if err != nil {
		t.Fatalf("MarkInterruptedActive: %v", err)
	}
	if n != 3 {
		t.Errorf("count = %d, want 3", n)
	}
}

func TestBacktestRepoGetJobSurfacesNotFound(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT id, fund_id, user_id, name, engine_kind`).
		WithArgs("missing").
		WillReturnError(sql.ErrNoRows)
	_, err := repo.GetJob(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestBacktestRepoListByFundReturnsRows(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()
	cols := []string{
		"id", "fund_id", "user_id", "name", "engine_kind", "status", "request", "error",
		"window_start", "window_end",
		"initial_cash", "final_nav", "cumulative_return", "annualized_return",
		"volatility", "sharpe_ratio", "max_drawdown", "win_rate",
		"trade_count", "winning_trade_count", "losing_trade_count",
		"total_days", "done_days",
		"submitted_at", "started_at", "completed_at",
		"sweep_id", "sweep_cell", "walk_forward",
	}
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows(cols).
		AddRow("job-1", "fund-1", "user-1", "trial", "fallback", "completed", []byte(`{"symbols":["AAPL"]}`), nil,
			now, now,
			sql.NullFloat64{Float64: 1000, Valid: true}, sql.NullFloat64{Float64: 1100, Valid: true}, sql.NullFloat64{Float64: 0.1, Valid: true}, sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			1, 0, 0,
			3, 3,
			now, sql.NullTime{}, sql.NullTime{Time: now, Valid: true},
			sql.NullString{}, []byte(`null`), []byte(`null`))
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1", 100).
		WillReturnRows(rows)

	out, err := repo.ListByFund(context.Background(), "fund-1", 0) // 0 → default 100
	if err != nil {
		t.Fatalf("ListByFund: %v", err)
	}
	if len(out) != 1 || out[0].ID != "job-1" || out[0].Status != "completed" {
		t.Errorf("unexpected list: %+v", out)
	}
}

func TestBacktestRepoCancelByIDIgnoresTerminal(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 20, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(`UPDATE backtest_jobs`).
		WithArgs(now, "job-1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	if err := repo.CancelByID(context.Background(), "job-1", now); !errors.Is(err, ErrNotFound) {
		t.Errorf("expected ErrNotFound when no rows match, got %v", err)
	}
}

func TestBacktestRepoNormaliseJSONRejectsInvalid(t *testing.T) {
	if _, err := normaliseJSON(json.RawMessage(`{not_json`)); err == nil {
		t.Errorf("expected error on invalid JSON")
	}
	out, err := normaliseJSON(nil)
	if err != nil || string(out) != "null" {
		t.Errorf("nil → null, got %q err=%v", out, err)
	}
}
