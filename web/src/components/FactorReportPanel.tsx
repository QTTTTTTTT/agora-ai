import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  formatApiError,
  runFactorReport,
  runWalkForwardFactor,
  type FactorReportView,
  type WalkForwardFactorResultView,
} from "../lib/api";
import { formatNumberForLanguage, type AppLanguage } from "../lib/preferences";

/**
 * Stage 2 — Factor IC/IR/分层 report panel.
 *
 * Rendered as a self-contained card on the Backtest page. The
 * user clicks "Run" → the panel POSTs to /api/factorlab/reports
 * with default settings (synthetic fixture, all 5 MVP factors,
 * horizons 5d + 22d, layered at 22d) and renders:
 *
 *   - one row per factor with the headline IC/IR table
 *   - a 5-quintile bar (visual quick-take on monotonicity)
 *   - a long/short Sharpe + max-drawdown pill
 *   - a "qualified" badge that aggregates IC/IR/t/positive ratio
 *     against the canonical thresholds
 *
 * Kept deliberately simple: no chart libraries are pulled in.
 * The quintile bar uses inline SVG; the IC time series is omitted
 * from the MVP (we keep the response payload smaller for now —
 * a future iteration can add a sparkline using the existing
 * Recharts integration).
 */

interface Props {
  language: AppLanguage;
}

const COPY = {
  zh: {
    title: "因子诊断 (IC / IR / 分层)",
    subtitle: "在合成 universe 上做横截面 IC 检验。每个因子按 Spearman / Pearson IC 和分层收益输出。",
    runCta: "运行因子报告",
    runningCta: "运行中...",
    refresh: "重新运行",
    factorCol: "因子",
    horizonCol: "Horizon",
    spearmanIC: "Spearman IC",
    spearmanIR: "IR",
    spearmanTStat: "t-stat",
    posRatio: "正 IC 占比",
    quintile: "5 分位平均收益（周期内）",
    spread: "L5 - L1 年化",
    longShortSharpe: "多空 Sharpe",
    longShortMDD: "多空最大回撤",
    universeSize: "Universe 中位数",
    observationDays: "观测天数",
    qualified: "通过",
    notQualified: "未通过",
    qualificationDetails: "(阈值：|IC|>0.025 & IR>0.4 & t>2 & 正IC≥53% & 多空Sharpe>0.8)",
    empty: "尚未运行报告。点击右上角按钮生成。",
    error: "运行失败",
    walkForwardCta: "Walk-Forward 稳定性",
    walkForwardLoading: "5 折回测中...",
    walkForwardClose: "收起 Walk-Forward",
    walkForwardMeanIC: "平均 IC",
    walkForwardMinIC: "最差 fold IC",
    walkForwardStability: "同方向 fold 占比",
    walkForwardQualified: "通过 fold 数",
    walkForwardFoldHeader: "Fold",
    walkForwardFoldWindow: "时间窗口",
    walkForwardFoldObs: "观测",
    walkForwardFoldIC: "IC",
    walkForwardFoldIR: "IR",
    walkForwardFoldLS: "L/S Sharpe",
  },
  en: {
    title: "Factor Diagnostics (IC / IR / Layered)",
    subtitle: "Cross-sectional IC tests on the synthetic universe. Each factor reports Spearman / Pearson IC and layered returns.",
    runCta: "Run factor report",
    runningCta: "Running...",
    refresh: "Re-run",
    factorCol: "Factor",
    horizonCol: "Horizon",
    spearmanIC: "Spearman IC",
    spearmanIR: "IR",
    spearmanTStat: "t-stat",
    posRatio: "+IC ratio",
    quintile: "Quintile mean return (per period)",
    spread: "L5 - L1 annual",
    longShortSharpe: "L/S Sharpe",
    longShortMDD: "L/S Max DD",
    universeSize: "Median universe",
    observationDays: "Observation days",
    qualified: "Qualified",
    notQualified: "Not qualified",
    qualificationDetails: "(thresholds: |IC|>0.025 & IR>0.4 & t>2 & +IC≥53% & L/S Sharpe>0.8)",
    empty: "No report yet. Click the button to run.",
    error: "Run failed",
    walkForwardCta: "Walk-Forward stability",
    walkForwardLoading: "Running 5 folds...",
    walkForwardClose: "Hide Walk-Forward",
    walkForwardMeanIC: "Mean IC",
    walkForwardMinIC: "Worst-fold IC",
    walkForwardStability: "Same-sign folds",
    walkForwardQualified: "Qualified folds",
    walkForwardFoldHeader: "Fold",
    walkForwardFoldWindow: "Window",
    walkForwardFoldObs: "Obs",
    walkForwardFoldIC: "IC",
    walkForwardFoldIR: "IR",
    walkForwardFoldLS: "L/S Sharpe",
  },
} as const;

