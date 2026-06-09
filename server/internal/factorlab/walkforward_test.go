package factorlab

import (
	"math"
	"testing"
)

func TestRunWalkForwardFactorProducesNonEmptyFolds(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 42, Days: 1500})
	res, err := RunWalkForwardFactor(f, WalkForwardConfig{
		NumFolds:                  5,
		Horizons:                  []int{22},
		Factor:                    Momentum12_1MFactor{},
		MinSymbolsPerCrossSection: 3,
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res == nil {
		t.Fatalf("nil result")
	}
	if res.NumFolds != 5 {
		t.Errorf("num folds = %d, want 5", res.NumFolds)
	}
	if len(res.Folds) != 5 {
		t.Errorf("folds slice len = %d, want 5", len(res.Folds))
	}
	validFolds := 0
	for i, fold := range res.Folds {
		if fold.Error != "" {
			t.Logf("fold %d skipped: %s", i, fold.Error)
			continue
		}
		validFolds++
		if fold.ObservationDays <= 0 {
			t.Errorf("fold %d has no observation days", i)
		}
	}
	if validFolds < 3 {
		t.Errorf("expected ≥3 valid folds, got %d", validFolds)
	}
}

func TestRunWalkForwardFactorRejectsTooShortFixture(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 1, Days: 400})
	// Asking for 5 folds with warmup 273 + horizon 22 means we
	// need ~ 273 + 5*(22+5) = 408 days; 400 fails. We accept
	// either a hard error (preferred) OR a degenerate result
	// where every fold errored out.
	res, err := RunWalkForwardFactor(f, WalkForwardConfig{
		NumFolds:                  5,
		Horizons:                  []int{22},
		Factor:                    Momentum12_1MFactor{},
		MinSymbolsPerCrossSection: 3,
	})
	if err == nil && res != nil {
		// Should have skipped/errored every fold.
		for _, fold := range res.Folds {
			if fold.Error == "" && fold.ObservationDays > 0 {
				t.Errorf("expected fold to error on too-short fixture, got obs=%d", fold.ObservationDays)
			}
		}
	}
}

func TestApplyStabilityComputesSameSignRatio(t *testing.T) {
	res := &WalkForwardFactorResult{
		Folds: []FoldICResult{
			{Index: 0, SpearmanMean: 0.05, ObservationDays: 10},
			{Index: 1, SpearmanMean: 0.03, ObservationDays: 10},
			{Index: 2, SpearmanMean: -0.02, ObservationDays: 10}, // flipped sign
			{Index: 3, SpearmanMean: 0.04, ObservationDays: 10},
			{Index: 4, SpearmanMean: 0.01, ObservationDays: 10},
		},
	}
	res.applyStability(QualificationThresholds{})
	// Mean = (0.05 + 0.03 - 0.02 + 0.04 + 0.01) / 5 = 0.022 → positive sign.
	// Same-sign folds: 0, 1, 3, 4 → 4 out of 5 → 0.8 stability ratio.
	if math.Abs(res.MeanIC22d-0.022) > 1e-6 {
		t.Errorf("mean IC = %v, want 0.022", res.MeanIC22d)
	}
	if math.Abs(res.ICStabilityRatio-0.8) > 1e-6 {
		t.Errorf("stability ratio = %v, want 0.8", res.ICStabilityRatio)
	}
	if res.MinIC22d != -0.02 {
		t.Errorf("min IC = %v, want -0.02", res.MinIC22d)
	}
}

func TestApplyStabilityHandlesErroredFolds(t *testing.T) {
	res := &WalkForwardFactorResult{
		Folds: []FoldICResult{
			{Index: 0, SpearmanMean: 0.05, ObservationDays: 10},
			{Index: 1, Error: "fold too short"}, // skipped
			{Index: 2, SpearmanMean: 0.03, ObservationDays: 10},
		},
	}
	res.applyStability(QualificationThresholds{})
	// Mean over the 2 valid folds = 0.04.
	if math.Abs(res.MeanIC22d-0.04) > 1e-6 {
		t.Errorf("mean IC = %v, want 0.04 (skipping errored fold)", res.MeanIC22d)
	}
	if res.ICStabilityRatio != 1.0 {
		t.Errorf("stability = %v, want 1.0", res.ICStabilityRatio)
	}
}

func TestSliceFixtureRestrictsBars(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 7, Days: 200})
	days := f.TradingDays()
	from := days[50]
	to := days[150]
	sub := sliceFixture(f, from, to)
	if sub == nil {
		t.Fatalf("nil sub-fixture")
	}
	subDays := sub.TradingDays()
	if len(subDays) == 0 {
		t.Fatalf("empty sub-fixture")
	}
	if subDays[0].Before(from) {
		t.Errorf("first sub-day %v before window start %v", subDays[0], from)
	}
	if subDays[len(subDays)-1].After(to) {
		t.Errorf("last sub-day %v after window end %v", subDays[len(subDays)-1], to)
	}
}
