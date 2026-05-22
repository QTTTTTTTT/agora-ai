package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Bar fixtures specific to DualMA tests
// ---------------------------------------------------------------------------

// buildBarsFromCloses materialises a slice of bars from a closes
// vector so test fixtures can shape the EMA path directly. High /
// low envelope the close by 0.5% so ATR has something non-zero
// to bite on.
func buildBarsFromCloses(closes []float64) []ohlc.Bar {
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, len(closes))
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

// crossoverUpBars produces bars whose last index is precisely the
// golden-cross day for EMA(12) / EMA(26). We construct the closes
// directly and append a final spike sized so the fast EMA just
// edges above the slow EMA on the last bar. Returning the bar
// length 100 keeps the indicator windows comfortably stable.
func crossoverUpBars() []ohlc.Bar {
	// Long downtrend so EMA(12) sits below EMA(26).
	closes := make([]float64, 0, 100)
	for i := 0; i < 99; i++ {
		closes = append(closes, 100.0-0.20*float64(i))
	}
	// Final bar: spike up just enough to flip the cross. The
	// magnitude was tuned empirically — large enough to clear
	// the 1+ point EMA(26) lag, small enough that the
	// confidence stays in-band.
	closes = append(closes, closes[len(closes)-1]+20.0)
	return buildBarsFromCloses(closes)
}

// crossoverDownBars mirrors the above for a death cross: long
// uptrend (fast EMA above slow EMA) capped by a big down spike.
func crossoverDownBars() []ohlc.Bar {
	closes := make([]float64, 0, 100)
	for i := 0; i < 99; i++ {
		closes = append(closes, 100.0+0.20*float64(i))
	}
	closes = append(closes, closes[len(closes)-1]-20.0)
	return buildBarsFromCloses(closes)
}

// Trivial unused import suppression — `math` is still used in the
// established-uptrend helper below.
var _ = math.Sin

// strongUptrendBars produces a clean uptrend where fast EMA has
// been ABOVE slow EMA for many bars — the no-cross case. The
// sleeve should return nil (not BUY) because the cross has
// already happened in the past.
func strongUptrendBars() []ohlc.Bar {
	closes := make([]float64, 0, 120)
	for i := 0; i < 120; i++ {
		closes = append(closes, 100.0+0.50*float64(i))
	}
	return buildBarsFromCloses(closes)
}

// ---------------------------------------------------------------------------
// DualMASleeve
// ---------------------------------------------------------------------------

func TestDualMASleeveFiresBuyOnGoldenCross(t *testing.T) {
	sleeve := NewDualMASleeve(defaultDualMA())
	b := Bundle{
		Symbol: "AAA",
		Bars:   crossoverUpBars(),
		Regime: regime.TrendUp,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected golden cross to produce a proposal, got nil")
	}
	if p.Action != ActionBuy {
		t.Fatalf("expected ActionBuy, got %q", p.Action)
	}
	if p.Confidence < 0.55 || p.Confidence > 0.95 {
		t.Fatalf("confidence %v outside [0.55, 0.95]", p.Confidence)
	}
	if p.SignalSource != dualMASignalSource {
		t.Fatalf("expected signal_source %q, got %q", dualMASignalSource, p.SignalSource)
	}
}

func TestDualMASleeveFiresSellOnDeathCross(t *testing.T) {
	sleeve := NewDualMASleeve(defaultDualMA())
	b := Bundle{
		Symbol: "AAA",
		Bars:   crossoverDownBars(),
		Regime: regime.TrendDown,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected death cross to produce a proposal, got nil")
	}
	if p.Action != ActionSell {
		t.Fatalf("expected ActionSell, got %q", p.Action)
	}
}

// TestDualMASleeveDoesNotFireWithoutCross asserts the sleeve only
// fires on the day the cross actually happens. A persistent
// uptrend (fast > slow for many bars) must produce nil because
// today's cross condition is false (fast > slow on BOTH this bar
// and yesterday).
func TestDualMASleeveDoesNotFireWithoutCross(t *testing.T) {
	sleeve := NewDualMASleeve(defaultDualMA())
	b := Bundle{
		Symbol: "AAA",
		Bars:   strongUptrendBars(),
		Regime: regime.TrendUp,
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("expected no proposal on established uptrend (cross already past), got %+v", p)
	}
}

func TestDualMASleeveRespectsRegimeGate(t *testing.T) {
	sleeve := NewDualMASleeve(defaultDualMA())
	// Same crossover-up bars but the regime is "range" — sleeve
	// must refuse to fire, defending against EMA whipsaw in
	// sideways markets.
	b := Bundle{
		Symbol: "AAA",
		Bars:   crossoverUpBars(),
		Regime: regime.Range,
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("expected regime gate to block fire, got %+v", p)
	}
}

func TestDualMASleeveSkipsWhenHistoryTooShort(t *testing.T) {
	sleeve := NewDualMASleeve(defaultDualMA())
	// Default slow EMA is 26; we need at least 31 bars (slow+5).
	short := crossoverUpBars()[:25]
	if p := sleeve.Evaluate(Bundle{Symbol: "AAA", Bars: short, Regime: regime.TrendUp}); p != nil {
		t.Fatalf("expected nil on insufficient history, got %+v", p)
	}
}

// TestDualMAEffectivePolicySwapsInverted guards the
// EffectivePolicy normaliser: when an operator (accidentally)
// configures fastEMA >= slowEMA, the merge logic swaps them so
// the sleeve still produces a sensible signal instead of
// inverting everything.
func TestDualMAEffectivePolicySwapsInverted(t *testing.T) {
	p := Policy{
		Enabled:        true,
		EnabledSleeves: []string{"dual_ma"},
		DualMA: &DualMAParams{
			FastEMA: 26, // larger than SlowEMA — inverted
			SlowEMA: 12,
		},
	}.EffectivePolicy()
	if p.DualMA == nil {
		t.Fatal("expected DualMA params to survive normalisation")
	}
	if p.DualMA.FastEMA != 12 || p.DualMA.SlowEMA != 26 {
		t.Fatalf("expected normalisation to swap to (12, 26), got (%d, %d)", p.DualMA.FastEMA, p.DualMA.SlowEMA)
	}
}
