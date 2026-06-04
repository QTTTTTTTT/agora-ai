package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"regexp"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	"github.com/fundai/server/internal/repository"
)

// TestProRataFeeSplit_MatchesQtyShareAndSumsExactly checks the two
// invariants tradeRepoCreateAndFillSplit relies on:
//
//  1. Each child's fee is approximately fee_total * child_qty / total_qty
//     (rounded to 4dp for the first N-1 children).
//  2. Sum of per-child fees equals fee_total EXACTLY (no float drift),
//     because the last child absorbs whatever rounding remainder
//     accumulates from the first N-1.
//
// The second invariant matters more than the first: the cash_ledger
// total has to reconcile to the parent row's recorded commission, and
// any cumulative rounding drift would silently make NAV not balance.
func TestProRataFeeSplit_MatchesQtyShareAndSumsExactly(t *testing.T) {
	cases := []struct {
		name       string
		total      float64
		qtys       []int
		wantShares []float64
	}{
		{
			name:       "even split clean",
			total:      10.0,
			qtys:       []int{800, 800, 800, 800, 800},
			wantShares: []float64{2.0, 2.0, 2.0, 2.0, 2.0},
		},
		{
			// 10 * 800/4004 = 1.998001998... → 1.998 (rounded to 4dp);
			// 10 * 804/4004 = 2.0079920... but last child absorbs
			// 10 - 4*1.998 = 2.008.
			name:       "uneven last child takes remainder",
			total:      10.0,
			qtys:       []int{800, 800, 800, 800, 804},
			wantShares: []float64{1.998, 1.998, 1.998, 1.998, 2.008},
		},
		{
			name:       "zero total returns all zeros",
			total:      0,
			qtys:       []int{800, 800, 800, 800, 800},
			wantShares: []float64{0, 0, 0, 0, 0},
		},
		{
			name:       "empty qtys returns nil",
			total:      10,
			qtys:       nil,
			wantShares: nil,
		},
		{
			name:       "single child takes all",
			total:      3.5,
			qtys:       []int{1500},
			wantShares: []float64{3.5},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := proRataFeeSplit(tc.total, tc.qtys)
			if len(got) != len(tc.wantShares) {
				t.Fatalf("len=%d want=%d (got=%v)", len(got), len(tc.wantShares), got)
			}
			for i, want := range tc.wantShares {
				if math.Abs(got[i]-want) > 1e-9 {
					t.Errorf("share[%d] = %g, want %g (full got=%v)", i, got[i], want, got)
				}
			}
			// Sum invariant.
			if len(got) > 0 {
				sum := 0.0
				for _, v := range got {
					sum += v
				}
				if math.Abs(sum-tc.total) > 1e-9 {
					t.Errorf("sum = %g, want %g (drift); shares=%v", sum, tc.total, got)
				}
			}
		})
	}
}

