// Package lessonrefute owns the "this lesson is misleading the LLM"
// half of the alphalesson learning loop.
//
// MOTIVATION
// ----------
// alphalesson surfaces past lessons (memory rows tagged with an
// agent + alpha + use_count) into agent prompts when the regime,
// agent identity, and tag overlap with the current decision. The
// promotion side is well-trodden: when a plan that USED a lesson
// produces a hit, use_count goes up and the lesson rises in the
// context block. The negative side has been silent: when a plan
// that USED a lesson misses badly, the lesson keeps being surfaced.
//
// W2-9 closes that gap. Migration 095 added refutation_count and
// status to memories. This package owns the policy that translates
// "plan X used memory M, plan X had bad outcome" into a refutation
// and, when refutations accumulate, into a soft- or hard-refuted
// status.
//
// THE WORD "REFUTATION" IS DELIBERATE
// -----------------------------------
// We are NOT saying "this memory is wrong about the world". We are
// saying "this memory was actively cited as a reason for a decision
// that did not work out". The two are different — a perfectly
// correct lesson can be misapplied to a regime it doesn't cover —
// but they look the same from the outside. The refutation count is
// the empirical proxy.
//
// POLICY
// ------
// A bad outcome is one of:
//
//   * Realised alpha < 0 by more than the configured floor.
//   * Realised alpha < 0 AND the plan triggered the drawdown soft
//     circuit breaker (i.e. the loss was meaningful, not noise).
//
// Counting policy:
//
//   * Each (memory, plan) pair contributes at most ONE refutation.
//     The caller dedupes via the dedupeKey() helper before calling
//     RegisterRefutation.
//   * A refutation does not decay. Lessons that are right for two
//     years and then become wrong for two months ARE refuted, and
//     that is the desired signal.
//
// Status flip thresholds (DefaultPolicy):
//
//   * 3 refutations  → status='soft_refuted' (down-weighted in
//                      alphalesson context, still surfaced if no
//                      better lesson exists).
//   * 5 refutations  → status='hard_refuted' (skipped entirely
//                      until an admin re-activates).
//
// This package is a pure decision module. It does NOT issue SQL.
// The caller wires it to the MemoryRepo's IncrementRefutation +
// SetMemoryStatus methods.
package lessonrefute

import (
	"sort"
	"sync"
	"time"
)

// Status mirrors the values stored in the memories.status column.
// The string form must match the migration 095 CHECK constraint.
type Status string

const (
	StatusActive       Status = "active"
	StatusSoftRefuted  Status = "soft_refuted"
	StatusHardRefuted  Status = "hard_refuted"
	StatusArchived     Status = "archived"
)

// Refutation is one (lesson, plan, outcome) record.
//
// Alpha is the realised excess return in fraction units (negative
// means the plan underperformed). DrawdownTriggered is the soft
// circuit breaker flag from the drawdown package. Either one alone
// is not enough — see Policy.IsBadOutcome.
type Refutation struct {
	MemoryID          string
	PlanID            string
	Alpha             float64
	DrawdownTriggered bool
	At                time.Time
}

// Policy holds the refutation thresholds. All defaults are
// deliberately conservative: we'd rather miss a refutation than
// silence a useful lesson.
type Policy struct {
	// AlphaFloor is the most-negative alpha at which a plan is
	// considered "fine, not refuting anything". Plans with
	// alpha < AlphaFloor count as refutations on their own.
	// Defaults to −0.005 (i.e. 0.5% under benchmark).
	AlphaFloor float64
	// AlphaSoftFloor is a milder floor that ALSO triggers a
	// refutation when combined with the drawdown circuit breaker.
	// Defaults to 0.0.
	AlphaSoftFloor float64
	// SoftRefuteThreshold is the refutation_count at which the
	// memory should flip from active → soft_refuted.
	SoftRefuteThreshold int
	// HardRefuteThreshold is the refutation_count at which the
	// memory should flip from soft_refuted → hard_refuted.
	HardRefuteThreshold int
}

// DefaultPolicy is the production-safe baseline.
func DefaultPolicy() Policy {
	return Policy{
		AlphaFloor:          -0.005,
		AlphaSoftFloor:      0.0,
		SoftRefuteThreshold: 3,
		HardRefuteThreshold: 5,
	}
}

// IsBadOutcome implements the "is this plan refuting the lesson?"
// gate. Both branches must be deliberate — the symmetric form would
// flag any plan with negative alpha, which is too aggressive.
func (p Policy) IsBadOutcome(alpha float64, drawdownTriggered bool) bool {
	if alpha < p.AlphaFloor {
		return true
	}
	if drawdownTriggered && alpha < p.AlphaSoftFloor {
		return true
	}
	return false
}

// NextStatus returns the target Status for a memory whose
// refutation_count just incremented to count. Returns currentStatus
// when no transition is warranted (so the caller can no-op).
func (p Policy) NextStatus(currentStatus Status, count int) Status {
	switch currentStatus {
	case StatusArchived, StatusHardRefuted:
		return currentStatus
	case StatusActive:
		if count >= p.HardRefuteThreshold {
			return StatusHardRefuted
		}
		if count >= p.SoftRefuteThreshold {
			return StatusSoftRefuted
		}
		return StatusActive
	case StatusSoftRefuted:
		if count >= p.HardRefuteThreshold {
			return StatusHardRefuted
		}
		return StatusSoftRefuted
	default:
		return StatusActive
	}
}

