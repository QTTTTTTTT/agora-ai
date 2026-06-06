// Package reflection coordinates "reflection" events — moments
// when the system should pause and ask one of the analyst-style
// agents to write a *meta* memory: "what did we just learn?".
//
// MOTIVATION
// ----------
// Today reflection happens on a fixed cadence: a daily job
// reads recent outcomes and asks each agent to summarise. That
// works for steady-state operations but misses the moments
// where reflection is most valuable:
//
//   * A drawdown soft circuit breaker just tripped → we want
//     the agents to immediately interrogate which lessons /
//     skills got us into the position.
//   * A risk gate REJECTED a plan the LLM strongly recommended
//     → the agents should explain why their priors disagreed
//     with the gate, so the gate can be tuned (or the agents
//     can be re-aligned).
//   * Lessons surfaced into a decision had high decay (haven't
//     been confirmed in 60+ days) → the agents should re-
//     examine whether the lesson still applies.
//
// W3-15 introduces explicit *reflection triggers*. The package
// is the trigger DEFINITION layer: it owns the rules that
// decide "this just happened → enqueue a reflection task". The
// actual reflection write (LLM call, memory persistence) lives
// in the agent layer; this package only emits the request.
//
// Three first-class triggers:
//
//   1. DrawdownTrigger — fund-level drawdown crosses the soft-
//      circuit-breaker threshold. Captures the (fund, date)
//      and the breaching plan id (if any).
//   2. RejectTrigger — a plan was rejected by a pre-trade gate
//      (risk, drawdown, market-status). Captures the
//      (plan, reason) tuple.
//   3. LessonDecayTrigger — at end of day, lessons surfaced
//      into a decision had no confirming outcome in N days.
//      Captures the (lesson, days-since-last-hit).
//
// COOL-DOWN
// ---------
// The triggers themselves don't dedupe — they emit on every
// matching event. The Coordinator that consumes them holds a
// per-(trigger-kind, scope-key) cool-down so a continuous
// drawdown doesn't pump 50 reflection tasks into the queue.
// Default cool-down is 6 hours.
//
// SCOPE
// -----
//   * Owns Trigger, Coordinator, the kind enum, and the
//     dedupe / cooldown logic.
//   * Does NOT own the LLM call. The wiring layer pulls
//     pending Triggers from Coordinator.Drain and dispatches.
package reflection

import (
	"sort"
	"sync"
	"time"
)

// Kind labels the reflection trigger.
type Kind string

const (
	KindDrawdown     Kind = "drawdown"
	KindReject       Kind = "reject"
	KindLessonDecay  Kind = "lesson_decay"
	KindManual       Kind = "manual" // operator-initiated
)

// Trigger is one pending reflection request.
type Trigger struct {
	Kind     Kind              `json:"kind"`
	FundID   string            `json:"fundId,omitempty"`
	PlanID   string            `json:"planId,omitempty"`
	LessonID string            `json:"lessonId,omitempty"`
	AgentID  string            `json:"agentId,omitempty"`
	Reason   string            `json:"reason,omitempty"`
	Severity string            `json:"severity,omitempty"`
	Metadata map[string]string `json:"metadata,omitempty"`
	At       time.Time         `json:"at"`
}

// scopeKey is the dedupe / cool-down bucket.
func (t Trigger) scopeKey() string {
	switch t.Kind {
	case KindDrawdown:
		return string(KindDrawdown) + ":" + t.FundID
	case KindReject:
		// Reject is per-plan because each plan-rejection is a
		// distinct event we want to learn from.
		return string(KindReject) + ":" + t.PlanID
	case KindLessonDecay:
		return string(KindLessonDecay) + ":" + t.LessonID
	default:
		return string(t.Kind) + ":" + t.FundID + ":" + t.PlanID
	}
}

// Config holds the cool-down / queue-bound parameters.
type Config struct {
	// CoolDown is the minimum duration between two triggers
	// with the same scope key. Default 6h.
	CoolDown time.Duration
	// MaxPending caps the queue size. When exceeded, the
	// oldest pending trigger is dropped (with a counter
	// observable via Stats). Default 256.
	MaxPending int
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		CoolDown:   6 * time.Hour,
		MaxPending: 256,
	}
}

