package shadowdelta

import (
	"strings"
	"testing"
	"time"
)

func TestRecordDeltaIgnoresIdenticalActions(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	d := Delta{
		PlanID: "p1", FundID: "f1", Symbol: "AAPL",
		ProductionAction: "buy", ShadowAction: "buy",
		ProductionWeight: 0.05, ShadowWeight: 0.05,
	}
	if tr.RecordDelta(d) {
		t.Errorf("identical action+weight should be ignored")
	}
}

func TestRecordDeltaAcceptsDivergence(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	d := Delta{
		PlanID: "p1", FundID: "f1", Symbol: "AAPL",
		ProductionAction: "buy", ShadowAction: "sell",
	}
	if !tr.RecordDelta(d) {
		t.Errorf("divergent action should be recorded")
	}
	if got := len(tr.PendingDeltas()); got != 1 {
		t.Errorf("PendingDeltas: got %d, want 1", got)
	}
}

func TestResolveOutcomeStagesWinningShadow(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.RecordDelta(Delta{
		PlanID: "p1", FundID: "f1", Symbol: "AAPL",
		ProductionAction: "buy", ShadowAction: "sell",
		ShadowAgentID: "shadow1", StrategyKey: "alpha-flip",
	})
	now := time.Now()
	ok := tr.ResolveOutcome(Outcome{
		PlanID:          "p1",
		ProductionAlpha: -0.02,
		ShadowAlpha:     0.03,
		Resolved:        now,
	}, now)
	if !ok {
		t.Errorf("winning shadow should be staged")
	}
	staged := tr.PeekStaged()
	if len(staged) != 1 {
		t.Fatalf("staged: got %d, want 1", len(staged))
	}
	if staged[0].AlphaDelta < 0.04 {
		t.Errorf("AlphaDelta: got %v, want ~0.05", staged[0].AlphaDelta)
	}
	if !strings.Contains(staged[0].Title, "AAPL") {
		t.Errorf("title should mention symbol: %q", staged[0].Title)
	}
}

func TestResolveOutcomeIgnoresLoss(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.RecordDelta(Delta{PlanID: "p1", FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
	if tr.ResolveOutcome(Outcome{PlanID: "p1", ProductionAlpha: 0.05, ShadowAlpha: -0.01}, time.Now()) {
		t.Errorf("losing shadow should not stage a lesson")
	}
}

func TestResolveOutcomeIgnoresMarginalDelta(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinAlphaDelta = 0.01
	tr := NewTracker(cfg)
	tr.RecordDelta(Delta{PlanID: "p1", FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
	// Δ = 0.005 — below threshold.
	if tr.ResolveOutcome(Outcome{PlanID: "p1", ProductionAlpha: 0.0, ShadowAlpha: 0.005}, time.Now()) {
		t.Errorf("marginal delta should not stage")
	}
}

func TestResolveOutcomeRequiresMinSampleAge(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MinSampleAge = time.Hour
	tr := NewTracker(cfg)
	tr.RecordDelta(Delta{PlanID: "p1", FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
	now := time.Now()
	// Outcome resolved 5 minutes ago — too fresh to surface.
	if tr.ResolveOutcome(Outcome{PlanID: "p1", ProductionAlpha: 0.0, ShadowAlpha: 0.05, Resolved: now.Add(-5 * time.Minute)}, now) {
		t.Errorf("fresh outcome should not stage")
	}
}

func TestPendingDeltasClearedOnResolution(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.RecordDelta(Delta{PlanID: "p1", FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
	tr.ResolveOutcome(Outcome{PlanID: "p1", ProductionAlpha: -0.02, ShadowAlpha: 0.03}, time.Now())
	if got := len(tr.PendingDeltas()); got != 0 {
		t.Errorf("PendingDeltas after resolve: got %d, want 0", got)
	}
}

func TestMaxStagingPerFundEnforced(t *testing.T) {
	cfg := DefaultConfig()
	cfg.MaxStagingPerFund = 2
	tr := NewTracker(cfg)
	now := time.Now()
	for i := 0; i < 5; i++ {
		id := "p" + string(rune('a'+i))
		tr.RecordDelta(Delta{PlanID: id, FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
		tr.ResolveOutcome(Outcome{PlanID: id, ProductionAlpha: 0, ShadowAlpha: 0.05, Resolved: now}, now.Add(time.Duration(i)*time.Second))
	}
	if got := len(tr.PeekStaged()); got != 2 {
		t.Errorf("staged: got %d, want 2 (cap)", got)
	}
}

func TestDrainStagedClearsQueue(t *testing.T) {
	tr := NewTracker(DefaultConfig())
	tr.RecordDelta(Delta{PlanID: "p1", FundID: "f1", ProductionAction: "buy", ShadowAction: "sell"})
	tr.ResolveOutcome(Outcome{PlanID: "p1", ProductionAlpha: 0, ShadowAlpha: 0.05}, time.Now())
	got := tr.DrainStaged()
	if len(got) != 1 {
		t.Errorf("Drain: got %d, want 1", len(got))
	}
	if len(tr.PeekStaged()) != 0 {
		t.Errorf("queue should be empty after Drain")
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tr *Tracker
	if tr.RecordDelta(Delta{PlanID: "p1"}) {
		t.Errorf("nil tracker RecordDelta should be false")
	}
	if tr.PeekStaged() != nil {
		t.Errorf("nil tracker PeekStaged should be nil")
	}
}

func TestBuildBodyContainsAllParts(t *testing.T) {
	d := Delta{Symbol: "AAPL", ProductionAction: "buy", ShadowAction: "sell",
		StrategyKey: "alpha", ProductionReason: "earnings_beat",
		ShadowReason: "guidance_cut"}
	body := buildBody(d, Outcome{ProductionAlpha: -0.02, ShadowAlpha: 0.03}, 0.05)
	for _, want := range []string{"AAPL", "buy", "sell", "alpha", "earnings_beat", "guidance_cut"} {
		if !strings.Contains(body, want) {
			t.Errorf("body should contain %q: %q", want, body)
		}
	}
}

func TestFormatPercent(t *testing.T) {
	cases := []struct {
		in   float64
		want string
	}{
		{0.05, "+5.00%"},
		{-0.025, "-2.50%"},
		{0, "+0.00%"},
	}
	for _, c := range cases {
		if got := formatPercent(c.in); got != c.want {
			t.Errorf("formatPercent(%v): got %q, want %q", c.in, got, c.want)
		}
	}
}
