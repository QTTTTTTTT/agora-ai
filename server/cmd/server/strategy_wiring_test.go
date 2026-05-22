package main

import (
	"context"
	"database/sql"
	"math"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/sizing"
	"github.com/fundai/server/internal/strategy"
)

// ---------------------------------------------------------------------------
// Pure-unit tests for mergeSleeveActions + buildSleevePlanAction
// ---------------------------------------------------------------------------

func TestMergeSleeveActionsReplacesLLMOnSameInstrument(t *testing.T) {
	sleeveAction := repository.PlanAction{
		InstrumentKey: "US:NVDA",
		Symbol:        "NVDA",
		Action:        "buy",
		Confidence:    sql.NullFloat64{Float64: 0.7, Valid: true},
		Sleeve:        sql.NullString{String: "trend", Valid: true},
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
	out := mergeSleeveActions([]repository.PlanAction{sleeveAction}, llm)
	if len(out) != 2 {
		t.Fatalf("expected 2 actions, got %d: %+v", len(out), out)
	}
	// Sleeve action prepended.
	if out[0].InstrumentKey != "US:NVDA" || out[0].Sleeve.String != "trend" {
		t.Fatalf("expected sleeve action first, got %+v", out[0])
	}
	if out[0].Action != "buy" {
		t.Fatalf("expected buy from sleeve, got %q", out[0].Action)
	}
	// LLM AAPL preserved.
	if out[1].InstrumentKey != "US:AAPL" || out[1].Sleeve.String != "llm_pm" {
		t.Fatalf("expected LLM AAPL preserved, got %+v", out[1])
	}
}

func TestMergeSleeveActionsDedupesAgreementByConfidence(t *testing.T) {
	weak := repository.PlanAction{
		InstrumentKey: "US:NVDA",
		Symbol:        "NVDA",
		Action:        "buy",
		Confidence:    sql.NullFloat64{Float64: 0.6, Valid: true},
		Sleeve:        sql.NullString{String: "trend", Valid: true},
		Reasoning:     sql.NullString{String: "trend buy", Valid: true},
	}
	strong := repository.PlanAction{
		InstrumentKey: "US:NVDA",
		Symbol:        "NVDA",
		Action:        "buy",
		Confidence:    sql.NullFloat64{Float64: 0.85, Valid: true},
		Sleeve:        sql.NullString{String: "mean_reversion", Valid: true},
		Reasoning:     sql.NullString{String: "mean rev buy", Valid: true},
	}
	out := mergeSleeveActions([]repository.PlanAction{weak, strong}, nil)
	if len(out) != 1 {
		t.Fatalf("expected dedupe to one action, got %d: %+v", len(out), out)
	}
	if out[0].Sleeve.String != "mean_reversion" {
		t.Fatalf("expected stronger sleeve to win, got %q", out[0].Sleeve.String)
	}
}

func TestMergeSleeveActionsConflictHigherConfidenceWins(t *testing.T) {
	buyWeak := repository.PlanAction{
		InstrumentKey: "US:NVDA",
		Action:        "buy",
		Confidence:    sql.NullFloat64{Float64: 0.55, Valid: true},
		Sleeve:        sql.NullString{String: "trend", Valid: true},
	}
	sellStrong := repository.PlanAction{
		InstrumentKey: "US:NVDA",
		Action:        "sell",
		Confidence:    sql.NullFloat64{Float64: 0.9, Valid: true},
		Sleeve:        sql.NullString{String: "mean_reversion", Valid: true},
	}
	out := mergeSleeveActions([]repository.PlanAction{buyWeak, sellStrong}, nil)
	if len(out) != 1 || out[0].Action != "sell" || out[0].Sleeve.String != "mean_reversion" {
		t.Fatalf("expected stronger sell to win conflict, got %+v", out)
	}
}

func TestBuildSleevePlanActionSetsSellQuantityAndExitReason(t *testing.T) {
	sa := strategy.SleeveAction{
		Sleeve:        "mean_reversion",
		Symbol:        "AAPL",
		InstrumentKey: "US:AAPL",
		Market:        "us_equity",
		AssetClass:    "equity",
		Regime:        regime.Range,
		Proposal: strategy.Proposal{
			Action:       strategy.ActionSell,
			Confidence:   0.78,
			Reasoning:    "overbought",
			SignalSource: "rsi_bb_14_20",
			StopLoss:     108.5,
		},
	}
	pos := &repository.HoldingPosition{
		Quantity:           150,
		Exchange:           sql.NullString{String: "NASDAQ", Valid: true},
		PositionSide:       sql.NullString{String: "long", Valid: true},
		QuoteCurrency:      sql.NullString{String: "USD", Valid: true},
		SettlementCurrency: sql.NullString{String: "USD", Valid: true},
	}
	action := buildSleevePlanAction(sa, pos, sizing.Result{}, 0)
	if action.Action != "sell" {
		t.Fatalf("action: got %q, want sell", action.Action)
	}
	if !action.Quantity.Valid || action.Quantity.Float64 != 150 {
		t.Fatalf("quantity should equal position qty: got %+v", action.Quantity)
	}
	if action.Sleeve.String != "mean_reversion" {
		t.Fatalf("sleeve: got %q", action.Sleeve.String)
	}
	if action.SignalSource.String != "rsi_bb_14_20" {
		t.Fatalf("signal_source: got %q", action.SignalSource.String)
	}
	if action.ExitReason.String != "mean_reversion" {
		t.Fatalf("exit_reason should be sleeve name on sell: got %q", action.ExitReason.String)
	}
	if !action.ReduceOnly.Valid || !action.ReduceOnly.Bool {
		t.Fatalf("reduce_only should be true on sleeve sell: got %+v", action.ReduceOnly)
	}
	if action.OpenClose.String != "close" {
		t.Fatalf("open_close: got %q, want close", action.OpenClose.String)
	}
	if action.RegimeTag.String != "range" {
		t.Fatalf("regime: got %q", action.RegimeTag.String)
	}
	if !action.StopLoss.Valid || action.StopLoss.Float64 != 108.5 {
		t.Fatalf("stop_loss: got %+v", action.StopLoss)
	}
}

func TestBuildSleevePlanActionBuyLeavesQuantityNullAndExitReasonUnset(t *testing.T) {
	sa := strategy.SleeveAction{
		Sleeve:        "trend",
		Symbol:        "NVDA",
		InstrumentKey: "US:NVDA",
		Regime:        regime.TrendUp,
		Proposal: strategy.Proposal{
			Action:       strategy.ActionBuy,
			Confidence:   0.8,
			SignalSource: "donchian_20",
		},
	}
	pos := &repository.HoldingPosition{Quantity: 100, PositionSide: sql.NullString{String: "long", Valid: true}}
	action := buildSleevePlanAction(sa, pos, sizing.Result{}, 0)
	if action.Quantity.Valid {
		t.Fatalf("buy quantity should be NULL when ATR sizing disabled (legacy downstream sizer takes over): got %+v", action.Quantity)
	}
	if action.ExitReason.Valid {
		t.Fatalf("buy should NOT carry exit_reason: got %+v", action.ExitReason)
	}
	if action.ReduceOnly.Valid && action.ReduceOnly.Bool {
		t.Fatalf("buy must not set reduce_only: got %+v", action.ReduceOnly)
	}
	if action.OpenClose.String != "open" {
		t.Fatalf("open_close: got %q, want open", action.OpenClose.String)
	}
}

func TestBuildSleevePlanActionBuyAppliesATRSizing(t *testing.T) {
	// With the Phase 3A-6 sizer applied, the buy action must
	// carry the ATR-derived quantity/amount/stop_loss instead
	// of leaving them NULL. Verifies the sized.Result fields
	// land on the PlanAction and the reason string is merged.
	sa := strategy.SleeveAction{
		Sleeve:        "trend",
		Symbol:        "NVDA",
		InstrumentKey: "US:NVDA",
		Market:        "us_equity",
		AssetClass:    "equity",
		Regime:        regime.TrendUp,
		Proposal: strategy.Proposal{
			Action:       strategy.ActionBuy,
			Confidence:   0.8,
			SignalSource: "donchian_20",
			Reasoning:    "20-day Donchian breakout",
		},
	}
	pos := &repository.HoldingPosition{
		Quantity:           100,
		PositionSide:       sql.NullString{String: "long", Valid: true},
		Exchange:           sql.NullString{String: "NASDAQ", Valid: true},
		QuoteCurrency:      sql.NullString{String: "USD", Valid: true},
		SettlementCurrency: sql.NullString{String: "USD", Valid: true},
	}
	sized := sizing.Result{
		Applied:     true,
		Quantity:    42,
		StopPrice:   95.0,
		RiskDollars: 210,
		ATR:         2.5,
		Reason:      "ATR-sized 42 shares @ 100.00 (risk $5.00 / share from-ATR×2.0, stop @ 95.00, ATR(14)=2.5000, R=0.50% NAV=500.00$)",
	}
	action := buildSleevePlanAction(sa, pos, sized, 100.0)
	if !action.Quantity.Valid || action.Quantity.Float64 != 42 {
		t.Fatalf("expected sized qty=42, got %+v", action.Quantity)
	}
	if !action.Price.Valid || action.Price.Float64 != 100.0 {
		t.Fatalf("expected price set from lastClose=100, got %+v", action.Price)
	}
	if !action.Amount.Valid || math.Abs(action.Amount.Float64-4200) > 0.01 {
		t.Fatalf("expected amount = qty*lastClose = 4200, got %+v", action.Amount)
	}
	if !action.StopLoss.Valid || action.StopLoss.Float64 != 95.0 {
		t.Fatalf("expected stop_loss = sized.StopPrice = 95, got %+v", action.StopLoss)
	}
	if !action.Reasoning.Valid {
		t.Fatalf("reasoning should be populated: got %+v", action.Reasoning)
	}
	want := "20-day Donchian breakout | ATR-sized"
	if got := action.Reasoning.String; len(got) < len(want) || got[:len(want)] != want {
		t.Fatalf("expected reasoning to merge sleeve + sizer, got %q", got)
	}
	if action.OpenClose.String != "open" {
		t.Fatalf("open_close: got %q, want open", action.OpenClose.String)
	}
}

// ---------------------------------------------------------------------------
// evaluateStrategySleeves: end-to-end via sqlmock
// ---------------------------------------------------------------------------

// strategyStubFetcher returns deterministic uptrend bars for a
// preset symbol; everything else 404s. Exercises the actual
// strategy.Service path against a real classifier so the wiring
// test surfaces any contract drift between layers.
type strategyStubFetcher struct {
	bars map[string][]ohlc.Bar
}

func (s *strategyStubFetcher) Fetch(_ context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	if b, ok := s.bars[req.Symbol]; ok {
		return b, nil
	}
	return nil, ohlc.ErrNoData
}

func makeUptrendForStrategy() []ohlc.Bar {
	n := 260
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		base := 100.0 * (1 + 0.5*float64(i)/float64(n-1))
		c := base + 0.3*math.Sin(float64(i)/5)
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.005,
			Low:    c * 0.995,
			Close:  c,
			Volume: 1e6,
		}
	}
	last := n - 1
	for j := last - 21; j < last; j++ {
		bars[j].High = bars[j].Close * 1.005
	}
	bars[last].Close = bars[last-1].High * 1.10
	bars[last].High = bars[last].Close
	return bars
}