const FactorReportPanel: React.FC<Props> = ({ language }) => {
  const lang = language === "zh-CN" ? "zh" : "en";
  const copy = COPY[lang];

  const [reports, setReports] = useState<FactorReportView[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const handleRun = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const out = await runFactorReport({
        horizons: [5, 22],
        layeredHorizonDays: 22,
      });
      setReports(out);
    } catch (e) {
      setError(formatApiError(e, lang === "zh" ? "运行因子报告失败" : "Failed to run factor report"));
    } finally {
      setLoading(false);
    }
  }, [lang]);

  // Auto-run on first mount so the empty state isn't sticky.
  useEffect(() => {
    if (reports === null && !loading && !error) {
      void handleRun();
    }
  }, [reports, loading, error, handleRun]);

  const pct = useCallback(
    (v: number | undefined, digits = 2) => {
      if (v === undefined || !Number.isFinite(v)) return "—";
      return `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: digits })}%`;
    },
    [language],
  );
  const num = useCallback(
    (v: number | undefined, digits = 2) => {
      if (v === undefined || !Number.isFinite(v)) return "—";
      return formatNumberForLanguage(v, language, { maximumFractionDigits: digits });
    },
    [language],
  );

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <header className="mb-3 flex items-start justify-between gap-4">
        <div>
          <h3 className="text-sm font-semibold text-slate-900">{copy.title}</h3>
          <p className="mt-1 text-xs text-slate-500">{copy.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void handleRun()}
          disabled={loading}
          className="rounded-md bg-slate-900 px-3 py-1.5 text-xs font-medium text-white shadow-sm transition-colors hover:bg-slate-800 disabled:cursor-not-allowed disabled:bg-slate-400"
        >
          {loading ? copy.runningCta : reports ? copy.refresh : copy.runCta}
        </button>
      </header>

      {error ? (
        <div className="mt-2 rounded-md border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-700">
          <span className="font-medium">{copy.error}: </span>
          {error}
        </div>
      ) : null}

      {!reports && !loading ? (
        <div className="rounded-md border border-dashed border-slate-200 bg-slate-50/50 px-3 py-6 text-center text-xs text-slate-500">
          {copy.empty}
        </div>
      ) : null}

      {reports ? (
        <div className="space-y-4">
          {reports.map((r) => (
            <FactorRow key={r.factorName} report={r} copy={copy} pct={pct} num={num} lang={lang} />
          ))}
        </div>
      ) : null}

      {reports ? (
        <p className="mt-3 text-[11px] text-slate-400">{copy.qualificationDetails}</p>
      ) : null}
    </section>
  );
};

type FactorRowCopy = Record<keyof (typeof COPY)["zh"], string>;

interface FactorRowProps {
  report: FactorReportView;
  copy: FactorRowCopy;
  pct: (v: number | undefined, digits?: number) => string;
  num: (v: number | undefined, digits?: number) => string;
  lang: "zh" | "en";
}

