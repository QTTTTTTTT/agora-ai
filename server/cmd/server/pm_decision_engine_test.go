// Phase 2A integration tests for the LLM-driven decision engine path
// in runtimePMAgent.buildPlanActions. These tests exercise the wiring
// layer end-to-end (sqlmock for DB + stub DecisionEngine for LLM) and
// verify:
//
//  1. When the engine returns a structured plan, buildPlanActions
//     translates it into repository.PlanAction with the engine's
//     reasoning + confidence + a "decision_engine" SupportedBy tag.
//  2. T+1 / lot-size / sellable-today normalisations are still
//     enforced on top of the engine's recommendation — a "reduce 100%"
//     against an A-share position with the full lot bought today
//     demotes to "hold".
//  3. When the engine returns an error (network failure, malformed
//     JSON) the agent transparently falls back to the deterministic
//     legacy heuristic so the workflow still completes.
//
// Together these guard the two contracts the Phase 1 auto-execute
// gate depends on: plan.confidence is populated from the LLM, and the
// fallback never auto-executes (confidence 0.55, below the 0.60 floor).
package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/decision"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// stubDecisionEngine satisfies decision.DecisionEngine with a fixed
// (output, error) tuple. Tests use it to simulate the LLM without a
// real provider.
type stubDecisionEngine struct {
	out *decision.DecisionOutput
	err error
}

func (s *stubDecisionEngine) Decide(_ context.Context, _ decision.DecisionInput) (*decision.DecisionOutput, error) {
	return s.out, s.err
}

