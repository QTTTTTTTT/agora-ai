package backtest

import "math"

// computeMetrics walks the NavCurve + Trades produced by a run and
// returns the summary block. Always returns a populated Metrics
// (zero values when a metric isn't computable, e.g. only one NAV
// point ⇒ no return series ⇒ Sharpe = 0).
//
// Convention: returns are fractions (0.18 = +18%). Annualization
// uses 252 trading days regardless of market — close enough for
// US/HK/A-share equities and crypto, which never sleep but for
// backtest purposes we sample daily.
func computeMetrics(curve []NavPoint, trades []TradeEvent) Metrics {
	out := Metrics{
		TradeCount: countFilledTrades(trades),
	}
	if len(curve) < 2 {
		return out
	}
	first := curve[0].Nav
	last := curve[len(curve)-1].Nav
	if first <= 0 {
		return out
	}

	out.CumulativeReturn = last/first - 1

	// Annualised return: assume 252-day trading year; even for
	// crypto where 365 would be more accurate we keep 252 so
	// cross-market comparisons stay consistent.
	years := float64(len(curve)-1) / 252.0
	if years > 0 && last > 0 {
		out.AnnualizedReturn = math.Pow(last/first, 1.0/years) - 1
	}

	dailyReturns := buildDailyReturns(curve)
	out.Volatility = annualisedStdev(dailyReturns)
	if out.Volatility > 0 {
		mean := meanOf(dailyReturns)
		// Annualise the mean by × 252 to match the annualised
		// stdev. Risk-free rate baked in as 0 — operators can
		// post-process if they want a non-zero RF.
		out.SharpeRatio = (mean * 252.0) / out.Volatility
	}

	out.MaxDrawdown = computeMaxDrawdown(curve)
	out.WinRate = computeWinRate(dailyReturns)
	out.WinningTradeCount, out.LosingTradeCount = countTradeWinsLosses(trades)
	return out
}

func countFilledTrades(trades []TradeEvent) int {
	n := 0
	for _, t := range trades {
		if t.Status == "filled" {
			n++
		}
	}
	return n
}

// buildDailyReturns produces fractional day-over-day returns from
// a NAV curve. Curves with non-positive NAV (shouldn't happen but
// we guard) skip the broken segments rather than divide-by-zero.
func buildDailyReturns(curve []NavPoint) []float64 {
	if len(curve) < 2 {
		return nil
	}
	out := make([]float64, 0, len(curve)-1)
	for i := 1; i < len(curve); i++ {
		prev := curve[i-1].Nav
		cur := curve[i].Nav
		if prev <= 0 {
			continue
		}
		out = append(out, cur/prev-1)
	}
	return out
}

// annualisedStdev returns the stdev × √252. Empty / single-point
// series return 0.
func annualisedStdev(series []float64) float64 {
	if len(series) < 2 {
		return 0
	}
	mean := meanOf(series)
	sumSq := 0.0
	for _, v := range series {
		d := v - mean
		sumSq += d * d
	}
	// Sample stdev (denominator n-1).
	variance := sumSq / float64(len(series)-1)
	return math.Sqrt(variance) * math.Sqrt(252)
}

func meanOf(series []float64) float64 {
	if len(series) == 0 {
		return 0
	}
	sum := 0.0
	for _, v := range series {
		sum += v
	}
	return sum / float64(len(series))
}

// computeMaxDrawdown finds the worst peak-to-trough drawdown in
// the curve as a negative number (-0.34 = -34% drawdown). The
// snapshot already stores per-day drawdowns so we just take min.
func computeMaxDrawdown(curve []NavPoint) float64 {
	worst := 0.0
	for _, p := range curve {
		if p.DrawdownPct < worst {
			worst = p.DrawdownPct
		}
	}
	return worst
}

// computeWinRate returns the fraction of daily returns > 0.
func computeWinRate(series []float64) float64 {
	if len(series) == 0 {
		return 0
	}
	wins := 0
	for _, v := range series {
		if v > 0 {
			wins++
		}
	}
	return float64(wins) / float64(len(series))
}

