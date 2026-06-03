package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/matching"
)

// fakePriceCollarGate is a deterministic stub.
type fakePriceCollarGate struct {
	verdict PriceCollarVerdict
	calls   int
	last    PriceCollarProbe
}

func (g *fakePriceCollarGate) CheckOrder(_ context.Context, probe PriceCollarProbe) PriceCollarVerdict {
	g.calls++
	g.last = probe
	return g.verdict
}

func newCollarSimulator(t *testing.T, gate PriceCollarGate) *Simulator {
	t.Helper()
	q := func(ctx context.Context, key, sym, mkt string) (matching.Quote, error) {
		return matching.Quote{Last: 500, Bid: 499.5, Ask: 500.5}, nil
	}
	return NewSimulator(q,
		WithPriceCollarGate(gate),
		WithIDGenerator(func() string { return "broker-collar-1" }),
	)
}

func newCollarLimitReq() PlaceOrderRequest {
	r := newPlaceReq()
	r.OrderType = OrderTypeLimit
	r.LimitPrice = 96226.4188 // the 2026-06-02 301308 regression price
	r.Symbol = "301308"
	r.InstrumentKey = "SZSE:301308"
	r.Market = "a_share"
	r.AssetClass = "equity"
	return r
}

func TestSimulator_PriceCollarGate_Rejects_96226Regression(t *testing.T) {
	// Lock in the production-grade defence: a 96,226 CNY/share limit
	// price on 301308 (true mid ~500) MUST be rejected before it
	// reaches the matcher. This is the exact bug from 2026-06-02.
	gate := &fakePriceCollarGate{verdict: PriceCollarVerdict{
		Rejected:       true,
		RejectReason:   "limit 96226.42 vs reference 500.00 = 19145% off, cap 21%",
		ReferencePrice: 500,
		ToleranceBps:   2100,
	}}
	sim := newCollarSimulator(t, gate)

	got, err := sim.PlaceOrder(context.Background(), newCollarLimitReq())
	if err == nil {
		t.Fatalf("expected reject error; got order=%+v", got)
	}
	if !errors.Is(err, ErrPriceCollarRejected) {
		t.Errorf("err = %v, want wraps ErrPriceCollarRejected", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
	// Rejected order must NOT be booked.
	if existing, _ := sim.GetOrderByClientID(context.Background(), "f1", "co-1"); existing != nil {
		t.Errorf("rejected order must not be persisted: %+v", existing)
	}
}

func TestSimulator_PriceCollarGate_Rejects_DefaultReason(t *testing.T) {
	gate := &fakePriceCollarGate{verdict: PriceCollarVerdict{Rejected: true}}
	sim := newCollarSimulator(t, gate)

	_, err := sim.PlaceOrder(context.Background(), newCollarLimitReq())
	if err == nil || !errors.Is(err, ErrPriceCollarRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestSimulator_PriceCollarGate_Allow_ProceedsToMatcher(t *testing.T) {
	gate := &fakePriceCollarGate{verdict: PriceCollarVerdict{}} // allow
	sim := newCollarSimulator(t, gate)

	req := newCollarLimitReq()
	req.LimitPrice = 525 // 5% off 500 — well inside the 21% wide-board collar
	got, err := sim.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got == nil {
		t.Fatal("expected booked order")
	}
}

func TestSimulator_PriceCollarGate_WarningsAttachedToOrder(t *testing.T) {
	gate := &fakePriceCollarGate{verdict: PriceCollarVerdict{
		Warnings: []string{"no usable reference quote; collar check skipped"},
	}}
	sim := newCollarSimulator(t, gate)

	got, err := sim.PlaceOrder(context.Background(), newCollarLimitReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 1 || got.Warnings[0] != "no usable reference quote; collar check skipped" {
		t.Errorf("warnings = %+v", got.Warnings)
	}
}

func TestSimulator_PriceCollarGate_NotCalledForMarketOrders(t *testing.T) {
	// Market orders carry IntendedPrice=0 to the probe; the engine
	// must short-circuit. We test that contract here at the
	// simulator boundary: the gate IS still called (the simulator
	// doesn't know the engine's logic), but probe.IntendedPrice
	// must be 0 so the gate's engine can route to allow.
	gate := &fakePriceCollarGate{}
	sim := newCollarSimulator(t, gate)

	req := newCollarLimitReq()
	req.OrderType = OrderTypeMarket
	req.LimitPrice = 0
	req.StopPrice = 0
	_, err := sim.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
	if gate.last.IntendedPrice != 0 {
		t.Errorf("market order must carry IntendedPrice=0 to the gate, got %v", gate.last.IntendedPrice)
	}
}

func TestSimulator_PriceCollarGate_StopPriceUsedWhenLimitMissing(t *testing.T) {
	// Pure stop orders carry only StopPrice (validatePlaceOrder
	// rejects stop-limit without LimitPrice). The collar gate
	// should fall back to StopPrice as the "intended" price for
	// the deviation check.
	gate := &fakePriceCollarGate{}
	sim := newCollarSimulator(t, gate)
	req := newCollarLimitReq()
	req.OrderType = OrderTypeStop
	req.LimitPrice = 0
	req.StopPrice = 480
	_, err := sim.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if gate.last.IntendedPrice != 480 {
		t.Errorf("probe.IntendedPrice = %v, want 480 (from StopPrice)", gate.last.IntendedPrice)
	}
}

func TestSimulator_NilPriceCollarGate_NoOp(t *testing.T) {
	sim := newCollarSimulator(t, nil)
	got, err := sim.PlaceOrder(context.Background(), newCollarLimitReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("nil gate must not attach warnings: %+v", got.Warnings)
	}
}
