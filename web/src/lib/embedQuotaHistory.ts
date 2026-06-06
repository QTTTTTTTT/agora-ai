// Pure-data helpers for the embedquota admin sparkline.
//
// Lives outside the React component so:
//
//   1. The hand-rolled Node test harness (`test:sparkline`) can
//      import it without needing a JSX-aware loader. The web
//      package deliberately stays away from a full Vitest /
//      Jest setup; everything testable lives in plain `.ts`
//      modules and is exercised via Node 22's
//      `--experimental-strip-types`.
//   2. The same logic could be reused by a future capacity-
//      review tab that wants 30-day history without re-deriving
//      the cap-line math.

import type { EmbedQuotaTokenDay } from "./api";

export interface HistoryBar {
  day: string;
  tokens: number;
  /** 0..1 — height fraction relative to displayMax. */
  ratio: number;
  /** True for the rightmost bar (today by RecentDays contract). */
  isToday: boolean;
}

export interface HistoryView {
  bars: HistoryBar[];
  /**
   * 0..1 — vertical position of the daily-cap reference line.
   * 0 means "no line should be drawn" (cap unknown or zero).
   * Capped at 1 so a cap below the observed peak still keeps
   * the line visible at the top edge.
   */
  capRatio: number;
  /**
   * Max value bars are scaled against. Equals max(peak, cap)
   * when cap is known, otherwise peak. Returned so the renderer
   * can show the actual upper-bound number in the legend.
   */
  displayMax: number;
}

/**
 * computeHistoryView normalises a token-history array into bar
 * data plus a daily-cap reference line position the renderer can
 * lay out without further math.
 *
 * Two modes:
 *
 *   1. cap > 0 (typical production): scale every bar against
 *      max(peak, cap) so the cap line stays visible even when
 *      a single day blew past the cap (e.g. operator raised the
 *      cap mid-day). The cap line tells the operator at a
 *      glance "how much of today's budget did we burn". This
 *      is the reason the panel exists — peak-normalised bars
 *      look identical at "5k tokens used out of 1M cap" and
 *      "990k tokens used out of 1M cap", which loses the alarm.
 *
 *   2. cap <= 0 (no cap configured): fall back to peak-normalised
 *      bars and skip the line entirely. capRatio = 0 signals
 *      the renderer to omit the dashed line.
 *
 * Negative or non-finite cap values are coerced to 0; a
 * misconfigured `tokensDailyMax` can't render an inverted line.
 */
export function computeHistoryView(
  history: EmbedQuotaTokenDay[],
  cap: number,
): HistoryView {
  if (history.length === 0) {
    return { bars: [], capRatio: 0, displayMax: 0 };
  }
  const safeCap = Number.isFinite(cap) && cap > 0 ? cap : 0;
  const peak = history.reduce((m, d) => Math.max(m, d.tokens), 0);
  const displayMax = safeCap > 0 ? Math.max(peak, safeCap) : peak;
  const lastIdx = history.length - 1;
  const bars = history.map((day, idx) => ({
    day: day.day,
    tokens: day.tokens,
    ratio: displayMax > 0 ? day.tokens / displayMax : 0,
    isToday: idx === lastIdx,
  }));
  const capRatio =
    safeCap > 0 && displayMax > 0 ? Math.min(1, safeCap / displayMax) : 0;
  return { bars, capRatio, displayMax };
}
