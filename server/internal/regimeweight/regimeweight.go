// Package regimeweight maintains a per-(agent, regime) track
// record so the alphalesson context builder can up-weight
// agents who were good *in this regime* — not just on aggregate.
//
// MOTIVATION
// ----------
// agent_reputation_stats records aggregate avg α, hit rate and
// decision count per (fund, agent). It is regime-blind. A
// quant agent who was elite in trend-up regimes and disastrous
// in chop will appear "average" on the rolled-up leaderboard.
// Surfacing only the aggregate to the LLM hides actionable
// signal: the quant is *right now* in a chop regime, but the
// LLM still sees their full track record and over-weights
// their vote.
//
// W2-13 supplies a regime-conditional reweighting layer:
//
//   * For each (agent, regime) pair we maintain count, hits,
//     sum-α. Same shape as agentreputation.Stats but sliced
//     by regime label.
//   * The weight() function returns a multiplier in (0, 1.5]
//     applied to the agent's nominal weight for the current
//     decision. >1 means the agent is historically strong in
//     this regime; <1 means they should be down-weighted; the
//     curve saturates so a single lucky decision in a rare
//     regime cannot 5x someone's vote.
//
// The package is in-memory and deterministic. The wiring layer
// hydrates it from agent_alpha_outcomes joined to a
// regime-classifier table at startup, then drains updated
// counts back to a journal table for cold-start recovery.
//
// MATH
// ----
// We use a Bayesian-shrunk mean-α estimator. The shrinkage
// constant is configurable; the default (10) means an agent
// needs at least ~10 decisions in a regime before their α
// estimate departs meaningfully from the global mean for that
// regime. This avoids the "1-decision miracle" trap where one
// great call inflates the agent's apparent regime skill into
// a 1.5x multiplier they didn't earn.
//
// w(agent, regime) = 1 + clamp(
//     k * (alpha_hat(agent, regime) - alpha_hat(_, regime)),
//     -0.5, +0.5)
//
// where alpha_hat is the shrunk mean. k is the slope; default 8.
// At k=8 a 4-percentage-point excess α gives a ~+0.32
// multiplier — meaningful but not dominating.
//
// SCOPE
// -----
//   * Owns the AgentRegimeKey, Stats, Tracker, weighter.
//   * Does NOT mutate agentreputation.Stats. The wiring layer
//     reads both and combines at render time.
package regimeweight

import (
	"math"
	"sort"
	"sync"
)

// Stats is one (agent, regime) accumulator.
type Stats struct {
	AgentID        string  `json:"agentId"`
	Regime         string  `json:"regime"`
	DecisionsCount int     `json:"decisionsCount"`
	HitsCount      int     `json:"hitsCount"`
	SumAlpha       float64 `json:"sumAlpha"`
	MeanAlpha      float64 `json:"meanAlpha"`
	HitRate        float64 `json:"hitRate"`
}

// Observation is one (agent, regime, outcome) record fed into
// the tracker.
type Observation struct {
	AgentID string
	Regime  string
	Hit     bool
	Alpha   float64
}

// Config tunes the weighting curve.
type Config struct {
	// ShrinkPrior is the Bayesian shrinkage prior count.
	// Effectively says "treat agents like the global regime
	// mean until they have ShrinkPrior decisions in this
	// regime". Default 10.
	ShrinkPrior int
	// SlopeK scales the multiplier deviation. Bigger = more
	// aggressive reweighting. Default 8.0.
	SlopeK float64
	// MaxBoost / MaxPenalty cap the multiplier on either side.
	// Default 0.5 each, giving multipliers in [0.5, 1.5].
	MaxBoost   float64
	MaxPenalty float64
	// MinDecisionsForBoost is a hard floor: agents with fewer
	// than this many decisions in the current regime always
	// receive multiplier 1.0. Default 3 — even with shrinkage,
	// 0-2 decisions is too few to act on.
	MinDecisionsForBoost int
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		ShrinkPrior:          10,
		SlopeK:               8.0,
		MaxBoost:             0.5,
		MaxPenalty:           0.5,
		MinDecisionsForBoost: 3,
	}
}

// Tracker accumulates observations and produces regime weights.
//
// The state is in-memory but designed to be persistable: the
// Snapshot()/LoadSnapshot() methods exchange a pure-data view
// the wiring layer can write to / read from a database table.
type Tracker struct {
	mu     sync.RWMutex
	cfg    Config
	cells  map[agentRegimeKey]*Stats
	regime map[string]*regimeAgg
}

type agentRegimeKey struct {
	agentID string
	regime  string
}

type regimeAgg struct {
	count    int
	sumAlpha float64
}

