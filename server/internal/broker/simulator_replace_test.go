package broker

import (
	"context"
	"errors"
	"testing"
)

// ---------------------------------------------------------------------------
// ReplaceOrder validation
// ---------------------------------------------------------------------------

func TestReplaceOrder_RejectsMissingIdentifiers(t *testing.T) {
	s := newTestSimulator(t, nil)
	q := 5.0
	_, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{NewQuantity: &q})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest", err)
	}

	_, err = s.ReplaceOrder(context.Background(), ReplaceOrderRequest{FundID: "f1"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err on missing broker_order_id = %v, want ErrInvalidRequest", err)
	}
}

func TestReplaceOrder_RejectsNoChange(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "id", 10, 50) // limit so it rests
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ReplaceOrder(context.Background(), ReplaceOrderRequest{FundID: "f1", BrokerOrderID: o.BrokerOrderID})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest for no-op replace", err)
	}
}

func TestReplaceOrder_NotFound(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	q := 5.0
	_, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: "ghost", NewQuantity: &q,
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestReplaceOrder_RejectsTerminalOrder(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	// Market order fills immediately and goes terminal.
	o, err := s.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "m1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideBuy, OrderType: OrderTypeMarket, Quantity: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.State != OrderStateFilled {
		t.Fatalf("expected filled, got %s", o.State)
	}
	q := 5.0
	_, err = s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewQuantity: &q,
	})
	if !errors.Is(err, ErrOrderTerminal) {
		t.Errorf("err = %v, want ErrOrderTerminal", err)
	}
}

// ---------------------------------------------------------------------------
// ReplaceOrder field-by-field validation
// ---------------------------------------------------------------------------

func TestReplaceOrder_ValidationErrors(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "id", 10, 50)
	if err != nil {
		t.Fatal(err)
	}

	zero := 0.0
	negative := -1.0
	below := 5.0 // below already-placed qty? we haven't filled, so just test < 0

	cases := []struct {
		name string
		req  ReplaceOrderRequest
	}{
		{"zero quantity", ReplaceOrderRequest{NewQuantity: &zero}},
		{"negative quantity", ReplaceOrderRequest{NewQuantity: &negative}},
		{"zero limit", ReplaceOrderRequest{NewLimitPrice: &zero}},
		{"negative limit", ReplaceOrderRequest{NewLimitPrice: &negative}},
		{"zero stop", ReplaceOrderRequest{NewStopPrice: &zero}},
		{"zero trail amount", ReplaceOrderRequest{NewTrailAmount: &zero}},
		{"zero trail percent", ReplaceOrderRequest{NewTrailPercent: &zero}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.req.FundID = "f1"
			c.req.BrokerOrderID = o.BrokerOrderID
			_, err := s.ReplaceOrder(context.Background(), c.req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Errorf("err = %v, want ErrInvalidRequest", err)
			}
		})
	}
	_ = below
}

func TestReplaceOrder_DisplayQtyOnlyValidOnIceberg(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "id", 10, 50)
	if err != nil {
		t.Fatal(err)
	}
	dq := 3.0
	_, err = s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewDisplayQty: &dq,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest (limit order doesn't carry display qty)", err)
	}
}

// ---------------------------------------------------------------------------
// ReplaceOrder happy path
// ---------------------------------------------------------------------------

func TestReplaceOrder_UpdatesLimitOnWorkingOrder(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "id", 10, 50) // resting limit at 50, market at 100
	if err != nil {
		t.Fatal(err)
	}
	// Bump limit to 105 — should now be marketable and fill.
	newLimit := 105.0
	updated, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewLimitPrice: &newLimit,
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if updated.State != OrderStateFilled {
		t.Errorf("state = %s, want filled after marketable replace", updated.State)
	}
	if updated.Request.LimitPrice != 105 {
		t.Errorf("LimitPrice = %v, want 105", updated.Request.LimitPrice)
	}
}

