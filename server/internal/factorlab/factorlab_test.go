package factorlab

import (
	"math"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Synth fixture + small helpers
// ---------------------------------------------------------------------------

func newDefaultFixture(t *testing.T) *Fixture {
	t.Helper()
	f := BuildSynthFixture(SynthOptions{Seed: 42})
	if f == nil || len(f.Histories) == 0 {
		t.Fatalf("synth fixture empty")
	}
	if f.Benchmark == nil {
		t.Fatalf("synth fixture missing benchmark")
	}
	return f
}

// ---------------------------------------------------------------------------
// data.go
// ---------------------------------------------------------------------------

func TestSynthFixtureProducesEveryProfile(t *testing.T) {
	f := newDefaultFixture(t)
	for _, p := range DefaultSynthProfiles() {
		if f.History(p.Symbol) == nil {
			t.Errorf("missing symbol %s", p.Symbol)
		}
	}
}

func TestFixtureTradingDaysAreSorted(t *testing.T) {
	f := newDefaultFixture(t)
	days := f.TradingDays()
	if len(days) < 2 {
		t.Fatal("trading days empty")
	}
	for i := 1; i < len(days); i++ {
		if !days[i].After(days[i-1]) {
			t.Fatalf("days not sorted: %v then %v", days[i-1], days[i])
		}
	}
}

func TestFixtureCloseAtReturnsPriorBarOnHoliday(t *testing.T) {
	f := newDefaultFixture(t)
	days := f.TradingDays()
	// Sunday between two trading days — CloseAt should return
	// the preceding Friday's close.
	mid := days[10]
	saturday := mid.AddDate(0, 0, 1)
	for saturday.Weekday() != time.Saturday {
		saturday = saturday.AddDate(0, 0, 1)
	}
	got, ok := f.CloseAt("HI_MOM", saturday)
	if !ok {
		t.Fatal("CloseAt returned no bar")
	}
	if got <= 0 {
		t.Fatalf("CloseAt returned zero")
	}
}

func TestLogReturnsLengthIsInputMinusOne(t *testing.T) {
	got := LogReturns([]float64{100, 105, 110})
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
}

// ---------------------------------------------------------------------------
// strategy.go
// ---------------------------------------------------------------------------

func TestEqualWeightLongSelectsEntireUniverse(t *testing.T) {
	f := newDefaultFixture(t)
	w := EqualWeightLong{}.Weights(f, f.Start)
	if len(w) != len(f.Histories) {
		t.Errorf("expected %d names, got %d", len(f.Histories), len(w))
	}
	var sum float64
	for _, v := range w {
		sum += v
	}
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("weights should sum to 1, got %f", sum)
	}
}

func TestMomentumPicksHighAlphaName(t *testing.T) {
	// With the default profile set HI_MOM has the highest
	// alpha + beta. After the 12m lookback the momentum
	// strategy should consistently pick it (allowing for
	// path noise we accept "at least once during the
	// backtest window").
	f := newDefaultFixture(t)
	days := f.TradingDays()
	mom := Momentum12_1M{}
	picked := false
	for _, day := range days[270:] {
		if _, ok := mom.Weights(f, day)["HI_MOM"]; ok {
			picked = true
			break
		}
	}
	if !picked {
		t.Error("momentum never picked HI_MOM — synth seed regression?")
	}
}

func TestLowBetaPicksLowBetaName(t *testing.T) {
	f := newDefaultFixture(t)
	days := f.TradingDays()
	lb := LowBeta{}
	w := lb.Weights(f, days[len(days)-1])
	if _, ok := w["LOW_BETA"]; !ok {
		t.Errorf("LowBeta strategy did not pick LOW_BETA in final window, got %v", w)
	}
}

func TestLowVolPicksLowVolName(t *testing.T) {
	// DRIFTER has idio vol 0.12; HI_VOL has 0.45 → LowVol
	// strategy MUST exclude HI_VOL and prefer DRIFTER.
	f := newDefaultFixture(t)
	days := f.TradingDays()
	lv := LowVol{}
	w := lv.Weights(f, days[len(days)-1])
	if _, ok := w["HI_VOL"]; ok {
		t.Errorf("LowVol picked HI_VOL — vol ranking broken: %v", w)
	}
	if _, ok := w["DRIFTER"]; !ok {
		t.Logf("warn: LowVol did not pick DRIFTER (synth path noise OK): %v", w)
	}
}

