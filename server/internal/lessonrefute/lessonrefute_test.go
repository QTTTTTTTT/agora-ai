package lessonrefute

import (
	"testing"
	"time"
)

func TestIsBadOutcomeAlphaFloor(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		alpha    float64
		drawdown bool
		want     bool
		name     string
	}{
		{alpha: 0.05, want: false, name: "good plan"},
		{alpha: -0.001, want: false, name: "noise level loss"},
		{alpha: -0.01, want: true, name: "below floor"},
		{alpha: -0.001, drawdown: true, want: true, name: "drawdown + tiny loss triggers soft floor"},
		{alpha: 0.001, drawdown: true, want: false, name: "drawdown + positive alpha is not refutation"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.IsBadOutcome(tc.alpha, tc.drawdown); got != tc.want {
				t.Errorf("IsBadOutcome(%v, %v): got %v, want %v",
					tc.alpha, tc.drawdown, got, tc.want)
			}
		})
	}
}

func TestNextStatusTransitions(t *testing.T) {
	p := DefaultPolicy()
	cases := []struct {
		current Status
		count   int
		want    Status
		name    string
	}{
		{StatusActive, 0, StatusActive, "active no refutations stays active"},
		{StatusActive, 2, StatusActive, "active under soft threshold"},
		{StatusActive, 3, StatusSoftRefuted, "active flips to soft at threshold"},
		{StatusActive, 4, StatusSoftRefuted, "active still soft below hard"},
		{StatusActive, 5, StatusHardRefuted, "active flips straight to hard at threshold"},
		{StatusSoftRefuted, 4, StatusSoftRefuted, "soft stays soft below hard"},
		{StatusSoftRefuted, 5, StatusHardRefuted, "soft flips to hard at threshold"},
		{StatusHardRefuted, 100, StatusHardRefuted, "hard never downgrades"},
		{StatusArchived, 100, StatusArchived, "archived is terminal"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := p.NextStatus(tc.current, tc.count); got != tc.want {
				t.Errorf("NextStatus(%v, %d): got %v, want %v",
					tc.current, tc.count, got, tc.want)
			}
		})
	}
}

func TestApplyDedupesPerPlan(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.SetCurrentStatus("m1", StatusActive, 0)
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	r := Refutation{MemoryID: "m1", PlanID: "p1", Alpha: -0.02, At: now}
	a1 := tr.Apply(r)
	a2 := tr.Apply(r) // same plan again — must dedupe.
	if a1.RefutationCount != 1 || a2.RefutationCount != 1 {
		t.Errorf("dedupe failed: counts %d/%d", a1.RefutationCount, a2.RefutationCount)
	}
	if !a1.WasTransitioned && a1.RecommendedNext != StatusActive {
		// 1 refutation, default soft threshold 3 — should not flip
		t.Errorf("did not expect transition at count=1, got next=%v transitioned=%v",
			a1.RecommendedNext, a1.WasTransitioned)
	}
	if a2.WasTransitioned {
		t.Errorf("dedup re-application should not re-transition")
	}
}

func TestApplyAccumulatesAcrossPlans(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.SetCurrentStatus("m1", StatusActive, 0)
	for i := 0; i < 5; i++ {
		tr.Apply(Refutation{
			MemoryID: "m1",
			PlanID:   string(rune('a' + i)),
			Alpha:    -0.02,
			At:       time.Now().UTC(),
		})
	}
	got := tr.Snapshot("m1")
	if got.RefutationCount != 5 {
		t.Errorf("RefutationCount: got %d, want 5", got.RefutationCount)
	}
	if got.RecommendedNext != StatusHardRefuted {
		t.Errorf("RecommendedNext: got %v, want %v", got.RecommendedNext, StatusHardRefuted)
	}
	if got.CurrentStatus != StatusHardRefuted {
		t.Errorf("CurrentStatus should reflect transition: got %v", got.CurrentStatus)
	}
}

