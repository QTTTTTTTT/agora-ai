package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/fundai/server/internal/ohlc"
	"github.com/fundai/server/internal/regime"
)

// ---------------------------------------------------------------------------
// Bar fixtures shared by trend + mean_reversion tests
// ---------------------------------------------------------------------------

// rampUpBars produces a clean uptrend long enough for MA200 +
// Donchian-20 + slope-20 to all stabilise. The last bar's close
// is set far above the prev-20 high so the breakout fires.
func rampUpBars() []ohlc.Bar {
	n := 260
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		base := 100.0 * (1 + 0.5*float64(i)/float64(n-1))
		c := base + 0.3*math.Sin(float64(i)/5)
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.005,
			Low:    c * 0.995,
			Close:  c,
			Volume: 1e6,
		}
	}
	// Spike the last close cleanly above the 20-day high — the
	// Donchian breakout signal we want to test.
	last := n - 1
	for j := last - 21; j < last; j++ {
		bars[j].High = bars[j].Close * 1.005
	}
	bars[last].Close = bars[last-1].High * 1.10
	bars[last].High = bars[last].Close
	return bars
}

// rampDownBars mirrors rampUpBars: clean downtrend, last close
// below 20-day low.
func rampDownBars() []ohlc.Bar {
	n := 260
	start := time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)
	bars := make([]ohlc.Bar, n)
	for i := 0; i < n; i++ {
		base := 100.0 * (1 - 0.5*float64(i)/float64(n-1))
		c := base + 0.3*math.Sin(float64(i)/5)
		bars[i] = ohlc.Bar{
			Time:   start.Add(time.Duration(i) * 24 * time.Hour),
			Open:   c,
			High:   c * 1.005,
			Low:    c * 0.995,
			Close:  c,
			Volume: 1e6,
		}
	}
	last := n - 1
	for j := last - 21; j < last; j++ {
		bars[j].Low = bars[j].Close * 0.995
	}
	bars[last].Close = bars[last-1].Low * 0.90
	bars[last].Low = bars[last].Close
	return bars
}

// flatBars: horizontal series, no breakout possible.
func flatBars() []ohlc.Bar {
	n := 260
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
// TrendSleeve
// ---------------------------------------------------------------------------

func TestTrendSleeveFiresBuyOnUpsideBreakout(t *testing.T) {
	sleeve := NewTrendSleeve(defaultTrend())
	b := Bundle{
		Symbol: "NVDA",
		Bars:   rampUpBars(),
		Regime: regime.TrendUp,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if p.Action != ActionBuy {
		t.Fatalf("action: got %q, want buy", p.Action)
	}
	if p.SignalSource != "donchian_20" {
		t.Fatalf("signal_source: got %q, want donchian_20", p.SignalSource)
	}
	if p.Confidence < 0.55 || p.Confidence > 0.95 {
		t.Fatalf("confidence outside band: %v", p.Confidence)
	}
	if p.StopLoss <= 0 {
		t.Fatalf("stop_loss should be set by default params: got %v", p.StopLoss)
	}
}

func TestTrendSleeveFiresSellOnDownsideBreakout(t *testing.T) {
	sleeve := NewTrendSleeve(defaultTrend())
	b := Bundle{
		Symbol: "NVDA",
		Bars:   rampDownBars(),
		Regime: regime.TrendDown,
	}
	p := sleeve.Evaluate(b)
	if p == nil {
		t.Fatal("expected a proposal, got nil")
	}
	if p.Action != ActionSell {
		t.Fatalf("action: got %q, want sell", p.Action)
	}
}

func TestTrendSleeveNoOpOnFlatRegime(t *testing.T) {
	sleeve := NewTrendSleeve(defaultTrend())
	b := Bundle{
		Symbol: "NVDA",
		Bars:   flatBars(),
		Regime: regime.Range, // regime gate forbids trend in range
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("expected nil in Range regime, got %+v", p)
	}
}

func TestTrendSleeveNoOpWhenRegimeDisagreesWithBreakout(t *testing.T) {
	// Upside breakout bars but regime says trend_down → gate
	// blocks even though the indicator says LONG.
	sleeve := NewTrendSleeve(defaultTrend())
	b := Bundle{
		Symbol: "NVDA",
		Bars:   rampUpBars(),
		Regime: regime.TrendDown,
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("regime gate should block: got %+v", p)
	}
}

func TestTrendSleeveSkipsOnInsufficientBars(t *testing.T) {
	sleeve := NewTrendSleeve(defaultTrend())
	b := Bundle{
		Symbol: "NVDA",
		Bars:   rampUpBars()[:50], // way short of MA200 + slope
		Regime: regime.TrendUp,
	}
	if p := sleeve.Evaluate(b); p != nil {
		t.Fatalf("expected nil on short input, got %+v", p)
	}
}

func TestTrendSleevePreferredRegimes(t *testing.T) {
	sleeve := NewTrendSleeve(defaultTrend())
	got := sleeve.PreferredRegimes()
	if len(got) != 2 || got[0] != regime.TrendUp || got[1] != regime.TrendDown {
		t.Fatalf("preferred regimes: got %+v", got)
	}
}

func TestTrendConfidenceMonotonic(t *testing.T) {
	// Higher ATR strength → higher confidence (monotonic).
	weak := trendConfidence(0.2)
	mid := trendConfidence(1.0)
	strong := trendConfidence(2.5) // saturated
	if !(weak < mid && mid < strong) {
		t.Fatalf("expected weak<mid<strong: got %v < %v < %v", weak, mid, strong)
	}
	if strong > 0.95+1e-9 {
		t.Fatalf("confidence should saturate at 0.95, got %v", strong)
	}
	if weak < 0.55-1e-9 {
		t.Fatalf("confidence should not go below 0.55, got %v", weak)
	}
}

func TestStopLossPriceForBuyAndSell(t *testing.T) {
	if got := stopLossPrice(100, 0.05, ActionBuy); !approxStrategy(got, 95) {
		t.Fatalf("buy stop: got %v, want 95", got)
	}
	if got := stopLossPrice(100, 0.05, ActionSell); !approxStrategy(got, 105) {
		t.Fatalf("sell stop: got %v, want 105", got)
	}
	if got := stopLossPrice(100, 0, ActionBuy); got != 0 {
		t.Fatalf("pct=0 should disable stop: got %v", got)
	}
}

func approxStrategy(a, b float64) bool {
	return math.Abs(a-b) < 1e-6
}
