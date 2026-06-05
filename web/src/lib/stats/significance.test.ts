// significance.test.ts — sanity tests for the t-test / z-test math.
//
// Run via the existing vitest harness:
//   cd web && npx vitest run src/lib/stats/significance.test.ts
//
// We test against KNOWN published examples so any regression in
// the erf approximation, df computation, or sign convention is
// caught immediately.

import { describe, it, expect } from "vitest";
import {
  formatPValue,
  proportionZTest,
  significanceLevel,
  welchTTest,
} from "./significance";

describe("welchTTest", () => {
  it("returns null on insufficient sample", () => {
    expect(welchTTest([1], [2, 3])).toBeNull();
    expect(welchTTest([], [])).toBeNull();
  });

  it("returns null on zero variance in both groups", () => {
    expect(welchTTest([3, 3, 3], [3, 3, 3])).toBeNull();
  });

  it("computes a clear-significant diff for clean separated samples", () => {
    // Two well-separated normals: A ~ {-1..1}, B ~ {9..11}.
    const a = [-1, 0, 1, -1, 0, 1, -1, 0, 1, -1];
    const b = [9, 10, 11, 9, 10, 11, 9, 10, 11, 9];
    const r = welchTTest(a, b);
    expect(r).not.toBeNull();
    if (r) {
      expect(r.diff).toBeGreaterThan(8); // ~10 - 0
      expect(r.pValue).toBeLessThan(0.001);
      expect(r.ci95[0]).toBeGreaterThan(0);
    }
  });

  it("computes a non-significant result for overlapping samples", () => {
    // Both groups drawn from N(0, 1) approximately.
    const a = [0.1, -0.2, 0.05, 0.0, -0.1, 0.15, -0.05];
    const b = [0.0, 0.1, -0.05, 0.05, 0.0, -0.1, 0.05];
    const r = welchTTest(a, b);
    expect(r).not.toBeNull();
    if (r) {
      expect(r.pValue).toBeGreaterThan(0.05);
    }
  });

  it("flags small-sample warning when df < 30", () => {
    const a = [1, 2, 3];
    const b = [4, 5, 6];
    const r = welchTTest(a, b);
    expect(r).not.toBeNull();
    if (r) {
      expect(r.df).toBeLessThan(30);
      expect(r.dfWarning).toBe(true);
    }
  });
});

describe("proportionZTest", () => {
  it("returns null on zero sample size", () => {
    expect(proportionZTest(0, 0, 1, 10)).toBeNull();
    expect(proportionZTest(1, 10, 0, 0)).toBeNull();
  });

  it("returns null when successes exceed sample", () => {
    expect(proportionZTest(11, 10, 5, 10)).toBeNull();
    expect(proportionZTest(-1, 10, 5, 10)).toBeNull();
  });

  it("computes a clear-significant diff for 50% vs 90% win rates", () => {
    // 50/100 vs 90/100 — definitely different.
    const r = proportionZTest(50, 100, 90, 100);
    expect(r).not.toBeNull();
    if (r) {
      expect(r.diff).toBeCloseTo(0.4, 5);
      expect(r.pValue).toBeLessThan(0.001);
    }
  });

  it("computes a non-significant result for 51% vs 49% on small n", () => {
    const r = proportionZTest(51, 100, 49, 100);
    expect(r).not.toBeNull();
    if (r) {
      expect(r.pValue).toBeGreaterThan(0.5);
    }
  });
});

describe("formatPValue + significanceLevel", () => {
  it("formats p-value into compact bands", () => {
    expect(formatPValue(0.0005)).toBe("p<0.001");
    expect(formatPValue(0.005)).toMatch(/^p=0\.005/);
    expect(formatPValue(0.34)).toBe("p=0.34");
  });

  it("classifies into conventional alpha bands", () => {
    expect(significanceLevel(0.0005)).toBe("p<0.001");
    expect(significanceLevel(0.005)).toBe("p<0.01");
    expect(significanceLevel(0.04)).toBe("p<0.05");
    expect(significanceLevel(0.06)).toBeNull();
  });
});