// applyBenchmarkMetrics fills the benchmark-relative block of a
// Metrics struct in-place. The runner only calls this when
// benchmarkCurve and navCurve are the same length AND there are
// at least two points to differentiate — degenerate inputs leave
// the fields at zero.
//
// The OLS regression for alpha/beta uses daily fractional
// returns; alpha is then × 252 to annualise. Tracking error is
// the annualised stdev of (strategy_daily - benchmark_daily) and
// the information ratio is excess-daily-mean × 252 / tracking_error.
// We intentionally don't subtract a risk-free rate from alpha —
// the rest of the metrics package assumes rf=0 and keeping the
// convention consistent makes back-of-the-envelope checks easier.
func applyBenchmarkMetrics(m *Metrics, navCurve []NavPoint, benchmarkCurve []BenchmarkPoint) {
	if m == nil || len(navCurve) != len(benchmarkCurve) || len(navCurve) < 2 {
		return
	}
	first := benchmarkCurve[0].Pct
	last := benchmarkCurve[len(benchmarkCurve)-1].Pct
	// Pct on the curve is already (close/anchor - 1) which is
	// the cumulative return relative to day 0. So
	// BenchmarkCumulativeReturn == last - first; in practice
	// first should always be 0 but we subtract for safety in
	// case anchor logic ever shifts.
	m.BenchmarkCumulativeReturn = last - first
	m.ExcessReturn = m.CumulativeReturn - m.BenchmarkCumulativeReturn

	stratDaily := buildDailyReturns(navCurve)
	benchDaily := buildBenchmarkDailyReturns(benchmarkCurve)
	if len(stratDaily) != len(benchDaily) || len(stratDaily) < 2 {
		return
	}

	// OLS for beta + alpha. cov / var on daily fractions, alpha
	// annualised by × 252. Guard against zero-variance benchmark
	// (e.g. flatline ETF in a short window).
	stratMean := meanOf(stratDaily)
	benchMean := meanOf(benchDaily)
	var cov, varB float64
	for i := range stratDaily {
		ds := stratDaily[i] - stratMean
		db := benchDaily[i] - benchMean
		cov += ds * db
		varB += db * db
	}
	denom := float64(len(stratDaily) - 1)
	if denom > 0 {
		cov /= denom
		varB /= denom
	}
	if varB > 0 {
		m.Beta = cov / varB
		m.Alpha = (stratMean - m.Beta*benchMean) * 252.0
	}

	excessDaily := make([]float64, len(stratDaily))
	for i := range stratDaily {
		excessDaily[i] = stratDaily[i] - benchDaily[i]
	}
	te := annualisedStdev(excessDaily)
	m.TrackingError = te
	if te > 0 {
		m.InformationRatio = (meanOf(excessDaily) * 252.0) / te
	}

	// Excess drawdown: build a synthetic equity curve where the
	// strategy returns are deflated by the benchmark return. We
	// use the pct-difference curve directly (strategy_pct -
	// benchmark_pct) and walk peak-to-trough on (1 + excess).
	excessEq := make([]float64, len(navCurve))
	for i := range navCurve {
		stratPct := 0.0
		if navCurve[0].Nav > 0 {
			stratPct = navCurve[i].Nav/navCurve[0].Nav - 1.0
		}
		excessEq[i] = 1.0 + (stratPct - benchmarkCurve[i].Pct)
	}
	peak := excessEq[0]
	worst := 0.0
	for _, v := range excessEq {
		if v > peak {
			peak = v
		}
		if peak > 0 {
			dd := v/peak - 1.0
			if dd < worst {
				worst = dd
			}
		}
	}
	m.ExcessMaxDrawdown = worst
}

// buildBenchmarkDailyReturns is the BenchmarkPoint equivalent of
// buildDailyReturns. Uses Pct field because day-over-day
// fractional returns are derivable from (1+pct_t)/(1+pct_{t-1})-1.
func buildBenchmarkDailyReturns(curve []BenchmarkPoint) []float64 {
	if len(curve) < 2 {
		return nil
	}
	out := make([]float64, 0, len(curve)-1)
	for i := 1; i < len(curve); i++ {
		prev := 1.0 + curve[i-1].Pct
		cur := 1.0 + curve[i].Pct
		if prev <= 0 {
			continue
		}
		out = append(out, cur/prev-1.0)
	}
	return out
}

// countTradeWinsLosses walks the trade log and counts how many
// closing trades realized positive vs negative P&L. The runner
// records the realized P&L delta in TradeEvent.Reason when it's a
// sell/reduce action — we parse that out via the convention
// established in runner.recordTrade. For the MVP we use a
// simpler rule: any "filled" sell/reduce where Notional > 0 is
// inspected; we don't try to attribute round-trip P&L here, just
// flag profitable sells based on the FillPrice vs a tracked cost
// basis.
//
// To keep this attribution honest without re-walking lots, we
// instead rely on TradeEvent.Confidence carrying the realized P&L
// when the runner records a sell — see recordSellEvent in
// runner.go. Confidence ≥ 0 for a profitable close, < 0 for a loss.
func countTradeWinsLosses(trades []TradeEvent) (int, int) {
	wins, losses := 0, 0
	for _, t := range trades {
		if t.Status != "filled" {
			continue
		}
		switch t.Action {
		case "sell", "reduce":
			// recordSellEvent stuffs the realized P&L into
			// Confidence (positive = winning close).
			if t.Confidence > 0 {
				wins++
			} else if t.Confidence < 0 {
				losses++
			}
		}
	}
	return wins, losses
}
