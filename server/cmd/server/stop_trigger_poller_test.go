package main

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
	"github.com/fundai/server/internal/stoptrigger"
)

// silentLogger discards log output so tests don't flood the console.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// staticQuote returns the supplied quote on every call.
func staticQuote(q matching.Quote, err error) broker.QuoteFn {
	return func(_ context.Context, _, _, _ string) (matching.Quote, error) {
		return q, err
	}
}

// trackingQuote returns the supplied quote AND counts invocations
// per (instrument_key, symbol).
func trackingQuote(q matching.Quote) (broker.QuoteFn, *atomic.Int32, *map[string]int32) {
	var calls atomic.Int32
	per := make(map[string]int32)
	return func(_ context.Context, ik, sym, _ string) (matching.Quote, error) {
		calls.Add(1)
		per[ik+"|"+sym]++
		return q, nil
	}, &calls, &per
}

func newPollerSimulator(t *testing.T, q broker.QuoteFn) *broker.Simulator {
	t.Helper()
	var n atomic.Uint64
	return broker.NewSimulator(q,
		broker.WithIDGenerator(func() string {
			n.Add(1)
			return "id-" + itoaInt(int(n.Load()))
		}),
		broker.WithClock(func() time.Time { return time.Date(2026, 1, 1, 9, 30, 0, 0, time.UTC) }),
	)
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	negative := n < 0
	if negative {
		n = -n
	}
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	out := string(buf[i:])
	if negative {
		return "-" + out
	}
	return out
}

// ---------------------------------------------------------------------------
// Constructor invariants
// ---------------------------------------------------------------------------

func TestNewStopTriggerPoller_NilOnMissingDeps(t *testing.T) {
	q := staticQuote(matching.Quote{Last: 100}, nil)
	sim := newPollerSimulator(t, q)
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))

	if newStopTriggerPoller(nil, sim, q, time.Second, silentLogger()) != nil {
		t.Errorf("expected nil when engine is missing")
	}
	if newStopTriggerPoller(eng, nil, q, time.Second, silentLogger()) != nil {
		t.Errorf("expected nil when sim is missing")
	}
	if newStopTriggerPoller(eng, sim, nil, time.Second, silentLogger()) != nil {
		t.Errorf("expected nil when quoteFn is missing")
	}
}

func TestNewStopTriggerPoller_DefaultsInterval(t *testing.T) {
	q := staticQuote(matching.Quote{Last: 100}, nil)
	sim := newPollerSimulator(t, q)
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, q, 0, silentLogger())
	if p == nil {
		t.Fatal("nil poller")
	}
	if p.interval != 5*time.Second {
		t.Errorf("interval = %v, want default 5s", p.interval)
	}
}

// ---------------------------------------------------------------------------
// Tick: deduplication + fan-out
// ---------------------------------------------------------------------------

func TestPoller_Tick_DeduplicatesQuotesByInstrument(t *testing.T) {
	// Quote is well above all stop levels so the engine inspects
	// but never fires, isolating the poller's dedup behaviour
	// from downstream child-order quote fetches inside the
	// matching engine.
	q := matching.Quote{Last: 500, Bid: 499.95, Ask: 500.05}
	quoteFn, calls, per := trackingQuote(q)

	sim := newPollerSimulator(t, quoteFn)
	ctx := context.Background()

	// Two stops on AAPL, one on TSLA — quote fetch should run
	// EXACTLY twice (once per unique instrument).
	for _, c := range []struct {
		clientID string
		sym, ik  string
		stop     float64
	}{
		{"a1", "AAPL", "us:AAPL", 95},
		{"a2", "AAPL", "us:AAPL", 96},
		{"t1", "TSLA", "us:TSLA", 200},
	} {
		if _, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
			FundID: "f1", ClientOrderID: c.clientID, Symbol: c.sym, InstrumentKey: c.ik,
			Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 1, StopPrice: c.stop,
		}); err != nil {
			t.Fatal(err)
		}
	}
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, quoteFn, time.Second, silentLogger())

	p.Tick(ctx)

	if got := calls.Load(); got != 2 {
		t.Errorf("quote fetches = %d, want 2 (one per unique instrument)", got)
	}
	if (*per)["us:AAPL|AAPL"] != 1 || (*per)["us:TSLA|TSLA"] != 1 {
		t.Errorf("per-instrument fetch count off: %#v", *per)
	}
}

// ---------------------------------------------------------------------------
// Tick: actually fires triggered stops
// ---------------------------------------------------------------------------

