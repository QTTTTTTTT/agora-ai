package embedquota

import (
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

func TestAcquireOKByDefault(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	wait, status, err := l.Acquire(100)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if wait != 0 {
		t.Errorf("wait: got %v, want 0", wait)
	}
	if status != StatusOK {
		t.Errorf("status: got %q, want ok", status)
	}
}

func TestAcquireThrottlesWhenRateExceeded(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MaxCallsPerMinute = 3
	l := NewWithClock(cfg, clk)
	for i := 0; i < 3; i++ {
		_, _, _ = l.Acquire(10)
	}
	wait, status, err := l.Acquire(10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if status != StatusThrottled {
		t.Errorf("status: got %q, want throttled", status)
	}
	if wait <= 0 {
		t.Errorf("wait: got %v, want > 0", wait)
	}
}

func TestAcquireExhaustsQuota(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.TokenQuotaPerDay = 100
	l := NewWithClock(cfg, clk)
	l.RecordUsage(95)
	wait, status, err := l.Acquire(20)
	if !errors.Is(err, ErrQuotaExhausted) {
		t.Errorf("expected ErrQuotaExhausted, got %v", err)
	}
	if status != StatusExhausted {
		t.Errorf("status: got %q, want exhausted", status)
	}
	if wait <= 0 {
		t.Errorf("wait: got %v, want > 0 (until midnight)", wait)
	}
}

func TestAcquireNearLimit(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.TokenQuotaPerDay = 100
	cfg.SoftLimitFraction = 0.80
	l := NewWithClock(cfg, clk)
	l.RecordUsage(70)
	_, status, err := l.Acquire(20) // projected = 90 → near limit
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if status != StatusNearLimit {
		t.Errorf("status: got %q, want near_limit", status)
	}
}

func TestSlidingWindowExpiresOldCalls(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MaxCallsPerMinute = 2
	l := NewWithClock(cfg, clk)
	_, _, _ = l.Acquire(10)
	_, _, _ = l.Acquire(10)
	clk.advance(2 * time.Minute)
	_, status, err := l.Acquire(10)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if status == StatusThrottled {
		t.Errorf("after 2min, expected status != throttled, got %q", status)
	}
}

func TestRecordUsageNegativeReleases(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	l.RecordUsage(50)
	l.RecordUsage(-30)
	snap := l.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snap: got %d, want 1", len(snap))
	}
	if snap[0].Tokens != 20 {
		t.Errorf("tokens: got %d, want 20", snap[0].Tokens)
	}
}

func TestRecordUsageDoesNotGoNegative(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	l.RecordUsage(10)
	l.RecordUsage(-100)
	snap := l.Snapshot()
	if len(snap) != 1 || snap[0].Tokens != 0 {
		t.Errorf("expected 0 tokens after over-release, got %+v", snap)
	}
}

func TestSnapshotPerDay(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	l.RecordUsage(50)
	clk.advance(24 * time.Hour)
	l.RecordUsage(70)
	snap := l.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snap: got %d, want 2", len(snap))
	}
	if snap[0].Day >= snap[1].Day {
		t.Errorf("snap should be sorted ascending")
	}
}

func TestCallsPerMinute(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	for i := 0; i < 5; i++ {
		_, _, _ = l.Acquire(10)
	}
	if got := l.CallsPerMinute(); got != 5 {
		t.Errorf("CallsPerMinute: got %d, want 5", got)
	}
}

func TestNilLimiterReturnsUnavailable(t *testing.T) {
	var l *Limiter
	_, status, err := l.Acquire(10)
	if err == nil {
		t.Errorf("nil Limiter Acquire should error")
	}
	if status != StatusUnavailable {
		t.Errorf("status: got %q, want unavailable", status)
	}
}

func TestConfigNormalisation(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(Config{}, clk) // all zero
	_, _, err := l.Acquire(10)
	if err != nil {
		t.Errorf("normalised config should not error: %v", err)
	}
}

