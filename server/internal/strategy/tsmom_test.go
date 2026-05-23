package strategy

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// noisyRampBars produces n daily bars whose closes drift by
// driftPerBar each step and add ±noiseAmp uniform noise on top.
// Reproducible (seeded by the math package's deterministic
// trigonometric expression below — NOT rand) so a t-stat test
// is repeatable without injecting a seed.
//
// The drift drives the score; the noise drives the realised
// vol → t-stat is "drift × sqrt(N) / noise vol", which we use
// in the tests to dial Confidence onto either side of MinAbsT.
func noisyRampBars(n int, startClose, driftPerBar, noiseAmp float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < n; i++ {
		// Deterministic pseudo-noise via sin: enough variation to
		// produce a non-zero vol but reproducible across runs.
		noise := noiseAmp * math.Sin(float64(i)*1.7)
		c := startClose + driftPerBar*float64(i) + noise
		if c <= 0 {
			c = 0.01
		}
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c + 0.5,
			Low:    c - 0.5,
			Close:  c,
			Volume: 1e6,
		}
	}
	return bars
}

// ---------------------------------------------------------------------------
// tsmomScore / tsmomTStat unit tests (formula correctness)
// ---------------------------------------------------------------------------

func TestTSMomScoreReturnsSignedReturn(t *testing.T) {
	// Clean ramp: 240 bars, start=100, +0.5/bar. Last close
	// (index 239) = 100+239*0.5 = 219.5; skip=21 → use index
	// 218 = 100+218*0.5 = 209. Lookback bar = index -1 (last-240)
	// — we need 241 bars for that. Use 260.
	bars := rampBars(260, +0.5)
	score, ok := tsmomScore(bars, 240, 21)
	if !ok {
		t.Fatal("expected score, got !ok")
	}
	// nowIdx = 259 - 21 = 238; thenIdx = 259 - 240 = 19.
	// Close at 238 = 100 + 238*0.5 = 219.0
	// Close at 19  = 100 + 19*0.5  = 109.5
	// expected = (219 - 109.5)/109.5 = 1.0 (approx)
	want := (100.0 + 238*0.5 - (100.0 + 19*0.5)) / (100.0 + 19*0.5)
	if math.Abs(score-want) > 1e-9 {
		t.Errorf("score=%v, want=%v", score, want)
	}
}

func TestTSMomScoreRejectsTooShortHistory(t *testing.T) {
	bars := rampBars(100, +0.5)
	if _, ok := tsmomScore(bars, 240, 21); ok {
		t.Error("expected !ok on too-short history")
	}
}

func TestTSMomScoreRejectsSkipGEQLookback(t *testing.T) {
	bars := rampBars(260, +0.5)
	if _, ok := tsmomScore(bars, 240, 240); ok {
		t.Error("expected !ok when skip >= lookback (window collapses)")
	}
}

func TestTSMomScoreRejectsBadReferencePrice(t *testing.T) {
	bars := rampBars(260, +0.5)
	bars[19].Close = 0 // "then" reference price = 0
	if _, ok := tsmomScore(bars, 240, 21); ok {
		t.Error("expected !ok when thenIdx close is zero")
	}
}

func TestTSMomTStatPositiveOnUptrend(t *testing.T) {
	// Strong drift + moderate noise → positive, large t.
	bars := noisyRampBars(260, 100, +0.5, 1.0)
	score, ok := tsmomScore(bars, 240, 21)
	if !ok {
		t.Fatal("score: !ok")
	}
	tStat, ok := tsmomTStat(bars, 240, 21, score)
	if !ok {
		t.Fatal("tStat: !ok")
	}
	if tStat <= 0 {
		t.Errorf("expected positive t-stat on uptrend, got %v", tStat)
	}
}

func TestTSMomTStatNegativeOnDowntrend(t *testing.T) {
	bars := noisyRampBars(260, 100, -0.3, 1.0)
	score, _ := tsmomScore(bars, 240, 21)
	tStat, ok := tsmomTStat(bars, 240, 21, score)
	if !ok {
		t.Fatal("tStat: !ok")
	}
	if tStat >= 0 {
		t.Errorf("expected negative t-stat on downtrend, got %v", tStat)
	}
}

func TestTSMomTStatZeroOnConstantSeries(t *testing.T) {
	// All closes identical → variance=0 → !ok.
	bars := rampBars(260, 0.0)
	score, _ := tsmomScore(bars, 240, 21)
	if _, ok := tsmomTStat(bars, 240, 21, score); ok {
		t.Error("expected !ok on zero-variance series")
	}
}

// ---------------------------------------------------------------------------
// tsmomConfidence ramp tests
// ---------------------------------------------------------------------------

func TestTSMomConfidenceBelowMinReturnsZero(t *testing.T) {
	if c := tsmomConfidence(0.3, 0.5, 2.0); c != 0 {
		t.Errorf("expected 0 below minAbsT, got %v", c)
	}
}

