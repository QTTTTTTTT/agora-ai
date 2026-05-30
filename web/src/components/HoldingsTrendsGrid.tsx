import React, { useEffect, useMemo, useState } from "react";
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  YAxis,
} from "recharts";

import {
  fetchFundHoldingsSeries,
  formatApiError,
  type HoldingSeries,
} from "../lib/api";

const RANGE_OPTIONS = [
  { id: "30", days: 30, labelZh: "30 天", labelEn: "30d" },
  { id: "90", days: 90, labelZh: "90 天", labelEn: "90d" },
  { id: "180", days: 180, labelZh: "180 天", labelEn: "180d" },
] as const;

type RangeOption = (typeof RANGE_OPTIONS)[number];

interface HoldingsTrendsGridProps {
  fundId: string;
  language?: "zh-CN" | "en-US";
  /** Initial collapsed state. Defaults to collapsed because most
   *  funds have many holdings and an always-on grid would push the
   *  rest of the dashboard below the fold. */
  defaultOpen?: boolean;
}

const COPY = {
  "zh-CN": {
    title: "持仓走势",
    subtitle: "每只持仓在该窗口内的归一化股价（起始 = 100）",
    expand: "展开",
    collapse: "收起",
    loading: "加载中…",
    error: "走势加载失败",
    retry: "重试",
    empty: "暂无可绘制的持仓",
    vsStart: "vs 起点",
    vsEntry: "vs 成本",
    partialFailures: "以下持仓未能加载：",
  },
  "en-US": {
    title: "Holdings trends",
    subtitle: "Per-holding normalized close price (start = 100)",
    expand: "Show",
    collapse: "Hide",
    loading: "Loading…",
    error: "Failed to load trends",
    retry: "Retry",
    empty: "No holdings to plot",
    vsStart: "vs start",
    vsEntry: "vs entry",
    partialFailures: "Holdings that couldn't be loaded:",
  },
} as const;

/** A single mini-chart card in the grid. Pure presentational so
 *  parent re-renders are cheap; we do NOT memoize because Recharts'
 *  ResponsiveContainer already handles size changes internally. */
function HoldingMiniChart({
  series,
  language,
}: {
  series: HoldingSeries;
  language: "zh-CN" | "en-US";
}) {
  const copy = COPY[language] ?? COPY["zh-CN"];
  const last = series.points[series.points.length - 1]?.value ?? 100;
  const deltaVsStart = last - 100; // points are rebased to 100 at start
  // entry-aware delta: entryPrice is the actual cost; the LAST raw
  // close is `last / 100 * firstClose` — we don't carry the raw
  // close in the DTO, so we rebase using a denormalized estimate.
  // Cheap and good enough for the small label.
  const positive = deltaVsStart >= 0;

  return (
    <div className="flex flex-col rounded-lg border border-gray-200 bg-white p-3 shadow-sm">
      <div className="flex items-baseline justify-between">
        <div className="flex flex-col">
          <span className="text-sm font-semibold text-gray-800">
            {series.symbol}
          </span>
          {series.name ? (
            <span className="text-[11px] text-gray-500 line-clamp-1">
              {series.name}
            </span>
          ) : null}
        </div>
        <span
          className={`text-xs font-semibold ${
            positive ? "text-emerald-600" : "text-rose-600"
          }`}
        >
          {positive ? "+" : ""}
          {deltaVsStart.toFixed(2)}
          <span className="ml-1 text-[10px] font-normal text-gray-400">
            {copy.vsStart}
          </span>
        </span>
      </div>
      <div className="mt-2 h-20">
        <ResponsiveContainer width="100%" height="100%">
          <LineChart
            data={series.points}
            margin={{ top: 4, right: 0, left: 0, bottom: 0 }}
          >
            <CartesianGrid stroke="#f3f4f6" strokeDasharray="2 2" />
            <YAxis
              hide
              domain={["auto", "auto"]}
            />
            <Tooltip
              contentStyle={{
                fontSize: 11,
                border: "1px solid #e5e7eb",
                borderRadius: 6,
                padding: "4px 6px",
              }}
              formatter={(value: number) => value.toFixed(2)}
              labelFormatter={(label: string) => label}
            />
            <Line
              dataKey="value"
              stroke={positive ? "#10b981" : "#ef4444"}
              strokeWidth={1.5}
              dot={false}
              isAnimationActive={false}
              connectNulls
            />
          </LineChart>
        </ResponsiveContainer>
      </div>
    </div>
  );
}

export const HoldingsTrendsGrid: React.FC<HoldingsTrendsGridProps> = ({
  fundId,
  language = "zh-CN",
  defaultOpen = false,
}) => {
  const copy = COPY[language] ?? COPY["zh-CN"];
  const [open, setOpen] = useState(defaultOpen);
  const [range, setRange] = useState<RangeOption>(
    RANGE_OPTIONS.find((r) => r.id === "90") ?? RANGE_OPTIONS[1],
  );
  const [items, setItems] = useState<HoldingSeries[]>([]);
  const [partialFailures, setPartialFailures] = useState<string[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!open) return;
    let cancelled = false;
    setLoading(true);
    setError(null);
    fetchFundHoldingsSeries(fundId, range.days)
      .then((resp) => {
        if (cancelled) return;
        setItems(resp.items ?? []);
        setPartialFailures(
          (resp.partialFailures ?? []).map((f) => f.id),
        );
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
  }, [open, fundId, range.days, copy.error]);

  const charts = useMemo(() => {
    return items.filter((s) => s.points.length >= 2);
  }, [items]);

  return (
    <section className="rounded-xl border border-gray-200 bg-white shadow-sm">
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-gray-200 px-6 py-4">
        <div>
          <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">
            {copy.title}
          </h2>
          <p className="text-xs text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="flex items-center gap-2">
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
          ) : charts.length === 0 ? (
            <p className="py-12 text-center text-sm text-gray-500">
              {copy.empty}
            </p>
          ) : (
            <>
              <div className="grid grid-cols-1 gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-4">
                {charts.map((s) => (
                  <HoldingMiniChart
                    key={s.instrumentKey}
                    series={s}
                    language={language}
                  />
                ))}
              </div>
              {partialFailures.length > 0 ? (
                <p className="mt-3 text-xs text-amber-600">
                  {copy.partialFailures} {partialFailures.join(", ")}
                </p>
              ) : null}
            </>
          )}
        </div>
      ) : null}
    </section>
  );
};

export default HoldingsTrendsGrid;