// mockPMAgentDBExpectations seeds sqlmock with the four DB lookups
// runtimePMAgent.buildPlanActions performs before consulting the
// decision engine: fund + team + positions + intraday buy sums.
func mockPMAgentDBExpectations(mock sqlmock.Sqlmock, fundID string, config []byte, positionsRow []driver.Value, tradingDate, now time.Time) {
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow(fundID, "company-1", "Test Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", config, now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs(fundID).
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}))
	if positionsRow != nil {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
			WithArgs(fundID).
			WillReturnRows(sqlmock.NewRows([]string{
				"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
			}).AddRow(positionsRow...))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`)).
			WithArgs(fundID, tradingDate, tradingDate.Add(24*time.Hour)).
			WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}))
	} else {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
			WithArgs(fundID).
			WillReturnRows(sqlmock.NewRows([]string{
				"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
			}))
	}
}

// Happy path: LLM returns a reduce action; the wiring layer
// translates it into a PlanAction tagged with decision_engine.
func TestBuildPlanActionsTranslatesLLMReduceAction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(2 * time.Hour)

	positionsRow := []driver.Value{
		"pos-1", "fund-1", "NASDAQ:NVDA", "NVDA", "NVIDIA",
		sql.NullString{String: "us_stock", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
		sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
		sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
		100.0, 100.0, 800.0, 1000.0, 100000.0, 1.0,
		sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
	}
	mockPMAgentDBExpectations(mock, "fund-1", []byte(`{"market":"us_stock"}`), positionsRow, tradingDate, now)

	engine := &stubDecisionEngine{out: &decision.DecisionOutput{
		Confidence: 0.82,
		Stance:     "trim NVDA on overextension",
		Actions: []decision.DecisionAction{{
			Symbol:     "NVDA",
			Action:     "reduce",
			QtyPct:     0.3,
			Reasoning:  "RSI 78 on weekly + earnings ahead",
			Confidence: 0.82,
		}},
	}}

	agent := &runtimePMAgent{
		planRepo:       repository.NewPlanRepo(db),
		fundRepo:       repository.NewFundRepo(db),
		positionRepo:   repository.NewPositionRepo(db),
		teamRepo:       repository.NewTeamRepo(db),
		agentRepo:      repository.NewAgentRepo(db),
		tradeRepo:      repository.NewTradeRepo(db),
		decisionEngine: engine,
	}
	actions, planConfidence, err := agent.buildPlanActions(context.Background(), "fund-1", tradingDate, &workflow.RoundtableResult{
		Consensus: []string{"NVDA at all-time highs, take some off the table"},
	})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if planConfidence < 0.81 || planConfidence > 0.83 {
		t.Errorf("planConfidence = %v, want ~0.82 (from LLM)", planConfidence)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "reduce" {
		t.Errorf("Action = %q, want reduce", got.Action)
	}
	if !got.Quantity.Valid || got.Quantity.Float64 != 30 {
		t.Errorf("Quantity = %v (valid=%t), want 30 (= floor(100 * 0.3))", got.Quantity.Float64, got.Quantity.Valid)
	}
	if !strings.Contains(got.Reasoning.String, "RSI 78") {
		t.Errorf("Reasoning should carry the LLM rationale; got %q", got.Reasoning.String)
	}
	found := false
	for _, s := range got.SupportedBy {
		if s == "decision_engine" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SupportedBy should include 'decision_engine'; got %v", got.SupportedBy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// T+1 safety: the engine recommends a reduce against an A-share
// position whose full lot was bought intraday. The translation
// layer must demote to "hold" with the market-rule note, just as
// the legacy path does. This proves the LLM cannot bypass T+1
// just by asserting confidence.
func TestBuildPlanActionsLLMReduceDemotesToHoldWhenT1Locked(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(2 * time.Hour)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
				 FROM funds WHERE id = $1`)).
		WithArgs("fund-cn").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-cn", "company-1", "CN Fund", nil, "simulation", 100000.0, 100000.0, 100000.0, 1.0, "active", []byte(`{"market":"a_share","assetClass":"equity"}`), now, now))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, agent_id, role, focus, joined_at, status, updated_at
			 FROM fund_team_members WHERE fund_id = $1 ORDER BY joined_at`)).
		WithArgs("fund-cn").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "agent_id", "role", "focus", "joined_at", "status", "updated_at"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side, quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value, weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used, updated_at
		 FROM holding_positions WHERE fund_id = $1 ORDER BY instrument_key`)).
		WithArgs("fund-cn").
		WillReturnRows(sqlmock.NewRows([]string{
			"id", "fund_id", "instrument_key", "symbol", "name", "market", "exchange", "asset_class", "instrument_type", "position_side", "quote_currency", "settlement_currency", "margin_mode", "quantity", "available_qty", "cost_price", "current_price", "market_value", "weight", "leverage", "contract_multiplier", "expiry_date", "unrealized_pnl", "margin_used", "updated_at",
		}).AddRow(
			"pos-1", "fund-cn", "SH:600519", "600519", "贵州茅台",
			sql.NullString{String: "a_share", Valid: true}, sql.NullString{String: "SSE", Valid: true},
			sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
			sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
			200.0, 0.0, 1700.0, 1700.0, 340000.0, 1.0,
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
		WithArgs("fund-cn", tradingDate, tradingDate.Add(24*time.Hour)).
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}).
			AddRow("SH:600519", "600519", 200.0))

	engine := &stubDecisionEngine{out: &decision.DecisionOutput{
		Confidence: 0.9,
		Actions: []decision.DecisionAction{{
			Symbol:     "600519",
			Action:     "reduce",
			QtyPct:     1.0,
			Reasoning:  "高估值，建议大幅减仓",
			Confidence: 0.9,
		}},
	}}
	agent := &runtimePMAgent{
		planRepo:       repository.NewPlanRepo(db),
		fundRepo:       repository.NewFundRepo(db),
		positionRepo:   repository.NewPositionRepo(db),
		teamRepo:       repository.NewTeamRepo(db),
		agentRepo:      repository.NewAgentRepo(db),
		tradeRepo:      repository.NewTradeRepo(db),
		decisionEngine: engine,
	}
	actions, _, err := agent.buildPlanActions(context.Background(), "fund-cn", tradingDate, &workflow.RoundtableResult{Consensus: []string{"短期止盈"}})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "hold" {
		t.Errorf("Action = %q, want hold (T+1 locked + LLM said reduce)", got.Action)
	}
	if got.Quantity.Valid {
		t.Errorf("Quantity should be invalid for hold; got %v", got.Quantity.Float64)
	}
	if !strings.Contains(got.Reasoning.String, "A股市场 T+1") {
		t.Errorf("Reasoning should still surface the T+1 market rule even when the LLM ignored it; got %q", got.Reasoning.String)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// Fallback path: the engine returns an error (provider down,
// malformed JSON, quota exceeded). buildPlanActions silently
// switches to the deterministic legacy heuristic, returns
// confidence 0.55 (below the auto-execute 0.60 floor), and still
// emits the expected reduce action on the held position.
func TestBuildPlanActionsFallsBackWhenDecisionEngineFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(2 * time.Hour)

	positionsRow := []driver.Value{
		"pos-1", "fund-1", "NASDAQ:AAPL", "AAPL", "Apple",
		sql.NullString{String: "us_stock", Valid: true}, sql.NullString{String: "NASDAQ", Valid: true},
		sql.NullString{String: "equity", Valid: true}, sql.NullString{String: "stock", Valid: true},
		sql.NullString{}, sql.NullString{}, sql.NullString{}, sql.NullString{},
		50.0, 50.0, 150.0, 180.0, 9000.0, 0.09,
		sql.NullFloat64{}, sql.NullFloat64{}, sql.NullTime{}, sql.NullFloat64{}, sql.NullFloat64{}, now,
	}
	mockPMAgentDBExpectations(mock, "fund-1", []byte(`{"market":"us_stock"}`), positionsRow, tradingDate, now)

	engine := &stubDecisionEngine{err: errors.New("provider rate-limited")}
	agent := &runtimePMAgent{
		planRepo:       repository.NewPlanRepo(db),
		fundRepo:       repository.NewFundRepo(db),
		positionRepo:   repository.NewPositionRepo(db),
		teamRepo:       repository.NewTeamRepo(db),
		agentRepo:      repository.NewAgentRepo(db),
		tradeRepo:      repository.NewTradeRepo(db),
		decisionEngine: engine,
	}
	actions, planConfidence, err := agent.buildPlanActions(context.Background(), "fund-1", tradingDate, &workflow.RoundtableResult{
		Consensus: []string{"take some profits on tech"},
	})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if planConfidence > 0.6 {
		t.Errorf("planConfidence = %v should stay below the auto-execute floor (0.60) when fallback fires", planConfidence)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 fallback action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "reduce" {
		t.Errorf("legacy heuristic should still reduce the only holding; got %q", got.Action)
	}
	for _, s := range got.SupportedBy {
		if s == "decision_engine" {
			t.Errorf("fallback path must NOT tag actions with decision_engine; got %v", got.SupportedBy)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

// Buy translation: LLM returns a buy against a fund without
// positions; the wiring layer hits the watch fallback because no
// quote provider is wired in the test. Quote-unavailable contract
// (production-grade as of 2026-06-03): the action is DOWNGRADED
// to "watch" — we no longer synthesise a buy with the budget
// stamped into the price, which is what produced the 96,226 CNY/
// share 301308 fill on 2026-06-02. The action still carries the
// decision_engine tag + the LLM rationale so the Decision Center
// surfaces the deferred intent.
func TestBuildPlanActionsTranslatesLLMBuyAction(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	now := tradingDate.Add(2 * time.Hour)
	mockPMAgentDBExpectations(mock, "fund-1", []byte(`{"market":"us_stock","universe":{"symbols":["AAPL"]}}`), nil, tradingDate, now)

	engine := &stubDecisionEngine{out: &decision.DecisionOutput{
		Confidence: 0.78,
		Actions: []decision.DecisionAction{{
			Symbol:     "AAPL",
			Action:     "buy",
			QtyPct:     0.05,
			Reasoning:  "expanding services revenue",
			Confidence: 0.78,
		}},
	}}
	agent := &runtimePMAgent{
		planRepo:       repository.NewPlanRepo(db),
		fundRepo:       repository.NewFundRepo(db),
		positionRepo:   repository.NewPositionRepo(db),
		teamRepo:       repository.NewTeamRepo(db),
		agentRepo:      repository.NewAgentRepo(db),
		tradeRepo:      repository.NewTradeRepo(db),
		decisionEngine: engine,
	}
	actions, confidence, err := agent.buildPlanActions(context.Background(), "fund-1", tradingDate, &workflow.RoundtableResult{})
	if err != nil {
		t.Fatalf("buildPlanActions: %v", err)
	}
	if confidence < 0.77 || confidence > 0.79 {
		t.Errorf("confidence = %v, want ~0.78", confidence)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	got := actions[0]
	if got.Action != "watch" {
		t.Errorf("Action = %q, want watch (quote-unavailable must downgrade — see 96,226 CNY/share regression on 2026-06-02)", got.Action)
	}
	if got.Price.Valid {
		t.Errorf("Price.Valid=true (=%v) on quote-unavailable; must NOT synthesise a price from the budget", got.Price.Float64)
	}
	if got.Quantity.Valid {
		t.Errorf("Quantity.Valid=true (=%v) on watch action; must be unset", got.Quantity.Float64)
	}
	if got.Symbol != "AAPL" {
		t.Errorf("Symbol = %q, want AAPL", got.Symbol)
	}
	if !strings.Contains(got.Reasoning.String, "expanding services revenue") {
		t.Errorf("Reasoning should carry LLM rationale; got %q", got.Reasoning.String)
	}
	if !strings.Contains(got.Reasoning.String, "quote unavailable") {
		t.Errorf("Reasoning should explain the downgrade; got %q", got.Reasoning.String)
	}
	found := false
	for _, s := range got.SupportedBy {
		if s == "decision_engine" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("SupportedBy should include decision_engine; got %v", got.SupportedBy)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}
