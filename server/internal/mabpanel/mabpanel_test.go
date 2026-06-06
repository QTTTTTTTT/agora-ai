package mabpanel

import (
	"math"
	"testing"
	"time"
)

func candidates(ids ...string) []Analyst {
	out := make([]Analyst, len(ids))
	for i, id := range ids {
		out[i] = Analyst{ID: id, Eligible: true}
	}
	return out
}

func TestSelectPicksTopK(t *testing.T) {
	b := NewBandit(Config{PanelSize: 2, ExplorationConstant: 0, MinPullsBeforeUCB: 0})
	for i := 0; i < 10; i++ {
		b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.05})
		b.Reward(Reward{AnalystID: "b", EffectiveAlpha: 0.02})
		b.Reward(Reward{AnalystID: "c", EffectiveAlpha: -0.01})
	}
	res := b.Select(candidates("a", "b", "c"))
	if res.K != 2 {
		t.Errorf("K: got %d, want 2", res.K)
	}
	if len(res.Selected) != 2 {
		t.Fatalf("Selected: got %d, want 2", len(res.Selected))
	}
	if res.Selected[0].AnalystID != "a" {
		t.Errorf("first: got %q, want a", res.Selected[0].AnalystID)
	}
	if res.Selected[1].AnalystID != "b" {
		t.Errorf("second: got %q, want b", res.Selected[1].AnalystID)
	}
}

func TestSelectExploresUnsampled(t *testing.T) {
	b := NewBandit(Config{PanelSize: 2, MinPullsBeforeUCB: 1})
	// Only a has been pulled — b and c should sort to the
	// top with +Inf UCB.
	for i := 0; i < 10; i++ {
		b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.10})
	}
	res := b.Select(candidates("a", "b", "c"))
	for _, s := range res.Selected {
		if s.AnalystID == "a" {
			t.Errorf("unsampled arms should outrank pulled arm a")
		}
	}
}

func TestExplorationBonusFavoursLessPulled(t *testing.T) {
	b := NewBandit(Config{PanelSize: 1, ExplorationConstant: math.Sqrt(2), MinPullsBeforeUCB: 1})
	for i := 0; i < 1000; i++ {
		b.Reward(Reward{AnalystID: "veteran", EffectiveAlpha: 0.05})
	}
	b.Reward(Reward{AnalystID: "newcomer", EffectiveAlpha: 0.06}) // similar mean, far fewer pulls
	res := b.Select(candidates("veteran", "newcomer"))
	if res.Selected[0].AnalystID != "newcomer" {
		t.Errorf("UCB should favour newcomer, got %q", res.Selected[0].AnalystID)
	}
}

func TestSelectIgnoresIneligible(t *testing.T) {
	b := NewBandit(DefaultConfig())
	cands := []Analyst{
		{ID: "a", Eligible: true},
		{ID: "b", Eligible: false},
		{ID: "c", Eligible: true},
	}
	res := b.Select(cands)
	for _, s := range res.Selected {
		if s.AnalystID == "b" {
			t.Errorf("ineligible candidate b should not be selected")
		}
	}
}

func TestSelectKLessThanCandidatesCount(t *testing.T) {
	b := NewBandit(Config{PanelSize: 5})
	res := b.Select(candidates("a", "b"))
	if len(res.Selected) != 2 {
		t.Errorf("Selected: got %d, want 2 (capped by candidate count)", len(res.Selected))
	}
}

func TestRewardEmptyAnalystIDIgnored(t *testing.T) {
	b := NewBandit(DefaultConfig())
	b.Reward(Reward{AnalystID: "", EffectiveAlpha: 0.1})
	if got := b.Snapshot(); len(got) != 0 {
		t.Errorf("empty analyst id should be ignored, got %v", got)
	}
}

func TestSnapshotSortedByID(t *testing.T) {
	b := NewBandit(DefaultConfig())
	for _, id := range []string{"c", "a", "b"} {
		b.Reward(Reward{AnalystID: id, EffectiveAlpha: 0.01})
	}
	snap := b.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snap len: got %d, want 3", len(snap))
	}
	if snap[0].AnalystID != "a" || snap[1].AnalystID != "b" || snap[2].AnalystID != "c" {
		t.Errorf("Snapshot not sorted: %+v", snap)
	}
}

func TestNilBanditIsNoOp(t *testing.T) {
	var b *Bandit
	b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.1})
	if got := b.Select(candidates("a")); got.K != 0 {
		t.Errorf("nil bandit Select: got K=%d, want 0", got.K)
	}
}

func TestDeterministicTieBreakByID(t *testing.T) {
	b := NewBandit(Config{PanelSize: 2, ExplorationConstant: 0, MinPullsBeforeUCB: 0})
	for _, id := range []string{"x", "z", "y"} {
		b.Reward(Reward{AnalystID: id, EffectiveAlpha: 0.05})
	}
	res1 := b.Select(candidates("x", "y", "z"))
	res2 := b.Select(candidates("z", "y", "x"))
	if len(res1.Selected) != 2 || len(res2.Selected) != 2 {
		t.Fatalf("len mismatch")
	}
	for i := 0; i < 2; i++ {
		if res1.Selected[i].AnalystID != res2.Selected[i].AnalystID {
			t.Errorf("non-deterministic at %d: %q vs %q",
				i, res1.Selected[i].AnalystID, res2.Selected[i].AnalystID)
		}
	}
}

func TestStatsLastPulledTracksLatest(t *testing.T) {
	b := NewBandit(DefaultConfig())
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(7 * 24 * time.Hour)
	b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.01, At: t1})
	b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.01, At: t2})
	b.Reward(Reward{AnalystID: "a", EffectiveAlpha: 0.01, At: t1})
	snap := b.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap: got %d", len(snap))
	}
	if !snap[0].LastPulled.Equal(t2) {
		t.Errorf("LastPulled: got %v, want %v", snap[0].LastPulled, t2)
	}
}

func TestConfigNormalisation(t *testing.T) {
	b := NewBandit(Config{}) // all zero
	res := b.Select(candidates("a", "b", "c", "d"))
	if res.K == 0 {
		t.Errorf("normalised PanelSize should be > 0")
	}
}