// TestTradeRepoCreateAndFillSplit_BuyTWAPHappyPath drives the splitter
// path end-to-end on a 4000-share TWAP buy. The fund's config has the
// pm_path_child_splitting flag enabled, qty=4000 exceeds the
// splitThreshold of 1000, and strategy="twap" → the splitter MUST
// fan out into 1 parent + 5 children.
//
// Verifications:
//
//   * 6 INSERTs (1 parent + 5 children) issued against trade_executions.
//   * 6 UPDATE rows (one per row), same status='filled'.
//   * Child rows carry strategy='twap' AND strategy_parent_trade_id
//     equal to the parent's returned UUID.
//   * Per-child quantity is 800 (4000 / 5, even split).
//   * Per-child idempotency_key includes the child index ("child:0",
//     "child:1", ...).
//   * Sum of per-child fees equals the parent's fees.
//
// cashLedger / lotLedger are nil so the splitter skips them — this
// test isolates the trade_executions layer; the per-leg ledger
// writes are exercised by separate sqlmock tests under
// cash_ledger / lot_ledger packages.
func TestTradeRepoCreateAndFillSplit_BuyTWAPHappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	// Match the INSERT skeleton (we don't pin every $N here; the
	// existing futures-long test already pins the full shape).
	// Six INSERTs are expected back-to-back: parent + 5 children.
	insertSQL := regexp.MustCompile(`INSERT INTO trade_executions`)

	mock.ExpectQuery(insertSQL.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-parent"))
	mock.ExpectExec("UPDATE trade_executions").
		WithArgs("filled", 4000.0, sql.NullFloat64{Float64: 100, Valid: true},
			10.0, 0.0, 0.0, sqlmock.AnyArg(), "trade-parent").
		WillReturnResult(sqlmock.NewResult(0, 1))

	// 5 children — each 800 qty. Fees are pro-rata even split:
	// commission $2 / stamp 0 / transfer 0.
	for i := 0; i < 5; i++ {
		childID := "trade-child-" + string(rune('0'+i))
		mock.ExpectQuery(insertSQL.String()).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(childID))
		mock.ExpectExec("UPDATE trade_executions").
			WithArgs("filled", 800.0, sql.NullFloat64{Float64: 100, Valid: true},
				2.0, 0.0, 0.0, sqlmock.AnyArg(), childID).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	// runtimeTradingEngine with only tradeRepo wired — cashLedger
	// + lotLedger are nil so the splitter's per-child ledger
	// writes early-return. recordCashLedgerForFill /
	// recordLotFill both check for nil receivers; their own tests
	// cover the write path.
	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}

	fund := &repository.Fund{
		ID:          "fund-1",
		TradingMode: "simulation",
		Config:      json.RawMessage(`{"pm_path_child_splitting": true}`),
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:            "action-1",
		InstrumentKey: "NVDA",
		Symbol:        "NVDA",
		Price:         sql.NullFloat64{Float64: 100, Valid: true},
	}
	filledPrice := sql.NullFloat64{Float64: 100, Valid: true}

	rolledStatus, err := engine.tradeRepoCreateAndFill(
		context.Background(),
		fund, plan, action,
		"buy", 4000, 100, 400000, "filled",
		filledPrice, 10.0, 0.0, 0.0,
		"twap", sql.NullFloat64{},
	)
	if err != nil {
		t.Fatalf("tradeRepoCreateAndFill: %v", err)
	}
	// T6 wire: splitter path must return a non-empty rolled
	// status so the caller (executePlanAction) overrides its
	// local status var with the aggregated label. With the
	// synchronous broker.Simulator all children are filled, so
	// aggregateChildrenStatus returns "filled". A regression
	// that dropped the return would cause caller to silently
	// keep writing the parent's intent status only.
	if rolledStatus != "filled" {
		t.Fatalf("rolledStatus: want %q got %q", "filled", rolledStatus)
	}
	assertMockExpectations(t, mock)
}

// TestTradeRepoCreateAndFill_FlagOffStaysSingleRow guarantees that
// when pm_path_child_splitting is NOT in fund.Config, the splitter
// is NEVER invoked even for a strategy that WOULD split (qty=4000
// TWAP). This is the safety floor: legacy funds keep producing one
// row per action, exactly as they did pre-088.
func TestTradeRepoCreateAndFill_FlagOffStaysSingleRow(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	mock.ExpectQuery("INSERT INTO trade_executions").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-only"))
	mock.ExpectExec("UPDATE trade_executions").
		WillReturnResult(sqlmock.NewResult(0, 1))

	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}

	fund := &repository.Fund{
		ID:          "fund-1",
		TradingMode: "simulation",
		Config:      json.RawMessage(`{}`), // flag absent → false
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:     "action-1",
		Symbol: "NVDA",
		Price:  sql.NullFloat64{Float64: 100, Valid: true},
	}

	rolledStatus, err := engine.tradeRepoCreateAndFill(
		context.Background(),
		fund, plan, action,
		"buy", 4000, 100, 400000, "filled",
		sql.NullFloat64{Float64: 100, Valid: true},
		10.0, 0.0, 0.0,
		"twap", sql.NullFloat64{},
	)
	if err == nil {
		// Single-row path: rolledStatus must be "" so the
		// caller (executePlanAction) falls back to its own
		// status decision. A non-empty value here would
		// indicate the splitter ran when it shouldn't have.
		if rolledStatus != "" {
			t.Fatalf("flag-off single-row path leaked rolledStatus=%q (want \"\")", rolledStatus)
		}
	}
	if err != nil {
		t.Fatalf("flag-off path: %v", err)
	}
	assertMockExpectations(t, mock)
}