// TestEvaluateStrategySleevesProducesActionForHeldUptrend covers
// the happy path: enabled trend sleeve, held NVDA position,
// uptrend OHLC. Expected: one trend BUY action with sleeve tag.
func TestEvaluateStrategySleevesProducesActionForHeldUptrend(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-strat").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(
				"fund-strat", "co-1", "Strategy Fund", nil,
				"simulation", 100000.0, 100000.0, 100000.0, 1.0, "active",
				[]byte(`{"market":"us_equity","strategySleeves":{"enabled":true,"enabledSleeves":["trend"]}}`),
				now, now,
			))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-strat").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-strat", "US:NVDA", "NVDA", "NVIDIA",
			sql.NullString{String: "us_equity", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{String: "long", Valid: true},
			sql.NullString{String: "USD", Valid: true}, sql.NullString{String: "USD", Valid: true}, sql.NullString{},
			100.0, 100.0, 100.0, 150.0, 15000.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))

	fetcher := &strategyStubFetcher{bars: map[string][]ohlc.Bar{
		"NVDA": makeUptrendForStrategy(),
	}}
	agent := &runtimePMAgent{
		fundRepo:      repository.NewFundRepo(db),
		positionRepo:  repository.NewPositionRepo(db),
		ohlcFetcher:   fetcher,
		regimeService: regime.NewService(fetcher),
	}

	actions := agent.evaluateStrategySleeves(context.Background(), "fund-strat", now)
	if len(actions) != 1 {
		t.Fatalf("expected 1 sleeve action, got %d: %+v", len(actions), actions)
	}
	got := actions[0]
	if got.Action != "buy" {
		t.Fatalf("action: got %q, want buy", got.Action)
	}
	if got.Sleeve.String != "trend" {
		t.Fatalf("sleeve: got %q, want trend", got.Sleeve.String)
	}
	if got.SignalSource.String != "donchian_20" {
		t.Fatalf("signal_source: got %q", got.SignalSource.String)
	}
	if got.RegimeTag.String != "trend_up" {
		t.Fatalf("regime: got %q, want trend_up", got.RegimeTag.String)
	}
	if !got.Confidence.Valid || got.Confidence.Float64 < 0.55 {
		t.Fatalf("confidence: got %+v", got.Confidence)
	}
	assertMockExpectations(t, mock)
}

