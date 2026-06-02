package drawdown

import (
	"math"
	"testing"
	"time"
)

func TestComputeDD(t *testing.T) {
	for _, tc := range []struct {
		name             string
		peak, current, w float64
	}{
		{"peak_zero", 0, 100, 0},
		{"peak_negative", -10, 100, 0},
		{"current_at_peak", 100, 100, 0},
		{"current_above_peak", 100, 110, 0},
		{"five_pct_dd", 100, 95, -0.05},
		{"twenty_pct_dd", 100, 80, -0.20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ComputeDD(tc.peak, tc.current)
			if math.Abs(got-tc.w) > 1e-9 {
				t.Errorf("got %v want %v", got, tc.w)
			}
		})
	}
}

func newSnapshot(peak, current float64, asOf time.Time, positions ...Position) *Snapshot {
	return &Snapshot{
		FundID:     "f1",
		PeakNAV:    peak,
		CurrentNAV: current,
		AsOf:       asOf,
		Positions:  positions,
	}
}

func tier(t int, dd, ratio float64, action Action, cooldown int) Tier {
	return Tier{Tier: t, DDPct: dd, Action: action, TrimRatio: ratio, CooldownHours: cooldown}
}

func TestEvaluate_NoBreach(t *testing.T) {
	e := NewEngine()
	snap := newSnapshot(100, 99, time.Now())
	policy := &Policy{FundID: "f1", Tiers: []Tier{tier(1, -0.05, 0.25, ActionTrimProportional, 24)}}
	got, err := e.Evaluate(snap, policy)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != nil {
		t.Errorf("expected no breach: %+v", got)
	}
}

func TestEvaluate_WorstTierWins(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := newSnapshot(100, 80, now,
		Position{Symbol: "AAPL", Quantity: 100, AvgCost: 175, MarketValue: 18000},
		Position{Symbol: "MSFT", Quantity: 50, AvgCost: 380, MarketValue: 19000},
	)
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
		tier(2, -0.10, 0.50, ActionTrimProportional, 24),
		tier(3, -0.15, 0, ActionFlatten, 24),
	}}
	got, err := e.Evaluate(snap, policy)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("expected breach event")
	}
	if got.Tier != 3 {
		t.Errorf("worst tier should fire, got tier %d", got.Tier)
	}
	if got.Action != ActionFlatten {
		t.Errorf("action = %s, want flatten", got.Action)
	}
	if len(got.TrimPlan) != 2 {
		t.Errorf("flatten plan should hit both positions: %+v", got.TrimPlan)
	}
	if got.TrimPlan[0].Quantity != 100 {
		t.Errorf("flatten qty = %v", got.TrimPlan[0].Quantity)
	}
}

func TestEvaluate_TrimProportional_Plan(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := newSnapshot(100, 92, now,
		Position{Symbol: "AAPL", Quantity: 100, AvgCost: 175, MarketValue: 18000},
		Position{Symbol: "MSFT", Quantity: 7, AvgCost: 380, MarketValue: 2660},
	)
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
		tier(2, -0.10, 0.50, ActionTrimProportional, 24),
	}}
	got, _ := e.Evaluate(snap, policy)
	if got == nil || got.Tier != 1 {
		t.Fatalf("got %+v", got)
	}
	// Tier 1 = -5%, dd=-8% so tier 1 fires (tier 2 needs -10%).
	// AAPL 100 * 0.25 = 25 shares
	// MSFT 7 * 0.25 = 1.75 → floor=1
	if len(got.TrimPlan) != 2 {
		t.Fatalf("trim plan = %+v", got.TrimPlan)
	}
	if got.TrimPlan[0].Quantity != 25 {
		t.Errorf("AAPL qty = %v want 25", got.TrimPlan[0].Quantity)
	}
	if got.TrimPlan[1].Quantity != 1 {
		t.Errorf("MSFT qty = %v want 1", got.TrimPlan[1].Quantity)
	}
}

func TestEvaluate_TrimRatio_FractionalShare_Skipped(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := newSnapshot(100, 92, now,
		Position{Symbol: "TINY", Quantity: 3, AvgCost: 10, MarketValue: 30},
	)
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
	}}
	got, _ := e.Evaluate(snap, policy)
	if got == nil {
		t.Fatal("expected breach")
	}
	// 3 * 0.25 = 0.75 → floor=0 → skipped
	if len(got.TrimPlan) != 0 {
		t.Errorf("sub-share trim should produce empty plan: %+v", got.TrimPlan)
	}
}

