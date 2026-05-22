package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fundai/server/internal/repository"
)

// TestBuildPromotedReflectionMemories_ProducesOnePerLesson — sanity check
// that the builder emits one long_term memory per unique lesson, tagged
// for downstream discoverability.
func TestBuildPromotedReflectionMemoriesProducesOnePerLesson(t *testing.T) {
	learning := abShadowAgentLearning{
		Lessons:           []string{"lesson-1", "lesson-2", "lesson-1"},
		LatestTradingDate: "2026-05-15",
	}
	mems := buildPromotedReflectionMemories("fund-control", "agent-x", learning, "test-1", "B")
	if len(mems) != 2 {
		t.Fatalf("expected 2 unique-lesson memories, got %d", len(mems))
	}
	for _, m := range mems {
		if m.FundID != "fund-control" || !m.AgentID.Valid || m.AgentID.String != "agent-x" {
			t.Fatalf("unexpected fund/agent assignment: %+v", m)
		}
		if m.Layer != reflectionMemoryLayer {
			t.Fatalf("expected layer=long_term, got %q", m.Layer)
		}
		hasAB, hasTest, hasVariant := false, false, false
		for _, tag := range m.Tags {
			switch tag {
			case "ab_promotion":
				hasAB = true
			case "ab:test-1":
				hasTest = true
			case "variant:B":
				hasVariant = true
			}
		}
		if !hasAB || !hasTest || !hasVariant {
			t.Fatalf("expected ab_promotion/ab:test-1/variant:B tags, got %v", m.Tags)
		}
		if !m.TradingDate.Valid {
			t.Fatalf("expected trading_date to be parsed from 2026-05-15")
		}
		if !strings.HasPrefix(m.Title.String, "A/B promoted reflection") {
			t.Fatalf("unexpected title prefix: %q", m.Title.String)
		}
	}
}

// TestBuildPromotedReflectionMemoriesEmptyWhenNoLessons ensures we don't
// emit empty rows when the shadow run produced no lessons; the rollback
// flow depends on "no inputs → no outputs → no rollback work".
func TestBuildPromotedReflectionMemoriesEmptyWhenNoLessons(t *testing.T) {
	learning := abShadowAgentLearning{Lessons: nil, LatestTradingDate: "2026-05-15"}
	if got := buildPromotedReflectionMemories("fund", "agent", learning, "t", "B"); len(got) != 0 {
		t.Fatalf("expected zero memories, got %d", len(got))
	}
	if got := buildPromotedReflectionMemories("", "agent", abShadowAgentLearning{Lessons: []string{"x"}}, "t", "B"); len(got) != 0 {
		t.Fatalf("expected zero memories when fundID empty, got %d", len(got))
	}
}

// TestBuildPromotedSkillCandidatesDerivesFromAdjustments verifies that
// the candidate-skill payloads carry status=proposed + role-scoped match,
// matching the F4 safety contract (a buggy promotion can never bypass
// the human approval gate or leak into other roles).
func TestBuildPromotedSkillCandidatesDerivesFromAdjustments(t *testing.T) {
	learning := abShadowAgentLearning{
		Adjustments: []string{"avoid weekend rebalances", "scale into trends slowly"},
	}
	got := buildPromotedSkillCandidates(learning, "trader", "test-1", "B")
	if len(got) != 2 {
		t.Fatalf("expected 2 candidate skills, got %d", len(got))
	}
	for _, skill := range got {
		if skill.Status != skillStatusProposed {
			t.Fatalf("expected status=proposed, got %q", skill.Status)
		}
		if skill.Enabled == nil || *skill.Enabled {
			t.Fatalf("expected enabled=false (pointer to false), got %v", skill.Enabled)
		}
		if !strings.HasPrefix(skill.Source, "ab_promotion:test-1:B") {
			t.Fatalf("unexpected source: %q", skill.Source)
		}
		if !strings.HasPrefix(skill.Key, "ab_promotion:test-1:B:") {
			t.Fatalf("unexpected key prefix: %q", skill.Key)
		}
		if len(skill.Match.Roles) != 1 || skill.Match.Roles[0] != "trader" {
			t.Fatalf("expected match.roles=[trader], got %v", skill.Match.Roles)
		}
	}
}

// TestBuildPromotedSkillCandidatesEmpty — no adjustments → no candidates.
func TestBuildPromotedSkillCandidatesEmpty(t *testing.T) {
	if got := buildPromotedSkillCandidates(abShadowAgentLearning{}, "trader", "t", "B"); len(got) != 0 {
		t.Fatalf("expected 0 candidates, got %d", len(got))
	}
}

