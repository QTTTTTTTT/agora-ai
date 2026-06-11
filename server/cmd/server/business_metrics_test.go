// business_metrics_test.go — B2 coverage. The three new business
// metrics are each verified at the unit boundary: the
// Observe/Record/Set method does what its docstring says, AND the
// resulting Prometheus output contains the expected series + the
// correct help/type headers (so a downstream Prometheus parser
// won't choke).
//
// Why a single file vs. one-per-metric: the metric plumbing all
// lives on the same serverMetrics struct and shares an exporter
// — keeping the assertions together makes the exporter contract
// (HELP / TYPE / sort order) easy to audit at a glance.
package main

import (
	"strings"
	"testing"
	"time"
)

func TestObserveDailyPicksPublish_PopulatesHistogram(t *testing.T) {
	m := newServerMetrics()
	m.ObserveDailyPicksPublish("growth", 12*time.Second)
	m.ObserveDailyPicksPublish("growth", 90*time.Second)
	m.ObserveDailyPicksPublish("value", 45*time.Second)

	out := m.ExportPrometheus()

	if !strings.Contains(out, "dailypicks_publish_duration_seconds_count{preset=\"growth\"} 2") {
		t.Fatalf("growth count missing or wrong:\n%s", filterLines(out, "dailypicks_publish"))
	}
	if !strings.Contains(out, "dailypicks_publish_duration_seconds_count{preset=\"value\"} 1") {
		t.Fatalf("value count missing or wrong:\n%s", filterLines(out, "dailypicks_publish"))
	}
	if !strings.Contains(out, "# TYPE dailypicks_publish_duration_seconds histogram") {
		t.Fatalf("histogram TYPE header missing:\n%s", filterLines(out, "dailypicks_publish"))
	}
	// 12s falls into the le=30 bucket (and every higher one).
	if !strings.Contains(out, `dailypicks_publish_duration_seconds_bucket{preset="growth",le="30"}`) {
		t.Fatalf("le=30 bucket missing for growth:\n%s", filterLines(out, "dailypicks_publish_duration_seconds_bucket"))
	}
}

func TestObserveDailyPicksPublish_EmptyPresetBucketsAsUnknown(t *testing.T) {
	m := newServerMetrics()
	m.ObserveDailyPicksPublish("", 5*time.Second)
	out := m.ExportPrometheus()
	if !strings.Contains(out, "preset=\"unknown\"") {
		t.Fatalf("empty preset should bucket as 'unknown':\n%s", filterLines(out, "dailypicks_publish"))
	}
}

func TestObserveDailyPicksPublish_NilReceiverIsNoOp(t *testing.T) {
	var m *serverMetrics
	// Must not panic. Whole reason we keep nil-receiver guards in
	// the metric methods is the unit-test surface where a
	// dailyPicksLoop is constructed without wiring serverMetrics.
	m.ObserveDailyPicksPublish("anything", time.Second)
}

func TestRecordComplianceFilterBlock_AccumulatesByPatternAndLayer(t *testing.T) {
	m := newServerMetrics()
	m.RecordComplianceFilterBlock("guaranteed_return", "advisor")
	m.RecordComplianceFilterBlock("guaranteed_return", "advisor")
	m.RecordComplianceFilterBlock("breakout_imminent", "advisor")
	m.RecordComplianceFilterBlock("guaranteed_return", "geo")

	out := m.ExportPrometheus()

	if !strings.Contains(out, `compliance_filter_blocked_total{pattern="guaranteed_return",layer="advisor"} 2`) {
		t.Fatalf("expected advisor=2 for guaranteed_return:\n%s", filterLines(out, "compliance_filter_blocked"))
	}
	if !strings.Contains(out, `compliance_filter_blocked_total{pattern="breakout_imminent",layer="advisor"} 1`) {
		t.Fatalf("expected advisor=1 for breakout_imminent")
	}
	if !strings.Contains(out, `compliance_filter_blocked_total{pattern="guaranteed_return",layer="geo"} 1`) {
		t.Fatalf("expected geo=1 for guaranteed_return (layer must segment the series)")
	}
	if !strings.Contains(out, "# TYPE compliance_filter_blocked_total counter") {
		t.Fatalf("counter TYPE header missing")
	}
}

func TestRecordComplianceFilterBlock_BlanksCollapseToUnknown(t *testing.T) {
	m := newServerMetrics()
	m.RecordComplianceFilterBlock("", "")
	out := m.ExportPrometheus()
	if !strings.Contains(out, `pattern="unknown",layer="unknown"`) {
		t.Fatalf("blank labels should collapse to 'unknown':\n%s", filterLines(out, "compliance_filter_blocked"))
	}
}

func TestSetSubscriptionMRR_LastWriteWins(t *testing.T) {
	m := newServerMetrics()
	m.SetSubscriptionMRR(1234.50)
	m.SetSubscriptionMRR(2345.75)
	out := m.ExportPrometheus()
	if !strings.Contains(out, "subscription_mrr_usd 2345.75") {
		t.Fatalf("expected most recent MRR value to win:\n%s", filterLines(out, "subscription_mrr"))
	}
}

func TestSetSubscriptionMRR_RejectsNegative(t *testing.T) {
	m := newServerMetrics()
	m.SetSubscriptionMRR(100.0)
	m.SetSubscriptionMRR(-50.0) // must NOT overwrite
	out := m.ExportPrometheus()
	if !strings.Contains(out, "subscription_mrr_usd 100.00") {
		t.Fatalf("negative MRR must not clobber prior value:\n%s", filterLines(out, "subscription_mrr"))
	}
}

// filterLines is a test-helper that returns only the lines of out
// containing needle. The full Prometheus dump is ~kilobytes; for
// failure-time diagnostics we only want the few lines for the
// metric under test.
func filterLines(out, needle string) string {
	var b strings.Builder
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, needle) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