// TestEvaluateStrategySleevesWithATRSizingPopulatesQuantity exercises
// the full Phase 3A-6 wiring: a fund config with both strategySleeves
// AND riskSizing enabled must produce a PlanAction whose quantity is
// sized by ATR (not left NULL for the downstream sizer).
func TestEvaluateStrategySleevesWithATRSizingPopulatesQuantity(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-atr").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(
				"fund-atr", "co-1", "ATR Fund", nil,
				"simulation", 100000.0, 100000.0, 100000.0, 1.0, "active",
				[]byte(`{"market":"us_equity","strategySleeves":{"enabled":true,"enabledSleeves":["trend"]},"riskSizing":{"enabled":true,"perTradeRiskPct":0.005,"atrLookback":14,"atrStopMultiplier":2.0,"maxNotionalPctOfNAV":0.5}}`),
				now, now,
			))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-atr").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-atr", "US:NVDA", "NVDA", "NVIDIA",
			sql.NullString{String: "us_equity", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{String: "long", Valid: true},
			sql.NullString{String: "USD", Valid: true}, sql.NullString{String: "USD", Valid: true}, sql.NullString{},
			100.0, 100.0, 100.0, 150.0, 15000.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))

	fetcher := &strategyStubFetcher{bars: map[string][]ohlc.Bar{
		"NVDA": makeUptrendForStrategy(),
	}}
	agent := &runtimePMAgent{
		fundRepo:      repository.NewFundRepo(db),
		positionRepo:  repository.NewPositionRepo(db),
		ohlcFetcher:   fetcher,
		regimeService: regime.NewService(fetcher),
	}

	actions := agent.evaluateStrategySleeves(context.Background(), "fund-atr", now)
	if len(actions) != 1 {
		t.Fatalf("expected 1 sleeve action, got %d: %+v", len(actions), actions)
	}
	got := actions[0]
	if got.Action != "buy" {
		t.Fatalf("action: got %q, want buy", got.Action)
	}
	if !got.Quantity.Valid || got.Quantity.Float64 <= 0 {
		t.Fatalf("ATR sizing must populate a positive quantity, got %+v", got.Quantity)
	}
	if !got.Amount.Valid || got.Amount.Float64 <= 0 {
		t.Fatalf("ATR sizing must populate a positive amount, got %+v", got.Amount)
	}
	if !got.StopLoss.Valid || got.StopLoss.Float64 <= 0 {
		t.Fatalf("ATR sizing must populate a stop_loss, got %+v", got.StopLoss)
	}
	// Sanity: notional must respect the 50% NAV cap configured above.
	notional := got.Quantity.Float64 * got.Price.Float64
	if notional > 100000.0*0.5+0.01 {
		t.Fatalf("notional %.2f exceeds 50%% NAV cap", notional)
	}
	// Reasoning should mention ATR sizing so the audit trail
	// surfaces *why* this quantity was picked.
	if !regexp.MustCompile(`(?i)ATR-sized`).MatchString(got.Reasoning.String) {
		t.Fatalf("reasoning should mention ATR sizing, got %q", got.Reasoning.String)
	}
	assertMockExpectations(t, mock)
}

