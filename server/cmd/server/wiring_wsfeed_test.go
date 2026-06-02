package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/fundai/server/internal/marketdata"
	"github.com/fundai/server/internal/matching"
	"github.com/fundai/server/internal/quotecache"
)

type fakeWSMetrics struct{ events map[string]int }

func newFakeWSMetrics() *fakeWSMetrics { return &fakeWSMetrics{events: map[string]int{}} }

func (m *fakeWSMetrics) RecordWSFeedEvent(event string) { m.events[event]++ }

func TestNewCacheAwareQuoteFnHitsCacheWhenFresh(t *testing.T) {
	now := time.Now().UTC()
	cache := quotecache.New(quotecache.Config{StaleAfter: 5 * time.Second})
	cache.Apply(quotecache.Tick{
		Symbol:    "AAPL",
		EventKind: "trade",
		Last:      210.50,
		Bid:       210.40,
		Ask:       210.60,
		Timestamp: now,
	})
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		t.Fatalf("fallback should not be called on fresh cache hit")
		return matching.Quote{}, nil
	}
	m := newFakeWSMetrics()
	fn := newCacheAwareQuoteFn(cache, fallback, m)
	q, err := fn(context.Background(), "AAPL", "AAPL", "US")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if q.Last != 210.50 || q.Bid != 210.40 || q.Ask != 210.60 {
		t.Fatalf("unexpected quote: %+v", q)
	}
	if m.events["quote_cache_hit"] != 1 {
		t.Fatalf("metric not recorded: %+v", m.events)
	}
}

func TestNewCacheAwareQuoteFnFallsBackOnMiss(t *testing.T) {
	cache := quotecache.New(quotecache.Config{StaleAfter: 5 * time.Second})
	called := false
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		called = true
		return matching.Quote{Last: 100}, nil
	}
	m := newFakeWSMetrics()
	fn := newCacheAwareQuoteFn(cache, fallback, m)
	q, err := fn(context.Background(), "TSLA", "TSLA", "US")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called || q.Last != 100 {
		t.Fatalf("fallback not used: %+v", q)
	}
	if m.events["quote_miss_fallback_ok"] != 1 {
		t.Fatalf("metric not recorded: %+v", m.events)
	}
}

func TestNewCacheAwareQuoteFnFallsBackOnStale(t *testing.T) {
	now := time.Now().UTC()
	cache := quotecache.New(quotecache.Config{StaleAfter: 1 * time.Second})
	cache.Apply(quotecache.Tick{Symbol: "MSFT", EventKind: "trade", Last: 410, Timestamp: now.Add(-10 * time.Second)})
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		return matching.Quote{Last: 420}, nil
	}
	m := newFakeWSMetrics()
	fn := newCacheAwareQuoteFn(cache, fallback, m)
	q, err := fn(context.Background(), "MSFT", "MSFT", "US")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if q.Last != 420 {
		t.Fatalf("expected fallback price 420, got %v", q.Last)
	}
	if m.events["quote_stale_fallback_ok"] != 1 {
		t.Fatalf("metric not recorded: %+v", m.events)
	}
}

func TestNewCacheAwareQuoteFnServesStaleWhenFallbackErrors(t *testing.T) {
	now := time.Now().UTC()
	cache := quotecache.New(quotecache.Config{StaleAfter: 1 * time.Second})
	cache.Apply(quotecache.Tick{Symbol: "MSFT", EventKind: "trade", Last: 410, Timestamp: now.Add(-10 * time.Second)})
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		return matching.Quote{}, marketdata.ErrQuoteUnavailable
	}
	m := newFakeWSMetrics()
	fn := newCacheAwareQuoteFn(cache, fallback, m)
	q, err := fn(context.Background(), "MSFT", "MSFT", "US")
	if err != nil {
		t.Fatalf("err: %v (expected stale value, not error)", err)
	}
	if q.Last != 410 {
		t.Fatalf("expected stale 410, got %v", q.Last)
	}
	if m.events["quote_stale_served_on_error"] != 1 {
		t.Fatalf("metric not recorded: %+v", m.events)
	}
}

func TestNewCacheAwareQuoteFnPropagatesErrorOnHardMiss(t *testing.T) {
	cache := quotecache.New(quotecache.Config{})
	wantErr := errors.New("boom")
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		return matching.Quote{}, wantErr
	}
	m := newFakeWSMetrics()
	fn := newCacheAwareQuoteFn(cache, fallback, m)
	_, err := fn(context.Background(), "NONE", "NONE", "US")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err=%v, want %v", err, wantErr)
	}
	if m.events["quote_miss_fallback_err"] != 1 {
		t.Fatalf("metric not recorded: %+v", m.events)
	}
}

func TestNewCacheAwareQuoteFnNilCacheUsesFallback(t *testing.T) {
	called := false
	fallback := func(context.Context, string, string, string) (matching.Quote, error) {
		called = true
		return matching.Quote{Last: 1}, nil
	}
	fn := newCacheAwareQuoteFn(nil, fallback, newFakeWSMetrics())
	if _, err := fn(context.Background(), "X", "X", "US"); err != nil {
		t.Fatalf("err: %v", err)
	}
	if !called {
		t.Fatalf("fallback should be invoked when cache is nil")
	}
}

func TestWSFeedConfigFromEnvDefaults(t *testing.T) {
	cfg := wsFeedConfigFromEnv(func(string) string { return "" })
	if cfg.Enabled {
		t.Fatalf("default Enabled should be false")
	}
	if len(cfg.ProviderNames) != 1 || cfg.ProviderNames[0] != "nop" {
		t.Fatalf("default ProviderNames=%v, want [nop]", cfg.ProviderNames)
	}
	if cfg.CacheStaleAfter != 10*time.Second {
		t.Fatalf("default CacheStaleAfter=%v", cfg.CacheStaleAfter)
	}
}

func TestWSFeedConfigFromEnvParsesProviders(t *testing.T) {
	env := map[string]string{
		"WSFEED_ENABLED":   "true",
		"WSFEED_PROVIDERS": "mock,nop",
	}
	cfg := wsFeedConfigFromEnv(func(k string) string { return env[k] })
	if !cfg.Enabled {
		t.Fatalf("Enabled should be true")
	}
	if len(cfg.ProviderNames) != 2 || cfg.ProviderNames[0] != "mock" || cfg.ProviderNames[1] != "nop" {
		t.Fatalf("ProviderNames=%v", cfg.ProviderNames)
	}
}
