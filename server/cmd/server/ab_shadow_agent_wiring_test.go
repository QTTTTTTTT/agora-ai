package main

import (
	"context"
	"reflect"
	"testing"

	"github.com/fundai/server/internal/api"
)

// TestJSONEqual_Primitives covers the value-equality cases the
// diff algorithm depends on. Subtle: floats / numeric strings
// from JSON unmarshaling must not be conflated with each other.
func TestJSONEqual_Primitives(t *testing.T) {
	cases := []struct {
		name string
		a, b any
		want bool
	}{
		{"both nil", nil, nil, true},
		{"int eq", 5, 5, true},
		{"int neq", 5, 6, false},
		{"float vs int", 5.0, 5, true},
		{"string eq", "a", "a", true},
		{"string neq", "a", "b", false},
		{"slice eq", []int{1, 2}, []int{1, 2}, true},
		{"slice neq", []int{1, 2}, []int{2, 1}, false},
		{"map eq", map[string]any{"x": 1}, map[string]any{"x": 1}, true},
		{"map neq value", map[string]any{"x": 1}, map[string]any{"x": 2}, false},
		{"map neq key set", map[string]any{"x": 1}, map[string]any{"y": 1}, false},
		{"nested map eq", map[string]any{"x": []int{1}}, map[string]any{"x": []int{1}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsonEqual(tc.a, tc.b); got != tc.want {
				t.Errorf("jsonEqual(%v, %v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
		})
	}
}

// TestTotalsToDTO covers the avg/win-rate computation including
// the divide-by-zero guard for variants with no trades.
func TestTotalsToDTO(t *testing.T) {
	cases := []struct {
		name        string
		tradeCount  int
		turnover    float64
		realizedPnL float64
		winTrades   int
		want        api.ABAttributionTotals
	}{
		{
			name: "happy path",
			tradeCount: 4, turnover: 1000, realizedPnL: 200, winTrades: 3,
			want: api.ABAttributionTotals{TradeCount: 4, Turnover: 1000, RealizedPnL: 200, WinTradeRate: 0.75, AvgPnL: 50},
		},
		{
			name: "zero trades — no divide by zero",
			tradeCount: 0, turnover: 0, realizedPnL: 0, winTrades: 0,
			want: api.ABAttributionTotals{TradeCount: 0, Turnover: 0, RealizedPnL: 0, WinTradeRate: 0, AvgPnL: 0},
		},
		{
			name: "all losers",
			tradeCount: 5, turnover: 500, realizedPnL: -100, winTrades: 0,
			want: api.ABAttributionTotals{TradeCount: 5, Turnover: 500, RealizedPnL: -100, WinTradeRate: 0, AvgPnL: -20},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := totalsToDTO(struct {
				Symbol      string
				TradeCount  int
				Turnover    float64
				RealizedPnL float64
				WinTrades   int
			}{
				TradeCount:  tc.tradeCount,
				Turnover:    tc.turnover,
				RealizedPnL: tc.realizedPnL,
				WinTrades:   tc.winTrades,
			})
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestComputeABEvolutionDiff_NoOpWhenIdentical asserts the diff
// returns nil when the proposed config matches the current state
// exactly. This is the "shadow run found nothing worth promoting"
// case and must not surface a noisy diff banner in the UI.
func TestComputeABEvolutionDiff_NoOpWhenIdentical(t *testing.T) {
	s := &abTestServiceAdapter{}
	// We can't load an agent in unit tests without a DB, so this
	// test exercises the early-return path: empty proposed → nil.
	if got := s.computeABEvolutionDiff(context.Background(), "agent-x", nil); got != nil {
		t.Errorf("nil proposed should yield nil diff, got %+v", got)
	}
	if got := s.computeABEvolutionDiff(context.Background(), "agent-x", map[string]any{}); got != nil {
		t.Errorf("empty proposed should yield nil diff, got %+v", got)
	}
}

// TestSortedAgentIDs ensures the per-variant agent ordering is
// deterministic across requests; the UI relies on this for stable
// scroll position when the user toggles the panel open.
func TestSortedAgentIDs(t *testing.T) {
	in := map[string][]abShadowAgentEvent{
		"zeta-2":  {{}},
		"alpha-1": {{}},
		"beta-3":  {{}, {}},
	}
	got := sortedAgentIDs(in)
	want := []string{"alpha-1", "beta-3", "zeta-2"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// TestMaxFloatAbsFloat covers the small helpers used by the
// gap-percent normaliser. Pinning here so a careless edit
// (e.g. `if a < b` typo) shows up as a failed test.
func TestMaxFloatAbsFloat(t *testing.T) {
	if v := maxFloat(3, 5); v != 5 {
		t.Errorf("maxFloat(3,5) = %v", v)
	}
	if v := maxFloat(-3, -5); v != -3 {
		t.Errorf("maxFloat(-3,-5) = %v", v)
	}
	if v := absFloat(-2.5); v != 2.5 {
		t.Errorf("absFloat(-2.5) = %v", v)
	}
	if v := absFloat(2.5); v != 2.5 {
		t.Errorf("absFloat(2.5) = %v", v)
	}
}