func TestApplySoftThenHard(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.SetCurrentStatus("m1", StatusActive, 0)
	for i := 0; i < 3; i++ {
		tr.Apply(Refutation{MemoryID: "m1", PlanID: string(rune('a' + i)), Alpha: -0.02})
	}
	if got := tr.Snapshot("m1"); got.CurrentStatus != StatusSoftRefuted {
		t.Errorf("after 3 refutations: got %v, want %v", got.CurrentStatus, StatusSoftRefuted)
	}
	for i := 3; i < 5; i++ {
		tr.Apply(Refutation{MemoryID: "m1", PlanID: string(rune('a' + i)), Alpha: -0.02})
	}
	if got := tr.Snapshot("m1"); got.CurrentStatus != StatusHardRefuted {
		t.Errorf("after 5 refutations: got %v, want %v", got.CurrentStatus, StatusHardRefuted)
	}
}

func TestApplyIgnoresNonBadOutcome(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.SetCurrentStatus("m1", StatusActive, 0)
	for i := 0; i < 5; i++ {
		tr.Apply(Refutation{MemoryID: "m1", PlanID: string(rune('a' + i)), Alpha: 0.03})
	}
	got := tr.Snapshot("m1")
	if got.RefutationCount != 0 {
		t.Errorf("good outcomes should not refute, got %d", got.RefutationCount)
	}
}

func TestApplyEmptyKeysIgnored(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Apply(Refutation{MemoryID: "", PlanID: "p", Alpha: -0.5})
	tr.Apply(Refutation{MemoryID: "m", PlanID: "", Alpha: -0.5})
	if got := len(tr.Snapshots()); got != 0 {
		t.Errorf("expected 0 snapshots, got %d", got)
	}
}

func TestSnapshotsSorted(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Apply(Refutation{MemoryID: "z", PlanID: "p1", Alpha: -0.05})
	tr.Apply(Refutation{MemoryID: "a", PlanID: "p1", Alpha: -0.05})
	tr.Apply(Refutation{MemoryID: "m", PlanID: "p1", Alpha: -0.05})
	all := tr.Snapshots()
	if len(all) != 3 {
		t.Fatalf("want 3, got %d", len(all))
	}
	if all[0].MemoryID != "a" || all[1].MemoryID != "m" || all[2].MemoryID != "z" {
		t.Errorf("not sorted: %+v", all)
	}
}

func TestSetCurrentStatusSeedsState(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.SetCurrentStatus("m1", StatusSoftRefuted, 4)
	tr.Apply(Refutation{MemoryID: "m1", PlanID: "p1", Alpha: -0.02})
	got := tr.Snapshot("m1")
	if got.RefutationCount != 5 {
		t.Errorf("seeded count not respected, got %d", got.RefutationCount)
	}
	if got.CurrentStatus != StatusHardRefuted {
		t.Errorf("expected hard transition from seeded soft, got %v", got.CurrentStatus)
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tr *Tracker
	tr.SetCurrentStatus("m", StatusActive, 0)
	got := tr.Apply(Refutation{MemoryID: "m", PlanID: "p", Alpha: -0.5})
	if got.RefutationCount != 0 {
		t.Errorf("nil tracker should produce empty aggregate")
	}
}

func TestPolicyNormalisesGarbage(t *testing.T) {
	tr := NewTracker(Policy{
		SoftRefuteThreshold: 0,
		HardRefuteThreshold: 0,
		AlphaFloor:          0.5, // positive — must be flipped.
	})
	tr.SetCurrentStatus("m1", StatusActive, 0)
	for i := 0; i < 10; i++ {
		tr.Apply(Refutation{MemoryID: "m1", PlanID: string(rune('a' + i)), Alpha: -0.02})
	}
	got := tr.Snapshot("m1")
	if got.RefutationCount == 0 {
		t.Errorf("expected normalised AlphaFloor to allow refutations")
	}
	if got.CurrentStatus != StatusHardRefuted {
		t.Errorf("expected hard transition with normalised thresholds, got %v", got.CurrentStatus)
	}
}
