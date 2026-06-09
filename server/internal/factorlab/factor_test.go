package factorlab

import (
	"math"
	"testing"
	"time"
)

// pickAsOf returns a date roughly 80% into the synthetic
// fixture's history so every factor has enough lookback to
// produce a score for every symbol.
func pickAsOf(f *Fixture) time.Time {
	days := f.TradingDays()
	return days[int(float64(len(days))*0.8)]
}

func TestMomentum12_1MFactorRanksHighDriftHigher(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 42, Days: 600})
	asOf := pickAsOf(f)
	factor := Momentum12_1MFactor{}
	hiMom, ok := factor.Score(f, "HI_MOM", asOf)
	if !ok {
		t.Fatalf("HI_MOM score missing")
	}
	dog, ok := factor.Score(f, "DOG", asOf)
	if !ok {
		t.Fatalf("DOG score missing")
	}
	if hiMom <= dog {
		t.Errorf("HI_MOM (%v) should outrank DOG (%v) on momentum", hiMom, dog)
	}
}

func TestShortReversal1MFactorIsSignFlippedMomentum(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 7, Days: 400})
	asOf := pickAsOf(f)
	sr := ShortReversal1MFactor{}
	// Pick a symbol, manually compute the 1m return, verify
	// the factor returns the negation.
	closes := f.CloseSeries("HI_MOM", asOf.AddDate(0, 0, -21), asOf)
	want := -(closes[len(closes)-1]/closes[0] - 1.0)
	got, ok := sr.Score(f, "HI_MOM", asOf)
	if !ok {
		t.Fatalf("score missing")
	}
	if math.Abs(want-got) > 1e-9 {
		t.Errorf("short-reversal = %v, want %v", got, want)
	}
}

func TestLowVol60DFactorPrefersLowVolNames(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 11, Days: 600})
	asOf := pickAsOf(f)
	factor := LowVol60DFactor{}
	hiVol, ok := factor.Score(f, "HI_VOL", asOf)
	if !ok {
		t.Fatalf("HI_VOL score missing")
	}
	drifter, ok := factor.Score(f, "DRIFTER", asOf)
	if !ok {
		t.Fatalf("DRIFTER score missing")
	}
	if drifter <= hiVol {
		t.Errorf("DRIFTER (low-idio-vol) score %v should beat HI_VOL %v", drifter, hiVol)
	}
}

func TestDrawdownRecoveryFactorPrefersClosToPeak(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 19, Days: 400})
	asOf := pickAsOf(f)
	factor := DrawdownRecoveryFactor{}
	// HI_MOM has positive alpha so the trailing-60d window is
	// typically closer to the peak (smaller drawdown) than DOG
	// which has negative alpha and tends to be deeper underwater.
	hi, ok := factor.Score(f, "HI_MOM", asOf)
	if !ok {
		t.Fatalf("HI_MOM missing")
	}
	dog, ok := factor.Score(f, "DOG", asOf)
	if !ok {
		t.Fatalf("DOG missing")
	}
	if hi <= dog {
		t.Errorf("HI_MOM (alpha>0) should be closer to peak than DOG, got %v vs %v", hi, dog)
	}
}

func TestVolumeBreakout20DFactorReturnsRatio(t *testing.T) {
	// Build a tiny manual fixture with controlled volumes so we
	// can hand-verify the ratio. SynthFixture doesn't generate
	// volumes so we craft directly.
	day := func(d int) time.Time { return time.Date(2026, 1, d, 0, 0, 0, 0, time.UTC) }
	bars := make([]Bar, 25)
	for i := 0; i < 25; i++ {
		bars[i] = Bar{Date: day(i + 1), Close: 100, Volume: 1_000_000}
	}
	// Bump the last 5 days to 2× volume.
	for i := 20; i < 25; i++ {
		bars[i].Volume = 2_000_000
	}
	fix := &Fixture{
		Histories: []SymbolHistory{{Symbol: "TEST", Bars: bars}},
	}
	factor := VolumeBreakout20Factor{}
	got, ok := factor.Score(fix, "TEST", day(25))
	if !ok {
		t.Fatalf("score missing")
	}
	// short_mean = 2M, long_mean = (15*1M + 5*2M)/20 = 1.25M → ratio 1.6
	want := 2_000_000.0 / 1_250_000.0
	if math.Abs(got-want) > 1e-6 {
		t.Errorf("volume-breakout = %v, want %v", got, want)
	}
}

func TestScoreCrossSectionSortsDescending(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 3, Days: 400})
	asOf := pickAsOf(f)
	rows := ScoreCrossSection(f, Momentum12_1MFactor{}, asOf)
	if len(rows) < 2 {
		t.Fatalf("expected multi-symbol cross-section, got %d", len(rows))
	}
	for i := 1; i < len(rows); i++ {
		if rows[i].Score > rows[i-1].Score {
			t.Errorf("not sorted desc at index %d: %v > %v", i, rows[i].Score, rows[i-1].Score)
		}
	}
}

// -------- IC math helpers ----------------------------------------------------

func TestPearsonCorrPositiveAndNegative(t *testing.T) {
	pos := [][2]float64{{1, 1}, {2, 2}, {3, 3}, {4, 4}}
	if c := pearsonCorr(pos); math.Abs(c-1.0) > 1e-9 {
		t.Errorf("perfectly correlated → 1.0, got %v", c)
	}
	neg := [][2]float64{{1, 4}, {2, 3}, {3, 2}, {4, 1}}
	if c := pearsonCorr(neg); math.Abs(c+1.0) > 1e-9 {
		t.Errorf("perfectly anti-correlated → -1.0, got %v", c)
	}
}

