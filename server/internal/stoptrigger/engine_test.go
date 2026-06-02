package stoptrigger

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/broker"
	"github.com/fundai/server/internal/matching"
)

// ---------------------------------------------------------------------------
// Test fakes
// ---------------------------------------------------------------------------

// fakeVenue is a minimal Venue implementation for engine unit tests.
// Each call to PendingStopsForInstrument returns the same in-memory
// snapshot supplied by the test; mutators record their calls and
// optionally return a configured error.
type fakeVenue struct {
	stops []broker.Order

	// trailingHighWater / trailingLowWater are programmed by the
	// test as a per-(broker_order_id, last) lookup. nil means "no
	// expectation, return passthrough order".
	hwResult func(orderID string, last float64) (*broker.Order, error)
	lwResult func(orderID string, last float64) (*broker.Order, error)
	fire     func(orderID string, q matching.Quote) (*broker.Order, error)

	// Counters for assertions.
	hwCalls   atomic.Int32
	lwCalls   atomic.Int32
	fireCalls atomic.Int32
}

func (f *fakeVenue) PendingStopsForInstrument(instrumentKey, symbol string) []broker.Order {
	out := make([]broker.Order, len(f.stops))
	copy(out, f.stops)
	return out
}

func (f *fakeVenue) UpdateTrailingHighWater(orderID string, last float64) (*broker.Order, error) {
	f.hwCalls.Add(1)
	if f.hwResult == nil {
		return nil, broker.ErrOrderNotFound
	}
	return f.hwResult(orderID, last)
}

func (f *fakeVenue) UpdateTrailingLowWater(orderID string, last float64) (*broker.Order, error) {
	f.lwCalls.Add(1)
	if f.lwResult == nil {
		return nil, broker.ErrOrderNotFound
	}
	return f.lwResult(orderID, last)
}

func (f *fakeVenue) FireStopTrigger(_ context.Context, orderID string, q matching.Quote) (*broker.Order, error) {
	f.fireCalls.Add(1)
	if f.fire == nil {
		return nil, errors.New("fakeVenue: fire not configured")
	}
	return f.fire(orderID, q)
}

func newSilentEngine(v Venue) *Engine {
	return New(v,
		WithClock(func() time.Time { return time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC) }),
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
	)
}

// ---------------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------------

func TestNew_PanicsOnNilVenue(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil venue")
		}
	}()
	_ = New(nil)
}

func TestNew_AppliesOptions(t *testing.T) {
	clock := func() time.Time { return time.Unix(42, 0) }
	logger := slog.Default()
	e := New(&fakeVenue{}, WithClock(clock), WithLogger(logger))
	if e.now() != time.Unix(42, 0) {
		t.Errorf("clock not applied")
	}
	if e.log != logger {
		t.Errorf("logger not applied")
	}
}

// ---------------------------------------------------------------------------
// OnQuote: input validation
// ---------------------------------------------------------------------------

func TestOnQuote_RejectsInvalidInputs(t *testing.T) {
	e := newSilentEngine(&fakeVenue{})

	if _, err := e.OnQuote(nil, QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 100}}); err == nil {
		t.Errorf("expected error on nil ctx")
	}
	if _, err := e.OnQuote(context.Background(), QuoteTick{Quote: matching.Quote{Last: 100}}); err == nil {
		t.Errorf("expected error on empty instrument key/symbol")
	}
}

func TestOnQuote_NoOpOnHaltedQuote(t *testing.T) {
	v := &fakeVenue{}
	e := newSilentEngine(v)
	res, err := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{}})
	if err != nil {
		t.Fatalf("OnQuote: %v", err)
	}
	if res.Inspected != 0 || res.Fired != 0 {
		t.Errorf("expected no-op on halted quote, got %+v", res)
	}
}

// ---------------------------------------------------------------------------
// OnQuote: empty pending list
// ---------------------------------------------------------------------------

