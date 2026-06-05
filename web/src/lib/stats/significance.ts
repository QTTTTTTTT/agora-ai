// significance.ts — light-weight statistical significance utilities.
//
// WHY THIS EXISTS
// ---------------
// The ABTestCompare page already shows a heuristic "low / medium /
// high confidence" badge derived server-side, but it's a soft
// judgment that doesn't quantify "is the observed return diff
// actually distinguishable from noise?" Operators promoting a
// strategy to production need the harder version: a p-value or
// confidence interval on the per-trade return diff.
//
// Two tests cover the AB compare cases we care about:
//
//   1. Welch's t-test on the per-trade realized PnL distribution
//      between variant A and variant B. Doesn't assume equal
//      variance, robust under the typical "one variant trades
//      tighter than the other" pattern.
//
//   2. Two-proportion z-test on win-rate (fraction of trades
//      with realizedPnL > 0) between A and B. Useful as a
//      complement when mean returns are noisy but win-rate
//      shifts are clean.
//
// We compute these client-side from variantATrades /
// variantBTrades arrays the API already returns. No backend
// change required.
//
// IMPLEMENTATION NOTES
// --------------------
//   - For p-values we use the standard normal CDF via the
//     Abramowitz–Stegun erf approximation (max abs error
//     ~1.5e-7), which is plenty for "is this < 0.05?". For
//     Welch with df < 30 we'd ideally use a t-distribution CDF,
//     but the practical sample sizes from AB tests (typically
//     dozens to hundreds of trades) put df well above 30 where
//     normal approximation is tight. We flag dfWarning when
//     df < 30 so the UI can show "small sample — interpret with
//     caution".
//   - Sample variance uses Bessel's correction (n-1) so it's an
//     unbiased estimator.
//   - All functions return null on degenerate input (n < 2,
//     zero variance, etc) — the UI is responsible for showing
//     an "insufficient data" message rather than NaNs.

export interface WelchTTestResult {
  meanA: number;
  meanB: number;
  diff: number; // meanB - meanA
  varA: number;
  varB: number;
  nA: number;
  nB: number;
  tStatistic: number;
  /** Welch–Satterthwaite degrees of freedom. */
  df: number;
  /** Two-sided p-value (P(|T| >= |t|)). */
  pValue: number;
  /** 95% CI for the diff (meanB - meanA). */
  ci95: [number, number];
  /** True when df < 30 — caller should flag small sample. */
  dfWarning: boolean;
}

export interface ProportionZTestResult {
  pA: number; // successA / nA
  pB: number; // successB / nB
  diff: number; // pB - pA
  nA: number;
  nB: number;
  zStatistic: number;
  /** Two-sided p-value (P(|Z| >= |z|)). */
  pValue: number;
  /** 95% CI for the diff (pB - pA), Wald-type. */
  ci95: [number, number];
}

/**
 * Welch's t-test for unpaired samples with potentially unequal
 * variances. Returns null when either sample has fewer than 2
 * observations or both variances are zero (no signal at all).
 */
export function welchTTest(samplesA: readonly number[], samplesB: readonly number[]): WelchTTestResult | null {
  const nA = samplesA.length;
  const nB = samplesB.length;
  if (nA < 2 || nB < 2) return null;

  const meanA = mean(samplesA);
  const meanB = mean(samplesB);
  const varA = variance(samplesA, meanA); // unbiased, n-1
  const varB = variance(samplesB, meanB);
  if (varA === 0 && varB === 0) return null;

  const diff = meanB - meanA;
  const seSqr = varA / nA + varB / nB;
  const se = Math.sqrt(seSqr);
  if (se === 0 || !Number.isFinite(se)) return null;

  const tStatistic = diff / se;

  // Welch–Satterthwaite df.
  const dfNum = seSqr * seSqr;
  const dfDen = (varA * varA) / (nA * nA * (nA - 1)) + (varB * varB) / (nB * nB * (nB - 1));
  const df = dfDen > 0 ? dfNum / dfDen : Math.max(nA, nB) - 1;

  // Two-sided p-value via normal approximation; tight for df>=30.
  const pValue = twoSidedNormalP(tStatistic);

  // 95% CI for the diff (Z=1.96 — close to t critical for df>=30).
  const z975 = 1.959964;
  const half = z975 * se;
  const ci95: [number, number] = [diff - half, diff + half];

  return {
    meanA,
    meanB,
    diff,
    varA,
    varB,
    nA,
    nB,
    tStatistic,
    df,
    pValue,
    ci95,
    dfWarning: df < 30,
  };
}

