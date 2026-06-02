package broker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/fundai/server/internal/matching"
)

// ---------------------------------------------------------------------------
// PlaceOrder defers stop-typed orders into Pending without touching the
// matching engine.
// ---------------------------------------------------------------------------

func TestPlaceOrder_StopOrder_RestsPending(t *testing.T) {
	// matchingShouldNotBeCalled fails the test if the matching path
	// is hit at all. Stop orders MUST NOT touch the matcher on
	// PlaceOrder; only the stop-trigger engine fires them later.
	var matched atomic.Bool
	quote := func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		matched.Store(true)
		return matching.Quote{Last: 100, Bid: 99.99, Ask: 100.01}, nil
	}
	sim := newTestSimulator(t, quote)

	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "stop-1",
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          SideSell,
		OrderType:     OrderTypeStop,
		Quantity:      10,
		StopPrice:     95,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if o.State != OrderStatePending {
		t.Errorf("state = %s, want pending", o.State)
	}
	if o.CurrentStopPrice != 95 {
		t.Errorf("CurrentStopPrice = %v, want 95", o.CurrentStopPrice)
	}
	if matched.Load() {
		t.Errorf("PlaceOrder called the matching engine for a stop order — must defer")
	}

	// And the order must show up in PendingStopsForInstrument.
	pendings := sim.PendingStopsForInstrument("us:AAPL", "")
	if len(pendings) != 1 || pendings[0].BrokerOrderID != o.BrokerOrderID {
		t.Errorf("PendingStopsForInstrument = %#v, want [%s]", pendings, o.BrokerOrderID)
	}
}

func TestPlaceOrder_TrailingStop_SeedsHighWaterFromQuote(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(120, 119.9, 120.1))

	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "trail-1",
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          SideSell, // protects a long
		OrderType:     OrderTypeTrailingStop,
		Quantity:      10,
		TrailAmount:   5,
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if o.State != OrderStatePending {
		t.Errorf("state = %s, want pending", o.State)
	}
	// HWM should seed at quote.Last (120), CurrentStopPrice =
	// 120 - 5 = 115.
	if o.TrailingHighWater != 120 {
		t.Errorf("HighWater = %v, want 120", o.TrailingHighWater)
	}
	if o.CurrentStopPrice != 115 {
		t.Errorf("CurrentStopPrice = %v, want 115", o.CurrentStopPrice)
	}
}

func TestPlaceOrder_TrailingStop_SeedsLowWater_BuySide(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(50, 49.9, 50.1))

	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "trail-buy",
		Symbol:        "TSLA",
		InstrumentKey: "us:TSLA",
		Side:          SideBuy, // protects a short
		OrderType:     OrderTypeTrailingStop,
		Quantity:      5,
		TrailPercent:  0.05, // 5%
	})
	if err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}
	if o.TrailingLowWater != 50 {
		t.Errorf("LowWater = %v, want 50", o.TrailingLowWater)
	}
	// 50 * (1 + 0.05) = 52.5
	if o.CurrentStopPrice != 52.5 {
		t.Errorf("CurrentStopPrice = %v, want 52.5", o.CurrentStopPrice)
	}
}

func TestPlaceOrder_TrailingStop_SurvivesQuoteOutage(t *testing.T) {
	sim := newTestSimulator(t, erroringQuote(errors.New("feed down")))

	// Even with the quote feed down, accepting a trailing stop
	// MUST NOT fail — the engine seeds on first OnQuote.
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "trail-2",
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          SideSell,
		OrderType:     OrderTypeTrailingStop,
		Quantity:      10,
		TrailAmount:   5,
	})
	if err != nil {
		t.Fatalf("PlaceOrder must accept trailing stops despite quote outage: %v", err)
	}
	if o.State != OrderStatePending {
		t.Errorf("state = %s, want pending", o.State)
	}
	if o.TrailingHighWater != 0 || o.CurrentStopPrice != 0 {
		t.Errorf("expected unset HWM and stop price under outage, got HWM=%v stop=%v",
			o.TrailingHighWater, o.CurrentStopPrice)
	}
}

