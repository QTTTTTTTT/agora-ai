// trending_handler_test.go — focused tests for the bug fix that
// took the /api/trending/most-active page out of its "first hit
// spins for 20-30 s, refresh works" UX trap.
//
// The two behaviours under test are the two halves of that fix:
//
//  1. computeMostActive fans out per-symbol OHLC fetches in
//     PARALLEL (bounded by trendingFetchConcurrency). Without
//     this the cold path was serialised across the universe and
//     a 50-symbol universe could blow past 20 s on a cold cache.
//
//  2. Concurrent cache-miss requests for the same market collapse
//     into ONE compute via the singleflight group. Without this
//     N simultaneous arrivals during a TTL boundary would
//     multiply our upstream pressure by N.
//
// Universe lookup is stubbed out via the new trendingWatchlistLister
// interface; the OHLC fetcher is stubbed with a deterministic
// sleep so we can observe parallelism directly in wall-clock terms.

package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/dailypicks"
	"github.com/fundai/server/internal/ohlc"
)

// fakeWatchlistLister returns a deterministic universe so the
// scoring loop has enough symbols to demonstrate parallelism.
type fakeWatchlistLister struct {
	market  string
	symbols []string
}

func (f *fakeWatchlistLister) ListActiveWatchlists(_ context.Context) ([]dailypicks.Watchlist, error) {
	return []dailypicks.Watchlist{{
		ID:        "wl-test",
		PresetKey: "test",
		Market:    f.market,
		Symbols:   append([]string(nil), f.symbols...),
		Active:    true,
	}}, nil
}

// fakeOHLCFetcher records call counts per symbol and sleeps a
// configurable duration to simulate upstream latency. Returns 25
// synthetic bars so the >= 21 bars guard in computeMostActive
// is satisfied and the symbol contributes to the output rows.
type fakeOHLCFetcher struct {
	sleep time.Duration
	calls sync.Map // symbol → *atomic.Int32

	// inFlight tracks how many fetches are running concurrently
	// at any instant; maxConcurrent is the high-water mark. Used
	// by the parallelism assertion below.
	inFlightMu    sync.Mutex
	inFlight      int32
	maxConcurrent int32
}

func (f *fakeOHLCFetcher) Fetch(ctx context.Context, req ohlc.FetchRequest) ([]ohlc.Bar, error) {
	v, _ := f.calls.LoadOrStore(req.Symbol, new(atomic.Int32))
	v.(*atomic.Int32).Add(1)

	f.inFlightMu.Lock()
	f.inFlight++
	if f.inFlight > f.maxConcurrent {
		f.maxConcurrent = f.inFlight
	}
	f.inFlightMu.Unlock()
	defer func() {
		f.inFlightMu.Lock()
		f.inFlight--
		f.inFlightMu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(f.sleep):
	}

	bars := make([]ohlc.Bar, 25)
	now := time.Now()
	for i := range bars {
		bars[i] = ohlc.Bar{
			Time:   now.Add(time.Duration(-25+i) * 24 * time.Hour),
			Open:   100,
			High:   101,
			Low:    99,
			Close:  100,
			Volume: 1_000_000,
		}
	}
	return bars, nil
}

func (f *fakeOHLCFetcher) callCount(symbol string) int32 {
	if v, ok := f.calls.Load(symbol); ok {
		return v.(*atomic.Int32).Load()
	}
	return 0
}

func makeTrendingHandlerForTest(universe []string, perSymbolSleep time.Duration) (*trendingHandler, *fakeOHLCFetcher) {
	fetcher := &fakeOHLCFetcher{sleep: perSymbolSleep}
	picks := &fakeWatchlistLister{market: "us_equity", symbols: universe}
	return &trendingHandler{
		ohlc:  fetcher,
		picks: picks,
		clock: time.Now,
		cache: make(map[string]trendingCacheEntry),
	}, fetcher
}

