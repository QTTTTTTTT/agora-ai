package recon

import (
	"testing"
	"time"
)

func sampleSnapshot() *InternalSnapshot {
	return &InternalSnapshot{
		FundID:   "fund-1",
		AsOfDate: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		Positions: []InternalPosition{
			{Symbol: "AAPL", Quantity: 100, AvgCost: 175, Currency: "USD"},
			{Symbol: "TSLA", Quantity: 50, AvgCost: 250, Currency: "USD"},
		},
		Cash: []InternalCash{
			{Currency: "USD", Balance: 10000},
		},
		Trades: []InternalTrade{
			{ExternalRef: "ORD-1", Symbol: "AAPL", Side: "buy", Quantity: 50, Price: 175, Currency: "USD"},
		},
	}
}

// PerfectMirror should diff to zero breaks.
func TestMockProvider_PerfectMirror_NoBreaks(t *testing.T) {
	snap := sampleSnapshot()
	mp := NewMockProvider(MockProviderOptions{Source: SourceMock})
	stmt := mp.Build(snap)
	res := NewEngine(DefaultTolerances).Diff(stmt, snap)
	if len(res.Breaks) != 0 {
		t.Errorf("perfect mirror produced breaks: %+v", res.Breaks)
	}
}

// WithDrift should produce at least one break per perturbed
// dimension.
func TestMockProvider_WithDrift_ProducesBreaks(t *testing.T) {
	snap := sampleSnapshot()
	mp := NewMockProvider(MockProviderOptions{
		IncludeDrift:  true,
		DriftQuantity: 1,
		DriftCash:     0.50,
		DriftPrice:    0.10,
	})
	stmt := mp.Build(snap)
	res := NewEngine(DefaultTolerances).Diff(stmt, snap)

	var sawQty, sawCash, sawPrice bool
	for _, b := range res.Breaks {
		switch b.Type {
		case BreakPositionQuantityMismatch:
			sawQty = true
		case BreakCashBalanceMismatch:
			sawCash = true
		case BreakTradePriceMismatch:
			sawPrice = true
		}
	}
	if !sawQty {
		t.Error("expected position qty break")
	}
	if !sawCash {
		t.Error("expected cash balance break")
	}
	if !sawPrice {
		t.Error("expected trade price break")
	}
}

// IngestParamsFromBuild marks the payload synthetic so a real-feed
// scrub query can filter it out.
func TestIngestParamsFromBuild_MarksSynthetic(t *testing.T) {
	snap := sampleSnapshot()
	stmt := NewMockProvider(MockProviderOptions{}).Build(snap)
	p := IngestParamsFromBuild(stmt, "ops-user")
	if got, _ := p.RawPayload["_synthetic"].(bool); !got {
		t.Errorf("_synthetic flag missing: %+v", p.RawPayload)
	}
	if p.IngestedBy != "ops-user" {
		t.Errorf("ingested_by = %q", p.IngestedBy)
	}
}

// nil snapshot is safe.
func TestMockProvider_NilSnapshot(t *testing.T) {
	if got := NewMockProvider(MockProviderOptions{}).Build(nil); got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}
