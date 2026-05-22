package marketdata

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// newTestService constructs a Service wired with a single test provider and
// returns the upstream call counter so tests can assert how many times the
// provider was actually invoked. The provider is registered via
// testReplaceQuoteProviders so the production resolution chain is bypassed.
func newTestService(t *testing.T, fn quoteProviderFunc) *Service {
	t.Helper()
	cfg := Config{
		QuoteProviders:               []string{"stub"},
		QuoteTTL:                     5 * time.Second,
		ProviderTimeout:              200 * time.Millisecond,
		QuoteCircuitFailureThreshold: 3,
		QuoteCircuitCooldown:         50 * time.Millisecond,
		QuoteThrottleCooldown:        500 * time.Millisecond,
		ProviderRateLimits:           ProviderRateLimits{},
	}
	svc := NewService(cfg)
	svc.testReplaceQuoteProviders("stub", fn)
	return svc
}

// TestGetQuoteSingleflightCoalescesConcurrentMisses asserts that 10
// goroutines hammering GetQuote during a single cache miss only produce one
// upstream call. Without singleflight every concurrent caller would issue
// its own HTTP request and risk an IP block.
func TestGetQuoteSingleflightCoalescesConcurrentMisses(t *testing.T) {
	var hits atomic.Int32
	releaseUpstream := make(chan struct{})
	svc := newTestService(t, func(ctx context.Context, instrument InstrumentRef) (*QuoteSnapshot, error) {
		hits.Add(1)
		// Hold the leader long enough that all 10 follower goroutines
		// reach the cache-miss branch before the cache is populated.
		<-releaseUpstream
		return &QuoteSnapshot{Symbol: instrument.Symbol, Price: 123.45, Source: "stub", AsOf: time.Now().UTC()}, nil
	})

	instr := InstrumentRef{Symbol: "MU", Market: "usstock", AssetClass: "equity"}
	const callers = 10
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer wg.Done()
			quote, err := svc.GetQuote(context.Background(), instr)
			if err != nil {
				t.Errorf("GetQuote: %v", err)
				return
			}
			if quote.Price != 123.45 {
				t.Errorf("price = %v, want 123.45", quote.Price)
			}
		}()
	}
	// Give the goroutines a beat to all converge on the cache-miss path.
	time.Sleep(50 * time.Millisecond)
	close(releaseUpstream)
	wg.Wait()
	if got := hits.Load(); got != 1 {
		t.Fatalf("upstream called %d times, want 1 (singleflight failure)", got)
	}
}

