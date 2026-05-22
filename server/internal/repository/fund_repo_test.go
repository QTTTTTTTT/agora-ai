package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestPlanRepoCreateWithActionsRollsBackOnActionInsertFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := NewPlanRepo(db)
	plan := &InvestmentPlan{
		FundID:         "fund-1",
		TradingDate:    time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC),
		Status:         "pending_user",
		Reasoning:      sql.NullString{String: "reasoning", Valid: true},
		RiskScore:      sql.NullFloat64{Float64: 0, Valid: true},
		ExpectedReturn: sql.NullFloat64{Float64: 0, Valid: true},
	}
	actions := []PlanAction{{
		InstrumentKey:   "NASDAQ:NVDA",
		Symbol:          "NVDA",
		Action:          "buy",
		Quantity:        sql.NullFloat64{Float64: 1, Valid: true},
		Price:           sql.NullFloat64{Float64: 25000, Valid: true},
		Amount:          sql.NullFloat64{Float64: 25000, Valid: true},
		Reasoning:       sql.NullString{String: "quote unavailable; plan keeps a buy action and will refresh pricing before execution", Valid: true},
		ExecutionStatus: "pending",
	}}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO investment_plans (fund_id, trading_date, status, reasoning, risk_score, expected_return, roundtable_id, pm_agent_id, risk_review, discussion_snapshot, confidence)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		 RETURNING id`)).
		WithArgs("fund-1", plan.TradingDate, "pending_user", plan.Reasoning, plan.RiskScore, plan.ExpectedReturn, plan.RoundtableID, plan.PMAgentID, []byte(`null`), []byte(`{}`), plan.Confidence).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("plan-1"))
	prepare := mock.ExpectPrepare(regexp.QuoteMeta(`INSERT INTO plan_actions (plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close, quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only, sleeve, regime_tag, signal_source, exit_reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)`))
	prepare.ExpectExec().
		WithArgs("plan-1", "NASDAQ:NVDA", "NVDA", actions[0].Market, actions[0].Exchange, actions[0].AssetClass, actions[0].InstrumentType, "buy", actions[0].PositionSide, actions[0].OpenClose, actions[0].Quantity, actions[0].Price, actions[0].Amount, actions[0].StopLoss, actions[0].TakeProfit, actions[0].Reasoning, actions[0].Confidence, sqlmock.AnyArg(), sqlmock.AnyArg(), "pending", 0, actions[0].QuoteCurrency, actions[0].SettlementCurrency, actions[0].MarginMode, actions[0].Leverage, actions[0].ContractMultiplier, actions[0].ExpiryDate, actions[0].ReduceOnly, actions[0].Sleeve, actions[0].RegimeTag, actions[0].SignalSource, actions[0].ExitReason).
		WillReturnError(errors.New("boom"))
	mock.ExpectRollback()

	_, err = repo.CreateWithActions(context.Background(), plan, actions)
	if err == nil {
		t.Fatal("expected create with actions to fail")
	}
	if !strings.Contains(err.Error(), "plan_repo: insert action") {
		t.Fatalf("expected action insert error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestMergeWorkflowRunPreservesTerminalCompletionAndStepHistory(t *testing.T) {
	existingDate := time.Date(2026, time.May, 14, 0, 0, 0, 0, time.UTC)
	completedAt := time.Date(2026, time.May, 14, 15, 30, 0, 0, time.UTC)
	existing := &WorkflowRun{
		ID:          "run-1",
		FundID:      "fund-1",
		TradingDate: existingDate,
		Status:      "completed",
		CurrentStep: sql.NullString{String: "daily_review", Valid: true},
		StepResults: json.RawMessage(`{"macro_brief":{"status":"success"},"daily_review":{"status":"success"}}`),
		StartedAt:   sql.NullTime{Time: existingDate.Add(9 * time.Hour), Valid: true},
		CompletedAt: sql.NullTime{Time: completedAt, Valid: true},
	}
	incoming := &WorkflowRun{
		FundID:      "fund-1",
		TradingDate: existingDate,
		Status:      "running",
		CurrentStep: sql.NullString{String: "settlement", Valid: true},
		StepResults: json.RawMessage(`{"settlement":{"status":"success"}}`),
	}

	merged, err := mergeWorkflowRun(existing, incoming)
	if err != nil {
		t.Fatalf("merge workflow run: %v", err)
	}
	if merged.Status != "completed" {
		t.Fatalf("expected terminal status to be preserved, got %q", merged.Status)
	}
	if !merged.CompletedAt.Valid || !merged.CompletedAt.Time.Equal(completedAt) {
		t.Fatalf("expected completed_at to be preserved, got %#v", merged.CompletedAt)
	}
	var steps map[string]map[string]string
	if err := json.Unmarshal(merged.StepResults, &steps); err != nil {
		t.Fatalf("unmarshal merged step results: %v", err)
	}
	if steps["macro_brief"]["status"] != "success" || steps["daily_review"]["status"] != "success" || steps["settlement"]["status"] != "success" {
		t.Fatalf("expected merged step results, got %#v", steps)
	}
}

func TestSumFilledBuyTodayByInstrumentAggregatesPerKey(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := NewTradeRepo(db)
	tradingDate := time.Date(2026, time.May, 20, 0, 0, 0, 0, time.UTC)
	end := tradingDate.Add(24 * time.Hour)

	rows := sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}).
		AddRow("SH:600519", "600519", 400.0).
		AddRow("SH:601318", "601318", 100.0).
		AddRow("", "688205", 50.0)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			   AND created_at >= $2
			   AND created_at <  $3
			 GROUP BY instrument_key, symbol`)).
		WithArgs("fund-1", tradingDate, end).
		WillReturnRows(rows)

	got, err := repo.SumFilledBuyTodayByInstrument(context.Background(), "fund-1", tradingDate)
	if err != nil {
		t.Fatalf("SumFilledBuyTodayByInstrument: %v", err)
	}
	if v := got["SH:600519"]; v != 400 {
		t.Errorf("600519 sum = %v, want 400", v)
	}
	if v := got["SH:601318"]; v != 100 {
		t.Errorf("601318 sum = %v, want 100", v)
	}
	// Empty instrument_key should fall back to symbol as the key.
	if v := got["688205"]; v != 50 {
		t.Errorf("688205 sum = %v, want 50", v)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestSumFilledBuyTodayByInstrumentZeroDateSkipsBound(t *testing.T) {
	// A zero tradingDate disables the date filter — used by callers
	// that don't care about the boundary (some tests, debugging).
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := NewTradeRepo(db)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT instrument_key, symbol, COALESCE(SUM(GREATEST(filled_qty, quantity)), 0)
			 FROM trade_executions
			 WHERE fund_id = $1
			   AND side = 'buy'
			   AND status = 'filled'
			 GROUP BY instrument_key, symbol`)).
		WithArgs("fund-1").
		WillReturnRows(sqlmock.NewRows([]string{"instrument_key", "symbol", "sum"}))

	if _, err := repo.SumFilledBuyTodayByInstrument(context.Background(), "fund-1", time.Time{}); err != nil {
		t.Fatalf("SumFilledBuyTodayByInstrument: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