// ---------------------------------------------------------------------------
// PendingStopsForInstrument
// ---------------------------------------------------------------------------

func TestPendingStopsForInstrument_FiltersByInstrumentAndState(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	ctx := context.Background()

	// Stop on AAPL.
	stopAAPL, err := sim.PlaceOrder(ctx, PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "s1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeStop, Quantity: 10, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Stop on TSLA — same fund, different instrument.
	if _, err := sim.PlaceOrder(ctx, PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "s2", Symbol: "TSLA", InstrumentKey: "us:TSLA",
		Side: SideSell, OrderType: OrderTypeStop, Quantity: 5, StopPrice: 200,
	}); err != nil {
		t.Fatal(err)
	}

	// Market order on AAPL — must NOT show up.
	if _, err := sim.PlaceOrder(ctx, PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "m1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideBuy, OrderType: OrderTypeMarket, Quantity: 1,
	}); err != nil {
		t.Fatal(err)
	}

	got := sim.PendingStopsForInstrument("us:AAPL", "")
	if len(got) != 1 || got[0].BrokerOrderID != stopAAPL.BrokerOrderID {
		t.Errorf("AAPL pendings = %#v, want only stop AAPL %s", got, stopAAPL.BrokerOrderID)
	}

	// Cancelling the AAPL stop should remove it from the pending
	// snapshot.
	if err := sim.CancelOrder(ctx, CancelOrderRequest{FundID: "f1", BrokerOrderID: stopAAPL.BrokerOrderID}); err != nil {
		t.Fatal(err)
	}
	got = sim.PendingStopsForInstrument("us:AAPL", "")
	if len(got) != 0 {
		t.Errorf("after cancel, pending stops = %d, want 0", len(got))
	}
}

func TestPendingStopsForInstrument_EmptyArgs(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	if got := sim.PendingStopsForInstrument("", ""); got != nil {
		t.Errorf("expected nil for empty inputs, got %#v", got)
	}
}

// ---------------------------------------------------------------------------
// UpdateTrailingHighWater / UpdateTrailingLowWater
// ---------------------------------------------------------------------------

func TestUpdateTrailingHighWater_RatchetsUp(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "t1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeTrailingStop, Quantity: 10, TrailAmount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Initial HWM=100 (seed), stop=95.
	if o.TrailingHighWater != 100 || o.CurrentStopPrice != 95 {
		t.Fatalf("seed wrong: HWM=%v stop=%v", o.TrailingHighWater, o.CurrentStopPrice)
	}

	// last 105 → HWM 105, stop 100.
	updated, err := sim.UpdateTrailingHighWater(o.BrokerOrderID, 105)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.TrailingHighWater != 105 || updated.CurrentStopPrice != 100 {
		t.Errorf("after 105: HWM=%v stop=%v, want 105/100", updated.TrailingHighWater, updated.CurrentStopPrice)
	}

	// last 102 (lower than HWM) → no-op.
	again, err := sim.UpdateTrailingHighWater(o.BrokerOrderID, 102)
	if err != nil {
		t.Fatalf("update no-op: %v", err)
	}
	if again.TrailingHighWater != 105 || again.CurrentStopPrice != 100 {
		t.Errorf("HWM regressed: %v / %v", again.TrailingHighWater, again.CurrentStopPrice)
	}
}

