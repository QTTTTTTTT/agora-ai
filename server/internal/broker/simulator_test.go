package broker

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/matching"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// fixedQuote returns the same quote for every call. Useful when the test
// only cares about whether the fill happens, not how it's priced.
func fixedQuote(last, bid, ask float64) QuoteFn {
	return func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		return matching.Quote{Last: last, Bid: bid, Ask: ask}, nil
	}
}

// erroringQuote returns the supplied error every time. The Simulator
// must reject orders with an unavailable quote.
func erroringQuote(err error) QuoteFn {
	return func(ctx context.Context, instrumentKey, symbol, market string) (matching.Quote, error) {
		return matching.Quote{}, err
	}
}

func newTestSimulator(t *testing.T, quoteFn QuoteFn, opts ...SimulatorOption) *Simulator {
	t.Helper()
	if quoteFn == nil {
		quoteFn = fixedQuote(100.0, 99.99, 100.01)
	}
	// Deterministic IDs so failures point at exact orders.
	var counter atomic.Uint64
	defaultOpts := []SimulatorOption{
		WithIDGenerator(func() string {
			n := counter.Add(1)
			return "broker-order-" + itoa(n)
		}),
		WithClock(func() time.Time { return time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC) }),
	}
	return NewSimulator(quoteFn, append(defaultOpts, opts...)...)
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

func validBuyMarket() PlaceOrderRequest {
	return PlaceOrderRequest{
		ClientOrderID: "client-1",
		FundID:        "fund-A",
		InstrumentKey: "us-equity:AAPL",
		Symbol:        "AAPL",
		Market:        "us",
		AssetClass:    "equity",
		Side:          SideBuy,
		OrderType:     OrderTypeMarket,
		Quantity:      100,
		TimeInForce:   TIFDay,
	}
}

func validSellLimit(price float64) PlaceOrderRequest {
	return PlaceOrderRequest{
		ClientOrderID: "client-sell-1",
		FundID:        "fund-A",
		InstrumentKey: "us-equity:AAPL",
		Symbol:        "AAPL",
		Market:        "us",
		AssetClass:    "equity",
		Side:          SideSell,
		OrderType:     OrderTypeLimit,
		LimitPrice:    price,
		Quantity:      50,
		TimeInForce:   TIFDay,
	}
}

// ---------------------------------------------------------------------------
// Capabilities
// ---------------------------------------------------------------------------

func TestSimulator_Capabilities(t *testing.T) {
	s := newTestSimulator(t, nil)
	caps := s.Capabilities()

	if caps.Name != "simulator" {
		t.Errorf("Name = %q, want simulator", caps.Name)
	}
	if !caps.HasOrderType(OrderTypeMarket) || !caps.HasOrderType(OrderTypeLimit) {
		t.Errorf("simulator must support market+limit, got %v", caps.OrderTypes)
	}
	// Stop / stop-limit / trailing-stop are P0-3 features and now
	// supported. They rest in OrderStatePending until the
	// stop-trigger engine fires them.
	for _, ot := range []OrderType{OrderTypeStop, OrderTypeStopLimit, OrderTypeTrailingStop} {
		if !caps.HasOrderType(ot) {
			t.Errorf("simulator must support %s post-P0-3, got %v", ot, caps.OrderTypes)
		}
	}
	for _, ot := range []OrderType{OrderTypeIceberg, OrderTypeMOC, OrderTypeMOO} {
		if caps.HasOrderType(ot) {
			t.Errorf("simulator must NOT yet support %s — that lands later", ot)
		}
	}
	if !caps.HasTimeInForce(TIFDay) || !caps.HasTimeInForce(TIFIOC) || !caps.HasTimeInForce(TIFFOK) {
		t.Errorf("simulator must support DAY/IOC/FOK, got %v", caps.TimeInForces)
	}
	if !caps.SupportsCancel {
		t.Errorf("simulator must support cancel")
	}
	if !caps.SupportsReplace {
		t.Errorf("simulator must support replace post-P0-5")
	}
	if !caps.SupportsStream {
		t.Errorf("simulator must support stream")
	}
	if caps.SupportsAccountSnapshot {
		t.Errorf("simulator GetAccountSnapshot is not the source of truth")
	}
}