/**
 * Two-proportion z-test on success rates between two groups.
 * Returns null when either group has zero observations.
 */
export function proportionZTest(
  successesA: number,
  nA: number,
  successesB: number,
  nB: number,
): ProportionZTestResult | null {
  if (nA <= 0 || nB <= 0) return null;
  if (successesA < 0 || successesB < 0) return null;
  if (successesA > nA || successesB > nB) return null;

  const pA = successesA / nA;
  const pB = successesB / nB;
  const diff = pB - pA;

  // Pooled-variance form for the test statistic — appropriate
  // when the null is "rates are equal".
  const pooled = (successesA + successesB) / (nA + nB);
  const seTest = Math.sqrt(pooled * (1 - pooled) * (1 / nA + 1 / nB));
  const zStatistic = seTest > 0 ? diff / seTest : 0;
  const pValue = twoSidedNormalP(zStatistic);

  // Wald-type 95% CI on the diff — uses unpooled variance,
  // standard for reporting effect size.
  const seCI = Math.sqrt((pA * (1 - pA)) / nA + (pB * (1 - pB)) / nB);
  const z975 = 1.959964;
  const half = z975 * seCI;
  const ci95: [number, number] = [diff - half, diff + half];

  return { pA, pB, diff, nA, nB, zStatistic, pValue, ci95 };
}

// --- formatting helpers --------------------------------------

/**
 * Compact p-value formatter — "p<0.001" / "p=0.012" / "p=0.34".
 * Use this for inline display rather than reporting raw decimals.
 */
export function formatPValue(p: number): string {
  if (!Number.isFinite(p)) return "p=N/A";
  if (p < 0.001) return "p<0.001";
  if (p < 0.01) return `p=${p.toFixed(3)}`;
  return `p=${p.toFixed(2)}`;
}

/**
 * Significance interpretation at the conventional alpha cutoffs.
 * Returns "p<0.001" / "p<0.01" / "p<0.05" / null (not
 * significant at 5%). UI can use this to render a "★/★★/★★★"
 * badge or color it.
 */
export function significanceLevel(p: number): "p<0.001" | "p<0.01" | "p<0.05" | null {
  if (!Number.isFinite(p)) return null;
  if (p < 0.001) return "p<0.001";
  if (p < 0.01) return "p<0.01";
  if (p < 0.05) return "p<0.05";
  return null;
}

// --- internals -----------------------------------------------

function mean(xs: readonly number[]): number {
  let sum = 0;
  for (const x of xs) sum += x;
  return sum / xs.length;
}

function variance(xs: readonly number[], m: number): number {
  if (xs.length < 2) return 0;
  let acc = 0;
  for (const x of xs) {
    const d = x - m;
    acc += d * d;
  }
  return acc / (xs.length - 1);
}

/**
 * Two-sided p-value under the standard normal:
 *     P(|Z| >= |z|) = 2 * (1 - Phi(|z|))
 * Phi is computed via erf. Abramowitz–Stegun 7.1.26 gives an
 * approximation with max abs error 1.5e-7 — more than enough
 * for the "is this < 0.05?" question.
 */
function twoSidedNormalP(z: number): number {
  if (!Number.isFinite(z)) return Number.NaN;
  return 2 * (1 - normalCdf(Math.abs(z)));
}

function normalCdf(z: number): number {
  return 0.5 * (1 + erf(z / Math.SQRT2));
}

function erf(x: number): number {
  // Abramowitz & Stegun 7.1.26 — max abs error ~1.5e-7.
  const sign = Math.sign(x);
  const ax = Math.abs(x);
  const a1 = 0.254829592;
  const a2 = -0.284496736;
  const a3 = 1.421413741;
  const a4 = -1.453152027;
  const a5 = 1.061405429;
  const p = 0.3275911;
  const t = 1 / (1 + p * ax);
  const y = 1 - (((((a5 * t + a4) * t) + a3) * t + a2) * t + a1) * t * Math.exp(-ax * ax);
  return sign * y;
}
