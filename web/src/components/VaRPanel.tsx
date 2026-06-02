// VaRPanel — per-fund Value-at-Risk + Conditional VaR dashboard
// (S7 / P3-2).
//
// Renders nine tiles: three methods (historical / parametric /
// Monte Carlo) × three confidences (90 / 95 / 99) for one
// horizon. The PM toggles the horizon dropdown (1d / 5d / 10d)
// and sees how the methods agree (small spread = calm market) or
// disagree (large spread = fat-tail risk the parametric branch
// undersells).
//
// Header summarises the sample window so it's obvious "this
// number reflects the past year" rather than the past month.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  fetchVaRSnapshot,
  formatApiError,
  type VaRSnapshot,
  type VaRMethod,
  type VaRConfidence,
} from "../lib/api";
import {
  ALL_VAR_METHODS,
  ALL_VAR_CONFIDENCES,
} from "@fundai/api-client";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  insufficientHistory: string;
  sampleSizeLabel: string;
  lookbackLabel: string;
  horizonLabel: string;
  horizon1d: string;
  horizon5d: string;
  horizon10d: string;
  meanLabel: string;
  stdevLabel: string;
  sampleWindowLabel: string;
  methodLabel: string;
  confidenceLabel: string;
  varLabel: string;
  cvarLabel: string;
  methodHistorical: string;
  methodParametric: string;
  methodMonteCarlo: string;
  methodHistoricalSubtitle: string;
  methodParametricSubtitle: string;
  methodMonteCarloSubtitle: string;
  confidence90Label: string;
  confidence95Label: string;
  confidence99Label: string;
  varInterpretation: string;
  cvarInterpretation: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "风险价值 (VaR / CVaR)",
    panelSubtitle:
      "基于 nav_snapshots.daily_return 时序，对历史模拟 / 参数法 / 蒙特卡洛三种方法在 90% / 95% / 99% 三档置信度下计算单期最大可能损失。三种方法的散布本身就是 fat-tail 诊断。",
    refresh: "刷新",
    loading: "计算中…",
    empty: "暂无数据",
    error: "加载失败",
    insufficientHistory: "历史样本不足，请先累积至少 20 个交易日的 NAV 序列。",
    sampleSizeLabel: "样本数",
    lookbackLabel: "回看",
    horizonLabel: "持有期",
    horizon1d: "1 日",
    horizon5d: "5 日",
    horizon10d: "10 日",
    meanLabel: "日均收益",
    stdevLabel: "波动率",
    sampleWindowLabel: "样本区间",
    methodLabel: "方法",
    confidenceLabel: "置信度",
    varLabel: "VaR",
    cvarLabel: "CVaR",
    methodHistorical: "历史模拟",
    methodParametric: "参数法",
    methodMonteCarlo: "蒙特卡洛",
    methodHistoricalSubtitle: "非参数：对收益序列直接取分位数",
    methodParametricSubtitle: "正态闭式：μ − z·σ",
    methodMonteCarloSubtitle: "5 万次抽样 N(μ, σ) 取分位数",
    confidence90Label: "90%",
    confidence95Label: "95%",
    confidence99Label: "99%",
    varInterpretation: "下一个持有期内损失大概率不超过此值（按置信度）",
    cvarInterpretation: "条件尾部期望：在 VaR 被突破时的平均损失",
  },
  "en-US": {
    panelTitle: "Value at Risk (VaR / CVaR)",
    panelSubtitle:
      "One-period worst-case loss across three methods (historical / parametric / Monte Carlo) and three confidence levels. The spread between methods itself is a fat-tail diagnostic.",
    refresh: "Refresh",
    loading: "Computing…",
    empty: "No data",
    error: "Failed to load",
    insufficientHistory:
      "Not enough history — accumulate at least 20 trading days of NAV first.",
    sampleSizeLabel: "Sample",
    lookbackLabel: "Lookback",
    horizonLabel: "Horizon",
    horizon1d: "1 day",
    horizon5d: "5 days",
    horizon10d: "10 days",
    meanLabel: "Mean daily return",
    stdevLabel: "Volatility",
    sampleWindowLabel: "Sample window",
    methodLabel: "Method",
    confidenceLabel: "Confidence",
    varLabel: "VaR",
    cvarLabel: "CVaR",
    methodHistorical: "Historical",
    methodParametric: "Parametric",
    methodMonteCarlo: "Monte Carlo",
    methodHistoricalSubtitle: "Non-parametric: percentile of realised returns",
    methodParametricSubtitle: "Normal closed-form μ − z·σ",
    methodMonteCarloSubtitle: "50 000 draws from N(μ, σ), empirical percentile",
    confidence90Label: "90%",
    confidence95Label: "95%",
    confidence99Label: "99%",
    varInterpretation:
      "Expected one-period loss won't exceed this number at the confidence level",
    cvarInterpretation:
      "Conditional tail expectation — average loss when VaR is breached",
  },
};

interface VaRPanelProps {
  fundId?: string;
  language?: Language;
}

const REFRESH_MS = 5 * 60_000;
const DEFAULT_LOOKBACK = 252;

function methodLabel(m: Messages, method: VaRMethod): string {
  switch (method) {
    case "historical":
      return m.methodHistorical;
    case "parametric":
      return m.methodParametric;
    case "monte_carlo":
      return m.methodMonteCarlo;
    default:
      return method;
  }
}

function methodSubtitle(m: Messages, method: VaRMethod): string {
  switch (method) {
    case "historical":
      return m.methodHistoricalSubtitle;
    case "parametric":
      return m.methodParametricSubtitle;
    case "monte_carlo":
      return m.methodMonteCarloSubtitle;
    default:
      return "";
  }
}

