// Package indicator computes classic technical indicators from OHLCV
// bars. It is intentionally a pure library — no IO, no globals, no
// external deps beyond stdlib. Callers (the runtimeResearcherPool's
// quant signal builder and the Phase 2B debate quant role) own data
// fetching via the ohlc package and pass slices into these helpers.
//
// Numerical contract:
//
//   - Input bars MUST be oldest-first (matching ohlc.Provider).
//   - Output slices are the same length as the input; positions that
//     don't have enough history to compute a value are zero (Go's
//     untyped float64 zero). Callers should consult the
//     corresponding Ready() helper or just inspect the tail (e.g.,
//     the last element).
//   - All functions are pure; they allocate one output slice and
//     return it. Pass slices of bars without worrying about hidden
//     mutation.
//
// Indicators implemented: SMA, EMA, RSI (Wilder), MACD, KDJ (the
// canonical 9/3/3 variant used by Chinese platforms), Bollinger
// Bands (BB, 20/2), ATR (Wilder), Volume MA + Relative Volume.
//
// A higher-level Snapshot function packs the latest values + simple
// derived flags (overbought, MACD cross, etc.) into a struct the
// quant role / debate prompt can serialize directly.
package indicator

import (
	"math"

	"github.com/fundai/server/internal/ohlc"
)

// SMA returns the simple moving average over closes. Period <=1 or
// <= 0 produces a copy of closes (each point is its own SMA). When
// fewer than period bars precede a point, the SMA is 0 for that
// position.
func SMA(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	if period <= 1 {
		copy(out, closes)
		return out
	}
	if len(closes) == 0 {
		return out
	}
	var window float64
	for i, c := range closes {
		window += c
		if i >= period {
			window -= closes[i-period]
		}
		if i >= period-1 {
			out[i] = window / float64(period)
		}
	}
	return out
}

// EMA returns the exponential moving average. The first valid
// position is at i = period-1 and is seeded with the SMA of the
// first `period` closes (the convention every commercial platform
// uses). Earlier positions are 0.
func EMA(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	if period <= 1 || len(closes) == 0 {
		copy(out, closes)
		return out
	}
	if len(closes) < period {
		return out
	}
	// Seed with SMA over the first `period` closes.
	var seed float64
	for i := 0; i < period; i++ {
		seed += closes[i]
	}
	seed /= float64(period)
	out[period-1] = seed
	k := 2.0 / float64(period+1)
	for i := period; i < len(closes); i++ {
		out[i] = closes[i]*k + out[i-1]*(1-k)
	}
	return out
}

