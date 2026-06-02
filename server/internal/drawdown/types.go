// Package drawdown implements P3-5 — the drawdown soft circuit
// breaker.
//
// What this package owns
//
//   - Domain types: Tier, Policy, Snapshot (peak / current /
//     positions), BreachEvent, TrimPlanItem.
//   - The pure Engine that turns (Snapshot, Policy) into either
//     "no breach" or a BreachEvent with a TrimPlan.
//   - The DB-backed Repo for policy CRUD + event lifecycle.
//
// What this package does NOT own
//
//   - Reading peak/current NAV or holdings. The cmd/server adapter
//     `drawdown_snapshot.go` does that; the engine consumes a
//     pre-built Snapshot.
//   - Submitting the trim orders. The execution side calls into
//     the existing order pipeline (idempotency + risk gates +
//     audit chain) — keeping the engine pure means it can be
//     unit-tested with golden fixtures.
//
// "Soft" circuit breaker
//
// Unlike the hard circuit breaker in agent/risk.go (which rejects
// a plan with `Rejections`), the soft breaker REDUCES exposure
// rather than halting trading entirely. Three actions:
//
//   * trim_proportional — sell `trim_ratio` of every long position
//     (e.g. 25% trim = sell 25% of each holding). Pro-rata keeps
//     the portfolio shape intact while reducing notional.
//   * flatten — sell 100% of every long position (and close shorts);
//     end of day = all cash.
//   * defensive_only — record an event but emit no trim orders;
//     intent is for the order entry layer to consult the same
//     policy and reject NEW long exposure while DD is breached.
//
// The engine is conservative: it always emits a `proposed` event
// for operator review unless `policy.auto_execute` is true. Even
// in auto mode the orders flow through the standard pipeline so
// any other risk check still applies.
//
// Cooldown
//
// `policy.cooldown_hours` rate-limits firing the SAME tier twice
// in a short window. The Engine doesn't know wall-clock time on
// its own; the snapshot includes `LastFiredAt[tier]` so the
// engine can compare and short-circuit.

package drawdown

import (
	"errors"
	"math"
	"sort"
	"strings"
	"time"
)

// Action is the closed vocabulary the engine emits. Matches the
// CHECK on `drawdown_policies.action` and `drawdown_events.action`.
type Action string

const (
	ActionTrimProportional Action = "trim_proportional"
	ActionFlatten          Action = "flatten"
	ActionDefensiveOnly    Action = "defensive_only"
)

// Status is the closed vocabulary for `drawdown_events.status`.
type Status string

const (
	StatusProposed   Status = "proposed"
	StatusApproved   Status = "approved"
	StatusExecuted   Status = "executed"
	StatusDismissed  Status = "dismissed"
	StatusSuperseded Status = "superseded"
)

// Tier holds one row of `drawdown_policies`.
type Tier struct {
	Tier          int       // 1..5
	DDPct         float64   // negative fraction, e.g. -0.05
	Action        Action
	TrimRatio     float64   // 0..1; only meaningful for trim_proportional
	CooldownHours int
	AutoExecute   bool
	Note          string
}

// Policy is the per-fund tier set, sorted from mildest to
// hardest. Operators add/remove tiers freely; the engine takes
// whichever is the WORST currently breached.
type Policy struct {
	FundID string
	Tiers  []Tier
}

// Position is one open holding the engine considers for a trim.
// Long positions only — shorts are out of scope for v1 (the
// soft breaker has no clear answer for "trim a short" because
// covering is a buy that increases risk, not reduces it).
type Position struct {
	Symbol        string
	InstrumentKey string
	Quantity      float64 // positive number; the trim engine ignores ≤ 0
	AvgCost       float64
	MarketValue   float64
}

// Snapshot is the per-fund input. Pure: no DB handles, no I/O.
type Snapshot struct {
	FundID       string
	PeakNAV      float64
	CurrentNAV   float64
	AsOf         time.Time
	NavSnapshotID string
	Positions    []Position
	// LastFiredAt maps tier number → most recent detected_at.
	// The engine uses this to enforce the cooldown without
	// reaching back into the DB.
	LastFiredAt  map[int]time.Time
}

// BreachEvent is the engine's output when a tier fires. Empty
// trim_plan for defensive_only.
type BreachEvent struct {
	FundID         string
	Tier           int
	CurrentDDPct   float64
	PeakNAV        float64
	CurrentNAV     float64
	Action         Action
	TrimPlan       []TrimPlanItem
	NavSnapshotID  string
	DetectedAt     time.Time
	DetectorVersion string
	Metadata       map[string]any
}

// TrimPlanItem is one element of the engine's order suggestion.
// Always side="sell"; quantity is positive. The order layer
// translates these into market-sell orders (or limit-at-mid for
// less liquid names, depending on the policy — that's an order
// layer concern, not an engine concern).
type TrimPlanItem struct {
	Symbol        string  `json:"symbol"`
	InstrumentKey string  `json:"instrument_key,omitempty"`
	Side          string  `json:"side"`     // always "sell"
	Quantity      float64 `json:"quantity"` // positive shares to sell
	Reason        string  `json:"reason"`   // "trim 25% of position"
}

// detectorVersion stamps every event so a future engine upgrade
// can re-evaluate a snapshot without dedup colliding.
const detectorVersion = "v1"

// ----- Errors -----

var (
	ErrPolicyNotFound = errors.New("drawdown: policy not found")
	ErrEventNotFound  = errors.New("drawdown: event not found")
	ErrInvalidStatus  = errors.New("drawdown: invalid status")
	ErrInvalidTier    = errors.New("drawdown: invalid tier")
)

// ----- Engine -----

// Engine evaluates a Snapshot against a Policy.
type Engine struct{}

