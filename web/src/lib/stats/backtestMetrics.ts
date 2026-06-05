// backtestMetrics.ts — extra performance / risk metrics
// computed client-side from a backtest's NAV curve.
//
// WHY THIS EXISTS
// ---------------
// The server returns a small set of headline metrics with each
// backtest result (cumulativeReturn, annualizedReturn,
// volatility, sharpe, maxDD, winRate, tradeCount). These are
// the "PM dashboard" view but they don't satisfy a quant
// reviewer asking
//   "is the alpha downside-skewed?"
//   "what's the Sortino, Calmar, recovery time?"
//   "what's the 95% / 99% VaR per day?"
//   "what's the worst day / best day / streak?"
//
// All of these are derived from the daily NAV series the
// backtest already carries (BacktestNavPoint[]). The server
// could ship them too, but a Go change cycles through schema
// migration + protobuf + frontend type updates; here in TS we
// can compute the lot in <100 LOC and surface them as an
// expandable "deep metrics" section. If a metric proves
// universally valuable we'll port it to Go next.
//
// CONVENTIONS
// -----------
//   - Daily returns are simple period returns:
//       r_t = nav_t / nav_{t-1} - 1
//   - "Annualization" assumes 252 trading days. Pass tradingDaysPerYear
//     to override for calendars that aren't equity-style.
//   - Sortino uses downside deviation against a target return of 0.
//     We don't subtract the risk-free rate — matches the
//     server's existing Sharpe convention so the two are
//     directly comparable.
//   - VaR / ES use historical (empirical) quantiles, not
//     parametric. With multi-year backtests this is the right
//     choice; with very short series (< ~30 obs) the estimator
//     is noisy and we surface a `lowSampleWarning` flag.
//   - Drawdown recovery time: count of obs from the trough of
//     max DD until NAV first returns to the prior peak.
//     Returns null if NAV never recovers within the series.
//
// All functions return null on insufficient data (< 2 obs)
// rather than NaN. Callers MUST handle null and render
// "insufficient data" in that case.

export interface DeepBacktestMetricsInput {
  /** Series of daily NAV points; only `nav` is used. Must be
      sorted ascending by date. */
  navCurve: ReadonlyArray<{ date: string; nav: number }>;
  /** Defaults to 252 (US equities). */
  tradingDaysPerYear?: number;
}

export interface DeepBacktestMetrics {
  /** count of returns observations (navCurve.length - 1). */
  obsCount: number;
  /** Sortino ratio = mean(r) / downsideStdev * sqrt(annualizationFactor). */
  sortino: number | null;
  /** Calmar ratio = annualized return / |max drawdown|. */
  calmar: number | null;
  /** Best single-day return. */
  bestDayReturn: number | null;
  /** Worst single-day return. */
  worstDayReturn: number | null;
  /** Number of up days. */
  upDays: number;
  /** Number of down days. */
  downDays: number;
  /** Mean of positive daily returns. */
  meanUpDay: number | null;
  /** Mean of negative daily returns (will be negative). */
  meanDownDay: number | null;
  /** Skewness of daily returns. Negative = left tail. */
  skewness: number | null;
  /** Excess kurtosis (Fisher convention; normal = 0). */
  excessKurtosis: number | null;
  /** Historical 95% VaR — return at the 5th percentile (will be negative). */
  var95: number | null;
  /** Historical 99% VaR — return at the 1st percentile. */
  var99: number | null;
  /** Expected shortfall at 95% — mean of returns below var95. */
  cvar95: number | null;
  /** Expected shortfall at 99%. */
  cvar99: number | null;
  /** Number of obs from peak to trough at max drawdown. */
  ddPeakToTroughDays: number | null;
  /** Number of obs from trough back to peak; null if NAV
      never recovered within the series. */
  ddRecoveryDays: number | null;
  /** True when obsCount < 30 — caller should flag results. */
  lowSampleWarning: boolean;
}

