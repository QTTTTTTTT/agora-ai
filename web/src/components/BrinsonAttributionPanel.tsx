// BrinsonAttributionPanel — per-fund Brinson three-effect runner
// (S7 / P3-4).
//
// The PM picks a benchmark + bucket dimension, hits "Run", and
// sees:
//   - Aggregate active-return decomposition (portfolio, benchmark,
//     allocation / selection / interaction totals).
//   - Per-bucket drill-down with weights, returns, and each
//     effect signed so red-bad / green-good is consistent.
//
// Read-only: the panel never mutates the benchmark catalog. Admin
// CRUD lives in AdminBrinsonCompositionsSection.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  fetchFundBrinsonHistory,
  formatApiError,
  listBrinsonBenchmarksForFund,
  runFundBrinsonAttribution,
  type BrinsonBenchmarkSummary,
  type BrinsonBucketDimension,
  type BrinsonResult,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface BrinsonAttributionMessages {
  panelTitle: string;
  panelSubtitle: string;
  runButton: string;
  running: string;
  benchmarkLabel: string;
  benchmarkPlaceholder: string;
  dimensionLabel: string;
  dimensionAssetClass: string;
  dimensionMarket: string;
  dimensionSector: string;
  benchmarkEmpty: string;
  portfolioReturn: string;
  benchmarkReturn: string;
  activeReturn: string;
  allocationEffect: string;
  selectionEffect: string;
  interactionEffect: string;
  bucketsTitle: string;
  bucketsEmpty: string;
  colBucket: string;
  colPortfolioWeight: string;
  colBenchmarkWeight: string;
  colPortfolioReturn: string;
  colBenchmarkReturn: string;
  colAllocation: string;
  colSelection: string;
  colInteraction: string;
  colTotal: string;
  persistLabel: string;
  error: string;
}

const brinsonAttributionMessages: Record<Language, BrinsonAttributionMessages> = {
  "zh-CN": {
    panelTitle: "Brinson 业绩归因",
    panelSubtitle: "将组合相对基准的超额收益拆解为配置效应、选股效应和交互效应。先在管理员后台维护基准成分，再在此面板按资产类别 / 市场维度运行归因。",
    runButton: "运行归因",
    running: "计算中…",
    benchmarkLabel: "基准",
    benchmarkPlaceholder: "选择基准…",
    dimensionLabel: "分桶维度",
    dimensionAssetClass: "资产类别",
    dimensionMarket: "市场",
    dimensionSector: "行业（暂未支持）",
    benchmarkEmpty: "尚未配置基准成分，请联系管理员到后台添加",
    portfolioReturn: "组合收益",
    benchmarkReturn: "基准收益",
    activeReturn: "主动收益",
    allocationEffect: "配置效应",
    selectionEffect: "选股效应",
    interactionEffect: "交互效应",
    bucketsTitle: "分桶明细",
    bucketsEmpty: "暂无分桶明细",
    colBucket: "分桶",
    colPortfolioWeight: "组合权重",
    colBenchmarkWeight: "基准权重",
    colPortfolioReturn: "组合收益",
    colBenchmarkReturn: "基准收益",
    colAllocation: "配置",
    colSelection: "选股",
    colInteraction: "交互",
    colTotal: "合计",
    persistLabel: "存档此次归因",
    error: "归因运行失败",
  },
  "en-US": {
    panelTitle: "Brinson Attribution",
    panelSubtitle: "Decompose active return into allocation, selection and interaction effects per bucket. Admin maintains the benchmark composition; the fund-level runner derives the portfolio side from live holdings.",
    runButton: "Run attribution",
    running: "Running…",
    benchmarkLabel: "Benchmark",
    benchmarkPlaceholder: "Pick a benchmark…",
    dimensionLabel: "Dimension",
    dimensionAssetClass: "Asset class",
    dimensionMarket: "Market",
    dimensionSector: "Sector (n/a)",
    benchmarkEmpty: "No benchmark compositions available — ask an admin to seed the catalog",
    portfolioReturn: "Portfolio return",
    benchmarkReturn: "Benchmark return",
    activeReturn: "Active return",
    allocationEffect: "Allocation effect",
    selectionEffect: "Selection effect",
    interactionEffect: "Interaction effect",
    bucketsTitle: "Per-bucket detail",
    bucketsEmpty: "No bucket detail",
    colBucket: "Bucket",
    colPortfolioWeight: "Port. wt",
    colBenchmarkWeight: "Bench. wt",
    colPortfolioReturn: "Port. ret",
    colBenchmarkReturn: "Bench. ret",
    colAllocation: "Allocation",
    colSelection: "Selection",
    colInteraction: "Interaction",
    colTotal: "Total",
    persistLabel: "Archive this run",
    error: "Attribution failed",
  },
};

