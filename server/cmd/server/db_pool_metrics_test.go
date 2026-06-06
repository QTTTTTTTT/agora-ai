package main

import (
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestExportRuntimePrometheusEmitsExtendedDBStats verifies that the
// /api/metrics output includes the new DB-pool gauges + churn
// counters added in W1-2: utilization%, wait-avg, max_idle_closed,
// max_idle_time_closed, max_lifetime_closed.
func TestExportRuntimePrometheusEmitsExtendedDBStats(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock open: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	out := exportRuntimePrometheus(db, nil)

	// Gauges that must appear regardless of traffic.
	expected := []string{
		"fundai_db_open_connections",
		"fundai_db_in_use_connections",
		"fundai_db_idle_connections",
		"fundai_db_max_open_connections 25",
		"fundai_db_pool_utilization_pct",
		"fundai_db_wait_count_total",
		"fundai_db_wait_duration_seconds_total",
		"fundai_db_wait_avg_seconds",
		"fundai_db_max_idle_closed_total",
		"fundai_db_max_idle_time_closed_total",
		"fundai_db_max_lifetime_closed_total",
	}
	for _, e := range expected {
		if !strings.Contains(out, e) {
			t.Errorf("expected %q in /metrics output, missing\n--- output ---\n%s\n--- end ---", e, out)
		}
	}

	// Sentinel values: with no waits the wait_avg gauge must be -1
	// (we treat -1 as "undefined" so dashboards can mask the panel).
	if !strings.Contains(out, "fundai_db_wait_avg_seconds -1.000000") {
		t.Errorf("expected wait_avg -1 sentinel for zero-wait pool, got:\n%s", out)
	}
}
