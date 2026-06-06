// Package skilleffect tracks the empirical effectiveness of agent
// "skills" — the long-term reflection-distilled prompt fragments
// surfaced by the SkillInbox / agent_skills table.
//
// MOTIVATION
// ----------
// Skills are written by the system itself (via reflection over
// past plan attribution) and inserted into agent prompts when
// the corresponding (fund, agent, regime) triggers fire. They
// are the "what should this analyst remember?" layer above raw
// memory recall. Their value is asserted but not measured: a
// skill that fires on every plan but never moves alpha is just
// noise in the prompt — and silently inflates token spend on
// every tick.
//
// W2-8 closes this loop. The W1-4 decision_provenance column now
// records the {agentId, skillKey} pairs active at decide time;
// the W1-5 plan_outcome column records the realised outcome.
// Joining the two yields a per-skill hit-rate / mean-alpha /
// sample-count time series. This package is the in-memory
// aggregator for that join.
//
// AUTO-DEPRECATE POLICY
// ---------------------
// A skill is "auto-deprecated" when:
//
//   * SampleCount ≥ MinSampleSize (default 25) — we don't pull
//     the rug after 3 plans.
//   * HitRate < HitRateFloor (default 0.45) — i.e. it fires but
//     correlates with hits less often than a coin flip.
//   * MeanAlpha < AlphaFloor (default −0.0) — mean alpha goes
//     negative (the skill actively hurts when applied).
//
// All three thresholds must trip simultaneously. The package
// exposes a Recommendation enum (Keep / Throttle / Deprecate)
// rather than mutating a database row directly — the SkillInbox
// admin UI consumes the enum, presents the suggestion to the
// operator, and the operator (or a future scheduled job) flips
// the canonical agent_skills.status flag. We deliberately do
// NOT auto-flip in this package: a wrongly-tuned policy
// shouldn't be able to silently demote every skill.
//
// SCOPE
// -----
//   * Owns the Observation value type, Tracker, Aggregate report.
//   * Does NOT own persistence or the agent_skills table mutation.
package skilleffect

import (
	"sort"
	"sync"
	"time"
)

// Observation records one (agent, skill, plan) usage with its
// realised outcome. Hit is the binary label (1 = good outcome,
// 0 = bad). Alpha is the realised excess return, in fraction
// units (0.02 = 2%); positive means the plan beat the
// benchmark, negative means it underperformed. PlanID is kept
// for drill-down in the SkillInbox UI.
type Observation struct {
	AgentID  string
	SkillKey string
	PlanID   string
	Hit      bool
	Alpha    float64
	At       time.Time
}

// Recommendation is the auto-deprecate enum. Tracker.Snapshot
// returns one per skill.
type Recommendation string

const (
	// RecommendationKeep — the skill clears all gates.
	RecommendationKeep Recommendation = "keep"
	// RecommendationThrottle — sample size too low to deprecate
	// but the early signal is poor; the operator should narrow
	// the firing condition (regime gate, agent gate) before
	// gathering more samples.
	RecommendationThrottle Recommendation = "throttle"
	// RecommendationDeprecate — full sample, poor performance.
	// The SkillInbox UI should mark it for review.
	RecommendationDeprecate Recommendation = "deprecate"
)

// Aggregate is the per-skill summary report.
type Aggregate struct {
	AgentID        string         `json:"agentId,omitempty"`
	SkillKey       string         `json:"skillKey"`
	SampleCount    int            `json:"sampleCount"`
	Hits           int            `json:"hits"`
	HitRate        float64        `json:"hitRate"`
	MeanAlpha      float64        `json:"meanAlpha"`
	StdDevAlpha    float64        `json:"stdDevAlpha"`
	LastUsedAt     time.Time      `json:"lastUsedAt"`
	Recommendation Recommendation `json:"recommendation"`
}

// Policy holds the auto-deprecate thresholds. Negative /
// non-finite values are normalised to defaults so the package
// is robust to misconfiguration.
type Policy struct {
	MinSampleSize int
	HitRateFloor  float64
	AlphaFloor    float64
	// MinSamplesForThrottle is the count below which we never
	// emit a Deprecate recommendation, even if the early sample
	// looks bad. Below this we emit Throttle instead so the
	// operator gets a "this is rocky" warning without the
	// "delete it" pressure.
	MinSamplesForThrottle int
}

// DefaultPolicy is the production-safe baseline.
func DefaultPolicy() Policy {
	return Policy{
		MinSampleSize:         25,
		HitRateFloor:          0.45,
		AlphaFloor:            0.0,
		MinSamplesForThrottle: 5,
	}
}