interface BrinsonAttributionPanelProps {
  fundId?: string;
  language?: Language;
}

function fmtPct(value: number): string {
  return `${(value * 100).toFixed(2)}%`;
}

function dimensionLabel(m: BrinsonAttributionMessages, d: BrinsonBucketDimension): string {
  if (d === "asset_class") return m.dimensionAssetClass;
  if (d === "market") return m.dimensionMarket;
  if (d === "sector") return m.dimensionSector;
  return d;
}

function classForSign(v: number): string {
  if (v > 0.0000001) return "text-emerald-600";
  if (v < -0.0000001) return "text-rose-600";
  return "text-gray-700";
}

export default function BrinsonAttributionPanel({
  fundId,
  language = "zh-CN",
}: BrinsonAttributionPanelProps) {
  const m = useMemo(() => brinsonAttributionMessages[language], [language]);
  const [benchmarks, setBenchmarks] = useState<BrinsonBenchmarkSummary[]>([]);
  const [dimensions, setDimensions] = useState<BrinsonBucketDimension[]>([]);
  const [benchmarkId, setBenchmarkId] = useState<string>("");
  const [dimension, setDimension] = useState<BrinsonBucketDimension>("asset_class");
  const [persist, setPersist] = useState(false);
  const [result, setResult] = useState<BrinsonResult | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const loadCatalog = useCallback(async () => {
    try {
      const resp = await listBrinsonBenchmarksForFund();
      setBenchmarks(resp.benchmarks ?? []);
      setDimensions(resp.dimensions ?? []);
      if (!benchmarkId && resp.benchmarks?.[0]) {
        setBenchmarkId(resp.benchmarks[0].benchmark_id);
        setDimension(resp.benchmarks[0].dimension);
      }
    } catch (err) {
      setError(formatApiError(err, m.error));
    }
  }, [benchmarkId, m.error]);

  useEffect(() => {
    loadCatalog().catch(() => {});
  }, [loadCatalog]);

  const runAttribution = useCallback(async () => {
    if (!fundId || !benchmarkId) return;
    setLoading(true);
    setError(null);
    try {
      const resp = await runFundBrinsonAttribution(fundId, {
        benchmarkId,
        dimension,
        persist,
      });
      setResult(resp.result);
      if (persist) {
        // Refresh history downstream if any panel surfaces it.
        await fetchFundBrinsonHistory(fundId, { benchmarkId, dimension, limit: 30 }).catch(() => {});
      }
    } catch (err) {
      setError(formatApiError(err, m.error));
      setResult(null);
    } finally {
      setLoading(false);
    }
  }, [fundId, benchmarkId, dimension, persist, m.error]);

  // Dimensions available for the selected benchmark. We surface
  // the dimensions for which the admin actually published a
  // composition.
  const dimensionsForBenchmark = useMemo(() => {
    if (!benchmarkId) return dimensions;
    const dims = new Set<BrinsonBucketDimension>();
    for (const b of benchmarks) {
      if (b.benchmark_id === benchmarkId) dims.add(b.dimension);
    }
    return Array.from(dims);
  }, [benchmarkId, benchmarks, dimensions]);

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="min-w-0">
          <h2 className="text-base font-semibold text-gray-900">{m.panelTitle}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.panelSubtitle}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <label className="text-xs text-gray-500">{m.benchmarkLabel}</label>
          <select
            value={benchmarkId}
            onChange={(e) => setBenchmarkId(e.target.value)}
            className="min-w-[12rem] rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            <option value="">{m.benchmarkPlaceholder}</option>
            {[...new Set(benchmarks.map((b) => b.benchmark_id))].map((bid) => (
              <option key={bid} value={bid}>
                {bid}
              </option>
            ))}
          </select>
          <label className="text-xs text-gray-500">{m.dimensionLabel}</label>
          <select
            value={dimension}
            onChange={(e) => setDimension(e.target.value as BrinsonBucketDimension)}
            className="rounded-md border border-gray-200 bg-white px-2 py-1 text-xs text-gray-700"
          >
            {(dimensionsForBenchmark.length > 0 ? dimensionsForBenchmark : dimensions).map((d) => (
              <option key={d} value={d}>
                {dimensionLabel(m, d)}
              </option>
            ))}
          </select>
          <label className="flex items-center gap-1 text-xs text-gray-500">
            <input
              type="checkbox"
              checked={persist}
              onChange={(e) => setPersist(e.target.checked)}
            />
            {m.persistLabel}
          </label>
          <button
            type="button"
            disabled={!fundId || !benchmarkId || loading}
            onClick={() => runAttribution().catch(() => {})}
            className="rounded-md border border-blue-200 bg-blue-50 px-3 py-1.5 text-xs font-medium text-blue-700 hover:bg-blue-100 disabled:cursor-not-allowed disabled:opacity-50"
          >
            {loading ? m.running : m.runButton}
          </button>
        </div>
      </header>

      {benchmarks.length === 0 && (
        <p className="rounded-md border border-amber-100 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          {m.benchmarkEmpty}
        </p>
      )}

      {error && (
        <div className="rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-xs text-amber-800">
          {error}
        </div>
      )}

      {result && (
        <>
          <div className="grid grid-cols-1 gap-4 rounded-md border border-gray-100 bg-gray-50 p-4 text-xs text-gray-600 md:grid-cols-6">
            <div>
              <div className="text-gray-400">{m.portfolioReturn}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.portfolio_return)}`}>
                {fmtPct(result.portfolio_return)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.benchmarkReturn}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.benchmark_return)}`}>
                {fmtPct(result.benchmark_return)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.activeReturn}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.active_return)}`}>
                {fmtPct(result.active_return)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.allocationEffect}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.allocation_total)}`}>
                {fmtPct(result.allocation_total)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.selectionEffect}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.selection_total)}`}>
                {fmtPct(result.selection_total)}
              </div>
            </div>
            <div>
              <div className="text-gray-400">{m.interactionEffect}</div>
              <div className={`mt-1 font-mono text-base ${classForSign(result.interaction_total)}`}>
                {fmtPct(result.interaction_total)}
              </div>
            </div>
          </div>

          <div>
            <h3 className="mb-2 text-sm font-semibold text-gray-800">{m.bucketsTitle}</h3>
            {result.buckets.length === 0 ? (
              <p className="rounded-md border border-gray-100 bg-gray-50 px-3 py-6 text-center text-xs text-gray-500">
                {m.bucketsEmpty}
              </p>
            ) : (
              <div className="overflow-x-auto rounded-md border border-gray-100">
                <table className="min-w-full text-xs">
                  <thead className="bg-gray-50 text-gray-500">
                    <tr>
                      <th className="px-3 py-2 text-left">{m.colBucket}</th>
                      <th className="px-3 py-2 text-right">{m.colPortfolioWeight}</th>
                      <th className="px-3 py-2 text-right">{m.colBenchmarkWeight}</th>
                      <th className="px-3 py-2 text-right">{m.colPortfolioReturn}</th>
                      <th className="px-3 py-2 text-right">{m.colBenchmarkReturn}</th>
                      <th className="px-3 py-2 text-right">{m.colAllocation}</th>
                      <th className="px-3 py-2 text-right">{m.colSelection}</th>
                      <th className="px-3 py-2 text-right">{m.colInteraction}</th>
                      <th className="px-3 py-2 text-right">{m.colTotal}</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-gray-100">
                    {result.buckets.map((b) => (
                      <tr key={b.key}>
                        <td className="px-3 py-2 font-mono text-gray-800">{b.key}</td>
                        <td className="px-3 py-2 text-right font-mono text-gray-700">{fmtPct(b.portfolio_weight)}</td>
                        <td className="px-3 py-2 text-right font-mono text-gray-700">{fmtPct(b.benchmark_weight)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${classForSign(b.portfolio_return)}`}>{fmtPct(b.portfolio_return)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${classForSign(b.benchmark_return)}`}>{fmtPct(b.benchmark_return)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${classForSign(b.allocation)}`}>{fmtPct(b.allocation)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${classForSign(b.selection)}`}>{fmtPct(b.selection)}</td>
                        <td className={`px-3 py-2 text-right font-mono ${classForSign(b.interaction)}`}>{fmtPct(b.interaction)}</td>
                        <td className={`px-3 py-2 text-right font-mono font-semibold ${classForSign(b.total_effect)}`}>{fmtPct(b.total_effect)}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
    </section>
  );
}
