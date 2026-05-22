package marketdata

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Test the user-visible resilience contract exposed by Service:
//   1. Stale-quote detection populates IsStale and surfaces an "outdated" note.
//   2. Provider circuit breaker bypasses a known-bad provider until cooldown.
//   3. Provider health counters expose the call totals + latencies.
//
// Each subtest uses an injected fake provider chain (no real network) so the
// behaviour is deterministic and fast.

func TestServiceMarksStaleQuoteAndEmitsNote(t *testing.T) {
	svc := NewService(Config{
		QuoteTTL:        time.Millisecond, // ~immediately expires so we always hit the provider
		StaleQuoteAfter: 10 * time.Minute,
	})

	old := time.Now().UTC().Add(-2 * time.Hour)
	svc.testReplaceQuoteProviders("fake", func(_ context.Context, ref InstrumentRef) (*QuoteSnapshot, error) {
		return &QuoteSnapshot{
			Symbol: ref.NormalizedSymbol(),
			Price:  100,
			AsOf:   old,
		}, nil
	})

	quote, err := svc.GetQuote(context.Background(), InstrumentRef{
		Symbol: "600519",
		Market: "cnstock",
	})
	if err != nil {
		t.Fatalf("GetQuote error: %v", err)
	}
	if !quote.IsStale {
		t.Fatalf("expected IsStale=true for 2-hour-old AsOf, got false")
	}

	note := staleQuoteNote(quote, time.Now().UTC())
	if !strings.HasPrefix(note, "quote outdated") {
		t.Fatalf("stale note = %q, want prefix 'quote outdated'", note)
	}
}

func TestServiceCircuitBreakerOpensThenCloses(t *testing.T) {
	svc := NewService(Config{
		QuoteTTL:                     time.Millisecond,
		QuoteCircuitFailureThreshold: 2,
		QuoteCircuitCooldown:         50 * time.Millisecond,
		StaleQuoteAfter:              15 * time.Minute,
	})

	var calls int32
	svc.testReplaceQuoteProviders("flaky", func(_ context.Context, _ InstrumentRef) (*QuoteSnapshot, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("upstream 503")
	})

	ref := InstrumentRef{Symbol: "AAPL", Market: "usstock"}

	// Two failures should trip the circuit.
	if _, err := svc.GetQuote(context.Background(), ref); err == nil {
		t.Fatalf("expected first call to fail")
	}
	if _, err := svc.GetQuote(context.Background(), ref); err == nil {
		t.Fatalf("expected second call to fail")
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("provider call count = %d, want 2", atomic.LoadInt32(&calls))
	}

	// Third request must skip the provider (circuit open) without invoking
	// it again. We assert the call count stays at 2.
	if _, err := svc.GetQuote(context.Background(), ref); err == nil {
		t.Fatalf("expected third call to fail")
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("provider should be skipped while circuit open; calls = %d, want 2", got)
	}

	// Wait out the cooldown and replace with a healthy provider; the next
	// request should succeed and reset the breaker.
	time.Sleep(80 * time.Millisecond)
	svc.testReplaceQuoteProviders("flaky", func(_ context.Context, ref InstrumentRef) (*QuoteSnapshot, error) {
		return &QuoteSnapshot{Symbol: ref.NormalizedSymbol(), Price: 42, AsOf: time.Now().UTC()}, nil
	})
	quote, err := svc.GetQuote(context.Background(), ref)
	if err != nil {
		t.Fatalf("post-cooldown call should succeed, got err=%v", err)
	}
	if quote.Price != 42 {
		t.Fatalf("expected recovered price 42, got %v", quote.Price)
	}

	stats := svc.ProviderHealth()["flaky"]
	if stats.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures should reset after success, got %d", stats.ConsecutiveFailures)
	}
	// 2 failures while the circuit was closed + 1 success after cooldown.
	// (The 3rd call is short-circuited; it doesn't touch the provider so
	// no counter is incremented there.)
	if stats.TotalSuccesses != 1 || stats.TotalFailures != 2 {
		t.Fatalf("unexpected counters: %+v", stats)
	}
}

// testReplaceQuoteProviders swaps the entire provider chain with a single
// named fake. Test-only helper to keep production resolution untouched.
// The cache is flushed but the providerHealth tracker is preserved so
// circuit-breaker state survives across provider swaps within a test.
func (s *Service) testReplaceQuoteProviders(name string, fn quoteProviderFunc) {
	s.cfg.QuoteProviders = []string{name}
	s.quoteCache = newTTLCache[*QuoteSnapshot]()
	if s.providerHealth == nil {
		s.providerHealth = newProviderHealthTracker(s.cfg.QuoteCircuitFailureThreshold, s.cfg.QuoteCircuitCooldown, s.cfg.QuoteThrottleCooldown)
	}
	s.testProviderOverrides = map[string]quoteProviderFunc{name: fn}
}