func TestUpdateTrailingHighWater_RejectsWrongSide(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(50, 49.9, 50.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "buy", Symbol: "X", InstrumentKey: "k",
		Side: SideBuy, OrderType: OrderTypeTrailingStop, Quantity: 1, TrailAmount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.UpdateTrailingHighWater(o.BrokerOrderID, 100); !errors.Is(err, ErrStopTriggerNotApplicable) {
		t.Errorf("expected ErrStopTriggerNotApplicable for buy-side high-water, got %v", err)
	}
}

func TestUpdateTrailingLowWater_RatchetsDown(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(50, 49.9, 50.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "buy", Symbol: "X", InstrumentKey: "k",
		Side: SideBuy, OrderType: OrderTypeTrailingStop, Quantity: 1, TrailAmount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed: LWM=50, stop=55.
	if o.TrailingLowWater != 50 || o.CurrentStopPrice != 55 {
		t.Fatalf("seed wrong: LWM=%v stop=%v", o.TrailingLowWater, o.CurrentStopPrice)
	}
	// last 45 → LWM 45, stop 50.
	upd, err := sim.UpdateTrailingLowWater(o.BrokerOrderID, 45)
	if err != nil {
		t.Fatal(err)
	}
	if upd.TrailingLowWater != 45 || upd.CurrentStopPrice != 50 {
		t.Errorf("after 45: LWM=%v stop=%v, want 45/50", upd.TrailingLowWater, upd.CurrentStopPrice)
	}
}

func TestUpdateTrailingHighWater_PostFireRejected(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "t", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeTrailingStop, Quantity: 1, TrailAmount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 95, Bid: 94.9, Ask: 95.1}); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.UpdateTrailingHighWater(o.BrokerOrderID, 200); !errors.Is(err, ErrStopAlreadyFired) {
		t.Errorf("expected ErrStopAlreadyFired post-fire, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// FireStopTrigger
// ---------------------------------------------------------------------------

func TestFireStopTrigger_StopOrder_FillsAsMarketChild(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(95, 94.9, 95.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeStop, Quantity: 10, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}

	child, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 94, Bid: 93.9, Ask: 94.1})
	if err != nil {
		t.Fatalf("FireStopTrigger: %v", err)
	}
	if child.State != OrderStateFilled {
		t.Errorf("child state = %s, want filled", child.State)
	}
	if child.FilledQuantity != 10 {
		t.Errorf("child filledQty = %v, want 10", child.FilledQuantity)
	}
	if child.Request.OrderType != OrderTypeMarket {
		t.Errorf("child orderType = %s, want market", child.Request.OrderType)
	}
	if child.ParentBrokerOrderID != o.BrokerOrderID {
		t.Errorf("child parent = %s, want %s", child.ParentBrokerOrderID, o.BrokerOrderID)
	}

	// Parent must be triggered + linked.
	parent, err := sim.GetOrder(context.Background(), "f1", o.BrokerOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if parent.State != OrderStateTriggered {
		t.Errorf("parent state = %s, want triggered", parent.State)
	}
	if parent.TriggeredChildOrderID != child.BrokerOrderID {
		t.Errorf("parent.TriggeredChildOrderID = %s, want %s", parent.TriggeredChildOrderID, child.BrokerOrderID)
	}
}

func TestFireStopTrigger_StopLimit_FillsAsLimitChild(t *testing.T) {
	// Quote at 95 / 94.9 / 95.1 — a sell-stop-limit at stop=95
	// limit=94 will be marketable (94 <= bid 94.9 false; 94 >= bid
	// 94.9 false; sell limit needs ask >= limit, ask=95.1 >= 94 OK).
	sim := newTestSimulator(t, fixedQuote(95, 94.9, 95.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop-limit", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeStopLimit, Quantity: 10, StopPrice: 95, LimitPrice: 94,
	})
	if err != nil {
		t.Fatal(err)
	}
	child, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 94, Bid: 93.9, Ask: 94.1})
	if err != nil {
		t.Fatalf("FireStopTrigger: %v", err)
	}
	if child.Request.OrderType != OrderTypeLimit {
		t.Errorf("child orderType = %s, want limit", child.Request.OrderType)
	}
	if child.Request.LimitPrice != 94 {
		t.Errorf("child limit = %v, want 94", child.Request.LimitPrice)
	}
}

