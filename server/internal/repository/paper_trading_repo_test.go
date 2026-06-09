package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func newPaperRepoTest(t *testing.T) (*PaperTradingRepo, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherRegexp))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewPaperTradingRepo(db), mock, func() { db.Close() }
}

func TestPaperRepoCreatePortfolio(t *testing.T) {
	repo, mock, cleanup := newPaperRepoTest(t)
	defer cleanup()

	in := PaperPortfolioRow{
		Name:           "US Momentum Top30",
		Strategy:       "momentum_top30_monthly",
		Market:         "us_equity",
		InitialCapital: 100_000,
	}
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO paper_portfolios`)).
		WithArgs(in.Name, in.Strategy, in.Market, sqlmock.AnyArg(),
			in.InitialCapital, in.InitialCapital, in.InitialCapital).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).
			AddRow("pf-1", time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)))

	out, err := repo.CreatePortfolio(context.Background(), in)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if out.ID != "pf-1" {
		t.Errorf("id = %q, want pf-1", out.ID)
	}
	if out.CurrentNav != 100_000 {
		t.Errorf("default current_nav = %v, want 100_000", out.CurrentNav)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPaperRepoInsertOrderRequiresHash(t *testing.T) {
	repo, _, cleanup := newPaperRepoTest(t)
	defer cleanup()

	_, err := repo.InsertOrder(context.Background(), PaperOrderRow{
		PortfolioID: "pf-1", Symbol: "AAPL", Action: "BUY",
		// missing HashSignature + CanonicalPayload
	})
	if err == nil {
		t.Fatalf("expected error for missing hash + payload")
	}
}

func TestPaperRepoInsertOrderHappyPath(t *testing.T) {
	repo, mock, cleanup := newPaperRepoTest(t)
	defer cleanup()

	in := PaperOrderRow{
		PortfolioID:      "pf-1",
		Symbol:           "AAPL",
		Action:           "BUY",
		HashSignature:    "deadbeef",
		CanonicalPayload: `{"a":1}`,
		AIReasoning:      json.RawMessage(`{"confidence":0.9}`),
	}
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO paper_orders`)).
		WithArgs("pf-1", "AAPL", "BUY",
			sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(),
			"deadbeef", `{"a":1}`, sqlmock.AnyArg(), "pending").
		WillReturnRows(sqlmock.NewRows([]string{"id", "decided_at"}).
			AddRow("order-1", time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)))

	out, err := repo.InsertOrder(context.Background(), in)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if out.ID != "order-1" {
		t.Errorf("id = %q", out.ID)
	}
	if out.OTSStatus != "pending" {
		t.Errorf("ots_status default = %q, want pending", out.OTSStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPaperRepoUpsertNav(t *testing.T) {
	repo, mock, cleanup := newPaperRepoTest(t)
	defer cleanup()

	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO paper_nav_history`)).
		WithArgs("pf-1", sqlmock.AnyArg(), 105_000.0, sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))

	dr := 0.02
	if err := repo.UpsertNav(context.Background(), PaperNavRow{
		PortfolioID:  "pf-1",
		SnapshotDate: time.Date(2026, 6, 5, 0, 0, 0, 0, time.UTC),
		Nav:          105_000,
		DailyReturn:  sql.NullFloat64{Float64: dr, Valid: true},
	}); err != nil {
		t.Fatalf("upsert nav: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPaperRepoNavHistoryEmpty(t *testing.T) {
	repo, mock, cleanup := newPaperRepoTest(t)
	defer cleanup()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT portfolio_id, snapshot_date, nav`)).
		WithArgs("pf-empty", 365).
		WillReturnRows(sqlmock.NewRows([]string{"portfolio_id", "snapshot_date", "nav", "daily_return", "benchmark_nav"}))

	out, err := repo.NavHistory(context.Background(), "pf-empty", 0)
	if err != nil {
		t.Fatalf("nav history: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %d rows", len(out))
	}
}