// RSI returns Wilder's Relative Strength Index. RSI(period=14) is
// the standard. The first valid position is at i = period; earlier
// positions are 0. The seed uses simple averages of the first
// `period` gains/losses, then Wilder's smoothing for subsequent
// updates — matching the formula every trading platform uses.
func RSI(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	if period <= 0 || len(closes) <= period {
		return out
	}
	var gainSum, lossSum float64
	for i := 1; i <= period; i++ {
		change := closes[i] - closes[i-1]
		if change > 0 {
			gainSum += change
		} else {
			lossSum -= change
		}
	}
	avgGain := gainSum / float64(period)
	avgLoss := lossSum / float64(period)
	out[period] = rsiFromAverages(avgGain, avgLoss)
	for i := period + 1; i < len(closes); i++ {
		change := closes[i] - closes[i-1]
		var gain, loss float64
		if change > 0 {
			gain = change
		} else {
			loss = -change
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		out[i] = rsiFromAverages(avgGain, avgLoss)
	}
	return out
}

func rsiFromAverages(gain, loss float64) float64 {
	if loss == 0 {
		if gain == 0 {
			return 50
		}
		return 100
	}
	rs := gain / loss
	return 100 - (100 / (1 + rs))
}

// MACDResult holds the three MACD curves produced together. Each
// slice is the same length as the input closes.
type MACDResult struct {
	Line      []float64 // EMA(fast) - EMA(slow)
	Signal    []float64 // EMA(MACD line, signal period)
	Histogram []float64 // Line - Signal
}

// MACD returns the standard 12/26/9 MACD by default; pass other
// periods to override. The result.Line first becomes valid at
// i = slow-1 (when both EMAs exist); Signal first becomes valid at
// i = slow-1 + signal-1.
func MACD(closes []float64, fast, slow, signal int) MACDResult {
	if fast <= 0 {
		fast = 12
	}
	if slow <= 0 {
		slow = 26
	}
	if signal <= 0 {
		signal = 9
	}
	emaFast := EMA(closes, fast)
	emaSlow := EMA(closes, slow)
	line := make([]float64, len(closes))
	for i := range closes {
		if i >= slow-1 {
			line[i] = emaFast[i] - emaSlow[i]
		}
	}
	// Compute EMA over the line, but only over the valid region.
	signalLine := emaOverValid(line, slow-1, signal)
	hist := make([]float64, len(closes))
	for i := range closes {
		if i >= slow-1+signal-1 {
			hist[i] = line[i] - signalLine[i]
		}
	}
	return MACDResult{Line: line, Signal: signalLine, Histogram: hist}
}

// emaOverValid runs an EMA starting at startIdx so the seeding uses
// the first `period` valid values rather than the leading zeros
// EMA() would otherwise consume. Used internally by MACD.
func emaOverValid(values []float64, startIdx, period int) []float64 {
	out := make([]float64, len(values))
	if period <= 1 || len(values) <= startIdx+period-1 {
		return out
	}
	var seed float64
	for i := 0; i < period; i++ {
		seed += values[startIdx+i]
	}
	seed /= float64(period)
	out[startIdx+period-1] = seed
	k := 2.0 / float64(period+1)
	for i := startIdx + period; i < len(values); i++ {
		out[i] = values[i]*k + out[i-1]*(1-k)
	}
	return out
}

// KDJResult mirrors what Chinese trading platforms display.
type KDJResult struct {
	K []float64
	D []float64
	J []float64
}

// KDJ implements the 9/3/3 Stochastic-style indicator used widely in
// A-share research. RSV (raw stochastic value) is computed over the
// last `n` bars; K is the EMA-style smoothing with `mK` smoothing,
// D is the same smoothing applied to K; J = 3K - 2D.
//
// Defaults: n=9, mK=3, mD=3 (Chinese broker convention). The first
// valid index is i = n-1 (the first window of size n).
func KDJ(highs, lows, closes []float64, n, mK, mD int) KDJResult {
	if n <= 0 {
		n = 9
	}
	if mK <= 0 {
		mK = 3
	}
	if mD <= 0 {
		mD = 3
	}
	out := KDJResult{
		K: make([]float64, len(closes)),
		D: make([]float64, len(closes)),
		J: make([]float64, len(closes)),
	}
	if len(closes) == 0 || len(closes) != len(highs) || len(closes) != len(lows) {
		return out
	}
	prevK := 50.0
	prevD := 50.0
	for i := range closes {
		if i < n-1 {
			out.K[i], out.D[i], out.J[i] = 0, 0, 0
			continue
		}
		lo := lows[i-n+1]
		hi := highs[i-n+1]
		for j := i - n + 2; j <= i; j++ {
			if lows[j] < lo {
				lo = lows[j]
			}
			if highs[j] > hi {
				hi = highs[j]
			}
		}
		var rsv float64
		if hi-lo == 0 {
			rsv = 50
		} else {
			rsv = (closes[i] - lo) / (hi - lo) * 100
		}
		k := (float64(mK-1)*prevK + rsv) / float64(mK)
		d := (float64(mD-1)*prevD + k) / float64(mD)
		j := 3*k - 2*d
		out.K[i] = k
		out.D[i] = d
		out.J[i] = j
		prevK = k
		prevD = d
	}
	return out
}

// BollingerBands returns the upper / mid (=SMA) / lower bands.
// stddev_period defaults to `period`. Multiplier defaults to 2.
// Positions before the first full window are 0 on all three bands.
func BollingerBands(closes []float64, period int, multiplier float64) (upper, mid, lower []float64) {
	if period <= 0 {
		period = 20
	}
	if multiplier <= 0 {
		multiplier = 2
	}
	mid = SMA(closes, period)
	upper = make([]float64, len(closes))
	lower = make([]float64, len(closes))
	for i := range closes {
		if i < period-1 {
			continue
		}
		// Standard deviation of the last `period` closes around mid[i].
		var sumSq float64
		for j := i - period + 1; j <= i; j++ {
			diff := closes[j] - mid[i]
			sumSq += diff * diff
		}
		std := math.Sqrt(sumSq / float64(period))
		upper[i] = mid[i] + multiplier*std
		lower[i] = mid[i] - multiplier*std
	}
	return upper, mid, lower
}

// DonchianChannel returns the Donchian channel over `period`
// bars: upper[i] = max(highs[i-period+1 .. i]) and
// lower[i] = min(lows[i-period+1 .. i]). The midline is the
// arithmetic mean of the two bands.
//
// Positions before period-1 are zeroed on all three channels so
// indicator readers can iterate without out-of-bounds guards.
//
// The Donchian channel is the price-extremum primitive behind the
// trend-following sleeve in Phase 3A-4: a close above upper is
// the breakout-buy signal; a close below lower is the
// breakdown-sell signal. We do NOT use prevUpper / prevLower
// (the "yesterday's channel" Turtle convention) — the strategy
// sleeve handles signal-vs-fill timing one layer up.
func DonchianChannel(highs, lows []float64, period int) (upper, mid, lower []float64) {
	if period <= 0 {
		period = 20
	}
	n := len(highs)
	upper = make([]float64, n)
	mid = make([]float64, n)
	lower = make([]float64, n)
	if n == 0 || n != len(lows) {
		return upper, mid, lower
	}
	for i := 0; i < n; i++ {
		if i < period-1 {
			continue
		}
		hi := highs[i-period+1]
		lo := lows[i-period+1]
		for j := i - period + 2; j <= i; j++ {
			if highs[j] > hi {
				hi = highs[j]
			}
			if lows[j] < lo {
				lo = lows[j]
			}
		}
		upper[i] = hi
		lower[i] = lo
		mid[i] = (hi + lo) / 2
	}
	return upper, mid, lower
}

// ATR returns Wilder's Average True Range over `period` bars.
// True Range = max(H-L, |H - prevClose|, |L - prevClose|).
// Standard period is 14; the first valid index is i = period.
func ATR(highs, lows, closes []float64, period int) []float64 {
	if period <= 0 {
		period = 14
	}
	out := make([]float64, len(closes))
	if len(closes) == 0 || len(closes) != len(highs) || len(closes) != len(lows) {
		return out
	}
	trs := make([]float64, len(closes))
	trs[0] = highs[0] - lows[0]
	for i := 1; i < len(closes); i++ {
		hl := highs[i] - lows[i]
		hc := math.Abs(highs[i] - closes[i-1])
		lc := math.Abs(lows[i] - closes[i-1])
		trs[i] = max3(hl, hc, lc)
	}
	if len(closes) <= period {
		return out
	}
	// Seed at period with SMA of the first `period` TRs.
	var seed float64
	for i := 1; i <= period; i++ {
		seed += trs[i]
	}
	out[period] = seed / float64(period)
	for i := period + 1; i < len(closes); i++ {
		out[i] = (out[i-1]*float64(period-1) + trs[i]) / float64(period)
	}
	return out
}

func max3(a, b, c float64) float64 {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	return m
}

// VolumeMA returns the simple moving average of volume — convenient
// for comparing the latest bar's volume against its recent baseline.
func VolumeMA(bars []ohlc.Bar, period int) []float64 {
	vols := make([]float64, len(bars))
	for i, b := range bars {
		vols[i] = b.Volume
	}
	return SMA(vols, period)
}

// RelativeVolume returns volume[i] / volumeMA[i] (>1 means above
// average, <1 means below). Positions where the SMA is zero produce
// 0 to keep callers safe from div-by-zero.
func RelativeVolume(bars []ohlc.Bar, period int) []float64 {
	ma := VolumeMA(bars, period)
	out := make([]float64, len(bars))
	for i, b := range bars {
		if ma[i] > 0 {
			out[i] = b.Volume / ma[i]
		}
	}
	return out
}

// Closes is a convenience extractor.
func Closes(bars []ohlc.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Close
	}
	return out
}

// Highs / Lows extractors used by KDJ/ATR.
func Highs(bars []ohlc.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.High
	}
	return out
}

func Lows(bars []ohlc.Bar) []float64 {
	out := make([]float64, len(bars))
	for i, b := range bars {
		out[i] = b.Low
	}
	return out
}
