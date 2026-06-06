package votepenalty

import (
	"math"
	"testing"
)

func TestEmptyVotesReturnsZero(t *testing.T) {
	got := Compute(nil, DefaultConfig())
	if got.RawCount != 0 {
		t.Errorf("RawCount: got %d, want 0", got.RawCount)
	}
	if got.PanelDir != DirNeutral {
		t.Errorf("PanelDir: got %v, want neutral", got.PanelDir)
	}
}

func TestSingleVoteIsUnpenalised(t *testing.T) {
	v := Vote{AgentID: "a", Direction: DirBuy, Confidence: 0.8}
	got := Compute([]Vote{v}, DefaultConfig())
	if got.PanelDir != DirBuy {
		t.Errorf("PanelDir: got %v, want buy", got.PanelDir)
	}
	if got.Penalties[0].EffectiveWeight != 0.8 {
		t.Errorf("single agent weight: got %v, want 0.8",
			got.Penalties[0].EffectiveWeight)
	}
}

func TestThreeIdenticalAgentsDownWeighted(t *testing.T) {
	rationale := "Earnings beat with strong margin expansion and guidance raise"
	tags := []string{"earnings_beat", "guidance_raise"}
	votes := []Vote{
		{AgentID: "bull", Direction: DirBuy, Confidence: 0.9, Rationale: rationale, ThesisTags: tags},
		{AgentID: "quant", Direction: DirBuy, Confidence: 0.9, Rationale: rationale, ThesisTags: tags},
		{AgentID: "bear", Direction: DirBuy, Confidence: 0.9, Rationale: rationale, ThesisTags: tags},
	}
	got := Compute(votes, DefaultConfig())
	// Three near-clones: ENC should be much less than 3.
	if got.EffectiveN >= 2.0 {
		t.Errorf("EffectiveN: got %v, want < 2.0 for three near-clones", got.EffectiveN)
	}
	if got.PanelDir != DirBuy {
		t.Errorf("PanelDir: got %v, want buy", got.PanelDir)
	}
	for _, p := range got.Penalties {
		if p.EffectiveWeight >= p.RawWeight {
			t.Errorf("agent %q: effective %v should be < raw %v",
				p.AgentID, p.EffectiveWeight, p.RawWeight)
		}
	}
}

func TestThreeIndependentAgentsKeepFullWeight(t *testing.T) {
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 0.9,
			Rationale: "Cash flow improving margins quality compounding leadership",
			ThesisTags: []string{"quality"}},
		{AgentID: "b", Direction: DirBuy, Confidence: 0.9,
			Rationale: "Sector rotation favours cyclicals industrial demand reviving",
			ThesisTags: []string{"sector_rotation"}},
		{AgentID: "c", Direction: DirBuy, Confidence: 0.9,
			Rationale: "Earnings beat dividend signal capital return announcement",
			ThesisTags: []string{"earnings_beat"}},
	}
	got := Compute(votes, DefaultConfig())
	// Independent rationales — three same-direction agents
	// will never reach ENC=3 (direction match contributes
	// some structural similarity), but should clear 2.0.
	if got.EffectiveN <= 2.0 {
		t.Errorf("EffectiveN: got %v, want > 2.0 for independent agents", got.EffectiveN)
	}
	if got.PanelDir != DirBuy {
		t.Errorf("PanelDir: got %v, want buy", got.PanelDir)
	}
}

func TestMixedDirectionsGoNeutral(t *testing.T) {
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 0.5},
		{AgentID: "b", Direction: DirSell, Confidence: 0.5},
	}
	got := Compute(votes, DefaultConfig())
	if got.PanelDir != DirNeutral {
		t.Errorf("PanelDir: got %v, want neutral", got.PanelDir)
	}
	if math.Abs(got.PanelScore) > 1e-9 {
		t.Errorf("PanelScore: got %v, want 0", got.PanelScore)
	}
}

func TestStructuralSimMatchesIdenticalVotes(t *testing.T) {
	a := Vote{Direction: DirBuy, Conviction: ConvictionHigh,
		ThesisTags: []string{"a", "b"}}
	b := a
	got := structuralSim(a, b)
	if got < 0.99 {
		// Identical structural votes should max out (0.25 +
		// 0.10 + 0.65*1.0 = 1.0).
		t.Errorf("structuralSim identical: got %v, want ~1.0", got)
	}
}

func TestStructuralSimZeroForOpposite(t *testing.T) {
	a := Vote{Direction: DirBuy, Conviction: ConvictionHigh,
		ThesisTags: []string{"a"}}
	b := Vote{Direction: DirSell, Conviction: ConvictionLow,
		ThesisTags: []string{"x"}}
	got := structuralSim(a, b)
	if got > 0.0 {
		t.Errorf("structuralSim opposites: got %v, want 0", got)
	}
}

func TestJaccardOnTokens(t *testing.T) {
	a := tokenise("Earnings beat strong guidance")
	b := tokenise("Guidance raised earnings beat guidance")
	got := jaccard(a, b)
	if got <= 0 || got >= 1 {
		t.Errorf("jaccard: got %v, want strictly between 0 and 1", got)
	}
}

func TestPanelScoreClampedTo1(t *testing.T) {
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 1.0},
		{AgentID: "b", Direction: DirBuy, Confidence: 1.0},
	}
	got := Compute(votes, DefaultConfig())
	if got.PanelScore > 1 || got.PanelScore < -1 {
		t.Errorf("PanelScore: got %v, want in [-1,1]", got.PanelScore)
	}
}

func TestThresholdGatesNeutral(t *testing.T) {
	cfg := DefaultConfig()
	cfg.PanelDirThreshold = 0.95
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 0.6},
		{AgentID: "b", Direction: DirNeutral, Confidence: 0.6},
	}
	got := Compute(votes, cfg)
	if got.PanelDir != DirNeutral {
		t.Errorf("PanelDir under high threshold: got %v, want neutral", got.PanelDir)
	}
}

func TestConfigNormalisation(t *testing.T) {
	cfg := Config{} // all zero
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 0.9},
		{AgentID: "b", Direction: DirBuy, Confidence: 0.9},
	}
	got := Compute(votes, cfg)
	if got.PanelScore <= 0 {
		t.Errorf("normalised config should still produce buy score, got %v", got.PanelScore)
	}
}

func TestDeterministic(t *testing.T) {
	votes := []Vote{
		{AgentID: "a", Direction: DirBuy, Confidence: 0.9, Rationale: "alpha", ThesisTags: []string{"x"}},
		{AgentID: "b", Direction: DirBuy, Confidence: 0.9, Rationale: "beta", ThesisTags: []string{"y"}},
	}
	a := Compute(votes, DefaultConfig())
	b := Compute(votes, DefaultConfig())
	if a.PanelScore != b.PanelScore || a.EffectiveN != b.EffectiveN {
		t.Errorf("non-deterministic: %+v vs %+v", a, b)
	}
}
