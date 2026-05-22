package main

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/exitmanager"
	"github.com/fundai/server/internal/repository"
)

// ---------------------------------------------------------------------------
// Pure unit tests for the wiring-side helpers
// ---------------------------------------------------------------------------

func TestMergeExitActionsDropsLLMActionForInstrumentUnderExitRule(t *testing.T) {
	exits := []repository.PlanAction{
		{
			InstrumentKey: "US:NVDA",
			Symbol:        "NVDA",
			Action:        "sell",
			Sleeve:        sql.NullString{String: "exit_manager", Valid: true},
			ExitReason:    sql.NullString{String: "stop_loss", Valid: true},
		},
	}
	llm := []repository.PlanAction{
		{
			InstrumentKey: "US:NVDA",
			Symbol:        "NVDA",
			Action:        "hold",
			Sleeve:        sql.NullString{String: "llm_pm", Valid: true},
		},
		{
			InstrumentKey: "US:AAPL",
			Symbol:        "AAPL",
			Action:        "buy",
			Sleeve:        sql.NullString{String: "llm_pm", Valid: true},
		},
	}
	out := mergeExitActions(exits, llm)
	if len(out) != 2 {
		t.Fatalf("expected 2 actions, got %d: %+v", len(out), out)
	}
	// Exit action first.
	if out[0].InstrumentKey != "US:NVDA" || out[0].Sleeve.String != "exit_manager" {
		t.Fatalf("expected exit action at front, got %+v", out[0])
	}
	// LLM AAPL preserved second.
	if out[1].InstrumentKey != "US:AAPL" || out[1].Action != "buy" {
		t.Fatalf("expected LLM AAPL action second, got %+v", out[1])
	}
}

func TestMergeExitActionsReturnsLLMUnchangedWhenNoExits(t *testing.T) {
	llm := []repository.PlanAction{
		{InstrumentKey: "X", Action: "buy"},
	}
	out := mergeExitActions(nil, llm)
	if len(out) != 1 || out[0].InstrumentKey != "X" {
		t.Fatalf("expected LLM passthrough, got %+v", out)
	}
}

func TestMergeExitActionsPreservesLLMRelativeOrder(t *testing.T) {
	exits := []repository.PlanAction{
		{InstrumentKey: "B", Action: "sell"},
	}
	llm := []repository.PlanAction{
		{InstrumentKey: "A", Action: "buy"},
		{InstrumentKey: "B", Action: "hold"}, // dropped
		{InstrumentKey: "C", Action: "buy"},
		{InstrumentKey: "D", Action: "buy"},
	}
	out := mergeExitActions(exits, llm)
	want := []string{"B", "A", "C", "D"}
	if len(out) != len(want) {
		t.Fatalf("expected %d actions, got %d", len(want), len(out))
	}
	for i, k := range want {
		if out[i].InstrumentKey != k {
			t.Fatalf("position %d: got %q, want %q", i, out[i].InstrumentKey, k)
		}
	}
}

