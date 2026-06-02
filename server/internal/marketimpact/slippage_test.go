package marketimpact

import (
	"math"
	"testing"

	"github.com/fundai/server/internal/matching"
)

type stubRecorder struct {
	calls int
	last  Estimate
}

func (s *stubRecorder) RecordEstimate(_ OrderProbe, est Estimate) {
	s.calls++
	s.last = est
}

func TestSlippageAdapter_BuyAddsImpactOnTopOfAsk(t *testing.T) {
	cache := NewCache(nil, CacheConfig{})
	adv := 10_000_000.0
	sigma := 0.02
	cache.SetRows([]Liquidity{{
		InstrumentKey:     "AAPL.US",
		Symbol:            "AAPL",
		Market:            "US",
		AssetClass:        "equity",
		ADVShares:         &adv,
		DailyVolatility:   &sigma,
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    500,
	}})
	rec := &stubRecorder{}
	adapter := NewSlippageAdapter(cache, NewEngine(), WithRecorder(rec))
	order := matching.Order{
		InstrumentKey: "AAPL.US",
		Side:          matching.SideBuy,
		Quantity:      100_000, // 1% of ADV → 20 bps
		AssetClass:    "equity",
	}
	q := matching.Quote{Last: 200, Bid: 199.95, Ask: 200.05}
	got := adapter.FillPrice(order, q)
	// Expected: ask 200.05 + 20bps = 200.05 * 1.002 = 200.4501.
	want := 200.05 * (1 + 20.0/10000)
	if math.Abs(got-want) > 0.01 {
		t.Errorf("expected fill ~%v, got %v", want, got)
	}
	if rec.calls != 1 {
		t.Errorf("recorder called %d times", rec.calls)
	}
	if rec.last.AdverseBps != 20 {
		t.Errorf("expected 20 bps in recorded estimate, got %v", rec.last.AdverseBps)
	}
}

func TestSlippageAdapter_SellSubtractsImpactFromBid(t *testing.T) {
	cache := NewCache(nil, CacheConfig{})
	adv := 10_000_000.0
	sigma := 0.02
	cache.SetRows([]Liquidity{{
		InstrumentKey:     "AAPL.US",
		AssetClass:        "equity",
		ADVShares:         &adv,
		DailyVolatility:   &sigma,
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    500,
	}})
	adapter := NewSlippageAdapter(cache, NewEngine())
	order := matching.Order{
		InstrumentKey: "AAPL.US",
		Side:          matching.SideSell,
		Quantity:      100_000,
		AssetClass:    "equity",
	}
	q := matching.Quote{Last: 200, Bid: 199.95, Ask: 200.05}
	got := adapter.FillPrice(order, q)
	// Expected: bid 199.95 - 20bps = 199.95 * 0.998 = 199.55001.
	want := 199.95 * (1 - 20.0/10000)
	if math.Abs(got-want) > 0.01 {
		t.Errorf("expected fill ~%v, got %v", want, got)
	}
}

func TestSlippageAdapter_NoCalibrationFallsBackToSpread(t *testing.T) {
	cache := NewCache(nil, CacheConfig{})
	adapter := NewSlippageAdapter(cache, NewEngine())
	order := matching.Order{
		InstrumentKey: "OBS.US",
		Side:          matching.SideBuy,
		Quantity:      1000,
		AssetClass:    "equity",
	}
	q := matching.Quote{Last: 100, Bid: 99.95, Ask: 100.05}
	got := adapter.FillPrice(order, q)
	// No calibration and engine returns just min_bps=1 as floor.
	// Expected: ask 100.05 + 1bps = 100.0600005 ≈ 100.06.
	want := 100.05 * (1 + 1.0/10000)
	if math.Abs(got-want) > 0.01 {
		t.Errorf("expected fill ~%v (spread + floor), got %v", want, got)
	}
}

func TestSlippageAdapter_NilEngineReturnsBase(t *testing.T) {
	adapter := &SlippageAdapter{Cache: NewCache(nil, CacheConfig{}), Engine: nil}
	order := matching.Order{
		InstrumentKey: "X",
		Side:          matching.SideBuy,
		Quantity:      100,
		AssetClass:    "equity",
	}
	q := matching.Quote{Last: 50, Bid: 49.99, Ask: 50.01}
	got := adapter.FillPrice(order, q)
	if math.Abs(got-50.01) > 1e-9 {
		t.Errorf("nil engine should return ask, got %v", got)
	}
}

func TestSlippageAdapter_NegativeBaseReturnsBaseUnchanged(t *testing.T) {
	cache := NewCache(nil, CacheConfig{})
	adapter := NewSlippageAdapter(cache, NewEngine())
	order := matching.Order{
		InstrumentKey: "X",
		Side:          matching.SideBuy,
		Quantity:      100,
		AssetClass:    "equity",
	}
	q := matching.Quote{Last: 0, Bid: 0, Ask: 0}
	got := adapter.FillPrice(order, q)
	if got != 0 {
		t.Errorf("expected 0 base, got %v", got)
	}
}

func TestSlippageAdapter_EstimateForProbe(t *testing.T) {
	cache := NewCache(nil, CacheConfig{})
	adv := 10_000_000.0
	sigma := 0.02
	cache.SetRows([]Liquidity{{
		InstrumentKey:     "AAPL.US",
		AssetClass:        "equity",
		ADVShares:         &adv,
		DailyVolatility:   &sigma,
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    500,
	}})
	adapter := NewSlippageAdapter(cache, NewEngine())
	est := adapter.EstimateForProbe(OrderProbe{
		InstrumentKey: "AAPL.US",
		AssetClass:    "equity",
		Side:          "buy",
		Quantity:      100_000,
		ReferencePx:   200,
	})
	if math.Abs(est.AdverseBps-20) > 0.05 {
		t.Errorf("expected 20 bps preview, got %v", est.AdverseBps)
	}
}