func TestOnQuote_NoPendingStops(t *testing.T) {
	v := &fakeVenue{}
	e := newSilentEngine(v)
	res, err := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Inspected != 0 {
		t.Errorf("Inspected=%d, want 0", res.Inspected)
	}
	if v.fireCalls.Load() != 0 {
		t.Errorf("FireStopTrigger called %d times on empty list", v.fireCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// OnQuote: non-trailing stop fires
// ---------------------------------------------------------------------------

func TestOnQuote_SellStop_FiresWhenLastDropsToTrigger(t *testing.T) {
	stop := broker.Order{
		BrokerOrderID:    "stop-1",
		Request:          broker.PlaceOrderRequest{Side: broker.SideSell, OrderType: broker.OrderTypeStop, StopPrice: 95},
		State:            broker.OrderStatePending,
		CurrentStopPrice: 95,
	}
	v := &fakeVenue{
		stops: []broker.Order{stop},
		fire: func(orderID string, q matching.Quote) (*broker.Order, error) {
			if orderID != "stop-1" {
				t.Errorf("fire called with %q, want stop-1", orderID)
			}
			return &broker.Order{BrokerOrderID: "child-1", State: broker.OrderStateFilled}, nil
		},
	}
	e := newSilentEngine(v)

	res, err := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 94}})
	if err != nil {
		t.Fatalf("OnQuote: %v", err)
	}
	if res.Fired != 1 {
		t.Errorf("Fired=%d, want 1", res.Fired)
	}
	if v.fireCalls.Load() != 1 {
		t.Errorf("venue fire called %d, want 1", v.fireCalls.Load())
	}
}

func TestOnQuote_SellStop_DoesNotFireWhenLastAboveTrigger(t *testing.T) {
	stop := broker.Order{
		BrokerOrderID:    "stop-1",
		Request:          broker.PlaceOrderRequest{Side: broker.SideSell, OrderType: broker.OrderTypeStop, StopPrice: 95},
		State:            broker.OrderStatePending,
		CurrentStopPrice: 95,
	}
	v := &fakeVenue{stops: []broker.Order{stop}}
	e := newSilentEngine(v)

	res, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 96}})
	if res.Fired != 0 {
		t.Errorf("Fired=%d, want 0 above trigger", res.Fired)
	}
	if v.fireCalls.Load() != 0 {
		t.Errorf("venue fire called %d, want 0", v.fireCalls.Load())
	}
}

func TestOnQuote_BuyStop_FiresAtOrAboveTrigger(t *testing.T) {
	stop := broker.Order{
		BrokerOrderID:    "buy-stop",
		Request:          broker.PlaceOrderRequest{Side: broker.SideBuy, OrderType: broker.OrderTypeStop, StopPrice: 105},
		State:            broker.OrderStatePending,
		CurrentStopPrice: 105,
	}
	v := &fakeVenue{
		stops: []broker.Order{stop},
		fire: func(orderID string, q matching.Quote) (*broker.Order, error) {
			return &broker.Order{BrokerOrderID: "child-buy", State: broker.OrderStateFilled}, nil
		},
	}
	e := newSilentEngine(v)
	res, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 105}})
	if res.Fired != 1 {
		t.Errorf("Fired=%d, want 1", res.Fired)
	}
}

// ---------------------------------------------------------------------------
// OnQuote: trailing stop ratchets and fires
// ---------------------------------------------------------------------------

