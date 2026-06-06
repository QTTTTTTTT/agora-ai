package signalbudget

import (
	"reflect"
	"sort"
	"testing"
)

func TestTier1AlwaysSelectedEvenAtTinyTargetK(t *testing.T) {
	in := Inputs{
		PresentBlocks: []string{
			"roundtableStance", "bullCase", "bearCase", "quantCase",
			"riskBudget", "exposure",
			"agentSkills", "recentLessons",
		},
	}
	res := Select(in, SelectionPolicy{TargetK: 2})
	if len(res.Selected) < 6 {
		t.Errorf("tier-1 must always be selected: got %d, want >=6", len(res.Selected))
	}
	got := res.SelectedBlocks()
	for _, want := range []string{"roundtableStance", "bullCase", "bearCase", "quantCase", "riskBudget", "exposure"} {
		if !contains(got, want) {
			t.Errorf("missing tier-1 block %q", want)
		}
	}
	if !res.OverflowOverK {
		t.Errorf("expected OverflowOverK=true with TargetK=2 but tier-1 has 6")
	}
}

func TestTopKOrderedByScore(t *testing.T) {
	in := Inputs{
		PresentBlocks: []string{
			"agentSkills", "recentLessons", "newsCatalysts",
			"sectorRotation", "macroBriefing", "lessonReplay",
			"qualityScores",
		},
	}
	res := Select(in, SelectionPolicy{TargetK: 3})
	got := res.SelectedBlocks()
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	// Tier-2 should come first by static priority.
	want := []string{"newsCatalysts", "agentSkills", "recentLessons"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Top-K: got %v, want %v", got, want)
	}
}

func TestDensityAndAlphaPushBlocksUp(t *testing.T) {
	in := Inputs{
		PresentBlocks: []string{
			"agentSkills", "recentLessons",
			"newsCatalysts", "earningsCalendar",
		},
		Density: map[string]float64{
			"agentSkills":   100, // saturates
			"recentLessons": 1,   // sparse
		},
		HistoricalAlpha: map[string]float64{
			"recentLessons": 0.05, // strong historical contributor
		},
	}
	policy := DefaultPolicy()
	policy.TargetK = 4
	res := Select(in, policy)
	got := res.SelectedBlocks()
	// All present blocks fit, just verify scoring boosts went
	// where expected.
	var skillsScore, lessonsScore float64
	for _, s := range res.Selected {
		switch s.Block {
		case "agentSkills":
			skillsScore = s.Score
		case "recentLessons":
			lessonsScore = s.Score
		}
	}
	if skillsScore <= 72 {
		t.Errorf("density should push agentSkills above its base 72: got %v", skillsScore)
	}
	if lessonsScore <= 70 {
		t.Errorf("alpha should push recentLessons above its base 70: got %v", lessonsScore)
	}
	if len(got) != 4 {
		t.Errorf("expected all 4 selected, got %d", len(got))
	}
}

func TestDroppedMarkedWithReason(t *testing.T) {
	in := Inputs{
		PresentBlocks: []string{
			"agentSkills", "macroBriefing", "qualityScores",
			"valueScores", "lowBetaScores", "pead", "cooldowns",
		},
	}
	res := Select(in, SelectionPolicy{TargetK: 2})
	if len(res.Selected) != 2 {
		t.Errorf("Selected: got %d, want 2", len(res.Selected))
	}
	if len(res.Dropped) != 5 {
		t.Errorf("Dropped: got %d, want 5", len(res.Dropped))
	}
	for _, d := range res.Dropped {
		if d.Reason != "below_budget_cutoff" {
			t.Errorf("dropped reason: got %q, want below_budget_cutoff", d.Reason)
		}
	}
}

func TestUnknownBlocksFallBelowEverything(t *testing.T) {
	in := Inputs{PresentBlocks: []string{"agentSkills", "mystery_block"}}
	res := Select(in, SelectionPolicy{TargetK: 2})
	got := res.SelectedBlocks()
	if got[0] != "agentSkills" {
		t.Errorf("first should be agentSkills, got %q", got[0])
	}
	for _, s := range res.Selected {
		if s.Block == "mystery_block" && s.Tier != TierUnknown {
			t.Errorf("mystery_block should be TierUnknown, got %v", s.Tier)
		}
	}
}

func TestEmptyInputReturnsEmptyResult(t *testing.T) {
	res := Select(Inputs{}, DefaultPolicy())
	if res.Present != 0 {
		t.Errorf("Present: got %d, want 0", res.Present)
	}
	if len(res.Selected) != 0 {
		t.Errorf("Selected: got %d, want 0", len(res.Selected))
	}
	if res.OverflowOverK {
		t.Errorf("OverflowOverK should be false for empty input")
	}
}

func TestPolicyDefaults(t *testing.T) {
	in := Inputs{PresentBlocks: []string{"agentSkills"}}
	res := Select(in, SelectionPolicy{}) // all zero
	if res.Policy.TargetK == 0 {
		t.Errorf("TargetK should be normalised to default")
	}
}

func TestDeterministicOrdering(t *testing.T) {
	// Two blocks with same tier and same priority — deterministic
	// tie-break by name.
	in := Inputs{PresentBlocks: []string{"valueScores", "qualityScores"}}
	res1 := Select(in, SelectionPolicy{TargetK: 2})
	res2 := Select(in, SelectionPolicy{TargetK: 2})
	if !reflect.DeepEqual(res1.SelectedBlocks(), res2.SelectedBlocks()) {
		t.Errorf("non-deterministic ordering: %v vs %v",
			res1.SelectedBlocks(), res2.SelectedBlocks())
	}
}

func TestFallbackBlocksStable(t *testing.T) {
	got := FallbackBlocks()
	want := []string{"roundtableStance", "riskBudget", "exposure"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("FallbackBlocks: got %v, want %v", got, want)
	}
}

func TestNormaliseDensityIsBounded(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0},
		{-5, 0},
		{1, 1.0 / 11},
		{10, 0.5},
		{1000, 1000.0 / 1010},
	}
	for _, c := range cases {
		got := normaliseDensity(c.in)
		if got != c.want {
			t.Errorf("normaliseDensity(%v): got %v, want %v", c.in, got, c.want)
		}
	}
}

func TestSelectedRespectsDescendingScore(t *testing.T) {
	in := Inputs{
		PresentBlocks: []string{
			"sectorRotation", "macroBriefing", "fundamentalSummary",
			"qualityScores", "valueScores",
		},
	}
	res := Select(in, DefaultPolicy())
	scores := make([]float64, 0, len(res.Selected))
	for _, s := range res.Selected {
		scores = append(scores, s.Score)
	}
	sorted := make([]float64, len(scores))
	copy(sorted, scores)
	sort.Sort(sort.Reverse(sort.Float64Slice(sorted)))
	if !reflect.DeepEqual(scores, sorted) {
		t.Errorf("selected not descending: %v vs %v", scores, sorted)
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
