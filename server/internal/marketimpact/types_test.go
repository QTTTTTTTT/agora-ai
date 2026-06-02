package marketimpact

import (
	"math"
	"testing"
	"time"
)

func ptrFloat(v float64) *float64 { return &v }

func TestEngine_InvalidProbeReturnsZero(t *testing.T) {
	e := NewEngine().withClock(func() time.Time { return time.Unix(0, 0).UTC() })
	cases := []OrderProbe{
		{InstrumentKey: "AAPL", Quantity: 0, ReferencePx: 100},
		{InstrumentKey: "AAPL", Quantity: 100, ReferencePx: 0},
		{InstrumentKey: "AAPL", Quantity: -10, ReferencePx: 100},
	}
	for i, p := range cases {
		got := e.Estimate(p, nil)
		if got.AdverseBps != 0 {
			t.Errorf("case %d: expected 0 bps, got %v", i, got.AdverseBps)
		}
		if got.Reason != "invalid probe" {
			t.Errorf("case %d: reason = %q", i, got.Reason)
		}
	}
}

func TestEngine_NoCalibration_UsesAssetDefaults(t *testing.T) {
	e := NewEngine()
	probe := OrderProbe{
		InstrumentKey: "AAPL", AssetClass: "equity",
		Side: "buy", Quantity: 1000, ReferencePx: 100,
	}
	got := e.Estimate(probe, nil)
	if !got.UsedDefaults {
		t.Fatal("expected UsedDefaults=true")
	}
	if !got.UsedADVFallback {
		t.Fatal("expected UsedADVFallback=true (no ADV in defaults path)")
	}
	if got.AdverseBps != 1 {
		// equity default min_bps = 1
		t.Errorf("expected 1 bps floor, got %v", got.AdverseBps)
	}
	if got.Reason != "equity:default+adv_missing" {
		t.Errorf("reason = %q", got.Reason)
	}
}

func TestEngine_SquareRootScalesWithSize(t *testing.T) {
	e := NewEngine()
	calib := &Liquidity{
		InstrumentKey: "AAPL", Symbol: "AAPL", Market: "US", AssetClass: "equity",
		ADVShares:         ptrFloat(10_000_000),
		DailyVolatility:   ptrFloat(0.02),
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    500,
	}
	probeSmall := OrderProbe{
		InstrumentKey: "AAPL", AssetClass: "equity",
		Side: "buy", Quantity: 1_000, ReferencePx: 200,
	}
	probeMedium := OrderProbe{
		InstrumentKey: "AAPL", AssetClass: "equity",
		Side: "buy", Quantity: 100_000, ReferencePx: 200,
	}
	probeLarge := OrderProbe{
		InstrumentKey: "AAPL", AssetClass: "equity",
		Side: "buy", Quantity: 1_000_000, ReferencePx: 200,
	}
	small := e.Estimate(probeSmall, calib).AdverseBps
	medium := e.Estimate(probeMedium, calib).AdverseBps
	large := e.Estimate(probeLarge, calib).AdverseBps
	if !(small < medium && medium < large) {
		t.Errorf("expected size monotonic; got small=%v medium=%v large=%v",
			small, medium, large)
	}
	// Spot-check: for ratio=0.01 (1% ADV) → 0.02 * sqrt(0.01) * 10000 = 20bps
	expectedMedium := 20.0
	if math.Abs(medium-expectedMedium) > 0.01 {
		t.Errorf("medium expected ~%v, got %v", expectedMedium, medium)
	}
}

func TestEngine_ClampsToMaxBps(t *testing.T) {
	e := NewEngine()
	calib := &Liquidity{
		InstrumentKey: "ILLIQ", AssetClass: "equity",
		ADVShares:         ptrFloat(1_000),
		DailyVolatility:   ptrFloat(0.02),
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    1,
		MaxSlippageBps:    250,
	}
	// Order = ADV → ratio = 1 → bps = 0.02 * sqrt(1) * 10000 = 200 (under cap).
	// Push order = 5x ADV → ratio = 5 → bps = 0.02 * sqrt(5) * 10000 ≈ 447 (over 250 cap).
	probe := OrderProbe{
		InstrumentKey: "ILLIQ", AssetClass: "equity",
		Side: "buy", Quantity: 5_000, ReferencePx: 50,
	}
	got := e.Estimate(probe, calib)
	if got.AdverseBps != 250 {
		t.Errorf("expected clamp to max 250 bps, got %v", got.AdverseBps)
	}
}

