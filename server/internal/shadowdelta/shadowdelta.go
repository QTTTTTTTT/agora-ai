// Package shadowdelta surfaces "what the shadow agent would
// have done" as a staging-lesson candidate when its decision
// meaningfully diverges from production.
//
// MOTIVATION
// ----------
// The strategy promotion pipeline runs candidates in shadow
// mode before letting them touch capital. The shadow comparator
// already records (production, candidate) decision pairs. What
// it does NOT do today: when the shadow disagrees AND turns
// out to be RIGHT, that is a lesson — but the lesson is buried
// in the comparator log and never circulates back into the
// alphalesson context that shapes future production decisions.
//
// W3-19 introduces a delta-surface module:
//
//   1. The comparator emits Delta records on every divergence.
//   2. After the W1-5 plan-outcome resolves, we ask: "did the
//      shadow win?". A "win" means the shadow's action would
//      have produced higher realised alpha than production's
//      actual action.
//   3. Wins crossing a meaningful-magnitude threshold are
//      converted into StagingLesson rows and queued for
//      operator review (or auto-promoted under a stricter
//      gate). The wiring layer drains the queue into the
//      memories table with status='pending_review'.
//
// SCOPE
// -----
//   * Owns Delta, StagingLesson, Tracker types.
//   * Pure / deterministic / testable; no DB or LLM calls.
package shadowdelta

import (
	"sort"
	"strings"
	"sync"
	"time"
)

// Delta is one (production, shadow) divergence record.
type Delta struct {
	PlanID            string
	FundID            string
	Symbol            string
	ProductionAction  string
	ShadowAction      string
	ProductionWeight  float64
	ShadowWeight      float64
	ProductionReason  string
	ShadowReason      string
	ShadowAgentID     string
	StrategyKey       string
	At                time.Time
}

// Outcome is the post-resolution alpha pair. We need both
// numbers — the comparator alone can tell us they DIVERGED but
// not who won. Production_alpha is the realised alpha of the
// decision that actually happened; Shadow_alpha is the
// estimated-alpha if the shadow's action had been taken.
type Outcome struct {
	PlanID          string
	ProductionAlpha float64
	ShadowAlpha     float64
	Resolved        time.Time
}

// StagingLesson is one candidate lesson surfaced from a winning
// shadow delta. The wiring layer writes these into the
// memories table with a status='pending_review' tag so the
// operator (or a periodic auto-promote job) can act on them.
type StagingLesson struct {
	FundID         string    `json:"fundId"`
	ShadowAgentID  string    `json:"shadowAgentId"`
	StrategyKey    string    `json:"strategyKey"`
	Symbol         string    `json:"symbol"`
	PlanID         string    `json:"planId"`
	Title          string    `json:"title"`
	Body           string    `json:"body"`
	AlphaDelta     float64   `json:"alphaDelta"`
	ProductionDir  string    `json:"productionDir"`
	ShadowDir      string    `json:"shadowDir"`
	GeneratedAt    time.Time `json:"generatedAt"`
}

// Config tunes the surfacing thresholds.
type Config struct {
	// MinAlphaDelta is the minimum (shadow_alpha − production_alpha)
	// for the divergence to count as a lesson. Default 0.005 (50 bps).
	MinAlphaDelta float64
	// MinSampleAge is the minimum elapsed time since the
	// outcome was resolved before we surface the lesson.
	// Avoids mis-attributing intraday noise. Default 0.
	MinSampleAge time.Duration
	// MaxStagingPerFund caps the number of staging rows we'll
	// keep per fund — keeps the operator's review surface
	// finite. Default 16.
	MaxStagingPerFund int
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		MinAlphaDelta:     0.005,
		MinSampleAge:      0,
		MaxStagingPerFund: 16,
	}
}

// Tracker accumulates pending Deltas, joins them with Outcomes,
// and returns StagingLesson rows for the wiring layer.
type Tracker struct {
	mu      sync.Mutex
	cfg     Config
	deltas  map[string]Delta // plan_id → delta
	staged  []StagingLesson
}

// NewTracker returns an empty Tracker.
func NewTracker(cfg Config) *Tracker {
	return &Tracker{
		cfg:    normalise(cfg),
		deltas: make(map[string]Delta),
	}
}