// Stats reports the coordinator's current state.
type Stats struct {
	Pending      int   `json:"pending"`
	TotalEnqueued int  `json:"totalEnqueued"`
	Suppressed   int   `json:"suppressed"`
	Dropped      int   `json:"dropped"`
}

// Coordinator owns the trigger queue.
//
// The wiring layer:
//   * calls Submit() from drawdown / reject / lesson-decay
//     event hooks.
//   * runs a worker that periodically Drain()s and dispatches.
type Coordinator struct {
	mu         sync.Mutex
	cfg        Config
	pending    []Trigger
	lastFired  map[string]time.Time
	stats      Stats
}

// NewCoordinator returns an empty coordinator.
func NewCoordinator(cfg Config) *Coordinator {
	return &Coordinator{
		cfg:       normalise(cfg),
		lastFired: make(map[string]time.Time),
	}
}

// Submit enqueues one trigger if the cool-down has elapsed.
// Returns true when enqueued, false when suppressed by
// cool-down.
func (c *Coordinator) Submit(t Trigger) bool {
	if c == nil {
		return false
	}
	if t.Kind == "" {
		return false
	}
	if t.At.IsZero() {
		t.At = time.Now().UTC()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := t.scopeKey()
	if last, ok := c.lastFired[key]; ok && t.At.Sub(last) < c.cfg.CoolDown {
		c.stats.Suppressed++
		return false
	}
	c.lastFired[key] = t.At
	if len(c.pending) >= c.cfg.MaxPending {
		// Drop oldest. We pick FIFO drop because newer
		// triggers are usually more actionable.
		c.pending = c.pending[1:]
		c.stats.Dropped++
	}
	c.pending = append(c.pending, t)
	c.stats.TotalEnqueued++
	c.stats.Pending = len(c.pending)
	return true
}

// Drain returns all pending triggers (sorted by At, oldest
// first) and clears the queue.
func (c *Coordinator) Drain() []Trigger {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	out := make([]Trigger, len(c.pending))
	copy(out, c.pending)
	c.pending = c.pending[:0]
	c.stats.Pending = 0
	sort.Slice(out, func(i, j int) bool {
		return out[i].At.Before(out[j].At)
	})
	return out
}

// Stats returns a snapshot of the coordinator counters.
func (c *Coordinator) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.stats
	s.Pending = len(c.pending)
	return s
}

// SubmitDrawdown is a sugar over Submit for the drawdown
// trigger pathway. The wiring layer calls this from the
// drawdown soft-circuit-breaker hook.
func (c *Coordinator) SubmitDrawdown(fundID, planID, severity string, at time.Time) bool {
	return c.Submit(Trigger{
		Kind:     KindDrawdown,
		FundID:   fundID,
		PlanID:   planID,
		Severity: severity,
		At:       at,
	})
}

// SubmitReject is a sugar for the reject pathway.
func (c *Coordinator) SubmitReject(fundID, planID, reason string, at time.Time) bool {
	return c.Submit(Trigger{
		Kind:   KindReject,
		FundID: fundID,
		PlanID: planID,
		Reason: reason,
		At:     at,
	})
}

// SubmitLessonDecay is a sugar for the decay pathway.
func (c *Coordinator) SubmitLessonDecay(fundID, lessonID, agentID string, daysSinceHit int, at time.Time) bool {
	t := Trigger{
		Kind:     KindLessonDecay,
		FundID:   fundID,
		LessonID: lessonID,
		AgentID:  agentID,
		At:       at,
	}
	if daysSinceHit > 0 {
		t.Metadata = map[string]string{"days_since_hit": itoa(daysSinceHit)}
	}
	return c.Submit(t)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func normalise(cfg Config) Config {
	d := DefaultConfig()
	if cfg.CoolDown <= 0 {
		cfg.CoolDown = d.CoolDown
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = d.MaxPending
	}
	return cfg
}
