package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/matching"
)

type fakeBorrowGate struct {
	verdict BorrowVerdict
	calls   int
	last    BorrowProbe
}

func (g *fakeBorrowGate) CheckOrder(_ context.Context, probe BorrowProbe) BorrowVerdict {
	g.calls++
	g.last = probe
	return g.verdict
}

func newBorrowGatedSimulator(t *testing.T, borrow BorrowGate) *Simulator {
	t.Helper()
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	return NewSimulator(q, WithBorrowGate(borrow), WithIDGenerator(func() string { return "broker-1" }))
}

func newShortSellRequest() PlaceOrderRequest {
	return PlaceOrderRequest{
		FundID:        "f1",
		ClientOrderID: "co-short",
		InstrumentKey: "TSLA.US",
		Symbol:        "TSLA",
		Market:        "US",
		AssetClass:    "equity",
		Side:          SideSell,
		Quantity:      1000,
		OrderType:     OrderTypeMarket,
		TimeInForce:   TIFDay,
	}
}

func TestSimulator_BorrowGateRejects_ShortBlocked(t *testing.T) {
	gate := &fakeBorrowGate{
		verdict: BorrowVerdict{
			Rejected:     true,
			RejectReason: "borrow unavailable: hard-to-borrow",
		},
	}
	sim := newBorrowGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if err == nil {
		t.Fatalf("expected reject, got %+v", got)
	}
	if !errors.Is(err, ErrBorrowRejected) {
		t.Errorf("err = %v, want ErrBorrowRejected", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate.calls = %d", gate.calls)
	}
	if existing, _ := sim.GetOrderByClientID(context.Background(), "f1", "co-short"); existing != nil {
		t.Errorf("rejected order must not be booked: %+v", existing)
	}
}

func TestSimulator_BorrowGateRejects_DefaultReason(t *testing.T) {
	gate := &fakeBorrowGate{verdict: BorrowVerdict{Rejected: true}}
	sim := newBorrowGatedSimulator(t, gate)
	_, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if err == nil || !errors.Is(err, ErrBorrowRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestSimulator_BorrowGateAllowsWithWarnings_AttachesToOrder(t *testing.T) {
	gate := &fakeBorrowGate{
		verdict: BorrowVerdict{
			Warnings: []string{"borrow: hard-to-borrow at 30%/yr"},
		},
	}
	sim := newBorrowGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 1 {
		t.Errorf("warnings = %+v", got.Warnings)
	}
}

func TestSimulator_NoBorrowGate_BehavesLikeBefore(t *testing.T) {
	sim := newBorrowGatedSimulator(t, nil)
	got, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("expected order")
	}
}

func TestSimulator_GatePriority_StatusBeatsLockupBeatsBorrow(t *testing.T) {
	statusGate := &fakeGate{verdict: MarketStatusVerdict{Rejected: true, RejectReason: "halted: news"}}
	lockupGate := &fakeLockupGate{verdict: LockupVerdict{Rejected: true, RejectReason: "lock-up"}}
	borrowGate := &fakeBorrowGate{verdict: BorrowVerdict{Rejected: true, RejectReason: "borrow"}}
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	sim := NewSimulator(q,
		WithMarketStatusGate(statusGate),
		WithLockupGate(lockupGate),
		WithBorrowGate(borrowGate),
		WithIDGenerator(func() string { return "broker-1" }),
	)
	_, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if err == nil {
		t.Fatal("expected reject")
	}
	if !errors.Is(err, ErrMarketStatusRejected) {
		t.Errorf("err = %v, want ErrMarketStatusRejected first", err)
	}
	if lockupGate.calls != 0 {
		t.Errorf("lock-up gate must not be called when status rejects")
	}
	if borrowGate.calls != 0 {
		t.Errorf("borrow gate must not be called when status rejects")
	}
}

func TestSimulator_GatePriority_LockupBeatsBorrow(t *testing.T) {
	lockupGate := &fakeLockupGate{verdict: LockupVerdict{Rejected: true, RejectReason: "locked"}}
	borrowGate := &fakeBorrowGate{verdict: BorrowVerdict{Rejected: true, RejectReason: "borrow"}}
	q := func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	sim := NewSimulator(q,
		WithLockupGate(lockupGate),
		WithBorrowGate(borrowGate),
		WithIDGenerator(func() string { return "broker-1" }),
	)
	_, err := sim.PlaceOrder(context.Background(), newShortSellRequest())
	if !errors.Is(err, ErrLockupRejected) {
		t.Errorf("err = %v, want ErrLockupRejected", err)
	}
	if borrowGate.calls != 0 {
		t.Errorf("borrow gate must not be called when lock-up rejects")
	}
}
