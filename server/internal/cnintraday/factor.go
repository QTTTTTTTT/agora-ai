package cnintraday

import (
	"math"
	"time"
)

// ComputeFactors is the pure-math kernel: given a MinuteWindow
// (at minute t), return the 5 intraday factor scores documented
// in the plan. The function makes NO I/O calls — all data must be
// in the window. Caller controls the window size; the function
// silently returns zero on any factor whose required window is
// shorter than available bars.
//
// Factors:
//
//   1. Breakout       : (close − prior60HighExcludingCurrent) /
//                       std(close, 60) — z-score above the
//                       trailing-hour high.
//   2. VolumeSurge    : last 5-min average volume / 20-min
//                       average volume.
//   3. BigInflow      : sum of BigOrderNet over last 5 bars
//                       (units = CNY). Zero when the provider
//                       didn't fill BigOrderNet.
//   4. OrderImbalance : average BidAskRatio over last 3 bars.
//                       Zero when the provider didn't fill the
//                       L1 snapshot.
//   5. SectorRank     : delegated to a SectorRankProvider so the
//                       caller can pin it from a sector-leaders
//                       dictionary. ComputeFactors itself sets
//                       this to 0.5 (neutral) — callers patch
//                       in the real rank before passing to
//                       Evaluate.
func ComputeFactors(window *MinuteWindow) FactorTuple {
	if window == nil || len(window.Bars) == 0 {
		return FactorTuple{SectorRank: 0.5}
	}
	bars := window.Bars
	last := bars[len(bars)-1]
	t := FactorTuple{SectorRank: 0.5}

	// 1. Breakout vs prior 60-bar high (excluding current bar).
	if len(bars) >= 61 {
		hist := bars[len(bars)-61 : len(bars)-1]
		prevHigh := hist[0].High
		for _, b := range hist {
			if b.High > prevHigh {
				prevHigh = b.High
			}
		}
		// Compute trailing close stdev to z-score the breakout.
		closes := make([]float64, 0, len(hist))
		for _, b := range hist {
			closes = append(closes, b.Close)
		}
		s := stdev(closes)
		if s > 0 && prevHigh > 0 {
			t.Breakout = (last.Close - prevHigh) / s
		}
	}

	// 2. VolumeSurge = mean(vol[-5:]) / mean(vol[-20:]).
	if len(bars) >= 20 {
		var v5, v20 float64
		for i := len(bars) - 20; i < len(bars); i++ {
			v20 += bars[i].Volume
		}
		for i := len(bars) - 5; i < len(bars); i++ {
			v5 += bars[i].Volume
		}
		m20 := v20 / 20.0
		m5 := v5 / 5.0
		if m20 > 0 {
			t.VolumeSurge = m5 / m20
		}
	}

	// 3. BigInflow = sum of last 5 bars' BigOrderNet (CNY).
	if len(bars) >= 5 {
		for i := len(bars) - 5; i < len(bars); i++ {
			t.BigInflow += bars[i].BigOrderNet
		}
	}

	// 4. OrderImbalance = mean of last 3 bars' BidAskRatio.
	if len(bars) >= 3 {
		var sum float64
		var n int
		for i := len(bars) - 3; i < len(bars); i++ {
			if bars[i].BidAskRatio > 0 {
				sum += bars[i].BidAskRatio
				n++
			}
		}
		if n > 0 {
			t.OrderImbalance = sum / float64(n)
		}
	}
	return t
}

// IntradayTimeFilter excludes minute timestamps outside the
// engine's operating window. A-share trading happens 9:30-11:30
// + 13:00-15:00 Beijing time. We additionally trim:
//
//   - 9:30-9:35  : open auction noise
//   - 14:55-15:00: close auction (cannot intervene)
//
// Returns true when the bar should be considered for signal
// emission.
func IntradayTimeFilter(t time.Time) bool {
	beijing := t.In(beijingTimezone())
	hm := beijing.Hour()*60 + beijing.Minute()
	switch {
	case hm < 9*60+35: // before 9:35
		return false
	case hm > 11*60+30 && hm < 13*60: // lunch break
		return false
	case hm >= 14*60+55: // last 5 min
		return false
	}
	return true
}

func beijingTimezone() *time.Location {
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		// Fall back to fixed UTC+8 if tzdata isn't bundled (e.g.
		// scratch container without /usr/share/zoneinfo).
		loc = time.FixedZone("CST", 8*3600)
	}
	return loc
}

// stdev is the sample std of a slice. Returns 0 for slices of
// length < 2. Used by factor calculation; not exported because
// other packages should reach for math/stat for general work.
func stdev(v []float64) float64 {
	if len(v) < 2 {
		return 0
	}
	var sum float64
	for _, x := range v {
		sum += x
	}
	m := sum / float64(len(v))
	var ss float64
	for _, x := range v {
		d := x - m
		ss += d * d
	}
	return math.Sqrt(ss / float64(len(v)-1))
}