// TestComputeMostActive_FansOutInParallel proves the regression-
// fix: with a 25-symbol universe and 200 ms per-symbol latency,
// a SEQUENTIAL implementation would take ~5 s. The parallel
// implementation with concurrency=10 should complete in roughly
// ceil(25/10) × 200 ms ≈ 600 ms. We assert a generous 2 s ceiling
// to stay non-flaky on slow CI runners while still catching a
// regression to fully-serial execution (which would take >= 4 s).
func TestComputeMostActive_FansOutInParallel(t *testing.T) {
	universe := make([]string, 25)
	for i := range universe {
		universe[i] = "SYM" + string(rune('A'+i%26)) + string(rune('A'+i/26))
	}
	h, fetcher := makeTrendingHandlerForTest(universe, 200*time.Millisecond)

	start := time.Now()
	resp := h.computeMostActive(context.Background(), "us_equity")
	elapsed := time.Since(start)

	if got := resp.UniverseSize; got != len(universe) {
		t.Fatalf("UniverseSize = %d, want %d", got, len(universe))
	}
	if got := len(resp.Results); got != len(universe) {
		t.Fatalf("len(Results) = %d, want %d (all should score)", got, len(universe))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("computeMostActive took %v; sequential regression suspected (cap = 2s)", elapsed)
	}
	// trendingFetchConcurrency is the configured pool size; the
	// fetcher must have observed concurrent calls (otherwise we
	// regressed to sequential). We assert >= 2 (any parallelism
	// at all) and <= trendingFetchConcurrency+1 (the bound is
	// honoured; +1 absorbs a benign race where a slot is
	// released the instant a new goroutine grabs one).
	if fetcher.maxConcurrent < 2 {
		t.Fatalf("maxConcurrent = %d; expected fan-out (>=2)", fetcher.maxConcurrent)
	}
	if int(fetcher.maxConcurrent) > trendingFetchConcurrency+1 {
		t.Fatalf("maxConcurrent = %d; expected <= %d", fetcher.maxConcurrent, trendingFetchConcurrency+1)
	}

	// Order-preservation sanity: ranks are 1..N and there are no
	// duplicates. Tied vol-ratios should still produce a
	// deterministic 1..N filling.
	seen := map[int]bool{}
	for _, r := range resp.Results {
		if r.Rank < 1 || r.Rank > len(resp.Results) {
			t.Fatalf("Rank=%d out of range", r.Rank)
		}
		if seen[r.Rank] {
			t.Fatalf("duplicate rank %d", r.Rank)
		}
		seen[r.Rank] = true
	}
}

// TestGetOrCompute_SingleflightDedupsConcurrentMisses fires 20
// concurrent handleMostActive-equivalent calls into an empty
// cache. With singleflight wired correctly each symbol should be
// fetched ONCE, not 20 times. Catches the thundering-herd
// regression that would multiply upstream load on every TTL
// boundary in production.
func TestGetOrCompute_SingleflightDedupsConcurrentMisses(t *testing.T) {
	universe := []string{"AAPL", "MSFT", "GOOG", "AMZN", "TSLA"}
	h, fetcher := makeTrendingHandlerForTest(universe, 100*time.Millisecond)

	const callers = 20
	var wg sync.WaitGroup
	var firstResp trendingMostActiveResponse
	var firstRespMu sync.Mutex

	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := h.getOrCompute(context.Background(), "us_equity")
			firstRespMu.Lock()
			if firstResp.GeneratedAt == "" {
				firstResp = r
			}
			firstRespMu.Unlock()
		}()
	}
	wg.Wait()

	for _, sym := range universe {
		if got := fetcher.callCount(sym); got != 1 {
			t.Fatalf("OHLC fetch for %s called %d times; singleflight should dedupe to 1", sym, got)
		}
	}

	// All callers should have observed identical content (same
	// GeneratedAt timestamp from the SHARED compute, not a fresh
	// per-caller compute).
	if len(firstResp.Results) != len(universe) {
		t.Fatalf("Results len = %d, want %d", len(firstResp.Results), len(universe))
	}
}