func TestPoller_Tick_FiresStopsThatTrigger(t *testing.T) {
	q := matching.Quote{Last: 90, Bid: 89.95, Ask: 90.05}
	sim := newPollerSimulator(t, staticQuote(q, nil))
	ctx := context.Background()

	parent, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 5, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}

	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, staticQuote(q, nil), time.Second, silentLogger())
	p.Tick(ctx)

	got, err := sim.GetOrder(ctx, "f1", parent.BrokerOrderID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != broker.OrderStateTriggered {
		t.Errorf("parent state = %s, want triggered", got.State)
	}
	snap := p.Snapshot()
	if snap.Ticks != 1 || snap.Fired != 1 {
		t.Errorf("snapshot = %+v, want Ticks=1 Fired=1", snap)
	}
}

// ---------------------------------------------------------------------------
// Tick: error tolerance
// ---------------------------------------------------------------------------

func TestPoller_Tick_QuoteErrorDoesNotFireStops(t *testing.T) {
	feedErr := errors.New("feed down")
	sim := newPollerSimulator(t, staticQuote(matching.Quote{}, feedErr))
	ctx := context.Background()

	parent, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 5, StopPrice: 95,
	})
	if err != nil {
		t.Fatal(err)
	}

	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, staticQuote(matching.Quote{}, feedErr), time.Second, silentLogger())
	p.Tick(ctx)

	got, _ := sim.GetOrder(ctx, "f1", parent.BrokerOrderID)
	if got.State != broker.OrderStatePending {
		t.Errorf("parent state = %s, want pending (quote error must not fire)", got.State)
	}
	snap := p.Snapshot()
	if snap.Errors == 0 {
		t.Errorf("expected Errors > 0 on quote feed failure, got %+v", snap)
	}
	if snap.Fired != 0 {
		t.Errorf("Fired = %d, want 0", snap.Fired)
	}
}

func TestPoller_Tick_HaltedQuoteIsSkippedSilently(t *testing.T) {
	sim := newPollerSimulator(t, staticQuote(matching.Quote{}, nil)) // empty quote
	ctx := context.Background()

	if _, err := sim.PlaceOrder(ctx, broker.PlaceOrderRequest{
		FundID: "f1", ClientOrderID: "stop", Symbol: "AAPL", InstrumentKey: "us:AAPL",
		Side: broker.SideSell, OrderType: broker.OrderTypeStop, Quantity: 5, StopPrice: 95,
	}); err != nil {
		t.Fatal(err)
	}

	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, staticQuote(matching.Quote{}, nil), time.Second, silentLogger())
	p.Tick(ctx)

	snap := p.Snapshot()
	if snap.Errors != 0 {
		t.Errorf("Errors = %d, want 0 (halted is not an error)", snap.Errors)
	}
	if snap.Fired != 0 {
		t.Errorf("Fired = %d, want 0", snap.Fired)
	}
}

// ---------------------------------------------------------------------------
// Tick: no work when no pending stops
// ---------------------------------------------------------------------------

func TestPoller_Tick_NoWorkWhenNoPendingStops(t *testing.T) {
	q := staticQuote(matching.Quote{Last: 100}, nil)
	sim := newPollerSimulator(t, q)
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))

	// trackingQuote variant so we can detect stray quote calls.
	tq, calls, _ := trackingQuote(matching.Quote{Last: 100})
	p := newStopTriggerPoller(eng, sim, tq, time.Second, silentLogger())
	p.Tick(context.Background())

	if calls.Load() != 0 {
		t.Errorf("quote fetches = %d, want 0 with empty pending list", calls.Load())
	}
	snap := p.Snapshot()
	if snap.Ticks != 1 {
		t.Errorf("Ticks = %d, want 1", snap.Ticks)
	}
}

// ---------------------------------------------------------------------------
// Run lifecycle: cancellation
// ---------------------------------------------------------------------------

func TestPoller_Run_StopsOnCtxCancel(t *testing.T) {
	q := staticQuote(matching.Quote{Last: 100}, nil)
	sim := newPollerSimulator(t, q)
	eng := stoptrigger.New(sim, stoptrigger.WithLogger(silentLogger()))
	p := newStopTriggerPoller(eng, sim, q, 10*time.Millisecond, silentLogger())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Let one tick fire so we know the loop is alive.
	time.Sleep(25 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("poller did not stop within 1s of cancel")
	}
}