// TestHealthSnapshotCountsThrottleAndExhaustEvents pins the W8-1
// counters: every Acquire call that hits the per-minute rate
// limit increments ThrottledTotal, and every call rejected for
// being over the daily quota increments ExhaustedTotal. The
// counters are independent — a single Acquire can only land in
// one branch.
func TestHealthSnapshotCountsThrottleAndExhaustEvents(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MaxCallsPerMinute = 2
	cfg.TokenQuotaPerDay = 100
	l := NewWithClock(cfg, clk)

	// Two clean acquires fill the per-minute window.
	for i := 0; i < 2; i++ {
		if _, status, err := l.Acquire(5); err != nil || status != StatusOK {
			t.Fatalf("warm-up Acquire #%d: status=%q err=%v", i, status, err)
		}
	}
	// Third call must throttle and bump the counter.
	if _, status, _ := l.Acquire(5); status != StatusThrottled {
		t.Fatalf("expected throttled, got %q", status)
	}
	if got := l.HealthSnapshot().ThrottledTotal; got != 1 {
		t.Errorf("ThrottledTotal after 1 throttle: got %d, want 1", got)
	}

	// A second throttle must add to the counter, not reset it.
	if _, status, _ := l.Acquire(5); status != StatusThrottled {
		t.Fatalf("expected throttled (#2), got %q", status)
	}
	if got := l.HealthSnapshot().ThrottledTotal; got != 2 {
		t.Errorf("ThrottledTotal after 2 throttles: got %d, want 2", got)
	}

	// Skip past the rate window so we can exercise the exhaust path
	// without colliding with throttle. Then push usage above the
	// daily cap.
	clk.advance(2 * time.Minute)
	l.RecordUsage(95)
	if _, status, err := l.Acquire(20); !errors.Is(err, ErrQuotaExhausted) || status != StatusExhausted {
		t.Fatalf("expected exhausted, got status=%q err=%v", status, err)
	}
	h := l.HealthSnapshot()
	if h.ExhaustedTotal != 1 {
		t.Errorf("ExhaustedTotal: got %d, want 1", h.ExhaustedTotal)
	}
	// Throttle counter stays where it was — exhaust doesn't double-count.
	if h.ThrottledTotal != 2 {
		t.Errorf("ThrottledTotal stable across exhaust: got %d, want 2", h.ThrottledTotal)
	}
}

// TestHealthSnapshotZeroCountersOnFreshLimiter pins the contract
// that a brand-new Limiter exposes zero-valued counters (so a
// fresh deploy doesn't trigger absent-series alerts on the
// rate(...) panel).
func TestHealthSnapshotZeroCountersOnFreshLimiter(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	h := l.HealthSnapshot()
	if h.ThrottledTotal != 0 || h.ExhaustedTotal != 0 {
		t.Errorf("fresh limiter should have zero counters, got throttled=%d exhausted=%d",
			h.ThrottledTotal, h.ExhaustedTotal)
	}
}

// TestWaitHistogramFreshLimiterIsZeroButSchemaCorrect pins that
// a never-called limiter still surfaces every bucket boundary at
// count=0. Otherwise a Prometheus scrape immediately after boot
// would have *no* series for the histogram and the dashboard
// panel would render "no data", indistinguishable from "scrape
// broken".
func TestWaitHistogramFreshLimiterIsZeroButSchemaCorrect(t *testing.T) {
	l := New(DefaultConfig())
	hist := l.WaitHistogram()
	if hist.Count != 0 || hist.SumSeconds != 0 {
		t.Errorf("fresh limiter should have zero count/sum, got count=%d sum=%v",
			hist.Count, hist.SumSeconds)
	}
	if len(hist.Buckets) != len(acquireWaitBucketsSec) {
		t.Fatalf("bucket count mismatch: got %d want %d",
			len(hist.Buckets), len(acquireWaitBucketsSec))
	}
	for i, b := range hist.Buckets {
		if b.LeSeconds != acquireWaitBucketsSec[i] {
			t.Errorf("bucket[%d] le mismatch: got %v want %v",
				i, b.LeSeconds, acquireWaitBucketsSec[i])
		}
		if b.Count != 0 {
			t.Errorf("bucket[%d] should be zero on fresh limiter, got %d", i, b.Count)
		}
	}
}