// TestEvaluateStrategySleevesNoopWhenPolicyDisabled confirms a
// fund without strategySleeves in its config does NOT trigger
// the position / OHLC lookup chain — keeps the legacy code path
// cost-neutral.
func TestEvaluateStrategySleevesNoopWhenPolicyDisabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 21, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-legacy").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(
				"fund-legacy", "co-1", "Legacy", nil,
				"simulation", 100000.0, 100000.0, 100000.0, 1.0, "active",
				[]byte(`{"market":"us_equity"}`),
				now, now,
			))

	fetcher := &strategyStubFetcher{bars: map[string][]ohlc.Bar{
		"NVDA": makeUptrendForStrategy(),
	}}
	agent := &runtimePMAgent{
		fundRepo:      repository.NewFundRepo(db),
		positionRepo:  repository.NewPositionRepo(db),
		ohlcFetcher:   fetcher,
		regimeService: regime.NewService(fetcher),
	}

	if got := agent.evaluateStrategySleeves(context.Background(), "fund-legacy", now); got != nil {
		t.Fatalf("expected nil on disabled policy, got %+v", got)
	}
	assertMockExpectations(t, mock)
}

func TestEvaluateStrategySleevesNilWhenWiringMissing(t *testing.T) {
	agent := &runtimePMAgent{}
	if got := agent.evaluateStrategySleeves(context.Background(), "fund-x", time.Now()); got != nil {
		t.Fatalf("expected nil on un-wired agent, got %+v", got)
	}
}
