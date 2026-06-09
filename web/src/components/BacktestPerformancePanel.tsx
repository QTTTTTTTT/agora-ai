import React, { useMemo, useState } from "react";
import {
  Area,
  AreaChart,
  Brush,
  CartesianGrid,
  Legend,
  ResponsiveContainer,
  Tooltip,
  TooltipProps,
  XAxis,
  YAxis,
} from "recharts";
import type {
  BacktestBenchmarkPoint,
  BacktestMetricsView,
  BacktestNavPoint,
} from "../lib/api";
import { formatNumberForLanguage, type AppLanguage } from "../lib/preferences";

/**
 * Stage 1 — US-equity backtest performance view.
 *
 * Three-series area chart (strategy / benchmark / excess) +
 * a 10-cell KPI strip above it. Matches the visual idiom of the
 * reference mock the team approved (see build/mockups/*.png).
 *
 * Renders nothing if navCurve is empty. Benchmark / excess lines
 * are conditional on benchmarkCurve being present and matching
 * navCurve in length — when a backtest ran without a benchmark
 * (legacy jobs / opt-out) we degrade to a single-line strategy
 * chart, the KPI strip drops the benchmark-relative cells.
 */

interface Props {
  benchmarkSymbol?: string;
  benchmarkCurve?: BacktestBenchmarkPoint[];
  navCurve: BacktestNavPoint[];
  metrics: BacktestMetricsView;
  initialCash: number;
  finalNav: number;
  language: AppLanguage;
}

type PeriodKey = "1M" | "6M" | "1Y" | "3Y" | "ALL";

interface PlotRow {
  date: string;
  /** Strategy cumulative return (%) — what NAV/initialCash * 100 - 100 produces. */
  strategy: number;
  /** Benchmark cumulative return (%). Optional. */
  benchmark?: number;
  /** Excess return (strategy - benchmark) (%). Optional. */
  excess?: number;
}

// Brand colors — match the reference mock:
//   strategy  → blue   (#1890ff)
//   benchmark → red    (#ff4d4f)
//   excess    → gold   (#faad14)
const COLOR_STRATEGY = "#1890ff";
const COLOR_BENCHMARK = "#ff4d4f";
const COLOR_EXCESS = "#faad14";

const COPY = {
  zh: {
    title: "策略业绩",
    kpiCumulative: "策略总收益",
    kpiBenchmark: "基准总收益",
    kpiExcess: "超额总收益",
    kpiAnnualized: "年化收益",
    kpiSharpe: "Sharpe",
    kpiMaxDD: "最大回撤",
    kpiExcessMaxDD: "超额最大回撤",
    kpiAlpha: "Alpha (年化)",
    kpiBeta: "Beta",
    kpiInfoRatio: "信息比率",
    legendStrategy: "策略收益率",
    legendBenchmark: "基准收益率",
    legendExcess: "超额收益",
    period1M: "近一月",
    period6M: "近半年",
    period1Y: "近一年",
    period3Y: "近三年",
    periodAll: "全部",
    noData: "暂无业绩数据",
    benchmarkSymbolFallback: "未设置基准",
  },
  en: {
    title: "Performance",
    kpiCumulative: "Strategy Return",
    kpiBenchmark: "Benchmark Return",
    kpiExcess: "Excess Return",
    kpiAnnualized: "Annualized",
    kpiSharpe: "Sharpe",
    kpiMaxDD: "Max Drawdown",
    kpiExcessMaxDD: "Excess Max DD",
    kpiAlpha: "Alpha (Ann.)",
    kpiBeta: "Beta",
    kpiInfoRatio: "Info Ratio",
    legendStrategy: "Strategy",
    legendBenchmark: "Benchmark",
    legendExcess: "Excess",
    period1M: "1M",
    period6M: "6M",
    period1Y: "1Y",
    period3Y: "3Y",
    periodAll: "ALL",
    noData: "No performance data",
    benchmarkSymbolFallback: "No benchmark",
  },
} as const;

