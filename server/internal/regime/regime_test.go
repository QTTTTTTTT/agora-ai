package regime

import (
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
)

// ---------------------------------------------------------------------------
// Synthetic bar generators
// ---------------------------------------------------------------------------

// makeBars builds `n` daily bars starting at `start`, where each
// bar's close is produced by closeFn(i). High = close * (1 + jitter)
// and Low = close * (1 - jitter) so the ATR helper sees a non-zero
// true range — without jitter ATR collapses to zero and we can't
// exercise the chop branch.
func makeBars(n int, start time.Time, jitter float64, closeFn func(i int) float64) []ohlc.Bar {
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		c := closeFn(i)
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * (1 + jitter),
			Low:    c * (1 - jitter),
			Close:  c,
			Volume: 1_000_000,
		}
	}
	return bars
}

// uptrend builds a clean uptrend: linear rise from base to base*1.5
// across n bars, plus tiny daily jitter.
func uptrend(n int, base float64) []ohlc.Bar {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	return makeBars(n, start, 0.005, func(i int) float64 {
		// linear ramp + sinusoidal wobble so the MA can confirm
		// instead of perfectly tracking the line.
		ramp := base * (1 + 0.5*float64(i)/float64(n-1))
		return ramp + 0.3*math.Sin(float64(i)/5)
	})
}

// downtrend mirrors uptrend going from base to base*0.5.
func downtrend(n int, base float64) []ohlc.Bar {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	return makeBars(n, start, 0.005, func(i int) float64 {
		ramp := base * (1 - 0.5*float64(i)/float64(n-1))
		return ramp + 0.3*math.Sin(float64(i)/5)
	})
}

// rangeBars builds a tight horizontal range around base with TINY
// jitter — should classify as Range (low ATR%).
func rangeBars(n int, base float64) []ohlc.Bar {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	return makeBars(n, start, 0.003, func(i int) float64 {
		// Sin oscillation around `base`, amplitude ±0.5% — well
		// inside the HighVolThresholdPct band.
		return base + base*0.005*math.Sin(float64(i)/4)
	})
}

// choppyBars builds the high-vol no-trend state: random-walk-ish
// sloshing around base with LARGE jitter so ATR% clears the
// HighVolThresholdPct cutoff.
func choppyBars(n int, base float64) []ohlc.Bar {
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	return makeBars(n, start, 0.04, func(i int) float64 {
		// 5%-amplitude oscillation around base, but slow enough
		// that no clean trend forms. The high `jitter` arg to
		// makeBars stretches the H/L band so daily true range
		// is wide.
		return base * (1 + 0.05*math.Sin(float64(i)/3))
	})
}

// ---------------------------------------------------------------------------
// Classify: four-branch coverage
// ---------------------------------------------------------------------------

func TestClassifyDetectsTrendUp(t *testing.T) {
	bars := uptrend(260, 100)
	if got := Classify(bars, DefaultParams()); got != TrendUp {
		t.Fatalf("expected TrendUp, got %q", got)
	}
}

func TestClassifyDetectsTrendDown(t *testing.T) {
	bars := downtrend(260, 100)
	if got := Classify(bars, DefaultParams()); got != TrendDown {
		t.Fatalf("expected TrendDown, got %q", got)
	}
}

func TestClassifyDetectsRange(t *testing.T) {
	bars := rangeBars(260, 100)
	if got := Classify(bars, DefaultParams()); got != Range {
		t.Fatalf("expected Range, got %q", got)
	}
}

func TestClassifyDetectsChop(t *testing.T) {
	bars := choppyBars(260, 100)
	if got := Classify(bars, DefaultParams()); got != Chop {
		t.Fatalf("expected Chop, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Edge cases
// ---------------------------------------------------------------------------

func TestClassifyReturnsUnknownOnInsufficientBars(t *testing.T) {
	bars := uptrend(50, 100) // far below MinBars=221
	if got := Classify(bars, DefaultParams()); got != Unknown {
		t.Fatalf("expected Unknown on short input, got %q", got)
	}
}

func TestClassifyReturnsUnknownOnEmptyInput(t *testing.T) {
	if got := Classify(nil, DefaultParams()); got != Unknown {
		t.Fatalf("expected Unknown on nil, got %q", got)
	}
	if got := Classify([]ohlc.Bar{}, DefaultParams()); got != Unknown {
		t.Fatalf("expected Unknown on empty slice, got %q", got)
	}
}

func TestClassifyReturnsUnknownOnZeroClose(t *testing.T) {
	bars := uptrend(260, 100)
	bars[len(bars)-1].Close = 0
	if got := Classify(bars, DefaultParams()); got != Unknown {
		t.Fatalf("expected Unknown on zero close, got %q", got)
	}
}

func TestClassifyFlatMASkipsTrend(t *testing.T) {
	// Perfectly flat price -> slope is 0 -> MinSlopePct cutoff
	// pushes us to Range (or Chop if ATR is high, but jitter is
	// tiny here).
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := makeBars(260, start, 0.002, func(int) float64 { return 100 })
	got := Classify(bars, DefaultParams())
	if got != Range {
		t.Fatalf("flat price should be Range, got %q", got)
	}
}

func TestClassifyZeroParamsFallsBackToDefaults(t *testing.T) {
	// Empty Params triggers the DefaultParams() fallback.
	bars := uptrend(260, 100)
	if got := Classify(bars, Params{}); got != TrendUp {
		t.Fatalf("expected TrendUp with zero params, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// Regime helpers
// ---------------------------------------------------------------------------

func TestRegimeIsKnown(t *testing.T) {
	known := []Regime{TrendUp, TrendDown, Range, Chop}
	for _, r := range known {
		if !r.IsKnown() {
			t.Fatalf("%q should be known", r)
		}
	}
	if Unknown.IsKnown() {
		t.Fatal("Unknown should not be known")
	}
	if Regime("garbage").IsKnown() {
		t.Fatal("arbitrary string should not be known")
	}
}

func TestRegimeStringEqualsValue(t *testing.T) {
	if TrendUp.String() != "trend_up" {
		t.Fatalf("String(): got %q, want trend_up", TrendUp.String())
	}
	if Unknown.String() != "" {
		t.Fatalf("Unknown.String() should be empty, got %q", Unknown.String())
	}
}
