package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/matching"
)

// fakeGate is a deterministic MarketStatusGate stub.
type fakeGate struct {
	verdict MarketStatusVerdict
	calls   int
	last    MarketStatusProbe
}

func (g *fakeGate) CheckOrder(ctx context.Context, probe MarketStatusProbe) MarketStatusVerdict {
	g.calls++
	g.last = probe
	return g.verdict
}

func newGatedSimulator(t *testing.T, gate MarketStatusGate) *Simulator {
	t.Helper()
	q := func(ctx context.Context, key, sym, mkt string) (matching.Quote, error) {
		return matching.Quote{Last: 200, Bid: 199.9, Ask: 200.1}, nil
	}
	return NewSimulator(q, WithMarketStatusGate(gate), WithIDGenerator(func() string { return "broker-1" }))
}

func newPlaceReq() PlaceOrderRequest {
	return PlaceOrderRequest{
		FundID:        "f1",
		ClientOrderID: "co-1",
		InstrumentKey: "AAPL.US",
		Symbol:        "AAPL",
		Market:        "US",
		AssetClass:    "equity",
		Side:          SideBuy,
		Quantity:      10,
		OrderType:     OrderTypeMarket,
		TimeInForce:   TIFDay,
	}
}

func TestSimulator_GateRejects_BlocksOrder(t *testing.T) {
	gate := &fakeGate{verdict: MarketStatusVerdict{Rejected: true, RejectReason: "halted: news pending"}}
	sim := newGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newPlaceReq())
	if err == nil {
		t.Fatalf("expected reject err; got order=%+v", got)
	}
	if !errors.Is(err, ErrMarketStatusRejected) {
		t.Errorf("err = %v, want ErrMarketStatusRejected", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
	// Order map should NOT contain the rejected client order id.
	if existing, _ := sim.GetOrderByClientID(context.Background(), "f1", "co-1"); existing != nil {
		t.Errorf("rejected order should not be booked: %+v", existing)
	}
}

func TestSimulator_GateRejects_DefaultReason(t *testing.T) {
	gate := &fakeGate{verdict: MarketStatusVerdict{Rejected: true}}
	sim := newGatedSimulator(t, gate)
	_, err := sim.PlaceOrder(context.Background(), newPlaceReq())
	if err == nil || !errors.Is(err, ErrMarketStatusRejected) {
		t.Fatalf("err = %v", err)
	}
}

func TestSimulator_GateAllowsWithWarnings_AttachesToOrder(t *testing.T) {
	gate := &fakeGate{verdict: MarketStatusVerdict{
		Warnings: []string{"stale quote: 90s old", "half-day session"},
	}}
	sim := newGatedSimulator(t, gate)
	got, err := sim.PlaceOrder(context.Background(), newPlaceReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 2 {
		t.Errorf("warnings = %+v, want 2", got.Warnings)
	}
	if got.Warnings[0] != "stale quote: 90s old" {
		t.Errorf("warnings[0] = %s", got.Warnings[0])
	}
}

func TestSimulator_NilGate_NoOp(t *testing.T) {
	sim := newGatedSimulator(t, nil)
	got, err := sim.PlaceOrder(context.Background(), newPlaceReq())
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got.Warnings) != 0 {
		t.Errorf("nil gate must not attach warnings: %+v", got.Warnings)
	}
}

func TestSimulator_GateProbe_LimitPriceCarried(t *testing.T) {
	gate := &fakeGate{}
	sim := newGatedSimulator(t, gate)
	req := newPlaceReq()
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 198.5
	_, _ = sim.PlaceOrder(context.Background(), req)
	if gate.last.IntendedPrice != 198.5 {
		t.Errorf("probe.IntendedPrice = %v, want 198.5", gate.last.IntendedPrice)
	}
	if gate.last.Side != string(SideBuy) {
		t.Errorf("probe.Side = %s", gate.last.Side)
	}
}

func TestSimulator_GateProbe_StopPriceCarriedWhenLimitMissing(t *testing.T) {
	gate := &fakeGate{}
	sim := newGatedSimulator(t, gate)
	req := newPlaceReq()
	req.OrderType = OrderTypeStop
	req.StopPrice = 195
	_, _ = sim.PlaceOrder(context.Background(), req)
	if gate.last.IntendedPrice != 195 {
		t.Errorf("probe.IntendedPrice = %v, want 195 (from StopPrice)", gate.last.IntendedPrice)
	}
}
