package embedquotaobs

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/fundai/server/internal/embedquota"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestRecorder(t *testing.T, cfg Config, clk Clock) *Recorder {
	t.Helper()
	if clk == nil {
		clk = &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	}
	return NewWithClock(cfg, clk)
}

// TestEmptyFundIDDropped pins the silent-drop contract: anonymous
// batches must not pollute per-fund metrics. If we recorded "" we'd
// see a phantom "no fund" row in dashboards that could mask real
// drift.
func TestEmptyFundIDDropped(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	r.RecordCall("", 100, 50*time.Millisecond)
	r.RecordThrottle("")
	r.RecordExhaust("")

	if got := r.Len(); got != 0 {
		t.Errorf("Len: got %d, want 0 (empty fundID should not allocate a shard)", got)
	}
	if snap := r.Snapshot(); len(snap) != 0 {
		t.Errorf("Snapshot: got %d entries, want 0", len(snap))
	}
}

// TestRecordCallBuildsHistogramAndTokens — the central happy path.
// One fund records two calls; the snapshot must reflect both as
// histogram observations + a tokensToday tally.
func TestRecordCallBuildsHistogramAndTokens(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	r.RecordCall("fund-a", 200, 30*time.Millisecond)
	r.RecordCall("fund-a", 800, 250*time.Millisecond)

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("Snapshot: got %d, want 1", len(snap))
	}
	s := snap[0]
	if s.FundID != "fund-a" {
		t.Errorf("FundID: got %q, want fund-a", s.FundID)
	}
	if s.WaitCount != 2 {
		t.Errorf("WaitCount: got %d, want 2", s.WaitCount)
	}
	if s.TokenCount != 2 {
		t.Errorf("TokenCount: got %d, want 2", s.TokenCount)
	}
	if s.TokenSum != uint64(200+800) {
		t.Errorf("TokenSum: got %d, want 1000", s.TokenSum)
	}
	if s.TokensTodayUsed != 200+800 {
		t.Errorf("TokensTodayUsed: got %d, want 1000", s.TokensTodayUsed)
	}

	// Histogram bucket schedules MUST match the limiter's —
	// drift breaks dashboards that overlay both.
	if len(s.WaitBuckets) != len(embedquota.AcquireWaitBucketsSec()) {
		t.Errorf("wait bucket count: got %d, want %d (must mirror embedquota)",
			len(s.WaitBuckets), len(embedquota.AcquireWaitBucketsSec()))
	}
	if len(s.TokenBuckets) != len(embedquota.RecordTokenBuckets()) {
		t.Errorf("token bucket count: got %d, want %d (must mirror embedquota)",
			len(s.TokenBuckets), len(embedquota.RecordTokenBuckets()))
	}
}

// TestZeroAndNegativeTokensIgnored — RecordCall with 0 / negative
// tokens (failed call, refund) bumps the wait histogram but not
// the token histogram or daily tally. Mirrors the limiter's
// "RecordUsage with positive value only" rule from W10-1.
func TestZeroAndNegativeTokensIgnored(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	r.RecordCall("fund-a", 0, 50*time.Millisecond)
	r.RecordCall("fund-a", -500, 80*time.Millisecond)

	s := r.Snapshot()[0]
	if s.WaitCount != 2 {
		t.Errorf("WaitCount: got %d, want 2", s.WaitCount)
	}
	if s.TokenCount != 0 {
		t.Errorf("TokenCount: got %d, want 0 (zero/negative tokens shouldn't bump)", s.TokenCount)
	}
	if s.TokenSum != 0 {
		t.Errorf("TokenSum: got %d, want 0", s.TokenSum)
	}
	if s.TokensTodayUsed != 0 {
		t.Errorf("TokensTodayUsed: got %d, want 0", s.TokensTodayUsed)
	}
}

// TestThrottleAndExhaustCounters — the dedicated event hooks
// don't go through RecordCall. Verify they bump the right
// counters and update lastSeenAt.
func TestThrottleAndExhaustCounters(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := newTestRecorder(t, Config{}, clk)
	r.RecordThrottle("fund-a")
	r.RecordThrottle("fund-a")
	r.RecordExhaust("fund-a")

	s := r.Snapshot()[0]
	if s.ThrottledTotal != 2 {
		t.Errorf("ThrottledTotal: got %d, want 2", s.ThrottledTotal)
	}
	if s.ExhaustedTotal != 1 {
		t.Errorf("ExhaustedTotal: got %d, want 1", s.ExhaustedTotal)
	}
	// LastSeenAt must reflect the clock at the most recent call.
	if !s.LastSeenAt.Equal(clk.now) {
		t.Errorf("LastSeenAt: got %v, want %v", s.LastSeenAt, clk.now)
	}
}

