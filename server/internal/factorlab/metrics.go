package factorlab

import (
	"math"
	"time"
)

// NavPoint is one observation on the equity curve.
type NavPoint struct {
	Date time.Time `json:"date"`
	Nav  float64   `json:"nav"`
}

// Result is the per-strategy backtest output. Equity carries the
// full curve (callers can re-plot it); the metrics below are
// pre-computed for the headline report. Annualisation uses 252
// trading days.
type Result struct {
	Strategy   string
	StartDate  time.Time
	EndDate    time.Time
	StartNav   float64
	Slippage   float64 // bps charged on turnover

	Equity []NavPoint
	DailyR []float64

	// Headline metrics (computed by applyMetrics()).
	FinalNav      float64
	TotalReturn   float64 // simple total return (FinalNav/StartNav - 1)
	AnnualReturn  float64 // annualised, geometric
	AnnualVol     float64 // sqrt(252) * stdev(daily_r)
	Sharpe        float64 // AnnualReturn / AnnualVol (r_f = 0)
	MaxDrawdown   float64 // negative value (e.g. -0.18 for -18%)
	HitRate       float64 // fraction of daily returns > 0
	WorstDay      float64
	BestDay       float64
	TradingDays   int
}

// applyMetrics is called by the simulator once the daily-return
// series is final. Pure math; no I/O.
func (r *Result) applyMetrics() {
	if len(r.Equity) == 0 {
		return
	}
	r.FinalNav = r.Equity[len(r.Equity)-1].Nav
	r.TotalReturn = r.FinalNav/r.StartNav - 1.0
	r.TradingDays = len(r.DailyR)

	years := float64(r.TradingDays) / 252.0
	if years <= 0 {
		years = 1.0 / 252.0
	}
	if r.FinalNav > 0 && r.StartNav > 0 {
		r.AnnualReturn = math.Pow(r.FinalNav/r.StartNav, 1.0/years) - 1.0
	}
	r.AnnualVol = annualisedStdev(r.DailyR)
	if r.AnnualVol > 0 {
		r.Sharpe = r.AnnualReturn / r.AnnualVol
	}
	r.MaxDrawdown = maxDrawdown(r.Equity)
	hits, wins, worst, best := 0, 0, 0.0, 0.0
	for i, v := range r.DailyR {
		if i == 0 {
			worst, best = v, v
		}
		if v > 0 {
			wins++
		}
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			hits++
		}
		if v < worst {
			worst = v
		}
		if v > best {
			best = v
		}
	}
	if hits > 0 {
		r.HitRate = float64(wins) / float64(hits)
	}
	r.WorstDay = worst
	r.BestDay = best
}

func annualisedStdev(r []float64) float64 {
	if len(r) < 2 {
		return 0
	}
	var sum float64
	for _, v := range r {
		sum += v
	}
	mean := sum / float64(len(r))
	var ss float64
	for _, v := range r {
		d := v - mean
		ss += d * d
	}
	dailyStdev := math.Sqrt(ss / float64(len(r)-1))
	return dailyStdev * math.Sqrt(252.0)
}

// maxDrawdown is the most negative peak-to-trough excursion in
// the equity curve. Reported as a negative number (e.g. -0.20
// for a 20% drawdown).
func maxDrawdown(equity []NavPoint) float64 {
	if len(equity) < 2 {
		return 0
	}
	peak := equity[0].Nav
	worst := 0.0
	for _, p := range equity {
		if p.Nav > peak {
			peak = p.Nav
		}
		if peak <= 0 {
			continue
		}
		dd := p.Nav/peak - 1.0
		if dd < worst {
			worst = dd
		}
	}
	return worst
}