// ---------------------------------------------------------------------------
// PlaceOrder — happy path
// ---------------------------------------------------------------------------

func TestPlaceOrder_MarketBuyFillsImmediately(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(150.0, 149.95, 150.05))
	ctx := context.Background()

	order, err := s.PlaceOrder(ctx, validBuyMarket())
	if err != nil {
		t.Fatalf("PlaceOrder error = %v", err)
	}
	if order.State != OrderStateFilled {
		t.Errorf("state = %s, want filled", order.State)
	}
	if order.FilledQuantity != 100 {
		t.Errorf("filled qty = %v, want 100", order.FilledQuantity)
	}
	if order.AvgFillPrice != 150.0 {
		t.Errorf("avg fill = %v, want 150 (zero-slippage default)", order.AvgFillPrice)
	}
	if order.BrokerOrderID == "" {
		t.Errorf("broker_order_id must be set")
	}
	if order.ClientOrderID != "client-1" {
		t.Errorf("client_order_id round-trip broken: got %q", order.ClientOrderID)
	}
}

func TestPlaceOrder_MarketSellFillsImmediately(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(50.0, 49.99, 50.01))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-sell-mkt"
	req.Side = SideSell

	order, err := s.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("PlaceOrder error = %v", err)
	}
	if order.State != OrderStateFilled {
		t.Errorf("state = %s, want filled", order.State)
	}
	if order.AvgFillPrice != 50.0 {
		t.Errorf("avg fill = %v, want 50", order.AvgFillPrice)
	}
}

func TestPlaceOrder_LimitMarketableFills(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-lim-marketable"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 105.0 // willing to pay 105, market is 100 → marketable

	order, err := s.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("PlaceOrder error = %v", err)
	}
	if order.State != OrderStateFilled {
		t.Errorf("state = %s, want filled", order.State)
	}
	if order.AvgFillPrice > req.LimitPrice {
		t.Errorf("fill price %v exceeded limit %v", order.AvgFillPrice, req.LimitPrice)
	}
}

// ---------------------------------------------------------------------------
// Limit non-marketable: rests vs cancels by TIF
// ---------------------------------------------------------------------------

func TestPlaceOrder_LimitDayRestsWhenNotMarketable(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-lim-day-rest"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 90.0 // willing to pay 90, market is 100 → NOT marketable
	req.TimeInForce = TIFDay

	order, err := s.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("PlaceOrder error = %v (DAY non-marketable should rest, not error)", err)
	}
	if order.State != OrderStateWorking {
		t.Errorf("state = %s, want working", order.State)
	}
	if order.FilledQuantity != 0 {
		t.Errorf("non-marketable DAY should not fill, got filled qty = %v", order.FilledQuantity)
	}

	// And it should appear in ListOpenOrders.
	open, err := s.ListOpenOrders(ctx, "fund-A")
	if err != nil {
		t.Fatalf("ListOpenOrders error = %v", err)
	}
	if len(open) != 1 || open[0].BrokerOrderID != order.BrokerOrderID {
		t.Errorf("expected resting order in open list, got %d entries", len(open))
	}
}

func TestPlaceOrder_LimitIOCCancelsWhenNotMarketable(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-lim-ioc-cancel"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 90.0
	req.TimeInForce = TIFIOC

	order, err := s.PlaceOrder(ctx, req)
	if !errors.Is(err, ErrLimitNotMarketable) {
		t.Errorf("err = %v, want ErrLimitNotMarketable", err)
	}
	if order == nil || order.State != OrderStateCancelled {
		t.Errorf("state = %v, want cancelled (IOC non-marketable)", order)
	}
}