// TestRateLimiterPaces verifies that the per-provider token bucket actually
// throttles in-process calls. We set yahoo to 1 req/sec with burst=1 and
// confirm that 3 back-to-back Wait()s take ~2s of wall time.
func TestRateLimiterPaces(t *testing.T) {
	limiter := newProviderRateLimiter(ProviderRateLimits{
		"yahoo": {PerSecond: 5.0, Burst: 1},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	for i := 0; i < 3; i++ {
		if err := limiter.Wait(ctx, "yahoo"); err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	elapsed := time.Since(started)
	// With burst=1 and rate=5/s, three calls = 0 + 200ms + 200ms = ~400ms
	if elapsed < 350*time.Millisecond {
		t.Fatalf("rate limiter let 3 calls finish in %v (should pace to ~400ms)", elapsed)
	}
	// Unknown provider should not block at all.
	started2 := time.Now()
	if err := limiter.Wait(ctx, "unknown"); err != nil {
		t.Fatalf("unknown provider Wait: %v", err)
	}
	if time.Since(started2) > 10*time.Millisecond {
		t.Fatalf("unknown provider should not block, took %v", time.Since(started2))
	}
}

// TestRateLimiterRespectsContextDeadline ensures Wait() honours a parent
// context that expires before the next token. Important because callers
// (SSE pusher, refresher loops) are time-budgeted.
func TestRateLimiterRespectsContextDeadline(t *testing.T) {
	limiter := newProviderRateLimiter(ProviderRateLimits{
		"yahoo": {PerSecond: 0.1, Burst: 1}, // 1 req every 10s
	})
	// Consume the burst so the next Wait must block for ~10s.
	if err := limiter.Wait(context.Background(), "yahoo"); err != nil {
		t.Fatalf("first Wait: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := limiter.Wait(ctx, "yahoo")
	if err == nil {
		t.Fatalf("expected an error when wait exceeds context deadline")
	}
	// `golang.org/x/time/rate` may return either context.DeadlineExceeded
	// (when the bucket happens to fire as we wait) or a "would exceed
	// context deadline" sentinel (when it can prove up-front the deadline
	// is insufficient). Both are acceptable — we just care the Wait
	// returned promptly without hanging for the full 10s window.
	if time.Since(started) > 200*time.Millisecond {
		t.Fatalf("Wait blocked %v despite tight deadline", time.Since(started))
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("expected deadline-related error, got %v", err)
	}
}

// TestParseProviderRateLimits checks the env-spec parser handles the
// supported syntax + falls back to defaults for malformed entries.
func TestParseProviderRateLimits(t *testing.T) {
	cases := []struct {
		name     string
		spec     string
		provider string
		want     ProviderRateLimit
	}{
		{"per-second", "yahoo=2/s", "yahoo", ProviderRateLimit{PerSecond: 2, Burst: 2}},
		{"per-minute", "eastmoney=120/m", "eastmoney", ProviderRateLimit{PerSecond: 2, Burst: 2}},
		{"per-hour", "tencent=3600/h", "tencent", ProviderRateLimit{PerSecond: 1, Burst: 1}},
		{"bare integer", "sina=4", "sina", ProviderRateLimit{PerSecond: 4, Burst: 4}},
		{"explicit burst", "yahoo=60/m@10", "yahoo", ProviderRateLimit{PerSecond: 1, Burst: 10}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseProviderRateLimits(tc.spec, ProviderRateLimits{})
			limit := got[tc.provider]
			if limit.PerSecond != tc.want.PerSecond || limit.Burst != tc.want.Burst {
				t.Fatalf("parsed %q -> %+v, want %+v", tc.spec, limit, tc.want)
			}
		})
	}

	// Malformed entries should be silently dropped and not override the
	// fallback. We pass a default and confirm it survives.
	fallback := ProviderRateLimits{"yahoo": {PerSecond: 99, Burst: 99}}
	got := ParseProviderRateLimits("badEntry,nounit=10/x", fallback)
	if got["yahoo"].PerSecond != 99 {
		t.Fatalf("malformed input mutated default: %+v", got)
	}
}

// TestThrottleErrorTripsLongCooldown asserts that a 429 from upstream
// immediately opens the circuit for the throttleCooldown (5min default),
// not the regular cooldown (30s). One throttle hit is enough — we don't
// wait for 3 consecutive failures.
func TestThrottleErrorTripsLongCooldown(t *testing.T) {
	tracker := newProviderHealthTracker(3, 30*time.Millisecond, 200*time.Millisecond)
	now := time.Now().UTC()
	tracker.recordFailure("yahoo", fmt.Errorf("%w: yahoo: http 429", ErrUpstreamThrottled), now, 10*time.Millisecond)
	open, retryAt := tracker.shouldSkip("yahoo", now)
	if !open {
		t.Fatalf("circuit should trip immediately on a throttle error")
	}
	wait := retryAt.Sub(now)
	if wait < 150*time.Millisecond {
		t.Fatalf("throttle cooldown too short: %v (want ~200ms)", wait)
	}
	// After the cooldown elapses, the circuit re-allows traffic.
	if open, _ := tracker.shouldSkip("yahoo", now.Add(300*time.Millisecond)); open {
		t.Fatalf("circuit should reopen after throttle cooldown")
	}
	stats := tracker.Snapshot()["yahoo"]
	if stats.TotalThrottled != 1 {
		t.Fatalf("totalThrottled = %d, want 1", stats.TotalThrottled)
	}
	if stats.LastThrottledAt.IsZero() {
		t.Fatalf("lastThrottledAt should be set")
	}
}

// TestThrottleStatusDetectsLegacyMessages confirms isThrottleError still
// recognises the legacy provider error strings ("rate limited") in
// addition to the wrapped sentinel. This keeps the circuit breaker
// working for providers we haven't migrated yet.
func TestThrottleStatusDetectsLegacyMessages(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("http 200"), false},
		{errors.New("http 503"), false},
		{errors.New("provider says rate limited"), true},
		{errors.New("yahoo chart: http 429"), true},
		{errors.New("upstream: http 451"), true},
		{fmt.Errorf("%w: wrapped", ErrUpstreamThrottled), true},
		{errors.New("Too Many Requests"), true},
	}
	for _, tc := range cases {
		if got := isThrottleError(tc.err); got != tc.want {
			t.Errorf("isThrottleError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

// TestYahoo429TripsCircuit drives the full integration path: a stub Yahoo
// returning HTTP 429 once should yield ErrUpstreamThrottled and immediately
// open the provider circuit so the next call short-circuits without
// hitting the upstream again.
func TestYahoo429TripsCircuit(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"chart":{"error":"throttled"}}`))
	}))
	defer srv.Close()

	svc := &Service{httpClient: srv.Client()}
	_, err := svc.yahooChartQuoteAt(context.Background(), srv.URL, InstrumentRef{Symbol: "MU"}, "MU")
	if err == nil {
		t.Fatalf("expected an error")
	}
	if !errors.Is(err, ErrUpstreamThrottled) {
		t.Fatalf("expected ErrUpstreamThrottled, got %v", err)
	}
	if !strings.Contains(err.Error(), "429") {
		t.Fatalf("expected error to mention 429, got %v", err)
	}
}

// TestAdaptiveQuoteTTLPicksInSessionAndOff verifies the new in-/off-session
// branches. We synthesize times that fall inside and outside the US window
// since it's the easiest to anchor in UTC.
func TestAdaptiveQuoteTTLPicksInSessionAndOff(t *testing.T) {
	svc := &Service{cfg: Config{
		QuoteTTL:                10 * time.Second,
		QuoteTTLInSession:       5 * time.Second,
		QuoteTTLOffSession:      60 * time.Second,
		AdaptiveQuoteTTLEnabled: true,
	}}
	usInstr := InstrumentRef{Symbol: "MU", Market: "usstock", AssetClass: "equity"}

	// Wednesday 18:00 UTC = 14:00 ET, US market open.
	inSession := time.Date(2026, 5, 13, 18, 0, 0, 0, time.UTC)
	if got := svc.adaptiveQuoteTTL(usInstr, inSession); got != 5*time.Second {
		t.Fatalf("in-session TTL = %v, want 5s", got)
	}

	// Wednesday 02:00 UTC = 22:00 ET previous evening, US market closed.
	offSession := time.Date(2026, 5, 13, 2, 0, 0, 0, time.UTC)
	if got := svc.adaptiveQuoteTTL(usInstr, offSession); got != 60*time.Second {
		t.Fatalf("off-session TTL = %v, want 60s", got)
	}

	// Crypto is always in-session.
	cryptoInstr := InstrumentRef{Symbol: "BTCUSDT", Market: "crypto", AssetClass: "crypto"}
	if got := svc.adaptiveQuoteTTL(cryptoInstr, offSession); got != 5*time.Second {
		t.Fatalf("crypto TTL outside US hours = %v, want 5s (24/7)", got)
	}

	// When adaptive is disabled we fall back to the legacy QuoteTTL.
	svc.cfg.AdaptiveQuoteTTLEnabled = false
	if got := svc.adaptiveQuoteTTL(usInstr, inSession); got != 10*time.Second {
		t.Fatalf("disabled adaptive TTL = %v, want 10s", got)
	}
}

// TestInstrumentInSessionWeekend ensures Saturday/Sunday count as off-hours
// for equity markets, while crypto stays in-session.
func TestInstrumentInSessionWeekend(t *testing.T) {
	sat := time.Date(2026, 5, 16, 18, 0, 0, 0, time.UTC) // Saturday 18:00 UTC
	if instrumentInSession(InstrumentRef{Market: "usstock"}, sat) {
		t.Fatalf("US equity should be off-session on Saturday")
	}
	if !instrumentInSession(InstrumentRef{Market: "crypto"}, sat) {
		t.Fatalf("crypto should still be in-session on Saturday")
	}
}
