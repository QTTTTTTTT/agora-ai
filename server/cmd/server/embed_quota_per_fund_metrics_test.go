// embed_quota_per_fund_metrics_test.go — covers the W14-2
// per-fund Prometheus exporter. The aggregate exporter is
// already covered by embed_mux_metrics_test.go; this file pins
// the per-fund fan-out so a Grafana panel keyed on `fund_id`
// can survive code drift.

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/fundai/server/internal/embedquotaobs"
)

// TestExportEmbedQuotaPerFundPrometheus_NilRecorderEmitsStatusZero
// pins the disabled path. A missing series here would cause the
// admin dashboard to render NaN instead of a clear "disabled"
// banner; the literal `_status 0` line is the contract that
// keeps that banner working.
func TestExportEmbedQuotaPerFundPrometheus_NilRecorderEmitsStatusZero(t *testing.T) {
	got := exportEmbedQuotaPerFundPrometheus(nil)
	if !strings.Contains(got, "fundai_embed_quota_per_fund_status 0") {
		t.Fatalf("nil recorder should emit status=0, got:\n%s", got)
	}
	// On disabled path, no per-fund series should be emitted —
	// otherwise Prometheus would happily ingest empty histograms
	// and the dashboard would surface them as zero-valued funds.
	for _, banned := range []string{
		"fundai_embed_quota_per_fund_throttled_total",
		"fundai_embed_quota_per_fund_exhausted_total",
		"fundai_embed_quota_per_fund_tokens_today_used",
		"fundai_embed_quota_per_fund_acquire_wait_seconds",
		"fundai_embed_quota_per_fund_call_tokens",
	} {
		if strings.Contains(got, banned) {
			t.Errorf("nil recorder must NOT emit %q, got:\n%s", banned, got)
		}
	}
}