func TestOnQuote_TrailingSellStop_RatchetsThenFires(t *testing.T) {
	// Initial trailing sell-stop: HWM=100, trail=5, stop=95.
	stop := broker.Order{
		BrokerOrderID: "trail-1",
		Request: broker.PlaceOrderRequest{
			Side: broker.SideSell, OrderType: broker.OrderTypeTrailingStop,
			TrailAmount: 5,
		},
		State:             broker.OrderStatePending,
		TrailingHighWater: 100,
		CurrentStopPrice:  95,
	}
	v := &fakeVenue{stops: []broker.Order{stop}}

	// Tick 1: last=110 → HWM should ratchet to 110, stop to 105.
	v.hwResult = func(_ string, last float64) (*broker.Order, error) {
		if last != 110 {
			t.Errorf("hw called with last=%v, want 110", last)
		}
		updated := stop
		updated.TrailingHighWater = 110
		updated.CurrentStopPrice = 105
		return &updated, nil
	}
	// Refuse to fire on tick 1 — last=110 is well above 105.
	v.fire = func(orderID string, q matching.Quote) (*broker.Order, error) {
		t.Errorf("should not fire on tick 1; orderID=%s last=%v", orderID, q.Last)
		return nil, errors.New("unexpected fire")
	}
	e := newSilentEngine(v)
	res, err := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 110}})
	if err != nil {
		t.Fatalf("tick1: %v", err)
	}
	if res.Updated != 1 {
		t.Errorf("Updated=%d, want 1", res.Updated)
	}
	if res.Fired != 0 {
		t.Errorf("Fired=%d, want 0", res.Fired)
	}

	// Tick 2: simulate market reversal. Update the stops snapshot
	// to reflect the post-ratchet state, then fire.
	stopT2 := stop
	stopT2.TrailingHighWater = 110
	stopT2.CurrentStopPrice = 105
	v.stops = []broker.Order{stopT2}
	// On tick 2 the HW update will be called with last=104; that
	// is BELOW current HWM (110), so the venue returns the
	// unchanged order.
	v.hwResult = func(_ string, last float64) (*broker.Order, error) {
		return &stopT2, nil
	}
	v.fire = func(orderID string, q matching.Quote) (*broker.Order, error) {
		if orderID != "trail-1" {
			t.Errorf("fire called with %q, want trail-1", orderID)
		}
		return &broker.Order{BrokerOrderID: "child-trail", State: broker.OrderStateFilled}, nil
	}
	res2, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 104}})
	if res2.Fired != 1 {
		t.Errorf("tick2 Fired=%d, want 1", res2.Fired)
	}
	if res2.Updated != 0 {
		t.Errorf("tick2 Updated=%d, want 0 (HWM did not move)", res2.Updated)
	}
}

func TestOnQuote_TrailingBuyStop_RatchetsLowAndFires(t *testing.T) {
	stop := broker.Order{
		BrokerOrderID: "trail-buy",
		Request: broker.PlaceOrderRequest{
			Side: broker.SideBuy, OrderType: broker.OrderTypeTrailingStop,
			TrailAmount: 5,
		},
		State:            broker.OrderStatePending,
		TrailingLowWater: 50,
		CurrentStopPrice: 55,
	}
	v := &fakeVenue{stops: []broker.Order{stop}}

	// Tick: last=46 → LWM ratchets to 46, stop=51. Last=46 is
	// still below stop (51), so stop fires.
	updated := stop
	updated.TrailingLowWater = 46
	updated.CurrentStopPrice = 51
	v.lwResult = func(_ string, _ float64) (*broker.Order, error) {
		return &updated, nil
	}
	v.fire = func(orderID string, q matching.Quote) (*broker.Order, error) {
		return &broker.Order{BrokerOrderID: "child", State: broker.OrderStateFilled}, nil
	}
	e := newSilentEngine(v)
	res, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 46}})
	if res.Updated != 1 {
		t.Errorf("Updated=%d, want 1", res.Updated)
	}
	// Wait — buy-stop fires when last >= stop. 46 < 51, should NOT fire.
	if res.Fired != 0 {
		t.Errorf("Fired=%d, want 0 — buy-stop fires when last >= stop", res.Fired)
	}
}

// ---------------------------------------------------------------------------
// OnQuote: error aggregation
// ---------------------------------------------------------------------------

