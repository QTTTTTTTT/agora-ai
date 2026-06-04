package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/repository"
)

// orderReplayColumns mirrors repository.tradeExecutionColumns. Kept
// in sync by hand here so a schema drift is caught by the test
// failing with "could not match actual sql".
var orderReplayColumns = []string{
	"id", "fund_id", "plan_id", "plan_action_id", "instrument_key", "symbol",
	"market", "exchange", "asset_class", "instrument_type", "side", "position_side",
	"open_close", "order_type", "quantity", "price", "amount", "filled_qty",
	"filled_price", "fee_commission", "fee_stamp_tax", "fee_transfer",
	"trading_mode", "broker_order_id", "mcp_server_id", "status", "executed_at",
	"quote_currency", "settlement_currency", "margin_mode", "leverage",
	"contract_multiplier", "expiry_date", "reduce_only", "slippage_pct",
	"stop_price", "trail_amount", "trail_percent", "display_qty",
	"time_in_force", "good_till_date", "parent_trade_id",
	"strategy", "strategy_parent_trade_id",
	"client_idempotency_key", "created_at",
	"cancelled_at", "cancel_reason", "replaced_at", "replace_count",
}

// quietLogger discards all log output so test runs stay legible.
var quietLogger = slog.New(slog.NewTextHandler(io.Discard, nil))

// fakeQuoteFn returns a static quote for the simulator's bootstrap.
// The replay path does NOT route orders through tryFill so the quote
// is only consulted when the user POSTs a fresh order; we still
// supply something sensible for completeness.
func fakeQuoteFn() broker.QuoteFn {
	return func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		return matching.Quote{Last: 100, Bid: 99.99, Ask: 100.01}, nil
	}
}

