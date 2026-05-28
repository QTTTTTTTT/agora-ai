// Integration test for runtimeApprovalGateway.RequestApproval covering
// the auto-execute fast path. Uses sqlmock to verify the exact DB
// sequence that fires when a plan is auto-approved (load fund → load
// plan → load actions → no daily-cumulative usage today → stamp
// auto_executed_at → overwrite risk_review JSON → flip plan status to
// "approved"). Companion to the pure-function tests in
// auto_execute_gate_test.go.
package main

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
	"github.com/fundai/server/internal/workflow"
)

// happy-path: fund with auto-execute enabled and all guardrails met →
// gateway stamps plan_actions.auto_executed_at, writes the audit JSON
// onto risk_review, and flips investment_plans.status to "approved".
func TestRequestApprovalAutoExecutesWhenGuardrailsPass(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	planRepo := repository.NewPlanRepo(db)
	fundRepo := repository.NewFundRepo(db)
	gw := &runtimeApprovalGateway{
		planRepo: planRepo,
		fundRepo: fundRepo,
		now:      func() time.Time { return now },
	}

	// 1) fund_repo.GetByID → AutoExecute enabled with default thresholds.
	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha", nil, "simulation", 1_000_000.0, 1_000_000.0, 1_000_000.0, 1.0, "active", []byte(`{"market":"us_equity","autoExecute":{"enabled":true,"maxOrderPctOfAssets":0.05,"maxDailyPctOfAssets":0.20,"minConfidence":0.60,"slippageBouncePolicy":"bounce_to_user"}}`), now, now))

	// 2) plan_repo.GetByID → confidence in risk_review = 0.8 (above 0.6 floor).
	mock.ExpectQuery(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", now, "pending_review", nil, nil, nil, []byte(`{"confidence":0.8}`), []byte(`{}`), nil, nil, nil, now, now))

	// 3) plan_repo.GetActions → one in-budget action (30k = 3% NAV, under 5% per-order cap).
	mock.ExpectQuery(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
		 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-1", "plan-1", "AAPL", "AAPL", "us_equity", nil, nil, nil, "buy", nil, nil, 100, 300.0, 30_000.0, nil, nil, "pm reasoning", 0.8, "{}", "{}", "pending", 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	// 4) Daily-cumulative sum lookup — no prior auto-executions today.
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	mock.ExpectQuery(`SELECT COALESCE(SUM(ABS(pa.amount)), 0)
		   FROM plan_actions pa
		   JOIN investment_plans ip ON ip.id = pa.plan_id
		  WHERE ip.fund_id = $1
		    AND pa.auto_executed_at IS NOT NULL
		    AND pa.auto_executed_at >= $2
		    AND pa.auto_executed_at < $3`).
		WithArgs("fund-1", dayStart, dayEnd).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0.0))

	// 5) StampAutoExecuted UPDATE on plan_actions.
	mock.ExpectExec(`UPDATE plan_actions SET auto_executed_at = $1 WHERE plan_id = $2`).
		WithArgs(now, "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 6) UpdateRiskReview overwrites risk_review JSON with the audit block.
	mock.ExpectExec(`UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`).
		WithArgs(sqlmock.AnyArg(), "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 7) Final status flip to approved.
	mock.ExpectExec(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`).
		WithArgs("approved", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	planResult := &workflow.InvestmentPlanResult{ID: "plan-1", FundID: "fund-1", Status: workflow.PlanStatusRiskReview}
	if err := gw.RequestApproval(context.Background(), planResult); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if planResult.Status != workflow.PlanStatusApproved {
		t.Errorf("plan.Status = %v, want approved", planResult.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// When auto-execute is disabled, the gateway must take the normal
// "pending_user" path: no stamping, no audit write, just the status
// flip. This protects existing funds that haven't enabled the feature.
func TestRequestApprovalFallsThroughWhenAutoExecuteDisabled(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	planRepo := repository.NewPlanRepo(db)
	fundRepo := repository.NewFundRepo(db)
	gw := &runtimeApprovalGateway{
		planRepo: planRepo,
		fundRepo: fundRepo,
		now:      func() time.Time { return now },
	}

	// Fund without auto-execute (legacy config).
	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha", nil, "simulation", 1_000_000.0, 1_000_000.0, 1_000_000.0, 1.0, "active", []byte(`{}`), now, now))

	// No plan/action queries — gateway short-circuits as soon as it
	// sees Enabled=false. Status flip is the only DB write.
	mock.ExpectExec(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`).
		WithArgs("pending_user", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	planResult := &workflow.InvestmentPlanResult{ID: "plan-1", FundID: "fund-1", Status: workflow.PlanStatusRiskReview}
	if err := gw.RequestApproval(context.Background(), planResult); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if planResult.Status != workflow.PlanStatusPendingUser {
		t.Errorf("plan.Status = %v, want pending_user", planResult.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// When auto-execute IS enabled but a guardrail refuses the plan
// (e.g. confidence below floor), the gateway must mark the plan
// rejected — NOT pending_user — so the daily workflow_run row is
// freed for the next slot. The old behaviour of dropping to
// pending_user caused the workflow to stall awaiting a human and
// silently skipped subsequent 30-min slots; this test pins the
// new contract so we don't regress.
func TestRequestApprovalRejectsLowConfidenceWhenAutoExecuteEnabled(t *testing.T) {
	db, mock, err := sqlmock.New(sqlmock.QueryMatcherOption(sqlmock.QueryMatcherEqual))
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 5, 22, 2, 7, 0, 0, time.UTC)
	planRepo := repository.NewPlanRepo(db)
	fundRepo := repository.NewFundRepo(db)
	gw := &runtimeApprovalGateway{
		planRepo: planRepo,
		fundRepo: fundRepo,
		now:      func() time.Time { return now },
	}

	// Fund with autoExecute enabled, minConfidence = 0.60.
	mock.ExpectQuery(`SELECT id, company_id, name, description, trading_mode, initial_capital, current_capital, total_assets, nav, status, config, created_at, updated_at
		 FROM funds WHERE id = $1`).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "company_id", "name", "description", "trading_mode", "initial_capital", "current_capital", "total_assets", "nav", "status", "config", "created_at", "updated_at"}).
			AddRow("fund-1", "company-1", "Alpha", nil, "simulation", 1_000_000.0, 1_000_000.0, 1_000_000.0, 1.0, "active", []byte(`{"market":"us_equity","autoExecute":{"enabled":true,"maxOrderPctOfAssets":0.50,"maxDailyPctOfAssets":0.50,"minConfidence":0.60,"slippageBouncePolicy":"bounce_to_user"}}`), now, now))

	// Plan with confidence = 0.55 (below floor).
	mock.ExpectQuery(`SELECT id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review, discussion_snapshot, roundtable_id, pm_agent_id, confidence, created_at, updated_at
		 FROM investment_plans WHERE id = $1`).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "fund_id", "trading_date", "status", "reasoning", "risk_score", "expected_return", "risk_review", "discussion_snapshot", "roundtable_id", "pm_agent_id", "confidence", "created_at", "updated_at"}).
			AddRow("plan-1", "fund-1", now, "pending_review", nil, nil, nil, []byte(`{"confidence":0.55}`), []byte(`{}`), nil, nil, 0.55, now, now))

	// Action in-budget so the only refusal reason is the confidence floor.
	mock.ExpectQuery(`SELECT id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, quote_refreshed_at, auto_executed_at, sleeve, regime_tag, signal_source, exit_reason, strategy
		 FROM plan_actions WHERE plan_id = $1 ORDER BY sort_order, id`).
		WithArgs("plan-1").
		WillReturnRows(sqlmock.NewRows([]string{"id", "plan_id", "instrument_key", "symbol", "market", "exchange", "asset_class", "instrument_type", "action", "position_side", "open_close", "quantity", "price", "amount", "stop_loss", "take_profit", "reasoning", "confidence", "supported_by", "opposed_by", "execution_status", "sort_order", "quote_currency", "settlement_currency", "margin_mode", "leverage", "contract_multiplier", "expiry_date", "reduce_only", "quote_refreshed_at", "auto_executed_at", "sleeve", "regime_tag", "signal_source", "exit_reason", "strategy"}).
			AddRow("action-1", "plan-1", "AAPL", "AAPL", "us_equity", nil, nil, nil, "buy", nil, nil, 100, 300.0, 30_000.0, nil, nil, "pm reasoning", 0.55, "{}", "{}", "pending", 0, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil))

	// Daily-cumulative lookup still runs (gate evaluates daily before confidence).
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	dayEnd := dayStart.Add(24 * time.Hour)
	mock.ExpectQuery(`SELECT COALESCE(SUM(ABS(pa.amount)), 0)
		   FROM plan_actions pa
		   JOIN investment_plans ip ON ip.id = pa.plan_id
		  WHERE ip.fund_id = $1
		    AND pa.auto_executed_at IS NOT NULL
		    AND pa.auto_executed_at >= $2
		    AND pa.auto_executed_at < $3`).
		WithArgs("fund-1", dayStart, dayEnd).
		WillReturnRows(sqlmock.NewRows([]string{"sum"}).AddRow(0.0))

	// Audit write (UpdateRiskReview) — gate persisted the refusal reason.
	mock.ExpectExec(`UPDATE investment_plans SET risk_review = $1, updated_at = NOW() WHERE id = $2`).
		WithArgs(sqlmock.AnyArg(), "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// Final status flip → "rejected" (NOT pending_user) because autoExecute is on.
	mock.ExpectExec(`UPDATE investment_plans SET status = $1, updated_at = NOW() WHERE id = $2`).
		WithArgs("rejected", "plan-1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	planResult := &workflow.InvestmentPlanResult{ID: "plan-1", FundID: "fund-1", Status: workflow.PlanStatusRiskReview}
	if err := gw.RequestApproval(context.Background(), planResult); err != nil {
		t.Fatalf("RequestApproval: %v", err)
	}
	if planResult.Status != workflow.PlanStatusRejected {
		t.Errorf("plan.Status = %v, want rejected (autoExecute enabled + low confidence)", planResult.Status)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}
