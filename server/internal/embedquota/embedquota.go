// Package embedquota provides backpressure and provider-quota
// awareness for the embed worker pipeline.
//
// MOTIVATION
// ----------
// The memory and re-embed pipelines call out to an external
// embedding provider (OpenAI / cohere / local model). Two
// failure modes are common in production:
//
//   1. Burst saturation. A consolidation batch fires off
//      hundreds of embed requests in seconds. The worker has
//      no notion of "back off when the provider is slowing
//      us down" — it just keeps issuing requests, runs into
//      the provider's per-minute cap, and the requests start
//      stacking up in the queue.
//   2. Quota exhaustion. The daily / monthly token quota
//      runs out. Requests fail with a 429 / quota error,
//      and the worker retries them indefinitely; the queue
//      grows unbounded; the system never recovers.
//
// W4-23 introduces an explicit Quota / Limiter layer:
//
//   * RateLimiter: a token-bucket style limiter for
//     per-minute call rate.
//   * QuotaLedger: a sliding-window counter for tokens
//     consumed (per minute / per hour / per day) with hooks
//     for "approaching limit" and "exhausted".
//   * ResultPolicy: how to handle a failed call:
//     retry-after / dead-letter / pause-pipeline.
//
// SCOPE
// -----
//   * Pure / deterministic (clock-injectable).
//   * Owns Limiter, Ledger, and the Status enum.
//   * Does NOT do the actual embed call. The wiring layer
//     wraps the existing EmbedClient with this.
package embedquota

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Status reflects the limiter's current health.
type Status string

const (
	StatusOK         Status = "ok"
	StatusThrottled  Status = "throttled"   // sleeping for backoff
	StatusNearLimit  Status = "near_limit"  // > 80% quota
	StatusExhausted  Status = "exhausted"   // 100% quota
	StatusUnavailable Status = "unavailable"
)

// Now is the clock used by all rate / ledger logic. Tests can
// swap it for a deterministic source.
type Clock interface {
	Now() time.Time
}

type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now().UTC() }

// Config tunes the limiter + ledger.
type Config struct {
	// MaxCallsPerMinute caps the rate of embed calls. 0 → 200.
	MaxCallsPerMinute int
	// TokenQuotaPerDay caps the total tokens (input + output)
	// consumed per UTC day. 0 → 1,000,000.
	TokenQuotaPerDay int
	// SoftLimitFraction triggers StatusNearLimit when usage
	// crosses this share of either cap. Defaults to 0.80.
	SoftLimitFraction float64
}

// DefaultConfig is the production-safe baseline.
func DefaultConfig() Config {
	return Config{
		MaxCallsPerMinute: 200,
		TokenQuotaPerDay:  1_000_000,
		SoftLimitFraction: 0.80,
	}
}

// Limiter coordinates the per-minute call rate AND the daily
// token quota. The wiring layer calls Acquire() before each
// embed call.
type Limiter struct {
	mu     sync.Mutex
	cfg    Config
	clock  Clock
	calls  []time.Time // sliding window of recent call times
	tokens map[string]int // YYYY-MM-DD → tokens spent

	// W8-1 — process-lifetime counters of *backpressure events*.
	// Distinct from the live `Status` because Status is a
	// point-in-time read; these are what an alert wants to look at
	// ("did we throttle in the last 5 minutes? did we ever exhaust
	// the daily quota since restart?"). Atomics so the metrics
	// export path can read them without contending on `mu`.
	throttledTotal uint64
	exhaustedTotal uint64

	// W9-1 — wait-time distribution. Counters alone tell us
	// "how often we throttled" but not "how *bad* it got": one
	// 30-second wait is operationally very different from a
	// hundred 50ms waits, even though both increment
	// throttledTotal once. A histogram lets a dashboard render
	// p50/p95/p99 wait latency, and an alert fire when the tail
	// drifts (e.g. p99 > 5s for 10 minutes).
	//
	// Fixed bucket schedule chosen to span the realistic range:
	// sub-ms (immediate go) → ~10min (we never wait 24h for the
	// exhausted-quota case; that's clamped before bucketing).
	// Atomics-only so we never block the hot path on the
	// histogram update.
	waitBuckets  [len(acquireWaitBucketsSec)]uint64
	waitCount    uint64
	waitSumNanos uint64

	// W10-1 — token-volume distribution per Acquire→RecordUsage
	// cycle. Pairs with the wait histogram so an on-caller can
	// distinguish "we throttled because call *count* spiked" from
	// "we throttled because each call got fatter" without staring
	// at separate counters and timestamps. Records only positive
	// observations (negative RecordUsage is a reservation refund —
	// counting refunds in p99 would imply "p99 of refunds" which
	// is meaningless for capacity planning).
	tokenBuckets [len(recordTokenBuckets)]uint64
	tokenCount   uint64
	tokenSum     uint64
}