// TestMergePromotedSkillsIntoConfigMergePreservesExisting verifies the
// default mode=merge keeps user-authored skills alongside the new
// candidates.
func TestMergePromotedSkillsIntoConfigMergePreservesExisting(t *testing.T) {
	enabled := true
	prev := parsedSkillConfig{Enabled: true, Skills: []parsedSkillEntry{
		{Key: "manual-1", Name: "Hand-authored", Status: skillStatusApproved, Enabled: &enabled},
	}}
	prevRaw, _ := json.Marshal(prev)

	disabled := false
	candidates := []parsedSkillEntry{
		{Key: "ab_promotion:t:B:abc", Name: "AB1", Status: skillStatusProposed, Enabled: &disabled, Source: "ab_promotion:t:B"},
	}
	outRaw, inserted, err := mergePromotedSkillsIntoConfig(prevRaw, candidates, abPromotionModeMerge)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if len(inserted) != 1 || inserted[0] != "ab_promotion:t:B:abc" {
		t.Fatalf("expected inserted [ab_promotion:t:B:abc], got %v", inserted)
	}

	var out parsedSkillConfig
	if err := json.Unmarshal(outRaw, &out); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	if len(out.Skills) != 2 {
		t.Fatalf("expected 2 skills (manual + candidate), got %d", len(out.Skills))
	}
	if out.Skills[0].Key != "manual-1" {
		t.Fatalf("expected manual skill preserved at index 0, got %q", out.Skills[0].Key)
	}
}

// TestMergePromotedSkillsIntoConfigOverwriteDropsPreviousABEntries
// confirms that mode=overwrite removes prior ab_promotion:* entries
// before appending the new ones — preventing cumulative bloat across
// re-runs of the same A/B test.
func TestMergePromotedSkillsIntoConfigOverwriteDropsPreviousABEntries(t *testing.T) {
	disabled := false
	prev := parsedSkillConfig{Enabled: true, Skills: []parsedSkillEntry{
		{Key: "manual-1", Name: "Hand-authored", Status: skillStatusApproved},
		{Key: "ab_promotion:t:B:old", Source: "ab_promotion:t:B", Status: skillStatusProposed, Enabled: &disabled},
	}}
	prevRaw, _ := json.Marshal(prev)

	candidates := []parsedSkillEntry{
		{Key: "ab_promotion:t:B:new", Source: "ab_promotion:t:B", Status: skillStatusProposed, Enabled: &disabled},
	}
	outRaw, inserted, err := mergePromotedSkillsIntoConfig(prevRaw, candidates, abPromotionModeOverwrite)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if len(inserted) != 1 || inserted[0] != "ab_promotion:t:B:new" {
		t.Fatalf("expected inserted [ab_promotion:t:B:new], got %v", inserted)
	}

	var out parsedSkillConfig
	if err := json.Unmarshal(outRaw, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(out.Skills) != 2 {
		t.Fatalf("expected 2 skills (manual + new candidate), got %d (%+v)", len(out.Skills), out.Skills)
	}
	for _, s := range out.Skills {
		if s.Key == "ab_promotion:t:B:old" {
			t.Fatalf("old ab_promotion entry should have been dropped, but is still present")
		}
	}
}

// TestMergePromotedSkillsIntoConfigDuplicateKey verifies that a candidate
// key already present in skill_config is reported as inserted (so the
// rollback bookkeeping records it) but not duplicated. This makes a retry
// of a half-failed promotion safe.
func TestMergePromotedSkillsIntoConfigDuplicateKey(t *testing.T) {
	disabled := false
	prev := parsedSkillConfig{Enabled: true, Skills: []parsedSkillEntry{
		{Key: "ab_promotion:t:B:dup", Source: "ab_promotion:t:B", Status: skillStatusProposed, Enabled: &disabled},
	}}
	prevRaw, _ := json.Marshal(prev)

	candidates := []parsedSkillEntry{
		{Key: "ab_promotion:t:B:dup", Source: "ab_promotion:t:B", Status: skillStatusProposed, Enabled: &disabled},
	}
	outRaw, inserted, err := mergePromotedSkillsIntoConfig(prevRaw, candidates, abPromotionModeMerge)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if len(inserted) != 1 || inserted[0] != "ab_promotion:t:B:dup" {
		t.Fatalf("expected inserted [dup-key] for bookkeeping, got %v", inserted)
	}
	var out parsedSkillConfig
	_ = json.Unmarshal(outRaw, &out)
	if len(out.Skills) != 1 {
		t.Fatalf("expected exactly 1 skill (no duplication), got %d", len(out.Skills))
	}
}

// TestMergePromotedSkillsIntoConfigEmptyCandidatesNoChange is the trivial
// case — no candidates means we return the previous raw bytes verbatim
// (no allocation, no JSON re-encoding).
func TestMergePromotedSkillsIntoConfigEmptyCandidatesNoChange(t *testing.T) {
	prev := json.RawMessage(`{"enabled":true,"skills":[]}`)
	out, inserted, err := mergePromotedSkillsIntoConfig(prev, nil, abPromotionModeMerge)
	if err != nil {
		t.Fatalf("merge failed: %v", err)
	}
	if string(out) != string(prev) {
		t.Fatalf("expected raw passthrough, got %s", string(out))
	}
	if len(inserted) != 0 {
		t.Fatalf("expected no inserts, got %v", inserted)
	}
}

// TestParsePromotedTradingDateHandlesBadInput keeps the regression bar
// for the small date parser local — a malformed shadow event must not
// crash the promotion pipeline.
func TestParsePromotedTradingDateHandlesBadInput(t *testing.T) {
	cases := []struct {
		in    string
		valid bool
	}{
		{"", false},
		{"   ", false},
		{"not-a-date", false},
		{"2026-05-15", true},
	}
	for _, c := range cases {
		got := parsePromotedTradingDate(c.in)
		if got.Valid != c.valid {
			t.Fatalf("parse %q: expected valid=%v got valid=%v", c.in, c.valid, got.Valid)
		}
	}
}

// TestBuildPromotedReflectionMemoriesTagsIncludeSourceMarker — guards
// against the F6 audit-tag contract (memories surfaced in the UI must be
// recognisable as A/B-promoted vs organic reflections).
func TestBuildPromotedReflectionMemoriesTagsIncludeSourceMarker(t *testing.T) {
	learning := abShadowAgentLearning{Lessons: []string{"x"}, LatestTradingDate: "2026-05-15"}
	mems := buildPromotedReflectionMemories("fund", "agent", learning, "test-1", "B")
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory, got %d", len(mems))
	}
	found := false
	for _, tag := range mems[0].Tags {
		if tag == "source:ab_test" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected source:ab_test tag, got %v", mems[0].Tags)
	}
}