// TestWaitHistogramNilSafe pins the contract that the export path
// (which calls .WaitHistogram() on a nil pointer when the limiter
// is disabled) gets back a schema-correct empty snapshot rather
// than crashing.
func TestWaitHistogramNilSafe(t *testing.T) {
	var l *Limiter
	hist := l.WaitHistogram()
	if hist.Count != 0 || hist.SumSeconds != 0 {
		t.Errorf("nil limiter should have zero count/sum, got %+v", hist)
	}
	if len(hist.Buckets) != len(acquireWaitBucketsSec) {
		t.Fatalf("nil limiter should still report %d buckets, got %d",
			len(acquireWaitBucketsSec), len(hist.Buckets))
	}
}

// TestWaitHistogramRecordsZeroAndThrottledObservations pins the
// joint contract: an ungated Acquire records a 0-wait sample
// (every bucket gets +1), and a throttled Acquire records a
// non-zero wait that lands in the appropriate bucket. We also
// assert that count and sum stay consistent — they're the two
// things histogram_quantile() depends on.
func TestWaitHistogramRecordsZeroAndThrottledObservations(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.MaxCallsPerMinute = 2
	cfg.TokenQuotaPerDay = 1_000_000
	l := NewWithClock(cfg, clk)

	if _, _, err := l.Acquire(10); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, _, err := l.Acquire(10); err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	wait, status, err := l.Acquire(10)
	if err != nil {
		t.Fatalf("third Acquire: %v", err)
	}
	if status != StatusThrottled {
		t.Fatalf("expected throttled, got %v", status)
	}
	if wait <= 0 {
		t.Fatalf("expected non-zero wait, got %v", wait)
	}

	hist := l.WaitHistogram()
	if hist.Count != 3 {
		t.Errorf("expected 3 observations, got %d", hist.Count)
	}
	// First two waits are 0 — they hit every bucket. Third is
	// ~60s (oldest call + 1 minute - now), which is above the
	// 30s bucket but at/below the 600s bucket.
	if got := hist.Buckets[0].Count; got != 2 {
		t.Errorf("0.001s bucket should hold the two zero-wait obs, got %d", got)
	}
	// Bucket index 8 is le=30 — must NOT have caught the
	// 60-second observation.
	if got := hist.Buckets[8].Count; got != 2 {
		t.Errorf("30s bucket should still be 2 (60s wait does not fit), got %d", got)
	}
	// Bucket index 9 is le=600 — must catch the 60s wait.
	if got := hist.Buckets[9].Count; got != 3 {
		t.Errorf("600s bucket should hold all 3 observations, got %d", got)
	}
	if hist.SumSeconds < 59 || hist.SumSeconds > 61 {
		t.Errorf("sum should be ≈60s, got %v", hist.SumSeconds)
	}
}

// TestTokenHistogramFreshLimiterIsZeroButSchemaCorrect mirrors
// the wait-histogram contract: a fresh deploy must surface every
// bucket boundary at count=0 so Prometheus doesn't see the
// series "appear" later and treat the gap as backfill.
func TestTokenHistogramFreshLimiterIsZeroButSchemaCorrect(t *testing.T) {
	l := New(DefaultConfig())
	hist := l.TokenHistogram()
	if hist.Count != 0 || hist.Sum != 0 {
		t.Errorf("fresh limiter should have zero count/sum, got count=%d sum=%d",
			hist.Count, hist.Sum)
	}
	if len(hist.Buckets) != len(recordTokenBuckets) {
		t.Fatalf("bucket count mismatch: got %d want %d",
			len(hist.Buckets), len(recordTokenBuckets))
	}
	for i, b := range hist.Buckets {
		if b.Le != recordTokenBuckets[i] {
			t.Errorf("bucket[%d] le mismatch: got %v want %v",
				i, b.Le, recordTokenBuckets[i])
		}
		if b.Count != 0 {
			t.Errorf("bucket[%d] should be zero on fresh limiter, got %d", i, b.Count)
		}
	}
}