func TestPlaceOrder_LimitFOKCancelsWhenNotMarketable(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-lim-fok-cancel"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 90.0
	req.TimeInForce = TIFFOK

	order, err := s.PlaceOrder(ctx, req)
	if !errors.Is(err, ErrLimitNotMarketable) {
		t.Errorf("err = %v, want ErrLimitNotMarketable", err)
	}
	if order == nil || order.State != OrderStateCancelled {
		t.Errorf("state = %v, want cancelled (FOK non-marketable)", order)
	}
}

// ---------------------------------------------------------------------------
// Idempotency: duplicate client_order_id returns the same order, no duplicate fill
// ---------------------------------------------------------------------------

func TestPlaceOrder_IdempotentDuplicateClientOrderID(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	first, err := s.PlaceOrder(ctx, validBuyMarket())
	if err != nil {
		t.Fatalf("first PlaceOrder error = %v", err)
	}

	// Second call with same (FundID, ClientOrderID) returns the same order.
	second, err := s.PlaceOrder(ctx, validBuyMarket())
	if err != nil {
		t.Fatalf("second PlaceOrder error = %v", err)
	}
	if first.BrokerOrderID != second.BrokerOrderID {
		t.Errorf("duplicate must return same broker_order_id; first=%s second=%s",
			first.BrokerOrderID, second.BrokerOrderID)
	}

	// And only ONE order exists in the simulator.
	got, err := s.GetOrderByClientID(ctx, "fund-A", "client-1")
	if err != nil {
		t.Fatalf("GetOrderByClientID error = %v", err)
	}
	if got.BrokerOrderID != first.BrokerOrderID {
		t.Errorf("looked-up order mismatch: got %s, want %s", got.BrokerOrderID, first.BrokerOrderID)
	}
}

func TestPlaceOrder_IdempotentDuplicateDoesNotEmitSecondFill(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fills, err := s.StreamFills(ctx, "fund-A")
	if err != nil {
		t.Fatalf("StreamFills error = %v", err)
	}

	// Place once; expect 1 fill.
	if _, err := s.PlaceOrder(ctx, validBuyMarket()); err != nil {
		t.Fatalf("PlaceOrder #1 error = %v", err)
	}
	// Place again with same id; must NOT emit.
	if _, err := s.PlaceOrder(ctx, validBuyMarket()); err != nil {
		t.Fatalf("PlaceOrder #2 (dup) error = %v", err)
	}

	count := drainFills(fills, 100*time.Millisecond)
	if count != 1 {
		t.Errorf("emitted fills = %d, want 1 (duplicate must be no-op)", count)
	}
}

// ---------------------------------------------------------------------------
// Idempotency boundary: same client_order_id but different fund -> distinct
// ---------------------------------------------------------------------------

func TestPlaceOrder_IdempotencyScopedByFund(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100.0, 99.95, 100.05))
	ctx := context.Background()

	a := validBuyMarket()
	b := validBuyMarket()
	b.FundID = "fund-B"

	oa, err := s.PlaceOrder(ctx, a)
	if err != nil {
		t.Fatalf("place A: %v", err)
	}
	ob, err := s.PlaceOrder(ctx, b)
	if err != nil {
		t.Fatalf("place B: %v", err)
	}
	if oa.BrokerOrderID == ob.BrokerOrderID {
		t.Errorf("idempotency must be scoped per fund: same broker id across funds is wrong")
	}
}

// ---------------------------------------------------------------------------
// Validation errors
// ---------------------------------------------------------------------------

