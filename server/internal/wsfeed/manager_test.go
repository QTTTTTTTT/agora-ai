package wsfeed_test

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/wsfeed"
	wsfeedprovider "github.com/fundai/server/internal/wsfeed/provider"
)

// waitFor polls fn every 5ms up to deadline; fails the test
// if fn never returns true. Used because the dispatcher
// runs in its own goroutine and we don't want flaky tests.
func waitFor(t *testing.T, deadline time.Duration, fn func() bool) {
	t.Helper()
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !fn() {
		t.Fatalf("condition not met within %s", deadline)
	}
}

func TestManagerSubscriptionRefCount(t *testing.T) {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	mock := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(mock); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	waitFor(t, time.Second, func() bool { return mock.State() == wsfeed.StateConnected })

	for i := 0; i < 3; i++ {
		if err := mgr.Subscribe(wsfeed.Subscription{Symbol: "AAPL", Market: "US"}); err != nil {
			t.Fatalf("Subscribe[%d]: %v", i, err)
		}
	}
	waitFor(t, time.Second, func() bool { return mock.SubscribeCalls() == 1 })
	if got := mock.SubscribeCalls(); got != 1 {
		t.Fatalf("upstream Subscribe called %d times, want 1", got)
	}

	subs := mgr.Subscriptions()
	if len(subs) != 1 {
		t.Fatalf("manager Subscriptions: %d, want 1", len(subs))
	}
	if subs[0].Consumers != 3 {
		t.Fatalf("Consumers=%d, want 3", subs[0].Consumers)
	}

	if err := mgr.Unsubscribe("AAPL"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if err := mgr.Unsubscribe("AAPL"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	if mock.UnsubscribeCalls() != 0 {
		t.Fatalf("UnsubscribeCalls=%d, want 0 (still have 1 ref)", mock.UnsubscribeCalls())
	}

	if err := mgr.Unsubscribe("AAPL"); err != nil {
		t.Fatalf("Unsubscribe: %v", err)
	}
	waitFor(t, time.Second, func() bool { return mock.UnsubscribeCalls() == 1 })
	if got := len(mgr.Subscriptions()); got != 0 {
		t.Fatalf("manager subs after full release: %d, want 0", got)
	}
}

func TestManagerFanOutTickToHandlers(t *testing.T) {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	mock := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(mock); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}

	var (
		mu    sync.Mutex
		ticks []wsfeed.Tick
	)
	mgr.AddTickHandler(func(t wsfeed.Tick) {
		mu.Lock()
		ticks = append(ticks, t)
		mu.Unlock()
	})

	var seen atomic.Uint64
	mgr.AddTickHandler(func(wsfeed.Tick) { seen.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	waitFor(t, time.Second, func() bool { return mock.State() == wsfeed.StateConnected })

	mock.EmitTrade("MSFT", 410.25, 100)
	mock.EmitTrade("MSFT", 410.50, 200)
	mock.EmitQuote("MSFT", 410.49, 50, 410.51, 75)

	waitFor(t, time.Second, func() bool { return seen.Load() == 3 })

	mu.Lock()
	defer mu.Unlock()
	if len(ticks) != 3 {
		t.Fatalf("got %d ticks, want 3", len(ticks))
	}
	if ticks[0].Symbol != "MSFT" || ticks[0].Last != 410.25 {
		t.Fatalf("ticks[0]=%+v", ticks[0])
	}
	if ticks[2].EventType != wsfeed.TickQuote || ticks[2].Bid != 410.49 {
		t.Fatalf("ticks[2]=%+v", ticks[2])
	}
}

func TestManagerReconnectResubscribesAllSymbols(t *testing.T) {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	mock := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(mock); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()

	waitFor(t, time.Second, func() bool { return mock.State() == wsfeed.StateConnected })

	for _, sym := range []string{"AAPL", "MSFT", "GOOG"} {
		if err := mgr.Subscribe(wsfeed.Subscription{Symbol: sym, Market: "US"}); err != nil {
			t.Fatalf("Subscribe %s: %v", sym, err)
		}
	}
	waitFor(t, time.Second, func() bool { return mock.SubscribeCalls() == 3 })

	mock.Disconnect("simulated drop")
	waitFor(t, time.Second, func() bool {
		for _, s := range mgr.ConnectionsSnapshot() {
			if s.Provider == "mock" && s.State == wsfeed.StateReconnecting {
				return true
			}
		}
		return false
	})

	mock.Reconnect()
	waitFor(t, time.Second, func() bool {
		for _, s := range mgr.ConnectionsSnapshot() {
			if s.Provider == "mock" && s.State == wsfeed.StateConnected {
				return true
			}
		}
		return false
	})

	waitFor(t, time.Second, func() bool { return mock.ResubscribeCalls() >= 1 })
	syms := mock.SubscribedSymbols()
	if len(syms) != 3 {
		t.Fatalf("resubscribed %d symbols, want 3: %v", len(syms), syms)
	}

	var reconnectCount uint64
	for _, s := range mgr.ConnectionsSnapshot() {
		if s.Provider == "mock" {
			reconnectCount = s.ReconnectCount
		}
	}
	if reconnectCount != 1 {
		t.Fatalf("ReconnectCount=%d, want 1", reconnectCount)
	}
}

func TestManagerStopIsIdempotent(t *testing.T) {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	mock := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(mock); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}

	mgr.Stop()
	mgr.Stop()

	if err := mgr.Subscribe(wsfeed.Subscription{Symbol: "AAPL"}); err != wsfeed.ErrManagerStopped {
		t.Fatalf("Subscribe after Stop: %v, want ErrManagerStopped", err)
	}
}

func TestManagerSubscribeWithoutSymbolFails(t *testing.T) {
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{})
	defer mgr.Stop()
	if err := mgr.Subscribe(wsfeed.Subscription{}); err == nil {
		t.Fatalf("Subscribe with empty symbol should error")
	}
}

func TestManagerHandlerPanicDoesNotWedgeDispatcher(t *testing.T) {
	var errs atomic.Uint64
	mgr := wsfeed.NewManager(wsfeed.ManagerConfig{
		OnError: func(error) { errs.Add(1) },
	})
	mock := wsfeedprovider.NewMock("mock")
	if err := mgr.AddProvider(mock); err != nil {
		t.Fatalf("AddProvider: %v", err)
	}
	mgr.AddTickHandler(func(wsfeed.Tick) { panic("boom") })

	var ok atomic.Uint64
	mgr.AddTickHandler(func(wsfeed.Tick) { ok.Add(1) })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := mgr.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer mgr.Stop()
	waitFor(t, time.Second, func() bool { return mock.State() == wsfeed.StateConnected })

	mock.EmitTrade("X", 1, 1)
	mock.EmitTrade("X", 2, 1)

	waitFor(t, time.Second, func() bool { return ok.Load() == 2 })
	if errs.Load() == 0 {
		t.Fatalf("OnError should fire on panic recovery")
	}
}
