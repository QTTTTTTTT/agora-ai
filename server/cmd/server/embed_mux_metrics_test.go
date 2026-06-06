package main

import (
	"strings"
	"testing"

	"github.com/fundai/server/internal/embedquota"
	"github.com/fundai/server/internal/memreembed"
)

// TestExportEmbedQuotaPrometheusEmitsAllSeries asserts that every
// W6-2 gauge name appears in the /api/metrics output, *both* when
// the limiter is wired and when it is nil — operators rely on the
// names being stable so a missing-series alert means "the worker
// stopped emitting", not "the gauge was renamed".
func TestExportEmbedQuotaPrometheusEmitsAllSeries(t *testing.T) {
	want := []string{
		"fundai_embed_quota_tokens_today_used",
		"fundai_embed_quota_tokens_daily_max",
		"fundai_embed_quota_calls_last_minute",
		"fundai_embed_quota_calls_per_minute_max",
		"fundai_embed_quota_status",
		// W8-1 — backpressure event counters MUST surface even on
		// a nil limiter so a fresh deploy doesn't trip absent-series
		// alerts on the rate(...) panel.
		"fundai_embed_quota_throttled_total",
		"fundai_embed_quota_exhausted_total",
		// W9-1 — wait-time histogram. Bucket / sum / count must
		// all be present for histogram_quantile() to work.
		"fundai_embed_quota_acquire_wait_seconds_bucket",
		"fundai_embed_quota_acquire_wait_seconds_sum",
		"fundai_embed_quota_acquire_wait_seconds_count",
		// W10-1 — token-volume histogram, paired with the wait
		// histogram so a dashboard can plot "throttled because
		// of more calls or because of fatter calls?".
		"fundai_embed_quota_call_tokens_bucket",
		"fundai_embed_quota_call_tokens_sum",
		"fundai_embed_quota_call_tokens_count",
	}
	got := exportEmbedQuotaPrometheus(nil)
	for _, name := range want {
		if !strings.Contains(got, name) {
			t.Errorf("nil-limiter export missing %q\n--- output ---\n%s\n--- end ---", name, got)
		}
	}
	// Status=unavailable for nil limiter encodes to 0.
	if !strings.Contains(got, "fundai_embed_quota_status 0") {
		t.Errorf("nil-limiter status should encode to 0, got:\n%s", got)
	}
	// Counters MUST be zero on a nil limiter — non-zero would be
	// surprising semantics for a process that has no limiter at all.
	if !strings.Contains(got, "fundai_embed_quota_throttled_total 0") {
		t.Errorf("nil-limiter throttled counter must be 0, got:\n%s", got)
	}
	if !strings.Contains(got, "fundai_embed_quota_exhausted_total 0") {
		t.Errorf("nil-limiter exhausted counter must be 0, got:\n%s", got)
	}
	// W9-1 — every bucket boundary must surface even on a nil
	// limiter, otherwise Prometheus will see the histogram
	// "appear" later and misclassify the gap as backfill.
	for _, le := range []string{`le="0.001"`, `le="0.005"`, `le="0.01"`, `le="0.05"`,
		`le="0.1"`, `le="0.5"`, `le="1"`, `le="5"`, `le="30"`, `le="600"`, `le="+Inf"`} {
		if !strings.Contains(got, le) {
			t.Errorf("nil-limiter histogram missing %s, got:\n%s", le, got)
		}
	}
	if !strings.Contains(got, "fundai_embed_quota_acquire_wait_seconds_count 0") {
		t.Errorf("nil-limiter histogram count must be 0, got:\n%s", got)
	}
	// W10-1 — token-volume histogram bucket boundaries must
	// also surface on a nil limiter.
	for _, le := range []string{
		`fundai_embed_quota_call_tokens_bucket{le="50"}`,
		`fundai_embed_quota_call_tokens_bucket{le="200"}`,
		`fundai_embed_quota_call_tokens_bucket{le="500"}`,
		`fundai_embed_quota_call_tokens_bucket{le="2000"}`,
		`fundai_embed_quota_call_tokens_bucket{le="8000"}`,
		`fundai_embed_quota_call_tokens_bucket{le="32000"}`,
		`fundai_embed_quota_call_tokens_bucket{le="100000"}`,
		`fundai_embed_quota_call_tokens_bucket{le="+Inf"}`,
	} {
		if !strings.Contains(got, le) {
			t.Errorf("nil-limiter token histogram missing %q, got:\n%s", le, got)
		}
	}
	if !strings.Contains(got, "fundai_embed_quota_call_tokens_count 0") {
		t.Errorf("nil-limiter token histogram count must be 0, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPrometheusHistogramReflectsLiveState pins
// that the histogram sum/count/buckets emitted on the metrics
// endpoint reflect actual Acquire activity. This is the
// integration-shaped sibling of the unit-level
// TestWaitHistogramRecordsZeroAndThrottledObservations: it
// guarantees the export *plumbing* doesn't silently drop the
// observations.
func TestExportEmbedQuotaPrometheusHistogramReflectsLiveState(t *testing.T) {
	cfg := embedquota.DefaultConfig()
	cfg.MaxCallsPerMinute = 100
	cfg.TokenQuotaPerDay = 1_000_000
	limiter := embedquota.New(cfg)
	for i := 0; i < 5; i++ {
		if _, _, err := limiter.Acquire(10); err != nil {
			t.Fatalf("Acquire %d: %v", i, err)
		}
	}
	got := exportEmbedQuotaPrometheus(limiter)
	if !strings.Contains(got, "fundai_embed_quota_acquire_wait_seconds_count 5") {
		t.Errorf("export should report count=5 after 5 Acquires, got:\n%s", got)
	}
	// All five observations were 0-wait — must be in every
	// bucket including le=0.001.
	if !strings.Contains(got, `fundai_embed_quota_acquire_wait_seconds_bucket{le="0.001"} 5`) {
		t.Errorf("0.001s bucket should hold all 5 zero-wait obs, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPrometheusTokenHistogramReflectsRecordUsage
// is the integration sibling for W10-1: it pins that the
// metrics endpoint surfaces RecordUsage observations as a
// histogram with the right bucket placement and sum.
func TestExportEmbedQuotaPrometheusTokenHistogramReflectsRecordUsage(t *testing.T) {
	cfg := embedquota.DefaultConfig()
	cfg.TokenQuotaPerDay = 1_000_000
	limiter := embedquota.New(cfg)
	limiter.RecordUsage(40)    // → le=50
	limiter.RecordUsage(150)   // → le=200
	limiter.RecordUsage(1_500) // → le=2000
	limiter.RecordUsage(-50)   // refund — must NOT be observed

	got := exportEmbedQuotaPrometheus(limiter)
	if !strings.Contains(got, "fundai_embed_quota_call_tokens_count 3") {
		t.Errorf("export should report count=3 after 3 positive observations, got:\n%s", got)
	}
	// Sum = 40 + 150 + 1500 = 1690 (refund excluded).
	if !strings.Contains(got, "fundai_embed_quota_call_tokens_sum 1690") {
		t.Errorf("export sum should ignore the refund and equal 1690, got:\n%s", got)
	}
	// le=50 holds only the 40-token sample.
	if !strings.Contains(got, `fundai_embed_quota_call_tokens_bucket{le="50"} 1`) {
		t.Errorf("le=50 should hold 1 obs, got:\n%s", got)
	}
	// le=2000 holds all three positive obs.
	if !strings.Contains(got, `fundai_embed_quota_call_tokens_bucket{le="2000"} 3`) {
		t.Errorf("le=2000 should hold all 3 obs, got:\n%s", got)
	}
}

// TestExportSSEMuxPrometheusEmitsAllSeries asserts the multiplex
// counters surface even when no client has connected yet (so a
// fresh deployment doesn't trip alerts that depend on the series
// existing).
func TestExportSSEMuxPrometheusEmitsAllSeries(t *testing.T) {
	want := []string{
		"fundai_workflow_sse_mux_active_connections",
		"fundai_workflow_sse_mux_connections_total",
		"fundai_workflow_sse_mux_subscriptions_total",
		"fundai_workflow_sse_mux_forbidden_frames_total",
	}
	out := exportSSEMuxPrometheus()
	for _, name := range want {
		if !strings.Contains(out, name) {
			t.Errorf("export missing %q\n--- output ---\n%s\n--- end ---", name, out)
		}
	}
}

// TestExportMemReembedPrometheusNilQueueOnlyEmitsStatus asserts the
// shape we contract with the dashboard for the "re-embed disabled"
// case: a single status=0 series, no pending/embedded gauges. The
// alternative — exposing zero-valued gauges for a disabled queue —
// would falsely satisfy a "metric exists" alert and let an outage
// hide.
func TestExportMemReembedPrometheusNilQueueOnlyEmitsStatus(t *testing.T) {
	got := exportMemReembedPrometheus(nil)
	if !strings.Contains(got, "fundai_memreembed_status 0") {
		t.Errorf("nil queue should emit status=0, got:\n%s", got)
	}
	for _, banned := range []string{
		"fundai_memreembed_pending",
		"fundai_memreembed_embedded_total",
		"fundai_memreembed_retried_total",
		"fundai_memreembed_dead_letter_total",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("nil queue must not emit %q (would mask a disabled worker), got:\n%s", banned, got)
		}
	}
}

// TestExportMemReembedPrometheusReportsLiveStats asserts the queue's
// pending count and counters surface correctly after enqueue +
// drain. We bypass the worker (no embedder/writer wired) and read
// the queue directly so the test stays a pure exporter check.
func TestExportMemReembedPrometheusReportsLiveStats(t *testing.T) {
	q := memreembed.NewQueue(memreembed.DefaultConfig())
	if err := q.Enqueue(memreembed.Request{MemoryID: "mem-1", Content: "alpha"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := q.Enqueue(memreembed.Request{MemoryID: "mem-2", Content: "beta"}); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	got := exportMemReembedPrometheus(q)
	want := []string{
		"fundai_memreembed_pending 2",
		"fundai_memreembed_embedded_total 0",
		"fundai_memreembed_retried_total 0",
		"fundai_memreembed_dead_letter_total 0",
		"fundai_memreembed_status 1",
	}
	for _, line := range want {
		if !strings.Contains(got, line) {
			t.Errorf("expected %q in export, got:\n%s", line, got)
		}
	}
}
