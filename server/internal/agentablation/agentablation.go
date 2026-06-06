// Package agentablation cycles individual analyst agents
// off-panel for a calendar window so we can MEASURE their
// marginal contribution.
//
// MOTIVATION
// ----------
// Even with the W3-20 MAB selection layer in place, every
// analyst on the panel has *some* effect on the panel's
// rolled-up alpha. The question we cannot answer today is:
// "what is the marginal contribution of agent X?". The W3-17
// counterfactual layer does this per-decision but is expensive
// (one LLM call per LOO check). For the panel-level question
// we need something cheaper and more honest.
//
// The cheapest honest answer is observational: turn agent X
// off for a calendar week, run as normal, then compare the
// fund's realised alpha against the previous-week baseline.
// If the fund did materially WORSE in agent X's off-week, X
// was contributing alpha. If the fund did the same or better,
// X was net neutral (or actively hurting).
//
// W3-21 introduces a scheduling layer that:
//
//   1. For each analyst on the panel, picks an off-week from
//      the rolling 8-week schedule (deterministic from a seed
//      + analyst id).
//   2. Returns IsOffThisWeek(agent, week) for the wiring layer
//      to consult before adding the agent to the panel.
//   3. Accumulates fund-level alpha snapshots per (agent,
//      on-week | off-week) and produces a Report comparing the
//      means.
//
// CADENCE
// -------
// Default: each analyst gets one off-week per 8-week rotation.
// We deliberately pick the same week deterministically per
// analyst id so reports are reproducible across deploys.
//
// SCOPE
// -----
//   * Owns the Schedule, Tracker, Report types.
//   * Pure / deterministic given the seed; the wiring layer
//     supplies the seed and the calendar.
package agentablation

import (
	"hash/fnv"
	"math"
	"sort"
	"sync"
	"time"
)

// Config tunes the rotation.
type Config struct {
	// CycleWeeks is the rotation length. Default 8.
	CycleWeeks int
	// MinSamplesForReport is the minimum samples per (agent,
	// on-or-off) bucket required before Report includes the
	// agent. Default 5.
	MinSamplesForReport int
	// SignificanceThreshold is the minimum |delta| (in absolute
	// alpha terms) at which we flag the agent as "material".
	// Default 0.005 (50 bps).
	SignificanceThreshold float64
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		CycleWeeks:            8,
		MinSamplesForReport:   5,
		SignificanceThreshold: 0.005,
	}
}

// Schedule answers "is agent X off-panel this week?".
//
// The schedule is fully deterministic given the agent id and
// the seed: weekIndex(agent) = hash(agent + seed) mod CycleWeeks.
type Schedule struct {
	cfg  Config
	seed string
}

// NewSchedule returns a stable Schedule.
func NewSchedule(seed string, cfg Config) Schedule {
	return Schedule{cfg: normalise(cfg), seed: seed}
}

// IsOffThisWeek returns true when the given (agent, time) falls
// in the agent's off-week within the rotation.
func (s Schedule) IsOffThisWeek(agentID string, t time.Time) bool {
	if s.cfg.CycleWeeks <= 1 {
		return false
	}
	week := isoWeekIndex(t)
	off := s.offWeekIndex(agentID)
	return week%uint64(s.cfg.CycleWeeks) == uint64(off)
}

func (s Schedule) offWeekIndex(agentID string) int {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s.seed + "|" + agentID))
	return int(h.Sum64() % uint64(s.cfg.CycleWeeks))
}

// isoWeekIndex returns the absolute week count since Unix
// epoch. Stable across daylight-saving boundaries.
func isoWeekIndex(t time.Time) uint64 {
	if t.IsZero() {
		return 0
	}
	return uint64(t.UTC().Unix() / (7 * 24 * 3600))
}

// AlphaSample is one fund-level alpha observation.
type AlphaSample struct {
	AgentID string
	IsOff   bool // true means the sample was taken while agent was off-panel.
	Alpha   float64
	At      time.Time
}

