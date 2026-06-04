package main

// PM-direct-fill child-order splitting (step 2 of the Trader Agent
// integration plan, docs/TRADER_AGENT_INTEGRATION.md).
//
// Step 1 wrote the SELECTED strategy ('immediate' / 'limit' / 'twap'
// / 'vwap') into trade_executions.strategy on a SINGLE row so the
// downstream daily-review LLM could reason about trader intent. Step 2
// actually slices that single row into N child rows when the strategy
// dictates so cash_ledger / position_lots reflect per-slice basis
// (Important: a 4000-share TWAP at three different intraday prices
// must NOT collapse to one weighted-average lot — the FIFO cost basis
// would then be wrong for any later partial sell.)
//
// This file is pure: no DB, no broker, no clock. The caller
// (tradeRepoCreateAndFill) feeds in the total parent quantity and the
// selected strategy and gets back the per-child quantities. The
// caller is responsible for:
//
//   * INSERTing the parent row (qty = sum of children, strategy
//     populated, strategy_parent_trade_id = NULL).
//   * Iterating over the returned slice and INSERTing one child
//     row per element (qty = element value, strategy = parent's
//     strategy, strategy_parent_trade_id = parent.ID).
//   * Writing one cash_ledger + position_lots row PER CHILD (the
//     splitter doesn't touch those — separation of concerns).
//
// The splitter intentionally returns INTEGER quantities so the sum
// of children equals the parent exactly. A naive "qty / N" would
// drift by up to (N-1) shares on odd totals (4001 / 5 = 800 each,
// summed = 4000, lost share). We assign the remainder to the LAST
// child so the FIRST N-1 children carry round lots — that's what
// most TWAP venues do in practice and makes the slice schedule
// easier to eyeball in the audit log.

import (
	"github.com/fundai/server/internal/agent"
)

// twapChildCount is the default number of TWAP slices the PM
// direct-fill path uses when the chosen strategy is 'twap'. Kept
// in sync with agent.DefaultTraderConfig.TWAPSlices (5) so the
// child-row layout matches what TraderAgent.ExecutePlan would
// have produced. If TWAPSlices ever becomes per-fund configurable
// the resolver should live next to pmPathChildSplittingEnabled in
// pm_path_feature_flag.go, not here.
const twapChildCount = 5

// splitParentIntoChildren returns the per-child quantities for a
// parent fill of `totalQty` units under the supplied execution
// strategy. The slice length is the number of child rows the caller
// should INSERT. Sum-of-slice always equals totalQty (rounding error
// is absorbed by the LAST child, see file-level comment).
//
// Semantics by strategy:
//
//   * "immediate" / "limit" → 1 child carrying the full qty. The
//     caller can choose to skip the parent+child split entirely and
//     write a single row instead (current pre-088 behaviour); the
//     splitter still returns a 1-element slice so a uniform loop is
//     valid in both code paths.
//
//   * "twap" → twapChildCount slices, round-lot quantities for the
//     first N-1 with the remainder on the last. If totalQty is
//     smaller than twapChildCount (rare in practice — TWAP is only
//     selected when qty > splitThreshold = 1000) we fall back to one
//     child per share so we never emit zero-quantity rows.
//
//   * "vwap" / "iceberg" / "pov" / anything else → same as
//     "immediate" (1 child) for now. VWAP requires market-volume
//     curve data the PM-direct-fill path doesn't have on hand;
//     iceberg/POV need live order-book signals. These are stubbed
//     as 1-child today so the column reads cleanly while the strategy
//     selector still gets to record intent.
//
// totalQty <= 0 returns nil (degenerate; caller should skip).
func splitParentIntoChildren(totalQty int, strategy string) []int {
	if totalQty <= 0 {
		return nil
	}

	switch strategy {
	case string(agent.StrategyTWAP):
		return splitEvenWithRemainder(totalQty, twapChildCount)

	case string(agent.StrategyImmediate),
		string(agent.StrategyLimit),
		string(agent.StrategyVWAP):
		// VWAP collapses to single-child until the volume-curve
		// data path lands; the strategy column on the (single)
		// row still says 'vwap' so analytics can flag the gap.
		return []int{totalQty}
	}

	// Unknown strategies (POV, iceberg, future labels) — fall
	// back to single-child so we never produce orphan rows.
	return []int{totalQty}
}

// splitEvenWithRemainder splits `total` into `n` slices. Each of the
// first n-1 slices gets floor(total/n); the remainder (which is in
// [0, n-1]) is appended to the LAST slice. If n exceeds total we
// produce one slice per unit (length = total).
//
// Examples:
//
//	splitEvenWithRemainder(4000, 5)  → [800, 800, 800, 800, 800]
//	splitEvenWithRemainder(4001, 5)  → [800, 800, 800, 800, 801]
//	splitEvenWithRemainder(4004, 5)  → [800, 800, 800, 800, 804]
//	splitEvenWithRemainder(3, 5)     → [1, 1, 1]  (degraded; below)
//	splitEvenWithRemainder(0, 5)     → []
func splitEvenWithRemainder(total, n int) []int {
	if total <= 0 || n <= 0 {
		return nil
	}
	// Degraded case: not enough volume to fill every slice. Emit
	// one slice per unit (length = total). This preserves the
	// invariant "no zero-quantity child" without forcing the
	// caller to special-case small TWAP intents.
	if total < n {
		out := make([]int, total)
		for i := range out {
			out[i] = 1
		}
		return out
	}

	base := total / n
	remainder := total - base*n

	out := make([]int, n)
	for i := 0; i < n-1; i++ {
		out[i] = base
	}
	out[n-1] = base + remainder
	return out
}

// shouldSplitParent reports whether the splitter would produce more
// than one child for the given (qty, strategy). The PM-direct-fill
// path uses this as an early-exit: when the answer is false, we can
// keep the legacy "single row, no parent_trade_id" code path
// untouched (and the strategy column is still populated so the
// label discipline holds even on non-split paths).
//
// This is just sugar over `len(splitParentIntoChildren(...)) > 1`
// kept here so the call site reads naturally.
func shouldSplitParent(totalQty int, strategy string) bool {
	return len(splitParentIntoChildren(totalQty, strategy)) > 1
}
