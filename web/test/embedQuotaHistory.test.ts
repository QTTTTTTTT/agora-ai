/**
 * web/test/embedQuotaHistory.test.ts — W13-2 unit test for the
 * sparkline math.
 *
 * Run via: npm run test:sparkline
 *
 * Uses the same hand-rolled assert harness as
 * lessonRenderer.test.ts and i18nNamespaceParity.test.ts so the
 * web package keeps a single Node-strip-types entry point and
 * doesn't pull in Vitest just for this.
 *
 * What this pins (the regressions I'd expect over time):
 *
 *   1. Empty history → empty bars + zero capRatio (renderer
 *      must skip the section, not divide by zero).
 *   2. cap=0 → peak-normalised mode and NO cap line (typical
 *      "limiter without quota" config).
 *   3. cap > peak → cap line below 100%, today's bar scaled
 *      against the cap (the central UX point of W13-2).
 *   4. peak > cap → cap line still inside the chart (clamped
 *      to ≤100%), bars scaled against peak so the over-cap
 *      day stays visible.
 *   5. Negative cap (config typo) → coerced to 0, no inverted
 *      line.
 *   6. Non-finite cap (NaN / Infinity) → same coercion.
 *   7. `isToday` always lands on the last entry (the W12-3
 *      RecentDays contract).
 */

import { computeHistoryView } from "../src/lib/embedQuotaHistory.ts";
import type { EmbedQuotaTokenDay } from "../src/lib/api.ts";

let failures = 0;
let total = 0;

function check(name: string, fn: () => void): void {
  total += 1;
  try {
    fn();
    console.log(`  PASS  ${name}`);
  } catch (e: unknown) {
    failures += 1;
    const msg = e instanceof Error ? e.message : String(e);
    console.error(`  FAIL  ${name}\n        ${msg}`);
  }
}

function eq<T>(actual: T, expected: T, label: string): void {
  if (actual !== expected) {
    throw new Error(`${label}: got ${JSON.stringify(actual)}, want ${JSON.stringify(expected)}`);
  }
}

function approxEq(actual: number, expected: number, label: string, tol = 1e-9): void {
  if (Math.abs(actual - expected) > tol) {
    throw new Error(`${label}: got ${actual}, want ≈${expected}`);
  }
}

const week = (...vals: number[]): EmbedQuotaTokenDay[] =>
  vals.map((tokens, idx) => ({
    day: `2026-06-${String(idx + 1).padStart(2, "0")}`,
    tokens,
  }));

console.log("[1] empty + degenerate inputs");

check("empty history → empty bars and zero capRatio", () => {
  const v = computeHistoryView([], 1_000_000);
  eq(v.bars.length, 0, "bars.length");
  eq(v.capRatio, 0, "capRatio");
  eq(v.displayMax, 0, "displayMax");
});

check("cap=0 falls back to peak normalisation, no cap line", () => {
  const v = computeHistoryView(week(0, 100, 200), 0);
  eq(v.bars.length, 3, "bars.length");
  eq(v.capRatio, 0, "capRatio");
  eq(v.displayMax, 200, "displayMax = peak");
  approxEq(v.bars[2].ratio, 1, "tallest bar");
});

check("negative cap is coerced (no inverted line)", () => {
  const v = computeHistoryView(week(50, 50), -10_000);
  eq(v.capRatio, 0, "capRatio");
  eq(v.displayMax, 50, "displayMax = peak");
});

check("NaN cap is coerced (no inverted line)", () => {
  const v = computeHistoryView(week(50, 50), Number.NaN);
  eq(v.capRatio, 0, "capRatio");
});

check("Infinity cap is coerced (no inverted line)", () => {
  // Finite check rules this out — cap stays "unknown".
  const v = computeHistoryView(week(50, 50), Number.POSITIVE_INFINITY);
  eq(v.capRatio, 0, "capRatio");
  eq(v.displayMax, 50, "displayMax falls back to peak");
});

console.log("[2] healthy production: cap > peak");

check("today bar lands at tokens/cap fraction, cap at 100%", () => {
  // 1M cap, used 100k today, 50k yesterday → today bar = 10%,
  // cap line at top edge.
  const v = computeHistoryView(week(50_000, 100_000), 1_000_000);
  eq(v.displayMax, 1_000_000, "displayMax");
  approxEq(v.capRatio, 1, "capRatio = 1 (cap == displayMax)");
  approxEq(v.bars[1].ratio, 0.1, "today bar ratio");
  approxEq(v.bars[0].ratio, 0.05, "yesterday bar ratio");
});

console.log("[3] post-incident: peak > cap (over-cap day)");

check("cap line stays inside chart, peak day still drawn", () => {
  // Operator raised cap mid-week and somebody blew past it on
  // day 4. We must keep both signals visible.
  const v = computeHistoryView(week(0, 0, 0, 1_500_000, 200_000), 1_000_000);
  eq(v.displayMax, 1_500_000, "displayMax = peak");
  approxEq(v.capRatio, 1_000_000 / 1_500_000, "capRatio");
  approxEq(v.bars[3].ratio, 1, "peak day at 100%");
});

console.log("[4] today marker contract");

check("isToday is set on the last entry only", () => {
  const v = computeHistoryView(week(1, 2, 3, 4, 5, 6, 7), 100);
  for (let i = 0; i < 6; i++) {
    eq(v.bars[i].isToday, false, `bars[${i}].isToday`);
  }
  eq(v.bars[6].isToday, true, "bars[last].isToday");
});

console.log("[5] all-zero week");

check("all zeros + cap=0 → all bars zero ratio, no NaN", () => {
  const v = computeHistoryView(week(0, 0, 0, 0, 0, 0, 0), 0);
  eq(v.displayMax, 0, "displayMax");
  for (const b of v.bars) {
    eq(b.ratio, 0, `bar ${b.day}.ratio`);
    eq(Number.isFinite(b.ratio), true, `bar ${b.day}.ratio finite`);
  }
});

check("all zeros + cap > 0 → bars at 0, cap at 100%", () => {
  const v = computeHistoryView(week(0, 0, 0, 0, 0, 0, 0), 50_000);
  eq(v.displayMax, 50_000, "displayMax");
  approxEq(v.capRatio, 1, "capRatio");
});

if (failures > 0) {
  console.error(`\n${failures} of ${total} cases FAILED`);
  process.exit(1);
}
console.log(`\n${total} cases passed`);
