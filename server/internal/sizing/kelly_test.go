package sizing

import (
	"math"
	"testing"
)

func TestApplyKellyNeverInflatesNominal(t *testing.T) {
	in := KellyInputs{
		Symbol:        "AAPL",
		NominalWeight: 0.02,
		Confidence:    1.0,
		ATR:           0.001, // tiny ATR — vol budget would be huge
	}
	got := ApplyKelly(in, DefaultKellyConfig())
	if got.FinalWeight > in.NominalWeight {
		t.Errorf("post-processor must not inflate: nominal=%v, final=%v",
			in.NominalWeight, got.FinalWeight)
	}
}

func TestApplyKellyVolBudgetBindsForHighVol(t *testing.T) {
	cfg := DefaultKellyConfig()
	cfg.KellyFraction = 1.0 // disable kelly damp so vol budget binds.
	cfg.MaxAbsWeight = 1.0
	in := KellyInputs{
		Symbol:        "GME",
		NominalWeight: 0.30,
		Confidence:    1.0,
		ATR:           0.20, // 20% ATR — extreme vol
	}
	got := ApplyKelly(in, cfg)
	if got.BindingConstraint != "vol_budget" {
		t.Errorf("binding: got %q, want vol_budget", got.BindingConstraint)
	}
	if got.FinalWeight >= in.NominalWeight {
		t.Errorf("vol budget should shrink size, got %v vs nominal %v",
			got.FinalWeight, in.NominalWeight)
	}
}

func TestApplyKellyDampApplies(t *testing.T) {
	cfg := DefaultKellyConfig()
	in := KellyInputs{
		Symbol:        "AAPL",
		NominalWeight: 0.04,
		Confidence:    0.9,
		ATR:           0.01,
	}
	got := ApplyKelly(in, cfg)
	expected := math.Abs(in.NominalWeight) * in.Confidence * cfg.KellyFraction
	if math.Abs(got.KellyWeight-expected) > 1e-9 {
		t.Errorf("KellyWeight: got %v, want %v", got.KellyWeight, expected)
	}
	if got.BindingConstraint != "kelly_fraction" {
		t.Errorf("binding: got %q, want kelly_fraction", got.BindingConstraint)
	}
}

func TestApplyKellyZeroNominalShortCircuits(t *testing.T) {
	got := ApplyKelly(KellyInputs{Symbol: "X", NominalWeight: 0, ATR: 0.01}, DefaultKellyConfig())
	if got.FinalWeight != 0 {
		t.Errorf("FinalWeight: got %v, want 0", got.FinalWeight)
	}
	if got.BindingConstraint != "llm_zero" {
		t.Errorf("binding: got %q, want llm_zero", got.BindingConstraint)
	}
}

func TestApplyKellyNegativeNominal(t *testing.T) {
	in := KellyInputs{Symbol: "X", NominalWeight: -0.04, Confidence: 0.8, ATR: 0.02}
	got := ApplyKelly(in, DefaultKellyConfig())
	if got.FinalWeight >= 0 {
		t.Errorf("negative nominal should produce negative final, got %v", got.FinalWeight)
	}
	if math.Abs(got.FinalWeight) > math.Abs(in.NominalWeight) {
		t.Errorf("absolute final must not exceed absolute nominal")
	}
}

func TestApplyKellyMaxAbsCap(t *testing.T) {
	cfg := DefaultKellyConfig()
	cfg.MaxAbsWeight = 0.05
	cfg.KellyFraction = 1.0
	in := KellyInputs{
		Symbol:         "X",
		NominalWeight:  0.20,
		Confidence:     1.0,
		ATR:            0.001,
		HighConviction: true,
	}
	got := ApplyKelly(in, cfg)
	if got.BindingConstraint != "max_abs_cap" {
		t.Errorf("binding: got %q, want max_abs_cap", got.BindingConstraint)
	}
	if got.FinalWeight != cfg.MaxAbsWeight {
		t.Errorf("FinalWeight: got %v, want %v", got.FinalWeight, cfg.MaxAbsWeight)
	}
}

func TestApplyKellyHighConvictionBudget(t *testing.T) {
	cfg := DefaultKellyConfig()
	in := KellyInputs{Symbol: "X", NominalWeight: 0.20, Confidence: 1.0, ATR: 0.05}
	cfg.KellyFraction = 1.0
	low := ApplyKelly(in, cfg)
	in.HighConviction = true
	high := ApplyKelly(in, cfg)
	if high.VolBudgetWeight <= low.VolBudgetWeight {
		t.Errorf("HighConviction should increase vol budget, got high=%v low=%v",
			high.VolBudgetWeight, low.VolBudgetWeight)
	}
}

func TestApplyKellyATRFloor(t *testing.T) {
	cfg := DefaultKellyConfig()
	in := KellyInputs{Symbol: "X", NominalWeight: 0.10, Confidence: 1.0, ATR: 0}
	got := ApplyKelly(in, cfg)
	expected := cfg.RiskPerTrade / cfg.MinATR
	if math.Abs(got.VolBudgetWeight-expected) > 1e-9 {
		t.Errorf("VolBudgetWeight: got %v, want %v (using MinATR floor)",
			got.VolBudgetWeight, expected)
	}
}

func TestApplyKellyConfidenceClamped(t *testing.T) {
	cfg := DefaultKellyConfig()
	in := KellyInputs{Symbol: "X", NominalWeight: 0.04, Confidence: 1.5, ATR: 0.01}
	got := ApplyKelly(in, cfg)
	if got.Confidence != 1.0 {
		t.Errorf("Confidence not clamped, got %v", got.Confidence)
	}
	in.Confidence = -0.3
	got = ApplyKelly(in, cfg)
	if got.Confidence != 0 {
		t.Errorf("Confidence not floored, got %v", got.Confidence)
	}
	if got.FinalWeight != 0 {
		t.Errorf("FinalWeight: got %v, want 0 when confidence=0", got.FinalWeight)
	}
}

func TestApplyKellyBatchPreservesOrder(t *testing.T) {
	cfg := DefaultKellyConfig()
	items := []KellyInputs{
		{Symbol: "A", NominalWeight: 0.02, Confidence: 0.5, ATR: 0.01},
		{Symbol: "B", NominalWeight: 0.04, Confidence: 0.9, ATR: 0.02},
		{Symbol: "C", NominalWeight: -0.01, Confidence: 0.3, ATR: 0.05},
	}
	got := ApplyKellyBatch(items, cfg)
	if len(got) != 3 {
		t.Fatalf("len: got %d, want 3", len(got))
	}
	for i, d := range got {
		if d.Symbol != items[i].Symbol {
			t.Errorf("at index %d: got %q, want %q", i, d.Symbol, items[i].Symbol)
		}
	}
}

func TestApplyKellyConfigNormalisation(t *testing.T) {
	cfg := KellyConfig{}
	in := KellyInputs{Symbol: "X", NominalWeight: 0.04, Confidence: 0.8, ATR: 0.01}
	got := ApplyKelly(in, cfg)
	if got.FinalWeight == 0 {
		t.Errorf("normalisation should fall back to defaults, got 0 weight")
	}
}

func TestApplyKellyNaNNominal(t *testing.T) {
	in := KellyInputs{Symbol: "X", NominalWeight: math.NaN(), Confidence: 0.5, ATR: 0.01}
	got := ApplyKelly(in, DefaultKellyConfig())
	if got.FinalWeight != 0 {
		t.Errorf("FinalWeight: got %v, want 0 for NaN nominal", got.FinalWeight)
	}
}
