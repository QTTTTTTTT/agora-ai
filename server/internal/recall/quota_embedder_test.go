package recall

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fundai/server/internal/embedquota"
	"github.com/fundai/server/internal/embedquotaobs"
)

// stubEmbedder is a deterministic Embedder for the decorator
// tests. It records every call so we can assert pass-through.
type stubEmbedder struct {
	calls    int64
	failNext atomic.Bool
	fixed    []float32
}

func (s *stubEmbedder) Embed(_ context.Context, _ string) ([]float32, error) {
	atomic.AddInt64(&s.calls, 1)
	if s.failNext.Load() {
		s.failNext.Store(false)
		return nil, errors.New("stub: simulated failure")
	}
	if s.fixed == nil {
		return []float32{0.1, 0.2, 0.3}, nil
	}
	return s.fixed, nil
}

func (s *stubEmbedder) Model() string { return "stub-embed-1" }

// fakeClock is the Clock used by embedquota tests; we replicate
// it here to avoid depending on test internals of the
// embedquota package.
type fakeClock struct {
	now time.Time
}

func (f *fakeClock) Now() time.Time { return f.now }

// TestQuotaEmbedder_NilLimiterReturnsInner confirms that the
// constructor short-circuits when no limiter is configured. This
// is the path operators take when they enable the recall worker
// before opting into quota gating.
func TestQuotaEmbedder_NilLimiterReturnsInner(t *testing.T) {
	stub := &stubEmbedder{}
	got := NewQuotaEmbedder(stub, nil)
	if got != stub {
		t.Fatalf("expected nil-limiter constructor to return the inner embedder unchanged, got %T", got)
	}
}

// TestQuotaEmbedder_AcquireRecordsUsage covers the happy path:
// Acquire succeeds → upstream is called → RecordUsage credits
// the daily ledger.
func TestQuotaEmbedder_AcquireRecordsUsage(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 10,
		TokenQuotaPerDay:  100_000,
	}, clock)
	stub := &stubEmbedder{}
	q := NewQuotaEmbedder(stub, limiter)

	if _, err := q.Embed(context.Background(), "hello world"); err != nil {
		t.Fatalf("first embed: %v", err)
	}
	if got := atomic.LoadInt64(&stub.calls); got != 1 {
		t.Fatalf("expected 1 upstream call, got %d", got)
	}
	snap := limiter.Snapshot()
	if len(snap) != 1 || snap[0].Tokens <= 0 {
		t.Fatalf("expected ledger to record positive tokens, got %+v", snap)
	}
}

// TestQuotaEmbedder_QuotaExhaustedShortCircuits is the critical
// safety property: when the daily ledger is full, the decorator
// MUST NOT call upstream — that's the whole point of the quota
// layer.
func TestQuotaEmbedder_QuotaExhaustedShortCircuits(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 100,
		TokenQuotaPerDay:  10, // tiny so a single call fills it
	}, clock)
	stub := &stubEmbedder{}
	q := NewQuotaEmbedder(stub, limiter)

	if _, err := q.Embed(context.Background(), "first call uses ~5 tokens"); err != nil {
		t.Fatalf("first embed unexpected err: %v", err)
	}

	_, err := q.Embed(context.Background(), "second call should be blocked because the day is full")
	if !errors.Is(err, embedquota.ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted on second call, got %v", err)
	}
	if got := atomic.LoadInt64(&stub.calls); got != 1 {
		t.Fatalf("upstream should have been called exactly once, got %d", got)
	}
}

// TestQuotaEmbedder_FailureStillRecordsUsage protects against
// the over-counting choice we made in the comment block: if the
// upstream embed call fails, we still credit the estimated
// usage so a retry storm doesn't invisibly blow the quota. The
// alternative (silent under-count) is the bug we're paid to
// avoid.
func TestQuotaEmbedder_FailureStillRecordsUsage(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 100,
		TokenQuotaPerDay:  100_000,
	}, clock)
	stub := &stubEmbedder{}
	stub.failNext.Store(true)
	q := NewQuotaEmbedder(stub, limiter)

	_, err := q.Embed(context.Background(), "this call will fail")
	if err == nil || err.Error() != "stub: simulated failure" {
		t.Fatalf("expected upstream failure to propagate, got %v", err)
	}
	snap := limiter.Snapshot()
	if len(snap) != 1 || snap[0].Tokens <= 0 {
		t.Fatalf("expected failure path to still record tokens, got %+v", snap)
	}
}

// TestQuotaEmbedder_ContextCancelledDuringWait protects the
// shutdown path: when the rate limiter says "wait 30s" but the
// caller's ctx gets cancelled, we must abort cleanly rather
// than block until the timer fires. The embed worker's outer
// ctx has a 5-minute budget, so a stuck wait would otherwise
// pin the worker for the duration.
func TestQuotaEmbedder_ContextCancelledDuringWait(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 1,
		TokenQuotaPerDay:  100_000,
	}, clock)
	stub := &stubEmbedder{}
	q := &QuotaEmbedder{
		Inner:          stub,
		Limiter:        limiter,
		MaxWaitPerCall: 50 * time.Millisecond,
	}

	if _, err := q.Embed(context.Background(), "first"); err != nil {
		t.Fatalf("first embed: %v", err)
	}

	// The second call would normally have to wait nearly a
	// minute (rate is 1 / minute). MaxWaitPerCall caps it at
	// 50ms; we cancel the ctx before that fires to confirm the
	// waiter responds to ctx cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := q.Embed(ctx, "second"); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}

