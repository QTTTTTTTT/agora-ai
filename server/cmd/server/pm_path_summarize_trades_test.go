package main

import (
	"database/sql"
	"testing"

	"github.com/fundai/server/internal/repository"
)

// TestSummarizeTrades_SplitterAware pins the bug-fix that
// motivated this commit: when the PM-path child-splitting
// flag is on, a single plan_action turns into 1 parent + N
// child trade rows. Pre-fix `summarizeTrades` treated every
// row as an independent trade and double-counted everything
// from `total` to `fillRatio`. Post-fix children are skipped
// for the per-plan-action counters and only feed the
// splitter-aware twap* fields.
//
// The matrix below covers the three input shapes that the
// daily-review pipeline can actually feed in:
//
//   * legacy single-row trades (parent_id NULL) — the
//     pre-splitter baseline, must be unchanged.
//   * parent + children both present (the splitter happy
//     path) — parent counts ONCE, children only feed twap*.
//   * children only (defensive — shouldn't happen in
//     production because the splitter writes parent then
//     children atomically, but cheap to pin).
func TestSummarizeTrades_SplitterAware(t *testing.T) {
	actionQty200 := repository.PlanAction{
		Quantity: sql.NullFloat64{Float64: 200, Valid: true},
		Price:    sql.NullFloat64{Float64: 100, Valid: true},
	}
	actionQty1000 := repository.PlanAction{
		Quantity: sql.NullFloat64{Float64: 1000, Valid: true},
		Price:    sql.NullFloat64{Float64: 50, Valid: true},
	}

	cases := []struct {
		name    string
		actions []repository.PlanAction
		trades  []repository.TradeExecution
		want    tradeSummary
	}{
		{
			name:    "legacy single-row trades unchanged",
			actions: []repository.PlanAction{actionQty200, actionQty200},
			trades: []repository.TradeExecution{
				{ID: "t1", Status: "filled", FilledQty: 200},
				{ID: "t2", Status: "filled", FilledQty: 200},
			},
			want: tradeSummary{
				total:           2,
				filled:          2,
				partial:         0,
				rejected:        0,
				fillRatio:       1.0,
				twapSliceCount:  0,
				twapParentCount: 0,
			},
		},
		{
			name:    "parent + 5 children counts parent once",
			actions: []repository.PlanAction{actionQty1000},
			trades: []repository.TradeExecution{
				// Parent: 1 row, status filled, qty 1000.
				{ID: "parent-1", Status: "filled", FilledQty: 1000},
				// 5 TWAP slices, each 200 qty. Without the
				// fix these would each bump total/filled
				// and double-count filled qty into fillRatio.
				{ID: "child-1", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-2", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-3", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-4", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-5", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
			},
			want: tradeSummary{
				total:           1,
				filled:          1,
				partial:         0,
				rejected:        0,
				fillRatio:       1.0, // 5 * 200 / 1000, not 6 * 200 / 1000
				twapSliceCount:  5,
				twapParentCount: 1,
			},
		},
		{
			name:    "two TWAP parents share twap counters",
			actions: []repository.PlanAction{actionQty1000, actionQty1000},
			trades: []repository.TradeExecution{
				{ID: "parent-1", Status: "filled", FilledQty: 1000},
				{ID: "child-1a", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1b", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1c", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1d", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1e", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "parent-2", Status: "filled", FilledQty: 1000},
				{ID: "child-2a", Status: "filled", FilledQty: 500, StrategyParentTradeID: sql.NullString{String: "parent-2", Valid: true}},
				{ID: "child-2b", Status: "filled", FilledQty: 500, StrategyParentTradeID: sql.NullString{String: "parent-2", Valid: true}},
			},
			want: tradeSummary{
				total:           2,
				filled:          2,
				partial:         0,
				rejected:        0,
				fillRatio:       1.0, // (5*200 + 2*500) / 2000 = 1.0
				twapSliceCount:  7,
				twapParentCount: 2,
			},
		},
		{
			name:    "mixed: 1 standalone + 1 split parent",
			actions: []repository.PlanAction{actionQty200, actionQty1000},
			trades: []repository.TradeExecution{
				// Standalone legacy row.
				{ID: "standalone-1", Status: "filled", FilledQty: 200},
				// Split parent.
				{ID: "parent-1", Status: "filled", FilledQty: 1000},
				{ID: "child-1a", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1b", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1c", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1d", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1e", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
			},
			want: tradeSummary{
				total:           2,
				filled:          2,
				partial:         0,
				rejected:        0,
				fillRatio:       1.0, // (200 + 5*200) / 1200 = 1.0
				twapSliceCount:  5,
				twapParentCount: 1,
			},
		},
		{
			name:    "partial fill: split parent with 3 of 5 children filled",
			actions: []repository.PlanAction{actionQty1000},
			trades: []repository.TradeExecution{
				{ID: "parent-1", Status: "partial", FilledQty: 600},
				{ID: "child-1a", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1b", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1c", Status: "filled", FilledQty: 200, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1d", Status: "rejected", FilledQty: 0, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
				{ID: "child-1e", Status: "rejected", FilledQty: 0, StrategyParentTradeID: sql.NullString{String: "parent-1", Valid: true}},
			},
			want: tradeSummary{
				total:           1,
				filled:          0,
				partial:         1,
				rejected:        0,
				fillRatio:       0.6, // 600 / 1000
				twapSliceCount:  5,
				twapParentCount: 1,
			},
		},
		{
			name:    "rejected parent counted as parent-level rejection only",
			actions: []repository.PlanAction{actionQty200},
			trades: []repository.TradeExecution{
				{ID: "t1", Status: "rejected", FilledQty: 0},
			},
			want: tradeSummary{
				total:           1,
				filled:          0,
				partial:         0,
				rejected:        1,
				fillRatio:       0,
				twapSliceCount:  0,
				twapParentCount: 0,
			},
		},
		{
			name:    "empty trades returns zero summary",
			actions: nil,
			trades:  nil,
			want:    tradeSummary{},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := summarizeTrades(tc.actions, tc.trades)
			if got.total != tc.want.total {
				t.Errorf("total: got %d want %d", got.total, tc.want.total)
			}
			if got.filled != tc.want.filled {
				t.Errorf("filled: got %d want %d", got.filled, tc.want.filled)
			}
			if got.partial != tc.want.partial {
				t.Errorf("partial: got %d want %d", got.partial, tc.want.partial)
			}
			if got.rejected != tc.want.rejected {
				t.Errorf("rejected: got %d want %d", got.rejected, tc.want.rejected)
			}
			if got.twapSliceCount != tc.want.twapSliceCount {
				t.Errorf("twapSliceCount: got %d want %d", got.twapSliceCount, tc.want.twapSliceCount)
			}
			if got.twapParentCount != tc.want.twapParentCount {
				t.Errorf("twapParentCount: got %d want %d", got.twapParentCount, tc.want.twapParentCount)
			}
			// fillRatio is float — allow small epsilon.
			if diff := got.fillRatio - tc.want.fillRatio; diff > 0.001 || diff < -0.001 {
				t.Errorf("fillRatio: got %.4f want %.4f", got.fillRatio, tc.want.fillRatio)
			}
		})
	}
}
