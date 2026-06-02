// Order replay / restart recovery for the in-memory Simulator (P1-5).
//
// On a clean restart the Simulator starts with an empty book. Open
// orders that were persisted to trade_executions before the crash
// would be invisible to the matching engine, the stop-trigger
// engine, and the cancel/replace API: the DB still says "working"
// but the simulator has no record of them.
//
// RestoreOpenOrders re-seeds the in-memory state from a snapshot of
// non-terminal orders. The caller is expected to read the snapshot
// from trade_executions (TradeRepo.ListOpenByFund or the all-funds
// equivalent) at boot, before any new placements arrive, and pass
// it here. After Restore returns, every order in the input is:
//
//   - addressable by BrokerOrderID via GetOrder/CancelOrder/Replace;
//   - addressable by ClientOrderID via GetOrderByClientID + the
//     idempotency index, so a duplicate PlaceOrder coming through
//     the runtime path collapses to the existing row;
//   - listed in ListOpenOrders for the fund;
//   - if a stop / stop_limit / trailing_stop, surfaced to the
//     stop-trigger engine via AllPendingStops so trailing high/low
//     watermarks resume tracking on the next quote tick.
//
// We deliberately do NOT replay fills back through tryFill: that
// would double-bill commissions and re-emit historical Fills onto
// any subscriber who connected after restart. Replay is for
// CONTINUITY of state, not for re-running execution. If the matcher
// was mid-fill when the process died, the partially-filled trade
// row records FilledQty already; the remainder simply resumes as a
// working order from "now".

package broker

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAlreadyRestored is returned by RestoreOpenOrders when the
// caller tries to restore the same order twice (same fund_id +
// broker_order_id). It is non-fatal: the caller can log and
// continue with the next row.
var ErrAlreadyRestored = errors.New("broker simulator: order already restored")

// RestoreReport summarises a Restore call. Counts are populated for
// every successful Restore, even on partial failure: callers should
// read Errors to find rejected rows.
type RestoreReport struct {
	// Restored is the number of orders successfully seeded.
	Restored int
	// Skipped is the number of input rows the caller passed that
	// turned out to be in a terminal state (e.g. between snapshot
	// time and Restore time another process cancelled them). These
	// are NOT errors — they are simply uninteresting.
	Skipped int
	// Errors collects per-row failures keyed by BrokerOrderID. A
	// missing FundID, missing BrokerOrderID, or duplicate restore
	// shows up here. The caller decides whether to fail loudly or
	// log-and-continue.
	Errors map[string]error
}

// RestoreOpenOrders re-seeds the simulator's in-memory state from a
// caller-supplied snapshot of open orders. Safe to call once at
// process start; safe to call multiple times only if the snapshots
// are disjoint (the function rejects duplicates rather than silently
// overwriting them — overwriting would lose runtime state like
// TrailingHighWater that the stop-trigger engine has been
// accumulating).
//
// The orders are stored exactly as supplied: the caller is
// responsible for normalising State, FilledQuantity, AvgFillPrice,
// CurrentStopPrice, etc. before passing them in. RestoreFromSnapshot
// (defined in the wiring layer) handles the
// repository.TradeExecution → broker.Order projection so the
// simulator package stays free of repository imports.
func (s *Simulator) RestoreOpenOrders(orders []Order) RestoreReport {
	report := RestoreReport{Errors: map[string]error{}}
	if len(orders) == 0 {
		return report
	}

	now := s.nowFn()
	if now.IsZero() {
		now = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range orders {
		o := orders[i]
		key := strings.TrimSpace(o.BrokerOrderID)
		if key == "" {
			report.Errors[fmt.Sprintf("idx-%d", i)] = fmt.Errorf("missing broker_order_id")
			continue
		}
		if strings.TrimSpace(o.Request.FundID) == "" {
			report.Errors[key] = fmt.Errorf("missing fund_id")
			continue
		}
		if o.State.IsTerminal() {
			report.Skipped++
			continue
		}
		if _, exists := s.orders[key]; exists {
			report.Errors[key] = ErrAlreadyRestored
			continue
		}

		// Defensive copy so the caller can't mutate our state via
		// the original slice. We also force UpdatedAt forward to
		// "now" so observers can tell that this Order was
		// reconstructed rather than handled in-flight (the
		// PlacedAt field is preserved untouched for audit).
		copied := o
		if copied.PlacedAt.IsZero() {
			copied.PlacedAt = now
		}
		copied.UpdatedAt = now

		// Stop-typed orders need CurrentStopPrice populated even
		// after a restart. We trust the caller to supply the
		// most-recent value (which for non-trailing stops equals
		// Request.StopPrice and for trailing stops is the
		// ratcheted level snapshotted to the trade row). If they
		// supplied 0 for a trailing stop we fall back to the
		// request stop price; the stop-trigger engine will
		// re-anchor on the next OnQuote.
		if copied.Request.OrderType.IsStopType() && copied.CurrentStopPrice == 0 {
			copied.CurrentStopPrice = copied.Request.StopPrice
		}

		s.orders[key] = &copied

		// Idempotency index — only when ClientOrderID is set
		// (legacy rows can be empty; dedupe is best-effort).
		if cid := strings.TrimSpace(copied.ClientOrderID); cid != "" {
			s.clientIndex[idempotencyKey(copied.Request.FundID, cid)] = key
		}

		// Open-orders index used by ListOpenOrders + cancel-all
		// flows. We re-add even when the snapshot was stale and
		// the order has since transitioned: the next Cancel/
		// Replace call will move it out of the open set anyway.
		s.markOpenLocked(copied.Request.FundID, key)

		report.Restored++
	}

	return report
}