// TestBuildPromotedSkillCandidatesCapsAtSix prevents unbounded skill
// inflation if a noisy shadow run emits dozens of adjustments. The cap
// mirrors limitStrings(6) inside the builder.
func TestBuildPromotedSkillCandidatesCapsAtSix(t *testing.T) {
	adjustments := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		adjustments = append(adjustments, "adjustment"+string(rune('A'+i)))
	}
	learning := abShadowAgentLearning{Adjustments: adjustments}
	got := buildPromotedSkillCandidates(learning, "trader", "t", "B")
	if len(got) != 6 {
		t.Fatalf("expected cap=6, got %d", len(got))
	}
}

// TestBuildPromotedReflectionMemoryShape — defensive check that the
// Memory struct stays in sync with how MemoryRepo.CreateWithTx scans it.
// If a future migration adds a NOT NULL column without a default,
// CreateWithTx will fail at runtime; this test forces us to update the
// builder when that happens.
func TestBuildPromotedReflectionMemoryShape(t *testing.T) {
	learning := abShadowAgentLearning{Lessons: []string{"x"}, LatestTradingDate: "2026-05-15"}
	mems := buildPromotedReflectionMemories("fund", "agent", learning, "t", "B")
	if len(mems) != 1 {
		t.Fatalf("expected 1 memory")
	}
	m := mems[0]
	required := map[string]string{
		"Visibility":  m.Visibility,
		"Sensitivity": m.Sensitivity,
		"OriginKind":  m.OriginKind,
	}
	for field, value := range required {
		if value == "" {
			t.Fatalf("%s must be non-empty (CreateWithTx will reject NULL)", field)
		}
	}
	// Ensure we satisfy the repository.Memory layer enum constraint.
	if m.Layer != "long_term" {
		t.Fatalf("layer must be long_term, got %q", m.Layer)
	}
	// Guard against a refactor that drops the agent_id link — the F6
	// reflection-promotion contract is per-agent.
	if !m.AgentID.Valid {
		t.Fatalf("agent_id must be populated")
	}
	_ = repository.Memory{} // imported via type usage above
}
