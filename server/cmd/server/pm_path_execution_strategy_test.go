package main

// Tests for selectPMPathExecutionStrategy. The decision rule is
// deliberately identical to agent.TraderAgent.selectStrategy, so a
// future B-step2 PR that replaces tradeRepoCreateAndFill with the
// real splitter can drop in without re-labelling historical rows.
//
// The matrix covers the three real-world execution intents a PM
// might want from the trader desk:
//
//   * Quantity gate dominates: any qty > pmPathSplitThreshold (1000
//     by default) is TWAP regardless of plan price. Pickle here
//     would be "PM set a 2000-share limit at 12.30, but executor
//     should still slice over the day".
//   * Below the quantity gate, plan price presence picks between
//     limit (price-targeted) and immediate (market).
//   * Zero / NULL plan price always falls through to immediate.

import (
	"database/sql"
	"testing"

	"github.com/fundai/server/internal/repository"
)

func TestSelectPMPathExecutionStrategy_QuantityGateDominates(t *testing.T) {
	cases := []struct {
		name     string
		quantity int
		price    sql.NullFloat64
		want     string
	}{
		{"big qty no price → twap", 1001, sql.NullFloat64{}, "twap"},
		{"big qty with price → twap (qty wins)", 5000, sql.NullFloat64{Valid: true, Float64: 12.30}, "twap"},
		{"at threshold (1000) → still bounded by < check, falls through", 1000, sql.NullFloat64{}, "immediate"},
		{"at threshold with price → limit", 1000, sql.NullFloat64{Valid: true, Float64: 12.30}, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := repository.PlanAction{Price: tc.price}
			got := selectPMPathExecutionStrategy(a, tc.quantity)
			if got != tc.want {
				t.Errorf("selectPMPathExecutionStrategy(qty=%d, price=%+v) = %q; want %q",
					tc.quantity, tc.price, got, tc.want)
			}
		})
	}
}

func TestSelectPMPathExecutionStrategy_SmallQtyPickedByPlanPrice(t *testing.T) {
	cases := []struct {
		name     string
		quantity int
		price    sql.NullFloat64
		want     string
	}{
		{"small qty, no price → immediate (market)", 100, sql.NullFloat64{}, "immediate"},
		{"small qty, zero price → immediate", 100, sql.NullFloat64{Valid: true, Float64: 0}, "immediate"},
		{"small qty, negative price → immediate", 100, sql.NullFloat64{Valid: true, Float64: -1}, "immediate"},
		{"small qty, positive price → limit", 100, sql.NullFloat64{Valid: true, Float64: 12.30}, "limit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := repository.PlanAction{Price: tc.price}
			got := selectPMPathExecutionStrategy(a, tc.quantity)
			if got != tc.want {
				t.Errorf("selectPMPathExecutionStrategy(qty=%d, price=%+v) = %q; want %q",
					tc.quantity, tc.price, got, tc.want)
			}
		})
	}
}

func TestNormalizePMPathStrategy(t *testing.T) {
	// Round-trip: every valid strategy normalises to itself.
	for _, s := range []string{"immediate", "limit", "twap", "vwap"} {
		if got := normalizePMPathStrategy(s); got != s {
			t.Errorf("normalizePMPathStrategy(%q) = %q; want %q", s, got, s)
		}
	}
	// Whitespace + case tolerated.
	if got := normalizePMPathStrategy("  TWAP  "); got != "twap" {
		t.Errorf("normalizePMPathStrategy mixed case/spaces = %q; want twap", got)
	}
	// Unknown collapses to immediate (safest default).
	for _, junk := range []string{"", "  ", "ladder", "unknown-strategy"} {
		if got := normalizePMPathStrategy(junk); got != "immediate" {
			t.Errorf("normalizePMPathStrategy(%q) = %q; want immediate", junk, got)
		}
	}
}