const PERIOD_DAYS: Record<PeriodKey, number | null> = {
  "1M": 22,
  "6M": 132,
  "1Y": 252,
  "3Y": 756,
  ALL: null,
};

type PerformanceCopy = {
  legendStrategy: string;
  legendBenchmark: string;
  legendExcess: string;
};

// Module-level tooltip renderer factory — avoids the
// react-hooks/static-components lint by keeping the component
// declaration outside the parent's render loop, and avoids
// Recharts' generic Tooltip typing pain by returning a plain
// function the Tooltip's `content` prop accepts. Captured props
// are stable (language/copy/hasBenchmark) — Recharts only re-runs
// this on hover state changes.
function makeTooltipRenderer(
  copy: PerformanceCopy,
  language: AppLanguage,
  hasBenchmark: boolean,
): (props: TooltipProps<number, string>) => React.ReactElement | null {
  return (tip) => {
    const { active, payload, label } = tip;
    if (!active || !payload || !payload.length) return null;
    const map = new Map<string, number>();
    payload.forEach((p) => {
      if (p.dataKey && typeof p.value === "number") map.set(String(p.dataKey), p.value);
    });
    const fmt = (v: number | undefined) => {
      if (v === undefined || !Number.isFinite(v)) return "—";
      return `${formatNumberForLanguage(v, language, { maximumFractionDigits: 2 })}%`;
    };
    return (
      <div className="rounded-md border border-slate-200 bg-white px-3 py-2 text-xs shadow-md">
        <div className="font-medium text-slate-900">{String(label)}</div>
        <div className="mt-1 space-y-0.5">
          <div className="flex items-center gap-2">
            <span className="inline-block h-2 w-2 rounded-full" style={{ background: COLOR_STRATEGY }} />
            <span className="text-slate-600">{copy.legendStrategy}:</span>
            <span className="font-medium text-slate-900">{fmt(map.get("strategy"))}</span>
          </div>
          {hasBenchmark && map.has("benchmark") ? (
            <div className="flex items-center gap-2">
              <span className="inline-block h-2 w-2 rounded-full" style={{ background: COLOR_BENCHMARK }} />
              <span className="text-slate-600">{copy.legendBenchmark}:</span>
              <span className="font-medium text-slate-900">{fmt(map.get("benchmark"))}</span>
            </div>
          ) : null}
          {hasBenchmark && map.has("excess") ? (
            <div className="flex items-center gap-2">
              <span className="inline-block h-2 w-2 rounded-full" style={{ background: COLOR_EXCESS }} />
              <span className="text-slate-600">{copy.legendExcess}:</span>
              <span className="font-medium text-slate-900">{fmt(map.get("excess"))}</span>
            </div>
          ) : null}
        </div>
      </div>
    );
  };
}

