package main

import (
	"testing"

	"github.com/fundai/server/internal/agent"
)

// TestSplitParentIntoChildren_StrategyMatrix asserts the
// table-driven contract of splitParentIntoChildren: which strategy
// produces N children, that the sum of children equals the parent,
// and that the LAST child carries any rounding remainder. These are
// the invariants tradeRepoCreateAndFill relies on to know the parent
// row's qty agrees with the children's per-row qty, and to know
// cash_ledger writes will sum back to the parent's expected debit.
func TestSplitParentIntoChildren_StrategyMatrix(t *testing.T) {
	tests := []struct {
		name        string
		totalQty    int
		strategy    string
		wantSlices  []int
		wantSplit   bool
	}{
		// TWAP — clean round split (4000 / 5 = 800 each).
		{
			name:       "twap clean 4000 produces 5 even slices",
			totalQty:   4000,
			strategy:   string(agent.StrategyTWAP),
			wantSlices: []int{800, 800, 800, 800, 800},
			wantSplit:  true,
		},
		// TWAP — remainder on last child.
		{
			name:       "twap 4001 puts remainder on last child",
			totalQty:   4001,
			strategy:   string(agent.StrategyTWAP),
			wantSlices: []int{800, 800, 800, 800, 801},
			wantSplit:  true,
		},
		// TWAP — large remainder still on last child.
		{
			name:       "twap 4004 puts +4 on last child",
			totalQty:   4004,
			strategy:   string(agent.StrategyTWAP),
			wantSlices: []int{800, 800, 800, 800, 804},
			wantSplit:  true,
		},
		// TWAP — degraded: total < slice count → one share per slice.
		{
			name:       "twap 3 degrades to 3 unit slices",
			totalQty:   3,
			strategy:   string(agent.StrategyTWAP),
			wantSlices: []int{1, 1, 1},
			wantSplit:  true,
		},
		// Immediate — 1 child = full qty.
		{
			name:       "immediate produces single child",
			totalQty:   1500,
			strategy:   string(agent.StrategyImmediate),
			wantSlices: []int{1500},
			wantSplit:  false,
		},
		// Limit — 1 child = full qty.
		{
			name:       "limit produces single child",
			totalQty:   1500,
			strategy:   string(agent.StrategyLimit),
			wantSlices: []int{1500},
			wantSplit:  false,
		},
		// VWAP — stubbed to single child (volume-curve not yet wired).
		{
			name:       "vwap stubbed to single child",
			totalQty:   4000,
			strategy:   string(agent.StrategyVWAP),
			wantSlices: []int{4000},
			wantSplit:  false,
		},
		// Unknown strategy — defaults to single child (no orphans).
		{
			name:       "unknown strategy defaults to single child",
			totalQty:   1234,
			strategy:   "iceberg",
			wantSlices: []int{1234},
			wantSplit:  false,
		},
		// Zero qty — empty slice (caller skips).
		{
			name:       "zero qty returns nil",
			totalQty:   0,
			strategy:   string(agent.StrategyTWAP),
			wantSlices: nil,
			wantSplit:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitParentIntoChildren(tt.totalQty, tt.strategy)
			if !sliceEq(got, tt.wantSlices) {
				t.Fatalf("splitParentIntoChildren(%d, %q) = %v, want %v",
					tt.totalQty, tt.strategy, got, tt.wantSlices)
			}

			// Invariant: sum(children) == parent. Skip when the
			// expected slice is nil (degenerate qty=0 case).
			if tt.wantSlices != nil {
				sum := 0
				for _, q := range got {
					sum += q
				}
				if sum != tt.totalQty {
					t.Errorf("sum(children) = %d, want %d", sum, tt.totalQty)
				}
			}

			if gotSplit := shouldSplitParent(tt.totalQty, tt.strategy); gotSplit != tt.wantSplit {
				t.Errorf("shouldSplitParent = %v, want %v", gotSplit, tt.wantSplit)
			}
		})
	}
}

// TestSplitParentIntoChildren_NoZeroQuantityChildren guards against
// regressions where a rounding edge case (e.g. totalQty == twapChildCount)
// would emit a 0-quantity slice. A 0-qty child row would break the
// CHECK constraint on filled_qty and silently inflate the cash_ledger
// row count without contributing capital — both are worse than the
// degraded "one slice per share" fallback.
func TestSplitParentIntoChildren_NoZeroQuantityChildren(t *testing.T) {
	// Exhaustive sweep across the small-qty / TWAP corner.
	for qty := 1; qty <= 20; qty++ {
		got := splitParentIntoChildren(qty, string(agent.StrategyTWAP))
		for i, slice := range got {
			if slice <= 0 {
				t.Errorf("qty=%d TWAP child[%d] = %d (want > 0); slices=%v",
					qty, i, slice, got)
			}
		}
	}
}

// TestSplitEvenWithRemainder_Boundary covers the rounding helper in
// isolation so splitParentIntoChildren can lean on it without
// duplicating the boundary cases.
func TestSplitEvenWithRemainder_Boundary(t *testing.T) {
	cases := []struct {
		total, n int
		want     []int
	}{
		{total: 0, n: 5, want: nil},     // zero total
		{total: 5, n: 0, want: nil},     // zero slice count
		{total: 5, n: 5, want: []int{1, 1, 1, 1, 1}},
		{total: 10, n: 5, want: []int{2, 2, 2, 2, 2}},
		{total: 11, n: 5, want: []int{2, 2, 2, 2, 3}},
		{total: 14, n: 5, want: []int{2, 2, 2, 2, 6}}, // remainder = 4
	}
	for _, tc := range cases {
		got := splitEvenWithRemainder(tc.total, tc.n)
		if !sliceEq(got, tc.want) {
			t.Errorf("splitEvenWithRemainder(%d, %d) = %v, want %v",
				tc.total, tc.n, got, tc.want)
		}
	}
}

func sliceEq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