function confidenceLabel(m: Messages, c: VaRConfidence): string {
  switch (c) {
    case 0.9:
      return m.confidence90Label;
    case 0.95:
      return m.confidence95Label;
    case 0.99:
      return m.confidence99Label;
    default:
      return String(c);
  }
}

// Format a fractional loss as a signed percentage with 2 dp.
function formatPct(value: number): string {
  const pct = value * 100;
  if (Math.abs(pct) < 0.001) {
    return "0.00%";
  }
  return `${pct.toFixed(2)}%`;
}

export default function VaRPanel({ fundId, language = "zh-CN" }: VaRPanelProps) {
  const m = useMemo(() => messages[language], [language]);
  const [snap, setSnap] = useState<VaRSnapshot | null>(null);
  const [horizon, setHorizon] = useState<number>(1);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const fetchSnap = useCallback(async () => {
    if (!fundId) {
      setSnap(null);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchVaRSnapshot(fundId, {
        lookback: DEFAULT_LOOKBACK,
        horizon,
      });
      setSnap(resp.snapshot);
    } catch (err) {
      setError(formatApiError(err, m.error));
      setSnap(null);
    } finally {
      setLoading(false);
    }
  }, [fundId, horizon, m.error]);

  useEffect(() => {
    fetchSnap().catch(() => {});
    const id = window.setInterval(() => {
      fetchSnap().catch(() => {});
    }, REFRESH_MS);
    return () => window.clearInterval(id);
  }, [fetchSnap]);

  // Index results by (method, confidence) so the 3×3 grid lookup
  // is O(1) regardless of backend ordering.
  const tileFor = useCallback(
    (method: VaRMethod, conf: VaRConfidence) => {
      if (!snap) return null;
      return (
        snap.results.find(
          (r) => r.method === method && Math.abs(r.confidence - conf) < 1e-6,
        ) ?? null
      );
    },
    [snap],
  );

  // Insufficient-history errors come back as 422 with a known
  // code; the front-end translates that to a friendlier hint than
  // the generic error string.
  const insufficient = error && /insufficient_history/.test(error);

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <label className="text-xs text-gray-500">{m.horizonLabel}</label>
          <select
            value={horizon}
            onChange={(e) => setHorizon(Number(e.target.value))}
            className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            <option value={1}>{m.horizon1d}</option>
            <option value={5}>{m.horizon5d}</option>
            <option value={10}>{m.horizon10d}</option>
          </select>
          <button
            type="button"
            onClick={() => fetchSnap().catch(() => {})}
            className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-medium text-gray-600 hover:bg-gray-50"
          >
            {m.refresh}
          </button>
        </div>
      </header>

      {error && (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          {insufficient ? m.insufficientHistory : error}
        </div>
      )}

      {snap && (
        <div className="grid grid-cols-2 gap-4 rounded-md border border-gray-100 bg-gray-50 p-4 text-xs text-gray-600 md:grid-cols-5">
          <div>
            <div className="text-gray-400">{m.sampleSizeLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {snap.sample_size}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.lookbackLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {snap.lookback_days}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.meanLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {formatPct(snap.mean_daily_return)}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.stdevLabel}</div>
            <div className="mt-1 font-mono text-base text-gray-900">
              {formatPct(snap.stdev_daily_return)}
            </div>
          </div>
          <div>
            <div className="text-gray-400">{m.sampleWindowLabel}</div>
            <div className="mt-1 font-mono text-xs text-gray-700">
              {snap.sample_window_start ?? "—"} → {snap.sample_window_end ?? "—"}
            </div>
          </div>
        </div>
      )}

      {loading && !snap && (
        <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
          {m.loading}
        </p>
      )}

      {!loading && !snap && !error && (
        <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
          {m.empty}
        </p>
      )}

      {snap && (
        <div className="space-y-3">
          {ALL_VAR_METHODS.map((method) => (
            <div key={method} className="rounded-md border border-gray-100 p-3">
              <div className="flex items-baseline justify-between gap-3">
                <div>
                  <div className="text-sm font-semibold text-gray-800">
                    {methodLabel(m, method)}
                  </div>
                  <div className="mt-0.5 text-[11px] text-gray-500">
                    {methodSubtitle(m, method)}
                  </div>
                </div>
              </div>
              <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-3">
                {ALL_VAR_CONFIDENCES.map((conf) => {
                  const tile = tileFor(method, conf);
                  return (
                    <div
                      key={`${method}-${conf}`}
                      className="rounded-md border border-gray-100 bg-gray-50 p-3"
                    >
                      <div className="text-[11px] text-gray-500">
                        {m.confidenceLabel} · {confidenceLabel(m, conf)}
                      </div>
                      <div className="mt-2 flex items-baseline justify-between">
                        <span className="text-[11px] text-gray-500">
                          {m.varLabel}
                        </span>
                        <span className="font-mono text-base text-rose-600">
                          {tile ? formatPct(tile.var_pct) : "—"}
                        </span>
                      </div>
                      <div className="mt-1 flex items-baseline justify-between">
                        <span className="text-[11px] text-gray-500">
                          {m.cvarLabel}
                        </span>
                        <span className="font-mono text-sm text-rose-700">
                          {tile ? formatPct(tile.cvar_pct) : "—"}
                        </span>
                      </div>
                    </div>
                  );
                })}
              </div>
            </div>
          ))}
          <div className="rounded-md border border-gray-100 bg-gray-50 p-3 text-[11px] text-gray-500">
            <div>· {m.varLabel}: {m.varInterpretation}</div>
            <div className="mt-1">· {m.cvarLabel}: {m.cvarInterpretation}</div>
          </div>
        </div>
      )}
    </section>
  );
}