func TestEngine_ClampsToMinBps(t *testing.T) {
	e := NewEngine()
	calib := &Liquidity{
		InstrumentKey: "MEGA", AssetClass: "equity",
		ADVShares:         ptrFloat(1_000_000_000),
		DailyVolatility:   ptrFloat(0.02),
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    2,
		MaxSlippageBps:    500,
	}
	// Order = 1 share against 1B ADV → ~0 raw bps, should snap to 2.
	probe := OrderProbe{
		InstrumentKey: "MEGA", AssetClass: "equity",
		Side: "buy", Quantity: 1, ReferencePx: 1,
	}
	got := e.Estimate(probe, calib)
	if got.AdverseBps != 2 {
		t.Errorf("expected min 2 bps, got %v", got.AdverseBps)
	}
}

func TestEngine_ADVMissingFallsBackToFloor(t *testing.T) {
	e := NewEngine()
	calib := &Liquidity{
		InstrumentKey: "NEW", AssetClass: "equity",
		ADVShares:         nil,
		DailyVolatility:   ptrFloat(0.02),
		ImpactCoefficient: 1.0,
		ImpactExponent:    0.5,
		MinSlippageBps:    3,
		MaxSlippageBps:    500,
	}
	probe := OrderProbe{
		InstrumentKey: "NEW", AssetClass: "equity",
		Side: "buy", Quantity: 1_000_000, ReferencePx: 50,
	}
	got := e.Estimate(probe, calib)
	if got.AdverseBps != 3 {
		t.Errorf("expected floor 3 bps when ADV missing, got %v", got.AdverseBps)
	}
	if !got.UsedADVFallback {
		t.Error("expected UsedADVFallback=true")
	}
	if got.UsedDefaults {
		t.Error("expected UsedDefaults=false (we have a calibration row)")
	}
}

func TestEngine_PartialCalibrationFallsBackPerField(t *testing.T) {
	// Operator filled in ADV but left coef/exp/sigma blank → engine
	// must fill the gaps with asset-class defaults.
	e := NewEngine()
	calib := &Liquidity{
		InstrumentKey: "PART", AssetClass: "equity",
		ADVShares:         ptrFloat(10_000_000),
		DailyVolatility:   nil, // ← missing
		ImpactCoefficient: 0,   // ← missing
		ImpactExponent:    0,   // ← missing
		MinSlippageBps:    1,
		MaxSlippageBps:    200,
	}
	probe := OrderProbe{
		InstrumentKey: "PART", AssetClass: "equity",
		Side: "buy", Quantity: 100_000, ReferencePx: 100,
	}
	got := e.Estimate(probe, calib)
	// Defaults: sigma=0.02, coef=1.0, alpha=0.5, ratio=0.01 → 20 bps.
	if math.Abs(got.AdverseBps-20) > 0.05 {
		t.Errorf("expected ~20 bps with default fallbacks, got %v", got.AdverseBps)
	}
}

func TestApplyAdverse(t *testing.T) {
	if got := ApplyAdverse(100, "buy", 50); math.Abs(got-100.5) > 1e-9 {
		t.Errorf("buy +50bps: got %v", got)
	}
	if got := ApplyAdverse(100, "sell", 50); math.Abs(got-99.5) > 1e-9 {
		t.Errorf("sell -50bps: got %v", got)
	}
	if got := ApplyAdverse(100, "unknown", 50); got != 100 {
		t.Errorf("unknown side should return base, got %v", got)
	}
	if got := ApplyAdverse(100, "buy", 0); got != 100 {
		t.Errorf("zero bps should return base, got %v", got)
	}
	// Pathological: bps > 10000 on a sell would cross zero → pinned.
	if got := ApplyAdverse(100, "sell", 99999); got >= 1 {
		t.Errorf("expected pinned-tiny for runaway sell, got %v", got)
	}
}

func TestAssetClassDefaultUnknownCollapsesToEquity(t *testing.T) {
	if got := AssetClassDefault("forex"); got != AssetClassDefault("equity") {
		t.Errorf("unknown asset class should == equity defaults, got %#v vs %#v", got, AssetClassDefault("equity"))
	}
}

func TestEngine_ZeroQuantityClamps(t *testing.T) {
	e := NewEngine()
	probe := OrderProbe{InstrumentKey: "AAPL", Quantity: 0, ReferencePx: 100}
	got := e.Estimate(probe, nil)
	if got.AdverseBps != 0 {
		t.Errorf("expected 0 for zero qty, got %v", got.AdverseBps)
	}
}
