package reflection

import (
	"testing"
	"time"
)

func TestSubmitEnqueuesNewTrigger(t *testing.T) {
	c := NewCoordinator(DefaultConfig())
	ok := c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: time.Now()})
	if !ok {
		t.Errorf("Submit: got false, want true on first submission")
	}
	if c.Stats().Pending != 1 {
		t.Errorf("Pending: got %d, want 1", c.Stats().Pending)
	}
}

func TestCoolDownSuppressesDuplicate(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: time.Hour, MaxPending: 10})
	now := time.Now()
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: now})
	ok := c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: now.Add(5 * time.Minute)})
	if ok {
		t.Errorf("second submission within cool-down should be suppressed")
	}
	if c.Stats().Suppressed != 1 {
		t.Errorf("Suppressed: got %d, want 1", c.Stats().Suppressed)
	}
}

func TestCoolDownAllowsAfterElapse(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: time.Hour, MaxPending: 10})
	now := time.Now()
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: now})
	ok := c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: now.Add(2 * time.Hour)})
	if !ok {
		t.Errorf("submission after cool-down should be enqueued")
	}
}

func TestDifferentFundsAreIndependent(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: time.Hour, MaxPending: 10})
	now := time.Now()
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: now})
	ok := c.Submit(Trigger{Kind: KindDrawdown, FundID: "f2", At: now})
	if !ok {
		t.Errorf("different fund should not be suppressed")
	}
}

func TestRejectIsPerPlan(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: time.Hour, MaxPending: 10})
	now := time.Now()
	if !c.Submit(Trigger{Kind: KindReject, PlanID: "p1", At: now}) {
		t.Errorf("first reject should enqueue")
	}
	if !c.Submit(Trigger{Kind: KindReject, PlanID: "p2", At: now}) {
		t.Errorf("different plan should not be suppressed")
	}
	if c.Submit(Trigger{Kind: KindReject, PlanID: "p1", At: now.Add(time.Minute)}) {
		t.Errorf("same plan within cool-down should be suppressed")
	}
}

func TestLessonDecayIsPerLesson(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: time.Hour, MaxPending: 10})
	now := time.Now()
	c.SubmitLessonDecay("f1", "L1", "a1", 30, now)
	c.SubmitLessonDecay("f1", "L2", "a1", 30, now)
	if c.Stats().Pending != 2 {
		t.Errorf("Pending: got %d, want 2", c.Stats().Pending)
	}
}

func TestDrainSortedByTime(t *testing.T) {
	c := NewCoordinator(DefaultConfig())
	t0 := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: t0.Add(2 * time.Hour)})
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f2", At: t0})
	c.Submit(Trigger{Kind: KindReject, PlanID: "p1", At: t0.Add(time.Hour)})
	got := c.Drain()
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].At.After(got[i].At) {
			t.Errorf("Drain order: %v after %v", got[i-1].At, got[i].At)
		}
	}
}

func TestDrainClearsQueue(t *testing.T) {
	c := NewCoordinator(DefaultConfig())
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: time.Now()})
	c.Drain()
	if c.Stats().Pending != 0 {
		t.Errorf("after Drain Pending: got %d, want 0", c.Stats().Pending)
	}
}

func TestMaxPendingDropsOldest(t *testing.T) {
	c := NewCoordinator(Config{CoolDown: 0, MaxPending: 2})
	now := time.Now()
	c.Submit(Trigger{Kind: KindReject, PlanID: "p1", At: now})
	c.Submit(Trigger{Kind: KindReject, PlanID: "p2", At: now.Add(time.Second)})
	c.Submit(Trigger{Kind: KindReject, PlanID: "p3", At: now.Add(2 * time.Second)})
	if c.Stats().Pending != 2 {
		t.Errorf("Pending: got %d, want 2", c.Stats().Pending)
	}
	if c.Stats().Dropped != 1 {
		t.Errorf("Dropped: got %d, want 1", c.Stats().Dropped)
	}
	got := c.Drain()
	for _, tr := range got {
		if tr.PlanID == "p1" {
			t.Errorf("p1 should have been dropped (oldest)")
		}
	}
}

func TestSubmitRejectsEmptyKind(t *testing.T) {
	c := NewCoordinator(DefaultConfig())
	if c.Submit(Trigger{}) {
		t.Errorf("empty kind should be rejected")
	}
}

func TestNilCoordinatorIsNoOp(t *testing.T) {
	var c *Coordinator
	if c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1"}) {
		t.Errorf("nil coordinator Submit should be false")
	}
	if c.Drain() != nil {
		t.Errorf("nil coordinator Drain should be nil")
	}
}

func TestSugarHelpersStampMetadata(t *testing.T) {
	c := NewCoordinator(DefaultConfig())
	now := time.Now()
	c.SubmitLessonDecay("f1", "L1", "a1", 47, now)
	got := c.Drain()
	if len(got) != 1 {
		t.Fatalf("len: got %d, want 1", len(got))
	}
	if got[0].Metadata["days_since_hit"] != "47" {
		t.Errorf("metadata: got %q, want 47", got[0].Metadata["days_since_hit"])
	}
}

func TestConfigNormalisation(t *testing.T) {
	c := NewCoordinator(Config{}) // zero
	c.Submit(Trigger{Kind: KindDrawdown, FundID: "f1", At: time.Now()})
	if c.Stats().Pending == 0 {
		t.Errorf("normalised config should still accept submissions")
	}
}
