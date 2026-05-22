package repository

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newSweepRepoTest(t *testing.T) (*SweepRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	return NewSweepRepo(db), mock, func() { db.Close() }
}

func TestSweepRepoInsertHappy(t *testing.T) {
	repo, mock, cleanup := newSweepRepoTest(t)
	defer cleanup()

	row := &SweepRow{
		ID:          "11111111-1111-1111-1111-111111111111",
		FundID:      "fund-1",
		UserID:      "user-1",
		Name:        "slip x maxOrd",
		BaseRequest: json.RawMessage(`{"symbols":["AAPL"]}`),
		Axes:        json.RawMessage(`[{"name":"slippageBps","values":["3","5"]}]`),
		TotalCells:  2,
		CreatedAt:   time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC),
	}
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO backtest_sweeps`)).
		WithArgs(row.ID, row.FundID, row.UserID, row.Name, sqlmock.AnyArg(), sqlmock.AnyArg(), row.TotalCells, row.CreatedAt).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := repo.Insert(context.Background(), row); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet: %v", err)
	}
}

func TestSweepRepoInsertRejectsEmptyID(t *testing.T) {
	repo, _, cleanup := newSweepRepoTest(t)
	defer cleanup()
	if err := repo.Insert(context.Background(), &SweepRow{FundID: "f"}); err == nil {
		t.Errorf("expected error for empty id")
	}
}

func TestSweepRepoGetNotFound(t *testing.T) {
	repo, mock, cleanup := newSweepRepoTest(t)
	defer cleanup()
	mock.ExpectQuery(`SELECT id, fund_id, user_id, name`).
		WithArgs("missing").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "user_id", "name", "base_request", "axes", "total_cells", "created_at"}))
	_, err := repo.Get(context.Background(), "missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestSweepRepoListByFund(t *testing.T) {
	repo, mock, cleanup := newSweepRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{"id", "fund_id", "user_id", "name", "base_request", "axes", "total_cells", "created_at"}).
		AddRow("s-1", "fund-1", "u-1", "trial", []byte(`{}`), []byte(`[]`), 4, now).
		AddRow("s-2", "fund-1", "u-1", "trial2", []byte(`{}`), []byte(`[]`), 6, now.Add(-time.Hour))
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("fund-1", 50).
		WillReturnRows(rows)
	out, err := repo.ListByFund(context.Background(), "fund-1", 0)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 || out[0].ID != "s-1" || out[1].ID != "s-2" {
		t.Errorf("unexpected list: %+v", out)
	}
}

func TestBacktestRepoListBySweep(t *testing.T) {
	repo, mock, cleanup := newBacktestRepoTest(t)
	defer cleanup()
	now := time.Date(2026, 5, 21, 0, 0, 0, 0, time.UTC)
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
	mock.ExpectQuery(`SELECT id, fund_id`).
		WithArgs("sweep-1").
		WillReturnRows(sqlmock.NewRows(cols).
			AddRow("job-a", "fund-1", "u", "cell A", "fallback", "completed", []byte(`{}`), nil,
				now, now, nil, nil, nil, nil, nil, nil, nil, nil,
				0, 0, 0, 0, 0, now, nil, nil,
				"sweep-1", []byte(`{"slippageBps":"3"}`), []byte(`null`)).
			AddRow("job-b", "fund-1", "u", "cell B", "fallback", "completed", []byte(`{}`), nil,
				now, now, nil, nil, nil, nil, nil, nil, nil, nil,
				0, 0, 0, 0, 0, now.Add(time.Second), nil, nil,
				"sweep-1", []byte(`{"slippageBps":"5"}`), []byte(`null`)))
	out, err := repo.ListBySweep(context.Background(), "sweep-1")
	if err != nil {
		t.Fatalf("ListBySweep: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	if out[0].SweepID.String != "sweep-1" || string(out[0].SweepCell) != `{"slippageBps":"3"}` {
		t.Errorf("row 0 sweep fields wrong: %+v / %s", out[0].SweepID, string(out[0].SweepCell))
	}
}
