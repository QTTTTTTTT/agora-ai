// backtestMetrics.test.ts — sanity tests for the deep-metrics
// computer. We test a few synthetic NAV paths whose properties
// are obvious by inspection, plus a couple of degenerate-input
// guards.

import { describe, it, expect } from "vitest";
import { computeDeepBacktestMetrics } from "./backtestMetrics";

const navCurveOf = (navs: number[]) =>
  navs.map((nav, i) => ({ date: `2026-01-${String(i + 1).padStart(2, "0")}`, nav }));

describe("computeDeepBacktestMetrics", () => {
  it("returns null on insufficient data", () => {
    expect(computeDeepBacktestMetrics({ navCurve: [] })).toBeNull();
    expect(
      computeDeepBacktestMetrics({ navCurve: [{ date: "2026-01-01", nav: 100 }] }),
    ).toBeNull();
  });

  it("flags low sample warning for short series", () => {
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf([100, 101, 102, 99, 100]) });
    expect(r).not.toBeNull();
    expect(r!.obsCount).toBe(4);
    expect(r!.lowSampleWarning).toBe(true);
  });

  it("computes correct max-DD path / recovery on a clean V-shape", () => {
    // 100 -> 90 (down 10%) -> 100 (recovered) -> 110.
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf([100, 90, 100, 110]) });
    expect(r).not.toBeNull();
    if (r) {
      expect(r.ddPeakToTroughDays).toBe(1);
      expect(r.ddRecoveryDays).toBe(1);
    }
  });

  it("returns null recoveryDays when not yet recovered", () => {
    // Strict downtrend — no recovery.
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf([100, 95, 90, 85]) });
    expect(r).not.toBeNull();
    if (r) {
      expect(r.ddRecoveryDays).toBeNull();
      expect(r.ddPeakToTroughDays).toBe(3);
    }
  });

  it("skews negative on left-tail series", () => {
    // Most days small +0.5%, occasional big -3%.
    const navs = [100];
    for (let i = 0; i < 50; i++) {
      const r = (i + 1) % 11 === 0 ? -0.03 : 0.005;
      navs.push(navs[navs.length - 1] * (1 + r));
    }
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf(navs) });
    expect(r).not.toBeNull();
    if (r) {
      expect(r.skewness).not.toBeNull();
      expect(r.skewness!).toBeLessThan(0);
    }
  });

  it("computes Sortino > 0 for positive-trending series with limited downside", () => {
    const navs = [100];
    for (let i = 0; i < 100; i++) {
      const r = i % 10 === 5 ? -0.005 : 0.003;
      navs.push(navs[navs.length - 1] * (1 + r));
    }
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf(navs) });
    expect(r).not.toBeNull();
    if (r) {
      expect(r.sortino).not.toBeNull();
      expect(r.sortino!).toBeGreaterThan(0);
    }
  });

  it("VaR99 <= VaR95 (more extreme losses)", () => {
    const navs = [100];
    for (let i = 0; i < 200; i++) {
      const r = (Math.sin(i * 1.7) + Math.cos(i * 0.31)) * 0.01;
      navs.push(navs[navs.length - 1] * (1 + r));
    }
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf(navs) });
    expect(r).not.toBeNull();
    if (r && r.var95 !== null && r.var99 !== null) {
      expect(r.var99).toBeLessThanOrEqual(r.var95);
    }
  });

  it("counts up days vs down days correctly", () => {
    const r = computeDeepBacktestMetrics({ navCurve: navCurveOf([100, 110, 100, 110, 100]) });
    expect(r).not.toBeNull();
    if (r) {
      expect(r.upDays).toBe(2);
      expect(r.downDays).toBe(2);
    }
  });
});