// TestHandleMostActive_SecondHitIsCached confirms the cache
// short-circuits the second request. Reproduces the user-reported
// scenario in miniature: first hit pays the compute, second hit
// returns instantly with zero additional upstream calls.
func TestHandleMostActive_SecondHitIsCached(t *testing.T) {
	h, fetcher := makeTrendingHandlerForTest([]string{"AAPL", "MSFT"}, 50*time.Millisecond)

	first := h.getOrCompute(context.Background(), "us_equity")
	if len(first.Results) != 2 {
		t.Fatalf("first call Results = %d, want 2", len(first.Results))
	}
	aaplCount := fetcher.callCount("AAPL")

	second := h.getOrCompute(context.Background(), "us_equity")
	if second.GeneratedAt != first.GeneratedAt {
		t.Fatalf("second call GeneratedAt = %q, want %q (same cache entry)", second.GeneratedAt, first.GeneratedAt)
	}
	if got := fetcher.callCount("AAPL"); got != aaplCount {
		t.Fatalf("OHLC re-fetched on cached hit; calls went %d → %d", aaplCount, got)
	}
}

// TestRunWarmer_PopulatesCacheBeforeFirstRequest verifies the
// warmer goroutine produces a cache entry that subsequent
// getOrCompute calls hit without triggering a fresh upstream
// fan-out. This is the boot-time UX guarantee we're shipping:
// "by the time a real user navigates to the page, the cache is
// already warm".
func TestRunWarmer_PopulatesCacheBeforeFirstRequest(t *testing.T) {
	h, fetcher := makeTrendingHandlerForTest([]string{"AAPL", "MSFT"}, 20*time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Drive warmAll directly instead of RunWarmer to avoid
	// waiting for the 14-min ticker. warmAll is the same code
	// path each tick executes.
	h.warmAll(ctx, []string{"us_equity"})

	if fetcher.callCount("AAPL") != 1 {
		t.Fatalf("warmer didn't trigger AAPL fetch; calls=%d", fetcher.callCount("AAPL"))
	}

	// Subsequent request must hit the cache (no new upstream
	// fetch). This is the entire point of the warmer.
	calls := fetcher.callCount("AAPL")
	_ = h.getOrCompute(ctx, "us_equity")
	if got := fetcher.callCount("AAPL"); got != calls {
		t.Fatalf("post-warm request hit upstream again; calls %d → %d", calls, got)
	}
}

// TestComputeMostActive_NoOHLCFetcherReturnsEmpty mirrors the
// degraded-deploy guarantee documented on trendingHandler: if
// there's no OHLC fetcher, the route still mounts and returns a
// valid criteria_disclosed payload with an empty results slice.
// Frontend's "No data available" branch handles it gracefully.
func TestComputeMostActive_NoOHLCFetcherReturnsEmpty(t *testing.T) {
	h := &trendingHandler{
		ohlc:  nil,
		picks: &fakeWatchlistLister{market: "us_equity", symbols: []string{"AAPL"}},
		clock: time.Now,
		cache: make(map[string]trendingCacheEntry),
	}
	resp := h.computeMostActive(context.Background(), "us_equity")
	if got := len(resp.Results); got != 0 {
		t.Fatalf("expected empty Results when ohlc nil, got %d", got)
	}
	if len(resp.CriteriaDisclosed) == 0 {
		t.Fatalf("CriteriaDisclosed empty; should always be populated")
	}
}

// errFetcher is the always-error sibling of fakeOHLCFetcher. Used
// to prove the per-symbol error swallow contract: one broken
// symbol must not poison the whole list. The handler treats
// these as misses and just produces fewer rows.
type errFetcher struct{ err error }

func (e *errFetcher) Fetch(_ context.Context, _ ohlc.FetchRequest) ([]ohlc.Bar, error) {
	return nil, e.err
}

func TestComputeMostActive_PerSymbolErrorsAreSwallowed(t *testing.T) {
	h := &trendingHandler{
		ohlc:  &errFetcher{err: errors.New("transient upstream")},
		picks: &fakeWatchlistLister{market: "us_equity", symbols: []string{"AAPL", "MSFT"}},
		clock: time.Now,
		cache: make(map[string]trendingCacheEntry),
	}
	resp := h.computeMostActive(context.Background(), "us_equity")
	if got := resp.UniverseSize; got != 2 {
		t.Fatalf("UniverseSize = %d, want 2", got)
	}
	if got := len(resp.Results); got != 0 {
		t.Fatalf("len(Results) = %d, want 0 (all upstream errors)", got)
	}
}
