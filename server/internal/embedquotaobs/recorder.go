// Package embedquotaobs is the side-car observability layer
// for embedquota.Limiter (W13-7).
//
// This package adds per-fund slicing to the existing
// process-global metrics WITHOUT changing the limiter's API or
// limiting semantics. See docs/PER_FUND_EMBEDQUOTA_OBSERVABILITY.md
// for the full design rationale and the reasoning behind picking
// "side-car" (Option B) over the alternatives.
//
// What ships in W13-7:
//
//   - Recorder data structure (this file).
//   - Tests (recorder_test.go).
//
// What is deliberately NOT in W13-7:
//
//   - Wiring into Acquire / RecordUsage call sites. Those PRs
//     get done one call site at a time once the data structure
//     is reviewed.
//   - Prometheus exporter (`exportEmbedQuotaPerFundPrometheus`).
//   - Admin JSON endpoint.
//
// The intent is "the data structure exists and is provably
// correct; mechanical wire-in lands in subsequent waves with
// minimal design discussion."
package embedquotaobs

import (
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fundai/server/internal/embedquota"
)

// Clock is the test seam. Production uses wallClock; the
// pruner test uses a fake clock to validate eviction without
// real time passing.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// Config tunes the recorder. Zero values resolve to
// production-safe defaults via Normalised().
type Config struct {
	// MaxFunds caps how many distinct fund shards live at once.
	// New funds beyond the cap are merged into a synthetic
	// "_overflow" shard. Defaults to 200.
	MaxFunds int

	// RetainFor evicts shards that haven't recorded an
	// observation in the given window. Caps cardinality even
	// when funds are created and abandoned. Defaults to 7 days,
	// clamped to [1h, 90d].
	RetainFor time.Duration
}

const (
	defaultMaxFunds  = 200
	defaultRetainFor = 7 * 24 * time.Hour
	minRetainFor     = time.Hour
	maxRetainFor     = 90 * 24 * time.Hour

	// OverflowFundID names the synthetic shard that absorbs
	// fund IDs beyond MaxFunds. Exported so callers / dashboard
	// authors can recognise it.
	OverflowFundID = "_overflow"
)

// Normalised fills zero / out-of-range fields with defaults.
// Always called by New so callers can pass Config{} and get
// sane behaviour.
func (c Config) Normalised() Config {
	out := c
	if out.MaxFunds <= 0 {
		out.MaxFunds = defaultMaxFunds
	}
	if out.RetainFor <= 0 {
		out.RetainFor = defaultRetainFor
	}
	if out.RetainFor < minRetainFor {
		out.RetainFor = minRetainFor
	}
	if out.RetainFor > maxRetainFor {
		out.RetainFor = maxRetainFor
	}
	return out
}

// fundShard holds the per-fund counters. Access pattern:
//
//   - shard pointer is allocated under Recorder.mu and never
//     freed mid-flight (eviction unlinks from the map but the
//     shard itself becomes garbage when no goroutine holds it).
//   - All counter mutations are atomic.
//   - Day rollover for tokensToday uses a dedicated mu.
type fundShard struct {
	// Lifetime totals (process start → now). Atomic.
	throttledTotal uint64
	exhaustedTotal uint64
	waitCount      uint64
	waitSumNanos   uint64
	tokenCount     uint64
	tokenSum       uint64

	// Histogram bucket cumulative counts, parallel to
	// embedquota.AcquireWaitBucketsSec / RecordTokenBuckets.
	// Atomic.
	waitBuckets  []uint64
	tokenBuckets []uint64

	// Day-keyed token tally — separate from tokenSum which is
	// process-lifetime. Used for "today's per-fund spend"
	// dashboards. Mutated under mu so day rollover is atomic
	// against reads.
	mu          sync.Mutex
	tokensByDay map[string]int

	// lastSeenAt is the most recent RecordCall observation.
	// Used by the pruner to evict idle shards. UnixNano so it
	// can be updated atomically without grabbing mu.
	lastSeenAtNanos int64
}

// Recorder is the side-car. Cheap to construct; safe for
// concurrent use.
type Recorder struct {
	cfg          Config
	clock        Clock
	waitBuckets  []float64
	tokenBuckets []float64

	mu     sync.RWMutex
	shards map[string]*fundShard

	// stopCh signals the pruner goroutine to exit. nil when
	// pruning is disabled (e.g. in unit tests using ManualPrune).
	stopCh chan struct{}
}

// New constructs a Recorder with a wall clock and starts the
// pruner goroutine. Call Close() to stop the pruner cleanly.
func New(cfg Config) *Recorder {
	r := newRecorder(cfg.Normalised(), wallClock{})
	r.startPruner()
	return r
}

// NewWithClock is the test-only constructor. Does not start
// the pruner — call ManualPrune() from tests instead.
func NewWithClock(cfg Config, clk Clock) *Recorder {
	return newRecorder(cfg.Normalised(), clk)
}