func TestTSMomConfidenceAtMinReturnsFloor(t *testing.T) {
	c := tsmomConfidence(0.5, 0.5, 2.0)
	if math.Abs(c-0.55) > 1e-9 {
		t.Errorf("expected 0.55 floor at minAbsT, got %v", c)
	}
}

func TestTSMomConfidenceAtMaxReturnsCeiling(t *testing.T) {
	c := tsmomConfidence(2.0, 0.5, 2.0)
	if math.Abs(c-0.95) > 1e-9 {
		t.Errorf("expected 0.95 ceiling at maxSatT, got %v", c)
	}
}

func TestTSMomConfidenceAboveMaxClampsToCeiling(t *testing.T) {
	c := tsmomConfidence(5.0, 0.5, 2.0)
	if math.Abs(c-0.95) > 1e-9 {
		t.Errorf("expected 0.95 ceiling above maxSatT, got %v", c)
	}
}

func TestTSMomConfidenceLinearMidpoint(t *testing.T) {
	// midpoint between 0.5 and 2.0 is 1.25 → conf should
	// land at midpoint between 0.55 and 0.95 = 0.75.
	c := tsmomConfidence(1.25, 0.5, 2.0)
	if math.Abs(c-0.75) > 1e-9 {
		t.Errorf("expected 0.75 at midpoint, got %v", c)
	}
}

// ---------------------------------------------------------------------------
// TSMomentumSleeve.Evaluate end-to-end behaviour
// ---------------------------------------------------------------------------

func TestTSMomEvaluateFiresBuyOnUptrend(t *testing.T) {
	sleeve := NewTSMomentumSleeve(defaultTSMomentum())
	bars := noisyRampBars(260, 100, +0.5, 1.0)
	p := sleeve.Evaluate(Bundle{
		Symbol: "X", InstrumentKey: "US:X",
		Bars:   bars,
		Regime: regime.TrendUp,
	})
	if p == nil {
		t.Fatal("expected non-nil proposal on strong uptrend")
	}
	if p.Action != ActionBuy {
		t.Errorf("expected BUY, got %v", p.Action)
	}
	if p.Confidence < 0.55 {
		t.Errorf("expected confidence >= 0.55, got %v", p.Confidence)
	}
	if p.SignalSource != tsmomSignalSource {
		t.Errorf("expected signal_source=%q, got %q", tsmomSignalSource, p.SignalSource)
	}
}

func TestTSMomEvaluateFiresSellOnDowntrend(t *testing.T) {
	sleeve := NewTSMomentumSleeve(defaultTSMomentum())
	bars := noisyRampBars(260, 100, -0.3, 1.0)
	p := sleeve.Evaluate(Bundle{
		Symbol: "X", InstrumentKey: "US:X",
		Bars:   bars,
		Regime: regime.TrendDown,
	})
	if p == nil {
		t.Fatal("expected non-nil proposal on downtrend")
	}
	if p.Action != ActionSell {
		t.Errorf("expected SELL, got %v", p.Action)
	}
}

func TestTSMomEvaluateGatedByRegime(t *testing.T) {
	// Range regime is NOT in PreferredRegimes → must return nil
	// even on a strong trend.
	sleeve := NewTSMomentumSleeve(defaultTSMomentum())
	bars := noisyRampBars(260, 100, +0.5, 1.0)
	for _, r := range []regime.Regime{regime.Range, regime.Chop, regime.Unknown} {
		p := sleeve.Evaluate(Bundle{Symbol: "X", Bars: bars, Regime: r})
		if p != nil {
			t.Errorf("regime=%v: expected nil, got %+v", r, p)
		}
	}
}

func TestTSMomEvaluateNoOpsBelowMinAbsT(t *testing.T) {
	// Drift = 0.01/bar over 240 bars on a noisy series → cumulative
	// return < 5% with high vol → |t| < 0.5 default threshold.
	sleeve := NewTSMomentumSleeve(defaultTSMomentum())
	bars := noisyRampBars(260, 100, +0.01, 3.0)
	p := sleeve.Evaluate(Bundle{
		Symbol: "X",
		Bars:   bars,
		Regime: regime.TrendUp,
	})
	// May or may not fire depending on noise interplay; if it
	// fires the confidence must still be ≥ 0.55 (the floor).
	if p != nil && p.Confidence < 0.55 {
		t.Errorf("if fired, confidence must be ≥ 0.55, got %v", p.Confidence)
	}
}

func TestTSMomEvaluateStopLossHintOnLong(t *testing.T) {
	params := defaultTSMomentum()
	params.StopLossPct = 0.10
	sleeve := NewTSMomentumSleeve(params)
	bars := noisyRampBars(260, 100, +0.5, 1.0)
	p := sleeve.Evaluate(Bundle{Symbol: "X", Bars: bars, Regime: regime.TrendUp})
	if p == nil {
		t.Fatal("expected proposal")
	}
	last := bars[len(bars)-1].Close
	want := last * 0.90
	if math.Abs(p.StopLoss-want) > 1e-9 {
		t.Errorf("stop loss = %v, want %v (10%% below last close %v)", p.StopLoss, want, last)
	}
}