func TestPlaceOrder_ValidationRejectsBadInput(t *testing.T) {
	s := newTestSimulator(t, nil)
	ctx := context.Background()

	cases := []struct {
		name   string
		mutate func(*PlaceOrderRequest)
		want   string // substring of error message
	}{
		{"missing fund id", func(r *PlaceOrderRequest) { r.FundID = "" }, "fund id"},
		{"missing client order id", func(r *PlaceOrderRequest) { r.ClientOrderID = "" }, "client_order_id"},
		{"whitespace client order id", func(r *PlaceOrderRequest) { r.ClientOrderID = "   " }, "client_order_id"},
		{"missing symbol+key", func(r *PlaceOrderRequest) { r.Symbol = ""; r.InstrumentKey = "" }, "symbol"},
		{"bad side", func(r *PlaceOrderRequest) { r.Side = "borrow" }, "side"},
		{"zero qty", func(r *PlaceOrderRequest) { r.Quantity = 0 }, "quantity"},
		{"negative qty", func(r *PlaceOrderRequest) { r.Quantity = -10 }, "quantity"},
		{"limit needs price", func(r *PlaceOrderRequest) { r.OrderType = OrderTypeLimit; r.LimitPrice = 0 }, "limit"},
		{"stop-limit needs both", func(r *PlaceOrderRequest) { r.OrderType = OrderTypeStopLimit; r.LimitPrice = 100; r.StopPrice = 0 }, "stop_price"},
		{"trailing needs trail", func(r *PlaceOrderRequest) { r.OrderType = OrderTypeTrailingStop }, "trail"},
		{"iceberg needs display", func(r *PlaceOrderRequest) { r.OrderType = OrderTypeIceberg; r.LimitPrice = 100 }, "display_qty"},
		{"unknown order type", func(r *PlaceOrderRequest) { r.OrderType = "fancy" }, "order type"},
		{"unknown tif", func(r *PlaceOrderRequest) { r.TimeInForce = "weird" }, "time_in_force"},
		{"gtd needs date", func(r *PlaceOrderRequest) { r.TimeInForce = TIFGTD }, "good_till_date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validBuyMarket()
			tc.mutate(&req)
			_, err := s.PlaceOrder(ctx, req)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want substring %q", err.Error(), tc.want)
			}
		})
	}
}

func TestPlaceOrder_RejectsUnsupportedOrderType(t *testing.T) {
	s := newTestSimulator(t, nil)
	ctx := context.Background()

	// Iceberg / MOC / MOO remain unsupported pending later sprints.
	// Stop / stop-limit / trailing-stop now supported (P0-3).
	for _, ot := range []OrderType{OrderTypeIceberg, OrderTypeMOC, OrderTypeMOO} {
		t.Run(string(ot), func(t *testing.T) {
			req := validBuyMarket()
			req.ClientOrderID = "client-" + string(ot)
			req.OrderType = ot
			req.LimitPrice = 100
			req.StopPrice = 100
			req.TrailAmount = 1
			req.DisplayQty = 50

			_, err := s.PlaceOrder(ctx, req)
			if !errors.Is(err, ErrUnsupportedOrderType) {
				t.Errorf("err = %v, want ErrUnsupportedOrderType", err)
			}
		})
	}
}

func TestPlaceOrder_RejectsUnsupportedTIF(t *testing.T) {
	s := newTestSimulator(t, nil)
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-tif-opg"
	req.TimeInForce = TIFOPG // valid TIF but Simulator does not honour it

	_, err := s.PlaceOrder(ctx, req)
	if !errors.Is(err, ErrUnsupportedTimeInForce) {
		t.Errorf("err = %v, want ErrUnsupportedTimeInForce", err)
	}
}

func TestPlaceOrder_DefaultsTIFToDay(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.TimeInForce = "" // unset

	order, err := s.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("PlaceOrder error = %v", err)
	}
	if order.Request.TimeInForce != TIFDay {
		t.Errorf("default TIF = %s, want day", order.Request.TimeInForce)
	}
}

// ---------------------------------------------------------------------------
// Quote unavailable
// ---------------------------------------------------------------------------