// AgentReport is one row of the comparison report.
type AgentReport struct {
	AgentID         string  `json:"agentId"`
	OnSamples       int     `json:"onSamples"`
	OffSamples      int     `json:"offSamples"`
	OnMeanAlpha     float64 `json:"onMeanAlpha"`
	OffMeanAlpha    float64 `json:"offMeanAlpha"`
	Delta           float64 `json:"delta"` // OnMeanAlpha − OffMeanAlpha
	OnStdDev        float64 `json:"onStdDev"`
	OffStdDev       float64 `json:"offStdDev"`
	IsMaterial      bool    `json:"isMaterial"`
}

// Tracker accumulates samples and produces reports.
type Tracker struct {
	mu     sync.Mutex
	cfg    Config
	bucket map[string]*agentBucket
}

type agentBucket struct {
	on  runningStats
	off runningStats
}

type runningStats struct {
	count    int
	sum      float64
	sumSq    float64
}

func (r *runningStats) add(v float64) {
	r.count++
	r.sum += v
	r.sumSq += v * v
}

func (r runningStats) mean() float64 {
	if r.count == 0 {
		return 0
	}
	return r.sum / float64(r.count)
}

func (r runningStats) stddev() float64 {
	if r.count <= 1 {
		return 0
	}
	m := r.mean()
	v := r.sumSq/float64(r.count) - m*m
	if v < 0 {
		v = 0
	}
	return math.Sqrt(v)
}

// NewTracker returns an empty Tracker.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:    normalise(cfg),
		bucket: make(map[string]*agentBucket),
	}
}

// Record appends one observation.
func (t *Tracker) Record(s AlphaSample) {
	if t == nil || s.AgentID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	b := t.bucket[s.AgentID]
	if b == nil {
		b = &agentBucket{}
		t.bucket[s.AgentID] = b
	}
	if s.IsOff {
		b.off.add(s.Alpha)
	} else {
		b.on.add(s.Alpha)
	}
}

// Report returns the current ablation comparison sorted by
// |Delta| descending so the most-impactful agents appear first.
func (t *Tracker) Report() []AgentReport {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]AgentReport, 0, len(t.bucket))
	for id, b := range t.bucket {
		if b.on.count < t.cfg.MinSamplesForReport || b.off.count < t.cfg.MinSamplesForReport {
			continue
		}
		on := b.on.mean()
		off := b.off.mean()
		delta := on - off
		out = append(out, AgentReport{
			AgentID:      id,
			OnSamples:    b.on.count,
			OffSamples:   b.off.count,
			OnMeanAlpha:  on,
			OffMeanAlpha: off,
			Delta:        delta,
			OnStdDev:     b.on.stddev(),
			OffStdDev:    b.off.stddev(),
			IsMaterial:   math.Abs(delta) >= t.cfg.SignificanceThreshold,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return math.Abs(out[i].Delta) > math.Abs(out[j].Delta)
	})
	return out
}

// Snapshot returns ALL agent buckets (including those without
// enough samples for Report). Useful for debugging.
func (t *Tracker) Snapshot() []AgentReport {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]AgentReport, 0, len(t.bucket))
	for id, b := range t.bucket {
		on := b.on.mean()
		off := b.off.mean()
		out = append(out, AgentReport{
			AgentID:      id,
			OnSamples:    b.on.count,
			OffSamples:   b.off.count,
			OnMeanAlpha:  on,
			OffMeanAlpha: off,
			Delta:        on - off,
			OnStdDev:     b.on.stddev(),
			OffStdDev:    b.off.stddev(),
			IsMaterial:   math.Abs(on-off) >= t.cfg.SignificanceThreshold,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].AgentID < out[j].AgentID })
	return out
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.CycleWeeks <= 0 {
		c.CycleWeeks = d.CycleWeeks
	}
	if c.MinSamplesForReport <= 0 {
		c.MinSamplesForReport = d.MinSamplesForReport
	}
	if c.SignificanceThreshold < 0 {
		c.SignificanceThreshold = d.SignificanceThreshold
	}
	return c
}