export function computeDeepBacktestMetrics(input: DeepBacktestMetricsInput): DeepBacktestMetrics | null {
  const { navCurve } = input;
  if (!navCurve || navCurve.length < 2) return null;
  const tdy = input.tradingDaysPerYear ?? 252;

  // Build daily returns. Drop any non-finite ratios (e.g. when
  // the previous NAV was zero, which shouldn't happen but
  // shouldn't crash either).
  const returns: number[] = [];
  for (let i = 1; i < navCurve.length; i++) {
    const prev = navCurve[i - 1].nav;
    const cur = navCurve[i].nav;
    if (!Number.isFinite(prev) || !Number.isFinite(cur) || prev <= 0) continue;
    returns.push(cur / prev - 1);
  }
  if (returns.length === 0) return null;

  const n = returns.length;
  const meanR = returns.reduce((a, b) => a + b, 0) / n;
  const variance = returns.reduce((a, b) => a + (b - meanR) * (b - meanR), 0) / Math.max(1, n - 1);
  const stdev = Math.sqrt(variance);

  // Downside deviation against target=0 — excludes positive
  // returns from the variance computation.
  const downside = returns.filter((r) => r < 0);
  const downsideVar =
    downside.length > 0
      ? downside.reduce((a, r) => a + r * r, 0) / Math.max(1, downside.length - 1)
      : 0;
  const downsideStdev = Math.sqrt(downsideVar);

  // Annualised return: geometric mean of (1+r) ^ tdy - 1.
  // For a stable result over multi-year series, take the log
  // mean instead — equivalent but more numerically stable.
  const meanLogR =
    returns.reduce((a, r) => a + Math.log(1 + Math.max(r, -0.999999)), 0) / n;
  const annualizedReturn = Math.exp(meanLogR * tdy) - 1;

  const sortino =
    downsideStdev > 0 ? (meanR / downsideStdev) * Math.sqrt(tdy) : null;

  // Recompute max drawdown locally so we don't depend on the
  // server figure being available; produces the same number
  // either way.
  let peak = navCurve[0].nav;
  let peakIdx = 0;
  let maxDD = 0;
  let maxDDPeakIdx = 0;
  let maxDDTroughIdx = 0;
  for (let i = 0; i < navCurve.length; i++) {
    const nav = navCurve[i].nav;
    if (!Number.isFinite(nav)) continue;
    if (nav > peak) {
      peak = nav;
      peakIdx = i;
    }
    if (peak > 0) {
      const dd = nav / peak - 1;
      if (dd < maxDD) {
        maxDD = dd;
        maxDDPeakIdx = peakIdx;
        maxDDTroughIdx = i;
      }
    }
  }
  const calmar = maxDD < 0 ? annualizedReturn / Math.abs(maxDD) : null;

  // Recovery: from trough, how many obs until we reach the
  // pre-DD peak again? Null if not yet recovered in series.
  let ddRecoveryDays: number | null = null;
  if (maxDD < 0) {
    const peakValue = navCurve[maxDDPeakIdx].nav;
    for (let i = maxDDTroughIdx + 1; i < navCurve.length; i++) {
      if (navCurve[i].nav >= peakValue) {
        ddRecoveryDays = i - maxDDTroughIdx;
        break;
      }
    }
  }

  // Best / worst single-day, up/down counts, conditional means.
  let bestDayReturn = -Infinity;
  let worstDayReturn = Infinity;
  let upDays = 0;
  let downDays = 0;
  let upSum = 0;
  let downSum = 0;
  for (const r of returns) {
    if (r > bestDayReturn) bestDayReturn = r;
    if (r < worstDayReturn) worstDayReturn = r;
    if (r > 0) {
      upDays++;
      upSum += r;
    } else if (r < 0) {
      downDays++;
      downSum += r;
    }
  }

  // Skewness & excess kurtosis. Use the sample formulas; on
  // small samples these are biased but match what most quant
  // textbooks report.
  let m3 = 0;
  let m4 = 0;
  for (const r of returns) {
    const d = r - meanR;
    m3 += d * d * d;
    m4 += d * d * d * d;
  }
  m3 /= n;
  m4 /= n;
  const skewness = stdev > 0 ? m3 / (stdev * stdev * stdev) : null;
  const excessKurtosis = stdev > 0 ? m4 / (stdev * stdev * stdev * stdev) - 3 : null;

  // Historical VaR + Expected Shortfall at 95% / 99%.
  const sortedReturns = [...returns].sort((a, b) => a - b);
  const quantile = (p: number) => {
    const idx = Math.max(0, Math.min(sortedReturns.length - 1, Math.floor(p * sortedReturns.length)));
    return sortedReturns[idx];
  };
  const var95 = quantile(0.05);
  const var99 = quantile(0.01);
  const cvar95 =
    sortedReturns.filter((r) => r <= var95).length > 0
      ? sortedReturns.filter((r) => r <= var95).reduce((a, b) => a + b, 0) /
        Math.max(1, sortedReturns.filter((r) => r <= var95).length)
      : null;
  const cvar99 =
    sortedReturns.filter((r) => r <= var99).length > 0
      ? sortedReturns.filter((r) => r <= var99).reduce((a, b) => a + b, 0) /
        Math.max(1, sortedReturns.filter((r) => r <= var99).length)
      : null;

  return {
    obsCount: n,
    sortino,
    calmar,
    bestDayReturn: bestDayReturn === -Infinity ? null : bestDayReturn,
    worstDayReturn: worstDayReturn === Infinity ? null : worstDayReturn,
    upDays,
    downDays,
    meanUpDay: upDays > 0 ? upSum / upDays : null,
    meanDownDay: downDays > 0 ? downSum / downDays : null,
    skewness,
    excessKurtosis,
    var95,
    var99,
    cvar95,
    cvar99,
    ddPeakToTroughDays: maxDD < 0 ? maxDDTroughIdx - maxDDPeakIdx : null,
    ddRecoveryDays,
    lowSampleWarning: n < 30,
  };
}