func TestBuildExitPlanActionCopiesPositionMetadataAndAttribution(t *testing.T) {
	dec := exitmanager.ExitDecision{
		InstrumentKey: "US:NVDA",
		Symbol:        "NVDA",
		Market:        "us_equity",
		AssetClass:    "equity",
		Quantity:      120,
		TriggerPrice:  92.5,
		Reason:        "stop_loss",
		SignalSource:  "stop_loss",
		Reasoning:     "stop_loss: price 92.50 fell below 8% of entry 100.00",
		LotID:         "lot-1",
	}
	pos := &repository.HoldingPosition{
		Exchange:           sql.NullString{String: "NASDAQ", Valid: true},
		PositionSide:       sql.NullString{String: "long", Valid: true},
		QuoteCurrency:      sql.NullString{String: "USD", Valid: true},
		SettlementCurrency: sql.NullString{String: "USD", Valid: true},
		MarginMode:         sql.NullString{},
		Leverage:           sql.NullFloat64{Float64: 1, Valid: true},
		ContractMultiplier: sql.NullFloat64{Float64: 1, Valid: true},
		AssetClass:         sql.NullString{String: "equity", Valid: true},
		Market:             sql.NullString{String: "us_equity", Valid: true},
	}
	action := buildExitPlanAction(dec, pos)
	if action.Action != "sell" {
		t.Fatalf("action: got %q, want sell", action.Action)
	}
	if !action.Quantity.Valid || action.Quantity.Float64 != 120 {
		t.Fatalf("qty: got %+v, want 120", action.Quantity)
	}
	if !action.Price.Valid || action.Price.Float64 != 92.5 {
		t.Fatalf("price: got %+v, want 92.5", action.Price)
	}
	if action.Sleeve.String != "exit_manager" {
		t.Fatalf("sleeve: got %q, want exit_manager", action.Sleeve.String)
	}
	if action.ExitReason.String != "stop_loss" {
		t.Fatalf("exit_reason: got %q, want stop_loss", action.ExitReason.String)
	}
	if action.SignalSource.String != "stop_loss" {
		t.Fatalf("signal_source: got %q, want stop_loss", action.SignalSource.String)
	}
	if !action.Confidence.Valid || action.Confidence.Float64 != 1.0 {
		t.Fatalf("confidence: got %+v, want 1.0", action.Confidence)
	}
	if action.OpenClose.String != "close" {
		t.Fatalf("open_close: got %q, want close", action.OpenClose.String)
	}
	if !action.ReduceOnly.Valid || !action.ReduceOnly.Bool {
		t.Fatalf("reduce_only: got %+v, want true", action.ReduceOnly)
	}
	if action.Exchange.String != "NASDAQ" {
		t.Fatalf("exchange: got %q, want NASDAQ", action.Exchange.String)
	}
	if action.QuoteCurrency.String != "USD" {
		t.Fatalf("quote_currency: got %q, want USD", action.QuoteCurrency.String)
	}
	if action.Market.String != "us_equity" {
		t.Fatalf("market: got %q, want us_equity", action.Market.String)
	}
}

func TestBuildExitPlanActionWorksWithoutPositionMetadata(t *testing.T) {
	dec := exitmanager.ExitDecision{
		InstrumentKey: "X",
		Symbol:        "X",
		Quantity:      1,
		TriggerPrice:  1,
		Reason:        "time_stop",
		SignalSource:  "time_stop",
	}
	action := buildExitPlanAction(dec, nil)
	if action.Action != "sell" || action.Sleeve.String != "exit_manager" {
		t.Fatalf("metadata-less build broken: %+v", action)
	}
	// open_close should NOT be auto-set without a position row to
	// confirm we're actually closing a long.
	if action.OpenClose.Valid {
		t.Fatalf("expected open_close=NULL without pos meta, got %+v", action.OpenClose)
	}
}

// ---------------------------------------------------------------------------
// evaluateExitActions: end-to-end via sqlmock
// ---------------------------------------------------------------------------