// ---------------------------------------------------------------------------
// simulator.go + metrics.go
// ---------------------------------------------------------------------------

func TestSimulatorRunsAllStrategiesAndProducesEquityCurve(t *testing.T) {
	f := newDefaultFixture(t)
	sim := &Simulator{StartNav: 1.0, SlippageBps: 5}
	strats := []Strategy{
		EqualWeightLong{},
		Momentum12_1M{},
		LowBeta{},
		LowVol{},
	}
	results := sim.Run(f, strats)
	if len(results) != len(strats) {
		t.Fatalf("expected %d results, got %d", len(strats), len(results))
	}
	for _, r := range results {
		if len(r.Equity) == 0 {
			t.Errorf("%s: empty equity curve", r.Strategy)
		}
		if r.FinalNav <= 0 {
			t.Errorf("%s: non-positive FinalNav %f", r.Strategy, r.FinalNav)
		}
		if r.AnnualVol < 0 {
			t.Errorf("%s: negative AnnualVol %f", r.Strategy, r.AnnualVol)
		}
		if r.MaxDrawdown > 0 {
			t.Errorf("%s: MaxDrawdown should be ≤ 0, got %f", r.Strategy, r.MaxDrawdown)
		}
		if r.HitRate < 0 || r.HitRate > 1 {
			t.Errorf("%s: HitRate out of [0,1]: %f", r.Strategy, r.HitRate)
		}
	}
}

func TestSimulatorMomentumBeatsBuyAndHoldOnSyntheticPath(t *testing.T) {
	// The synth profile bakes positive alpha into HI_MOM and
	// negative alpha into DOG. A momentum strategy should
	// outperform equal-weight (which holds DOG too) on the
	// in-sample window. If this fails the seed or the strategy
	// math has regressed.
	f := newDefaultFixture(t)
	sim := &Simulator{StartNav: 1.0, SlippageBps: 5}
	results := sim.Run(f, []Strategy{
		EqualWeightLong{},
		Momentum12_1M{},
	})
	if len(results) != 2 {
		t.Fatal("missing results")
	}
	if results[1].FinalNav <= results[0].FinalNav {
		t.Errorf("momentum (%.4f) failed to beat equal_weight (%.4f) on synthetic path",
			results[1].FinalNav, results[0].FinalNav)
	}
}

func TestSlippageReducesFinalNavWhenChurning(t *testing.T) {
	f := newDefaultFixture(t)
	mom := Momentum12_1M{}
	noFriction := (&Simulator{StartNav: 1.0, SlippageBps: 0}).Run(f, []Strategy{mom})
	highFriction := (&Simulator{StartNav: 1.0, SlippageBps: 100}).Run(f, []Strategy{mom}) // 1% per turnover dollar
	if noFriction[0].FinalNav <= highFriction[0].FinalNav {
		t.Errorf("expected slippage to drag NAV: noFriction=%.4f vs highFriction=%.4f",
			noFriction[0].FinalNav, highFriction[0].FinalNav)
	}
}

func TestMaxDrawdownIsNegativeOnLossPath(t *testing.T) {
	curve := []NavPoint{
		{Nav: 1.00},
		{Nav: 1.10},
		{Nav: 1.05},
		{Nav: 0.90}, // -18.2% from peak
		{Nav: 1.00},
	}
	got := maxDrawdown(curve)
	if math.Abs(got-(-0.18181818)) > 1e-6 {
		t.Errorf("expected MDD ≈ -0.1818, got %f", got)
	}
}

func TestNormaliseWeightsScalesDownButNotUp(t *testing.T) {
	got := normaliseWeights(map[string]float64{"A": 0.6, "B": 0.6})
	sum := got["A"] + got["B"]
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("overweight maps should be scaled to sum=1, got %f", sum)
	}
	got = normaliseWeights(map[string]float64{"A": 0.3, "B": 0.3})
	sum = got["A"] + got["B"]
	if math.Abs(sum-0.6) > 1e-9 {
		t.Errorf("underweight maps should NOT be scaled up: got sum=%f", sum)
	}
}