// RecordDelta stores a divergence pending outcome resolution.
// Returns false when the delta would not be considered (empty
// plan id, identical actions).
func (t *Tracker) RecordDelta(d Delta) bool {
	if t == nil || d.PlanID == "" {
		return false
	}
	if strings.EqualFold(d.ProductionAction, d.ShadowAction) &&
		d.ProductionWeight == d.ShadowWeight {
		return false
	}
	if d.At.IsZero() {
		d.At = time.Now().UTC()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.deltas[d.PlanID] = d
	return true
}

// ResolveOutcome processes one (delta, outcome) pair. If the
// shadow won by at least cfg.MinAlphaDelta and the sample is
// old enough, a StagingLesson is appended. Returns true iff
// a lesson was surfaced.
func (t *Tracker) ResolveOutcome(o Outcome, now time.Time) bool {
	if t == nil || o.PlanID == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	d, ok := t.deltas[o.PlanID]
	if !ok {
		return false
	}
	delete(t.deltas, o.PlanID)
	delta := o.ShadowAlpha - o.ProductionAlpha
	if delta < t.cfg.MinAlphaDelta {
		return false
	}
	if t.cfg.MinSampleAge > 0 && now.Sub(o.Resolved) < t.cfg.MinSampleAge {
		return false
	}
	lesson := StagingLesson{
		FundID:        d.FundID,
		ShadowAgentID: d.ShadowAgentID,
		StrategyKey:   d.StrategyKey,
		Symbol:        d.Symbol,
		PlanID:        d.PlanID,
		Title:         buildTitle(d, delta),
		Body:          buildBody(d, o, delta),
		AlphaDelta:    delta,
		ProductionDir: d.ProductionAction,
		ShadowDir:     d.ShadowAction,
		GeneratedAt:   now.UTC(),
	}
	t.staged = append(t.staged, lesson)
	t.enforceFundCap()
	return true
}

func (t *Tracker) enforceFundCap() {
	cap := t.cfg.MaxStagingPerFund
	if cap <= 0 {
		return
	}
	counts := make(map[string]int)
	out := t.staged[:0]
	// Walk in reverse-time order (newest first) and keep up
	// to cap per fund.
	staged := append([]StagingLesson(nil), t.staged...)
	sort.SliceStable(staged, func(i, j int) bool {
		return staged[i].GeneratedAt.After(staged[j].GeneratedAt)
	})
	for _, s := range staged {
		if counts[s.FundID] >= cap {
			continue
		}
		counts[s.FundID]++
		out = append(out, s)
	}
	// Restore chronological order for downstream consumers.
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].GeneratedAt.Before(out[j].GeneratedAt)
	})
	t.staged = out
}

// PendingDeltas returns the in-flight (not yet resolved) deltas.
// Used by the wiring layer to drive resolution from the outcome
// sweeper.
func (t *Tracker) PendingDeltas() []Delta {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Delta, 0, len(t.deltas))
	for _, d := range t.deltas {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PlanID < out[j].PlanID })
	return out
}

// DrainStaged returns and clears staged lessons.
func (t *Tracker) DrainStaged() []StagingLesson {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := append([]StagingLesson(nil), t.staged...)
	t.staged = t.staged[:0]
	return out
}

// PeekStaged returns staged lessons without clearing the queue.
func (t *Tracker) PeekStaged() []StagingLesson {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]StagingLesson(nil), t.staged...)
}

func buildTitle(d Delta, delta float64) string {
	return "Shadow agent " + d.ShadowAgentID + " out-performed production on " + d.Symbol
}

func buildBody(d Delta, o Outcome, delta float64) string {
	var sb strings.Builder
	sb.WriteString("On ")
	sb.WriteString(d.Symbol)
	sb.WriteString(" production took ")
	sb.WriteString(strings.ToLower(d.ProductionAction))
	sb.WriteString(" while the shadow strategy ")
	sb.WriteString(d.StrategyKey)
	sb.WriteString(" recommended ")
	sb.WriteString(strings.ToLower(d.ShadowAction))
	sb.WriteString(". Realised production α=")
	sb.WriteString(formatPercent(o.ProductionAlpha))
	sb.WriteString(", shadow α=")
	sb.WriteString(formatPercent(o.ShadowAlpha))
	sb.WriteString(" (Δ=+")
	sb.WriteString(formatPercent(delta))
	sb.WriteString("). Production reason: ")
	sb.WriteString(strings.TrimSpace(d.ProductionReason))
	sb.WriteString(". Shadow reason: ")
	sb.WriteString(strings.TrimSpace(d.ShadowReason))
	return sb.String()
}

func formatPercent(v float64) string {
	pct := v * 100
	sign := ""
	if pct >= 0 {
		sign = "+"
	}
	return sign + trim(pct, 2) + "%"
}

func trim(v float64, decimals int) string {
	const fmtBase = "0123456789"
	negative := v < 0
	if negative {
		v = -v
	}
	mul := 1.0
	for i := 0; i < decimals; i++ {
		mul *= 10
	}
	scaled := int64(v*mul + 0.5)
	if scaled == 0 {
		return "0.00"
	}
	whole := scaled / int64(mul)
	frac := scaled - whole*int64(mul)
	wholeStr := ""
	if whole == 0 {
		wholeStr = "0"
	} else {
		for whole > 0 {
			wholeStr = string(fmtBase[whole%10]) + wholeStr
			whole /= 10
		}
	}
	fracStr := ""
	for i := 0; i < decimals; i++ {
		fracStr = string(fmtBase[frac%10]) + fracStr
		frac /= 10
	}
	out := wholeStr + "." + fracStr
	if negative {
		out = "-" + out
	}
	return out
}

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.MinAlphaDelta < 0 {
		c.MinAlphaDelta = d.MinAlphaDelta
	}
	if c.MaxStagingPerFund <= 0 {
		c.MaxStagingPerFund = d.MaxStagingPerFund
	}
	if c.MinSampleAge < 0 {
		c.MinSampleAge = 0
	}
	return c
}