func TestFireStopTrigger_AlreadyTriggered_ReturnsAlreadyFired(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(95, 94.9, 95.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeStop, Quantity: 1, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 94}); err != nil {
		t.Fatal(err)
	}
	if _, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 93}); !errors.Is(err, ErrStopAlreadyFired) {
		t.Errorf("expected ErrStopAlreadyFired on double fire, got %v", err)
	}
}

func TestFireStopTrigger_RejectsNonStopOrder(t *testing.T) {
	sim := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := sim.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "m1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideBuy, OrderType: OrderTypeMarket, Quantity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sim.FireStopTrigger(context.Background(), o.BrokerOrderID, matching.Quote{Last: 100}); !errors.Is(err, ErrStopTriggerNotApplicable) {
		t.Errorf("expected ErrStopTriggerNotApplicable for market order, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// StopShouldFire predicate
// ---------------------------------------------------------------------------

func TestStopShouldFire(t *testing.T) {
	cases := []struct {
		name string
		side Side
		stop float64
		last float64
		want bool
	}{
		{"sell-stop above last fires", SideSell, 100, 99, true},
		{"sell-stop equal fires", SideSell, 100, 100, true},
		{"sell-stop below last does not fire", SideSell, 100, 101, false},
		{"buy-stop below last fires", SideBuy, 100, 101, true},
		{"buy-stop equal fires", SideBuy, 100, 100, true},
		{"buy-stop above last does not fire", SideBuy, 100, 99, false},
		{"non-positive stop never fires", SideSell, 0, 99, false},
		{"non-positive last never fires", SideSell, 100, 0, false},
		{"unknown side never fires", "long", 100, 99, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := StopShouldFire(c.side, c.stop, c.last)
			if got != c.want {
				t.Errorf("StopShouldFire(%s, %v, %v) = %v, want %v", c.side, c.stop, c.last, got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// computeTrailingStop helpers
// ---------------------------------------------------------------------------

func TestComputeTrailingStopFromHigh(t *testing.T) {
	cases := []struct {
		name string
		hw   float64
		req  PlaceOrderRequest
		want float64
	}{
		{"amount", 100, PlaceOrderRequest{TrailAmount: 5}, 95},
		{"percent", 100, PlaceOrderRequest{TrailPercent: 0.05}, 95},
		{"amount preferred over percent", 100, PlaceOrderRequest{TrailAmount: 5, TrailPercent: 0.10}, 95},
		{"non-positive HWM", 0, PlaceOrderRequest{TrailAmount: 5}, 0},
		{"no trail params", 100, PlaceOrderRequest{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeTrailingStopFromHigh(c.hw, c.req)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestComputeTrailingStopFromLow(t *testing.T) {
	cases := []struct {
		name string
		lw   float64
		req  PlaceOrderRequest
		want float64
	}{
		{"amount", 100, PlaceOrderRequest{TrailAmount: 5}, 105},
		{"percent", 100, PlaceOrderRequest{TrailPercent: 0.05}, 105},
		{"non-positive LWM", 0, PlaceOrderRequest{TrailAmount: 5}, 0},
		{"no trail params", 100, PlaceOrderRequest{}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := computeTrailingStopFromLow(c.lw, c.req)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// IsStopType
// ---------------------------------------------------------------------------

func TestOrderType_IsStopType(t *testing.T) {
	want := map[OrderType]bool{
		OrderTypeMarket:       false,
		OrderTypeLimit:        false,
		OrderTypeStop:         true,
		OrderTypeStopLimit:    true,
		OrderTypeTrailingStop: true,
		OrderTypeMOC:          false,
		OrderTypeMOO:          false,
		OrderTypeIceberg:      false,
	}
	for ot, w := range want {
		if got := ot.IsStopType(); got != w {
			t.Errorf("%s.IsStopType() = %v, want %v", ot, got, w)
		}
	}
}