// acquireWaitBucketsSec is the upper-bound (le) schedule for the
// Acquire wait-time histogram, in seconds. Cumulative
// Prometheus-style; the implicit +Inf bucket equals waitCount.
//
// Public-by-name (not exported) so tests can assert against it
// without baking magic numbers into multiple files.
var acquireWaitBucketsSec = [...]float64{
	0.001, 0.005, 0.01, 0.05, 0.1, 0.5, 1, 5, 30, 600,
}

// acquireWaitBucketCap is the largest finite bucket boundary; we
// clamp observations above it to avoid a single 24h
// "exhausted-quota" wait from inflating the running sum and
// blowing up p99 estimates for the next ~hours of history.
const acquireWaitBucketCap = 600 * time.Second

// recordTokenBuckets is the upper-bound (le) schedule for the
// RecordUsage token-volume histogram. Spans realistic embed
// payload sizes: a single sentence (~50 tokens) → a chunked
// long document (~32k tokens) with a +Inf overflow for batch
// jobs sized in the hundreds of thousands.
//
// Cumulative Prometheus-style; the implicit +Inf bucket equals
// tokenCount.
var recordTokenBuckets = [...]float64{
	50, 200, 500, 2_000, 8_000, 32_000, 100_000,
}

// AcquireWaitBucketsSec returns a copy of the wait-time
// histogram bucket boundaries (le schedule). Exported so sibling
// packages (embedquotaobs / W13-7) can reuse the exact same
// boundaries without re-declaring magic numbers and risking drift
// from this canonical schedule.
//
// Returns a fresh slice each call; the receiver may freely
// mutate it without affecting the limiter.
func AcquireWaitBucketsSec() []float64 {
	out := make([]float64, len(acquireWaitBucketsSec))
	copy(out, acquireWaitBucketsSec[:])
	return out
}

// RecordTokenBuckets is the token-volume sibling of
// AcquireWaitBucketsSec. Same single-source-of-truth contract.
func RecordTokenBuckets() []float64 {
	out := make([]float64, len(recordTokenBuckets))
	copy(out, recordTokenBuckets[:])
	return out
}

// New returns a Limiter using wall-clock time.
func New(cfg Config) *Limiter {
	return NewWithClock(cfg, wallClock{})
}

// NewWithClock is the test-friendly constructor.
func NewWithClock(cfg Config, clock Clock) *Limiter {
	if clock == nil {
		clock = wallClock{}
	}
	return &Limiter{
		cfg:    normalise(cfg),
		clock:  clock,
		tokens: make(map[string]int),
	}
}

// Acquire registers an intention to make one embed call.
// Returns the recommended wait duration before the call should
// proceed (0 = go now), and the current Status. When the daily
// token quota is exhausted, returns ErrQuotaExhausted with a
// non-zero suggested wait until UTC midnight.
//
// Token cost is estimated: pass an upper bound; the actual
// usage is reconciled by RecordUsage after the call.
func (l *Limiter) Acquire(estimatedTokens int) (time.Duration, Status, error) {
	if l == nil {
		return 0, StatusUnavailable, errors.New("embedquota: nil limiter")
	}
	now := l.clock.Now().UTC()
	l.mu.Lock()
	defer l.mu.Unlock()

	// 1. Daily token check.
	day := now.Format("2006-01-02")
	used := l.tokens[day]
	projected := used + estimatedTokens
	if projected > l.cfg.TokenQuotaPerDay {
		atomic.AddUint64(&l.exhaustedTotal, 1)
		nextMidnight := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)
		wait := nextMidnight.Sub(now)
		l.recordWaitLocked(wait)
		return wait, StatusExhausted, ErrQuotaExhausted
	}

	// 2. Per-minute call rate check.
	cutoff := now.Add(-time.Minute)
	pruned := l.calls[:0]
	for _, t := range l.calls {
		if t.After(cutoff) {
			pruned = append(pruned, t)
		}
	}
	l.calls = pruned
	if len(l.calls) >= l.cfg.MaxCallsPerMinute {
		atomic.AddUint64(&l.throttledTotal, 1)
		oldest := l.calls[0]
		wait := oldest.Add(time.Minute).Sub(now)
		if wait < 0 {
			wait = 0
		}
		l.recordWaitLocked(wait)
		return wait, StatusThrottled, nil
	}

	l.calls = append(l.calls, now)

	// 3. Health snapshot.
	status := StatusOK
	if float64(projected) >= float64(l.cfg.TokenQuotaPerDay)*l.cfg.SoftLimitFraction {
		status = StatusNearLimit
	}
	l.recordWaitLocked(0)
	return 0, status, nil
}