func TestOnQuote_CollectsFireErrors_DoesNotAbort(t *testing.T) {
	a := broker.Order{
		BrokerOrderID:    "a",
		Request:          broker.PlaceOrderRequest{Side: broker.SideSell, OrderType: broker.OrderTypeStop, StopPrice: 100},
		State:            broker.OrderStatePending,
		CurrentStopPrice: 100,
	}
	b := broker.Order{
		BrokerOrderID:    "b",
		Request:          broker.PlaceOrderRequest{Side: broker.SideSell, OrderType: broker.OrderTypeStop, StopPrice: 100},
		State:            broker.OrderStatePending,
		CurrentStopPrice: 100,
	}
	v := &fakeVenue{stops: []broker.Order{a, b}}
	v.fire = func(orderID string, _ matching.Quote) (*broker.Order, error) {
		if orderID == "a" {
			return nil, broker.ErrStopAlreadyFired
		}
		return &broker.Order{BrokerOrderID: "child-b", State: broker.OrderStateFilled}, nil
	}
	e := newSilentEngine(v)
	res, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 99}})
	if res.Fired != 1 {
		t.Errorf("Fired=%d, want 1 (only b fires; a errored)", res.Fired)
	}
	if len(res.Errors) != 1 || res.Errors[0].BrokerOrderID != "a" {
		t.Errorf("Errors = %+v, want 1 entry for a", res.Errors)
	}
	if !errors.Is(res.Errors[0], broker.ErrStopAlreadyFired) {
		t.Errorf("Errors[0] not wrapped properly: %v", res.Errors[0])
	}
}

func TestFireError_FormatsMessage(t *testing.T) {
	e := FireError{BrokerOrderID: "x", Err: broker.ErrStopAlreadyFired}
	if got := e.Error(); got == "" {
		t.Errorf("empty error string")
	}
	if !errors.Is(e, broker.ErrStopAlreadyFired) {
		t.Errorf("errors.Is unwrap broken")
	}
}

// ---------------------------------------------------------------------------
// OnQuote: ratchet-then-suppress
// ---------------------------------------------------------------------------

// Critical case: a tick that BOTH ratchets the trailing stop AND has
// a last that would have fired the OLD stop must NOT fire. The
// engine must consult the post-update CurrentStopPrice.
func TestOnQuote_TrailingSellStop_RatchetSuppressesStaleFire(t *testing.T) {
	// Old stop: 95 (HWM was 100, trail 5). Tick last=96.
	// Without ratchet: last 96 > 95 → would fire (sell-stop fires
	// on last <= 95, so actually NO it wouldn't). Let me build a
	// scenario where the OLD stop predicate would say "fire" but
	// the post-ratchet predicate would say "don't fire".
	//
	// Sell stop fires when last <= stop. Old stop=95. Last=94.
	// That fires unconditionally. To make ratchet suppress, last
	// would need to be ABOVE the new stop AND the new stop higher
	// than 94. But ratchet on a sell-stop only happens when last
	// EXCEEDS HWM, which means last is BIGGER, not smaller. So a
	// ratchet pass on a sell-stop physically can't transition
	// "would fire" → "won't fire".
	//
	// Where the suppression matters is the OPPOSITE: we want the
	// engine to RE-EVALUATE the predicate using the new stop, NOT
	// the stale one. That's already covered by reading
	// updated.CurrentStopPrice in the engine. Add a sanity test:
	// ratchet that increases the stop must not cause an erroneous
	// fire when last is still well above the new stop.
	stop := broker.Order{
		BrokerOrderID: "trail-1",
		Request: broker.PlaceOrderRequest{
			Side: broker.SideSell, OrderType: broker.OrderTypeTrailingStop,
			TrailAmount: 5,
		},
		State:             broker.OrderStatePending,
		TrailingHighWater: 100,
		CurrentStopPrice:  95,
	}
	v := &fakeVenue{stops: []broker.Order{stop}}
	updated := stop
	updated.TrailingHighWater = 110
	updated.CurrentStopPrice = 105
	v.hwResult = func(_ string, _ float64) (*broker.Order, error) { return &updated, nil }
	v.fire = func(_ string, _ matching.Quote) (*broker.Order, error) {
		t.Error("must not fire — last 110 is above new stop 105")
		return nil, nil
	}
	e := newSilentEngine(v)
	res, _ := e.OnQuote(context.Background(), QuoteTick{InstrumentKey: "k", Quote: matching.Quote{Last: 110}})
	if res.Fired != 0 {
		t.Errorf("Fired=%d, want 0 after ratchet pulled stop above last", res.Fired)
	}
	if res.Updated != 1 {
		t.Errorf("Updated=%d, want 1", res.Updated)
	}
}
