package indicator

import (
	"math"
	"testing"

	"github.com/fundai/server/internal/ohlc"
)

func approxEqual(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}

// ---------------------------------------------------------------------------
// DonchianChannel
// ---------------------------------------------------------------------------

func TestDonchianChannelTracksHighestHighAndLowestLow(t *testing.T) {
	// 5-bar window over a monotonic ramp: after position 4 the
	// upper should equal that bar's high (it's the new max) and
	// the lower should equal bar 0's low (it's still the min).
	highs := []float64{10, 11, 12, 13, 14, 15, 16}
	lows := []float64{9, 9.5, 10, 10.5, 11, 11.5, 12}
	upper, mid, lower := DonchianChannel(highs, lows, 5)
	// Pre-window positions are 0.
	for i := 0; i < 4; i++ {
		if upper[i] != 0 || lower[i] != 0 || mid[i] != 0 {
			t.Fatalf("pre-window pos %d should be 0, got u=%v l=%v m=%v", i, upper[i], lower[i], mid[i])
		}
	}
	// At i=4: window covers indices 0..4, max high=14, min low=9.
	if !approxEqual(upper[4], 14, 1e-9) {
		t.Fatalf("upper[4] = %v, want 14", upper[4])
	}
	if !approxEqual(lower[4], 9, 1e-9) {
		t.Fatalf("lower[4] = %v, want 9", lower[4])
	}
	if !approxEqual(mid[4], 11.5, 1e-9) {
		t.Fatalf("mid[4] = %v, want 11.5", mid[4])
	}
	// At i=6: window covers 2..6, max high=16, min low=10.
	if !approxEqual(upper[6], 16, 1e-9) {
		t.Fatalf("upper[6] = %v, want 16", upper[6])
	}
	if !approxEqual(lower[6], 10, 1e-9) {
		t.Fatalf("lower[6] = %v, want 10", lower[6])
	}
}

func TestDonchianChannelHandlesEmptyAndMismatchedInput(t *testing.T) {
	u, m, l := DonchianChannel(nil, nil, 20)
	if len(u) != 0 || len(m) != 0 || len(l) != 0 {
		t.Fatalf("expected empty outputs, got u=%v m=%v l=%v", u, m, l)
	}
	// Mismatched lengths: function should return zero-filled
	// outputs of len(highs) instead of crashing.
	u, _, _ = DonchianChannel([]float64{1, 2, 3}, []float64{1, 2}, 2)
	if len(u) != 3 {
		t.Fatalf("expected len 3, got %d", len(u))
	}
	for i := range u {
		if u[i] != 0 {
			t.Fatalf("mismatched input should zero out positions, got u[%d]=%v", i, u[i])
		}
	}
}

func TestDonchianChannelDefaultsZeroPeriod(t *testing.T) {
	highs := make([]float64, 30)
	lows := make([]float64, 30)
	for i := range highs {
		highs[i] = float64(i + 10) // 10..39
		lows[i] = float64(i + 1)   // 1..30 (non-zero so we can distinguish "filled with min=1" from "not filled")
	}
	upper, _, lower := DonchianChannel(highs, lows, 0) // default 20
	if upper[18] != 0 || lower[18] != 0 {
		t.Fatalf("default period should leave pos 18 zero, got u=%v l=%v", upper[18], lower[18])
	}
	if upper[19] != 29 { // highs[0..19] max = 10+19 = 29
		t.Fatalf("upper[19]: got %v, want 29", upper[19])
	}
	if lower[19] != 1 { // lows[0..19] min = 1
		t.Fatalf("lower[19]: got %v, want 1", lower[19])
	}
}

// SMA of a known constant series equals that constant once the
// window is full.
func TestSMAConstantSeriesYieldsConstant(t *testing.T) {
	in := []float64{5, 5, 5, 5, 5}
	out := SMA(in, 3)
	if len(out) != 5 {
		t.Fatalf("len = %d, want 5", len(out))
	}
	if out[0] != 0 || out[1] != 0 {
		t.Errorf("pre-window positions must be 0, got %v", out[:2])
	}
	for i := 2; i < 5; i++ {
		if out[i] != 5 {
			t.Errorf("out[%d] = %v, want 5", i, out[i])
		}
	}
}

