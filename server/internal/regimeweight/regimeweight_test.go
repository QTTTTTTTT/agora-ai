package regimeweight

import (
	"math"
	"testing"
)

func TestEmptyTrackerWeightIsOne(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	if got := tr.Weight("a", "trend_up"); got != 1.0 {
		t.Errorf("Weight on empty tracker: got %v, want 1.0", got)
	}
}

func TestUnknownAgentWeightIsOne(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	for i := 0; i < 20; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "trend_up", Hit: true, Alpha: 0.04})
	}
	if got := tr.Weight("unknown", "trend_up"); got != 1.0 {
		t.Errorf("unknown agent: got %v, want 1.0", got)
	}
}

func TestUnknownRegimeWeightIsOne(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	for i := 0; i < 20; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "trend_up", Hit: true, Alpha: 0.04})
	}
	if got := tr.Weight("a", "chop"); got != 1.0 {
		t.Errorf("unknown regime: got %v, want 1.0", got)
	}
}

func TestStrongAgentInRegimeBoosted(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	// Agent A is great in trend_up, mediocre everywhere else.
	for i := 0; i < 30; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "trend_up", Hit: true, Alpha: 0.05})
	}
	// Agent B is mediocre in trend_up.
	for i := 0; i < 30; i++ {
		tr.Record(Observation{AgentID: "b", Regime: "trend_up", Hit: i%2 == 0, Alpha: 0.0})
	}
	// Mediocre baseline pulls regime mean down to ~0.025.
	wA := tr.Weight("a", "trend_up")
	wB := tr.Weight("b", "trend_up")
	if wA <= 1.0 {
		t.Errorf("strong agent in regime should be boosted, got %v", wA)
	}
	if wB >= 1.0 {
		t.Errorf("mediocre agent should be discounted, got %v", wB)
	}
	if wA <= wB {
		t.Errorf("ordering: wA=%v should be > wB=%v", wA, wB)
	}
}

func TestBoostsAreCapped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxBoost = 0.5
	cfg.MaxPenalty = 0.5
	tr := NewTracker(cfg)
	for i := 0; i < 100; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "r", Hit: true, Alpha: 1.0})
	}
	for i := 0; i < 100; i++ {
		tr.Record(Observation{AgentID: "b", Regime: "r", Hit: false, Alpha: -1.0})
	}
	wA := tr.Weight("a", "r")
	wB := tr.Weight("b", "r")
	if wA > 1.0+cfg.MaxBoost+1e-9 {
		t.Errorf("boost cap violated: got %v, want <= %v", wA, 1.0+cfg.MaxBoost)
	}
	if wB < 1.0-cfg.MaxPenalty-1e-9 {
		t.Errorf("penalty cap violated: got %v, want >= %v", wB, 1.0-cfg.MaxPenalty)
	}
}

func TestMinDecisionsFloorBlocksMicroSamples(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.Record(Observation{AgentID: "lucky", Regime: "r", Hit: true, Alpha: 0.50})
	tr.Record(Observation{AgentID: "lucky", Regime: "r", Hit: true, Alpha: 0.50})
	if got := tr.Weight("lucky", "r"); got != 1.0 {
		t.Errorf("micro-sample weight: got %v, want 1.0 (min_decisions floor)", got)
	}
	for i := 0; i < 10; i++ {
		tr.Record(Observation{AgentID: "noise", Regime: "r", Hit: false, Alpha: 0.0})
	}
	tr.Record(Observation{AgentID: "lucky", Regime: "r", Hit: true, Alpha: 0.50})
	if got := tr.Weight("lucky", "r"); got <= 1.0 {
		t.Errorf("after 3 decisions: got %v, want > 1.0 with positive alpha", got)
	}
}

func TestShrinkPriorPullsMicroSampleTowardMean(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinDecisionsForBoost = 3
	cfg.ShrinkPrior = 50 // very strong prior
	tr := NewTracker(cfg)
	for i := 0; i < 100; i++ {
		tr.Record(Observation{AgentID: "noise", Regime: "r", Hit: false, Alpha: 0.0})
	}
	for i := 0; i < 5; i++ {
		tr.Record(Observation{AgentID: "newcomer", Regime: "r", Hit: true, Alpha: 0.20})
	}
	w := tr.Weight("newcomer", "r")
	if w >= 1.30 {
		t.Errorf("strong prior should keep weight close to 1.0, got %v", w)
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	for i := 0; i < 5; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "r1", Hit: true, Alpha: 0.05})
	}
	for i := 0; i < 5; i++ {
		tr.Record(Observation{AgentID: "b", Regime: "r2", Hit: false, Alpha: -0.02})
	}
	snap := tr.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot len: got %d, want 2", len(snap))
	}
	if snap[0].AgentID != "a" || snap[0].Regime != "r1" {
		t.Errorf("snapshot ordering: got %+v", snap)
	}
	wA := tr.Weight("a", "r1")
	wB := tr.Weight("b", "r2")

	tr2 := NewTracker(DefaultConfig())
	tr2.LoadSnapshot(snap)
	if got := tr2.Weight("a", "r1"); math.Abs(got-wA) > 1e-9 {
		t.Errorf("round-trip Weight(a, r1): got %v, want %v", got, wA)
	}
	if got := tr2.Weight("b", "r2"); math.Abs(got-wB) > 1e-9 {
		t.Errorf("round-trip Weight(b, r2): got %v, want %v", got, wB)
	}
}

func TestLoadSnapshotIgnoresEmptyKeys(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.LoadSnapshot([]Stats{
		{AgentID: "", Regime: "r", DecisionsCount: 5, SumAlpha: 0.3, HitsCount: 3},
		{AgentID: "a", Regime: "", DecisionsCount: 5, SumAlpha: 0.3, HitsCount: 3},
		{AgentID: "a", Regime: "r", DecisionsCount: 5, SumAlpha: 0.3, HitsCount: 3},
	})
	if got := len(tr.Snapshot()); got != 1 {
		t.Errorf("LoadSnapshot accepted invalid rows: got %d, want 1", got)
	}
}

func TestRecordIgnoresEmptyKeys(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.Record(Observation{AgentID: "", Regime: "r"})
	tr.Record(Observation{AgentID: "a", Regime: ""})
	if got := len(tr.Snapshot()); got != 0 {
		t.Errorf("Record accepted empty keys: got %d, want 0", got)
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tr *Tracker
	tr.Record(Observation{AgentID: "a", Regime: "r"})
	if got := tr.Weight("a", "r"); got != 1.0 {
		t.Errorf("nil tracker Weight: got %v, want 1.0", got)
	}
	if got := tr.Snapshot(); got != nil {
		t.Errorf("nil tracker Snapshot: got %v, want nil", got)
	}
}

func TestConfigNormalisation(t *testing.T) {
	tr := NewTracker(Config{}) // all zero
	for i := 0; i < 30; i++ {
		tr.Record(Observation{AgentID: "a", Regime: "r", Hit: true, Alpha: 0.10})
	}
	for i := 0; i < 30; i++ {
		tr.Record(Observation{AgentID: "b", Regime: "r", Hit: false, Alpha: 0.0})
	}
	if got := tr.Weight("a", "r"); got <= 1.0 {
		t.Errorf("normalised config should still apply boost, got %v", got)
	}
}
