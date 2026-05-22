package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Bar fixtures specific to mean-reversion: an oscillating range
// with a final spike outside the BB to trigger the signal.
// ---------------------------------------------------------------------------

// oversoldRangeBars: 14 consecutive losses with a sharp final
// capitulation. The trend keeps RSI(14) below 30 while the last
// bar's panic drop pushes close well below the BB lower band.
func oversoldRangeBars() []ohlc.Bar {
	n := 80
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		var c float64
		switch {
		case i < n-14: // calm window holds BB mid near 100
			c = 100.0 + 0.3*math.Sin(float64(i)/3)
		case i < n-1: // 13 mild loss bars feed RSI
			c = 100.0 - 1.0*float64(i-(n-14)+1)
		default: // last bar: panic capitulation
			c = 50.0
		}
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.003,
			Low:    c * 0.997,
			Close:  c,
			Volume: 1e6,
		}
	}
	return bars
}

// overboughtRangeBars: mirror image of oversoldRangeBars.
func overboughtRangeBars() []ohlc.Bar {
	n := 80
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		var c float64
		switch {
		case i < n-14:
			c = 100.0 + 0.3*math.Sin(float64(i)/3)
		case i < n-1:
			c = 100.0 + 1.0*float64(i-(n-14)+1)
		default:
			c = 150.0
		}
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.003,
			Low:    c * 0.997,
			Close:  c,
			Volume: 1e6,
		}
	}
	return bars
}

// calmRangeBars: pure oscillation, never breaches BB.
func calmRangeBars() []ohlc.Bar {
	n := 80
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		c := 100.0 + 0.5*math.Sin(float64(i)/4)
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.003,
			Low:    c * 0.997,
			Close:  c,
			Volume: 1e6,
		}
	}
	return bars
}

// ---------------------------------------------------------------------------
// MeanReversionSleeve
// ---------------------------------------------------------------------------

func TestMeanReversionFiresBuyOnOversold(t *testing.T) {
	sleeve := NewMeanReversionSleeve(defaultMeanReversion())
	b := Bundle{
		Symbol: "AAPL",
		Bars:   oversoldRangeBars(),
		Regime: regime.Range,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected oversold buy proposal, got nil")
	}
	if p.Action != ActionBuy {
		t.Fatalf("action: got %q, want buy", p.Action)
	}
	if p.SignalSource != "rsi_bb_14_20" {
		t.Fatalf("signal_source: got %q, want rsi_bb_14_20", p.SignalSource)
	}
	if p.Confidence < 0.55 || p.Confidence > 0.95 {
		t.Fatalf("confidence outside band: %v", p.Confidence)
	}
}

func TestMeanReversionFiresSellOnOverbought(t *testing.T) {
	sleeve := NewMeanReversionSleeve(defaultMeanReversion())
	b := Bundle{
		Symbol: "AAPL",
		Bars:   overboughtRangeBars(),
		Regime: regime.Range,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected overbought sell proposal, got nil")
	}
	if p.Action != ActionSell {
		t.Fatalf("action: got %q, want sell", p.Action)
	}
}

func TestMeanReversionSkipsWhenCalm(t *testing.T) {
	sleeve := NewMeanReversionSleeve(defaultMeanReversion())
	b := Bundle{
		Symbol: "AAPL",
		Bars:   calmRangeBars(),
		Regime: regime.Range,
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("expected nil on calm range, got %+v", p)
	}
}

func TestMeanReversionRefusesNonRangeRegime(t *testing.T) {
	sleeve := NewMeanReversionSleeve(defaultMeanReversion())
	for _, r := range []regime.Regime{regime.TrendUp, regime.TrendDown, regime.Chop, regime.Unknown} {
		b := Bundle{Symbol: "AAPL", Bars: oversoldRangeBars(), Regime: r}
		if p := sleeve.Evaluate(b); p != nil {
			t.Fatalf("regime=%s should be gated off, got %+v", r, p)
		}
	}
}

func TestMeanReversionPreferredRegimes(t *testing.T) {
	sleeve := NewMeanReversionSleeve(defaultMeanReversion())
	got := sleeve.PreferredRegimes()
	if len(got) != 1 || got[0] != regime.Range {
		t.Fatalf("preferred regimes: got %+v, want [range]", got)
	}
}

func TestMeanReversionConfidenceMonotonic(t *testing.T) {
	// RSI=29 (just below 30) → 0.55 (entry threshold).
	// RSI=15 (half-way to floor) → ~0.75 (mid band).
	// RSI=0  (absolute floor)    → 0.95 (saturated).
	a := meanRevConfidenceOversold(29, 30)
	b := meanRevConfidenceOversold(15, 30)
	c := meanRevConfidenceOversold(0, 30)
	if !(a < b && b < c) {
		t.Fatalf("expected a<b<c: got %v < %v < %v", a, b, c)
	}
	if !approxStrategy(c, 0.95) {
		t.Fatalf("RSI=0 should saturate at 0.95, got %v", c)
	}
	if a > 0.6 {
		t.Fatalf("RSI=29 should be barely above 0.55, got %v", a)
	}
}