// W14-1 — recorder integration tests.
//
// These tests pin three properties:
//
//  1. A successful Embed routes a per-fund observation to the
//     recorder, with token count > 0 and the right fundID.
//  2. The Quota-exhausted path bumps RecordExhaust on the
//     side-car so dashboards can localise the burn.
//  3. Missing fundID (blank ctx) makes the recorder receive no
//     per-fund observations — but the Limiter ledger still
//     records aggregate usage. This is the "anonymous backfill"
//     path that must not pollute fund metrics with "".

func newRecorderForTest(t *testing.T) *embedquotaobs.Recorder {
	t.Helper()
	r := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 16, RetainFor: time.Hour})
	t.Cleanup(func() { r.Close() })
	return r
}

func TestQuotaEmbedder_RecorderReceivesPerFundCall(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 60,
		TokenQuotaPerDay:  100_000,
	}, clock)
	rec := newRecorderForTest(t)
	stub := &stubEmbedder{}
	q := NewQuotaEmbedderWithRecorder(stub, limiter, rec)

	ctx := WithFundID(context.Background(), "fund-alpha")
	if _, err := q.Embed(ctx, "hello world from alpha"); err != nil {
		t.Fatalf("embed: %v", err)
	}

	snaps := rec.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("expected one fund snapshot, got %d (%+v)", len(snaps), snaps)
	}
	if snaps[0].FundID != "fund-alpha" {
		t.Fatalf("wrong fundID: %s", snaps[0].FundID)
	}
	if snaps[0].TokensTodayUsed <= 0 {
		t.Fatalf("expected positive tokens, got %d", snaps[0].TokensTodayUsed)
	}
	if snaps[0].ThrottledTotal != 0 {
		t.Fatalf("expected no throttle on healthy call, got %d", snaps[0].ThrottledTotal)
	}
	if snaps[0].ExhaustedTotal != 0 {
		t.Fatalf("expected no exhaust on healthy call, got %d", snaps[0].ExhaustedTotal)
	}
}

func TestQuotaEmbedder_RecorderExhaustOnQuotaBurn(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 60,
		TokenQuotaPerDay:  10, // tiny — first call eats it all
	}, clock)
	rec := newRecorderForTest(t)
	stub := &stubEmbedder{}
	q := NewQuotaEmbedderWithRecorder(stub, limiter, rec)

	ctx := WithFundID(context.Background(), "fund-beta")
	if _, err := q.Embed(ctx, "first call within quota"); err != nil {
		t.Fatalf("first embed: %v", err)
	}
	if _, err := q.Embed(ctx, "second call should be rejected by quota"); !errors.Is(err, embedquota.ErrQuotaExhausted) {
		t.Fatalf("expected ErrQuotaExhausted, got %v", err)
	}

	snaps := rec.Snapshot()
	if len(snaps) != 1 {
		t.Fatalf("expected one fund snapshot, got %d", len(snaps))
	}
	if snaps[0].ExhaustedTotal != 1 {
		t.Fatalf("expected ExhaustedTotal=1 after quota burn, got %d", snaps[0].ExhaustedTotal)
	}
}

func TestQuotaEmbedder_RecorderDropsAnonymousCalls(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 60,
		TokenQuotaPerDay:  100_000,
	}, clock)
	rec := newRecorderForTest(t)
	stub := &stubEmbedder{}
	q := NewQuotaEmbedderWithRecorder(stub, limiter, rec)

	if _, err := q.Embed(context.Background(), "no fundID on ctx"); err != nil {
		t.Fatalf("embed: %v", err)
	}

	if got := rec.Len(); got != 0 {
		t.Fatalf("expected recorder to remain empty for anonymous calls, got %d funds", got)
	}
	if snap := limiter.Snapshot(); len(snap) != 1 || snap[0].Tokens <= 0 {
		t.Fatalf("aggregate ledger should still record tokens, got %+v", snap)
	}
}

// Per-fund observations on the failure path must still flow —
// otherwise a spike of upstream 5xx looks like "fund went idle"
// in the dashboard. We mirror the W5-3 behaviour where the
// limiter ledger is debited on failure so the per-fund counter
// has to follow the same rule.
func TestQuotaEmbedder_RecorderRecordsOnUpstreamFailure(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC)}
	limiter := embedquota.NewWithClock(embedquota.Config{
		MaxCallsPerMinute: 60,
		TokenQuotaPerDay:  100_000,
	}, clock)
	rec := newRecorderForTest(t)
	stub := &stubEmbedder{}
	stub.failNext.Store(true)
	q := NewQuotaEmbedderWithRecorder(stub, limiter, rec)

	ctx := WithFundID(context.Background(), "fund-gamma")
	if _, err := q.Embed(ctx, "this fails upstream"); err == nil {
		t.Fatalf("expected upstream error, got nil")
	}

	snaps := rec.Snapshot()
	if len(snaps) != 1 || snaps[0].FundID != "fund-gamma" {
		t.Fatalf("expected one fund-gamma snapshot, got %+v", snaps)
	}
	if snaps[0].TokensTodayUsed <= 0 {
		t.Fatalf("expected tokens debited on upstream failure, got %d", snaps[0].TokensTodayUsed)
	}
}