const BacktestPerformancePanel: React.FC<Props> = ({
  benchmarkSymbol,
  benchmarkCurve,
  navCurve,
  metrics,
  initialCash,
  language,
}) => {
  const lang = language === "zh-CN" ? "zh" : "en";
  const copy = COPY[lang];
  const [period, setPeriod] = useState<PeriodKey>("ALL");

  const hasBenchmark = !!benchmarkCurve && benchmarkCurve.length === navCurve.length && navCurve.length >= 2;

  // Build the unified series the chart actually plots. Returns are
  // expressed in percentage points (so 18.5 means +18.5%) — that
  // way the Y-axis tick labels can be "%-suffixed" without any
  // scaling math at render time.
  const allRows: PlotRow[] = useMemo(() => {
    if (!navCurve.length || initialCash <= 0) return [];
    return navCurve.map((p, i) => {
      const stratPct = (p.nav / initialCash - 1) * 100;
      const benchPct = hasBenchmark && benchmarkCurve ? benchmarkCurve[i].pct * 100 : undefined;
      const excess = benchPct !== undefined ? stratPct - benchPct : undefined;
      return {
        date: typeof p.date === "string" ? p.date.slice(0, 10) : new Date(p.date).toISOString().slice(0, 10),
        strategy: stratPct,
        benchmark: benchPct,
        excess,
      };
    });
  }, [navCurve, benchmarkCurve, hasBenchmark, initialCash]);

  const rows = useMemo(() => {
    const days = PERIOD_DAYS[period];
    if (days === null || days >= allRows.length) return allRows;
    return allRows.slice(allRows.length - days);
  }, [allRows, period]);

  if (!allRows.length) {
    return (
      <div className="rounded-xl border border-slate-200 bg-white p-6 text-center text-sm text-slate-500">
        {copy.noData}
      </div>
    );
  }

  const pct = (v: number | undefined, digits = 2): string => {
    if (v === undefined || !Number.isFinite(v)) return "—";
    return `${formatNumberForLanguage(v * 100, language, { maximumFractionDigits: digits })}%`;
  };
  const num = (v: number | undefined, digits = 2): string => {
    if (v === undefined || !Number.isFinite(v)) return "—";
    return formatNumberForLanguage(v, language, { maximumFractionDigits: digits });
  };
  const tone = (v: number | undefined): string => {
    if (v === undefined || !Number.isFinite(v)) return "text-slate-400";
    if (v > 0) return "text-emerald-600";
    if (v < 0) return "text-rose-600";
    return "text-slate-700";
  };

  const kpiCells: { label: string; value: string; toneClass: string; hidden?: boolean }[] = [
    {
      label: copy.kpiCumulative,
      value: pct(metrics.cumulativeReturn),
      toneClass: tone(metrics.cumulativeReturn),
    },
    {
      label: copy.kpiBenchmark,
      value: pct(metrics.benchmarkCumulativeReturn),
      toneClass: tone(metrics.benchmarkCumulativeReturn),
      hidden: !hasBenchmark,
    },
    {
      label: copy.kpiExcess,
      value: pct(metrics.excessReturn),
      toneClass: tone(metrics.excessReturn),
      hidden: !hasBenchmark,
    },
    {
      label: copy.kpiAnnualized,
      value: pct(metrics.annualizedReturn),
      toneClass: tone(metrics.annualizedReturn),
    },
    {
      label: copy.kpiSharpe,
      value: num(metrics.sharpeRatio),
      toneClass: "text-slate-900",
    },
    {
      label: copy.kpiMaxDD,
      value: pct(metrics.maxDrawdown),
      toneClass: "text-rose-600",
    },
    {
      label: copy.kpiExcessMaxDD,
      value: pct(metrics.excessMaxDrawdown),
      toneClass: "text-rose-600",
      hidden: !hasBenchmark,
    },
    {
      label: copy.kpiAlpha,
      value: pct(metrics.alpha),
      toneClass: tone(metrics.alpha),
      hidden: !hasBenchmark,
    },
    {
      label: copy.kpiBeta,
      value: num(metrics.beta),
      toneClass: "text-slate-900",
      hidden: !hasBenchmark,
    },
    {
      label: copy.kpiInfoRatio,
      value: num(metrics.informationRatio),
      toneClass: "text-slate-900",
      hidden: !hasBenchmark,
    },
  ];
  const visibleKpis = kpiCells.filter((k) => !k.hidden);

  return (
    <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
      <header className="mb-4 flex flex-wrap items-center justify-between gap-3">
        <div>
          <h3 className="text-sm font-semibold text-slate-900">{copy.title}</h3>
          <p className="mt-0.5 text-xs text-slate-500">
            {hasBenchmark
              ? `${benchmarkSymbol ?? ""} · ${allRows[0]?.date} → ${allRows[allRows.length - 1]?.date}`
              : `${copy.benchmarkSymbolFallback} · ${allRows[0]?.date} → ${allRows[allRows.length - 1]?.date}`}
          </p>
        </div>
        <div className="flex items-center gap-1 rounded-md bg-slate-100 p-1 text-xs">
          {(Object.keys(PERIOD_DAYS) as PeriodKey[]).map((k) => (
            <button
              key={k}
              type="button"
              onClick={() => setPeriod(k)}
              className={`rounded px-2 py-1 transition-colors ${
                period === k ? "bg-white text-slate-900 shadow-sm" : "text-slate-500 hover:text-slate-700"
              }`}
            >
              {k === "1M" ? copy.period1M : k === "6M" ? copy.period6M : k === "1Y" ? copy.period1Y : k === "3Y" ? copy.period3Y : copy.periodAll}
            </button>
          ))}
        </div>
      </header>

      <div
        className="mb-4 grid gap-3 rounded-lg border border-slate-100 bg-slate-50/60 p-3"
        style={{ gridTemplateColumns: `repeat(${Math.min(visibleKpis.length, 5)}, minmax(0, 1fr))` }}
      >
        {visibleKpis.map((cell) => (
          <div key={cell.label} className="flex flex-col">
            <span className="text-[11px] uppercase tracking-wide text-slate-500">{cell.label}</span>
            <span className={`mt-1 text-base font-semibold ${cell.toneClass}`}>{cell.value}</span>
          </div>
        ))}
      </div>

      <div className="h-[360px] w-full">
        <ResponsiveContainer width="100%" height="100%">
          <AreaChart data={rows} margin={{ top: 8, right: 16, bottom: 8, left: 4 }}>
            <defs>
              <linearGradient id="bpStrategyGrad" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={COLOR_STRATEGY} stopOpacity={0.25} />
                <stop offset="100%" stopColor={COLOR_STRATEGY} stopOpacity={0.0} />
              </linearGradient>
              <linearGradient id="bpExcessGrad" x1="0" y1="0" x2="0" y2="1">
                <stop offset="0%" stopColor={COLOR_EXCESS} stopOpacity={0.2} />
                <stop offset="100%" stopColor={COLOR_EXCESS} stopOpacity={0.0} />
              </linearGradient>
            </defs>
            <CartesianGrid stroke="#e5e7eb" strokeDasharray="3 3" />
            <XAxis dataKey="date" tick={{ fontSize: 11 }} minTickGap={48} />
            <YAxis
              tickFormatter={(v: number) => `${formatNumberForLanguage(v, language, { maximumFractionDigits: 0 })}%`}
              tick={{ fontSize: 11 }}
              width={56}
            />
            <Tooltip content={makeTooltipRenderer(copy, language, hasBenchmark)} />
            <Legend wrapperStyle={{ fontSize: 12 }} />
            <Area
              type="monotone"
              dataKey="strategy"
              name={copy.legendStrategy}
              stroke={COLOR_STRATEGY}
              strokeWidth={2}
              fill="url(#bpStrategyGrad)"
              isAnimationActive={false}
              dot={false}
            />
            {hasBenchmark ? (
              <Area
                type="monotone"
                dataKey="benchmark"
                name={copy.legendBenchmark}
                stroke={COLOR_BENCHMARK}
                strokeWidth={2}
                fill="none"
                isAnimationActive={false}
                dot={false}
              />
            ) : null}
            {hasBenchmark ? (
              <Area
                type="monotone"
                dataKey="excess"
                name={copy.legendExcess}
                stroke={COLOR_EXCESS}
                strokeWidth={1.5}
                strokeDasharray="4 2"
                fill="url(#bpExcessGrad)"
                isAnimationActive={false}
                dot={false}
              />
            ) : null}
            <Brush
              dataKey="date"
              height={24}
              stroke={COLOR_STRATEGY}
              travellerWidth={8}
              tickFormatter={(v: string) => (typeof v === "string" ? v.slice(5) : v)}
            />
          </AreaChart>
        </ResponsiveContainer>
      </div>
    </section>
  );
};

export default BacktestPerformancePanel;