func newRecorder(cfg Config, clk Clock) *Recorder {
	return &Recorder{
		cfg:          cfg,
		clock:        clk,
		waitBuckets:  embedquota.AcquireWaitBucketsSec(),
		tokenBuckets: embedquota.RecordTokenBuckets(),
		shards:       make(map[string]*fundShard),
	}
}

// Close stops the pruner goroutine. Safe to call multiple times
// and on a recorder constructed with NewWithClock (no-op).
func (r *Recorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopCh != nil {
		close(r.stopCh)
		r.stopCh = nil
	}
}

func (r *Recorder) startPruner() {
	r.stopCh = make(chan struct{})
	go func(stop <-chan struct{}) {
		// Prune cadence is RetainFor / 8 — gives ~hourly cycles
		// at the 7-day default. Cheap; the prune itself is O(N)
		// in the shard count.
		interval := r.cfg.RetainFor / 8
		if interval < time.Minute {
			interval = time.Minute
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-t.C:
				r.ManualPrune()
			}
		}
	}(r.stopCh)
}

// RecordCall is the per-call hook. fundID may be "" — empty IDs
// are silently dropped so anonymous batches don't pollute the
// per-fund slice. tokensActual is post-call; pass 0 if the call
// failed before producing a token count. waitDuration is the
// time spent in Acquire (limiter wait), not the request RTT.
//
// Recorder being nil is a no-op (matches the limiter's
// nil-safety convention).
func (r *Recorder) RecordCall(fundID string, tokensActual int, waitDuration time.Duration) {
	if r == nil {
		return
	}
	if fundID == "" {
		return
	}

	shard := r.shardFor(fundID)
	if shard == nil {
		return
	}
	now := r.clock.Now().UTC()
	atomic.StoreInt64(&shard.lastSeenAtNanos, now.UnixNano())

	if waitDuration > 0 {
		r.recordWait(shard, waitDuration)
	} else {
		// Even zero-wait calls bump the count + smallest bucket
		// so the histogram total reflects total successful
		// Acquires, matching the limiter's convention.
		r.recordWait(shard, 0)
	}
	if tokensActual > 0 {
		r.recordTokens(shard, tokensActual, now)
	}
}

// RecordThrottle / RecordExhaust are bumped from the limiter's
// status path — caller threads the fund context. Splitting these
// from RecordCall keeps the wire signatures small and makes the
// no-fund-context-yet call sites easy to identify in `git grep`.
func (r *Recorder) RecordThrottle(fundID string) {
	if r == nil || fundID == "" {
		return
	}
	if shard := r.shardFor(fundID); shard != nil {
		atomic.AddUint64(&shard.throttledTotal, 1)
		atomic.StoreInt64(&shard.lastSeenAtNanos, r.clock.Now().UTC().UnixNano())
	}
}

func (r *Recorder) RecordExhaust(fundID string) {
	if r == nil || fundID == "" {
		return
	}
	if shard := r.shardFor(fundID); shard != nil {
		atomic.AddUint64(&shard.exhaustedTotal, 1)
		atomic.StoreInt64(&shard.lastSeenAtNanos, r.clock.Now().UTC().UnixNano())
	}
}

// shardFor returns the shard for fundID, allocating it on
// first sight. Honours MaxFunds by funnelling overflow into a
// synthetic shard. Lock discipline: holds mu in write mode only
// when a new shard is created.
func (r *Recorder) shardFor(fundID string) *fundShard {
	r.mu.RLock()
	if s, ok := r.shards[fundID]; ok {
		r.mu.RUnlock()
		return s
	}
	r.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	// Double-check after upgrading the lock — another goroutine
	// may have created the shard while we were waiting.
	if s, ok := r.shards[fundID]; ok {
		return s
	}
	if len(r.shards) >= r.cfg.MaxFunds && fundID != OverflowFundID {
		// Recurse with the overflow ID; the recursion is
		// guaranteed to terminate because MaxFunds covers at
		// least one slot for OverflowFundID once it exists.
		// Release the lock first since shardFor relocks.
		// Defer is fine since defer doesn't run until return.
		// But re-entering shardFor while holding mu would
		// deadlock — so we read/write directly here.
		if existing, ok := r.shards[OverflowFundID]; ok {
			return existing
		}
		s := r.newShard()
		r.shards[OverflowFundID] = s
		return s
	}
	s := r.newShard()
	r.shards[fundID] = s
	return s
}

func (r *Recorder) newShard() *fundShard {
	return &fundShard{
		waitBuckets:  make([]uint64, len(r.waitBuckets)),
		tokenBuckets: make([]uint64, len(r.tokenBuckets)),
		tokensByDay:  make(map[string]int),
	}
}

func (r *Recorder) recordWait(shard *fundShard, d time.Duration) {
	if d < 0 {
		d = 0
	}
	atomic.AddUint64(&shard.waitCount, 1)
	// Clamp the same way the limiter does — see embedquota.go's
	// recordWaitLocked rationale (one 24h "exhausted" wait
	// shouldn't poison the running average).
	const cap = 600 * time.Second
	bumpD := d
	if bumpD > cap {
		bumpD = cap
	}
	atomic.AddUint64(&shard.waitSumNanos, uint64(bumpD.Nanoseconds()))
	v := bumpD.Seconds()
	for i, le := range r.waitBuckets {
		if v <= le {
			atomic.AddUint64(&shard.waitBuckets[i], 1)
		}
	}
}