// Tracker is an in-memory aggregator over observations. Keyed
// by (agent_id, skill_key). Safe for concurrent Record calls.
type Tracker struct {
	mu     sync.RWMutex
	byKey  map[string]*aggregator
	policy Policy
}

type aggregator struct {
	AgentID    string
	SkillKey   string
	count      int
	hits       int
	sumAlpha   float64
	sumAlphaSq float64
	lastUsedAt time.Time
}

// NewTracker creates an empty Tracker with the given policy.
// Pass DefaultPolicy() unless you have a reason otherwise.
func NewTracker(p Policy) *Tracker {
	return &Tracker{
		byKey:  make(map[string]*aggregator),
		policy: normalisePolicy(p),
	}
}

// Record appends one observation.
func (t *Tracker) Record(o Observation) {
	if t == nil {
		return
	}
	if o.AgentID == "" || o.SkillKey == "" {
		return
	}
	key := keyOf(o.AgentID, o.SkillKey)
	t.mu.Lock()
	defer t.mu.Unlock()
	a := t.byKey[key]
	if a == nil {
		a = &aggregator{AgentID: o.AgentID, SkillKey: o.SkillKey}
		t.byKey[key] = a
	}
	a.count++
	if o.Hit {
		a.hits++
	}
	a.sumAlpha += o.Alpha
	a.sumAlphaSq += o.Alpha * o.Alpha
	if o.At.After(a.lastUsedAt) {
		a.lastUsedAt = o.At
	}
}

// Snapshot returns the per-skill aggregate for one (agent, skill).
// Returns SampleCount==0 when nothing has been recorded.
func (t *Tracker) Snapshot(agentID, skillKey string) Aggregate {
	if t == nil {
		return Aggregate{AgentID: agentID, SkillKey: skillKey}
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	a := t.byKey[keyOf(agentID, skillKey)]
	if a == nil {
		return Aggregate{AgentID: agentID, SkillKey: skillKey, Recommendation: RecommendationKeep}
	}
	return finalise(a, t.policy)
}

// Snapshots returns the aggregate for every observed skill,
// sorted by (agent_id, skill_key). Useful for batch reports.
func (t *Tracker) Snapshots() []Aggregate {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Aggregate, 0, len(t.byKey))
	for _, a := range t.byKey {
		out = append(out, finalise(a, t.policy))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentID != out[j].AgentID {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].SkillKey < out[j].SkillKey
	})
	return out
}

func finalise(a *aggregator, p Policy) Aggregate {
	if a.count == 0 {
		return Aggregate{AgentID: a.AgentID, SkillKey: a.SkillKey, Recommendation: RecommendationKeep}
	}
	mean := a.sumAlpha / float64(a.count)
	variance := a.sumAlphaSq/float64(a.count) - mean*mean
	if variance < 0 {
		variance = 0 // numerical noise
	}
	std := sqrt(variance)
	hitRate := float64(a.hits) / float64(a.count)
	rec := evaluate(a.count, hitRate, mean, p)
	return Aggregate{
		AgentID:        a.AgentID,
		SkillKey:       a.SkillKey,
		SampleCount:    a.count,
		Hits:           a.hits,
		HitRate:        hitRate,
		MeanAlpha:      mean,
		StdDevAlpha:    std,
		LastUsedAt:     a.lastUsedAt,
		Recommendation: rec,
	}
}

func evaluate(count int, hitRate, meanAlpha float64, p Policy) Recommendation {
	belowGates := hitRate < p.HitRateFloor && meanAlpha < p.AlphaFloor
	switch {
	case count >= p.MinSampleSize && belowGates:
		return RecommendationDeprecate
	case count >= p.MinSamplesForThrottle && belowGates:
		return RecommendationThrottle
	default:
		return RecommendationKeep
	}
}

func keyOf(agentID, skillKey string) string {
	return agentID + "\x00" + skillKey
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	// Newton's method, 5 iterations — fine for the variance
	// values we'll see (< 1.0). Avoids the math.Sqrt import
	// cost on hot ticks.
	x := v
	for i := 0; i < 5; i++ {
		x = 0.5 * (x + v/x)
	}
	return x
}

func normalisePolicy(p Policy) Policy {
	if p.MinSampleSize <= 0 {
		p.MinSampleSize = DefaultPolicy().MinSampleSize
	}
	if p.HitRateFloor <= 0 || p.HitRateFloor >= 1 {
		p.HitRateFloor = DefaultPolicy().HitRateFloor
	}
	if p.MinSamplesForThrottle < 0 {
		p.MinSamplesForThrottle = DefaultPolicy().MinSamplesForThrottle
	}
	if p.MinSamplesForThrottle > p.MinSampleSize {
		p.MinSamplesForThrottle = p.MinSampleSize
	}
	return p
}
