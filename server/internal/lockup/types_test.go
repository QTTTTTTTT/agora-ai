package lockup

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) time.Time {
	t.Helper()
	tt, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return tt
}

func TestEngine_BuyAlwaysAllowed(t *testing.T) {
	e := NewEngine().withClock(func() time.Time {
		return mustParse(t, "2026-09-01T00:00:00Z")
	})
	probe := Probe{
		FundID: "f1", InstrumentKey: "AAPL.US",
		Side: "buy", Quantity: 1000, PositionQty: 0,
	}
	d := e.Evaluate(probe, Snapshot{Records: []Record{{
		FundID: "f1", InstrumentKey: "AAPL.US",
		LockedQty: 1_000_000, LockedUntil: mustParse(t, "2099-01-01T00:00:00Z"),
		Reason: ReasonIPO,
	}}})
	if d.Kind != DecisionAllowNonSell || !d.Allowed() {
		t.Errorf("expected allow_non_sell, got %#v", d)
	}
}

func TestEngine_NoActiveRecords(t *testing.T) {
	e := NewEngine()
	probe := Probe{Side: "sell", Quantity: 100, PositionQty: 1000, AsOf: mustParse(t, "2026-09-01T00:00:00Z")}
	// All expired or released.
	rel := mustParse(t, "2026-08-15T00:00:00Z")
	snap := Snapshot{Records: []Record{
		{LockedQty: 200, LockedUntil: mustParse(t, "2026-08-01T00:00:00Z"), Reason: ReasonIPO},          // expired
		{LockedQty: 100, LockedUntil: mustParse(t, "2099-01-01T00:00:00Z"), ReleasedAt: &rel, Reason: ReasonRSU}, // released
	}}
	d := e.Evaluate(probe, snap)
	if d.Kind != DecisionAllowNoLockup {
		t.Errorf("expected allow_no_lockup, got %#v", d)
	}
}

func TestEngine_AllowWithinUnlocked(t *testing.T) {
	e := NewEngine()
	asOf := mustParse(t, "2026-09-01T00:00:00Z")
	probe := Probe{Side: "sell", Quantity: 40, PositionQty: 100, AsOf: asOf}
	snap := Snapshot{Records: []Record{
		{LockedQty: 60, LockedUntil: mustParse(t, "2026-12-01T00:00:00Z"), Reason: ReasonIPO},
	}}
	d := e.Evaluate(probe, snap)
	if d.Kind != DecisionAllow {
		t.Fatalf("expected allow, got %#v", d)
	}
	if d.LockedQty != 60 || d.AvailableQty != 40 {
		t.Errorf("locked=%v available=%v", d.LockedQty, d.AvailableQty)
	}
}

func TestEngine_RejectExceedsAvailable(t *testing.T) {
	e := NewEngine()
	asOf := mustParse(t, "2026-09-01T00:00:00Z")
	unlock := mustParse(t, "2026-12-01T00:00:00Z")
	probe := Probe{Side: "sell", Quantity: 50, PositionQty: 100, AsOf: asOf}
	snap := Snapshot{Records: []Record{
		{LockedQty: 60, LockedUntil: unlock, Reason: ReasonIPO},
	}}
	d := e.Evaluate(probe, snap)
	if d.Kind != DecisionRejectLocked {
		t.Fatalf("expected reject_locked, got %#v", d)
	}
	if d.AvailableQty != 40 {
		t.Errorf("available = %v", d.AvailableQty)
	}
	if d.NextUnlockAt == nil || !d.NextUnlockAt.Equal(unlock) {
		t.Errorf("NextUnlockAt = %v want %v", d.NextUnlockAt, unlock)
	}
	if !strings.Contains(d.Reason, "next unlock at") {
		t.Errorf("reason missing next-unlock: %s", d.Reason)
	}
}

