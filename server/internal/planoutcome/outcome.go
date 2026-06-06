// Package planoutcome owns the post-decision OUTCOME schema:
// "given that plan P was decided at T0 and its window W has now
// elapsed, what was the realised performance?". It is the
// counterpart to internal/decision.Provenance:
//
//   * Provenance captures WHAT shaped the plan (lessons, skills,
//     signal blocks). Captured at decide-time.
//   * Outcome captures HOW the plan performed. Captured at
//     window-end-time, asynchronously.
//
// Together, the two close the self-learning loop: the Wave-2
// trackers (calibration #7, skill effectiveness #8, lesson
// refute #9) join on plan_id to compute "did using lesson X
// actually correlate with positive alpha?".
//
// SCOPE
// -----
// This package owns:
//   * the Outcome value type;
//   * its JSON shape (so the repository can pass through bytes);
//   * the Resolver interface (one method, ResolveForPlan, that
//     concrete implementations satisfy);
//   * a NoopResolver for tests / pre-wiring builds;
//   * a small WindowKind enum for the canonical resolver flavours.
//
// It deliberately does NOT own:
//   * The database access. That lives in repository.PlanRepo
//     (SetPlanOutcome / GetPlanOutcome).
//   * The actual P&L computation. That lives in the wiring layer
//     because it needs market data + position state + benchmark
//     curves; this package would otherwise drag a bulky import
//     graph into every consumer that just wants to read a row.
package planoutcome

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// WindowKind enumerates the canonical post-decision windows the
// outcome resolver supports. Adding a new kind here is allowed
// without a migration — the column is JSONB, so new values just
// flow through.
type WindowKind string

const (
	// WindowFixed5d closes the outcome window 5 trading days
	// after plan creation. Matches the canonical "short-term
	// thesis" horizon.
	WindowFixed5d WindowKind = "fixed_5d"
	// WindowFixed10d is the medium-term horizon — typical for
	// fundamentals-driven theses that need a few earnings
	// reactions to mature.
	WindowFixed10d WindowKind = "fixed_10d"
	// WindowFixed20d is the long-term horizon for
	// regime-following / value sleeves where the thesis takes
	// a full month to play out.
	WindowFixed20d WindowKind = "fixed_20d"
	// WindowNextEarnings closes at the symbol's next scheduled
	// earnings release. Used for PEAD / earnings-drift theses
	// (Wave 3 #16).
	WindowNextEarnings WindowKind = "next_earnings"
	// WindowNextNews closes at the next high-importance news
	// catalyst for the symbol. Used for catalyst theses.
	WindowNextNews WindowKind = "next_news"
	// WindowManual is set by an operator who closed the
	// position by hand. The notes field carries the rationale.
	WindowManual WindowKind = "manual"
)

// IsValid reports whether the receiver is one of the canonical
// WindowKind values. New unrecognised kinds (a future engine
// added a new resolver) are reported as valid; we only fail on
// empty.
func (k WindowKind) IsValid() bool {
	return k != ""
}

// Outcome is the persisted value object for the plan_outcome
// JSONB column. All fields are optional from the wire's
// perspective — a partial Outcome is more useful than no
// Outcome.
//
// Field order matches the column comment in
// migrations/094_plan_outcome.sql; new fields go at the end so
// the diff stays clean.
type Outcome struct {
	WindowKind     WindowKind `json:"windowKind"`
	WindowEndedAt  time.Time  `json:"windowEndedAt"`
	RealizedPnL    float64    `json:"realizedPnL,omitempty"`
	VsBenchmark    float64    `json:"vsBenchmark,omitempty"`
	Alpha          float64    `json:"alpha,omitempty"`
	WinRate        float64    `json:"winRate,omitempty"`
	SampleCount    int        `json:"sampleCount,omitempty"`
	ComputedAt     time.Time  `json:"computedAt"`
	ComputedBy     string     `json:"computedBy,omitempty"`
	Notes          string     `json:"notes,omitempty"`
}

// Marshal serialises the Outcome to JSON suitable for handing to
// repository.PlanRepo.SetPlanOutcome. Returns nil for the zero
// value so the repo can pass nil through to a SQL NULL.
func Marshal(o Outcome) ([]byte, error) {
	if o.IsZero() {
		return nil, nil
	}
	if !o.WindowKind.IsValid() {
		return nil, fmt.Errorf("planoutcome: missing windowKind on non-zero Outcome")
	}
	return json.Marshal(o)
}

// Unmarshal deserialises a JSONB blob into an Outcome. nil /
// empty input returns the zero Outcome with no error — readers
// should treat zero as "the resolver has not run yet".
func Unmarshal(raw []byte) (Outcome, error) {
	var out Outcome
	if len(raw) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return Outcome{}, fmt.Errorf("planoutcome: unmarshal: %w", err)
	}
	return out, nil
}

// IsZero reports whether the Outcome is the empty value (no
// resolver ever ran). The zero check matches the round-trip
// contract in Marshal: any non-zero field must produce a
// non-nil JSON payload.
func (o Outcome) IsZero() bool {
	return o.WindowKind == "" &&
		o.WindowEndedAt.IsZero() &&
		o.RealizedPnL == 0 &&
		o.VsBenchmark == 0 &&
		o.Alpha == 0 &&
		o.WinRate == 0 &&
		o.SampleCount == 0 &&
		o.ComputedAt.IsZero() &&
		o.ComputedBy == "" &&
		o.Notes == ""
}

// Resolver is the contract the wiring layer satisfies to compute
// outcomes for a plan id. The contract is:
//
//   - Returns (zero, false, nil) when the window for the plan
//     has not yet elapsed; caller skips and tries again later.
//   - Returns (outcome, true, nil) when the window has elapsed
//     and the implementation produced a snapshot. Caller
//     persists the outcome via PlanRepo.SetPlanOutcome.
//   - Returns (_, _, err) on a transient or permanent failure.
//     Caller decides whether to retry.
//
// The interface intentionally takes the plan id (not a Plan
// struct) so the resolver implementation can fetch any
// additional context it needs from its own repos. Keeping the
// surface narrow lets us add a NoopResolver for tests +
// pre-wiring without dragging in market-data dependencies.
type Resolver interface {
	ResolveForPlan(ctx context.Context, planID string) (Outcome, bool, error)
}

// NoopResolver is the placeholder Resolver implementation:
// always returns (zero, false, nil) — i.e. "the window hasn't
// elapsed for any plan". Used by tests and by the production
// build until the wiring layer attaches a real PnL-aware
// resolver. Soft-fails through the rest of the persistence
// stack.
type NoopResolver struct{}

// ResolveForPlan — see Resolver. NoopResolver always declines.
func (NoopResolver) ResolveForPlan(_ context.Context, _ string) (Outcome, bool, error) {
	return Outcome{}, false, nil
}