// recordWaitLocked updates the Acquire wait-time histogram. Must
// be called with l.mu held — the histogram itself is atomic, but
// pinning the update to the lock means the bucket increment
// always corresponds to the decision Acquire just made (no
// "saw a 0-wait observation but recorded 30s because another
// call slipped between the unlock and the record").
//
// We clamp the observation at acquireWaitBucketCap so a single
// 24h exhausted-quota wait doesn't blow up the running sum.
// Counts in the +Inf bucket reflect "how often did we hit the
// catastrophic case", which the exhaustedTotal counter already
// tracks more cleanly.
func (l *Limiter) recordWaitLocked(d time.Duration) {
	if d < 0 {
		d = 0
	}
	atomic.AddUint64(&l.waitCount, 1)
	clamped := d
	if clamped > acquireWaitBucketCap {
		clamped = acquireWaitBucketCap
	}
	atomic.AddUint64(&l.waitSumNanos, uint64(clamped))
	secs := d.Seconds()
	for i, le := range acquireWaitBucketsSec {
		if secs <= le {
			atomic.AddUint64(&l.waitBuckets[i], 1)
		}
	}
}

// RecordUsage reconciles the actual tokens consumed. The caller
// passes negative values to release a previously-reserved
// estimate.
//
// W10-1 — only positive observations contribute to the
// token-volume histogram. A negative reconciliation is a
// reservation refund (over-estimated upstream), not a real call
// size; folding refunds into the histogram would make "p99 call
// size" meaningless to a capacity planner.
func (l *Limiter) RecordUsage(actualTokens int) {
	if l == nil {
		return
	}
	now := l.clock.Now().UTC()
	day := now.Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()
	l.tokens[day] += actualTokens
	if l.tokens[day] < 0 {
		l.tokens[day] = 0
	}
	if actualTokens > 0 {
		l.recordTokensLocked(actualTokens)
	}
}

// recordTokensLocked updates the RecordUsage token-volume
// histogram. Mirrors recordWaitLocked — atomics for the bucket
// updates, called under l.mu so the bucket increment always
// corresponds to the day-tally update RecordUsage just made.
func (l *Limiter) recordTokensLocked(tokens int) {
	atomic.AddUint64(&l.tokenCount, 1)
	atomic.AddUint64(&l.tokenSum, uint64(tokens))
	v := float64(tokens)
	for i, le := range recordTokenBuckets {
		if v <= le {
			atomic.AddUint64(&l.tokenBuckets[i], 1)
		}
	}
}

// Snapshot returns the current per-day token usage. Sorted by
// day ascending for stable rendering.
type DaySnapshot struct {
	Day    string `json:"day"`
	Tokens int    `json:"tokens"`
}

// Snapshot returns the current usage tally.
func (l *Limiter) Snapshot() []DaySnapshot {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]DaySnapshot, 0, len(l.tokens))
	for day, n := range l.tokens {
		out = append(out, DaySnapshot{Day: day, Tokens: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Day < out[j].Day })
	return out
}

