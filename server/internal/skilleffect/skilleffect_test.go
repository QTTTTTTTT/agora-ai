package skilleffect

import (
	"math"
	"testing"
	"time"
)

func TestRecommendationKeepWhenStrong(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	for i := 0; i < 50; i++ {
		tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true, Alpha: 0.02})
	}
	got := tr.Snapshot("a", "s")
	if got.Recommendation != RecommendationKeep {
		t.Errorf("Recommendation: got %q, want %q", got.Recommendation, RecommendationKeep)
	}
	if got.SampleCount != 50 {
		t.Errorf("SampleCount: got %d, want 50", got.SampleCount)
	}
	if got.HitRate != 1.0 {
		t.Errorf("HitRate: got %v, want 1.0", got.HitRate)
	}
	if math.Abs(got.MeanAlpha-0.02) > 1e-9 {
		t.Errorf("MeanAlpha: got %v, want 0.02", got.MeanAlpha)
	}
}

func TestRecommendationDeprecateAfterFullSampleAndPoorPerformance(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	// 30 obs (above MinSampleSize=25), 30% hit rate, alpha
	// negative — should trip Deprecate.
	for i := 0; i < 30; i++ {
		hit := i%10 < 3 // 30% hits
		tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: hit, Alpha: -0.01})
	}
	got := tr.Snapshot("a", "s")
	if got.Recommendation != RecommendationDeprecate {
		t.Errorf("Recommendation: got %q, want %q", got.Recommendation, RecommendationDeprecate)
	}
	if got.HitRate >= DefaultPolicy().HitRateFloor {
		t.Errorf("HitRate %v should be below floor %v", got.HitRate, DefaultPolicy().HitRateFloor)
	}
}

func TestRecommendationThrottleEarly(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	// 10 obs, 30% hit rate, negative alpha — too few to fully
	// deprecate but enough to throttle.
	for i := 0; i < 10; i++ {
		hit := i%10 < 3
		tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: hit, Alpha: -0.01})
	}
	got := tr.Snapshot("a", "s")
	if got.Recommendation != RecommendationThrottle {
		t.Errorf("Recommendation: got %q, want %q", got.Recommendation, RecommendationThrottle)
	}
}

func TestRecommendationKeepWhenLowSampleAndUnclear(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: false, Alpha: 0})
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true, Alpha: 0.01})
	got := tr.Snapshot("a", "s")
	if got.Recommendation != RecommendationKeep {
		t.Errorf("Recommendation: got %q, want %q (too few samples to throttle)",
			got.Recommendation, RecommendationKeep)
	}
}

func TestSnapshotsSortedByAgentThenSkill(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Record(Observation{AgentID: "b", SkillKey: "s2", Hit: true, Alpha: 0})
	tr.Record(Observation{AgentID: "a", SkillKey: "s2", Hit: true, Alpha: 0})
	tr.Record(Observation{AgentID: "a", SkillKey: "s1", Hit: true, Alpha: 0})

	all := tr.Snapshots()
	if len(all) != 3 {
		t.Fatalf("expected 3 aggregates, got %d", len(all))
	}
	if all[0].AgentID != "a" || all[0].SkillKey != "s1" {
		t.Errorf("first: got (%q, %q), want (a, s1)", all[0].AgentID, all[0].SkillKey)
	}
	if all[1].AgentID != "a" || all[1].SkillKey != "s2" {
		t.Errorf("second: got (%q, %q), want (a, s2)", all[1].AgentID, all[1].SkillKey)
	}
	if all[2].AgentID != "b" || all[2].SkillKey != "s2" {
		t.Errorf("third: got (%q, %q), want (b, s2)", all[2].AgentID, all[2].SkillKey)
	}
}

func TestRecordIgnoresEmptyKeys(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Record(Observation{AgentID: "", SkillKey: "s", Hit: true})
	tr.Record(Observation{AgentID: "a", SkillKey: "", Hit: true})
	if got := len(tr.Snapshots()); got != 0 {
		t.Errorf("expected empty snapshots for blank keys, got %d", got)
	}
}

func TestNilTrackerIsNoOp(t *testing.T) {
	var tr *Tracker
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true})
	got := tr.Snapshot("a", "s")
	if got.SampleCount != 0 {
		t.Errorf("nil tracker should produce zero aggregate, got SampleCount=%d", got.SampleCount)
	}
}

func TestSnapshotMissingKeyReturnsKeep(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	got := tr.Snapshot("a", "unknown")
	if got.Recommendation != RecommendationKeep {
		t.Errorf("Recommendation: got %q, want %q", got.Recommendation, RecommendationKeep)
	}
	if got.SampleCount != 0 {
		t.Errorf("SampleCount: got %d, want 0", got.SampleCount)
	}
}

func TestLastUsedAtTracksLatest(t *testing.T) {
	tr := NewTracker(DefaultPolicy())
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true, At: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)})
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true, At: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)})
	tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: true, At: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)})
	got := tr.Snapshot("a", "s")
	want := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	if !got.LastUsedAt.Equal(want) {
		t.Errorf("LastUsedAt: got %v, want %v", got.LastUsedAt, want)
	}
}

func TestPolicyNormalisation(t *testing.T) {
	tr := NewTracker(Policy{}) // all zero — should fall back to defaults.
	for i := 0; i < 30; i++ {
		hit := i%10 < 2
		tr.Record(Observation{AgentID: "a", SkillKey: "s", Hit: hit, Alpha: -0.05})
	}
	got := tr.Snapshot("a", "s")
	if got.Recommendation != RecommendationDeprecate {
		t.Errorf("Recommendation: got %q, want %q (policy should normalise to defaults)",
			got.Recommendation, RecommendationDeprecate)
	}
}
