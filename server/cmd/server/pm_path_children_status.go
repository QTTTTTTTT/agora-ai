package main

// Roll-up of N child trade rows into a single parent-level
// execution status string. This is a step-2 follow-up of the
// trader-agent integration: docs/TRADER_AGENT_INTEGRATION.md
// item "plan_action.execution_status aggregation".
//
// Why this exists:
//
// Today's PM-direct-fill path goes through broker.Simulator and
// fills synchronously — every child of a TWAP parent lands in the
// same call, all with status='filled'. The legacy caller-decided
// status (one of "filled" / "rejected" / "pending") therefore
// already writes the right value to plan_actions.execution_status
// even with the splitter on.
//
// Where this helper EARNS its keep is the next iteration, when
// broker integrations land that may partially fill or asynchronously
// reject children:
//
//   * Live brokers (Alpaca / IBKR) emit a sequence of fill events
//     and a TWAP child can be partially-filled at the close.
//   * The simulator's TIF=GTD path could mark stale children
//     'cancelled' at the trading-day boundary.
//
// In those flows the per-child status_es will disagree and the
// caller of tradeRepoCreateAndFillSplit needs ONE summary string
// to write into plan_actions.execution_status. That's what this
// helper produces.
//
// Status hierarchy (worst → best):
//
//   filled      — every child is filled (terminal happy path)
//   partial:NN  — at least one child has positive filled_qty AND
//                 the sum is strictly less than the parent total;
//                 NN is the integer percent rounded DOWN
//                 (66.7% → "partial:66" so 100% is reserved for
//                 the "filled" label and never confused).
//   pending     — at least one child is still pending / working
//                 / triggered AND no child has filled anything.
//                 (Children that are partly-filled are reflected
//                 in "partial:NN" above; this is the strictly-
//                 zero-fill case.)
//   rejected    — every child is rejected / cancelled (terminal
//                 unhappy path).
//
// The "partial:NN" label intentionally encodes the percent in the
// string instead of a separate column so the existing
// plan_actions.execution_status varchar column accommodates it
// without a schema change. Frontend parsers should strip the
// "partial:" prefix and treat the numeric suffix as advisory only.
//
// Inputs:
//
//   children — per-child (status, filledQty) pairs. Order doesn't
//              matter. An empty slice returns "" — the caller's
//              cue that nothing was rolled up and they should fall
//              back to whatever pre-splitter status they already
//              computed.
//   parentTotalQty — the parent row's intended quantity. Used as
//                    the denominator for the partial percent.
//                    If <= 0 the helper degrades to "filled" iff
//                    every child is filled, else "pending" — we
//                    never divide by zero.

import (
	"fmt"
	"strings"
)

// ChildStatus is the per-child input to aggregateChildrenStatus.
// Status follows the same vocabulary trade_executions.status uses
// today (filled / partial / pending / working / triggered /
// rejected / cancelled). FilledQty is in lot units (NOT pct).
type ChildStatus struct {
	Status    string
	FilledQty float64
}

const (
	rolledStatusFilled   = "filled"
	rolledStatusPending  = "pending"
	rolledStatusRejected = "rejected"
)

// aggregateChildrenStatus produces the parent-level execution
// status string for the given children. See file-level comment
// for the precedence table.
func aggregateChildrenStatus(children []ChildStatus, parentTotalQty float64) string {
	if len(children) == 0 {
		return ""
	}

	allFilled := true
	allTerminallyRejected := true
	totalFilled := 0.0
	for _, c := range children {
		statusLower := strings.ToLower(strings.TrimSpace(c.Status))
		switch statusLower {
		case "filled":
			// row is fully filled — counts toward "all filled".
		default:
			allFilled = false
		}
		switch statusLower {
		case "rejected", "cancelled":
			// row is terminally unsuccessful — counts toward
			// "all rejected".
		default:
			allTerminallyRejected = false
		}
		if c.FilledQty > 0 {
			totalFilled += c.FilledQty
		}
	}

	if allFilled {
		return rolledStatusFilled
	}
	if allTerminallyRejected {
		return rolledStatusRejected
	}
	if totalFilled > 0 && parentTotalQty > 0 {
		// Round DOWN: 4000/6000 = 66.66... → 66, never 67.
		// 100% is reserved for the "filled" label above so we
		// floor here even when the math would round to 100.
		pct := int((totalFilled / parentTotalQty) * 100.0)
		if pct >= 100 {
			pct = 99
		}
		if pct < 1 {
			pct = 1
		}
		return fmt.Sprintf("partial:%d", pct)
	}
	// Mixed pending / working / triggered with zero filled qty —
	// the parent is in flight but hasn't started landing fills.
	return rolledStatusPending
}
