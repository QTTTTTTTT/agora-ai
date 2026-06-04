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

// splitterEnabledForSide reports whether the (side, position_side,
// asset_class) tuple for `action` falls within the subset of trade
// shapes the splitter is currently wired for. The splitter logic
// itself is side-agnostic — it hands per-child fills to the existing
// recordLotFill / recordCashLedgerForFill helpers — but those
// helpers behave differently depending on whether they're opening
// or closing a long, whether the asset is equity vs futures, and
// the lot-ledger short-side branch is a no-op pending the parallel
// short-lot model. See the call-site matrix in tradeRepoCreateAndFill
// (wiring_adapters.go).
//
// Truth table (lowercased + trimmed):
//
//	side    position_side   asset_class       result   reason
//	------  -------------   ---------------   ------   ------
//	buy     "" / "long"     non-futures       true     equity open / add
//	sell    "" / "long"     non-futures       true     equity close (FIFO via lotledger)
//	buy     "short"         *                 false    closes a short
//	sell    "short"         *                 false    opens a short
//	*       *               "futures"         false    margin release per child not yet wired
//	(other side)            *                 false    safety
//
// Why empty position_side is treated as long: every equity row in
// the production schema leaves PositionSide unset (the column is a
// nullable string used primarily by the futures path). Treating
// empty as long keeps the equity sell/buy paths flowing through
// the splitter without forcing a backfill of
// PlanAction.position_side on every historical equity action.
//
// Why short is excluded: lotledger.recordSell only handles LONG-side
// close (sell consumes long lots FIFO). A short open (sell +
// position_side=short) or short close (buy + position_side=short)
// would either be a no-op in the lot ledger (silent FIFO drift) or
// double-count the cash side. The parallel short-lot ledger is
// tracked in the ADR; until it lands, the splitter must stay off
// for short positions.
//
// Why futures is excluded: recordCashLedgerForFill writes a single
// SellNotional row for any sell-side fill regardless of asset_class.
// For equity that's correct (proceeds hit cash); for futures it's
// a simplification — a real futures close should release margin in
// per-child increments and book realized PnL separately. The
// single-row legacy path already has this simplification baked in,
// but the splitter must not amplify it across N children until
// per-child margin release is wired (see ADR "deferred").
func splitterEnabledForSide(side string, action repository.PlanAction) bool {
	return splitterEnabledForSideWithConfig(side, action, nil)
}

// splitterEnabledForSideWithConfig is the T7 follow-up that re-opens
// the futures branch when the fund has opted into the v2 cash flow.
//
// Truth table (lowercased + trimmed):
//
//	side    position_side   asset_class       v2 flag   result
//	------  -------------   ---------------   -------   ------
//	buy     "" / "long"     non-futures       *         true
//	sell    "" / "long"     non-futures       *         true
//	buy     "" / "long"     "futures"         true      true   (T7 unlocks)
//	sell    "" / "long"     "futures"         true      true   (T7 unlocks)
//	*       *               "futures"         false     false  (legacy notional path)
//	*       "short"         *                 *         false  (short-lot ledger pending)
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
func splitterEnabledForSideWithConfig(side string, action repository.PlanAction, fundConfig json.RawMessage) bool {
	normalizedSide := strings.ToLower(strings.TrimSpace(side))
	positionSide := strings.ToLower(strings.TrimSpace(action.PositionSide.String))
	assetClass := strings.ToLower(strings.TrimSpace(action.AssetClass.String))
	if positionSide == "short" {
		return false
	}
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
