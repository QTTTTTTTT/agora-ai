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

// TestAgentRepoGetByIDIncludesUserID pins down the P2 root cause: the
// SELECT in GetByID used to omit user_id, so every call site that
// touched pmAgent.UserID got an empty string. That broke
// ModelRouter.ResolveModel's agentDefaults lookup — owner=""
// short-circuits straight to platform default. The schema column
// order matters: UserID is the second field of the struct, second
// column of the SELECT. If anyone ever drops user_id from the
// projection again, this test will fail.
func TestAgentRepoGetByIDIncludesUserID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	repo := NewAgentRepo(db)
	now := time.Now().UTC()
	rows := sqlmock.NewRows([]string{
		"id", "user_id", "name", "role", "focus", "llm_model", "model_provider", "model_name",
		"system_prompt", "skill_config", "domain_config", "evolution_config",
		"pending_marketplace_snapshot", "marketplace_snapshot_imported_at", "status", "created_at", "updated_at",
	}	).AddRow(
		"agent-1", "user-tong", "Portfolio Manager", "pm", nil, nil, "claude", "claude-sonnet-4-20250514",
		nil, []byte("{}"), []byte("{}"), []byte("{}"),
		[]byte("{}"), nil, "active", now, now,
	)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT id, user_id, name, role, focus, llm_model, model_provider, model_name, system_prompt, skill_config, domain_config, evolution_config, pending_marketplace_snapshot, marketplace_snapshot_imported_at, status, created_at, updated_at")).
		WithArgs("agent-1").
		WillReturnRows(rows)

	agent, err := repo.GetByID(context.Background(), "agent-1")
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if agent.UserID != "user-tong" {
		t.Fatalf("agent.UserID = %q, want user-tong (P2 routing depends on this)", agent.UserID)
	}
	if agent.ModelProvider.String != "claude" {
		t.Fatalf("agent.ModelProvider = %q, want claude", agent.ModelProvider.String)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("sqlmock expectations: %v", err)
	}
}

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

// TestApplyMemoryDefaultsBackfillsCheckConstraintFields locks the
// invariant that MemoryRepo.Create cannot send an empty string for
// any field with a DB CHECK constraint, even if the caller forgot to
// set it. The original bug: runtimeMemorySystem.writeLearningMemory
// built Memory{} without OriginKind so every daily_review attempt for
// every fund silently failed with
//   "violates check constraint memories_origin_kind_check (23514)"
// for 3+ days before someone audited the memories table and noticed
// "agent learning" was a no-op end-to-end.
//
// The DB has DEFAULT 'native' on origin_kind, but because the INSERT
// in MemoryRepo.Create lists every column explicitly, passing '' as
// the parameter value beats the DEFAULT to the check constraint.
// Centralising the default in the repo (applyMemoryDefaults) is the
// last line of defence — any new caller that forgets a CHECK field
// now still produces a valid row.
func TestApplyMemoryDefaultsBackfillsCheckConstraintFields(t *testing.T) {
	t.Run("nil memory is a safe no-op", func(t *testing.T) {
		applyMemoryDefaults(nil) // must not panic
	})

	t.Run("empty CHECK fields get filled with DB-compatible defaults", func(t *testing.T) {
		m := &Memory{FundID: "fund-1", Layer: "daily", Content: "{}"}
		applyMemoryDefaults(m)
		if m.OriginKind != "native" {
			t.Errorf("OriginKind: expected 'native', got %q (this is the wedge — empty string violates memories_origin_kind_check)", m.OriginKind)
		}
		if m.Visibility != "private" {
			t.Errorf("Visibility: expected 'private', got %q", m.Visibility)
		}
		if m.Sensitivity != "internal" {
			t.Errorf("Sensitivity: expected 'internal', got %q", m.Sensitivity)
		}
	})

	t.Run("whitespace-only fields treated as empty", func(t *testing.T) {
		m := &Memory{OriginKind: "   ", Visibility: "\t", Sensitivity: " "}
		applyMemoryDefaults(m)
		if m.OriginKind != "native" || m.Visibility != "private" || m.Sensitivity != "internal" {
			t.Fatalf("whitespace must be treated as empty, got origin=%q vis=%q sens=%q", m.OriginKind, m.Visibility, m.Sensitivity)
		}
	})

	t.Run("explicit values are preserved", func(t *testing.T) {
		m := &Memory{
			OriginKind:  "imported_from_marketplace",
			Visibility:  "marketplace",
			Sensitivity: "public",
		}
		applyMemoryDefaults(m)
		if m.OriginKind != "imported_from_marketplace" {
			t.Errorf("OriginKind: explicit value clobbered, got %q", m.OriginKind)
		}
		if m.Visibility != "marketplace" {
			t.Errorf("Visibility: explicit value clobbered, got %q", m.Visibility)
		}
		if m.Sensitivity != "public" {
			t.Errorf("Sensitivity: explicit value clobbered, got %q", m.Sensitivity)
		}
	})
}

// TestMemoryRepoCreateFillsDefaultsBeforeInsert pins the SQL-level
// behaviour: even if the caller hands Create a Memory with empty
// CHECK fields, the INSERT must carry the backfilled defaults so
// PostgreSQL doesn't reject the row. We assert the parameter VALUES
// directly via sqlmock — if the defaults aren't applied, the mock
// expectation will fail with "actual call was missing".
func TestMemoryRepoCreateFillsDefaultsBeforeInsert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	defer db.Close()

	repo := NewMemoryRepo(db)
	in := &Memory{
		FundID:      "fund-1",
		Layer:       "daily",
		Content:     `{"summary":"ok"}`,
		TradingDate: sql.NullTime{Time: time.Date(2026, 5, 22, 0, 0, 0, 0, time.UTC), Valid: true},
	}

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO memories (fund_id, agent_id, owner_user_id, visibility, sensitivity, origin_kind, source_listing_id, layer, title, content, trading_date, tags)`)).
		WithArgs(
			"fund-1",                        // fund_id
			sql.NullString{},                // agent_id
			sql.NullString{},                // owner_user_id
			"private",                       // visibility ← backfilled
			"internal",                      // sensitivity ← backfilled
			"native",                        // origin_kind ← backfilled (the bug fix)
			sql.NullString{},                // source_listing_id
			"daily",                         // layer
			sql.NullString{},                // title
			`{"summary":"ok"}`,              // content
			in.TradingDate,                  // trading_date
			sqlmock.AnyArg(),                // tags (pq.Array wrapped, opaque)
		).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("memory-1"))

	id, err := repo.Create(context.Background(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "memory-1" {
		t.Fatalf("expected id 'memory-1', got %q", id)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