func (r *Recorder) recordTokens(shard *fundShard, tokens int, now time.Time) {
	atomic.AddUint64(&shard.tokenCount, 1)
	atomic.AddUint64(&shard.tokenSum, uint64(tokens))
	v := float64(tokens)
	for i, le := range r.tokenBuckets {
		if v <= le {
			atomic.AddUint64(&shard.tokenBuckets[i], 1)
		}
	}
	day := now.Format("2006-01-02")
	shard.mu.Lock()
	shard.tokensByDay[day] += tokens
	shard.mu.Unlock()
}

// FundSnapshot is the read-side wire shape for one fund.
type FundSnapshot struct {
	FundID          string         `json:"fundId"`
	ThrottledTotal  uint64         `json:"throttledTotal"`
	ExhaustedTotal  uint64         `json:"exhaustedTotal"`
	WaitCount       uint64         `json:"waitCount"`
	WaitSumSeconds  float64        `json:"waitSumSeconds"`
	TokenCount      uint64         `json:"tokenCount"`
	TokenSum        uint64         `json:"tokenSum"`
	TokensTodayUsed int            `json:"tokensTodayUsed"`
	WaitBuckets     []BucketCount  `json:"waitBuckets"`
	TokenBuckets    []BucketCount  `json:"tokenBuckets"`
	LastSeenAt      time.Time      `json:"lastSeenAt"`
}

// BucketCount carries (le, cumulative count) — same shape as
// embedquota's WaitBucket / TokenBucket but unified here so a
// generic sparkline / heatmap renderer can consume both.
type BucketCount struct {
	Le    float64 `json:"le"`
	Count uint64  `json:"count"`
}

// Snapshot returns a stable, sorted snapshot of every active
// fund. Sort is by fundID ascending so dashboards have stable
// row order. The OverflowFundID (when present) sorts wherever
// "_overflow" lands lexicographically — its leading underscore
// pushes it to the top, which is the right UX (operators want
// the cardinality alarm above the noise).
func (r *Recorder) Snapshot() []FundSnapshot {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	out := make([]FundSnapshot, 0, len(r.shards))
	today := r.clock.Now().UTC().Format("2006-01-02")
	for fundID, shard := range r.shards {
		out = append(out, fundSnapshot(fundID, shard, r.waitBuckets, r.tokenBuckets, today))
	}
	r.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].FundID < out[j].FundID })
	return out
}

func fundSnapshot(fundID string, shard *fundShard, waitLes, tokenLes []float64, today string) FundSnapshot {
	wait := make([]BucketCount, len(waitLes))
	for i, le := range waitLes {
		wait[i] = BucketCount{Le: le, Count: atomic.LoadUint64(&shard.waitBuckets[i])}
	}
	token := make([]BucketCount, len(tokenLes))
	for i, le := range tokenLes {
		token[i] = BucketCount{Le: le, Count: atomic.LoadUint64(&shard.tokenBuckets[i])}
	}
	shard.mu.Lock()
	tokensToday := shard.tokensByDay[today]
	shard.mu.Unlock()
	waitSumSec := float64(atomic.LoadUint64(&shard.waitSumNanos)) / 1e9
	lastSeen := time.Unix(0, atomic.LoadInt64(&shard.lastSeenAtNanos)).UTC()
	return FundSnapshot{
		FundID:          fundID,
		ThrottledTotal:  atomic.LoadUint64(&shard.throttledTotal),
		ExhaustedTotal:  atomic.LoadUint64(&shard.exhaustedTotal),
		WaitCount:       atomic.LoadUint64(&shard.waitCount),
		WaitSumSeconds:  waitSumSec,
		TokenCount:      atomic.LoadUint64(&shard.tokenCount),
		TokenSum:        atomic.LoadUint64(&shard.tokenSum),
		TokensTodayUsed: tokensToday,
		WaitBuckets:     wait,
		TokenBuckets:    token,
		LastSeenAt:      lastSeen,
	}
}

// ManualPrune evicts shards idle longer than RetainFor. Called
// automatically by the background ticker in production; tests
// invoke it explicitly to drive deterministic eviction.
//
// Returns the count of evicted shards — useful in tests; in
// production callers can ignore the return.
func (r *Recorder) ManualPrune() int {
	if r == nil {
		return 0
	}
	cutoff := r.clock.Now().UTC().Add(-r.cfg.RetainFor).UnixNano()
	r.mu.Lock()
	defer r.mu.Unlock()
	evicted := 0
	for fundID, shard := range r.shards {
		if atomic.LoadInt64(&shard.lastSeenAtNanos) < cutoff {
			delete(r.shards, fundID)
			evicted++
		}
	}
	return evicted
}

// Len reports the current number of live shards (including
// overflow). Useful for dashboards and the "approaching
// MaxFunds" alarm.
func (r *Recorder) Len() int {
	if r == nil {
		return 0
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.shards)
}
