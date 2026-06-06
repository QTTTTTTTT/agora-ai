// provenance.go — W1-4: capture *what shaped a plan*, alongside
// *what the plan said*.
//
// WHY THIS EXISTS
// ---------------
// The Wave-2 self-learning loops (calibration, skill-effectiveness,
// lesson-refute) all need to answer "which lessons / skills / signal
// blocks did the LLM see when it made THIS decision?". Today the
// codebase already persists the OUTPUT of a decision (actions,
// reasoning, confidence, decision_source, fallback_reason,
// block_contributions). It does NOT persist the INPUT context the
// LLM had — that information is built up in the wiring layer, used
// once for the LLM call, and then discarded.
//
// This file owns the canonical Provenance struct that:
//
//   1. The wiring layer fills in (or partially fills in) when it
//      assembles a DecisionInput, before invoking the engine.
//   2. The repository layer marshals to the new
//      investment_plans.decision_provenance JSONB column.
//   3. The Wave-2 trackers read back to attribute outcomes to
//      lessons / skills / prompt fragments.
//
// FORWARD COMPATIBILITY
// ---------------------
// Provenance is a value object marshalled to JSONB; new fields
// can be appended without a SQL migration. Old rows that were
// inserted before this commit remain NULL — readers MUST treat
// NULL provenance as "unknown / not captured" and not as "no
// signals were used".
//
// The struct shape mirrors the column comment in
// migrations/093_decision_provenance.sql so the two stay in
// lockstep.

package decision

import "encoding/json"

// Provenance is the captured-at-decide-time view of WHAT
// information shaped one plan. Every field is optional — a partial
// Provenance is more useful than no Provenance.
type Provenance struct {
	// PromptBlocks is the canonical list of signal-block names
	// that ended up in the LLM prompt. Examples: "regime",
	// "exposure", "qualityScores", "newsCatalysts", "pead". The
	// list is what `Trace.PresentBlocks()` already exposes — we
	// re-record it here so old plans stay queryable even if the
	// trace JSON shape evolves.
	PromptBlocks []string `json:"promptBlocks,omitempty"`

	// LessonsUsed is the set of alpha-tagged memory rows pulled
	// in by alphalesson.BuildContext for this plan. Each entry
	// carries enough id / kind / agentTag to let the Wave-2
	// lesson-refute path attribute outcomes back to the lesson.
	LessonsUsed []ProvenanceLesson `json:"lessonsUsed,omitempty"`

	// SkillsUsed is the set of agent skill rows that were active
	// in skill_config.skills at decide time. Used by the Wave-2
	// skill-effectiveness tracker to compute hit-rate-per-skill.
	SkillsUsed []ProvenanceSkill `json:"skillsUsed,omitempty"`

	// SignalCount is the number of distinct numeric signals fed
	// to the engine (counted at the wiring layer). Useful for
	// signal-budgeting analysis (Wave 2 #10) — high count + low
	// outcome alpha = "we drowned the LLM in noise".
	SignalCount int `json:"signalCount,omitempty"`

	// PromptTokens / CompletionTokens are the LLM accounting
	// stats. Captured so cost-per-decision and tokens-per-block
	// can be cross-referenced with outcome quality.
	PromptTokens     int `json:"promptTokens,omitempty"`
	CompletionTokens int `json:"completionTokens,omitempty"`

	// PromptHash is a stable fingerprint over the assembled
	// prompt body. Lets the consistency-CI fixture
	// (Wave 2 #14) detect when the same DecisionInput produced
	// drifted prompts. "sha256:<hex>" is the canonical shape.
	PromptHash string `json:"promptHash,omitempty"`
}

// ProvenanceLesson identifies one alpha-tagged memory row that
// shaped a decision. Kept narrow on purpose: the Wave-2 refute
// path joins this back to the memories table by id.
type ProvenanceLesson struct {
	// ID is the memories.id UUID. Required — without an id we
	// can't trace the lesson back.
	ID string `json:"id"`
	// Kind is the memory layer / category, e.g. "alpha_tagged",
	// "long_term", "reflection". Optional — present when the
	// caller knows it; the refute path doesn't depend on it.
	Kind string `json:"kind,omitempty"`
	// AgentTag is the agentreputation agent_id this lesson
	// summarises (the field added by migration 074). Optional;
	// useful for per-agent calibration drill-downs.
	AgentTag string `json:"agentTag,omitempty"`
}

// ProvenanceSkill identifies one agent-skill entry that was
// active when this plan was decided. The wiring layer already
// has fund/agent → skill_config mappings; we record a snapshot
// of the keys so subsequent skill_config edits don't retroact.
type ProvenanceSkill struct {
	// AgentID is the fund-team-member uuid the skill belongs to.
	AgentID string `json:"agentId"`
	// SkillKey is the stable per-agent identifier (the .key
	// field on parsedSkillEntry). Required — the
	// skill-effectiveness tracker pivots on this key.
	SkillKey string `json:"skillKey"`
	// SourceReflectionID, when set, ties the skill back to the
	// long-term reflection that proposed it. Useful for the
	// "did this reflection actually help?" drill-down.
	SourceReflectionID string `json:"sourceReflectionId,omitempty"`
}

// MarshalProvenance serialises a Provenance to JSON. Returns nil
// for the zero value so the repository can pass nil through to a
// SQL NULL — that lets us distinguish "we captured no provenance"
// from "we captured an empty provenance" (which never happens in
// practice but the type system thanks us).
func MarshalProvenance(p Provenance) ([]byte, error) {
	if isZeroProvenance(p) {
		return nil, nil
	}
	return json.Marshal(p)
}

// UnmarshalProvenance deserialises a JSONB blob into a
// Provenance. Empty / nil input returns the zero value with no
// error — readers should treat zero as "unknown / not captured".
func UnmarshalProvenance(raw []byte) (Provenance, error) {
	var out Provenance
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Provenance{}, err
	}
	return out, nil
}

func isZeroProvenance(p Provenance) bool {
	return len(p.PromptBlocks) == 0 &&
		len(p.LessonsUsed) == 0 &&
		len(p.SkillsUsed) == 0 &&
		p.SignalCount == 0 &&
		p.PromptTokens == 0 &&
		p.CompletionTokens == 0 &&
		p.PromptHash == ""
}