// NewTracker returns an empty Tracker bound to the given config.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:    normalise(cfg),
		cells:  make(map[agentRegimeKey]*Stats),
		regime: make(map[string]*regimeAgg),
	}
}

// Record appends one observation.
func (t *Tracker) Record(o Observation) {
	if t == nil {
		return
	}
	if o.AgentID == "" || o.Regime == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := agentRegimeKey{agentID: o.AgentID, regime: o.Regime}
	s := t.cells[key]
	if s == nil {
		s = &Stats{AgentID: o.AgentID, Regime: o.Regime}
		t.cells[key] = s
	}
	s.DecisionsCount++
	if o.Hit {
		s.HitsCount++
	}
	s.SumAlpha += o.Alpha
	s.MeanAlpha = s.SumAlpha / float64(s.DecisionsCount)
	s.HitRate = float64(s.HitsCount) / float64(s.DecisionsCount)

	r := t.regime[o.Regime]
	if r == nil {
		r = &regimeAgg{}
		t.regime[o.Regime] = r
	}
	r.count++
	r.sumAlpha += o.Alpha
}

// Weight returns the multiplier for one (agent, regime) pair.
// Returns 1.0 when not enough data exists.
func (t *Tracker) Weight(agentID, regime string) float64 {
	if t == nil || agentID == "" || regime == "" {
		return 1.0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	s := t.cells[agentRegimeKey{agentID: agentID, regime: regime}]
	if s == nil || s.DecisionsCount < t.cfg.MinDecisionsForBoost {
		return 1.0
	}
	r := t.regime[regime]
	if r == nil || r.count == 0 {
		return 1.0
	}
	regimeMean := r.sumAlpha / float64(r.count)
	shrunkAgent := shrunkMean(s.SumAlpha, s.DecisionsCount, regimeMean, t.cfg.ShrinkPrior)
	delta := shrunkAgent - regimeMean
	multiplier := 1.0 + clamp(t.cfg.SlopeK*delta, -t.cfg.MaxPenalty, t.cfg.MaxBoost)
	return multiplier
}

// Snapshot returns a copy of all per-cell stats. Sorted by
// agent then regime for deterministic round-tripping.
func (t *Tracker) Snapshot() []Stats {
	if t == nil {
		return nil
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]Stats, 0, len(t.cells))
	for _, s := range t.cells {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].AgentID != out[j].AgentID {
			return out[i].AgentID < out[j].AgentID
		}
		return out[i].Regime < out[j].Regime
	})
	return out
}

// LoadSnapshot rehydrates the tracker from a Snapshot, replacing
// any existing state. Used on startup to restore from a
// persisted journal.
func (t *Tracker) LoadSnapshot(stats []Stats) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cells = make(map[agentRegimeKey]*Stats, len(stats))
	t.regime = make(map[string]*regimeAgg)
	for _, s := range stats {
		if s.AgentID == "" || s.Regime == "" {
			continue
		}
		copy := s
		if copy.DecisionsCount > 0 {
			copy.MeanAlpha = copy.SumAlpha / float64(copy.DecisionsCount)
			copy.HitRate = float64(copy.HitsCount) / float64(copy.DecisionsCount)
		}
		t.cells[agentRegimeKey{agentID: copy.AgentID, regime: copy.Regime}] = &copy
		r := t.regime[copy.Regime]
		if r == nil {
			r = &regimeAgg{}
			t.regime[copy.Regime] = r
		}
		r.count += copy.DecisionsCount
		r.sumAlpha += copy.SumAlpha
	}
}

// shrunkMean is the Bayesian-shrunk α estimator.
//
// Returns (sumAlpha + prior*priorMean) / (count + prior).
//
// At count=0 returns priorMean exactly. At count >> prior the
// estimate converges to the agent's empirical mean.
func shrunkMean(sumAlpha float64, count int, priorMean float64, prior int) float64 {
	if count == 0 && prior == 0 {
		return 0
	}
	denom := float64(count + prior)
	if denom == 0 {
		return 0
	}
	return (sumAlpha + float64(prior)*priorMean) / denom
}

func clamp(v, lo, hi float64) float64 {
	if math.IsNaN(v) {
		return 0
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.ShrinkPrior <= 0 {
		c.ShrinkPrior = d.ShrinkPrior
	}
	if c.SlopeK <= 0 {
		c.SlopeK = d.SlopeK
	}
	if c.MaxBoost <= 0 {
		c.MaxBoost = d.MaxBoost
	}
	if c.MaxPenalty <= 0 {
		c.MaxPenalty = d.MaxPenalty
	}
	if c.MinDecisionsForBoost < 0 {
		c.MinDecisionsForBoost = d.MinDecisionsForBoost
	}
	return c
}
