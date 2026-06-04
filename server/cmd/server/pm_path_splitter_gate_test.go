package main

import (
	"database/sql"
	"encoding/json"
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

// TestSplitterEnabledForSideWithConfig_FuturesUnlock covers the
// T7-T8a addition: futures fills become splitter-eligible only
// when the fund has opted into the v2 cash flow. The legacy
// gate (splitterEnabledForSide / without-config) must STILL
// reject futures so existing callers that aren't config-aware
// don't accidentally enable splitting on a fund whose cash
// ledger is still on the v1 trade_*_notional path.
func TestSplitterEnabledForSideWithConfig_FuturesUnlock(t *testing.T) {
	cases := []struct {
		name         string
		side         string
		positionSide string
		assetClass   string
		config       json.RawMessage
		want         bool
	}{
		// Equity behaviour unchanged regardless of the v2 flag.
		{name: "equity + v2 off keeps splitter on",
			side: "buy", positionSide: "long", assetClass: "equity",
			config: json.RawMessage(`{}`),
			want:   true,
		},
		{name: "equity + v2 on keeps splitter on",
			side: "sell", positionSide: "long", assetClass: "equity",
			config: json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want:   true,
		},

		// Futures + v2 off: REJECTED (legacy cash flow).
		{name: "futures + nil config rejected",
			side: "buy", positionSide: "long", assetClass: "futures",
			config: nil,
			want:   false,
		},
		{name: "futures + v2 explicitly false rejected",
			side: "buy", positionSide: "long", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":false}`),
			want:   false,
		},
		{name: "futures + malformed config rejected",
			side: "buy", positionSide: "long", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":`),
			want:   false,
		},

		// Futures + v2 on: ENABLED (T8a unlock). Both sides on
		// the long axis. Short futures still excluded by the
		// position_side guard regardless of the v2 flag.
		{name: "futures buy + v2 on enabled",
			side: "buy", positionSide: "long", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want:   true,
		},
		{name: "futures sell + v2 on enabled",
			side: "sell", positionSide: "long", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want:   true,
		},
		{name: "futures buy + unset position_side + v2 on enabled",
			side: "buy", positionSide: "", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want:   true,
		},

		// Short futures stays blocked even with v2 — short-lot
		// ledger is the prerequisite there, NOT the cash flow.
		{name: "futures short + v2 on still rejected",
			side: "sell", positionSide: "short", assetClass: "futures",
			config: json.RawMessage(`{"futures_cash_ledger_v2":true}`),
			want:   false,
		},

		// Coexistence with splitter flag: irrelevant — this
		// function only judges shape, not the master enable.
		{name: "futures + v2 on + splitter flag on enabled",
			side: "buy", positionSide: "long", assetClass: "futures",
			config: json.RawMessage(`{
				"pm_path_child_splitting": true,
				"futures_cash_ledger_v2": true
			}`),
			want: true,
		},
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
			got := splitterEnabledForSideWithConfig(tc.side, action, tc.config)
			if got != tc.want {
				t.Errorf("splitterEnabledForSideWithConfig(%q, position_side=%q, asset_class=%q, config=%s) = %v, want %v",
					tc.side, tc.positionSide, tc.assetClass, string(tc.config), got, tc.want)
			}
		})
	}
}

// TestSplitterEnabledForSide_LegacyShapeUnchanged is a defense-
// in-depth assertion that the no-config wrapper produces the
// EXACT same answers it did pre-T8a (i.e. futures still rejected
// even when the fund has the v2 flag on — because the legacy
// wrapper doesn't know about the flag). Without this test a
// regression that "helpfully" made the legacy wrapper look at
// some global state would silently change every existing caller.
func TestSplitterEnabledForSide_LegacyShapeUnchanged(t *testing.T) {
	action := repository.PlanAction{
		AssetClass:   sql.NullString{String: "futures", Valid: true},
		PositionSide: sql.NullString{String: "long", Valid: true},
	}
	if got := splitterEnabledForSide("buy", action); got {
		t.Fatalf("legacy splitterEnabledForSide(buy, long futures) = true, want false (the legacy wrapper has no config so v2 unlock can't apply)")
	}
}