// Aggregate is the per-memory snapshot returned by Tracker.
type Aggregate struct {
	MemoryID         string    `json:"memoryId"`
	RefutationCount  int       `json:"refutationCount"`
	LastRefutedAt    time.Time `json:"lastRefutedAt"`
	CurrentStatus    Status    `json:"currentStatus"`
	RecommendedNext  Status    `json:"recommendedNext"`
	WasTransitioned  bool      `json:"wasTransitioned"`
}

// Tracker is the in-memory accumulator the wiring layer drains
// during plan-outcome resolution. It dedupes (memory, plan) pairs
// internally so re-running the resolver on the same plan is safe.
//
// The current status of each memory is supplied by the caller via
// SetCurrentStatus before applying refutations — the tracker does
// not read the database.
type Tracker struct {
	mu     sync.Mutex
	policy Policy
	state  map[string]*entry
}

type entry struct {
	memoryID string
	count    int
	lastAt   time.Time
	current  Status
	seen     map[string]struct{} // plan ids
}

// NewTracker returns an empty Tracker bound to the given policy.
// Pass DefaultPolicy() unless you have a tested reason otherwise.
func NewTracker(p Policy) *Tracker {
	return &Tracker{
		policy: normalisePolicy(p),
		state:  make(map[string]*entry),
	}
}

// SetCurrentStatus seeds the tracker with the database-side status
// before any refutations are applied. Calling this is required for
// NextStatus transitions to be correct.
func (t *Tracker) SetCurrentStatus(memoryID string, status Status, refutationCount int) {
	if t == nil || memoryID == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entryLocked(memoryID)
	e.current = normaliseStatus(status)
	if refutationCount > e.count {
		e.count = refutationCount
	}
}

// Apply registers a refutation. Returns the resulting Aggregate
// (refutation_count, recommended status). Duplicate (memory, plan)
// pairs are no-ops.
func (t *Tracker) Apply(r Refutation) Aggregate {
	if t == nil {
		return Aggregate{}
	}
	if r.MemoryID == "" || r.PlanID == "" {
		return Aggregate{}
	}
	if !t.policy.IsBadOutcome(r.Alpha, r.DrawdownTriggered) {
		// Even if the caller forwarded a non-bad outcome, return
		// a snapshot reflecting the current state so the caller
		// has something useful to log.
		return t.snapshot(r.MemoryID)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	e := t.entryLocked(r.MemoryID)
	if _, dup := e.seen[r.PlanID]; dup {
		return aggregateOf(e, t.policy)
	}
	e.seen[r.PlanID] = struct{}{}
	e.count++
	if r.At.After(e.lastAt) {
		e.lastAt = r.At
	}
	agg := aggregateOf(e, t.policy)
	if agg.RecommendedNext != e.current {
		agg.WasTransitioned = true
		e.current = agg.RecommendedNext
	}
	return agg
}

// Snapshot returns the current Aggregate for one memory.
func (t *Tracker) Snapshot(memoryID string) Aggregate {
	if t == nil {
		return Aggregate{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshot(memoryID)
}

func (t *Tracker) snapshot(memoryID string) Aggregate {
	e, ok := t.state[memoryID]
	if !ok {
		return Aggregate{MemoryID: memoryID, CurrentStatus: StatusActive, RecommendedNext: StatusActive}
	}
	return aggregateOf(e, t.policy)
}

// Snapshots returns all aggregates ordered by memory_id. Used by
// the wiring layer to drain the tracker into the database.
func (t *Tracker) Snapshots() []Aggregate {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Aggregate, 0, len(t.state))
	for _, e := range t.state {
		out = append(out, aggregateOf(e, t.policy))
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].MemoryID < out[j].MemoryID
	})
	return out
}

func (t *Tracker) entryLocked(memoryID string) *entry {
	e := t.state[memoryID]
	if e == nil {
		e = &entry{memoryID: memoryID, current: StatusActive, seen: make(map[string]struct{})}
		t.state[memoryID] = e
	}
	return e
}

func aggregateOf(e *entry, p Policy) Aggregate {
	return Aggregate{
		MemoryID:        e.memoryID,
		RefutationCount: e.count,
		LastRefutedAt:   e.lastAt,
		CurrentStatus:   e.current,
		RecommendedNext: p.NextStatus(e.current, e.count),
	}
}

func normalisePolicy(p Policy) Policy {
	if p.SoftRefuteThreshold <= 0 {
		p.SoftRefuteThreshold = DefaultPolicy().SoftRefuteThreshold
	}
	if p.HardRefuteThreshold <= 0 || p.HardRefuteThreshold < p.SoftRefuteThreshold {
		p.HardRefuteThreshold = max(p.SoftRefuteThreshold+2, DefaultPolicy().HardRefuteThreshold)
	}
	if p.AlphaFloor >= 0 {
		// AlphaFloor must be strictly negative — a non-negative
		// floor would refute every plan.
		p.AlphaFloor = DefaultPolicy().AlphaFloor
	}
	return p
}

func normaliseStatus(s Status) Status {
	switch s {
	case StatusActive, StatusSoftRefuted, StatusHardRefuted, StatusArchived:
		return s
	default:
		return StatusActive
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