func TestEngine_MultipleRecordsSummed(t *testing.T) {
	e := NewEngine()
	asOf := mustParse(t, "2026-09-01T00:00:00Z")
	earliest := mustParse(t, "2026-12-01T00:00:00Z")
	later := mustParse(t, "2027-03-01T00:00:00Z")
	snap := Snapshot{Records: []Record{
		{LockedQty: 60, LockedUntil: later, Reason: ReasonRSU},
		{LockedQty: 40, LockedUntil: earliest, Reason: ReasonIPO},
		{LockedQty: 20, LockedUntil: mustParse(t, "2026-08-01T00:00:00Z"), Reason: ReasonOther}, // expired, ignored
	}}
	probe := Probe{Side: "sell", Quantity: 1, PositionQty: 100, AsOf: asOf}
	d := e.Evaluate(probe, snap)
	// Active locked = 60 + 40 = 100; position 100 → available 0; quantity 1 > 0 → reject.
	if d.Kind != DecisionRejectLocked {
		t.Fatalf("expected reject, got %#v", d)
	}
	if d.LockedQty != 100 || d.AvailableQty != 0 {
		t.Errorf("locked=%v available=%v", d.LockedQty, d.AvailableQty)
	}
	if d.NextUnlockAt == nil || !d.NextUnlockAt.Equal(earliest) {
		t.Errorf("NextUnlockAt should be earliest active unlock, got %v", d.NextUnlockAt)
	}
}

func TestEngine_LockedExceedsPositionCapsAtZero(t *testing.T) {
	// Config bug: 200 locked vs 100 position. Engine caps available
	// at 0 and rejects, doesn't go negative.
	e := NewEngine()
	asOf := mustParse(t, "2026-09-01T00:00:00Z")
	snap := Snapshot{Records: []Record{
		{LockedQty: 200, LockedUntil: mustParse(t, "2099-01-01T00:00:00Z"), Reason: ReasonIPO},
	}}
	probe := Probe{Side: "sell", Quantity: 1, PositionQty: 100, AsOf: asOf}
	d := e.Evaluate(probe, snap)
	if d.AvailableQty != 0 {
		t.Errorf("available should cap at 0, got %v", d.AvailableQty)
	}
	if d.Kind != DecisionRejectLocked {
		t.Errorf("kind = %s", d.Kind)
	}
}

func TestEngine_NoPosition(t *testing.T) {
	e := NewEngine()
	asOf := mustParse(t, "2026-09-01T00:00:00Z")
	snap := Snapshot{Records: []Record{
		{LockedQty: 50, LockedUntil: mustParse(t, "2026-12-01T00:00:00Z"), Reason: ReasonIPO},
	}}
	probe := Probe{Side: "sell", Quantity: 10, PositionQty: 0, AsOf: asOf}
	d := e.Evaluate(probe, snap)
	if d.Kind != DecisionRejectNoPos {
		t.Errorf("kind = %s", d.Kind)
	}
}

func TestEngine_AsOfDefaultsToNow(t *testing.T) {
	clock := mustParse(t, "2026-09-01T00:00:00Z")
	e := NewEngine().withClock(func() time.Time { return clock })
	probe := Probe{Side: "sell", Quantity: 10, PositionQty: 100} // no AsOf
	snap := Snapshot{Records: []Record{
		{LockedQty: 50, LockedUntil: mustParse(t, "2026-12-01T00:00:00Z"), Reason: ReasonIPO},
	}}
	d := e.Evaluate(probe, snap)
	if !d.AsOf.Equal(clock) {
		t.Errorf("AsOf default = %v, want %v", d.AsOf, clock)
	}
}

func TestRecord_Active(t *testing.T) {
	now := mustParse(t, "2026-09-01T00:00:00Z")
	released := mustParse(t, "2026-08-15T00:00:00Z")
	cases := []struct {
		name string
		r    Record
		want bool
	}{
		{"active", Record{LockedUntil: mustParse(t, "2026-12-01T00:00:00Z")}, true},
		{"expired", Record{LockedUntil: mustParse(t, "2026-08-01T00:00:00Z")}, false},
		{"released", Record{LockedUntil: mustParse(t, "2026-12-01T00:00:00Z"), ReleasedAt: &released}, false},
	}
	for _, c := range cases {
		if got := c.r.Active(now); got != c.want {
			t.Errorf("%s: Active = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestIsValidReason(t *testing.T) {
	for _, r := range AllReasons {
		if !IsValidReason(string(r)) {
			t.Errorf("expected %s to be valid", r)
		}
	}
	if IsValidReason("nonsense") {
		t.Error("expected nonsense to be invalid")
	}
}