func TestPlaceOrder_QuoteErrorRejects(t *testing.T) {
	wantErr := errors.New("upstream timeout")
	s := newTestSimulator(t, erroringQuote(wantErr))
	ctx := context.Background()

	order, err := s.PlaceOrder(ctx, validBuyMarket())
	if !errors.Is(err, ErrNoQuote) {
		t.Errorf("err = %v, want ErrNoQuote", err)
	}
	if order == nil || order.State != OrderStateRejected {
		t.Errorf("order state = %v, want rejected", order)
	}
}

func TestPlaceOrder_EmptyQuoteRejects(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(0, 0, 0)) // no last, no spread
	ctx := context.Background()

	order, err := s.PlaceOrder(ctx, validBuyMarket())
	if !errors.Is(err, ErrNoQuote) {
		t.Errorf("err = %v, want ErrNoQuote", err)
	}
	if order == nil || order.State != OrderStateRejected {
		t.Errorf("order state = %v, want rejected", order)
	}
}

// ---------------------------------------------------------------------------
// Cancel
// ---------------------------------------------------------------------------

func TestCancelOrder_WorkingOrderTransitionsToCancelled(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	// Non-marketable DAY limit rests in working state.
	req := validBuyMarket()
	req.ClientOrderID = "client-cancel-me"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 50.0
	order, err := s.PlaceOrder(ctx, req)
	if err != nil || order.State != OrderStateWorking {
		t.Fatalf("setup: order state = %v err = %v", order, err)
	}

	if err := s.CancelOrder(ctx, CancelOrderRequest{
		FundID:        "fund-A",
		BrokerOrderID: order.BrokerOrderID,
	}); err != nil {
		t.Fatalf("CancelOrder error = %v", err)
	}
	got, _ := s.GetOrder(ctx, "fund-A", order.BrokerOrderID)
	if got.State != OrderStateCancelled {
		t.Errorf("state after cancel = %s, want cancelled", got.State)
	}

	// And it should disappear from the open list.
	open, _ := s.ListOpenOrders(ctx, "fund-A")
	for _, o := range open {
		if o.BrokerOrderID == order.BrokerOrderID {
			t.Errorf("cancelled order still in open list")
		}
	}
}

func TestCancelOrder_FilledOrderReturnsTerminalError(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	order, err := s.PlaceOrder(ctx, validBuyMarket())
	if err != nil || order.State != OrderStateFilled {
		t.Fatalf("setup: order state = %v err = %v", order, err)
	}
	err = s.CancelOrder(ctx, CancelOrderRequest{
		FundID:        "fund-A",
		BrokerOrderID: order.BrokerOrderID,
	})
	if !errors.Is(err, ErrOrderTerminal) {
		t.Errorf("err = %v, want ErrOrderTerminal", err)
	}
}