// TestMaxFundsOverflow — fund 201 onwards must merge into the
// synthetic _overflow shard, not allocate unbounded memory.
func TestMaxFundsOverflow(t *testing.T) {
	r := newTestRecorder(t, Config{MaxFunds: 3}, nil)
	r.RecordCall("fund-1", 10, time.Millisecond)
	r.RecordCall("fund-2", 20, time.Millisecond)
	r.RecordCall("fund-3", 30, time.Millisecond)
	// 4th distinct fund should land in overflow, not break the cap.
	r.RecordCall("fund-4", 40, time.Millisecond)
	r.RecordCall("fund-5", 50, time.Millisecond)

	if got := r.Len(); got != 4 { // fund-1..3 + overflow
		t.Fatalf("Len: got %d, want 4 (3 funds + overflow)", got)
	}
	snap := r.Snapshot()
	overflowFound := false
	for _, s := range snap {
		if s.FundID == OverflowFundID {
			overflowFound = true
			if s.TokenSum != uint64(40+50) {
				t.Errorf("overflow TokenSum: got %d, want 90 (fund-4+fund-5)", s.TokenSum)
			}
		}
	}
	if !overflowFound {
		t.Errorf("expected an _overflow shard in snapshot, got: %v", snap)
	}
}

// TestPrunerEvictsIdleShards — the cardinality safety net. A
// shard idle longer than RetainFor must be evicted on the next
// ManualPrune.
func TestPrunerEvictsIdleShards(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := newTestRecorder(t, Config{RetainFor: 24 * time.Hour}, clk)

	r.RecordCall("fund-a", 100, time.Millisecond)
	clk.advance(12 * time.Hour)
	r.RecordCall("fund-b", 200, time.Millisecond)

	// fund-a hasn't been touched for 12h; not yet evictable.
	if got := r.ManualPrune(); got != 0 {
		t.Errorf("Prune at 12h: got %d evicted, want 0", got)
	}
	if got := r.Len(); got != 2 {
		t.Errorf("Len at 12h: got %d, want 2", got)
	}

	// Advance another 13h — fund-a is now 25h idle, fund-b is 13h.
	// Only fund-a should evict.
	clk.advance(13 * time.Hour)
	if got := r.ManualPrune(); got != 1 {
		t.Errorf("Prune at 25h/13h: got %d, want 1", got)
	}

	snap := r.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 shard remaining, got %d", len(snap))
	}
	if snap[0].FundID != "fund-b" {
		t.Errorf("survivor: got %q, want fund-b", snap[0].FundID)
	}
}

// TestNilSafe — every public method must tolerate a nil receiver
// (matches the limiter's nil-safety convention).
func TestNilSafe(t *testing.T) {
	var r *Recorder
	r.RecordCall("fund-a", 100, time.Second) // must not panic
	r.RecordThrottle("fund-a")
	r.RecordExhaust("fund-a")
	if got := r.Snapshot(); got != nil {
		t.Errorf("nil Snapshot: got %v, want nil", got)
	}
	if got := r.ManualPrune(); got != 0 {
		t.Errorf("nil ManualPrune: got %d, want 0", got)
	}
	if got := r.Len(); got != 0 {
		t.Errorf("nil Len: got %d, want 0", got)
	}
	r.Close() // must not panic
}

// TestConfigNormalisation — clamp behaviour for misuse.
func TestConfigNormalisation(t *testing.T) {
	cases := []struct {
		name string
		in   Config
		want Config
	}{
		{
			name: "all zero → defaults",
			in:   Config{},
			want: Config{MaxFunds: defaultMaxFunds, RetainFor: defaultRetainFor},
		},
		{
			name: "RetainFor below floor → minRetainFor",
			in:   Config{MaxFunds: 50, RetainFor: 5 * time.Minute},
			want: Config{MaxFunds: 50, RetainFor: minRetainFor},
		},
		{
			name: "RetainFor above ceiling → maxRetainFor",
			in:   Config{MaxFunds: 50, RetainFor: 365 * 24 * time.Hour},
			want: Config{MaxFunds: 50, RetainFor: maxRetainFor},
		},
		{
			name: "negative MaxFunds → default",
			in:   Config{MaxFunds: -10, RetainFor: time.Hour},
			want: Config{MaxFunds: defaultMaxFunds, RetainFor: time.Hour},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.in.Normalised()
			if got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestSnapshotIsSortedByFundID — dashboard rendering depends on
// stable order. Test by inserting in non-alphabetical order and
// checking the snapshot comes back sorted.
func TestSnapshotIsSortedByFundID(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	r.RecordCall("zebra", 1, time.Millisecond)
	r.RecordCall("alpha", 1, time.Millisecond)
	r.RecordCall("middle", 1, time.Millisecond)

	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("expected 3, got %d", len(snap))
	}
	want := []string{"alpha", "middle", "zebra"}
	for i, w := range want {
		if snap[i].FundID != w {
			t.Errorf("snap[%d].FundID = %q, want %q", i, snap[i].FundID, w)
		}
	}
}

// TestConcurrentRecordCallsAreAtomic — sanity check that 1k
// concurrent RecordCall on one fund land atomically (no lost
// updates). Catches a future "I forgot to atomic.Add" regression.
func TestConcurrentRecordCallsAreAtomic(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	const n = 1000
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			r.RecordCall("fund-a", 100, time.Millisecond)
		}()
	}
	wg.Wait()

	s := r.Snapshot()[0]
	if s.WaitCount != n {
		t.Errorf("WaitCount: got %d, want %d (lost concurrent updates)", s.WaitCount, n)
	}
	if s.TokenCount != n {
		t.Errorf("TokenCount: got %d, want %d", s.TokenCount, n)
	}
	if s.TokenSum != 100*n {
		t.Errorf("TokenSum: got %d, want %d", s.TokenSum, 100*n)
	}
}

