package main

// Splitter eligibility gate per (side, position_side) for the
// PM-direct-fill child-order splitter. Pulled into its own file
// (rather than living in pm_path_child_split.go) so the splitter
// math stays free of repository.PlanAction — that file is pure
// data-in-data-out, useful for property-testing without a DB.

import (
	"encoding/json"
	"strings"

	"github.com/fundai/server/internal/repository"
)

// splitterEnabledForSide is the LEGACY no-config shim retained for
// pre-T7 callers that don't have a fund.Config in scope. It is
// equivalent to calling splitterEnabledForSideWithConfig with a nil
// config, which means BOTH the v2-futures unlock (T7) and any
// future per-fund config-gated unlocks (T8 short-side never
// added a config gate, it's globally on) collapse to their
// "config absent → conservative default" branch.
//
// Concretely, with nil config:
//   - futures branch closes (v2 flag is off when config is nil)
//   - short branch is open globally because T8 unlocked it without
//     a per-fund flag (the data layer is atomic with the schema)
//
// New callers should pass fundConfig and use the With variant
// directly. This shim is here to keep tests and any leftover
// non-config call sites compiling.
func splitterEnabledForSide(side string, action repository.PlanAction) bool {
	return splitterEnabledForSideWithConfig(side, action, nil)
}

// splitterEnabledForSideWithConfig is the T7 / T8 follow-up that
// re-opens (a) the futures branch when the fund has opted into the
// v2 cash flow and (b) the short branch now that the lot ledger
// has a parallel short-side model.
//
// Truth table (lowercased + trimmed):
//
//	side    position_side   asset_class       v2 flag   result
//	------  -------------   ---------------   -------   ------
//	buy     "" / "long"     non-futures       *         true
//	sell    "" / "long"     non-futures       *         true
//	buy     "" / "long"     "futures"         true      true   (T7 unlocks)
//	sell    "" / "long"     "futures"         true      true   (T7 unlocks)
//	sell    "short"         non-futures       *         true   (T8 unlocks — open short)
//	buy     "short"         non-futures       *         true   (T8 unlocks — cover short)
//	sell    "short"         "futures"         true      true   (T7+T8)
//	buy     "short"         "futures"         true      true   (T7+T8)
//	*       *               "futures"         false     false  (legacy notional path)
//	(other side)            *                 *         false
//
// Why the v2 flag matters: pre-T7 the cash-ledger writer for a
// futures fill collapsed everything into trade_*_notional, which
// is fine on a single row but wrong if you amplify it across N
// children (the amplification doesn't show up as a CHECK violation
// or a balance error — it silently overstates cash movement). T7
// teaches recordCashLedgerForFill the futures branch and adds
// pro-rata PnL splitting; the splitter gate can now safely fan
// out a futures parent into children iff the fund's writer is on
// the v2 path. Funds on the legacy path stay single-row even
// when the splitter flag is on.
//
// Why short is now safe: T8 lands a parallel short-lot model in
// position_lots (side='short') with a matching closed_lots row,
// and recordLotFill routes PositionSide="short" into
// recordShortOpen / recordShortClose. A short open (sell + short)
// or short close (buy + short) consumes / produces lots through
// the correct FIFO with the correct PnL sign; the splitter can
// amplify across N children without silent ledger drift.
func splitterEnabledForSideWithConfig(side string, action repository.PlanAction, fundConfig json.RawMessage) bool {
	normalizedSide := strings.ToLower(strings.TrimSpace(side))
	assetClass := strings.ToLower(strings.TrimSpace(action.AssetClass.String))
	if assetClass == "futures" {
		// T7-T8a unlock: only when the writer can correctly
		// shape the per-child cash flow into margin + PnL. The
		// pmPathChildSplittingEnabled check is the parent gate
		// in the dispatcher; this function only decides shape.
		if !futuresCashLedgerV2Enabled(fundConfig) {
			return false
		}
	}
	switch normalizedSide {
	case "buy", "sell":
		return true
	}
	return false
}
