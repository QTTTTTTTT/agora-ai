package marketdata

import (
	"errors"
	"testing"
	"time"
)

func TestIsQuoteStale(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		asOf   time.Time
		maxAge time.Duration
		want   bool
	}{
		{"fresh", now.Add(-5 * time.Minute), 15 * time.Minute, false},
		{"borderline", now.Add(-15 * time.Minute), 15 * time.Minute, false},
		{"clearly stale", now.Add(-31 * time.Minute), 15 * time.Minute, true},
		{"zero asOf disables check", time.Time{}, 15 * time.Minute, false},
		{"non-positive maxAge disables check", now.Add(-1 * time.Hour), 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isQuoteStale(tc.asOf, now, tc.maxAge)
			if got != tc.want {
				t.Fatalf("isQuoteStale(%v, %v, %v) = %v, want %v", tc.asOf, now, tc.maxAge, got, tc.want)
			}
		})
	}
}

func TestProviderHealthTrackerCircuitOpenAndClose(t *testing.T) {
	tracker := newProviderHealthTracker(3, 100*time.Millisecond, 0)
	now := time.Now().UTC()

	// First two failures should keep the circuit closed.
	tracker.recordFailure("tencent", errors.New("503"), now, 12*time.Millisecond)
	tracker.recordFailure("tencent", errors.New("503"), now, 11*time.Millisecond)
	if open, _ := tracker.shouldSkip("tencent", now); open {
		t.Fatalf("circuit unexpectedly open after 2 failures")
	}

	// Third failure trips it.
	tracker.recordFailure("tencent", errors.New("503"), now, 13*time.Millisecond)
	open, retryAt := tracker.shouldSkip("tencent", now)
	if !open {
		t.Fatalf("circuit should be open after %d failures", 3)
	}
	if !retryAt.After(now) {
		t.Fatalf("retryAt = %v should be after now %v", retryAt, now)
	}

	// After cooldown elapses, it should allow retries again.
	if open, _ := tracker.shouldSkip("tencent", now.Add(time.Second)); open {
		t.Fatalf("circuit should reopen after cooldown")
	}

	// A successful call resets consecutive failures + clears openUntil.
	tracker.recordSuccess("tencent", 5*time.Millisecond)
	stats := tracker.Snapshot()["tencent"]
	if stats.ConsecutiveFailures != 0 {
		t.Fatalf("consecutive failures = %d, want 0 after success", stats.ConsecutiveFailures)
	}
	if !stats.CircuitOpenUntil.IsZero() {
		t.Fatalf("openUntil should be zero after success, got %v", stats.CircuitOpenUntil)
	}
	if stats.TotalSuccesses != 1 || stats.TotalFailures != 3 || stats.TotalCalls != 4 {
		t.Fatalf("unexpected counters: %+v", stats)
	}
	if stats.LastLatencyMs <= 0 {
		t.Fatalf("expected non-zero last latency, got %d", stats.LastLatencyMs)
	}
	if stats.EMALatencyMs <= 0 {
		t.Fatalf("expected non-zero EMA latency, got %d", stats.EMALatencyMs)
	}
}

func TestProviderHealthTrackerNilSafe(t *testing.T) {
	var tracker *providerHealthTracker
	open, _ := tracker.shouldSkip("x", time.Now())
	if open {
		t.Fatalf("nil tracker should never open the circuit")
	}
	tracker.recordSuccess("x", time.Millisecond)
	tracker.recordFailure("x", errors.New("err"), time.Now(), time.Millisecond)
	if got := tracker.Snapshot(); got != nil {
		t.Fatalf("nil tracker snapshot should be nil, got %v", got)
	}
}

func TestProviderHealthTrackerEMASmoothing(t *testing.T) {
	tracker := newProviderHealthTracker(3, time.Second, 0)
	tracker.recordSuccess("yahoo", 100*time.Millisecond)
	tracker.recordSuccess("yahoo", 200*time.Millisecond)
	tracker.recordSuccess("yahoo", 1000*time.Millisecond)
	stats := tracker.Snapshot()["yahoo"]
	// EMA = 100, then alpha*200+0.8*100 = 120, then alpha*1000+0.8*120 = 296
	if stats.EMALatencyMs < 200 || stats.EMALatencyMs > 350 {
		t.Fatalf("ema %dms should be smoothed between recent samples", stats.EMALatencyMs)
	}
	if stats.LastLatencyMs != 1000 {
		t.Fatalf("last latency = %dms, want 1000", stats.LastLatencyMs)
	}
}

func TestStaleQuoteNoteFormats(t *testing.T) {
	now := time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		quote  *QuoteSnapshot
		expect string
	}{
		{"nil quote", nil, "quote outdated"},
		{"sub-hour", &QuoteSnapshot{AsOf: now.Add(-45 * time.Minute)}, "quote outdated (age: 45m0s)"},
		{"hours", &QuoteSnapshot{AsOf: now.Add(-2*time.Hour - 15*time.Minute)}, "quote outdated (age: 2h15m0s)"},
		{"days", &QuoteSnapshot{AsOf: now.Add(-3 * 24 * time.Hour)}, "quote outdated (age: 3d)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := staleQuoteNote(tc.quote, now)
			if got != tc.expect {
				t.Fatalf("staleQuoteNote = %q, want %q", got, tc.expect)
			}
		})
	}
}

func TestAdaptiveNewsTTL(t *testing.T) {
	svc := NewService(Config{NewsTTL: 2 * time.Minute, AdaptiveTTLEnabled: true})

	active := time.Date(2026, 5, 18, 8, 0, 0, 0, time.UTC) // Monday 08:00 UTC, intersects EU+Asia
	if got := svc.adaptiveNewsTTL(active); got != 2*time.Minute {
		t.Fatalf("active hour TTL = %v, want 2m", got)
	}

	offHours := time.Date(2026, 5, 18, 22, 0, 0, 0, time.UTC) // Monday 22:00 UTC, after US close
	if got := svc.adaptiveNewsTTL(offHours); got != 6*time.Minute {
		t.Fatalf("off-hours TTL = %v, want 6m (3x base)", got)
	}

	weekend := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC) // Sunday
	if got := svc.adaptiveNewsTTL(weekend); got != 6*time.Minute {
		t.Fatalf("weekend TTL = %v, want 6m (3x base)", got)
	}

	svc2 := NewService(Config{NewsTTL: 2 * time.Minute, AdaptiveTTLEnabled: false})
	if got := svc2.adaptiveNewsTTL(weekend); got != 2*time.Minute {
		t.Fatalf("disabled flag should fall back to base TTL, got %v", got)
	}
}

func TestAdaptiveNewsTTLCapsAtTenMinutes(t *testing.T) {
	svc := NewService(Config{NewsTTL: 5 * time.Minute, AdaptiveTTLEnabled: true})
	weekend := time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
	if got := svc.adaptiveNewsTTL(weekend); got != 10*time.Minute {
		t.Fatalf("ttl should cap at 10m, got %v", got)
	}
}
