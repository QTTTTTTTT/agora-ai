package main

import (
	"database/sql"
	"testing"

	"github.com/fundai/server/internal/repository"
)

// TestSplitterEnabledForSide_Matrix nails down each (side,
// position_side) cell in the splitter eligibility table. The two
// safety-critical cells are:
//
//   * "buy" + position_side="short" → false
//     (a short close — the lot ledger doesn't model short lots
//     today, splitting would silently FIFO-close zero rows per
//     child and leak position drift.)
//   * "sell" + position_side="short" → false
//     (a short open — symmetric blocker.)
//
// A regression flipping either of those to true would fan out a
// 4000-share short-side operation across 5 children whose lot-ledger
// writes are all no-ops, leaving the holdings table out of sync
// with the trade_executions stream.
func TestSplitterEnabledForSide_Matrix(t *testing.T) {
	cases := []struct {
		name         string
		side         string
		positionSide string
		assetClass   string
		want         bool
	}{
		// long-side equity (or unset position_side) — splitter enabled.
		{name: "buy + unset position_side + equity", side: "buy", positionSide: "", assetClass: "equity", want: true},
		{name: "buy + long + equity", side: "buy", positionSide: "long", assetClass: "equity", want: true},
		{name: "sell + unset position_side + equity", side: "sell", positionSide: "", assetClass: "equity", want: true},
		{name: "sell + long + equity", side: "sell", positionSide: "long", assetClass: "equity", want: true},
		// Equity with asset_class unset (legacy rows) — still on.
		{name: "buy + long + unset asset_class", side: "buy", positionSide: "long", assetClass: "", want: true},

		// short-side — splitter blocked regardless of asset_class.
		{name: "buy + short equity closes a short", side: "buy", positionSide: "short", assetClass: "equity", want: false},
		{name: "sell + short equity opens a short", side: "sell", positionSide: "short", assetClass: "equity", want: false},
		{name: "buy + short futures closes a short", side: "buy", positionSide: "short", assetClass: "futures", want: false},
		{name: "sell + short futures opens a short", side: "sell", positionSide: "short", assetClass: "futures", want: false},

		// futures (any long-side) — splitter blocked pending per-child
		// margin release wiring. Same fail-closed reasoning as short.
		{name: "buy + long futures open", side: "buy", positionSide: "long", assetClass: "futures", want: false},
		{name: "sell + long futures close", side: "sell", positionSide: "long", assetClass: "futures", want: false},
		{name: "buy + unset position_side + futures", side: "buy", positionSide: "", assetClass: "futures", want: false},

		// Unknown sides — fail safe.
		{name: "unknown side returns false", side: "exchange", positionSide: "long", assetClass: "equity", want: false},
		{name: "empty side returns false", side: "", positionSide: "long", assetClass: "equity", want: false},

		// Case + whitespace normalisation.
		{name: "BUY upper-case + LONG + EQUITY", side: "BUY", positionSide: "LONG", assetClass: "EQUITY", want: true},
		{name: "  sell  + Short  + Equity with spaces", side: "  sell  ", positionSide: "  Short  ", assetClass: "  Equity  ", want: false},
		{name: "  buy  + Long + FUTURES with spaces", side: "  buy  ", positionSide: "Long", assetClass: "  FUTURES  ", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			action := repository.PlanAction{}
			if tc.positionSide != "" {
				action.PositionSide = sql.NullString{String: tc.positionSide, Valid: true}
			}
			if tc.assetClass != "" {
				action.AssetClass = sql.NullString{String: tc.assetClass, Valid: true}
			}
			got := splitterEnabledForSide(tc.side, action)
			if got != tc.want {
				t.Errorf("splitterEnabledForSide(%q, position_side=%q, asset_class=%q) = %v, want %v",
					tc.side, tc.positionSide, tc.assetClass, got, tc.want)
			}
		})
	}
}
