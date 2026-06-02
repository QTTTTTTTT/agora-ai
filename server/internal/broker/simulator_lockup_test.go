package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/matching"
)

// fakeLockupGate is a deterministic LockupGate stub.
type fakeLockupGate struct {
	verdict LockupVerdict
	calls   int
	last    LockupProbe
}

func (g *fakeLockupGate) CheckOrder(_ context.Context, probe LockupProbe) LockupVerdict {
	g.calls++
	g.last = probe
	return g.verdict
}

func newLockupGatedSimulator(t *testing.T, lockup LockupGate) *Simulator {
	t.Helper()
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	return NewSimulator(q, WithLockupGate(lockup), WithIDGenerator(func() string { return "broker-1" }))
}

func newSellRequest() PlaceOrderRequest {
	return PlaceOrderRequest{
		FundID:        "f1",
		ClientOrderID: "co-1",
		InstrumentKey: "AAPL.US",
		Symbol:        "AAPL",
		Market:        "US",
		AssetClass:    "equity",
		Side:          SideSell,
		Quantity:      50,
		OrderType:     OrderTypeMarket,
		TimeInForce:   TIFDay,
	}
}

func TestSimulator_LockupGateRejects_BlocksSell(t *testing.T) {
	gate := &fakeLockupGate{
		verdict: LockupVerdict{
			Rejected:     true,
			RejectReason: "locked: order requires 50 but only 40 unlocked, next unlock at 2026-12-01T00:00:00Z",
		},
	}
	sim := newLockupGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newSellRequest())
	if err == nil {
		t.Fatalf("expected reject, got order=%+v", got)
	}
	if !errors.Is(err, ErrLockupRejected) {
		t.Errorf("err = %v, want ErrLockupRejected", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d", gate.calls)
	}
	if gate.last.Side != "sell" {
		t.Errorf("gate side = %s", gate.last.Side)
	}
	if existing, _ := sim.GetOrderByClientID(context.Background(), "f1", "co-1"); existing != nil {
		t.Errorf("rejected order should not be booked: %+v", existing)
	}
}

func TestSimulator_LockupGateRejects_DefaultReason(t *testing.T) {
	gate := &fakeLockupGate{verdict: LockupVerdict{Rejected: true}}
	sim := newLockupGatedSimulator(t, gate)
	_, err := sim.PlaceOrder(context.Background(), newSellRequest())
	if err == nil || !errors.Is(err, ErrLockupRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestSimulator_LockupGateAllowsWithWarnings_AttachesToOrder(t *testing.T) {
	gate := &fakeLockupGate{
		verdict: LockupVerdict{
			Warnings: []string{"partial unlock approaching: 50 shares unlock 2026-12-01"},
		},
	}
	sim := newLockupGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newSellRequest())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] == "" {
		t.Errorf("warnings = %+v", got.Warnings)
	}
}

func TestSimulator_LockupGateBypassedForBuys(t *testing.T) {
	gate := &fakeLockupGate{
		// Even though gate would reject, side=buy must bypass.
		verdict: LockupVerdict{Rejected: true, RejectReason: "should not fire"},
	}
	sim := newLockupGatedSimulator(t, gate)
	req := newSellRequest()
	req.Side = SideBuy
	req.ClientOrderID = "co-buy"
	// The fake gate does not implement the side check itself —
	// but the production adapter does. Here we exercise that the
	// simulator hands every probe to the gate; the side filter is
	// the gate's responsibility. Confirm the probe carries the
	// right side so the production gate can short-circuit on
	// "buy".
	_, err := sim.PlaceOrder(context.Background(), req)
	// Test gate still rejects (fake doesn't filter), so we expect
	// the same path to fire — but the side must propagate.
	if err == nil {
		t.Fatalf("expected reject from fake gate, got nil")
	}
	if gate.last.Side != "buy" {
		t.Errorf("expected side=buy in probe, got %s", gate.last.Side)
	}
}

func TestSimulator_NoLockupGate_BehavesLikeBefore(t *testing.T) {
	sim := newLockupGatedSimulator(t, nil)
	got, err := sim.PlaceOrder(context.Background(), newSellRequest())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("expected order")
	}
}

func TestSimulator_BothGates_StatusFiresFirst(t *testing.T) {
	// When market-status rejects, lock-up is never asked. This
	// keeps the reject reason focused on the more dramatic halt.
	statusGate := &fakeGate{verdict: MarketStatusVerdict{Rejected: true, RejectReason: "halted: news"}}
	lockupGate := &fakeLockupGate{}
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	sim := NewSimulator(q,
		WithMarketStatusGate(statusGate),
		WithLockupGate(lockupGate),
		WithIDGenerator(func() string { return "broker-1" }),
	)
	_, err := sim.PlaceOrder(context.Background(), newSellRequest())
	if err == nil {
		t.Fatal("expected reject")
	}
	if !errors.Is(err, ErrMarketStatusRejected) {
		t.Errorf("err = %v, want ErrMarketStatusRejected first", err)
	}
	if lockupGate.calls != 0 {
		t.Errorf("lock-up gate should not have been called when status rejects, calls=%d", lockupGate.calls)
	}
}