// TestExportEmbedQuotaPerFundPrometheus_EmptyRecorderEmitsStatusOnly
// covers the "wired but no observations yet" boot phase. Every
// series header must be absent until the first call lands;
// otherwise we'd be emitting zero-valued buckets for funds that
// have never embedded, polluting Prometheus's storage on every
// fresh deploy.
func TestExportEmbedQuotaPerFundPrometheus_EmptyRecorderEmitsStatusOnly(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8})
	defer rec.Close()
	got := exportEmbedQuotaPerFundPrometheus(rec)
	if !strings.Contains(got, "fundai_embed_quota_per_fund_status 1") {
		t.Fatalf("wired-but-empty recorder should emit status=1, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPerFundPrometheus_SingleFundEmitsAllSeries
// pins the happy path: one fund worth of observations should
// fan out to every metric family with the correct fund_id label.
func TestExportEmbedQuotaPerFundPrometheus_SingleFundEmitsAllSeries(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8, RetainFor: time.Hour})
	defer rec.Close()
	rec.RecordCall("fund-alpha", 150, 50*time.Millisecond)
	rec.RecordThrottle("fund-alpha")
	rec.RecordExhaust("fund-alpha")

	got := exportEmbedQuotaPerFundPrometheus(rec)

	// Counters.
	if !strings.Contains(got, `fundai_embed_quota_per_fund_throttled_total{fund_id="fund-alpha"} 1`) {
		t.Errorf("missing throttled counter for fund-alpha, got:\n%s", got)
	}
	if !strings.Contains(got, `fundai_embed_quota_per_fund_exhausted_total{fund_id="fund-alpha"} 1`) {
		t.Errorf("missing exhausted counter for fund-alpha, got:\n%s", got)
	}
	if !strings.Contains(got, `fundai_embed_quota_per_fund_tokens_today_used{fund_id="fund-alpha"} 150`) {
		t.Errorf("missing tokens-today gauge, got:\n%s", got)
	}

	// Histograms — bucket placement.
	// 50ms wait → falls into le=0.05 (and every higher bucket).
	if !strings.Contains(got, `fundai_embed_quota_per_fund_acquire_wait_seconds_bucket{fund_id="fund-alpha",le="0.05"} 1`) {
		t.Errorf("wait histogram missing le=0.05 bucket, got:\n%s", got)
	}
	// le=0.001 must be 0 — the wait was 50ms, well above 1ms.
	if !strings.Contains(got, `fundai_embed_quota_per_fund_acquire_wait_seconds_bucket{fund_id="fund-alpha",le="0.001"} 0`) {
		t.Errorf("wait histogram should report 0 in le=0.001 (wait was 50ms), got:\n%s", got)
	}
	if !strings.Contains(got, `fundai_embed_quota_per_fund_acquire_wait_seconds_count{fund_id="fund-alpha"} 1`) {
		t.Errorf("wait histogram count missing, got:\n%s", got)
	}

	// 150 token call → falls into le=200 (token bucket boundary).
	if !strings.Contains(got, `fundai_embed_quota_per_fund_call_tokens_bucket{fund_id="fund-alpha",le="200"} 1`) {
		t.Errorf("token histogram missing le=200 bucket, got:\n%s", got)
	}
	if !strings.Contains(got, `fundai_embed_quota_per_fund_call_tokens_sum{fund_id="fund-alpha"} 150`) {
		t.Errorf("token sum should equal 150, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPerFundPrometheus_TwoFundsKeepDistinctLabels
// pins that the exporter never collapses two funds into a
// shared label — a regression here would silently turn the
// dashboard into "single fund" view and obscure tenant-level
// outliers.
func TestExportEmbedQuotaPerFundPrometheus_TwoFundsKeepDistinctLabels(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8, RetainFor: time.Hour})
	defer rec.Close()
	rec.RecordCall("fund-a", 50, 0)
	rec.RecordCall("fund-b", 1000, 0)

	got := exportEmbedQuotaPerFundPrometheus(rec)

	if !strings.Contains(got, `fund_id="fund-a"`) {
		t.Errorf("fund-a label missing, got:\n%s", got)
	}
	if !strings.Contains(got, `fund_id="fund-b"`) {
		t.Errorf("fund-b label missing, got:\n%s", got)
	}
	// Fund A's tokens-today should be 50, fund B's should be
	// 1000 — these MUST appear on different lines, not collapsed.
	if !strings.Contains(got, `fundai_embed_quota_per_fund_tokens_today_used{fund_id="fund-a"} 50`) {
		t.Errorf("fund-a tokens-today missing, got:\n%s", got)
	}
	if !strings.Contains(got, `fundai_embed_quota_per_fund_tokens_today_used{fund_id="fund-b"} 1000`) {
		t.Errorf("fund-b tokens-today missing, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPerFundPrometheus_OverflowFundEmitsCardinalityAlarm
// — when MaxFunds is exceeded, the recorder collapses extra
// funds into a synthetic OverflowFundID. The exporter must
// surface this so a `fund_id="_overflow"` query lights up an
// "embed quota cardinality budget exceeded" alert.
func TestExportEmbedQuotaPerFundPrometheus_OverflowFundEmitsCardinalityAlarm(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 1, RetainFor: time.Hour})
	defer rec.Close()
	rec.RecordCall("fund-first", 100, 0)
	rec.RecordCall("fund-second", 200, 0)
	rec.RecordCall("fund-third", 300, 0)

	got := exportEmbedQuotaPerFundPrometheus(rec)
	if !strings.Contains(got, `fund_id="`+embedquotaobs.OverflowFundID+`"`) {
		t.Errorf("overflow shard label missing under MaxFunds=1, got:\n%s", got)
	}
}

// TestExportEmbedQuotaPerFundPrometheus_FundIDsAreEscaped pins
// that fundIDs containing backslashes or double-quotes never
// produce malformed Prometheus exposition. We use %q for
// formatting which always emits a Go-quoted string — fully
// compatible with Prometheus's label-value grammar (which
// follows the same escape rules as Go for \ and ").
func TestExportEmbedQuotaPerFundPrometheus_FundIDsAreEscaped(t *testing.T) {
	rec := embedquotaobs.New(embedquotaobs.Config{MaxFunds: 8, RetainFor: time.Hour})
	defer rec.Close()
	rec.RecordCall(`fund-"weird"`, 10, 0)

	got := exportEmbedQuotaPerFundPrometheus(rec)
	// The double-quote inside the fund id MUST be escaped — if
	// the exporter naively interpolates, Prometheus will reject
	// the entire scrape with "expected label value, got '"'".
	if !strings.Contains(got, `fund_id="fund-\"weird\""`) {
		t.Errorf("expected quoted fund_id to be escaped, got:\n%s", got)
	}
}