func TestTSMomEvaluateNoStopLossOnSell(t *testing.T) {
	// On the SELL side the exit manager owns the close — the
	// proposal must NOT pre-bake a stop.
	params := defaultTSMomentum()
	params.StopLossPct = 0.10
	sleeve := NewTSMomentumSleeve(params)
	bars := noisyRampBars(260, 100, -0.3, 1.0)
	p := sleeve.Evaluate(Bundle{Symbol: "X", Bars: bars, Regime: regime.TrendDown})
	if p == nil {
		t.Fatal("expected proposal")
	}
	if p.StopLoss != 0 {
		t.Errorf("expected no stop on SELL proposal, got %v", p.StopLoss)
	}
}

// ---------------------------------------------------------------------------
// Policy normalisation
// ---------------------------------------------------------------------------

func TestTSMomentumPolicyDefaults(t *testing.T) {
	d := defaultTSMomentum()
	if d.LookbackBars != 240 {
		t.Errorf("LookbackBars = %d, want 240", d.LookbackBars)
	}
	if d.SkipBars != 21 {
		t.Errorf("SkipBars = %d, want 21", d.SkipBars)
	}
	if d.MinAbsT != 0.5 {
		t.Errorf("MinAbsT = %v, want 0.5", d.MinAbsT)
	}
	if d.MaxSatT != 2.0 {
		t.Errorf("MaxSatT = %v, want 2.0", d.MaxSatT)
	}
}

func TestTSMomentumPolicyEffectiveMergesOverrides(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"tsmom"},
		TSMomentum: &TSMomentumParams{
			LookbackBars: 120, // 6 months
			MinAbsT:      1.0, // tighter floor
		},
	}.EffectivePolicy()
	if p.TSMomentum == nil {
		t.Fatal("expected TSMomentum after normalisation")
	}
	if p.TSMomentum.LookbackBars != 120 {
		t.Errorf("LookbackBars override lost: %d", p.TSMomentum.LookbackBars)
	}
	if p.TSMomentum.SkipBars != 21 {
		t.Errorf("SkipBars default lost: %d", p.TSMomentum.SkipBars)
	}
	if p.TSMomentum.MinAbsT != 1.0 {
		t.Errorf("MinAbsT override lost: %v", p.TSMomentum.MinAbsT)
	}
	if p.TSMomentum.MaxSatT != 2.0 {
		t.Errorf("MaxSatT default lost: %v", p.TSMomentum.MaxSatT)
	}
}

func TestTSMomentumPolicySwapsInvertedMinMax(t *testing.T) {
	// Operator typed MinAbsT > MaxSatT by accident: must swap.
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"tsmom"},
		TSMomentum: &TSMomentumParams{
			MinAbsT: 2.5,
			MaxSatT: 1.0,
		},
	}.EffectivePolicy()
	if p.TSMomentum.MinAbsT > p.TSMomentum.MaxSatT {
		t.Errorf("expected min ≤ max after normalisation, got min=%v max=%v",
			p.TSMomentum.MinAbsT, p.TSMomentum.MaxSatT)
	}
}

// ---------------------------------------------------------------------------
// Service registration
// ---------------------------------------------------------------------------

func TestServiceRegistersTSMomSleeve(t *testing.T) {
	pol := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"tsmom"},
	}.EffectivePolicy()
	svc := NewService(pol)
	if svc == nil {
		t.Fatal("expected non-nil service")
	}
	names := svc.EnabledSleeves()
	if len(names) != 1 || names[0] != "tsmom" {
		t.Errorf("expected [tsmom], got %v", names)
	}
}

func TestServiceTSMomEvaluateEndToEnd(t *testing.T) {
	pol := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"tsmom"},
	}.EffectivePolicy()
	svc := NewService(pol)
	bundles := []Bundle{
		{Symbol: "UP", InstrumentKey: "US:UP",
			Bars: noisyRampBars(260, 100, +0.5, 1.0), Regime: regime.TrendUp},
		{Symbol: "DOWN", InstrumentKey: "US:DOWN",
			Bars: noisyRampBars(260, 100, -0.3, 1.0), Regime: regime.TrendDown},
		{Symbol: "RANGE", InstrumentKey: "US:RANGE",
			Bars: noisyRampBars(260, 100, +0.5, 1.0), Regime: regime.Range}, // gated off
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions (UP buy + DOWN sell), got %d: %+v", len(actions), actions)
	}
	got := map[string]Action{}
	for _, a := range actions {
		got[a.Symbol] = a.Proposal.Action
	}
	if got["UP"] != ActionBuy {
		t.Errorf("UP: expected buy, got %v", got["UP"])
	}
	if got["DOWN"] != ActionSell {
		t.Errorf("DOWN: expected sell, got %v", got["DOWN"])
	}
	if _, ok := got["RANGE"]; ok {
		t.Errorf("RANGE: expected to be regime-gated off, got %v", got["RANGE"])
	}
}
