package earnings

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingFetcher records the number of upstream calls so we
// can verify the cache actually hits.
type countingFetcher struct {
	calls  int64
	events []Event
	err    error
}

func (f *countingFetcher) Fetch(_ context.Context, _ FetchRequest) ([]Event, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out, nil
}

type countingHistoryFetcher struct {
	calls  int64
	events []HistoricalEvent
	err    error
}

func (f *countingHistoryFetcher) FetchHistory(_ context.Context, _ HistoryRequest) ([]HistoricalEvent, error) {
	atomic.AddInt64(&f.calls, 1)
	if f.err != nil {
		return nil, f.err
	}
	out := make([]HistoricalEvent, len(f.events))
	copy(out, f.events)
	return out, nil
}

// ---------------------------------------------------------------------------
// Forward cache
// ---------------------------------------------------------------------------

func TestForwardCacheServesHitFromMemory(t *testing.T) {
	upstream := &countingFetcher{
		events: []Event{
			{Symbol: "AAPL", EventDate: time.Now().Add(48 * time.Hour)},
		},
	}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	req := FetchRequest{Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14}
	for i := 0; i < 5; i++ {
		events, err := cache.Fetch(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(events) != 1 || events[0].Symbol != "AAPL" {
			t.Fatalf("call %d: bad payload %+v", i, events)
		}
	}
	if got := atomic.LoadInt64(&upstream.calls); got != 1 {
		t.Errorf("expected exactly 1 upstream call, got %d", got)
	}
}

func TestForwardCacheKeyNormalisesSymbolOrder(t *testing.T) {
	// Same set of symbols in different order MUST share an entry.
	upstream := &countingFetcher{events: []Event{{Symbol: "A"}}}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL", "MSFT", "GOOG"}, Market: "us_equity", HorizonDays: 14,
	})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"GOOG", "MSFT", "AAPL"}, Market: "us_equity", HorizonDays: 14,
	})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"msft", "aapl", "goog"}, Market: "us_equity", HorizonDays: 14,
	})
	if got := atomic.LoadInt64(&upstream.calls); got != 1 {
		t.Errorf("expected exactly 1 upstream call (sorted+upper-cased dedupes), got %d", got)
	}
}

func TestForwardCacheKeyDiffersByMarketAndHorizon(t *testing.T) {
	upstream := &countingFetcher{events: []Event{{Symbol: "A"}}}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14,
	})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL"}, Market: "hk_equity", HorizonDays: 14,
	})
	_, _ = cache.Fetch(context.Background(), FetchRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 30,
	})
	if got := atomic.LoadInt64(&upstream.calls); got != 3 {
		t.Errorf("expected 3 distinct upstream calls (market+horizon vary), got %d", got)
	}
}

func TestForwardCacheExpiresAfterTTL(t *testing.T) {
	upstream := &countingFetcher{events: []Event{{Symbol: "A"}}}
	cache := NewCache(upstream, CacheOptions{TTL: 50 * time.Millisecond})
	req := FetchRequest{Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14}
	_, _ = cache.Fetch(context.Background(), req)
	if got := atomic.LoadInt64(&upstream.calls); got != 1 {
		t.Fatalf("expected 1 call before sleep, got %d", got)
	}
	time.Sleep(80 * time.Millisecond)
	_, _ = cache.Fetch(context.Background(), req)
	if got := atomic.LoadInt64(&upstream.calls); got != 2 {
		t.Errorf("expected 2 calls after TTL expiry, got %d", got)
	}
}

func TestForwardCachePurgeForcesRefresh(t *testing.T) {
	upstream := &countingFetcher{events: []Event{{Symbol: "A"}}}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	req := FetchRequest{Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14}
	_, _ = cache.Fetch(context.Background(), req)
	cache.Purge()
	_, _ = cache.Fetch(context.Background(), req)
	if got := atomic.LoadInt64(&upstream.calls); got != 2 {
		t.Errorf("expected 2 calls (one before+after Purge), got %d", got)
	}
}

func TestForwardCacheSingleflightCollapses(t *testing.T) {
	// 100 concurrent identical-request callers should collapse
	// to 1 upstream call. We slow the upstream so the in-flight
	// window covers all callers.
	upstream := &slowCountingFetcher{
		delay:  100 * time.Millisecond,
		events: []Event{{Symbol: "A"}},
	}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	req := FetchRequest{Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14}
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = cache.Fetch(context.Background(), req)
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&upstream.calls); got != 1 {
		t.Errorf("singleflight failed: expected 1 upstream call for 100 concurrent, got %d", got)
	}
}

