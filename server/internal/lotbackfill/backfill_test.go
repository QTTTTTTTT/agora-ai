package lotbackfill

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

func newTestService(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db, slog.New(slog.NewTextHandler(io.Discard, nil))), mock
}

func TestNewReturnsServiceWithDefaultLogger(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	svc := New(db, nil)
	if svc == nil {
		t.Fatal("New returned nil")
	}
	if svc.logger == nil {
		t.Fatal("logger left nil after New with nil arg")
	}
}

func TestRunRejectsNilReceiver(t *testing.T) {
	var s *Service
	if _, err := s.Run(context.Background()); err == nil {
		t.Fatal("expected error for nil receiver")
	}
}

func TestRunRejectsNilDB(t *testing.T) {
	s := &Service{}
	if _, err := s.Run(context.Background()); err == nil {
		t.Fatal("expected error for nil db")
	}
}

func TestRunInsertsAndReportsZeroSkipped(t *testing.T) {
	svc, mock := newTestService(t)

	// Two legacy holdings end up as two lots.
	mock.ExpectExec(insertSQLForTest).
		WithArgs(SleeveLabel).
		WillReturnResult(sqlmock.NewResult(0, 2))

	mock.ExpectQuery(skippedSQLForTest).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(0))

	stats, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Inserted != 2 {
		t.Fatalf("inserted = %d, want 2", stats.Inserted)
	}
	if stats.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0", stats.Skipped)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestRunReportsSkippedHoldings(t *testing.T) {
	svc, mock := newTestService(t)

	// Nothing inserted but a couple of holdings have no matching buy trade.
	mock.ExpectExec(insertSQLForTest).
		WithArgs(SleeveLabel).
		WillReturnResult(sqlmock.NewResult(0, 0))

	mock.ExpectQuery(skippedSQLForTest).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(3))

	stats, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Inserted != 0 {
		t.Fatalf("inserted = %d, want 0", stats.Inserted)
	}
	if stats.Skipped != 3 {
		t.Fatalf("skipped = %d, want 3", stats.Skipped)
	}
}

func TestRunReturnsErrorOnInsertFailure(t *testing.T) {
	svc, mock := newTestService(t)

	mock.ExpectExec(insertSQLForTest).
		WithArgs(SleeveLabel).
		WillReturnError(sql.ErrConnDone)

	_, err := svc.Run(context.Background())
	if !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("err = %v, want wrap of sql.ErrConnDone", err)
	}
}

func TestRunSwallowsSkippedQueryFailure(t *testing.T) {
	svc, mock := newTestService(t)

	mock.ExpectExec(insertSQLForTest).
		WithArgs(SleeveLabel).
		WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(skippedSQLForTest).
		WillReturnError(errors.New("boom"))

	stats, err := svc.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stats.Inserted != 1 {
		t.Fatalf("inserted = %d, want 1", stats.Inserted)
	}
	// Skipped left at zero when the query failed — we only
	// report what we know.
	if stats.Skipped != 0 {
		t.Fatalf("skipped = %d, want 0 on query failure", stats.Skipped)
	}
}

// Duplicate the SQL the production code embeds. Keeping a copy
// in the test file lets us pin the QueryMatcherEqual matcher
// against the exact bytes so a stray whitespace edit fails the
// test loudly rather than silently producing a different plan.
const insertSQLForTest = `
INSERT INTO position_lots
    (fund_id, instrument_key, symbol, market, asset_class,
     opening_trade_id, opened_at, entry_price, entry_fees,
     quantity_opened, quantity_remaining,
     sleeve, highest_price_seen, lowest_price_seen, last_price, last_price_at,
     status)
SELECT hp.fund_id, hp.instrument_key, hp.symbol, hp.market, hp.asset_class,
       te.id, te.executed_at, hp.cost_price, 0,
       hp.quantity, hp.quantity,
       $1, hp.cost_price, hp.cost_price, hp.cost_price, te.executed_at,
       'open'
  FROM holding_positions hp
  JOIN LATERAL (
    SELECT id, executed_at FROM trade_executions
     WHERE fund_id = hp.fund_id
       AND instrument_key = hp.instrument_key
       AND status = 'filled'
       AND side IN ('buy', 'open_long', 'close_short')
       AND executed_at IS NOT NULL
     ORDER BY executed_at DESC
     LIMIT 1
  ) te ON TRUE
 WHERE hp.quantity > 0
   AND hp.instrument_key <> ''
   AND hp.cost_price > 0
   AND (hp.position_side IS NULL OR hp.position_side = 'long')
   AND NOT EXISTS (
     SELECT 1 FROM position_lots pl
     WHERE pl.fund_id = hp.fund_id
       AND pl.instrument_key = hp.instrument_key
       AND pl.status <> 'closed'
   )`

const skippedSQLForTest = `
SELECT COUNT(*)
  FROM holding_positions hp
 WHERE hp.quantity > 0
   AND hp.instrument_key <> ''
   AND hp.cost_price > 0
   AND (hp.position_side IS NULL OR hp.position_side = 'long')
   AND NOT EXISTS (
     SELECT 1 FROM position_lots pl
     WHERE pl.fund_id = hp.fund_id
       AND pl.instrument_key = hp.instrument_key
       AND pl.status <> 'closed'
   )
   AND NOT EXISTS (
     SELECT 1 FROM trade_executions te
     WHERE te.fund_id = hp.fund_id
       AND te.instrument_key = hp.instrument_key
       AND te.status = 'filled'
       AND te.side IN ('buy', 'open_long', 'close_short')
       AND te.executed_at IS NOT NULL
   )`
