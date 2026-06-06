// Package mabpanel selects which subset of analyst-style
// agents to put on a panel for a given decision, treating
// each agent as a multi-armed-bandit arm scored by realised
// effective alpha.
//
// MOTIVATION
// ----------
// The roundtable today runs a fixed analyst panel (bull, bear,
// quant, fundamentalist, sector-specialist…) on every decision.
// Two costs:
//   1. Token cost. Every analyst contributes ~3-5k input tokens
//      per decision. A panel of 5 is 15-25k tokens before the
//      PM stage even starts.
//   2. Echo chamber. When two analysts share a strong prior
//      (W2-12 collinearity penalty hits this) they add noise
//      not signal. We are paying for redundant opinions.
//
// W3-20 introduces a multi-armed bandit over the analyst pool.
// The panel size K is fixed (default 3); the bandit picks the
// top-K analysts by UCB1 over realised effective alpha.
//
// REWARD
// ------
// The reward signal for each analyst is the W2-12 effective
// alpha contribution: their down-weighted vote × realised plan
// alpha. This rewards analysts who:
//
//   * vote in the WINNING direction more often than not
//     (because realised alpha is positive when they were
//     right);
//   * don't echo other analysts' votes (because effective
//     weight is shrunk when they do).
//
// EXPLORATION
// -----------
// UCB1's confidence bonus 2*ln(N)/n_i ensures every analyst
// gets sampled regularly even when one runs away with the
// rolling reward. Operators can tune the exploration constant
// c (default sqrt(2)) to favour exploit (lower c) or explore
// (higher c).
//
// SCOPE
// -----
//   * Owns Analyst, Bandit, SelectionResult.
//   * Pure / deterministic given the random seed (we don't use
//     randomness in UCB1 — the only randomness is the
//     deterministic tie-break by analyst id).
package mabpanel

import (
	"math"
	"sort"
	"sync"
	"time"
)

// Analyst is one MAB arm: a candidate for the panel.
type Analyst struct {
	ID       string
	Label    string
	Tags     []string
	// Eligible is the gate the wiring layer applies before
	// selection. False analysts are skipped entirely.
	Eligible bool
}

// Reward is one realised reward signal for an analyst.
type Reward struct {
	AnalystID       string
	PlanID          string
	EffectiveAlpha  float64 // signed: + means right, − means wrong
	At              time.Time
}

// Stats is one analyst's running summary.
type Stats struct {
	AnalystID   string  `json:"analystId"`
	Pulls       int     `json:"pulls"`
	SumReward   float64 `json:"sumReward"`
	MeanReward  float64 `json:"meanReward"`
	UCBScore    float64 `json:"ucbScore"`
	LastPulled  time.Time `json:"lastPulled,omitempty"`
}

// SelectionResult is the per-decision panel pick.
type SelectionResult struct {
	K        int      `json:"k"`
	Selected []Stats  `json:"selected"`
	Skipped  []Stats  `json:"skipped"`
}

// Config tunes the bandit.
type Config struct {
	// PanelSize is K — how many analysts on the panel. Default 3.
	PanelSize int
	// ExplorationConstant is c in UCB1. Default math.Sqrt(2).
	ExplorationConstant float64
	// MinPullsBeforeUCB requires every analyst to be sampled
	// at least this many times before UCB scoring kicks in.
	// Defaults to 1 — pulls each new arm before exploiting.
	MinPullsBeforeUCB int
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		PanelSize:           3,
		ExplorationConstant: math.Sqrt(2),
		MinPullsBeforeUCB:   1,
	}
}

// Bandit is the in-memory MAB state.
type Bandit struct {
	mu      sync.Mutex
	cfg     Config
	stats   map[string]*Stats
	totalPulls int
}

// NewBandit returns an empty Bandit.
func NewBandit(cfg Config) *Bandit {
	return &Bandit{
		cfg:   normalise(cfg),
		stats: make(map[string]*Stats),
	}
}

// Reward registers one observed reward. Idempotent on
// (analyst_id, plan_id) is the caller's responsibility — we
// don't dedupe here because the wiring layer sees plan_id as a
// natural key already.
func (b *Bandit) Reward(r Reward) {
	if b == nil || r.AnalystID == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.stats[r.AnalystID]
	if s == nil {
		s = &Stats{AnalystID: r.AnalystID}
		b.stats[r.AnalystID] = s
	}
	s.Pulls++
	s.SumReward += r.EffectiveAlpha
	if s.Pulls > 0 {
		s.MeanReward = s.SumReward / float64(s.Pulls)
	}
	if r.At.After(s.LastPulled) {
		s.LastPulled = r.At
	}
	b.totalPulls++
}

// Select returns the top-K analysts by UCB1.
//
// The selection deterministically tie-breaks by analyst id so
// the same input always produces the same panel.
func (b *Bandit) Select(candidates []Analyst) SelectionResult {
	if b == nil {
		return SelectionResult{K: 0}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	res := SelectionResult{K: b.cfg.PanelSize}

	scored := make([]Stats, 0, len(candidates))
	skipped := make([]Stats, 0)
	for _, a := range candidates {
		if !a.Eligible {
			skipped = append(skipped, Stats{AnalystID: a.ID})
			continue
		}
		s := b.stats[a.ID]
		if s == nil {
			s = &Stats{AnalystID: a.ID}
			b.stats[a.ID] = s
		}
		s.UCBScore = b.ucbScoreLocked(s)
		// Deep-copy so Sort doesn't mutate the live state.
		scored = append(scored, *s)
	}

	sort.SliceStable(scored, func(i, j int) bool {
		if scored[i].UCBScore != scored[j].UCBScore {
			return scored[i].UCBScore > scored[j].UCBScore
		}
		return scored[i].AnalystID < scored[j].AnalystID
	})

	k := b.cfg.PanelSize
	if k > len(scored) {
		k = len(scored)
	}
	res.Selected = scored[:k]
	if k < len(scored) {
		res.Skipped = append(skipped, scored[k:]...)
	} else {
		res.Skipped = skipped
	}
	return res
}

func (b *Bandit) ucbScoreLocked(s *Stats) float64 {
	// Encourage every arm to be pulled MinPullsBeforeUCB times
	// before exploiting — return +Inf so the sort puts unsampled
	// arms first.
	if s.Pulls < b.cfg.MinPullsBeforeUCB || b.totalPulls == 0 {
		return math.Inf(1)
	}
	mean := s.MeanReward
	bonus := b.cfg.ExplorationConstant * math.Sqrt(math.Log(float64(b.totalPulls))/float64(s.Pulls))
	return mean + bonus
}

// Snapshot returns all current stats sorted by analyst id.
func (b *Bandit) Snapshot() []Stats {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]Stats, 0, len(b.stats))
	for _, s := range b.stats {
		c := *s
		c.UCBScore = b.ucbScoreLocked(s)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AnalystID < out[j].AnalystID })
	return out
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.PanelSize <= 0 {
		c.PanelSize = d.PanelSize
	}
	if c.ExplorationConstant <= 0 {
		c.ExplorationConstant = d.ExplorationConstant
	}
	if c.MinPullsBeforeUCB < 0 {
		c.MinPullsBeforeUCB = 0
	}
	return c
}
