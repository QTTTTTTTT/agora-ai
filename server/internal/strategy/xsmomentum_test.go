package strategy

import (
	"context"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// rampBars produces `n` daily bars whose closes start at 100 and
// move by `slopePerBar` each step. Positive slope = uptrend,
// negative = downtrend, zero = flat. Used by the cross-sectional
// momentum tests to dial each name's 12-1 score deterministically.
func rampBars(n int, slopePerBar float64) []ohlc.Bar {
	closes := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100.0 + slopePerBar*float64(i)
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i, c := range closes {
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.005,
			Low:    c * 0.995,
			Close:  c,
			Volume: 1e6,
		}
	}
	return bars
}

// ---------------------------------------------------------------------------
// CrossSectionalMomentumSleeve
// ---------------------------------------------------------------------------

// TestXSMomentumPickedTopAndBottom verifies the canonical contract:
// given a 5-name universe with monotonically differing slopes,
// the sleeve emits BUY for the steepest uptrend and SELL for the
// steepest downtrend, with `nil` in the middle.
func TestXSMomentumPickedTopAndBottom(t *testing.T) {
	sleeve := NewCrossSectionalMomentumSleeve(defaultXSMomentum())
	n := 260
	bundles := []Bundle{
		{Symbol: "BEST", InstrumentKey: "US:BEST", Bars: rampBars(n, +0.30), Regime: regime.TrendUp},
		{Symbol: "GOOD", InstrumentKey: "US:GOOD", Bars: rampBars(n, +0.10), Regime: regime.TrendUp},
		{Symbol: "FLAT", InstrumentKey: "US:FLAT", Bars: rampBars(n, 0.00), Regime: regime.TrendUp},
		{Symbol: "BAD", InstrumentKey: "US:BAD", Bars: rampBars(n, -0.10), Regime: regime.TrendDown},
		{Symbol: "WORST", InstrumentKey: "US:WORST", Bars: rampBars(n, -0.30), Regime: regime.TrendDown},
	}
	proposals := sleeve.EvaluateBatch(bundles)
	if len(proposals) != len(bundles) {
		t.Fatalf("expected len(proposals)=%d, got %d", len(bundles), len(proposals))
	}
	if proposals[0] == nil || proposals[0].Action != ActionBuy {
		t.Fatalf("expected BEST to receive a BUY, got %+v", proposals[0])
	}
	if proposals[4] == nil || proposals[4].Action != ActionSell {
		t.Fatalf("expected WORST to receive a SELL, got %+v", proposals[4])
	}
	// Middle band — quintile=0.20 on 5 names yields ceil(1) per
	// side, so indices 1, 2, 3 must be nil.
	for _, mid := range []int{1, 2, 3} {
		if proposals[mid] != nil {
			t.Fatalf("expected middle band index %d to be nil, got %+v", mid, proposals[mid])
		}
	}
}

// TestXSMomentumSkipsTooSmallUniverse guards the
// MinUniverseSize gate: fewer than 5 names = all nil.
func TestXSMomentumSkipsTooSmallUniverse(t *testing.T) {
	sleeve := NewCrossSectionalMomentumSleeve(defaultXSMomentum())
	n := 260
	bundles := []Bundle{
		{Symbol: "AAA", Bars: rampBars(n, +0.30), Regime: regime.TrendUp},
		{Symbol: "BBB", Bars: rampBars(n, -0.30), Regime: regime.TrendDown},
	}
	proposals := sleeve.EvaluateBatch(bundles)
	for i, p := range proposals {
		if p != nil {
			t.Fatalf("expected nil at index %d, got %+v", i, p)
		}
	}
}

