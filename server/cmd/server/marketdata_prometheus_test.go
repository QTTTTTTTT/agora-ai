package main

import (
	"strings"
	"testing"

	"github.com/fundai/server/internal/marketdata"
)

func TestExportMarketDataPrometheusEmitsExpectedSeries(t *testing.T) {
	svc := marketdata.NewService(marketdata.Config{
		QuoteTTL:                     1,
		QuoteCircuitFailureThreshold: 1,
	})
	// Use the test hook to record a failure -> success sequence so the
	// emitted text contains numbers we can assert on.
	svc.RecordTestProviderFailure("tencent")
	svc.RecordTestProviderSuccess("yahoo")

	out := exportMarketDataPrometheus(svc)
	for _, want := range []string{
		`# TYPE fundai_marketdata_provider_calls_total counter`,
		`fundai_marketdata_provider_calls_total{provider="tencent"}`,
		`fundai_marketdata_provider_calls_total{provider="yahoo"}`,
		`fundai_marketdata_provider_failures_total{provider="tencent"} 1`,
		`fundai_marketdata_provider_consecutive_failures{provider="tencent"} 1`,
		`fundai_marketdata_provider_circuit_open{provider="tencent"}`,
		`fundai_marketdata_provider_latency_ms_ema{provider="yahoo"}`,
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q\n--- output ---\n%s", want, out)
		}
	}
}

func TestExportMarketDataPrometheusEmptyWhenNoActivity(t *testing.T) {
	svc := marketdata.NewService(marketdata.Config{})
	if got := exportMarketDataPrometheus(svc); got != "" {
		t.Fatalf("expected empty exporter output without activity, got %q", got)
	}
}

func TestExportMarketDataPrometheusNilSvc(t *testing.T) {
	if got := exportMarketDataPrometheus(nil); got != "" {
		t.Fatalf("expected empty output for nil service, got %q", got)
	}
}