// TestTokenHistogramNilSafe matches the Wait sibling — the
// export path calls .TokenHistogram() on a nil limiter when
// the limiter is disabled and must get back a schema-correct
// snapshot, not a panic.
func TestTokenHistogramNilSafe(t *testing.T) {
	var l *Limiter
	hist := l.TokenHistogram()
	if hist.Count != 0 || hist.Sum != 0 {
		t.Errorf("nil limiter should have zero count/sum, got %+v", hist)
	}
	if len(hist.Buckets) != len(recordTokenBuckets) {
		t.Fatalf("nil limiter should still report %d buckets, got %d",
			len(recordTokenBuckets), len(hist.Buckets))
	}
}

// TestTokenHistogramRecordsPositiveObservationsOnly is the
// joint contract on RecordUsage:
//   - positive observations are placed in the right bucket and
//     accumulate Sum/Count;
//   - negative observations (reservation refunds) are NOT
//     counted — including them would make "p99 call size"
//     meaningless for capacity planning.
func TestTokenHistogramRecordsPositiveObservationsOnly(t *testing.T) {
	l := New(DefaultConfig())
	l.RecordUsage(45)    // → 50 bucket
	l.RecordUsage(150)   // → 200 bucket
	l.RecordUsage(2_500) // → 8000 bucket
	l.RecordUsage(-100)  // refund — must NOT be observed
	l.RecordUsage(60_000) // → 100000 bucket

	hist := l.TokenHistogram()
	if hist.Count != 4 {
		t.Errorf("expected 4 positive observations, got %d", hist.Count)
	}
	if hist.Sum != uint64(45+150+2_500+60_000) {
		t.Errorf("sum should ignore refunds, got %d", hist.Sum)
	}
	// Bucket 0 = le=50: holds the 45-token sample.
	if hist.Buckets[0].Count != 1 {
		t.Errorf("le=50 should hold 1 obs, got %d", hist.Buckets[0].Count)
	}
	// Bucket 1 = le=200: holds 45 + 150 cumulatively.
	if hist.Buckets[1].Count != 2 {
		t.Errorf("le=200 should hold 2 obs, got %d", hist.Buckets[1].Count)
	}
	// Bucket 4 = le=8000: holds 45 + 150 + 2500.
	if hist.Buckets[4].Count != 3 {
		t.Errorf("le=8000 should hold 3 obs, got %d", hist.Buckets[4].Count)
	}
	// Bucket 6 = le=100000: holds all 4.
	if hist.Buckets[6].Count != 4 {
		t.Errorf("le=100000 should hold all 4 obs, got %d", hist.Buckets[6].Count)
	}
}

// TestTokenHistogramIgnoresZeroObservation pins that
// RecordUsage(0) — which can happen on a fully-cancelled
// reservation — does not contribute a sample. Treating it as a
// real call would inflate Count without inflating Sum, making
// p99 collapse toward zero.
func TestTokenHistogramIgnoresZeroObservation(t *testing.T) {
	l := New(DefaultConfig())
	l.RecordUsage(0)
	hist := l.TokenHistogram()
	if hist.Count != 0 {
		t.Errorf("zero observation should be ignored, got count=%d", hist.Count)
	}
}

