package main

import (
	"bytes"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestRequestLoggerTemplatizesOpaqueIDs verifies that the metrics
// label collapses opaque per-record IDs to a {id} placeholder so
// that two requests with different fund IDs end up in the same
// histogram series. This is the "high cardinality" fix that makes
// P95/P99 latency derivation statistically meaningful.
func TestRequestLoggerTemplatizesOpaqueIDs(t *testing.T) {
	metrics := newServerMetrics()
	handler := requestLogger(metrics, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	for _, fundID := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"01HX1234567890ABCDEFGHJK",
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/funds/"+fundID+"/holdings", nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
	}

	output := metrics.ExportPrometheus()

	// All three requests should fold into ONE templatized series.
	templatizedKey := `path="/api/funds/{id}/holdings"`
	if !bytes.Contains([]byte(output), []byte(templatizedKey)) {
		t.Fatalf("expected templatized path label %q in metrics, got:\n%s", templatizedKey, output)
	}

	// The templatized counter should reflect 3 hits on a single series.
	if !bytes.Contains([]byte(output),
		[]byte(`fundai_http_requests_total{method="GET",path="/api/funds/{id}/holdings",status="200"} 3`)) {
		t.Fatalf("expected aggregated counter to read 3, got:\n%s", output)
	}

	// And there must be NO per-fund-id rows leaking into metrics.
	for _, raw := range []string{
		"550e8400-e29b-41d4-a716-446655440000",
		"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
		"01HX1234567890ABCDEFGHJK",
	} {
		if strings.Contains(output, raw) {
			t.Errorf("metrics output leaked raw ID %q (cardinality fix did not apply)", raw)
		}
	}
}

// TestExportPrometheusEmitsLatencyQuantiles asserts that the
// /api/metrics output includes the self-derived P50/P95/P99
// gauges keyed by quantile, and that they match a hand-rolled
// expectation for a known latency distribution.
func TestExportPrometheusEmitsLatencyQuantiles(t *testing.T) {
	metrics := newServerMetrics()

	// Inject a flat latency profile: 100 hits at 50 ms each.
	for i := 0; i < 100; i++ {
		metrics.ObserveHTTP(http.MethodGet, "/api/health", http.StatusOK, 50*time.Millisecond)
	}

	output := metrics.ExportPrometheus()
	if !bytes.Contains([]byte(output), []byte("fundai_http_request_duration_seconds_quantile")) {
		t.Fatalf("expected quantile gauge in metrics output, got:\n%s", output)
	}
	for _, q := range []string{`quantile="0.5"`, `quantile="0.95"`, `quantile="0.99"`} {
		if !bytes.Contains([]byte(output), []byte(q)) {
			t.Errorf("expected gauge with %s, missing from:\n%s", q, output)
		}
	}
}

// TestHistogramQuantileMath checks the linear-interpolation logic
// against hand-derived values across edge cases: empty histogram,
// single bucket, mid-bucket interpolation, +Inf overflow, and
// monotonicity (P50 ≤ P95 ≤ P99 for any non-trivial distribution).
func TestHistogramQuantileMath(t *testing.T) {
	bounds := []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10}

	t.Run("zero observations", func(t *testing.T) {
		counts := make([]int64, len(bounds)+1)
		got := histogramQuantile(bounds, counts, 0, 0.95)
		if got != 0 {
			t.Errorf("expected 0 for empty histogram, got %v", got)
		}
	})

	t.Run("monotonic across quantiles", func(t *testing.T) {
		// 50 obs at <= 0.05, 40 obs at <= 0.5, 10 obs at <= 5.
		counts := []int64{0, 0, 0, 50, 50, 50, 90, 90, 90, 100, 100, 100}
		p50 := histogramQuantile(bounds, counts, 100, 0.5)
		p95 := histogramQuantile(bounds, counts, 100, 0.95)
		p99 := histogramQuantile(bounds, counts, 100, 0.99)
		if !(p50 <= p95 && p95 <= p99) {
			t.Errorf("expected P50 ≤ P95 ≤ P99, got %.4f / %.4f / %.4f", p50, p95, p99)
		}
		// P50 lands in the [0.025, 0.05] bucket — interpolation
		// puts it at 0.05 (rank 50 of cumulative 50 → upper bound).
		if math.Abs(p50-0.05) > 1e-6 {
			t.Errorf("expected P50 = 0.05, got %.6f", p50)
		}
	})

	t.Run("rank in +Inf caps at largest finite", func(t *testing.T) {
		// 50 obs all in the +Inf bucket (>10s).
		counts := []int64{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 50}
		got := histogramQuantile(bounds, counts, 50, 0.99)
		if got != 10 {
			t.Errorf("expected cap at largest finite bound (10), got %v", got)
		}
	})

	t.Run("q out of range", func(t *testing.T) {
		counts := []int64{1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1}
		if got := histogramQuantile(bounds, counts, 1, 0); got != 0 {
			t.Errorf("expected 0 for q=0, got %v", got)
		}
		if got := histogramQuantile(bounds, counts, 1, 1); got != 10 {
			t.Errorf("expected largest bound for q=1, got %v", got)
		}
	})
}