func TestEvaluate_DefensiveOnly_EmptyPlan(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := newSnapshot(100, 95, now,
		Position{Symbol: "AAPL", Quantity: 100, AvgCost: 175, MarketValue: 18000},
	)
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0, ActionDefensiveOnly, 24),
	}}
	got, _ := e.Evaluate(snap, policy)
	if got == nil || got.Action != ActionDefensiveOnly {
		t.Fatalf("got %+v", got)
	}
	if len(got.TrimPlan) != 0 {
		t.Errorf("defensive_only must emit empty plan")
	}
}

func TestEvaluate_Cooldown_SkipsTier(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		FundID: "f1", PeakNAV: 100, CurrentNAV: 80,
		AsOf:        now,
		LastFiredAt: map[int]time.Time{2: now.Add(-1 * time.Hour)},
		Positions: []Position{
			{Symbol: "AAPL", Quantity: 100, AvgCost: 175, MarketValue: 18000},
		},
	}
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
		tier(2, -0.10, 0.50, ActionTrimProportional, 24),
	}}
	got, _ := e.Evaluate(snap, policy)
	if got == nil {
		t.Fatal("expected fall-through to tier 1")
	}
	// Tier 2 in cooldown (24h, last fired 1h ago) → skip to tier 1.
	if got.Tier != 1 {
		t.Errorf("tier = %d, expected fallback to tier 1", got.Tier)
	}
}

func TestEvaluate_Cooldown_AllTiersInCooldown_NoBreach(t *testing.T) {
	e := NewEngine()
	now := time.Date(2026, 6, 1, 14, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		FundID: "f1", PeakNAV: 100, CurrentNAV: 80,
		AsOf: now,
		LastFiredAt: map[int]time.Time{
			1: now.Add(-1 * time.Hour),
			2: now.Add(-1 * time.Hour),
		},
		Positions: []Position{
			{Symbol: "AAPL", Quantity: 100, AvgCost: 175, MarketValue: 18000},
		},
	}
	policy := &Policy{FundID: "f1", Tiers: []Tier{
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
		tier(2, -0.10, 0.50, ActionTrimProportional, 24),
	}}
	got, _ := e.Evaluate(snap, policy)
	if got != nil {
		t.Errorf("all in cooldown should produce no breach, got %+v", got)
	}
}

func TestEvaluate_FundIDMismatch_Errors(t *testing.T) {
	e := NewEngine()
	snap := newSnapshot(100, 90, time.Now())
	policy := &Policy{FundID: "f2", Tiers: []Tier{tier(1, -0.05, 0.25, ActionTrimProportional, 24)}}
	if _, err := e.Evaluate(snap, policy); err == nil {
		t.Error("expected error on fund_id mismatch")
	}
}

func TestEvaluate_EmptyPolicy_NoBreach(t *testing.T) {
	e := NewEngine()
	snap := newSnapshot(100, 50, time.Now())
	policy := &Policy{FundID: "f1", Tiers: nil}
	got, err := e.Evaluate(snap, policy)
	if err != nil || got != nil {
		t.Errorf("empty policy must not breach, got %+v / err=%v", got, err)
	}
}

func TestBuildTrimPlan_IgnoresShorts(t *testing.T) {
	plan := BuildTrimPlan([]Position{
		{Symbol: "AAPL", Quantity: -100},
		{Symbol: "MSFT", Quantity: 50},
	}, tier(1, -0.05, 0.5, ActionTrimProportional, 24))
	if len(plan) != 1 || plan[0].Symbol != "MSFT" {
		t.Errorf("plan = %+v, expected only MSFT", plan)
	}
}

func TestBuildTrimPlan_FlattenIgnoresShorts(t *testing.T) {
	plan := BuildTrimPlan([]Position{
		{Symbol: "AAPL", Quantity: -100},
		{Symbol: "MSFT", Quantity: 50},
	}, tier(3, -0.15, 0, ActionFlatten, 24))
	if len(plan) != 1 || plan[0].Quantity != 50 {
		t.Errorf("flatten plan = %+v", plan)
	}
}

func TestSortPolicyTiers(t *testing.T) {
	in := []Tier{
		tier(3, -0.15, 0, ActionFlatten, 24),
		tier(1, -0.05, 0.25, ActionTrimProportional, 24),
		tier(2, -0.10, 0.50, ActionTrimProportional, 24),
	}
	out := SortPolicyTiers(in)
	for i, w := range []int{1, 2, 3} {
		if out[i].Tier != w {
			t.Errorf("idx %d tier=%d want %d", i, out[i].Tier, w)
		}
	}
}