// TestXSMomentumDropsBundlesWithShortHistory verifies that
// bundles lacking enough bars for the 12-bar lookback are
// dropped from the ranking but don't crash the batch.
func TestXSMomentumDropsBundlesWithShortHistory(t *testing.T) {
	sleeve := NewCrossSectionalMomentumSleeve(defaultXSMomentum())
	n := 260
	bundles := []Bundle{
		{Symbol: "FULL_1", Bars: rampBars(n, +0.30), Regime: regime.TrendUp},
		{Symbol: "FULL_2", Bars: rampBars(n, +0.20), Regime: regime.TrendUp},
		{Symbol: "FULL_3", Bars: rampBars(n, +0.10), Regime: regime.TrendUp},
		{Symbol: "FULL_4", Bars: rampBars(n, -0.10), Regime: regime.TrendDown},
		{Symbol: "FULL_5", Bars: rampBars(n, -0.20), Regime: regime.TrendDown},
		{Symbol: "SHORT_HISTORY", Bars: rampBars(50, +0.30), Regime: regime.TrendUp}, // way under the lookback window
	}
	proposals := sleeve.EvaluateBatch(bundles)
	if proposals[5] != nil {
		t.Fatalf("expected SHORT_HISTORY to receive no proposal, got %+v", proposals[5])
	}
	if proposals[0] == nil || proposals[0].Action != ActionBuy {
		t.Fatalf("expected FULL_1 to land in the top bucket, got %+v", proposals[0])
	}
}

// TestXSMomentumPerBundleRegimeGate confirms a name whose regime
// has rotated to chop loses its BUY even when its momentum still
// ranks in the top quintile.
func TestXSMomentumPerBundleRegimeGate(t *testing.T) {
	sleeve := NewCrossSectionalMomentumSleeve(defaultXSMomentum())
	n := 260
	bundles := []Bundle{
		{Symbol: "BEST_BUT_CHOP", Bars: rampBars(n, +0.30), Regime: regime.Chop}, // top by score, gated by regime
		{Symbol: "GOOD", Bars: rampBars(n, +0.20), Regime: regime.TrendUp},
		{Symbol: "FLAT", Bars: rampBars(n, 0.00), Regime: regime.TrendUp},
		{Symbol: "BAD", Bars: rampBars(n, -0.20), Regime: regime.TrendDown},
		{Symbol: "WORST", Bars: rampBars(n, -0.30), Regime: regime.TrendDown},
	}
	proposals := sleeve.EvaluateBatch(bundles)
	// Index 0 ranks top BUT regime=chop → no fire.
	if proposals[0] != nil {
		t.Fatalf("expected per-bundle regime gate to block top-ranked chop bundle, got %+v", proposals[0])
	}
}

// TestXSMomentumServiceBatchDispatch is the end-to-end check that
// strategy.Service detects BatchSleeve and routes through it. We
// register only xs_momentum and assert that the per-bundle path
// is never invoked (the per-bundle Evaluate returns nil
// unconditionally; the BUY/SELL we see here can only come from
// EvaluateBatch).
func TestXSMomentumServiceBatchDispatch(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"xs_momentum"},
		XSMomentum:     ptrXSM(defaultXSMomentum()),
	}.EffectivePolicy()
	svc := NewService(p)
	if svc == nil {
		t.Fatal("expected service for xs_momentum policy")
	}
	n := 260
	bundles := []Bundle{
		{Symbol: "TOP1", InstrumentKey: "US:TOP1", Bars: rampBars(n, +0.40), Regime: regime.TrendUp},
		{Symbol: "TOP2", InstrumentKey: "US:TOP2", Bars: rampBars(n, +0.20), Regime: regime.TrendUp},
		{Symbol: "MID", InstrumentKey: "US:MID", Bars: rampBars(n, 0.00), Regime: regime.TrendUp},
		{Symbol: "BOT1", InstrumentKey: "US:BOT1", Bars: rampBars(n, -0.20), Regime: regime.TrendDown},
		{Symbol: "BOT2", InstrumentKey: "US:BOT2", Bars: rampBars(n, -0.40), Regime: regime.TrendDown},
	}
	actions, err := svc.Evaluate(context.Background(), bundles)
	if err != nil {
		t.Fatalf("evaluate: %v", err)
	}
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions (1 BUY + 1 SELL), got %d: %+v", len(actions), actions)
	}
	// Sorted by instrument_key; check shape.
	for _, a := range actions {
		if a.Sleeve != "xs_momentum" {
			t.Fatalf("expected sleeve=xs_momentum, got %q", a.Sleeve)
		}
		if a.Proposal.SignalSource != xsMomentumSignalSource {
			t.Fatalf("expected signal_source=%q, got %q", xsMomentumSignalSource, a.Proposal.SignalSource)
		}
	}
}

func ptrXSM(p CrossSectionalMomentumParams) *CrossSectionalMomentumParams {
	return &p
}