const FactorRow: React.FC<FactorRowProps> = ({ report, copy, pct, num, lang }) => {
  const [wfResult, setWfResult] = useState<WalkForwardFactorResultView | null>(null);
  const [wfLoading, setWfLoading] = useState(false);
  const [wfError, setWfError] = useState<string | null>(null);

  const handleWfToggle = useCallback(async () => {
    if (wfResult) {
      setWfResult(null);
      return;
    }
    setWfLoading(true);
    setWfError(null);
    try {
      const res = await runWalkForwardFactor({
        factorName: report.factorName,
        numFolds: 5,
        horizons: [22],
      });
      setWfResult(res);
    } catch (e) {
      setWfError(formatApiError(e, lang === "zh" ? "Walk-Forward 失败" : "Walk-forward failed"));
    } finally {
      setWfLoading(false);
    }
  }, [wfResult, report.factorName, lang]);
  // Sort horizon keys numerically — Object.keys order on a JSON
  // object isn't guaranteed and we want 5d before 22d.
  const horizonKeys = useMemo(
    () => Object.keys(report.ic).sort((a, b) => Number(a) - Number(b)),
    [report.ic],
  );

  const quintile = report.layered?.quintileMeanReturn ?? null;

  // Compute max abs quintile so the SVG bars are scaled relative.
  const quintileMaxAbs = useMemo(() => {
    if (!quintile) return 0;
    return quintile.reduce((m, v) => Math.max(m, Math.abs(v)), 0);
  }, [quintile]);

  return (
    <div
      className={`rounded-lg border p-4 ${
        report.qualified ? "border-emerald-200 bg-emerald-50/30" : "border-slate-200 bg-white"
      }`}
    >
      <div className="mb-3 flex flex-wrap items-center justify-between gap-3">
        <div>
          <div className="flex items-center gap-2">
            <span className="font-mono text-sm font-semibold text-slate-900">{report.factorName}</span>
            <span
              className={`inline-flex rounded-full px-2 py-0.5 text-[10px] font-medium ${
                report.qualified
                  ? "bg-emerald-100 text-emerald-700"
                  : "bg-slate-100 text-slate-500"
              }`}
            >
              {report.qualified ? `✓ ${copy.qualified}` : `· ${copy.notQualified}`}
            </span>
          </div>
          <div className="mt-1 text-[11px] text-slate-500">
            {copy.observationDays}: {report.observationDays} · {copy.universeSize}: {report.universeMedianSize}
          </div>
        </div>
        {report.longShort ? (
          <div className="flex items-center gap-3 text-xs">
            <div className="flex flex-col items-end">
              <span className="text-[10px] uppercase text-slate-500">{copy.longShortSharpe}</span>
              <span
                className={`font-semibold ${
                  (report.longShort.sharpe ?? 0) > 0 ? "text-emerald-700" : "text-rose-700"
                }`}
              >
                {num(report.longShort.sharpe)}
              </span>
            </div>
            <div className="flex flex-col items-end">
              <span className="text-[10px] uppercase text-slate-500">{copy.longShortMDD}</span>
              <span className="font-semibold text-rose-700">{pct(report.longShort.maxDrawdown)}</span>
            </div>
            {report.layered ? (
              <div className="flex flex-col items-end">
                <span className="text-[10px] uppercase text-slate-500">{copy.spread}</span>
                <span
                  className={`font-semibold ${
                    (report.layered.spreadAnnual ?? 0) > 0 ? "text-emerald-700" : "text-rose-700"
                  }`}
                >
                  {pct(report.layered.spreadAnnual)}
                </span>
              </div>
            ) : null}
          </div>
        ) : null}
      </div>

      <div className="overflow-x-auto">
        <table className="min-w-full text-xs">
          <thead className="text-[10px] uppercase tracking-wide text-slate-500">
            <tr>
              <th className="py-1 pr-3 text-left">{copy.horizonCol}</th>
              <th className="py-1 pr-3 text-right">{copy.spearmanIC}</th>
              <th className="py-1 pr-3 text-right">{copy.spearmanIR}</th>
              <th className="py-1 pr-3 text-right">{copy.spearmanTStat}</th>
              <th className="py-1 pr-3 text-right">{copy.posRatio}</th>
            </tr>
          </thead>
          <tbody>
            {horizonKeys.map((k) => {
              const s = report.ic[k];
              const passesIR = Math.abs(s.spearmanIR) >= 0.4;
              const passesT = Math.abs(s.spearmanTStat) >= 2.0;
              return (
                <tr key={k} className="border-t border-slate-100">
                  <td className="py-1.5 pr-3 font-mono text-slate-700">{s.horizonDays}d</td>
                  <td
                    className={`py-1.5 pr-3 text-right font-mono ${
                      Math.abs(s.spearmanMean) >= 0.025 ? "font-semibold text-slate-900" : "text-slate-500"
                    }`}
                  >
                    {num(s.spearmanMean, 4)}
                  </td>
                  <td className={`py-1.5 pr-3 text-right font-mono ${passesIR ? "text-emerald-700" : "text-slate-500"}`}>
                    {num(s.spearmanIR, 3)}
                  </td>
                  <td className={`py-1.5 pr-3 text-right font-mono ${passesT ? "text-emerald-700" : "text-slate-500"}`}>
                    {num(s.spearmanTStat, 2)}
                  </td>
                  <td className="py-1.5 pr-3 text-right font-mono text-slate-700">
                    {pct(s.positiveICRatio, 1)}
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>

      {quintile ? (
        <div className="mt-3">
          <div className="mb-1 flex items-baseline justify-between">
            <span className="text-[11px] uppercase tracking-wide text-slate-500">
              {copy.quintile} (h={report.layered?.horizonDays}d)
            </span>
            {report.layered?.monotonic ? (
              <span className="text-[10px] font-semibold text-emerald-700">↑ monotonic</span>
            ) : (
              <span className="text-[10px] text-slate-400">non-monotonic</span>
            )}
          </div>
          <svg viewBox="0 0 500 80" className="h-20 w-full" preserveAspectRatio="none">
            {/* zero line */}
            <line x1="0" x2="500" y1="40" y2="40" stroke="#cbd5e1" strokeWidth="1" />
            {quintile.map((v, i) => {
              const w = 80;
              const gap = 15;
              const x = i * (w + gap) + 30;
              const scale = quintileMaxAbs > 0 ? Math.abs(v) / quintileMaxAbs : 0;
              const h = scale * 32;
              const y = v >= 0 ? 40 - h : 40;
              const color = v >= 0 ? "#10b981" : "#f43f5e";
              return (
                <g key={i}>
                  <rect x={x} y={y} width={w} height={h} fill={color} opacity={0.85} />
                  <text x={x + w / 2} y={68} textAnchor="middle" fontSize="9" fill="#64748b">
                    Q{i + 1}
                  </text>
                  <text x={x + w / 2} y={78} textAnchor="middle" fontSize="9" fill="#0f172a" fontFamily="ui-monospace, monospace">
                    {pct(v, 2)}
                  </text>
                </g>
              );
            })}
          </svg>
        </div>
      ) : null}

      <div className="mt-3 border-t border-slate-100 pt-3">
        <button
          type="button"
          onClick={() => void handleWfToggle()}
          disabled={wfLoading}
          className="rounded-md border border-slate-200 px-2 py-1 text-[11px] font-medium text-slate-700 transition-colors hover:border-slate-400 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {wfLoading ? copy.walkForwardLoading : wfResult ? copy.walkForwardClose : copy.walkForwardCta}
        </button>

        {wfError ? (
          <div className="mt-2 rounded border border-rose-200 bg-rose-50 px-2 py-1.5 text-[11px] text-rose-700">
            {wfError}
          </div>
        ) : null}

        {wfResult ? (
          <div className="mt-3 space-y-2">
            <div className="grid grid-cols-4 gap-3 rounded-md bg-slate-50 p-2 text-[11px]">
              <KpiCell label={copy.walkForwardMeanIC} value={num(wfResult.meanIC22d, 4)} />
              <KpiCell
                label={copy.walkForwardMinIC}
                value={num(wfResult.minIC22d, 4)}
                tone={wfResult.minIC22d < 0 ? "text-rose-600" : "text-slate-900"}
              />
              <KpiCell
                label={copy.walkForwardStability}
                value={pct(wfResult.icStabilityRatio, 0)}
                tone={wfResult.icStabilityRatio >= 0.8 ? "text-emerald-600" : "text-slate-900"}
              />
              <KpiCell
                label={copy.walkForwardQualified}
                value={`${wfResult.qualifiedFoldCount} / ${wfResult.numFolds}`}
                tone={wfResult.allFoldsQualified ? "text-emerald-600" : "text-slate-900"}
              />
            </div>

            <div className="overflow-x-auto">
              <table className="min-w-full text-[11px]">
                <thead className="text-[10px] uppercase tracking-wide text-slate-500">
                  <tr>
                    <th className="py-1 pr-2 text-left">{copy.walkForwardFoldHeader}</th>
                    <th className="py-1 pr-2 text-left">{copy.walkForwardFoldWindow}</th>
                    <th className="py-1 pr-2 text-right">{copy.walkForwardFoldObs}</th>
                    <th className="py-1 pr-2 text-right">{copy.walkForwardFoldIC}</th>
                    <th className="py-1 pr-2 text-right">{copy.walkForwardFoldIR}</th>
                    <th className="py-1 pr-2 text-right">{copy.walkForwardFoldLS}</th>
                  </tr>
                </thead>
                <tbody>
                  {wfResult.folds.map((f) => (
                    <tr key={f.index} className="border-t border-slate-100">
                      <td className="py-1 pr-2 font-mono text-slate-700">#{f.index + 1}</td>
                      <td className="py-1 pr-2 text-slate-600">
                        {f.error ? (
                          <span className="text-rose-600">{f.error}</span>
                        ) : (
                          `${f.startDate.slice(0, 10)} → ${f.endDate.slice(0, 10)}`
                        )}
                      </td>
                      <td className="py-1 pr-2 text-right font-mono text-slate-700">{f.observationDays}</td>
                      <td
                        className={`py-1 pr-2 text-right font-mono ${
                          f.spearmanMean > 0 ? "text-emerald-700" : f.spearmanMean < 0 ? "text-rose-700" : "text-slate-500"
                        }`}
                      >
                        {num(f.spearmanMean, 4)}
                      </td>
                      <td className="py-1 pr-2 text-right font-mono text-slate-700">{num(f.spearmanIR, 3)}</td>
                      <td className="py-1 pr-2 text-right font-mono text-slate-700">{num(f.longShortSharpe, 2)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
};

const KpiCell: React.FC<{ label: string; value: string; tone?: string }> = ({ label, value, tone }) => (
  <div className="flex flex-col">
    <span className="text-[10px] uppercase tracking-wide text-slate-500">{label}</span>
    <span className={`mt-0.5 font-semibold ${tone ?? "text-slate-900"}`}>{value}</span>
  </div>
);

export default FactorReportPanel;