// TestWaitHistogramClampsExhaustedQuotaWait pins the
// recordWaitLocked clamp: when Acquire returns a 24h
// "wait until midnight" duration on quota exhaustion, the
// histogram does NOT add 86400 seconds to the running sum.
// Otherwise a single quota-exhaust event at 00:00 UTC would
// permanently destroy the dashboard's p99 estimate.
func TestWaitHistogramClampsExhaustedQuotaWait(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)}
	cfg := DefaultConfig()
	cfg.TokenQuotaPerDay = 100
	l := NewWithClock(cfg, clk)

	wait, status, err := l.Acquire(1_000)
	if status != StatusExhausted {
		t.Fatalf("expected exhausted, got %v err=%v", status, err)
	}
	if wait < 23*time.Hour {
		t.Fatalf("expected ≈24h wait, got %v", wait)
	}

	hist := l.WaitHistogram()
	if hist.Count != 1 {
		t.Errorf("expected 1 observation, got %d", hist.Count)
	}
	if hist.SumSeconds > acquireWaitBucketCap.Seconds()+1 {
		t.Errorf("sum should be clamped to <= %vs, got %v",
			acquireWaitBucketCap.Seconds(), hist.SumSeconds)
	}
}

// TestRecentDaysReturnsExactlyN — the contract the Admin UI
// sparkline depends on: the slice length is always n regardless
// of how many days the limiter has actually observed. A consumer
// that needs to render 7 bars cannot tolerate a variable-length
// reply.
func TestRecentDaysReturnsExactlyN(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)
	l.RecordUsage(100) // only one day observed

	got := l.RecentDays(7)
	if len(got) != 7 {
		t.Fatalf("expected 7 entries, got %d", len(got))
	}

	// Last entry must be today; missing days zero-padded.
	if got[6].Day != "2026-06-01" {
		t.Errorf("last day: got %q, want 2026-06-01", got[6].Day)
	}
	if got[6].Tokens != 100 {
		t.Errorf("today tokens: got %d, want 100", got[6].Tokens)
	}
	for i := 0; i < 6; i++ {
		if got[i].Tokens != 0 {
			t.Errorf("day[%d] (%s) should be zero-padded, got %d",
				i, got[i].Day, got[i].Tokens)
		}
	}
}

// TestRecentDaysOrderingAndPadding — across multiple recorded
// days, RecentDays must place the oldest first and today last,
// preserve every observed value, and zero-fill gaps in between.
// Without this, a sparkline drawn left-to-right would mis-attribute
// usage to the wrong day.
func TestRecentDaysOrderingAndPadding(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)

	l.RecordUsage(100) // day 06-01
	clk.advance(24 * time.Hour)
	l.RecordUsage(200) // day 06-02
	clk.advance(48 * time.Hour) // skip 06-03
	l.RecordUsage(300) // day 06-04 (now today)

	got := l.RecentDays(5) // 05-31, 06-01, 06-02, 06-03, 06-04
	if len(got) != 5 {
		t.Fatalf("expected 5, got %d", len(got))
	}
	want := []DaySnapshot{
		{Day: "2026-05-31", Tokens: 0},
		{Day: "2026-06-01", Tokens: 100},
		{Day: "2026-06-02", Tokens: 200},
		{Day: "2026-06-03", Tokens: 0},
		{Day: "2026-06-04", Tokens: 300},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("day[%d]: got %+v, want %+v", i, got[i], w)
		}
	}
}

// TestRecentDaysNilSafeAndBounds — a nil receiver and any n <= 0
// must come back with a nil slice rather than a panic. n is also
// clamped to 366 so a misuse can't allocate an absurd slice and
// hang the handler.
func TestRecentDaysNilSafeAndBounds(t *testing.T) {
	var nilL *Limiter
	if got := nilL.RecentDays(7); got != nil {
		t.Errorf("nil receiver: got %v, want nil", got)
	}

	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	l := NewWithClock(DefaultConfig(), clk)

	if got := l.RecentDays(0); got != nil {
		t.Errorf("n=0: got %v, want nil", got)
	}
	if got := l.RecentDays(-3); got != nil {
		t.Errorf("n=-3: got %v, want nil", got)
	}

	got := l.RecentDays(10_000) // clamped to 366
	if len(got) != 366 {
		t.Errorf("n=10000 should clamp to 366, got %d", len(got))
	}
}