func TestReplaceOrder_UpdatesStopOnPendingStop(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := s.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop-1", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeStop, Quantity: 10, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.CurrentStopPrice != 95 {
		t.Fatalf("seed stop wrong: %v", o.CurrentStopPrice)
	}
	newStop := 90.0
	updated, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewStopPrice: &newStop,
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if updated.State != OrderStatePending {
		t.Errorf("state = %s, want pending", updated.State)
	}
	if updated.CurrentStopPrice != 90 {
		t.Errorf("CurrentStopPrice = %v, want 90", updated.CurrentStopPrice)
	}
	if updated.Request.StopPrice != 90 {
		t.Errorf("Request.StopPrice = %v, want 90", updated.Request.StopPrice)
	}
}

func TestReplaceOrder_TrailingStop_NewTrailRecomputesFromHWM(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := s.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "trail", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: SideSell, OrderType: OrderTypeTrailingStop, Quantity: 10, TrailAmount: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Seed: HWM=100 (from quote), stop=95.
	if o.TrailingHighWater != 100 || o.CurrentStopPrice != 95 {
		t.Fatalf("seed wrong")
	}

	// Bump trail to 10 — stop should drop to 90.
	newTrail := 10.0
	updated, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewTrailAmount: &newTrail,
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if updated.Request.TrailAmount != 10 {
		t.Errorf("TrailAmount = %v, want 10", updated.Request.TrailAmount)
	}
	if updated.CurrentStopPrice != 90 {
		t.Errorf("CurrentStopPrice = %v, want 90 (HWM 100 - trail 10)", updated.CurrentStopPrice)
	}
}

func TestReplaceOrder_PartialFilled_RejectsQtyBelowFilled(t *testing.T) {
	// Build a fake partially-filled order by setting FilledQuantity
	// directly — easier than driving a partial through the
	// matcher.
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "id", 10, 50)
	if err != nil {
		t.Fatal(err)
	}
	// Inject a partial fill state.
	s.mu.Lock()
	s.orders[o.BrokerOrderID].FilledQuantity = 3
	s.orders[o.BrokerOrderID].State = OrderStatePartial
	s.mu.Unlock()

	low := 2.0
	_, err = s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", BrokerOrderID: o.BrokerOrderID, NewQuantity: &low,
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("err = %v, want ErrInvalidRequest (qty below filled)", err)
	}
}

func TestReplaceOrder_ByClientOrderID(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.9, 100.1))
	o, err := placeWorkingLimit(t, s, "f1", "client-xyz", 10, 50)
	if err != nil {
		t.Fatal(err)
	}
	newQty := 20.0
	updated, err := s.ReplaceOrder(context.Background(), ReplaceOrderRequest{
		FundID: "f1", ClientOrderID: "client-xyz", NewQuantity: &newQty,
	})
	if err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if updated.BrokerOrderID != o.BrokerOrderID {
		t.Errorf("resolved wrong broker id: got %s want %s", updated.BrokerOrderID, o.BrokerOrderID)
	}
	if updated.Request.Quantity != 20 {
		t.Errorf("Quantity = %v, want 20", updated.Request.Quantity)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// placeWorkingLimit places a buy LIMIT well below the market mid so
// the simulator routes it to OrderStateWorking (DAY/GTC) rather than
// filling immediately. Useful as the canonical "open order" fixture.
func placeWorkingLimit(t *testing.T, s *Simulator, fundID, clientID string, qty, limit float64) (*Order, error) {
	t.Helper()
	return s.PlaceOrder(context.Background(), PlaceOrderRequest{
		FundID:        fundID,
		ClientOrderID: clientID,
		Symbol:        "AAPL",
		InstrumentKey: "us:AAPL",
		Side:          SideBuy,
		OrderType:     OrderTypeLimit,
		Quantity:      qty,
		LimitPrice:    limit,
		TimeInForce:   TIFDay,
	})
}

func TestReplaceRequestHasChanges(t *testing.T) {
	if replaceRequestHasChanges(ReplaceOrderRequest{}) {
		t.Errorf("empty req reported as having changes")
	}
	q := 1.0
	if !replaceRequestHasChanges(ReplaceOrderRequest{NewQuantity: &q}) {
		t.Errorf("qty change not detected")
	}
	if !replaceRequestHasChanges(ReplaceOrderRequest{NewLimitPrice: &q}) {
		t.Errorf("limit change not detected")
	}
}