func TestForwardCacheReturnsCopyNotReference(t *testing.T) {
	// Callers must not be able to mutate the cached slice.
	upstream := &countingFetcher{
		events: []Event{{Symbol: "AAPL", EventDate: time.Now()}},
	}
	cache := NewCache(upstream, CacheOptions{TTL: time.Hour})
	req := FetchRequest{Symbols: []string{"AAPL"}, Market: "us_equity", HorizonDays: 14}
	got, _ := cache.Fetch(context.Background(), req)
	got[0].Symbol = "MUTATED"
	again, _ := cache.Fetch(context.Background(), req)
	if again[0].Symbol != "AAPL" {
		t.Errorf("cache returned the same slice — mutation leaked into entry: %v", again)
	}
}

func TestForwardCacheNilUpstreamReturnsEmpty(t *testing.T) {
	cache := NewCache(nil, CacheOptions{})
	got, err := cache.Fetch(context.Background(), FetchRequest{Symbols: []string{"A"}})
	if err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if got != nil {
		t.Errorf("nil upstream should yield nil events, got %+v", got)
	}
}

// ---------------------------------------------------------------------------
// History cache (same surface as forward; lighter coverage since
// the parallel structure makes regressions obvious).
// ---------------------------------------------------------------------------

func TestHistoryCacheServesHitFromMemory(t *testing.T) {
	upstream := &countingHistoryFetcher{
		events: []HistoricalEvent{{Symbol: "AAPL", EventDate: time.Now().AddDate(0, 0, -10)}},
	}
	cache := NewHistoryCache(upstream, CacheOptions{TTL: time.Hour})
	req := HistoryRequest{Symbols: []string{"AAPL"}, Market: "us_equity", LookbackDays: 60}
	for i := 0; i < 4; i++ {
		events, err := cache.FetchHistory(context.Background(), req)
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if len(events) != 1 || events[0].Symbol != "AAPL" {
			t.Fatalf("call %d: bad payload %+v", i, events)
		}
	}
	if got := atomic.LoadInt64(&upstream.calls); got != 1 {
		t.Errorf("expected exactly 1 upstream call, got %d", got)
	}
}

func TestHistoryCacheDefaultTTLIs24Hours(t *testing.T) {
	c := NewHistoryCache(nil, CacheOptions{})
	if c.ttl != 24*time.Hour {
		t.Errorf("default history TTL = %v, want 24h", c.ttl)
	}
}

func TestForwardCacheDefaultTTLIs6Hours(t *testing.T) {
	c := NewCache(nil, CacheOptions{})
	if c.ttl != 6*time.Hour {
		t.Errorf("default forward TTL = %v, want 6h", c.ttl)
	}
}

func TestHistoryCacheKeyDiffersByLookback(t *testing.T) {
	upstream := &countingHistoryFetcher{events: []HistoricalEvent{{Symbol: "A"}}}
	cache := NewHistoryCache(upstream, CacheOptions{TTL: time.Hour})
	_, _ = cache.FetchHistory(context.Background(), HistoryRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", LookbackDays: 60,
	})
	_, _ = cache.FetchHistory(context.Background(), HistoryRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", LookbackDays: 90,
	})
	if got := atomic.LoadInt64(&upstream.calls); got != 2 {
		t.Errorf("expected 2 distinct upstream calls (lookback varies), got %d", got)
	}
}

func TestHistoryCacheUpstreamErrorPropagates(t *testing.T) {
	upstream := &countingHistoryFetcher{err: errTest}
	cache := NewHistoryCache(upstream, CacheOptions{TTL: time.Hour})
	_, err := cache.FetchHistory(context.Background(), HistoryRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", LookbackDays: 60,
	})
	if err == nil {
		t.Error("expected upstream error to propagate")
	}
	// Errors are NOT cached — second call hits upstream again.
	_, _ = cache.FetchHistory(context.Background(), HistoryRequest{
		Symbols: []string{"AAPL"}, Market: "us_equity", LookbackDays: 60,
	})
	if got := atomic.LoadInt64(&upstream.calls); got != 2 {
		t.Errorf("errors must not be cached; got %d calls, want 2", got)
	}
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

type slowCountingFetcher struct {
	calls  int64
	delay  time.Duration
	events []Event
}

func (f *slowCountingFetcher) Fetch(ctx context.Context, _ FetchRequest) ([]Event, error) {
	atomic.AddInt64(&f.calls, 1)
	select {
	case <-time.After(f.delay):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	out := make([]Event, len(f.events))
	copy(out, f.events)
	return out, nil
}

var errTest = &stringError{msg: "test-only error"}

type stringError struct{ msg string }

func (e *stringError) Error() string { return e.msg }