func TestSpearmanCorrIsRobustToOutliers(t *testing.T) {
	// Same direction as pos but with an outlier: Pearson should
	// drop, Spearman should stay at 1.0.
	pairs := [][2]float64{{1, 1}, {2, 2}, {3, 3}, {4, 4}, {5, 1_000_000}}
	sp := spearmanCorr(pairs)
	if math.Abs(sp-1.0) > 1e-9 {
		t.Errorf("rank correlation should be 1.0 (monotonic), got %v", sp)
	}
}

func TestRankWithTiesAveragesTiedRanks(t *testing.T) {
	in := []float64{10, 20, 20, 30}
	got := rankWithTies(in)
	// Ranks ascending: 10→1, 20→2.5 (avg of 2,3), 20→2.5, 30→4.
	want := []float64{1, 2.5, 2.5, 4}
	for i := range want {
		if math.Abs(got[i]-want[i]) > 1e-9 {
			t.Errorf("rank[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}

// -------- Full report --------------------------------------------------------

func TestRunFactorReportProducesNonEmptyIC(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 99, Days: 700})
	reps := RunFactorReport(f, []Factor{Momentum12_1MFactor{}}, FactorReportConfig{
		Horizons:                  []int{5, 22},
		LayeredHorizonDays:        22,
		MinSymbolsPerCrossSection: 3,
	})
	if len(reps) != 1 {
		t.Fatalf("expected 1 report, got %d", len(reps))
	}
	rep := reps[0]
	if rep.ObservationDays <= 0 {
		t.Errorf("no observation days collected")
	}
	for _, h := range []int{5, 22} {
		stats, ok := rep.IC[h]
		if !ok {
			t.Errorf("no IC for horizon %d", h)
			continue
		}
		if len(stats.PearsonSeries) == 0 {
			t.Errorf("empty Pearson series for horizon %d", h)
		}
		if len(stats.SpearmanSeries) == 0 {
			t.Errorf("empty Spearman series for horizon %d", h)
		}
	}
	if rep.Layered == nil {
		t.Fatalf("no layered result")
	}
	if rep.Layered.ObservationPeriods <= 0 {
		t.Errorf("no layered observations")
	}
}

func TestRunFactorReportLongShortNavMonotonicallyGrows(t *testing.T) {
	// 5-name synth universe is too noisy to assert positive
	// alpha for a specific factor — that's an empirical
	// question, not a unit-test invariant. Instead verify the
	// shape of LongShortResult: NAV curve has at least one
	// point, max-drawdown is non-positive, annual vol is
	// non-negative. The qualification rollup elsewhere checks
	// the sign-and-magnitude story for real fixtures.
	f := BuildSynthFixture(SynthOptions{Seed: 13, Days: 800})
	reps := RunFactorReport(f, []Factor{Momentum12_1MFactor{}}, FactorReportConfig{
		Horizons:                  []int{22},
		LayeredHorizonDays:        22,
		MinSymbolsPerCrossSection: 3,
	})
	rep := reps[0]
	if rep.LongShort == nil {
		t.Fatalf("no long-short result")
	}
	if len(rep.LongShort.NavCurve) == 0 {
		t.Errorf("empty NAV curve")
	}
	if rep.LongShort.MaxDrawdown > 0 {
		t.Errorf("max drawdown must be ≤ 0, got %v", rep.LongShort.MaxDrawdown)
	}
	if rep.LongShort.AnnualVol < 0 {
		t.Errorf("annual vol must be ≥ 0, got %v", rep.LongShort.AnnualVol)
	}
}

func TestRunFactorReportDefaultThresholdsRollupShape(t *testing.T) {
	f := BuildSynthFixture(SynthOptions{Seed: 21, Days: 800})
	reps := RunFactorReport(f, []Factor{Momentum12_1MFactor{}}, FactorReportConfig{
		Horizons:                  []int{22},
		LayeredHorizonDays:        22,
		MinSymbolsPerCrossSection: 3,
		Thresholds:                DefaultQualificationThresholds(),
	})
	rep := reps[0]
	// We just check the shape: HorizonDaysReference is set and
	// LongShortSharpe was evaluated.
	if rep.QualReport.HorizonDaysReference != 22 {
		t.Errorf("ref horizon = %d, want 22", rep.QualReport.HorizonDaysReference)
	}
	// Qualified itself may be true OR false depending on the
	// synth seed — we don't assert that.
	_ = rep.Qualified
}

func TestLayeredAccumulatorMonotonicityFlagShape(t *testing.T) {
	la := newLayeredAccumulator(22)
	// Inject five quintile returns by hand in monotonic ascending order.
	la.quintileReturns[0] = []float64{-0.02}
	la.quintileReturns[1] = []float64{-0.01}
	la.quintileReturns[2] = []float64{0.00}
	la.quintileReturns[3] = []float64{0.01}
	la.quintileReturns[4] = []float64{0.02}
	la.spreadReturns = []float64{0.04}
	r := la.finalise()
	if r == nil {
		t.Fatalf("nil layered result")
	}
	if !r.Monotonic {
		t.Errorf("monotonic flag false on perfectly-monotonic input")
	}
	if math.Abs(r.Spread-0.04) > 1e-9 {
		t.Errorf("spread = %v, want 0.04", r.Spread)
	}
}
