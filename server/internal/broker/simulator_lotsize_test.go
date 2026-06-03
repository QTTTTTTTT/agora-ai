package broker

import (
	"context"
	"errors"
	"testing"

	"github.com/fundai/server/internal/matching"
)

// fakeLotSizeGate is a deterministic stub for the lot-size gate.
type fakeLotSizeGate struct {
	verdict LotSizeVerdict
	calls   int
	last    LotSizeProbe
}

func (g *fakeLotSizeGate) CheckOrder(_ context.Context, probe LotSizeProbe) LotSizeVerdict {
	g.calls++
	g.last = probe
	return g.verdict
}

func newLotSizeSimulator(t *testing.T, gate LotSizeGate) *Simulator {
	t.Helper()
	q := func(ctx context.Context, key, sym, mkt string) (matching.Quote, error) {
		return matching.Quote{Last: 500, Bid: 499.5, Ask: 500.5}, nil
	}
	return NewSimulator(q,
		WithLotSizeGate(gate),
		WithIDGenerator(func() string { return "broker-lot-1" }),
	)
}

// 301308 buy 1 share — the ChiNext minimum is 100. Production
// regression for the 2026-06-02 fill that slipped past the upstream
// normaliser because no broker-side gate existed.
func TestSimulator_LotSizeGate_Rejects_301308_1Share_Regression(t *testing.T) {
	gate := &fakeLotSizeGate{verdict: LotSizeVerdict{
		Rejected:     true,
		RejectReason: "lot-size: 301308 buy qty=1 below a_share minimum 100",
		SuggestedQty: 0,
	}}
	sim := newLotSizeSimulator(t, gate)

	req := newPlaceReq()
	req.Symbol = "301308"
	req.InstrumentKey = "SZSE:301308"
	req.Market = "a_share"
	req.AssetClass = "equity"
	req.Quantity = 1
	req.Side = SideBuy

	got, err := sim.PlaceOrder(context.Background(), req)
	if err == nil {
		t.Fatalf("expected reject error; got order=%+v", got)
	}
	if !errors.Is(err, ErrLotSizeRejected) {
		t.Errorf("err = %v, want wraps ErrLotSizeRejected", err)
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
	// Rejected order must NOT be booked.
	if existing, _ := sim.GetOrderByClientID(context.Background(), req.FundID, req.ClientOrderID); existing != nil {
		t.Errorf("rejected order must not be persisted: %+v", existing)
	}
}

func TestSimulator_LotSizeGate_AllowsValidOrder(t *testing.T) {
	gate := &fakeLotSizeGate{verdict: LotSizeVerdict{}} // allow
	sim := newLotSizeSimulator(t, gate)

	req := newPlaceReq()
	req.Symbol = "600519"
	req.Market = "a_share"
	req.Quantity = 100

	got, err := sim.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("order should be booked")
	}
	if gate.calls != 1 {
		t.Errorf("gate calls = %d, want 1", gate.calls)
	}
}

func TestSimulator_LotSizeGate_RejectWithSuggestion_IncludesSuggestionInError(t *testing.T) {
	gate := &fakeLotSizeGate{verdict: LotSizeVerdict{
		Rejected:     true,
		RejectReason: "lot-size: 688195 partial sell would leave odd-lot residual",
		SuggestedQty: 404,
	}}
	sim := newLotSizeSimulator(t, gate)

	req := newPlaceReq()
	req.Symbol = "688195"
	req.Side = SideSell
	req.Quantity = 200

	_, err := sim.PlaceOrder(context.Background(), req)
	if err == nil || !errors.Is(err, ErrLotSizeRejected) {
		t.Fatalf("err = %v, want wraps ErrLotSizeRejected", err)
	}
	if got := err.Error(); !contains(got, "suggested qty=404") {
		t.Errorf("error %q should mention suggested qty 404", got)
	}
}

func TestSimulator_LotSizeGate_WarningsAttachedToOrder(t *testing.T) {
	gate := &fakeLotSizeGate{verdict: LotSizeVerdict{
		Warnings: []string{"lot-size: STAR full-position sell leaves no residual"},
	}}
	sim := newLotSizeSimulator(t, gate)

	req := newPlaceReq()
	req.Side = SideSell
	req.Quantity = 100

	got, err := sim.PlaceOrder(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.Warnings) == 0 || got.Warnings[0] != "lot-size: STAR full-position sell leaves no residual" {
		t.Errorf("warning not propagated; got %+v", got.Warnings)
	}
}

func TestSimulator_LotSizeGate_RunsAfterPriceCollar(t *testing.T) {
	// Verify the gate ordering: price-collar must reject FIRST so
	// its reason wins over a lot-size reject when both fire.
	lotGate := &fakeLotSizeGate{verdict: LotSizeVerdict{Rejected: true, RejectReason: "lot-size fail"}}
	collarGate := &fakePriceCollarGate{verdict: PriceCollarVerdict{Rejected: true, RejectReason: "collar fail"}}

	q := func(ctx context.Context, key, sym, mkt string) (matching.Quote, error) {
		return matching.Quote{Last: 500}, nil
	}
	sim := NewSimulator(q,
		WithPriceCollarGate(collarGate),
		WithLotSizeGate(lotGate),
		WithIDGenerator(func() string { return "ord-1" }),
	)

	req := newPlaceReq()
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 1000

	_, err := sim.PlaceOrder(context.Background(), req)
	if err == nil {
		t.Fatal("expected reject")
	}
	if !errors.Is(err, ErrPriceCollarRejected) {
		t.Errorf("err = %v, want ErrPriceCollarRejected (collar must fire first)", err)
	}
	if lotGate.calls != 0 {
		t.Errorf("lot-size gate should NOT be consulted after collar reject; calls=%d", lotGate.calls)
	}
}

func TestSimulator_NilLotSizeGate_NoOp(t *testing.T) {
	sim := newLotSizeSimulator(t, nil)
	_, err := sim.PlaceOrder(context.Background(), newPlaceReq())
	if err != nil {
		t.Fatalf("nil gate should not reject: %v", err)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
