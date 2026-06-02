// Package lockup implements the S6.3 IPO / private-placement /
// restricted-share lock-up gate.
//
// Domain story
//
// When a fund acquires shares via an IPO allocation, a pre-IPO
// private placement, an RSU vest, or any other channel that
// imposes a contractual hold, those shares cannot be sold for
// some period (typically 90 / 180 days). The lock-up is
// associated with the *event of acquisition* — different lots
// can unlock on different dates.
//
// For each (fund, instrument), the engine sums the locked qty
// across all currently-active records:
//
//	locked_at(t)   = Σ rec.LockedQty where
//	                   rec.ReleasedAt IS NULL
//	                AND rec.LockedUntil > t
//	available(t)   = max(0, position_qty - locked_at(t))
//
// A sell of size Q at time t is allowed iff Q <= available(t).
//
// Buys are never blocked by lock-ups (the constraint is on
// disposing existing shares, not acquiring new ones).
//
// What this package owns
//
//   - Domain types: Record, Probe, Snapshot, Decision, Engine.
//   - Pure decision logic (no DB / time.Now() / I/O).
//   - DB-backed Repo for the position_lockups table.
//
// What it does NOT own
//
//   - Loading the position quantity / active records — the
//     cmd/server adapter does that, then hands a complete
//     Snapshot to the engine.
//   - Wiring the gate into the broker simulator — that's an
//     adapter in cmd/server too, which keeps internal/lockup
//     free of broker dependencies (and circular imports).
package lockup

import (
	"errors"
	"strings"
	"time"
)

// LockupReason mirrors the closed enum in the schema. Operators
// pick one when creating a record so analytics can pivot by
// reason later (IPO vs RSU mix, etc).
type LockupReason string

const (
	ReasonIPO              LockupReason = "ipo"
	ReasonPrivatePlacement LockupReason = "private_placement"
	ReasonRSU              LockupReason = "rsu"
	ReasonRestricted       LockupReason = "restricted"
	ReasonEmployeeGrant    LockupReason = "employee_grant"
	ReasonBlockSale        LockupReason = "block_sale"
	ReasonOther            LockupReason = "other"
)

// AllReasons is the canonical list. Used by the admin handler
// when validating writes.
var AllReasons = []LockupReason{
	ReasonIPO, ReasonPrivatePlacement, ReasonRSU, ReasonRestricted,
	ReasonEmployeeGrant, ReasonBlockSale, ReasonOther,
}

// IsValidReason reports whether s is one of AllReasons.
func IsValidReason(s string) bool {
	v := LockupReason(strings.ToLower(strings.TrimSpace(s)))
	for _, r := range AllReasons {
		if r == v {
			return true
		}
	}
	return false
}