func TestReplayOpenOrders_PopulatesSimulatorBook(t *testing.T) {
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 5, 30, 9, 30, 0, 0, time.UTC)

	// Two open trade rows: a working limit and a pending trailing-stop.
	rows := sqlmock.NewRows(orderReplayColumns).
		AddRow(
			"trade-working-1", "fund-A",
			sql.NullString{}, sql.NullString{},
			"us-equity:MSFT", "MSFT",
			sql.NullString{String: "us", Valid: true}, sql.NullString{},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{},
			"buy", sql.NullString{}, sql.NullString{},
			"limit",
			200.0,
			sql.NullFloat64{Float64: 250.5, Valid: true},
			sql.NullFloat64{},
			0.0,
			sql.NullFloat64{},
			0.0, 0.0, 0.0,
			"simulation",
			sql.NullString{String: "broker-restored-1", Valid: true},
			sql.NullString{},
			"working",
			sql.NullTime{},
			sql.NullString{}, sql.NullString{}, sql.NullString{},
			sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullTime{},
			sql.NullBool{},
			sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullString{String: "gtc", Valid: true},
			sql.NullTime{},
			sql.NullString{},
			sql.NullString{}, sql.NullString{}, // strategy + strategy_parent_trade_id
			sql.NullString{String: "client-restored-1", Valid: true},
			now,
			sql.NullTime{}, sql.NullString{}, sql.NullTime{},
			0,
		).
		AddRow(
			"trade-trailing-1", "fund-A",
			sql.NullString{}, sql.NullString{},
			"us-equity:NVDA", "NVDA",
			sql.NullString{String: "us", Valid: true}, sql.NullString{},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{},
			"sell", sql.NullString{}, sql.NullString{},
			"trailing_stop",
			50.0,
			sql.NullFloat64{},
			sql.NullFloat64{},
			0.0,
			sql.NullFloat64{},
			0.0, 0.0, 0.0,
			"simulation",
			sql.NullString{String: "broker-trailing-1", Valid: true},
			sql.NullString{},
			"pending",
			sql.NullTime{},
			sql.NullString{}, sql.NullString{}, sql.NullString{},
			sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullTime{},
			sql.NullBool{},
			sql.NullFloat64{},
			sql.NullFloat64{Float64: 90, Valid: true},
			sql.NullFloat64{}, // trail_amount
			sql.NullFloat64{Float64: 0.05, Valid: true},
			sql.NullFloat64{},
			sql.NullString{String: "gtc", Valid: true},
			sql.NullTime{},
			sql.NullString{},
			sql.NullString{}, sql.NullString{}, // strategy + strategy_parent_trade_id
			sql.NullString{String: "client-trailing-1", Valid: true},
			now,
			sql.NullTime{}, sql.NullString{}, sql.NullTime{},
			0,
		)

	mock.ExpectQuery(regexp.QuoteMeta(`FROM trade_executions
		 WHERE status IN ('pending', 'working', 'triggered', 'partial')`)).
		WithArgs(10000).
		WillReturnRows(rows)

	sim := broker.NewSimulator(fakeQuoteFn())
	report, err := replayOpenOrders(context.Background(), sim, repository.NewTradeRepo(db), quietLogger)
	if err != nil {
		t.Fatalf("replayOpenOrders: %v", err)
	}
	if report.Restored != 2 {
		t.Errorf("Restored = %d, want 2 (errors=%v)", report.Restored, report.Errors)
	}
	if len(report.Errors) != 0 {
		t.Errorf("unexpected per-row errors: %#v", report.Errors)
	}

	got, err := sim.GetOrder(context.Background(), "fund-A", "broker-restored-1")
	if err != nil {
		t.Fatalf("GetOrder: %v", err)
	}
	if got.State != broker.OrderStateWorking {
		t.Errorf("limit State = %v, want working", got.State)
	}
	if got.Request.LimitPrice != 250.5 {
		t.Errorf("limit price = %v, want 250.5", got.Request.LimitPrice)
	}
	if got.Request.TimeInForce != broker.TIFGTC {
		t.Errorf("limit TIF = %v, want gtc", got.Request.TimeInForce)
	}

	stop, err := sim.GetOrder(context.Background(), "fund-A", "broker-trailing-1")
	if err != nil {
		t.Fatalf("GetOrder trailing: %v", err)
	}
	if stop.Request.OrderType != broker.OrderTypeTrailingStop {
		t.Errorf("trailing OrderType = %v", stop.Request.OrderType)
	}
	if stop.Request.TrailPercent != 0.05 {
		t.Errorf("trailing TrailPercent = %v, want 0.05", stop.Request.TrailPercent)
	}
	if stop.CurrentStopPrice != 90 {
		t.Errorf("trailing CurrentStopPrice = %v, want 90 (seeded from stop_price)", stop.CurrentStopPrice)
	}

	// Trailing stop should now be visible to the trigger engine.
	pending := sim.AllPendingStops()
	if len(pending) != 1 || pending[0].BrokerOrderID != "broker-trailing-1" {
		t.Errorf("AllPendingStops = %#v, want one row with broker-trailing-1", pending)
	}

	assertMockExpectations(t, mock)
}

func TestReplayOpenOrders_NoOpOnEmptyResultSet(t *testing.T) {
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })

	mock.ExpectQuery(regexp.QuoteMeta(`FROM trade_executions
		 WHERE status IN ('pending', 'working', 'triggered', 'partial')`)).
		WithArgs(10000).
		WillReturnRows(sqlmock.NewRows(orderReplayColumns))

	sim := broker.NewSimulator(fakeQuoteFn())
	report, err := replayOpenOrders(context.Background(), sim, repository.NewTradeRepo(db), quietLogger)
	if err != nil {
		t.Fatalf("replayOpenOrders: %v", err)
	}
	if report.Restored != 0 {
		t.Errorf("Restored = %d, want 0 (no rows)", report.Restored)
	}
	assertMockExpectations(t, mock)
}