// NewEngine returns the production engine. Stateless; safe to
// share across goroutines.
func NewEngine() *Engine { return &Engine{} }

// ComputeDD returns the current drawdown as a non-positive fraction.
//
//   peak <= 0 → 0 (we have no reference, so no DD); reporting "DD"
//                 against a non-positive peak would be nonsense.
//   current >= peak → 0 (we're at or above peak; no DD).
//   else            → (current - peak) / peak (negative).
func ComputeDD(peak, current float64) float64 {
	if peak <= 0 {
		return 0
	}
	if current >= peak {
		return 0
	}
	return (current - peak) / peak
}

// Evaluate is the engine's main entry point. Returns nil if no
// tier breached or the breached tier is in cooldown. Returns the
// worst-tier event otherwise.
func (e *Engine) Evaluate(snap *Snapshot, policy *Policy) (*BreachEvent, error) {
	if snap == nil {
		return nil, errors.New("drawdown: nil snapshot")
	}
	if policy == nil {
		return nil, errors.New("drawdown: nil policy")
	}
	if strings.TrimSpace(snap.FundID) == "" || strings.TrimSpace(policy.FundID) == "" {
		return nil, errors.New("drawdown: fund_id required")
	}
	if snap.FundID != policy.FundID {
		return nil, errors.New("drawdown: snapshot/policy fund_id mismatch")
	}
	if len(policy.Tiers) == 0 {
		return nil, nil
	}

	dd := ComputeDD(snap.PeakNAV, snap.CurrentNAV)
	if dd >= 0 {
		return nil, nil
	}

	// Sort tiers from hardest (most negative dd_pct) to mildest
	// so we pick the worst breach in one pass.
	tiers := append([]Tier(nil), policy.Tiers...)
	sort.Slice(tiers, func(i, j int) bool {
		return tiers[i].DDPct < tiers[j].DDPct
	})

	now := snap.AsOf
	if now.IsZero() {
		now = time.Now().UTC()
	}

	for _, tier := range tiers {
		if dd > tier.DDPct {
			// Not deep enough for THIS tier; since tiers are
			// sorted hardest-first, we can keep going to softer
			// tiers, but a softer tier MIGHT match.
			continue
		}
		// Cooldown check: skip if we fired this tier recently.
		if tier.CooldownHours > 0 {
			if last, ok := snap.LastFiredAt[tier.Tier]; ok {
				if !last.IsZero() && now.Sub(last) < time.Duration(tier.CooldownHours)*time.Hour {
					continue
				}
			}
		}
		// Tier matched and not in cooldown.
		ev := &BreachEvent{
			FundID:          snap.FundID,
			Tier:            tier.Tier,
			CurrentDDPct:    dd,
			PeakNAV:         snap.PeakNAV,
			CurrentNAV:      snap.CurrentNAV,
			Action:          tier.Action,
			NavSnapshotID:   snap.NavSnapshotID,
			DetectedAt:      now,
			DetectorVersion: detectorVersion,
			Metadata: map[string]any{
				"trim_ratio":     tier.TrimRatio,
				"cooldown_hours": tier.CooldownHours,
				"auto_execute":   tier.AutoExecute,
			},
		}
		ev.TrimPlan = BuildTrimPlan(snap.Positions, tier)
		return ev, nil
	}
	return nil, nil
}

// BuildTrimPlan turns a tier into a list of sell suggestions.
// Pure function; safe to call independently of Evaluate (the admin
// UI calls it for "preview" before approving).
//
// Allocation strategy:
//
//   - trim_proportional: floor(qty * trim_ratio) for every position
//     with qty > 0. We ROUND DOWN so the trim never accidentally
//     overshoots a fractional share. Positions whose trim resolves
//     to < 1 share are left alone (selling 0.7 of a share is
//     usually impossible anyway).
//   - flatten: sell ALL of every long position. trim_ratio is
//     ignored.
//   - defensive_only: empty plan; the order entry layer is
//     expected to consult the policy and reject NEW long exposure.
func BuildTrimPlan(positions []Position, tier Tier) []TrimPlanItem {
	if len(positions) == 0 {
		return nil
	}
	switch tier.Action {
	case ActionFlatten:
		out := make([]TrimPlanItem, 0, len(positions))
		for _, p := range positions {
			if p.Quantity <= 0 {
				continue
			}
			out = append(out, TrimPlanItem{
				Symbol:        p.Symbol,
				InstrumentKey: p.InstrumentKey,
				Side:          "sell",
				Quantity:      p.Quantity,
				Reason:        "flatten",
			})
		}
		return out
	case ActionTrimProportional:
		ratio := tier.TrimRatio
		if ratio <= 0 || ratio > 1 {
			return nil
		}
		out := make([]TrimPlanItem, 0, len(positions))
		for _, p := range positions {
			if p.Quantity <= 0 {
				continue
			}
			qty := math.Floor(p.Quantity * ratio)
			if qty < 1 {
				continue
			}
			out = append(out, TrimPlanItem{
				Symbol:        p.Symbol,
				InstrumentKey: p.InstrumentKey,
				Side:          "sell",
				Quantity:      qty,
				Reason:        "trim_proportional",
			})
		}
		return out
	case ActionDefensiveOnly:
		return nil
	default:
		return nil
	}
}

// SortPolicyTiers returns a copy of the tiers ordered ASCENDING by
// the tier number. The admin UI renders them top-down by tier.
func SortPolicyTiers(in []Tier) []Tier {
	out := append([]Tier(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Tier < out[j].Tier })
	return out
}

// canonicalAction trims+lowercases. Used by the repo when accepting
// strings from JSON requests.
func canonicalAction(s string) Action {
	return Action(strings.ToLower(strings.TrimSpace(s)))
}