// RecentDays returns the last n calendar days of token usage as
// observed by the limiter, sorted ascending so the last element is
// "today" from the limiter's clock. Days the limiter never recorded
// against (e.g. an idle weekend, or n larger than the process
// uptime) come back with Tokens=0 — that's the desired UX for a
// sparkline / weekly budget review on the Admin panel: the absence
// of a day's bar communicates "we didn't run", but a *missing
// element* in the array would force every consumer to special-case
// it.
//
// Why this lives here rather than in the handler:
//
//  1. The "today" cutoff has to come from `l.clock`, which is
//     test-injectable. Computing it from `time.Now()` in the
//     handler would make the JSON disagree with the limiter under
//     a fake clock and bleed real-time semantics into otherwise
//     deterministic tests.
//  2. Keeps the unbounded `l.tokens` map private — callers ask
//     for the bounded view they actually want.
//
// Returns nil for n <= 0 or a nil receiver. n is clamped to a
// sane upper bound (366) so a misuse can't allocate an absurd
// slice.
func (l *Limiter) RecentDays(n int) []DaySnapshot {
	if l == nil || n <= 0 {
		return nil
	}
	if n > 366 {
		n = 366
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	today := l.clock.Now().UTC()
	out := make([]DaySnapshot, n)
	for i := 0; i < n; i++ {
		// i = 0 is the oldest, i = n-1 is today, so the slice
		// reads ascending by day for natural sparkline rendering.
		day := today.AddDate(0, 0, -(n - 1 - i)).Format("2006-01-02")
		out[i] = DaySnapshot{Day: day, Tokens: l.tokens[day]}
	}
	return out
}

// CallsPerMinute returns the count of calls in the last 60s.
func (l *Limiter) CallsPerMinute() int {
	if l == nil {
		return 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := l.clock.Now().UTC().Add(-time.Minute)
	count := 0
	for _, t := range l.calls {
		if t.After(cutoff) {
			count++
		}
	}
	return count
}

// Health returns a Prometheus-friendly snapshot of the limiter:
// today's token usage, the configured daily cap, the current
// per-minute call rate, and the configured rate cap. Designed for
// the W6-2 metrics export — exposing each value as its own gauge
// lets a dashboard plot "% of daily quota burned" and "% of rate
// budget burned" side-by-side without touching internals.
//
// W8-1 — also surfaces process-lifetime counters of throttle and
// exhaustion *events* so an alert can fire on "we throttled in
// the last 5 minutes" via rate(...) without having to scrape the
// Status string and turn it into a number.
type Health struct {
	TokensTodayUsed    int    `json:"tokensTodayUsed"`
	TokensDailyMax     int    `json:"tokensDailyMax"`
	CallsLastMinute    int    `json:"callsLastMinute"`
	CallsPerMinuteMax  int    `json:"callsPerMinuteMax"`
	SoftLimitFraction  float64 `json:"softLimitFraction"`
	Status             Status `json:"status"`
	ThrottledTotal     uint64 `json:"throttledTotal"`
	ExhaustedTotal     uint64 `json:"exhaustedTotal"`
}

// HealthSnapshot returns a Health snapshot. Safe on a nil
// receiver — returns zero-value Status="unavailable" so the
// caller can render a "limiter disabled" panel without nil
// checks scattered through the export path.
func (l *Limiter) HealthSnapshot() Health {
	if l == nil {
		return Health{Status: StatusUnavailable}
	}
	now := l.clock.Now().UTC()
	day := now.Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-time.Minute)
	calls := 0
	for _, t := range l.calls {
		if t.After(cutoff) {
			calls++
		}
	}
	tokens := l.tokens[day]
	status := StatusOK
	if tokens >= l.cfg.TokenQuotaPerDay {
		status = StatusExhausted
	} else if float64(tokens) >= float64(l.cfg.TokenQuotaPerDay)*l.cfg.SoftLimitFraction {
		status = StatusNearLimit
	} else if calls >= l.cfg.MaxCallsPerMinute {
		status = StatusThrottled
	}
	return Health{
		TokensTodayUsed:   tokens,
		TokensDailyMax:    l.cfg.TokenQuotaPerDay,
		CallsLastMinute:   calls,
		CallsPerMinuteMax: l.cfg.MaxCallsPerMinute,
		SoftLimitFraction: l.cfg.SoftLimitFraction,
		Status:            status,
		ThrottledTotal:    atomic.LoadUint64(&l.throttledTotal),
		ExhaustedTotal:    atomic.LoadUint64(&l.exhaustedTotal),
	}
}

// WaitBucket is one cumulative bucket in the Acquire wait-time
// histogram: every observation with wait <= LeSeconds is
// counted in Count.
type WaitBucket struct {
	LeSeconds float64 `json:"leSeconds"`
	Count     uint64  `json:"count"`
}

// WaitHistogramSnapshot is the consistent point-in-time view of
// the Acquire wait-time histogram, suitable for both Prometheus
// export and JSON debug surfaces.
//
// Buckets are cumulative and ordered ascending by LeSeconds.
// Count is the total number of Acquire decisions observed
// (== the implicit +Inf bucket). SumSeconds is the sum of all
// observed wait durations, with each individual sample clamped
// at acquireWaitBucketCap before summing — see
// recordWaitLocked for the rationale.
type WaitHistogramSnapshot struct {
	Buckets    []WaitBucket `json:"buckets"`
	Count      uint64       `json:"count"`
	SumSeconds float64      `json:"sumSeconds"`
}

// WaitHistogram returns a consistent snapshot of the Acquire
// wait-time histogram. Safe on a nil receiver — returns an
// empty schema-correct snapshot so the export path can render
// "limiter disabled" without nil checks.
func (l *Limiter) WaitHistogram() WaitHistogramSnapshot {
	out := WaitHistogramSnapshot{
		Buckets: make([]WaitBucket, len(acquireWaitBucketsSec)),
	}
	for i, le := range acquireWaitBucketsSec {
		out.Buckets[i] = WaitBucket{LeSeconds: le}
	}
	if l == nil {
		return out
	}
	for i := range acquireWaitBucketsSec {
		out.Buckets[i].Count = atomic.LoadUint64(&l.waitBuckets[i])
	}
	out.Count = atomic.LoadUint64(&l.waitCount)
	out.SumSeconds = float64(atomic.LoadUint64(&l.waitSumNanos)) / float64(time.Second)
	return out
}

// TokenBucket is one cumulative bucket in the RecordUsage
// token-volume histogram: every observation with tokens <= Le
// is counted in Count.
type TokenBucket struct {
	Le    float64 `json:"le"`
	Count uint64  `json:"count"`
}

// TokenHistogramSnapshot is the consistent point-in-time view
// of the RecordUsage token-volume histogram. Sum is the total
// number of tokens observed (positive observations only — see
// RecordUsage for why); Count is the number of *call sites*
// that contributed (which equals the implicit +Inf bucket).
type TokenHistogramSnapshot struct {
	Buckets []TokenBucket `json:"buckets"`
	Count   uint64        `json:"count"`
	Sum     uint64        `json:"sum"`
}

// TokenHistogram returns a consistent snapshot of the
// RecordUsage token-volume histogram. Nil-safe like its sibling
// WaitHistogram — returns a schema-correct empty snapshot so
// the export path doesn't need a separate code branch when the
// limiter is disabled.
func (l *Limiter) TokenHistogram() TokenHistogramSnapshot {
	out := TokenHistogramSnapshot{
		Buckets: make([]TokenBucket, len(recordTokenBuckets)),
	}
	for i, le := range recordTokenBuckets {
		out.Buckets[i] = TokenBucket{Le: le}
	}
	if l == nil {
		return out
	}
	for i := range recordTokenBuckets {
		out.Buckets[i].Count = atomic.LoadUint64(&l.tokenBuckets[i])
	}
	out.Count = atomic.LoadUint64(&l.tokenCount)
	out.Sum = atomic.LoadUint64(&l.tokenSum)
	return out
}

// ErrQuotaExhausted is returned by Acquire when the daily
// token cap is exceeded.
var ErrQuotaExhausted = errors.New("embedquota: daily token quota exhausted")

func normalise(c Config) Config {
	d := DefaultConfig()
	if c.MaxCallsPerMinute <= 0 {
		c.MaxCallsPerMinute = d.MaxCallsPerMinute
	}
	if c.TokenQuotaPerDay <= 0 {
		c.TokenQuotaPerDay = d.TokenQuotaPerDay
	}
	if c.SoftLimitFraction <= 0 || c.SoftLimitFraction > 1 {
		c.SoftLimitFraction = d.SoftLimitFraction
	}
	return c
}