func TestReplayOpenOrders_SkipsRowsWithoutBrokerOrderID(t *testing.T) {
	db, mock := newMockDB(t)
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 5, 30, 9, 30, 0, 0, time.UTC)

	// Synthetic row with no broker_order_id (e.g. settlement-only row
	// inserted by the runtime before P0-2). Should be skipped at the
	// projection step, not panic.
	rows := sqlmock.NewRows(orderReplayColumns).
		AddRow(
			"trade-no-broker", "fund-A",
			sql.NullString{}, sql.NullString{},
			"us-equity:AAPL", "AAPL",
			sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
			"buy", sql.NullString{}, sql.NullString{},
			"limit",
			10.0,
			sql.NullFloat64{Float64: 200, Valid: true},
			sql.NullFloat64{},
			0.0,
			sql.NullFloat64{},
			0.0, 0.0, 0.0,
			"simulation",
			sql.NullString{}, // broker_order_id NULL
			sql.NullString{},
			"working",
			sql.NullTime{},
			sql.NullString{}, sql.NullString{}, sql.NullString{},
			sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullTime{},
			sql.NullBool{},
			sql.NullFloat64{},
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{}, sql.NullFloat64{},
			sql.NullString{},
			sql.NullTime{},
			sql.NullString{},
			sql.NullString{}, sql.NullString{}, // strategy + strategy_parent_trade_id
			sql.NullString{},
			now,
			sql.NullTime{}, sql.NullString{}, sql.NullTime{},
			0,
		)

	mock.ExpectQuery(regexp.QuoteMeta(`FROM trade_executions
		 WHERE status IN ('pending', 'working', 'triggered', 'partial')`)).
		WithArgs(10000).
		WillReturnRows(rows)

	sim := broker.NewSimulator(fakeQuoteFn())
	report, err := replayOpenOrders(context.Background(), sim, repository.NewTradeRepo(db), quietLogger)
	if err != nil {
		t.Fatalf("replayOpenOrders: %v", err)
	}
	if report.Restored != 0 {
		t.Errorf("Restored = %d, want 0 (row had no broker_order_id)", report.Restored)
	}
	assertMockExpectations(t, mock)
}

func TestReplayOpenOrders_NilSimulatorIsNoOp(t *testing.T) {
	report, err := replayOpenOrders(context.Background(), nil, nil, quietLogger)
	if err != nil {
		t.Fatalf("replayOpenOrders nil: %v", err)
	}
	if report.Restored != 0 {
		t.Errorf("Restored = %d, want 0 on nil deps", report.Restored)
	}
}

func TestMapTradeStatusToOrderState(t *testing.T) {
	cases := map[string]broker.OrderState{
		"pending":           broker.OrderStatePending,
		"submitted":         broker.OrderStatePending,
		"working":           broker.OrderStateWorking,
		"triggered":         broker.OrderStateTriggered,
		"partial":           broker.OrderStatePartial,
		"partial_filled":    broker.OrderStatePartial,
		"partially_filled":  broker.OrderStatePartial,
		"filled":            broker.OrderStateFilled,
		"cancelled":         broker.OrderStateCancelled,
		"rejected":          broker.OrderStateRejected,
		"failed":            broker.OrderStateRejected,
		"expired":           broker.OrderStateExpired,
		"   PENDING  ":      broker.OrderStatePending,
		"some-other-state":  broker.OrderState(""),
		"":                  broker.OrderState(""),
	}
	for in, want := range cases {
		got := mapTradeStatusToOrderState(in)
		if got != want {
			t.Errorf("mapTradeStatusToOrderState(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTradeRowToBrokerOrder_RejectsTerminalRows(t *testing.T) {
	terminal := &repository.TradeExecution{
		ID:            "trade-1",
		FundID:        "fund-A",
		InstrumentKey: "us-equity:AAPL",
		Symbol:        "AAPL",
		Side:          "buy",
		OrderType:     "limit",
		Quantity:      10,
		Status:        "filled",
		BrokerOrderID: sql.NullString{String: "broker-1", Valid: true},
	}
	if _, ok := tradeRowToBrokerOrder(terminal); ok {
		t.Errorf("tradeRowToBrokerOrder accepted a terminal row")
	}
}