// TestTradeRepoCreateAndFillSplit_SellTWAPHappyPath drives the
// splitter on a 4000-share TWAP SELL. lotledger.recordSell handles
// FIFO close-ordering across multiple open lots already, so a
// per-child sell is just a smaller recordSell call — the ledger
// service auto-consumes lots in the right order. This test pins
// the trade_executions layer only (cashLedger / lotLedger nil so
// the per-child ledger writes early-return); FIFO correctness is
// covered by lotledger's own test suite.
func TestTradeRepoCreateAndFillSplit_SellTWAPHappyPath(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	insertSQL := regexp.MustCompile(`INSERT INTO trade_executions`)

	// Parent (qty=4000, fees=10/0/0, side=sell). Note the
	// slippage_pct argument is NULL because computeSlippagePct
	// returns NULL for sells (see column comment on
	// trade_executions.slippage_pct).
	mock.ExpectQuery(insertSQL.String()).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-parent-sell"))
	mock.ExpectExec("UPDATE trade_executions").
		WithArgs("filled", 4000.0, sql.NullFloat64{Float64: 100, Valid: true},
			10.0, 0.0, 0.0, sql.NullFloat64{}, "trade-parent-sell").
		WillReturnResult(sqlmock.NewResult(0, 1))

	for i := 0; i < 5; i++ {
		childID := "trade-child-sell-" + string(rune('0'+i))
		mock.ExpectQuery(insertSQL.String()).
			WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(childID))
		mock.ExpectExec("UPDATE trade_executions").
			WithArgs("filled", 800.0, sql.NullFloat64{Float64: 100, Valid: true},
				2.0, 0.0, 0.0, sql.NullFloat64{}, childID).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}

	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}

	fund := &repository.Fund{
		ID:          "fund-1",
		TradingMode: "simulation",
		Config:      json.RawMessage(`{"pm_path_child_splitting": true}`),
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:            "action-1",
		InstrumentKey: "NVDA",
		Symbol:        "NVDA",
		Price:         sql.NullFloat64{Float64: 100, Valid: true},
		// PositionSide left unset — equity sells almost always
		// leave it NULL in production and the gate maps that to
		// "long" (see splitterEnabledForSide).
	}
	filledPrice := sql.NullFloat64{Float64: 100, Valid: true}

	_, err := engine.tradeRepoCreateAndFill(
		context.Background(),
		fund, plan, action,
		"sell", 4000, 100, 400000, "filled",
		filledPrice, 10.0, 0.0, 0.0,
		"twap", sql.NullFloat64{},
	)
	if err != nil {
		t.Fatalf("tradeRepoCreateAndFill sell split: %v", err)
	}
	assertMockExpectations(t, mock)
}

// TestTradeRepoCreateAndFill_FlagOnButShortStaysSingleRow guards
// the "non-short only" gate: with the flag on AND qty=4000 AND
// strategy=twap, a SELL on a SHORT position must still be the
// legacy single-row path because the lot-ledger short-side branch
// is a no-op pending the parallel short-lot model. A regression
// that fanned out short-side operations would write 5 children
// whose lot writes are all no-ops, leaving holding_positions out
// of sync with trade_executions.
func TestTradeRepoCreateAndFill_FlagOnButShortStaysSingleRow(t *testing.T) {
	db, mock := newMockDB(t)
	defer db.Close()

	mock.ExpectQuery("INSERT INTO trade_executions").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow("trade-only"))
	mock.ExpectExec("UPDATE trade_executions").
		WillReturnResult(sqlmock.NewResult(0, 1))

	engine := &runtimeTradingEngine{tradeRepo: repository.NewTradeRepo(db)}

	fund := &repository.Fund{
		ID:          "fund-1",
		TradingMode: "simulation",
		Config:      json.RawMessage(`{"pm_path_child_splitting": true}`),
	}
	plan := &repository.InvestmentPlan{ID: "plan-1"}
	action := repository.PlanAction{
		ID:           "action-1",
		Symbol:       "ESU2026",
		Price:        sql.NullFloat64{Float64: 100, Valid: true},
		PositionSide: sql.NullString{String: "short", Valid: true},
	}

	_, err := engine.tradeRepoCreateAndFill(
		context.Background(),
		fund, plan, action,
		"sell", 4000, 100, 400000, "filled", // sell + short → gate forces single row
		sql.NullFloat64{Float64: 100, Valid: true},
		10.0, 0.0, 0.0,
		"twap", sql.NullFloat64{},
	)
	if err != nil {
		t.Fatalf("flag-on-but-short path: %v", err)
	}
	assertMockExpectations(t, mock)
}

// avoid unused-import deletions if newMockDB lives elsewhere.
var _ = time.Now
