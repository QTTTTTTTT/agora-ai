package decision

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestMarshalProvenanceRoundTrip is the canonical "encode then
// decode → equal" sanity check. Adding a new field to Provenance
// should not silently drop here.
func TestMarshalProvenanceRoundTrip(t *testing.T) {
	in := Provenance{
		PromptBlocks: []string{"regime", "exposure", "qualityScores"},
		LessonsUsed: []ProvenanceLesson{
			{ID: "11111111-1111-1111-1111-111111111111", Kind: "alpha_tagged", AgentTag: "fundamentals"},
			{ID: "22222222-2222-2222-2222-222222222222", Kind: "long_term"},
		},
		SkillsUsed: []ProvenanceSkill{
			{AgentID: "agent-1", SkillKey: "wait_for_fundamental_confirmation"},
			{AgentID: "agent-2", SkillKey: "size_into_low_beta", SourceReflectionID: "refl-7"},
		},
		SignalCount:      27,
		PromptTokens:     12480,
		CompletionTokens: 1834,
		PromptHash:       "sha256:deadbeef",
	}

	payload, err := MarshalProvenance(in)
	if err != nil {
		t.Fatalf("MarshalProvenance: %v", err)
	}
	if len(payload) == 0 {
		t.Fatalf("expected non-empty payload for non-zero provenance")
	}

	out, err := UnmarshalProvenance(payload)
	if err != nil {
		t.Fatalf("UnmarshalProvenance: %v", err)
	}

	if got, want := len(out.PromptBlocks), len(in.PromptBlocks); got != want {
		t.Errorf("PromptBlocks count: got %d, want %d", got, want)
	}
	if out.SignalCount != in.SignalCount {
		t.Errorf("SignalCount mismatch: got %d, want %d", out.SignalCount, in.SignalCount)
	}
	if got, want := len(out.LessonsUsed), len(in.LessonsUsed); got != want {
		t.Errorf("LessonsUsed count: got %d, want %d", got, want)
	}
	if got, want := len(out.SkillsUsed), len(in.SkillsUsed); got != want {
		t.Errorf("SkillsUsed count: got %d, want %d", got, want)
	}
	if out.PromptHash != in.PromptHash {
		t.Errorf("PromptHash: got %q, want %q", out.PromptHash, in.PromptHash)
	}
}

// TestMarshalProvenanceZeroIsNil — the zero Provenance MUST
// marshal to nil so the repository can pass nil straight to
// SetDecisionProvenance which interprets nil as "no-op". Without
// this, every plan would get an empty {} JSONB blob; later
// readers would have to disambiguate "captured nothing" from
// "didn't capture".
func TestMarshalProvenanceZeroIsNil(t *testing.T) {
	payload, err := MarshalProvenance(Provenance{})
	if err != nil {
		t.Fatalf("MarshalProvenance(zero): %v", err)
	}
	if payload != nil {
		t.Errorf("expected nil payload for zero provenance, got %q", string(payload))
	}
}

// TestUnmarshalProvenanceEmpty — caller might pass a NULL column
// read (zero-length bytes). UnmarshalProvenance must NOT error
// on this; it returns the zero value so callers can treat
// missing-equals-unknown.
func TestUnmarshalProvenanceEmpty(t *testing.T) {
	out, err := UnmarshalProvenance(nil)
	if err != nil {
		t.Fatalf("UnmarshalProvenance(nil): %v", err)
	}
	if len(out.PromptBlocks) != 0 || len(out.LessonsUsed) != 0 || len(out.SkillsUsed) != 0 {
		t.Errorf("expected zero provenance, got %+v", out)
	}
	out2, err := UnmarshalProvenance([]byte{})
	if err != nil {
		t.Fatalf("UnmarshalProvenance(empty): %v", err)
	}
	if !isZeroProvenance(out2) {
		t.Errorf("expected zero provenance, got %+v", out2)
	}
}

// TestProvenanceJSONShapeIsStable verifies the JSON shape that
// downstream Wave-2 trackers + analytics consumers will see.
// Field names are part of the public contract — renaming any
// of them is a breaking change for the column readers.
func TestProvenanceJSONShapeIsStable(t *testing.T) {
	in := Provenance{
		PromptBlocks: []string{"regime"},
		LessonsUsed:  []ProvenanceLesson{{ID: "x", Kind: "k", AgentTag: "a"}},
		SkillsUsed:   []ProvenanceSkill{{AgentID: "a1", SkillKey: "k1", SourceReflectionID: "r1"}},
		SignalCount:  7,
		PromptHash:   "sha256:0000",
	}
	payload, err := MarshalProvenance(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	str := string(payload)
	required := []string{
		`"promptBlocks":["regime"]`,
		`"lessonsUsed":[`,
		`"id":"x"`,
		`"kind":"k"`,
		`"agentTag":"a"`,
		`"skillsUsed":[`,
		`"agentId":"a1"`,
		`"skillKey":"k1"`,
		`"sourceReflectionId":"r1"`,
		`"signalCount":7`,
		`"promptHash":"sha256:0000"`,
	}
	for _, frag := range required {
		if !strings.Contains(str, frag) {
			t.Errorf("expected JSON to contain %q, got:\n%s", frag, str)
		}
	}

	// Also confirm there are no surprise extra top-level keys —
	// guards against a refactor accidentally exporting a private
	// field via an unintended struct tag rename.
	var top map[string]json.RawMessage
	if err := json.Unmarshal(payload, &top); err != nil {
		t.Fatalf("decode top-level: %v", err)
	}
	allowed := map[string]bool{
		"promptBlocks":     true,
		"lessonsUsed":      true,
		"skillsUsed":       true,
		"signalCount":      true,
		"promptTokens":     true,
		"completionTokens": true,
		"promptHash":       true,
	}
	for k := range top {
		if !allowed[k] {
			t.Errorf("unexpected top-level key %q in provenance JSON", k)
		}
	}
}