func TestCancelOrder_NonExistentReturnsNotFound(t *testing.T) {
	s := newTestSimulator(t, nil)
	ctx := context.Background()
	err := s.CancelOrder(ctx, CancelOrderRequest{
		FundID:        "fund-A",
		BrokerOrderID: "does-not-exist",
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("err = %v, want ErrOrderNotFound", err)
	}
}

func TestCancelOrder_ByClientOrderIDWorks(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.ClientOrderID = "client-cancel-by-clid"
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 50.0
	if _, err := s.PlaceOrder(ctx, req); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := s.CancelOrder(ctx, CancelOrderRequest{
		FundID:        "fund-A",
		ClientOrderID: "client-cancel-by-clid",
	}); err != nil {
		t.Errorf("CancelOrder error = %v", err)
	}
}

func TestCancelOrder_RejectsCrossFundCancel(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	req := validBuyMarket()
	req.OrderType = OrderTypeLimit
	req.LimitPrice = 50.0
	order, err := s.PlaceOrder(ctx, req)
	if err != nil {
		t.Fatalf("setup: %v", err)
	}

	// fund-B tries to cancel fund-A's order.
	err = s.CancelOrder(ctx, CancelOrderRequest{
		FundID:        "fund-B",
		BrokerOrderID: order.BrokerOrderID,
	})
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("cross-fund cancel must return ErrOrderNotFound, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// GetOrder / GetOrderByClientID / ListOpenOrders
// ---------------------------------------------------------------------------

func TestGetOrder_RejectsCrossFund(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	order, _ := s.PlaceOrder(ctx, validBuyMarket())
	_, err := s.GetOrder(ctx, "fund-B", order.BrokerOrderID)
	if !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("cross-fund GetOrder must return ErrOrderNotFound, got %v", err)
	}
}

func TestListOpenOrders_OnlyReturnsNonTerminal(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx := context.Background()

	// 1) market order — fills → terminal, NOT in open list.
	if _, err := s.PlaceOrder(ctx, validBuyMarket()); err != nil {
		t.Fatalf("market: %v", err)
	}
	// 2) limit non-marketable DAY → working, IN open list.
	working1 := validBuyMarket()
	working1.ClientOrderID = "client-working-1"
	working1.OrderType = OrderTypeLimit
	working1.LimitPrice = 50.0
	if _, err := s.PlaceOrder(ctx, working1); err != nil {
		t.Fatalf("working1: %v", err)
	}
	// 3) another working
	working2 := validSellLimit(500.0) // sell 500 vs market 100 → non-marketable
	if _, err := s.PlaceOrder(ctx, working2); err != nil {
		t.Fatalf("working2: %v", err)
	}

	open, err := s.ListOpenOrders(ctx, "fund-A")
	if err != nil {
		t.Fatalf("ListOpenOrders error = %v", err)
	}
	if len(open) != 2 {
		t.Errorf("open count = %d, want 2 (filled order should not appear)", len(open))
	}
	for _, o := range open {
		if o.State.IsTerminal() {
			t.Errorf("terminal order leaked into open list: %s/%s", o.BrokerOrderID, o.State)
		}
	}
}

// ---------------------------------------------------------------------------
// StreamFills
// ---------------------------------------------------------------------------

func TestStreamFills_EmitsAndCloses(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx, cancel := context.WithCancel(context.Background())

	fills, err := s.StreamFills(ctx, "fund-A")
	if err != nil {
		t.Fatalf("StreamFills error = %v", err)
	}

	if _, err := s.PlaceOrder(ctx, validBuyMarket()); err != nil {
		t.Fatalf("PlaceOrder: %v", err)
	}

	select {
	case f, ok := <-fills:
		if !ok {
			t.Fatalf("channel closed before fill emitted")
		}
		if f.FundID != "fund-A" {
			t.Errorf("fill FundID = %q", f.FundID)
		}
		if f.Quantity != 100 {
			t.Errorf("fill qty = %v, want 100", f.Quantity)
		}
		if f.Price != 100.0 {
			t.Errorf("fill price = %v, want 100", f.Price)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("timed out waiting for fill")
	}

	// Cancelling ctx must close the channel.
	cancel()
	select {
	case _, ok := <-fills:
		if ok {
			// Drain anything still buffered, then the next recv should be closed.
			for range fills {
			}
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatalf("channel did not close after ctx cancel")
	}
}

func TestStreamFills_OnlyEmitsToCorrectFund(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fundA, _ := s.StreamFills(ctx, "fund-A")
	fundB, _ := s.StreamFills(ctx, "fund-B")

	if _, err := s.PlaceOrder(ctx, validBuyMarket()); err != nil { // fund-A
		t.Fatalf("PlaceOrder: %v", err)
	}

	if got := drainFills(fundA, 100*time.Millisecond); got != 1 {
		t.Errorf("fund-A fills = %d, want 1", got)
	}
	if got := drainFills(fundB, 100*time.Millisecond); got != 0 {
		t.Errorf("fund-B fills = %d, want 0 (no order placed for fund-B)", got)
	}
}

func TestStreamFills_FullBufferDropsRatherThanBlocks(t *testing.T) {
	// Tiny buffer, no draining → second fill should drop, third too,
	// and PlaceOrder for those should NOT block.
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05), WithStreamBufferSize(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if _, err := s.StreamFills(ctx, "fund-A"); err != nil {
		t.Fatalf("StreamFills: %v", err)
	}

	for i := 0; i < 5; i++ {
		req := validBuyMarket()
		req.ClientOrderID = "client-drop-" + itoa(uint64(i))
		done := make(chan error, 1)
		go func() { _, err := s.PlaceOrder(ctx, req); done <- err }()
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("PlaceOrder #%d: %v", i, err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("PlaceOrder #%d blocked: full buffer must drop, not block", i)
		}
	}
}

// ---------------------------------------------------------------------------
// GetAccountSnapshot — explicit unsupported (Replace shipped in P0-5)
// ---------------------------------------------------------------------------

func TestGetAccountSnapshot_ReturnsUnsupported(t *testing.T) {
	s := newTestSimulator(t, nil)
	_, err := s.GetAccountSnapshot(context.Background(), "fund-A")
	if !errors.Is(err, ErrUnsupported) {
		t.Errorf("err = %v, want ErrUnsupported", err)
	}
}

// ---------------------------------------------------------------------------
// Concurrency
// ---------------------------------------------------------------------------

func TestPlaceOrder_ConcurrentDuplicatesBookExactlyOnce(t *testing.T) {
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fills, _ := s.StreamFills(ctx, "fund-A")

	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			_, _ = s.PlaceOrder(ctx, validBuyMarket()) // same client_order_id
		}()
	}
	wg.Wait()

	// Exactly one fill should have been emitted across all 50 calls.
	count := drainFills(fills, 200*time.Millisecond)
	if count != 1 {
		t.Errorf("emitted fills = %d, want 1 (idempotency violated under contention)", count)
	}

	// And only one order exists.
	open, _ := s.ListOpenOrders(ctx, "fund-A")
	if len(open) != 0 {
		t.Errorf("open orders = %d, want 0 (the single order should be filled)", len(open))
	}
	got, err := s.GetOrderByClientID(ctx, "fund-A", "client-1")
	if err != nil {
		t.Fatalf("GetOrderByClientID: %v", err)
	}
	if got.State != OrderStateFilled {
		t.Errorf("state = %s, want filled", got.State)
	}
}

func TestPlaceOrder_ConcurrentDistinctOrdersAllBook(t *testing.T) {
	// Use a generous stream buffer so this test exercises both the
	// canonical "all orders filled" property AND the stream emission
	// without hitting the drop-on-full-buffer path (which has its
	// own dedicated test).
	const n = 50
	s := newTestSimulator(t, fixedQuote(100, 99.95, 100.05), WithStreamBufferSize(n*2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fills, _ := s.StreamFills(ctx, "fund-A")

	clientIDs := make([]string, n)
	for i := range clientIDs {
		clientIDs[i] = "client-conc-" + itoa(uint64(i))
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			req := validBuyMarket()
			req.ClientOrderID = clientIDs[i]
			_, _ = s.PlaceOrder(ctx, req)
		}(i)
	}
	wg.Wait()

	// Canonical: every distinct client_order_id resolves to a filled
	// order. This is the contract callers rely on.
	for _, id := range clientIDs {
		got, err := s.GetOrderByClientID(ctx, "fund-A", id)
		if err != nil {
			t.Errorf("GetOrderByClientID(%s): %v", id, err)
			continue
		}
		if got.State != OrderStateFilled {
			t.Errorf("order %s state = %s, want filled", id, got.State)
		}
	}

	// And the stream — with a large enough buffer — captured all of
	// them.
	got := drainFills(fills, 500*time.Millisecond)
	if got != n {
		t.Errorf("emitted fills = %d, want %d", got, n)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// drainFills reads from ch until either the channel is empty for `idle` or
// the test deadline elapses, and returns how many fills were received.
func drainFills(ch <-chan Fill, idle time.Duration) int {
	count := 0
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return count
			}
			count++
		case <-time.After(idle):
			return count
		}
	}
}