// TestNewRunsPrunerInBackground — production constructor must
// start the pruner; calling Close() must stop it. Without this
// test, dropping the goroutine in a refactor would silently
// blow the cardinality budget in production.
func TestNewRunsPrunerInBackground(t *testing.T) {
	r := New(Config{})
	if r.stopCh == nil {
		t.Errorf("New() should start the pruner; stopCh is nil")
	}
	r.Close()
	if r.stopCh != nil {
		t.Errorf("Close() should clear stopCh; still set")
	}
	// Idempotent.
	r.Close()
}

// TestSnapshotReflectsPostCloseState — Close() stops the pruner
// but leaves the data structure usable for a final read. This
// matches the convention of "Close means stop background work,
// not invalidate the object" used elsewhere in this codebase.
func TestSnapshotReflectsPostCloseState(t *testing.T) {
	r := New(Config{})
	r.RecordCall("fund-a", 100, time.Millisecond)
	r.Close()
	if got := r.Snapshot(); len(got) != 1 {
		t.Errorf("post-Close Snapshot: got %d, want 1", len(got))
	}
}

// Smoke check that bucket schedules really come from the
// canonical embedquota source. If someone replaces
// embedquota.AcquireWaitBucketsSec() with a divergent local
// copy, this catches it.
func TestBucketSchedulesMirrorEmbedquota(t *testing.T) {
	r := newTestRecorder(t, Config{}, nil)
	want := embedquota.AcquireWaitBucketsSec()
	if len(r.waitBuckets) != len(want) {
		t.Fatalf("waitBuckets length: got %d, want %d",
			len(r.waitBuckets), len(want))
	}
	for i, le := range want {
		if r.waitBuckets[i] != le {
			t.Errorf("waitBuckets[%d]: got %v, want %v", i, r.waitBuckets[i], le)
		}
	}
}

// Sanity guard for the day-rollover seam — a second call on a
// new day must not double-count under tokensToday for the
// previous day. Important for the per-fund daily-spend
// dashboard.
func TestTokensTodayResetsOnDayRollover(t *testing.T) {
	clk := &fakeClock{now: time.Date(2026, 6, 1, 23, 30, 0, 0, time.UTC)}
	r := newTestRecorder(t, Config{}, clk)
	r.RecordCall("fund-a", 100, time.Millisecond) // 06-01
	clk.advance(2 * time.Hour)                     // now 06-02
	r.RecordCall("fund-a", 200, time.Millisecond) // 06-02

	s := r.Snapshot()[0]
	// today is 06-02 → only the 200-token call counts in tokensToday.
	if s.TokensTodayUsed != 200 {
		t.Errorf("TokensTodayUsed (06-02): got %d, want 200", s.TokensTodayUsed)
	}
	// But the lifetime sum keeps both.
	if s.TokenSum != 300 {
		t.Errorf("TokenSum (lifetime): got %d, want 300", s.TokenSum)
	}
}

// TestOverflowFundCanBeTouchedDirectly — if a caller explicitly
// records against the overflow ID (programmer error or a future
// "manual cardinality drop" feature), it should land in the
// overflow shard naturally without panicking.
func TestOverflowFundCanBeTouchedDirectly(t *testing.T) {
	r := newTestRecorder(t, Config{MaxFunds: 1}, nil)
	r.RecordCall("real-fund", 1, time.Millisecond)
	r.RecordCall(OverflowFundID, 99, time.Millisecond)

	snap := r.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 shards (real + overflow), got %d", len(snap))
	}
	for _, s := range snap {
		if s.FundID == OverflowFundID && s.TokenSum != 99 {
			t.Errorf("explicit overflow record: got TokenSum=%d, want 99", s.TokenSum)
		}
	}
}

// Demonstrative example for documentation: prove that a 1000-fund
// fuzzed scenario with MaxFunds=10 stays bounded. Belt-and-braces
// against the cardinality bomb.
func TestThousandFundFuzzStaysBounded(t *testing.T) {
	r := newTestRecorder(t, Config{MaxFunds: 10}, nil)
	for i := 0; i < 1000; i++ {
		r.RecordCall(fmt.Sprintf("fuzz-%d", i), 50, time.Millisecond)
	}
	if got := r.Len(); got > 11 { // 10 named + overflow
		t.Errorf("Len bound: got %d, want ≤ 11", got)
	}
}