// SMA(1, 2, 3, 4, 5) period=3 → [0,0,2,3,4].
func TestSMAExactValues(t *testing.T) {
	got := SMA([]float64{1, 2, 3, 4, 5}, 3)
	want := []float64{0, 0, 2, 3, 4}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// EMA of constant series should converge to that constant from the
// first valid index.
func TestEMAConstantSeriesYieldsConstant(t *testing.T) {
	in := []float64{10, 10, 10, 10, 10, 10}
	out := EMA(in, 3)
	for i := 2; i < len(out); i++ {
		if !approxEqual(out[i], 10, 1e-9) {
			t.Errorf("out[%d] = %v, want 10", i, out[i])
		}
	}
}

// RSI of a strictly monotonically increasing series should saturate
// near 100 (only gains, no losses).
func TestRSIMonotonicGainsSaturate(t *testing.T) {
	in := make([]float64, 30)
	for i := range in {
		in[i] = float64(i + 1)
	}
	out := RSI(in, 14)
	if out[20] < 99 {
		t.Errorf("RSI on monotonic up should be ~100, got %v", out[20])
	}
}

// RSI of a strictly monotonically decreasing series → near 0.
func TestRSIMonotonicLossesSaturate(t *testing.T) {
	in := make([]float64, 30)
	for i := range in {
		in[i] = float64(30 - i)
	}
	out := RSI(in, 14)
	if out[20] > 1 {
		t.Errorf("RSI on monotonic down should be ~0, got %v", out[20])
	}
}

// MACD histogram on a step-change series should flip sign at the
// crossover, which is the only thing the cross detector cares about.
func TestMACDProducesAlignedLengthAndSignal(t *testing.T) {
	closes := make([]float64, 100)
	for i := range closes {
		closes[i] = 10 + 0.1*float64(i)
	}
	out := MACD(closes, 12, 26, 9)
	if len(out.Line) != 100 || len(out.Signal) != 100 || len(out.Histogram) != 100 {
		t.Errorf("MACD result lengths mismatch")
	}
	// Last point should be a positive MACD on a steady uptrend.
	if out.Line[99] <= 0 {
		t.Errorf("uptrend should yield positive MACD line, got %v", out.Line[99])
	}
}

// KDJ on a constant series → K=D=50 by construction (RSV undefined,
// we coerce to 50; smoothed average of 50 stays at 50).
func TestKDJConstantSeries(t *testing.T) {
	closes := make([]float64, 20)
	highs := make([]float64, 20)
	lows := make([]float64, 20)
	for i := range closes {
		closes[i] = 100
		highs[i] = 100
		lows[i] = 100
	}
	out := KDJ(highs, lows, closes, 9, 3, 3)
	if !approxEqual(out.K[19], 50, 1e-6) || !approxEqual(out.D[19], 50, 1e-6) {
		t.Errorf("KDJ on constant series should be 50/50; got K=%v D=%v", out.K[19], out.D[19])
	}
	if !approxEqual(out.J[19], 50, 1e-6) {
		t.Errorf("J should be 3K-2D = 50; got %v", out.J[19])
	}
}

// BollingerBands: on constant series, std = 0 → upper = mid = lower.
func TestBollingerBandsConstantSeriesZeroStdDev(t *testing.T) {
	in := make([]float64, 30)
	for i := range in {
		in[i] = 7
	}
	upper, mid, lower := BollingerBands(in, 20, 2)
	if !approxEqual(upper[29], 7, 1e-9) || !approxEqual(mid[29], 7, 1e-9) || !approxEqual(lower[29], 7, 1e-9) {
		t.Errorf("constant series should collapse bands to mid, got upper=%v mid=%v lower=%v", upper[29], mid[29], lower[29])
	}
}

// BollingerBands: width on a sin-wave should be > 0 (real volatility).
func TestBollingerBandsHasWidthWithVolatility(t *testing.T) {
	in := make([]float64, 60)
	for i := range in {
		in[i] = 100 + 5*math.Sin(float64(i)/3)
	}
	upper, _, lower := BollingerBands(in, 20, 2)
	if upper[59]-lower[59] <= 0 {
		t.Errorf("volatile series should yield positive band width; got %v", upper[59]-lower[59])
	}
}

// ATR: on a series with H-L=1 every bar and unchanged close, ATR
// should converge to 1.
func TestATRConvergesToTrueRangeAverage(t *testing.T) {
	const n = 40
	highs := make([]float64, n)
	lows := make([]float64, n)
	closes := make([]float64, n)
	for i := range highs {
		highs[i] = 10
		lows[i] = 9
		closes[i] = 9.5
	}
	out := ATR(highs, lows, closes, 14)
	if !approxEqual(out[n-1], 1, 1e-6) {
		t.Errorf("ATR should converge to 1.0, got %v", out[n-1])
	}
}

// VolumeMA + RelativeVolume work end-to-end on a real bar slice.
// The 20-bar SMA at i=24 includes bars[5..24], so the last bar's
// own volume is in the denominator (the standard rolling-window
// definition). With 19 bars at 100 and the last at 300, the SMA is
// (19*100 + 300)/20 = 110, and relative volume = 300/110 ≈ 2.727.
func TestVolumeMARelativeVolume(t *testing.T) {
	bars := make([]ohlc.Bar, 25)
	for i := range bars {
		bars[i] = ohlc.Bar{Close: 1, Volume: 100}
	}
	bars[24].Volume = 300
	rv := RelativeVolume(bars, 20)
	want := 300.0 / 110.0
	if !approxEqual(rv[24], want, 0.001) {
		t.Errorf("relative volume = %v, want ~%v (300 / SMA=110)", rv[24], want)
	}
	if rv[24] <= 1 {
		t.Errorf("3x spike should still be > 1 RV; got %v", rv[24])
	}
}

// Compute produces a fully populated snapshot when given enough
// history.
func TestSnapshotComputeProducesTagsAndFlags(t *testing.T) {
	bars := buildSampleBars(120)
	snap := Compute(bars)
	if snap.LastClose <= 0 {
		t.Errorf("LastClose should be > 0, got %v", snap.LastClose)
	}
	if snap.RSI14 <= 0 || snap.RSI14 >= 100 {
		t.Errorf("RSI should be in (0,100), got %v", snap.RSI14)
	}
	if snap.MACDHist == 0 && snap.MACDLine == 0 {
		t.Errorf("MACD should be non-zero on real series")
	}
	if snap.BBUpper <= snap.BBMid || snap.BBMid <= snap.BBLower {
		t.Errorf("BB ordering wrong: %+v", snap)
	}
	if snap.KDJK == 0 && snap.KDJD == 0 {
		t.Errorf("KDJ should compute on 120 bars")
	}
	if len(snap.Tags) == 0 {
		t.Errorf("tags should be populated; got snap=%+v", snap)
	}
}

// Snapshot.FormatForPrompt returns "" when the snapshot is degenerate.
func TestSnapshotFormatHandlesEmpty(t *testing.T) {
	empty := Snapshot{}
	if empty.FormatForPrompt("ABC") != "" {
		t.Errorf("empty snapshot should produce empty prompt line")
	}
	snap := Compute(buildSampleBars(120))
	got := snap.FormatForPrompt("aapl")
	if got == "" || got[0] != 'A' {
		t.Errorf("FormatForPrompt should uppercase symbol and produce non-empty line, got %q", got)
	}
}

// Compute on a short bars slice returns an empty Snapshot without
// panicking.
func TestSnapshotComputeShortInput(t *testing.T) {
	snap := Compute([]ohlc.Bar{{Close: 1, High: 1, Low: 1}})
	if snap.LastClose != 0 {
		t.Errorf("expected zero snapshot for <5 bars, got %+v", snap)
	}
}

// buildSampleBars produces a synthetic OHLCV series with both up
// and down phases so RSI/MACD/etc have something to chew on.
func buildSampleBars(n int) []ohlc.Bar {
	out := make([]ohlc.Bar, n)
	price := 100.0
	for i := 0; i < n; i++ {
		// Two phases: gentle uptrend for the first half, drift down
		// in the second half, with sinusoidal noise on top.
		if i < n/2 {
			price += 0.2
		} else {
			price -= 0.15
		}
		jitter := math.Sin(float64(i)/3) * 0.8
		closePx := price + jitter
		out[i] = ohlc.Bar{
			Open:   closePx - 0.3,
			High:   closePx + 0.6,
			Low:    closePx - 0.7,
			Close:  closePx,
			Volume: 1_000_000 * (1 + 0.2*math.Sin(float64(i)/5)),
		}
	}
	return out
}
