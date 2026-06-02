package marketstatus

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

func TestRepo_GetByKey_NotFoundReturnsNil(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_market_status")).
		WithArgs("AAPL.US").
		WillReturnError(sqlNoRowsErr())
	got, err := repo.GetByKey(context.Background(), "AAPL.US")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("got %+v, want nil", got)
	}
}

func TestRepo_GetByKey_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("FROM instrument_market_status")).
		WithArgs("AAPL.US").
		WillReturnRows(sqlmock.NewRows([]string{
			"instrument_key", "symbol", "market", "status", "halt_reason",
			"halt_started_at", "halt_until", "lower_limit", "upper_limit",
			"last_quote_at", "last_quote_price",
			"asset_class", "staleness_budget_seconds", "note", "updated_at",
		}).AddRow(
			"AAPL.US", "AAPL", "US", "trading", "",
			nil, nil, nil, nil,
			now.Add(-10*time.Second), 200.5,
			"equity", nil, "", now,
		))
	got, err := repo.GetByKey(context.Background(), "AAPL.US")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil || got.Status != "trading" || got.LastQuoteAt == nil {
		t.Errorf("got %+v", got)
	}
}

func TestRepo_UpsertStatus_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO instrument_market_status")).
		WithArgs("600519.SH", "600519", "CN", "trading", nil,
			nil, nil, float64(1800), float64(2200),
			"equity", nil, nil, nil).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertStatus(context.Background(), UpsertStatusParams{
		InstrumentKey: "600519.SH", Symbol: "600519", Market: "CN",
		Status: "trading",
		LowerLimit: fptr(1800), UpperLimit: fptr(2200),
	}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_UpsertStatus_RejectsBadStatus(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertStatus(context.Background(), UpsertStatusParams{
		InstrumentKey: "X.US", Status: "wat",
	}); err == nil {
		t.Error("expected status validation error")
	}
}

func TestRepo_UpsertStatus_RejectsLowerGtUpper(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	if err := repo.UpsertStatus(context.Background(), UpsertStatusParams{
		InstrumentKey: "X.US", Status: "trading",
		LowerLimit: fptr(200), UpperLimit: fptr(100),
	}); err == nil {
		t.Error("expected lower>upper validation error")
	}
}

func TestRepo_UpsertStatus_RejectsBadStaleBudget(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	bad := 0
	if err := repo.UpsertStatus(context.Background(), UpsertStatusParams{
		InstrumentKey: "X.US", Status: "trading", StalenessBudgetSecs: &bad,
	}); err == nil {
		t.Error("expected staleness validation error")
	}
}

func TestRepo_TouchQuote_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO instrument_market_status")).
		WithArgs("AAPL.US", "AAPL", "US", "equity", now, 200.5).
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.TouchQuote(context.Background(), "AAPL.US", "AAPL", "US", "equity", 200.5, now); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_UpsertCalendarDay_Defaults(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	d := time.Date(2026, 12, 24, 0, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO trading_calendar")).
		WithArgs("HK", "2026-12-24", true, "09:30", "12:00", "Asia/Hong_Kong", true, "Christmas Eve").
		WillReturnResult(sqlmock.NewResult(1, 1))
	if err := repo.UpsertCalendarDay(context.Background(), UpsertCalendarDayParams{
		Market: "HK", TradingDate: d, IsOpen: true,
		OpenLocal: "09:30", CloseLocal: "12:00",
		MarketTZ: "Asia/Hong_Kong", HalfDay: true, Note: "Christmas Eve",
	}); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestRepo_GetCalendarDay_NotFound(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM trading_calendar")).
		WithArgs("US", "2026-06-01").
		WillReturnError(sqlNoRowsErr())
	got, err := repo.GetCalendarDay(context.Background(), "US", time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("got %+v", got)
	}
}

func TestRepo_InsertEvent_RefusesAllow(t *testing.T) {
	repo, _, cleanup := newMockedRepo(t)
	defer cleanup()
	_, err := repo.InsertEvent(context.Background(), "f1", "AAPL.US", "AAPL", "co-1", Event{
		Decision: DecisionAllow, RuleCode: RuleHalted, Summary: "won't be persisted",
	})
	if err == nil {
		t.Error("allow events must not persist")
	}
}

func TestRepo_InsertEvent_Happy(t *testing.T) {
	repo, mock, cleanup := newMockedRepo(t)
	defer cleanup()
	now := time.Date(2026, 6, 1, 14, 30, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO marketstatus_events")).
		WithArgs("f1", "AAPL.US", "AAPL", "reject", "halted",
			"halted", sqlmock.AnyArg(), "co-1", now).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("ev-1"))
	id, err := repo.InsertEvent(context.Background(), "f1", "AAPL.US", "AAPL", "co-1", Event{
		Decision: DecisionReject, RuleCode: RuleHalted,
		Summary: "halted", DetectedAt: now,
		Metadata: map[string]any{"halt_reason": "news"},
	})
	if err != nil || id != "ev-1" {
		t.Errorf("id = %q err = %v", id, err)
	}
}

// sqlNoRowsErr returns the standard sql.ErrNoRows so the repo's
// errors.Is check matches.
func sqlNoRowsErr() error { return sql.ErrNoRows }
