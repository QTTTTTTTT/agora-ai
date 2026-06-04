package main

// PM-direct-fill path execution-strategy selection. This is the
// FIRST half of wiring agent.TraderAgent into the actual trading
// path; the second half (child-order splitting + parent_trade_id
// linkage + cross-child slippage aggregation) is deliberately
// out of scope for this commit and tracked in agent_self_learning_prompts.go
// / docs as a follow-up.
//
// What this file does today:
//   * Mirrors agent.TraderAgent.selectStrategy's decision logic on the
//     PM-direct-fill path so trade_executions.strategy is populated
//     with the same label the TraderAgent would have chosen
//     ("immediate" / "limit" / "twap" / "vwap"). The chosen strategy
//     is the only piece of TraderAgent state that lives on disk
//     today, but it gives downstream analytics + the daily-review
//     LLM call something to reason about ("today's trader logged a
//     TWAP intent on a 4000-share buy that filled at +3bps") even
//     before real splitting goes live.
//   * Provides a thresholded variant that respects per-action
//     features (already-set price → limit; large quantity → TWAP)
//     using the same defaults agent.TraderAgent ships with so the
//     two code paths converge as soon as we wire the real splitter.
//
// What this file does NOT do:
//   * It does not split a parent order into N children. The PM
//     direct-fill engine still writes ONE trade_execution per
//     plan_action; the only change is that row now has a
//     strategy='twap' label even if there's a single fill behind it.
//   * It does not call broker.Simulator multiple times. A future
//     B-step2 PR will replace the single tradeRepoCreateAndFill call
//     with a loop guarded by the same 5 pre-trade gates we already
//     have on the parent action.

import (
	"strings"

	"github.com/fundai/server/internal/agent"
	"github.com/fundai/server/internal/repository"
)

// PM-direct-fill defaults. Kept in sync with agent.DefaultTraderConfig
// so the strategy label written here matches what the TraderAgent
// would have picked, and a future B-step2 that swaps in real
// splitting won't suddenly disagree with the historical label.
//
// Sourced from agent.DefaultTraderConfig (server/internal/agent/trader.go:80-86):
//   SplitThreshold: 1000  → above this qty we record TWAP intent
//   LimitPriceOffset: 0.002 → unused here (recorded on broker side)
const (
	pmPathSplitThreshold = 1000
)

// selectPMPathExecutionStrategy returns one of:
//
//   * "immediate" — qty ≤ threshold AND action has NO explicit
//                   plan price (market order)
//   * "limit"     — qty ≤ threshold AND action carries a plan price
//   * "twap"      — qty > threshold (TWAP slice over the day)
//
// VWAP is not chosen automatically here: it requires market-volume
// curve data the PM-direct-fill path doesn't have on hand. A future
// venue-data integration could promote large TWAP-eligible orders to
// VWAP; until then we record the conservative TWAP label.
//
// The decision rule is intentionally identical to
// agent.TraderAgent.selectStrategy so a future B-step2 PR that
// replaces tradeRepoCreateAndFill with TraderAgent.ExecutePlan can
// drop in without re-labelling historical rows.
func selectPMPathExecutionStrategy(action repository.PlanAction, quantity int) string {
	// Quantity gate first: a large order's strategy concern is
	// "spread the impact across the day", which dominates
	// "match a target price" — pick TWAP regardless of whether
	// action.Price was set.
	if quantity > pmPathSplitThreshold {
		return string(agent.StrategyTWAP)
	}

	// At smaller sizes, the presence/absence of a plan price is
	// the discriminator: a non-zero plan price means the PM is
	// targeting a specific entry → limit order. Zero plan price
	// (or zero-after-validation) is the "execute at whatever the
	// market gives us" case → market order.
	if action.Price.Valid && action.Price.Float64 > 0 {
		return string(agent.StrategyLimit)
	}
	return string(agent.StrategyImmediate)
}

// normalizePMPathStrategy is a tiny defensive helper: strategies are
// written into a sql.NullString column, so we want to make sure
// we never emit an empty / unrecognised value that would clutter
// downstream analytics. Anything we don't recognise collapses to
// "immediate" (the safest default — equivalent to today's pre-Trader
// PM-direct-fill behaviour).
func normalizePMPathStrategy(strategy string) string {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case string(agent.StrategyImmediate),
		string(agent.StrategyLimit),
		string(agent.StrategyTWAP),
		string(agent.StrategyVWAP):
		return strings.ToLower(strings.TrimSpace(strategy))
	}
	return string(agent.StrategyImmediate)
}
