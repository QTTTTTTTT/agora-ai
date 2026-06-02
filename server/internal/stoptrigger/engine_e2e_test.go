package stoptrigger_test

import (
	"context"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/stoptrigger"
)

// silentEngine builds an Engine wired to a real broker.Simulator, with
// quote feed initially returning the supplied seed.
func newSilentEngine(t *testing.T, seedLast float64) (*stoptrigger.Engine, *broker.Simulator, *atomic.Pointer[matching.Quote]) {
	t.Helper()

	currentQuote := &atomic.Pointer[matching.Quote]{}
	currentQuote.Store(&matching.Quote{Last: seedLast, Bid: seedLast - 0.05, Ask: seedLast + 0.05})

	quoteFn := func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		q := currentQuote.Load()
		if q == nil {
			return matching.Quote{}, nil
		}
		return *q, nil
	}

	var idCount atomic.Uint64
	sim := broker.NewSimulator(quoteFn,
		broker.WithIDGenerator(func() string {
			idCount.Add(1)
			return "broker-id-" + itoa(idCount.Load())
		}),
		broker.WithClock(func() time.Time { return time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC) }),
	)
	eng := stoptrigger.New(sim,
		stoptrigger.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
	return eng, sim, currentQuote
}

func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// ---------------------------------------------------------------------------
// End-to-end: place sell stop, drive quote across trigger, observe child fill.
// ---------------------------------------------------------------------------

func TestEndToEnd_SellStopFiresAndChildFills(t *testing.T) {
	ctx := context.Background()
	eng, sim, q := newSilentEngine(t, 100)

	parent, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "stop-loss",
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          broker.SideSell,
		OrderType:     broker.OrderTypeStop,
		Quantity:      10,
		StopPrice:     95,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if parent.State != broker.OrderStatePending {
		t.Fatalf("parent state = %s, want pending", parent.State)
	}

	// Tick that does NOT breach trigger.
	if _, err := eng.OnQuote(ctx, stoptrigger.QuoteTick{
		InstrumentKey: "us:AAPL",
		Quote:         matching.Quote{Last: 96, Bid: 95.95, Ask: 96.05},
	}); err != nil {
		t.Fatalf("OnQuote: %v", err)
	}
	pending := sim.PendingStopsForInstrument("us:AAPL", "")
	if len(pending) != 1 {
		t.Fatalf("expected stop still pending, got %d", len(pending))
	}

	// Drop quote to 94 — sell-stop trigger.
	q.Store(&matching.Quote{Last: 94, Bid: 93.95, Ask: 94.05})
	res, err := eng.OnQuote(ctx, stoptrigger.QuoteTick{
		InstrumentKey: "us:AAPL",
		Quote:         matching.Quote{Last: 94, Bid: 93.95, Ask: 94.05},
	})
	if err != nil {
		t.Fatalf("OnQuote: %v", err)
	}
	if res.Fired != 1 {
		t.Errorf("Fired=%d, want 1; errors=%v", res.Fired, res.Errors)
	}

	// Parent must be triggered, child must be filled.
	parentNow, err := sim.GetOrder(ctx, "fund-A", parent.BrokerOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if parentNow.State != broker.OrderStateTriggered {
		t.Errorf("parent state = %s, want triggered", parentNow.State)
	}
	if parentNow.TriggeredChildOrderID == "" {
		t.Fatal("parent missing TriggeredChildOrderID")
	}
	child, err := sim.GetOrder(ctx, "fund-A", parentNow.TriggeredChildOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if child.State != broker.OrderStateFilled {
		t.Errorf("child state = %s, want filled", child.State)
	}
	if child.FilledQuantity != 10 {
		t.Errorf("child filledQty = %v, want 10", child.FilledQuantity)
	}

	// Pending list must be empty post-fire.
	if pending := sim.PendingStopsForInstrument("us:AAPL", ""); len(pending) != 0 {
		t.Errorf("post-fire pending = %d, want 0", len(pending))
	}
}

// ---------------------------------------------------------------------------
// End-to-end: trailing sell-stop ratchets across rising market then fires.
// ---------------------------------------------------------------------------

func TestEndToEnd_TrailingSellStop_RatchetsAndFires(t *testing.T) {
	ctx := context.Background()
	eng, sim, q := newSilentEngine(t, 100)

	parent, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "trail",
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          broker.SideSell,
		OrderType:     broker.OrderTypeTrailingStop,
		Quantity:      10,
		TrailAmount:   5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parent.TrailingHighWater != 100 || parent.CurrentStopPrice != 95 {
		t.Fatalf("seed wrong: HWM=%v stop=%v", parent.TrailingHighWater, parent.CurrentStopPrice)
	}

	// Rising sequence: 100 → 110 → 115. Stop should ratchet to 105
	// then 110.
	for _, last := range []float64{110, 115} {
		q.Store(&matching.Quote{Last: last, Bid: last - 0.05, Ask: last + 0.05})
		if _, err := eng.OnQuote(ctx, stoptrigger.QuoteTick{
			InstrumentKey: "us:AAPL",
			Quote:         matching.Quote{Last: last, Bid: last - 0.05, Ask: last + 0.05},
		}); err != nil {
			t.Fatalf("OnQuote(%v): %v", last, err)
		}
	}
	mid, err := sim.GetOrder(ctx, "fund-A", parent.BrokerOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.TrailingHighWater != 115 || mid.CurrentStopPrice != 110 {
		t.Errorf("after ratchet: HWM=%v stop=%v, want 115/110", mid.TrailingHighWater, mid.CurrentStopPrice)
	}

	// Reversal: 109 — should fire (109 <= 110).
	q.Store(&matching.Quote{Last: 109, Bid: 108.95, Ask: 109.05})
	res, _ := eng.OnQuote(ctx, stoptrigger.QuoteTick{
		InstrumentKey: "us:AAPL",
		Quote:         matching.Quote{Last: 109, Bid: 108.95, Ask: 109.05},
	})
	if res.Fired != 1 {
		t.Errorf("Fired=%d, want 1", res.Fired)
	}
}

// ---------------------------------------------------------------------------
// End-to-end: idempotent under double-tick (re-firing must be safe).
// ---------------------------------------------------------------------------

func TestEndToEnd_DoubleTick_IsIdempotent(t *testing.T) {
	ctx := context.Background()
	eng, sim, q := newSilentEngine(t, 100)

	if _, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID: "fund-A", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 5, StopPrice: 95,
	}); err != nil {
		t.Fatal(err)
	}

	q.Store(&matching.Quote{Last: 90, Bid: 89.95, Ask: 90.05})

	// Tick A and tick B both at 90 — second pass must not re-fire.
	res1, _ := eng.OnQuote(ctx, stoptrigger.QuoteTick{InstrumentKey: "us:AAPL", Quote: matching.Quote{Last: 90, Bid: 89.95, Ask: 90.05}})
	res2, _ := eng.OnQuote(ctx, stoptrigger.QuoteTick{InstrumentKey: "us:AAPL", Quote: matching.Quote{Last: 90, Bid: 89.95, Ask: 90.05}})

	if res1.Fired != 1 {
		t.Errorf("tick1 Fired=%d, want 1", res1.Fired)
	}
	if res2.Inspected != 0 {
		t.Errorf("tick2 should see empty pending list, got Inspected=%d", res2.Inspected)
	}
	if res2.Fired != 0 {
		t.Errorf("tick2 Fired=%d, want 0", res2.Fired)
	}
}
