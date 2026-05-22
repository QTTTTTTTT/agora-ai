package main

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// TestBuildPlanActionsDemotesT1IntradayBuyToHold reproduces the
// "tong's OCS fund bought 600519 today but PM still recommends sell"
// scenario reported by the user:
//   - The fund holds 200 shares of an A-share symbol (600519, T+1).
//   - The trade_executions table shows all 200 shares were filled
//     today during the current trading session.
//   - PM must therefore down-grade the proposed `reduce` action to
//     `hold` and NOT propose any sell qty (because all 200 shares are
//     still settling).
//
// The test mocks the two queries buildPlanActions issues against the
// DB on the holdings branch (positions + intraday-buy sums) and then
// inspects the returned actions slice.
func TestBuildPlanActionsDemotesT1IntradayBuyToHold(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(2 * time.Hour)

	// Fund lookup (called early in buildPlanActions).
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-ocs", "company-1", "OCS Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"a_share","assetClass":"equity","primaryDirection":"stocks"}`), now, now))

	// PM agent lookup (team/agent queries) — return empty team so the
	// skill-context path no-ops.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}))

	// Positions: one A-share holding, 200 shares of 600519.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-ocs", "SH:600519", "600519", "贵州茅台",
			sql.NullString{String: "a_share", Valid: true}, sql.NullString{String: "SSE", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
			200.0, 0.0, 1700.0, 1700.0, 340000.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))

	// Intraday buy aggregation: 200 shares of 600519 filled today.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`)).
		WithArgs("fund-ocs", tradingDate, tradingDate.Add(24*time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}).
			AddRow("SH:600519", "600519", 200.0))

	agent := &runtimePMAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
	}
	actions, _, err := agent.buildPlanActions(context.Background(), "fund-ocs", tradingDate, &workflow.RoundtableResult{
		Consensus: []string{"白酒板块今日反弹，但短期估值偏高，可考虑止盈一部分仓位"},
	})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]

	if got.Action != "hold" {
		t.Errorf("Action = %q, want %q (PM must demote when intraday buy locks the full position)", got.Action, "hold")
	}
	if got.Quantity.Valid {
		t.Errorf("Quantity should be invalid for a hold action; got %v", got.Quantity.Float64)
	}
	if got.Amount.Valid {
		t.Errorf("Amount should be invalid for a hold action; got %v", got.Amount.Float64)
	}
	if !strings.Contains(got.Reasoning.String, "A股市场 T+1") {
		t.Errorf("Reasoning should frame T+1 as a market rule; got %q", got.Reasoning.String)
	}
	// The note must phrase the lock as a market rule, not as a
	// symbol-specific property — so the symbol code shouldn't appear
	// in the appended T+1 segment. We check the whole reasoning for
	// "600519 is T+1" / "600519 T+1" framings that would slip back.
	if strings.Contains(got.Reasoning.String, "600519 is T+1") || strings.Contains(got.Reasoning.String, "600519 T+1") {
		t.Errorf("Reasoning must not present T+1 as a per-symbol property; got %q", got.Reasoning.String)
	}
	if !strings.Contains(got.Reasoning.String, "200") {
		t.Errorf("Reasoning should mention the locked qty (200); got %q", got.Reasoning.String)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestBuildPlanActionsPartialT1LockReducesOnlyAvailable verifies the
// in-between case: of 1000 held shares, only 400 were bought today
// (still locked). PM should propose `reduce` for the unlocked 600 and
// surface a reasoning note about the locked 400.
func TestBuildPlanActionsPartialT1LockReducesOnlyAvailable(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(3 * time.Hour)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-ocs", "company-1", "OCS Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"a_share"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-ocs").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-ocs", "SH:600519", "600519", "贵州茅台",
			sql.NullString{String: "a_share", Valid: true}, sql.NullString{String: "SSE", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
			1000.0, 600.0, 1700.0, 1700.0, 1700000.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`)).
		WithArgs("fund-ocs", tradingDate, tradingDate.Add(24*time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}).
			AddRow("SH:600519", "600519", 400.0))

	agent := &runtimePMAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
	}
	actions, _, err := agent.buildPlanActions(context.Background(), "fund-ocs", tradingDate, &workflow.RoundtableResult{
		Consensus: []string{"短期止盈"},
	})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "reduce" {
		t.Errorf("Action = %q, want %q", got.Action, "reduce")
	}
	if !got.Quantity.Valid || got.Quantity.Float64 != 600 {
		t.Errorf("Quantity = %v (valid=%t), want 600", got.Quantity.Float64, got.Quantity.Valid)
	}
	if !strings.Contains(got.Reasoning.String, "A股市场 T+1") {
		t.Errorf("Reasoning should frame T+1 as a market rule; got %q", got.Reasoning.String)
	}
	if !strings.Contains(got.Reasoning.String, "400") {
		t.Errorf("Reasoning should mention locked qty 400; got %q", got.Reasoning.String)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// TestExtractRiskNotesAggregatesT1AsMarketRule verifies that the
// plan-level reasoning generator collapses multiple per-action T+1
// notes into a single market-level reminder. Reviewers see "A 股
// market T+1 rule applies to N positions", not N copies of
// "symbol X is T+1".
func TestExtractRiskNotesAggregatesT1AsMarketRule(t *testing.T) {
	actions := []repository.PlanAction{
		{Symbol: "600519", Action: "hold", Reasoning: nullStringFromValue("止盈 | A股市场 T+1 结算规则：今日新买 100 股需待下一交易日方可卖出")},
		{Symbol: "300750", Action: "hold", Reasoning: nullStringFromValue("减仓 | A股市场 T+1 结算规则：今日新买 200 股需待下一交易日方可卖出")},
		{Symbol: "688205", Action: "reduce", Reasoning: nullStringFromValue("normal reduce without lock note")},
	}
	notes := extractRiskNotesFromActions(actions)
	matches := 0
	for _, n := range notes {
		if strings.Contains(n, "A 股市场 T+1 结算规则生效") {
			matches++
		}
	}
	if matches != 1 {
		t.Errorf("expected exactly one aggregated T+1 note, got %d: %v", matches, notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "A 股市场 T+1 结算规则生效") {
			if !strings.Contains(n, "2 个") {
				t.Errorf("aggregated note should report 2 positions locked, got %q", n)
			}
		}
	}
}

func nullStringFromValue(v string) sql.NullString {
	return sql.NullString{String: v, Valid: v != ""}
}

// TestBuildPlanActionsT0MarketIgnoresIntradayBuys confirms that on
// T+0 markets (US equity, crypto) the intraday-buy signal is ignored
// — the full position remains sellable for the day's plan.
func TestBuildPlanActionsT0MarketIgnoresIntradayBuys(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(1 * time.Hour)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-us").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-us", "company-1", "US Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"us_stock"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-us").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-us").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-us", "NASDAQ:NVDA", "NVDA", "NVIDIA",
			sql.NullString{String: "us_stock", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
			100.0, 100.0, 800.0, 1000.0, 100000.0, 1.0,
			sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
		))
	// Even though there's an intraday buy on NVDA, T+0 means it's
	// immediately sellable. SellableQtyToday returns the full Quantity.
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`)).
		WithArgs("fund-us", tradingDate, tradingDate.Add(24*time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}).
			AddRow("NASDAQ:NVDA", "NVDA", 50.0))

	agent := &runtimePMAgent{
		planRepo:     repository.NewPlanRepo(db),
		fundRepo:     repository.NewFundRepo(db),
		positionRepo: repository.NewPositionRepo(db),
		teamRepo:     repository.NewTeamRepo(db),
		agentRepo:    repository.NewAgentRepo(db),
		tradeRepo:    repository.NewTradeRepo(db),
	}
	actions, _, err := agent.buildPlanActions(context.Background(), "fund-us", tradingDate, &workflow.RoundtableResult{
		Consensus: []string{"take profits"},
	})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "reduce" {
		t.Errorf("Action = %q, want %q", got.Action, "reduce")
	}
	if !got.Quantity.Valid || got.Quantity.Float64 != 100 {
		t.Errorf("Quantity = %v, want full 100 for T+0", got.Quantity.Float64)
	}
	if strings.Contains(got.Reasoning.String, "A股市场 T+1") {
		t.Errorf("T+0 reasoning must not mention A-share T+1 rule; got %q", got.Reasoning.String)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