// Record is one row of position_lockups. Pointer fields
// distinguish "no value" from "explicit zero".
type Record struct {
	ID             string
	FundID         string
	InstrumentKey  string
	Symbol         string
	LockedQty      float64
	LockedUntil    time.Time
	Reason         LockupReason
	SourceLotID    *string
	Note           string
	ReleasedAt     *time.Time
	ReleasedReason string
	ReleasedBy     string
	CreatedBy      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Active reports whether this record is counted by the engine
// at time t — released_at IS NULL AND locked_until > t.
func (r Record) Active(t time.Time) bool {
	if r.ReleasedAt != nil {
		return false
	}
	return r.LockedUntil.After(t)
}

// Probe is the gate input. PositionQty is required; the engine
// caps available at zero rather than going negative if the sum
// of locked qty exceeds it (which would be a config bug).
type Probe struct {
	FundID        string
	InstrumentKey string
	Symbol        string
	Side          string  // "buy" | "sell"
	Quantity      float64
	PositionQty   float64
	AsOf          time.Time
}

// Snapshot bundles the records the cmd/server adapter loaded
// for this (fund, instrument). The engine filters them by
// Active(asOf) so callers can pass everything (including
// expired / released records) without double-filtering.
type Snapshot struct {
	Records []Record
}

// DecisionKind is the closed verdict vocabulary.
type DecisionKind string

const (
	DecisionAllow         DecisionKind = "allow"
	DecisionRejectLocked  DecisionKind = "reject_locked"
	DecisionRejectNoPos   DecisionKind = "reject_no_position"
	DecisionAllowNonSell  DecisionKind = "allow_non_sell"
	DecisionAllowNoLockup DecisionKind = "allow_no_lockup"
)

// Decision is the engine output. NextUnlockAt is the earliest
// `locked_until` of any active record; on a reject it tells
// the operator "the next time this could become tradeable".
type Decision struct {
	Kind         DecisionKind
	LockedQty    float64
	AvailableQty float64
	OrderQty     float64
	NextUnlockAt *time.Time
	// Reason is a human-readable string suitable for the broker
	// reject reason / order warning.
	Reason   string
	AsOf     time.Time
	// Records are the active records the engine summed.
	// Caller can attach to event log without a second DB read.
	Records []Record
}

// Allowed is a convenience predicate on Kind.
func (d Decision) Allowed() bool {
	switch d.Kind {
	case DecisionAllow, DecisionAllowNonSell, DecisionAllowNoLockup:
		return true
	default:
		return false
	}
}

// Engine is the pure decision function. Stateless; safe to
// share across goroutines.
type Engine struct {
	now func() time.Time
}

// NewEngine returns the engine.
func NewEngine() *Engine {
	return &Engine{now: func() time.Time { return time.Now().UTC() }}
}

// withClock is a test seam.
func (e *Engine) withClock(now func() time.Time) *Engine {
	if now != nil {
		e.now = now
	}
	return e
}

// Evaluate runs the lock-up rule. Pure: no DB, no I/O.
//
// Behaviour:
//
//   - Side != "sell"            → DecisionAllowNonSell.
//   - No active records         → DecisionAllowNoLockup.
//   - Order qty <= available    → DecisionAllow.
//   - Order qty >  available    → DecisionRejectLocked, with
//     NextUnlockAt = min of active records' LockedUntil.
//   - PositionQty <= 0          → still evaluated normally; the
//     broker layer separately rejects "you have no position".
//     The engine simply reports DecisionRejectLocked when the
//     locked qty > 0 (sell against zero position is also a
//     reject anywhere downstream). This avoids carrying
//     position-availability semantics into the lock-up rule.
func (e *Engine) Evaluate(probe Probe, snap Snapshot) Decision {
	asOf := probe.AsOf
	if asOf.IsZero() && e != nil && e.now != nil {
		asOf = e.now()
	}
	side := strings.ToLower(strings.TrimSpace(probe.Side))
	if side != "sell" {
		return Decision{
			Kind:         DecisionAllowNonSell,
			OrderQty:     probe.Quantity,
			AvailableQty: probe.PositionQty,
			Reason:       "non-sell side: lock-up does not apply",
			AsOf:         asOf,
		}
	}

	// Filter active records and sum locked qty.
	var (
		locked    float64
		records   []Record
		nextUnloc *time.Time
	)
	for _, r := range snap.Records {
		if !r.Active(asOf) {
			continue
		}
		records = append(records, r)
		locked += r.LockedQty
		t := r.LockedUntil
		if nextUnloc == nil || t.Before(*nextUnloc) {
			tt := t
			nextUnloc = &tt
		}
	}
	if len(records) == 0 {
		return Decision{
			Kind:         DecisionAllowNoLockup,
			OrderQty:     probe.Quantity,
			AvailableQty: probe.PositionQty,
			Reason:       "no active lock-up records",
			AsOf:         asOf,
		}
	}
	available := probe.PositionQty - locked
	if available < 0 {
		available = 0
	}
	if probe.Quantity <= available {
		return Decision{
			Kind:         DecisionAllow,
			OrderQty:     probe.Quantity,
			LockedQty:    locked,
			AvailableQty: available,
			NextUnlockAt: nextUnloc,
			Records:      records,
			AsOf:         asOf,
			Reason:       "ok: order within unlocked qty",
		}
	}
	if probe.PositionQty <= 0 {
		return Decision{
			Kind:         DecisionRejectNoPos,
			OrderQty:     probe.Quantity,
			LockedQty:    locked,
			AvailableQty: 0,
			NextUnlockAt: nextUnloc,
			Records:      records,
			AsOf:         asOf,
			Reason:       "no position to sell against",
		}
	}
	return Decision{
		Kind:         DecisionRejectLocked,
		OrderQty:     probe.Quantity,
		LockedQty:    locked,
		AvailableQty: available,
		NextUnlockAt: nextUnloc,
		Records:      records,
		AsOf:         asOf,
		Reason:       formatRejectReason(probe.Quantity, available, nextUnloc),
	}
}

func formatRejectReason(orderQty, available float64, nextUnloc *time.Time) string {
	var b strings.Builder
	b.WriteString("locked: order requires ")
	b.WriteString(formatQty(orderQty))
	b.WriteString(" but only ")
	b.WriteString(formatQty(available))
	b.WriteString(" unlocked")
	if nextUnloc != nil {
		b.WriteString(", next unlock at ")
		b.WriteString(nextUnloc.UTC().Format(time.RFC3339))
	}
	return b.String()
}

func formatQty(q float64) string {
	// strconv.FormatFloat avoids trailing zeros for whole-number
	// share counts ("100" instead of "100.000000").
	if q == float64(int64(q)) {
		return formatInt(int64(q))
	}
	return formatFloat(q)
}

func formatInt(n int64) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func formatFloat(f float64) string {
	// We round to 4 decimals (matches the schema's NUMERIC(20,4)).
	r := float64(int64(f*10000+0.5)) / 10000
	whole := int64(r)
	frac := int64((r - float64(whole)) * 10000)
	if frac < 0 {
		frac = -frac
	}
	out := formatInt(whole)
	if frac != 0 {
		out += "."
		// Pad to 4 digits.
		s := formatInt(frac)
		for len(s) < 4 {
			s = "0" + s
		}
		// Strip trailing zeros.
		for len(s) > 1 && s[len(s)-1] == '0' {
			s = s[:len(s)-1]
		}
		out += s
	}
	return out
}

// ----- Errors -----

var (
	ErrInvalidRecord = errors.New("lockup: invalid record")
)
