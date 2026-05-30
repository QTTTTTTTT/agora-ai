import React, { useEffect, useMemo, useState } from "react";
import {
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";

import {
  fetchFundBenchmarkHistory,
  formatApiError,
  type BenchmarkHistoryResponse,
  type BenchmarkSeries,
  type BenchmarkHoldingOverlap,
} from "../lib/api";

/** Days options the user can flip through. The server already
 *  clamps to [7, 1825], so any value here is safe. */
const RANGE_OPTIONS = [
  { id: "30", days: 30, labelZh: "30 天", labelEn: "30d" },
  { id: "90", days: 90, labelZh: "90 天", labelEn: "90d" },
  { id: "180", days: 180, labelZh: "180 天", labelEn: "180d" },
  { id: "365", days: 365, labelZh: "1 年", labelEn: "1y" },
] as const;

type RangeOption = (typeof RANGE_OPTIONS)[number];

/** Display mode toggle. "compare" plots fund + benchmarks side by
 *  side. "alpha" plots a single line: fund - benchmarks[0], i.e.
 *  cumulative outperformance vs the primary benchmark. */
type ChartMode = "compare" | "alpha";

interface BenchmarkChartProps {
  fundId: string;
  language?: "zh-CN" | "en-US";
  /** Initial collapsed state. The chart panel sits below positions
   *  in the fund overview, so we default to expanded for desktop —
   *  it's the panel users actually came to see. */
  defaultOpen?: boolean;
}

/** Pure-function copy table. Avoids dragging in i18next on the web
 *  side; the Android port re-uses the shared catalog via i18n.ts. */
const COPY = {
  "zh-CN": {
    title: "基金 vs 大盘",
    subtitle: "净值与基准指数同起点归一化（起始 = 100）",
    expand: "展开",
    collapse: "收起",
    loading: "加载中…",
    error: "基准加载失败",
    retry: "重试",
    empty: "暂无可对比的净值数据",
    fundLegend: "本基金",
    addBenchmark: "添加基准",
    legendStart: "起始 = 100",
    partialFailures: "部分基准未能加载，已跳过：",
    modeCompare: "对比",
    modeAlpha: "Alpha",
    alphaSubtitle: "本基金相对首选基准的累计超额收益",
    alphaLegend: "Alpha (本基金 − ",
    alphaNoBenchmark: "Alpha 视图需要至少一个基准",
    alphaZeroLine: "0 = 与基准持平",
    overlapDominantTitle: "本基金主仓 ≈ 大盘",
    overlapDominantBody:
      "基金主要持仓与所选基准为同一标的，对比模式下两条曲线会高度重合；建议切换 Alpha 视图查看相对超额收益。",
    overlapPartialTitle: "部分持仓与基准重叠",
    overlapPartialBody:
      "基金部分持仓与所选基准为同一标的，可切到 Alpha 视图观察相对走势。",
    overlapSwitchToAlpha: "切换到 Alpha 视图",
  },
  "en-US": {
    title: "Fund vs Market",
    subtitle: "Fund NAV and benchmarks rebased to 100 at start",
    expand: "Show",
    collapse: "Hide",
    loading: "Loading…",
    error: "Failed to load benchmarks",
    retry: "Retry",
    empty: "No NAV history yet",
    fundLegend: "This fund",
    addBenchmark: "Add benchmark",
    legendStart: "start = 100",
    partialFailures: "Some benchmarks could not be loaded:",
    modeCompare: "Compare",
    modeAlpha: "Alpha",
    alphaSubtitle: "Cumulative outperformance vs the primary benchmark",
    alphaLegend: "Alpha (fund − ",
    alphaNoBenchmark: "Alpha view requires at least one benchmark",
    alphaZeroLine: "0 = matches benchmark",
    overlapDominantTitle: "Your fund ≈ this benchmark",
    overlapDominantBody:
      "The fund's largest position is the same instrument as the selected benchmark — the two lines in Compare track each other tightly. Switch to Alpha for relative outperformance.",
    overlapPartialTitle: "Holdings overlap the benchmark",
    overlapPartialBody:
      "Some of the fund's holdings overlap the selected benchmark. Switch to Alpha for relative performance.",
    overlapSwitchToAlpha: "Switch to Alpha view",
  },
} as const;

/** Recharts requires data points to share a common shape. We merge
 *  the fund + each benchmark series into a single row keyed by
 *  date string. Missing dates leave the corresponding series gap.
 *
 *  This is pure: deterministic on input, no side effects. Tests
 *  could be added if the merging logic grows; for now visual
 *  inspection in the dashboard is sufficient. */
function mergeSeriesByDate(
  fund: BenchmarkSeries,
  benchmarks: BenchmarkSeries[],
): Array<Record<string, string | number>> {
  const dates = new Set<string>();
  for (const p of fund.points) {
    dates.add(p.date);
  }
  for (const series of benchmarks) {
    for (const p of series.points) {
      dates.add(p.date);
    }
  }
  const sorted = Array.from(dates).sort();

  const fundIndex = new Map(fund.points.map((p) => [p.date, p.value]));
  const benchIndex = benchmarks.map(
    (s) => [s, new Map(s.points.map((p) => [p.date, p.value]))] as const,
  );

  return sorted.map((date) => {
    const row: Record<string, string | number> = { date };
    const fv = fundIndex.get(date);
    if (typeof fv === "number") {
      row.fund = fv;
    }
    for (const [series, idx] of benchIndex) {
      const bv = idx.get(date);
      if (typeof bv === "number") {
        row[series.id] = bv;
      }
    }
    return row;
  });
}

/** Compute the alpha series: fund.value - primary.value at each
 *  shared date. Mirrors benchmark.AlphaSpread on the server but
 *  runs in the browser so toggling between modes is instant.
 *
 *  A 0 value means "matched the benchmark" — the chart's reference
 *  line. Positive = outperformance, negative = underperformance.
 *
 *  Returns an empty array when either side is empty or no dates
 *  overlap; the caller renders an empty state in that case. */
function computeAlphaSeries(
  fund: BenchmarkSeries,
  primary: BenchmarkSeries | null,
): Array<Record<string, string | number>> {
  if (!primary || fund.points.length === 0 || primary.points.length === 0) {
    return [];
  }
  const primaryIdx = new Map(primary.points.map((p) => [p.date, p.value]));
  return fund.points
    .map((fp) => {
      const bv = primaryIdx.get(fp.date);
      if (typeof bv !== "number") return null;
      return { date: fp.date, alpha: fp.value - bv };
    })
    .filter((r): r is { date: string; alpha: number } => r !== null);
}

/** Stable color palette so the same benchmark gets the same line
/** Stable color palette so the same benchmark gets the same line
 *  color across renders / range flips. Hand-picked for contrast on
 *  white backgrounds; the fund line uses the indigo accent that
 *  matches the rest of the dashboard. */
const BENCHMARK_COLORS = [
  "#0ea5e9", // sky-500
  "#10b981", // emerald-500
  "#f59e0b", // amber-500
  "#ef4444", // red-500
  "#8b5cf6", // violet-500
  "#ec4899", // pink-500
];

function colorForSeries(id: string, index: number): string {
  // Deterministic hash → palette index so same id keeps same color
  // even if order changes (e.g., user toggles series).
  let h = 0;
  for (let i = 0; i < id.length; i += 1) {
    h = (h * 31 + id.charCodeAt(i)) | 0;
  }
  if (h < 0) h = -h;
  return BENCHMARK_COLORS[(h + index) % BENCHMARK_COLORS.length];
}

/**
 * HoldingOverlapBanner — surfaces the server's `holdingOverlap`
 * hint when the fund's holdings structurally overlap a rendered
 * benchmark (e.g., a futures fund 100% in BTCUSDT alongside a
 * btc_usdt benchmark line). In Compare mode the two lines would
 * track each other almost perfectly, which looks like a flat
 * uninformative chart; the banner names the overlapping benchmark
 * and offers a one-click switch to the Alpha view, where the
 * structural overlap is differenced out and only residual
 * outperformance shows.
 *
 * Pure presentational: takes the overlap DTO + the rendered
 * benchmarks (so we can label by benchmark.label rather than the
 * less friendly id), and a callback to flip the parent's mode.
 *
 * Render decision:
 *   "dominant" → strong copy, prominent indigo accent.
 *   "partial"  → softer copy, amber accent (informational, not
 *                a "you should switch" mandate).
 *   any other  → don't render (defensive against future strengths).
 */
function HoldingOverlapBanner({
  copy,
  overlap,
  benchmarks,
  onSwitchToAlpha,
}: {
  copy: (typeof COPY)[keyof typeof COPY];
  overlap: BenchmarkHoldingOverlap;
  benchmarks: BenchmarkSeries[];
  onSwitchToAlpha: () => void;
}): React.ReactElement | null {
  if (!overlap || !overlap.primaryBenchmark) return null;
  const dominant = overlap.overlapStrength === "dominant";
  const partial = overlap.overlapStrength === "partial";
  if (!dominant && !partial) return null;

  // Resolve the benchmark label by id; fall back to the id itself
  // if the chart is rendering a benchmark id we somehow don't
  // have in the visible list (shouldn't happen given the wiring,
  // but defensive).
  const benchLabel =
    benchmarks.find((b) => b.id === overlap.primaryBenchmark)?.label ??
    overlap.primaryBenchmark;
  const matched = (overlap.matchedSymbols ?? []).join(", ");

  const containerClass = dominant
    ? "rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-3 text-sm text-indigo-900"
    : "rounded-lg border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900";

  return (
    <div className={`mb-3 ${containerClass}`}>
      <div className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <p className="font-semibold">
            {dominant ? copy.overlapDominantTitle : copy.overlapPartialTitle}
          </p>
          <p className="mt-1 text-xs leading-relaxed">
            {dominant ? copy.overlapDominantBody : copy.overlapPartialBody}{" "}
            <span className="font-medium">
              ({benchLabel}
              {matched ? ` ↔ ${matched}` : ""})
            </span>
          </p>
        </div>
        <button
          type="button"
          onClick={onSwitchToAlpha}
          className={`shrink-0 rounded-md px-3 py-1.5 text-xs font-medium ${
            dominant
              ? "bg-indigo-600 text-white hover:bg-indigo-700"
              : "bg-amber-600 text-white hover:bg-amber-700"
          }`}
        >
          {copy.overlapSwitchToAlpha}
        </button>
      </div>
    </div>
  );
}

export const BenchmarkChart: React.FC<BenchmarkChartProps> = ({
  fundId,
  language = "zh-CN",
  defaultOpen = true,
}) => {
  const copy = COPY[language] ?? COPY["zh-CN"];
  const [open, setOpen] = useState(defaultOpen);
  const [mode, setMode] = useState<ChartMode>("compare");
  const [range, setRange] = useState<RangeOption>(
    RANGE_OPTIONS.find((r) => r.id === "90") ?? RANGE_OPTIONS[1],
  );
  // selected = explicit user picks; null means "use server default"
  const [selected, setSelected] = useState<string[] | null>(null);
  const [data, setData] = useState<BenchmarkHistoryResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Lazy fetch: only hit the API when the panel is expanded. A
  // collapsed panel costs zero bandwidth.
  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetchFundBenchmarkHistory(
      fundId,
      range.days,
      selected ?? undefined,
    )
      .then((resp) => {
        if (cancelled) return;
        setData(resp);
      })
      .catch((e: unknown) => {
        if (cancelled) return;
        setError(formatApiError(e, copy.error));
      })
      .finally(() => {
        if (cancelled) return;
        setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, fundId, range.days, selected, copy.error]);

  // The set of series the chart actually renders. When the user
  // hasn't picked, fall back to whatever the server returned (which
  // already applied Recommend rules). Otherwise honour their picks.
  const activeBenchmarks = useMemo<BenchmarkSeries[]>(() => {
    if (!data) return [];
    return data.benchmarks;
  }, [data]);

  const merged = useMemo(() => {
    if (!data) return [];
    return mergeSeriesByDate(data.fund, activeBenchmarks);
  }, [data, activeBenchmarks]);

  // Alpha view computes fund - primary benchmark[0] in the browser.
  // Recomputes on data / mode change; cheap (single pass over fund.points).
  const primaryBenchmark = activeBenchmarks[0] ?? null;
  const alphaSeries = useMemo(
    () => (data ? computeAlphaSeries(data.fund, primaryBenchmark) : []),
    [data, primaryBenchmark],
  );

  // Picker uses server's catalog so we don't have to ship a
  // duplicate list on the client.
  const availableForPicker = data?.available ?? [];
  const currentIds = data
    ? selected ?? activeBenchmarks.map((s) => s.id)
    : [];

  const handleToggleSeries = (id: string) => {
    setSelected((prev) => {
      const base = prev ?? currentIds;
      if (base.includes(id)) {
        const next = base.filter((x) => x !== id);
        // Empty selection wraps back to "use server default" so the
        // server still renders something. Otherwise the chart goes
        // fund-only which is rarely what the user wanted.
        return next.length === 0 ? null : next;
      }
      return [...base, id];
    });
  };

  return (
    <section className="rounded-xl border border-gray-200 bg-white shadow-sm">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 px-6 py-4">
        <div>
          <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">
            {copy.title}
          </h2>
          <p className="text-xs text-gray-500">
            {mode === "alpha" ? copy.alphaSubtitle : copy.subtitle}
          </p>
        </div>
        <div className="flex items-center gap-2">
          <div className="flex overflow-hidden rounded-lg border border-gray-200 text-xs">
            <button
              type="button"
              onClick={() => setMode("compare")}
              className={`px-3 py-1.5 transition ${
                mode === "compare"
                  ? "bg-indigo-600 text-white"
                  : "bg-white text-gray-600 hover:bg-gray-50"
              }`}
            >
              {copy.modeCompare}
            </button>
            <button
              type="button"
              onClick={() => setMode("alpha")}
              className={`px-3 py-1.5 transition ${
                mode === "alpha"
                  ? "bg-indigo-600 text-white"
                  : "bg-white text-gray-600 hover:bg-gray-50"
              }`}
            >
              {copy.modeAlpha}
            </button>
          </div>
          <div className="flex overflow-hidden rounded-lg border border-gray-200 text-xs">
            {RANGE_OPTIONS.map((opt) => (
              <button
                key={opt.id}
                type="button"
                onClick={() => setRange(opt)}
                className={`px-3 py-1.5 transition ${
                  range.id === opt.id
                    ? "bg-indigo-600 text-white"
                    : "bg-white text-gray-600 hover:bg-gray-50"
                }`}
              >
                {language === "zh-CN" ? opt.labelZh : opt.labelEn}
              </button>
            ))}
          </div>
          <button
            type="button"
            onClick={() => setOpen((v) => !v)}
            className="rounded-lg border border-gray-200 px-3 py-1.5 text-xs text-gray-600 hover:bg-gray-50"
          >
            {open ? copy.collapse : copy.expand}
          </button>
        </div>
      </header>

      {open ? (
        <div className="px-6 py-5">
          {loading ? (
            <p className="py-12 text-center text-sm text-gray-500">
              {copy.loading}
            </p>
          ) : error ? (
            <div className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">
              <p>{error}</p>
              <button
                type="button"
                onClick={() => setRange({ ...range })}
                className="mt-2 inline-flex rounded-lg bg-red-600 px-3 py-1.5 text-xs text-white hover:bg-red-700"
              >
                {copy.retry}
              </button>
            </div>
          ) : !data || data.fund.points.length === 0 ? (
            <p className="py-12 text-center text-sm text-gray-500">
              {copy.empty}
            </p>
          ) : mode === "alpha" && !primaryBenchmark ? (
            <p className="py-12 text-center text-sm text-gray-500">
              {copy.alphaNoBenchmark}
            </p>
          ) : (
            <>
              {mode === "compare" && data.holdingOverlap ? (
                <HoldingOverlapBanner
                  copy={copy}
                  overlap={data.holdingOverlap}
                  benchmarks={activeBenchmarks}
                  onSwitchToAlpha={() => setMode("alpha")}
                />
              ) : null}
              <div className="h-72">
                <ResponsiveContainer width="100%" height="100%">
                  {mode === "compare" ? (
                    <LineChart data={merged} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
                      <CartesianGrid stroke="#e5e7eb" strokeDasharray="4 4" />
                      <XAxis
                        dataKey="date"
                        tick={{ fontSize: 11, fill: "#6b7280" }}
                        minTickGap={32}
                      />
                      <YAxis
                        domain={["auto", "auto"]}
                        tick={{ fontSize: 11, fill: "#6b7280" }}
                        width={48}
                      />
                      <Tooltip
                        contentStyle={{
                          fontSize: 12,
                          border: "1px solid #e5e7eb",
                          borderRadius: 8,
                        }}
                        formatter={(value: number) => value.toFixed(2)}
                      />
                      <Legend wrapperStyle={{ fontSize: 12 }} />
                      <Line
                        dataKey="fund"
                        name={copy.fundLegend}
                        stroke="#4f46e5"
                        strokeWidth={2.25}
                        dot={false}
                        isAnimationActive={false}
                        connectNulls
                      />
                      {activeBenchmarks.map((s, i) => (
                        <Line
                          key={s.id}
                          dataKey={s.id}
                          name={s.label}
                          stroke={colorForSeries(s.id, i)}
                          strokeWidth={1.5}
                          strokeDasharray="3 3"
                          dot={false}
                          isAnimationActive={false}
                          connectNulls
                        />
                      ))}
                    </LineChart>
                  ) : (
                    <LineChart data={alphaSeries} margin={{ top: 8, right: 24, left: 0, bottom: 0 }}>
                      <CartesianGrid stroke="#e5e7eb" strokeDasharray="4 4" />
                      <XAxis
                        dataKey="date"
                        tick={{ fontSize: 11, fill: "#6b7280" }}
                        minTickGap={32}
                      />
                      <YAxis
                        domain={["auto", "auto"]}
                        tick={{ fontSize: 11, fill: "#6b7280" }}
                        width={48}
                      />
                      <Tooltip
                        contentStyle={{
                          fontSize: 12,
                          border: "1px solid #e5e7eb",
                          borderRadius: 8,
                        }}
                        formatter={(value: number) => value.toFixed(2)}
                      />
                      <Legend wrapperStyle={{ fontSize: 12 }} />
                      <Line
                        dataKey="alpha"
                        name={`${copy.alphaLegend}${primaryBenchmark?.label ?? ""})`}
                        stroke="#16a34a"
                        strokeWidth={2.25}
                        dot={false}
                        isAnimationActive={false}
                        connectNulls
                      />
                    </LineChart>
                  )}
                </ResponsiveContainer>
              </div>

              <div className="mt-3 text-xs text-gray-400">
                {mode === "alpha" ? copy.alphaZeroLine : copy.legendStart}
              </div>

              {availableForPicker.length > 0 ? (
                <div className="mt-4 flex flex-wrap gap-2">
                  {availableForPicker.map((item) => {
                    const active = currentIds.includes(item.id);
                    return (
                      <button
                        key={item.id}
                        type="button"
                        onClick={() => handleToggleSeries(item.id)}
                        className={`rounded-full border px-3 py-1 text-xs transition ${
                          active
                            ? "border-indigo-500 bg-indigo-50 text-indigo-700"
                            : "border-gray-200 bg-white text-gray-600 hover:bg-gray-50"
                        }`}
                      >
                        {item.label}
                      </button>
                    );
                  })}
                </div>
              ) : null}

              {data.partialFailures && data.partialFailures.length > 0 ? (
                <p className="mt-3 text-xs text-amber-600">
                  {copy.partialFailures}{" "}
                  {data.partialFailures.map((f) => f.id).join(", ")}
                </p>
              ) : null}
            </>
          )}
        </div>
      ) : null}
    </section>
  );
};

export default BenchmarkChart;
