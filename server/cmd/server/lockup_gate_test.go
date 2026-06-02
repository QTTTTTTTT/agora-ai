package main

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/broker"
)

func newLockupGateEnv(t *testing.T) (*lockupGate, sqlmock.Sqlmock, func()) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	g := newLockupGate(db, newServerMetrics(), nil)
	return g, mock, func() { _ = db.Close() }
}

func lockupRecordRows() *sqlmock.Rows {
	return sqlmock.NewRows([]string{
		"id", "fund_id", "instrument_key", "symbol",
		"locked_qty", "locked_until", "lockup_reason",
		"source_lot_id", "note",
		"released_at", "released_reason", "released_by",
		"created_by", "created_at", "updated_at",
	})
}

func TestLockupGate_BuyShortCircuits(t *testing.T) {
	g, _, cleanup := newLockupGateEnv(t)
	defer cleanup()
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "buy", Quantity: 100,
	})
	if verdict.Rejected {
		t.Errorf("buy must never be rejected")
	}
}

func TestLockupGate_NoActiveRecords_Allow(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows())
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 100,
	})
	if verdict.Rejected {
		t.Errorf("expected allow with no records, got %+v", verdict)
	}
}

func TestLockupGate_RejectsLockedSell(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(60), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "AAPL.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(100)))
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 50,
	})
	// Available = 100 - 60 = 40, order 50 > 40 → reject.
	if !verdict.Rejected {
		t.Fatalf("expected reject, got %+v", verdict)
	}
	if verdict.RejectReason == "" {
		t.Error("expected non-empty reject reason")
	}
}

func TestLockupGate_AllowsWithinUnlocked(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(60), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "AAPL.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(100)))
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 30,
	})
	if verdict.Rejected {
		t.Fatalf("expected allow, got %+v", verdict)
	}
}

func TestLockupGate_FailOpenOnLookupErr(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnError(errors.New("db down"))
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 50,
	})
	if verdict.Rejected {
		t.Errorf("fail-open: expected allow on lookup err, got %+v", verdict)
	}
}

func TestLockupGate_FailOpenOnPositionErr(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(60), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "AAPL.US").
		WillReturnError(errors.New("db down"))
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 50,
	})
	if verdict.Rejected {
		t.Errorf("fail-open: expected allow on position err, got %+v", verdict)
	}
	if len(verdict.Warnings) == 0 {
		t.Errorf("expected warning when failing open")
	}
}

func TestLockupGate_NoPositionRow_Reject(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(90 * 24 * time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(60), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	// No position → sql.ErrNoRows from Scan, gate translates to qty=0.
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "AAPL.US").
		WillReturnError(sql.ErrNoRows)
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 50,
	})
	if !verdict.Rejected {
		t.Errorf("expected reject when position 0 + locked records, got %+v", verdict)
	}
}

func TestLockupGate_WarningOnApproachingUnlock(t *testing.T) {
	g, mock, cleanup := newLockupGateEnv(t)
	defer cleanup()
	now := time.Now().UTC()
	until := now.Add(2 * 24 * time.Hour) // < 7 days away
	mock.ExpectQuery(regexp.QuoteMeta("FROM position_lockups")).
		WithArgs("f1", "AAPL.US", sqlmock.AnyArg()).
		WillReturnRows(lockupRecordRows().AddRow(
			"id1", "f1", "AAPL.US", "AAPL",
			float64(40), until, "ipo",
			nil, "",
			nil, "", nil,
			nil, now, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta("FROM holding_positions")).
		WithArgs("f1", "AAPL.US").
		WillReturnRows(sqlmock.NewRows([]string{"quantity"}).AddRow(float64(100)))
	verdict := g.CheckOrder(context.Background(), broker.LockupProbe{
		FundID: "f1", InstrumentKey: "AAPL.US", Side: "sell", Quantity: 30,
	})
	if verdict.Rejected {
		t.Fatalf("expected allow, got %+v", verdict)
	}
	if len(verdict.Warnings) == 0 {
		t.Error("expected approaching-unlock warning within 7 days")
	}
}

func TestLockupGate_NilSafe(t *testing.T) {
	var g *lockupGate
	v := g.CheckOrder(context.Background(), broker.LockupProbe{Side: "sell"})
	if v.Rejected {
		t.Error("nil gate should not reject")
	}
}