// TestEvaluateExitActionsFiresStopLossEndToEnd walks the full path:
//   - fundRepo.GetByID returns a Fund whose config.exitPolicy has
//     a 8% stop_loss.
//   - positionRepo.ListByFund returns one held position whose
//     current_price is 8.1% below the lot entry.
//   - lotRepo.ListOpenByInstrument returns the matching open lot.
//   - The helper produces ONE PlanAction with sleeve=exit_manager
//     and exit_reason=stop_loss.
func TestEvaluateExitActionsFiresStopLossEndToEnd(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC)

	// 1. Fund lookup. exitPolicy enabled + 8% stop_loss.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-ex").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(
				"fund-ex", "co-1", "Exit Test", nil,
				"simulation", 100000.0, 100000.0, 100000.0, 1.0, "active",
				[]byte(`{"market":"us_equity","exitPolicy":{"enabled":true,"stopLoss":{"percent":0.08}}}`),
				now, now,
			))

	// 2. Positions: NVDA, 100 shares, current_price 91.9 (entry was 100).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-ex").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-ex", "US:NVDA", "NVDA", "NVIDIA",
			sql.NullString{String: "us_equity", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{String: "long", Valid: true},
			sql.NullString{String: "USD", Valid: true}, sql.NullString{String: "USD", Valid: true}, sql.NullString{},
			100.0, 100.0, 100.0, 91.9, 9190.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))

	// 3. Lot lookup: one open lot at entry 100, 100 shares.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, market, asset_class,
       opening_trade_id, opening_plan_action_id,
       opened_at, entry_price, entry_fees,
       quantity_opened, quantity_remaining,
       sleeve, regime_at_entry, signal_source, confidence_at_entry,
       highest_price_seen, lowest_price_seen, last_price, last_price_at,
       status, closed_at, created_at, updated_at
  FROM position_lots WHERE fund_id = $1 AND instrument_key = $2 AND status != 'closed' ORDER BY opened_at ASC, id ASC`)).
		WithArgs("fund-ex", "US:NVDA").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "market", "asset_class",
			"opening_trade_id", "opening_plan_action_id",
			"opened_at", "entry_price", "entry_fees",
			"quantity_opened", "quantity_remaining",
			"sleeve", "regime_at_entry", "signal_source", "confidence_at_entry",
			"highest_price_seen", "lowest_price_seen", "last_price", "last_price_at",
			"status", "closed_at", "created_at", "updated_at",
		}).AddRow(
			"lot-1", "fund-ex", "US:NVDA", "NVDA",
			sql.NullString{String: "us_equity", Valid: true}, sql.NullString{String: "equity", Valid: true},
			"trade-1", sql.NullString{},
			now.Add(-72*time.Hour), 100.0, 0.0,
			100.0, 100.0,
			sql.NullString{String: "llm_pm", Valid: true}, sql.NullString{}, sql.NullString{String: "llm_pm", Valid: true}, sql.NullFloat64{},
			sql.NullFloat64{Float64: 105, Valid: true}, sql.NullFloat64{Float64: 91.9, Valid: true},
			sql.NullFloat64{Float64: 91.9, Valid: true}, sql.NullTime{Time: now, Valid: true},
			"open", sql.NullTime{},
			now.Add(-72*time.Hour), now,
		))

	agent := &runtimePMAgent{
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		lotRepo:      repository.NewLotRepo(db),
		exitManager:  exitmanager.NewService(exitmanager.WithClock(func() time.Time { return now })),
	}

	actions := agent.evaluateExitActions(context.Background(), "fund-ex", now)
	if len(actions) != 1 {
		t.Fatalf("expected 1 exit action, got %d: %+v", len(actions), actions)
	}
	got := actions[0]
	if got.Action != "sell" {
		t.Fatalf("action: got %q, want sell", got.Action)
	}
	if got.Sleeve.String != "exit_manager" {
		t.Fatalf("sleeve: got %q, want exit_manager", got.Sleeve.String)
	}
	if got.ExitReason.String != "stop_loss" {
		t.Fatalf("exit_reason: got %q, want stop_loss", got.ExitReason.String)
	}
	if got.Quantity.Float64 != 100 {
		t.Fatalf("quantity: got %v, want 100", got.Quantity.Float64)
	}
	if got.InstrumentKey != "US:NVDA" {
		t.Fatalf("instrument_key: got %q, want US:NVDA", got.InstrumentKey)
	}
	assertMockExpectations(t, mock)
}

// TestEvaluateExitActionsNoopWhenPolicyDisabled confirms that an
// unconfigured (or explicitly disabled) exitPolicy short-circuits
// the path BEFORE any position / lot queries fire — i.e. zero
// extra DB load on funds that haven't opted in.
func TestEvaluateExitActionsNoopWhenPolicyDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, time.May, 20, 10, 0, 0, 0, time.UTC)

	// Fund lookup returns a config WITHOUT exitPolicy → policy disabled.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(
				"fund-legacy", "co-1", "Legacy Fund", nil,
				"simulation", 100000.0, 100000.0, 100000.0, 1.0, "active",
				[]byte(`{"market":"us_equity"}`),
				now, now,
			))

	agent := &runtimePMAgent{
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		lotRepo:      repository.NewLotRepo(db),
		exitManager:  exitmanager.NewService(exitmanager.WithClock(func() time.Time { return now })),
	}

	actions := agent.evaluateExitActions(context.Background(), "fund-legacy", now)
	if actions != nil {
		t.Fatalf("expected nil actions on disabled policy, got %+v", actions)
	}
	assertMockExpectations(t, mock)
}

// TestEvaluateExitActionsNilWhenWiringMissing exercises the
// defensive guard for tests / legacy deployments that don't wire
// lotRepo + exitManager. The path must safely no-op.
func TestEvaluateExitActionsNilWhenWiringMissing(t *testing.T) {
	agent := &runtimePMAgent{
		fundRepo:     nil,
		positionRepo: nil,
		lotRepo:      nil,
		exitManager:  nil,
	}
	if got := agent.evaluateExitActions(context.Background(), "fund-x", time.Now()); got != nil {
		t.Fatalf("expected nil on un-wired agent, got %+v", got)
	}
}
