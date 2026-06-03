// AdminLLMObservabilitySection — S14.A "看板" tab for the platform
// LLM provider admin. Shows:
//   * Per-provider health table over a 6h/24h/7d window with an
//     inline 144-point sparkline (5min ticks → 12h of activity).
//   * Per-provider cost totals over a 24h/7d/30d window + per-day
//     stacked area across providers (one stack per day).
//   * Status pills for "probe ticks since boot" and "rollup ticks
//     since boot" so the operator can confirm the loops are alive
//     without opening the server logs.
//
// Recharts is already in the bundle (Usage.tsx etc.), so we reuse it
// rather than rolling SVG by hand. The component is deliberately
// independent of AdminLLMProvidersSection so the diff stays
// reviewable and the two can be repositioned in the page later.

import { useCallback, useEffect, useMemo, useState } from "react";
import {
  Area,
  AreaChart,
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
  formatApiError,
  getAdminProviderCostDashboard,
  getAdminProviderHealthDashboard,
  type ProviderCostDashboardResponse,
  type ProviderHealthDashboardResponse,
  type ProviderHealthDashboardRow,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  refresh: string;
  healthHeader: string;
  costHeader: string;
  rangeLabel: string;
  health24h: string;
  health6h: string;
  health7d: string;
  cost24h: string;
  cost7d: string;
  cost30d: string;
  probeTicks: string;
  rollupTicks: string;
  colProvider: string;
  colLabel: string;
  colChecks: string;
  colSuccessRate: string;
  colP50: string;
  colP95: string;
  colMax: string;
  colLastCheck: string;
  colSparkline: string;
  costTotalsTitle: string;
  costDailyTitle: string;
  costCalls: string;
  costTokens: string;
  costUSD: string;
  costDays: string;
  errorPrefix: string;
  loading: string;
  empty: string;
  noPings: string;
  ok: string;
  fail: string;
}

const COPY: Record<Language, Copy> = {
  "zh-CN": {
    title: "Provider 看板 · 健康 & 成本",
    subtitle: "5 分钟探针 + 每小时成本汇总，看板数据由 probe 与 rollup 后台 loop 实时维护。",
    refresh: "刷新",
    healthHeader: "健康",
    costHeader: "成本",
    rangeLabel: "时窗",
    health24h: "24 小时",
    health6h: "6 小时",
    health7d: "7 天",
    cost24h: "24 小时",
    cost7d: "7 天",
    cost30d: "30 天",
    probeTicks: "探针累计次数",
    rollupTicks: "成本汇总累计次数",
    colProvider: "Provider",
    colLabel: "Label",
    colChecks: "采样",
    colSuccessRate: "成功率",
    colP50: "P50 延迟",
    colP95: "P95 延迟",
    colMax: "最大延迟",
    colLastCheck: "最近检查",
    colSparkline: "趋势",
    costTotalsTitle: "Provider 总额",
    costDailyTitle: "按天累计",
    costCalls: "调用",
    costTokens: "Token",
    costUSD: "USD",
    costDays: "天",
    errorPrefix: "加载失败",
    loading: "加载中…",
    empty: "暂无数据（探针还未跑过完整一轮）",
    noPings: "—",
    ok: "OK",
    fail: "异常",
  },
  "en-US": {
    title: "Provider Observability · Health & Cost",
    subtitle: "5-minute probe + hourly cost rollup. Data is maintained by the background loops in real time.",
    refresh: "Refresh",
    healthHeader: "Health",
    costHeader: "Cost",
    rangeLabel: "Window",
    health24h: "24h",
    health6h: "6h",
    health7d: "7d",
    cost24h: "24h",
    cost7d: "7d",
    cost30d: "30d",
    probeTicks: "Probe ticks since boot",
    rollupTicks: "Rollup ticks since boot",
    colProvider: "Provider",
    colLabel: "Label",
    colChecks: "Checks",
    colSuccessRate: "Success rate",
    colP50: "P50 latency",
    colP95: "P95 latency",
    colMax: "Max latency",
    colLastCheck: "Last check",
    colSparkline: "Trend",
    costTotalsTitle: "Per-provider totals",
    costDailyTitle: "Per-day",
    costCalls: "Calls",
    costTokens: "Tokens",
    costUSD: "USD",
    costDays: "days",
    errorPrefix: "Load failed",
    loading: "Loading…",
    empty: "No data yet (probe hasn't completed a full cycle)",
    noPings: "—",
    ok: "OK",
    fail: "Down",
  },
};

const HEALTH_RANGE = ["6h", "24h", "7d"] as const;
const COST_RANGE = ["24h", "7d", "30d"] as const;
type HealthRange = (typeof HEALTH_RANGE)[number];
type CostRange = (typeof COST_RANGE)[number];

function formatTimestamp(iso?: string): string {
  if (!iso) return "";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function fmtPct(v: number): string {
  if (!Number.isFinite(v)) return "—";
  return `${(v * 100).toFixed(1)}%`;
}

function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return "—";
  return v.toLocaleString();
}

function fmtUSD(v: number): string {
  if (!Number.isFinite(v)) return "—";
  return `$${v.toFixed(4)}`;
}

function SparklineCell({ row }: { row: ProviderHealthDashboardRow }) {
  // Transform sparkline points into a recharts-friendly shape with
  // dual encoding (latency line + ok/fail colour). We use 0 for
  // failed probes so the dip is visible on the chart.
  const data = useMemo(
    () =>
      row.sparkline.map((p, idx) => ({
        idx,
        latency: p.ok ? p.latency_ms : 0,
        ok: p.ok ? 1 : 0,
      })),
    [row.sparkline],
  );
  if (!data.length) {
    return <span className="text-xs text-zinc-500">—</span>;
  }
  return (
    <div className="h-8 w-32">
      <ResponsiveContainer width="100%" height="100%">
        <LineChart data={data} margin={{ left: 0, right: 0, top: 1, bottom: 1 }}>
          <Line
            type="monotone"
            dataKey="latency"
            stroke="#34d399"
            strokeWidth={1.5}
            dot={false}
            isAnimationActive={false}
          />
        </LineChart>
      </ResponsiveContainer>
    </div>
  );
}

export default function AdminLLMObservabilitySection({ language }: Props) {
  const t = COPY[language];

  const [healthRange, setHealthRange] = useState<HealthRange>("24h");
  const [costRange, setCostRange] = useState<CostRange>("7d");
  const [health, setHealth] = useState<ProviderHealthDashboardResponse | null>(null);
  const [cost, setCost] = useState<ProviderCostDashboardResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [h, c] = await Promise.all([
        getAdminProviderHealthDashboard(healthRange),
        getAdminProviderCostDashboard(costRange),
      ]);
      setHealth(h);
      setCost(c);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [healthRange, costRange, t.errorPrefix]);

  useEffect(() => {
    reload();
  }, [reload]);

  // Daily series → wide format keyed by provider for the area chart.
  const costDailyWide = useMemo(() => {
    if (!cost || !cost.daily.length) return [];
    const byDay = new Map<string, Record<string, number>>();
    for (const row of cost.daily) {
      const bucket = byDay.get(row.day) ?? {};
      bucket[row.provider] = (bucket[row.provider] ?? 0) + row.cost_usd;
      byDay.set(row.day, bucket);
    }
    return Array.from(byDay.entries())
      .sort(([a], [b]) => a.localeCompare(b))
      .map(([day, providers]) => ({ day, ...providers }));
  }, [cost]);

  const costProviders = useMemo(
    () => (cost ? Array.from(new Set(cost.daily.map((d) => d.provider))).sort() : []),
    [cost],
  );

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={reload}
            disabled={loading}
            className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
          >
            {t.refresh}
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 rounded-md border border-rose-700 bg-rose-900/30 p-3 text-sm text-rose-200">
          {error}
        </div>
      )}

      <div className="mt-4 flex flex-wrap items-center gap-3 text-xs text-zinc-400">
        <span>{t.probeTicks}: <code className="text-zinc-200">{health?.probe_ticks_since_boot ?? 0}</code></span>
        <span className="ml-4">{t.rollupTicks}: <code className="text-zinc-200">{cost?.rollup_ticks_since_boot ?? 0}</code></span>
      </div>

      {/* Health block */}
      <div className="mt-6">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-zinc-200">{t.healthHeader}</h3>
          <div className="flex items-center gap-2 text-xs">
            <span className="text-zinc-400">{t.rangeLabel}:</span>
            {HEALTH_RANGE.map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setHealthRange(r)}
                className={
                  "rounded border px-2 py-0.5 " +
                  (healthRange === r
                    ? "border-emerald-500/50 bg-emerald-500/10 text-emerald-200"
                    : "border-zinc-700 bg-zinc-800/40 text-zinc-300 hover:bg-zinc-800")
                }
              >
                {r === "6h" ? t.health6h : r === "24h" ? t.health24h : t.health7d}
              </button>
            ))}
          </div>
        </div>
        <div className="overflow-x-auto rounded-lg border border-zinc-700 bg-zinc-900/30">
          {loading && !health ? (
            <p className="p-4 text-sm text-zinc-400">{t.loading}</p>
          ) : !health || health.rows.length === 0 ? (
            <p className="p-4 text-sm text-zinc-400">{t.empty}</p>
          ) : (
            <table className="w-full text-xs text-zinc-200">
              <thead>
                <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                  <th className="px-2 py-1 text-left">{t.colProvider}</th>
                  <th className="px-2 py-1 text-left">{t.colLabel}</th>
                  <th className="px-2 py-1 text-right">{t.colChecks}</th>
                  <th className="px-2 py-1 text-right">{t.colSuccessRate}</th>
                  <th className="px-2 py-1 text-right">{t.colP50}</th>
                  <th className="px-2 py-1 text-right">{t.colP95}</th>
                  <th className="px-2 py-1 text-right">{t.colMax}</th>
                  <th className="px-2 py-1 text-left">{t.colLastCheck}</th>
                  <th className="px-2 py-1 text-left">{t.colSparkline}</th>
                </tr>
              </thead>
              <tbody>
                {health.rows.map((row) => {
                  const lastOK = row.last_ok;
                  const statusChip =
                    lastOK === undefined ? null : lastOK ? (
                      <span className="rounded border border-emerald-500/30 bg-emerald-500/10 px-1.5 py-0.5 text-[10px] uppercase text-emerald-200">
                        {t.ok}
                      </span>
                    ) : (
                      <span className="rounded border border-rose-500/30 bg-rose-500/10 px-1.5 py-0.5 text-[10px] uppercase text-rose-200">
                        {t.fail}
                      </span>
                    );
                  return (
                    <tr key={row.provider_id} className="border-b border-zinc-800/40 hover:bg-zinc-800/20">
                      <td className="px-2 py-1 font-medium uppercase">{row.provider}</td>
                      <td className="px-2 py-1">{row.label}</td>
                      <td className="px-2 py-1 text-right tabular-nums">{fmtNum(row.checks)}</td>
                      <td className="px-2 py-1 text-right tabular-nums">{fmtPct(row.success_rate)}</td>
                      <td className="px-2 py-1 text-right tabular-nums">{fmtNum(row.latency_p50_ms)} ms</td>
                      <td className="px-2 py-1 text-right tabular-nums">{fmtNum(row.latency_p95_ms)} ms</td>
                      <td className="px-2 py-1 text-right tabular-nums">{fmtNum(row.latency_max_ms)} ms</td>
                      <td className="px-2 py-1">
                        <div className="flex items-center gap-1">
                          {statusChip}
                          <span className="text-zinc-400">{formatTimestamp(row.last_checked_at)}</span>
                        </div>
                      </td>
                      <td className="px-2 py-1">
                        <SparklineCell row={row} />
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      </div>

      {/* Cost block */}
      <div className="mt-8">
        <div className="mb-2 flex items-center justify-between">
          <h3 className="text-sm font-semibold text-zinc-200">{t.costHeader}</h3>
          <div className="flex items-center gap-2 text-xs">
            <span className="text-zinc-400">{t.rangeLabel}:</span>
            {COST_RANGE.map((r) => (
              <button
                key={r}
                type="button"
                onClick={() => setCostRange(r)}
                className={
                  "rounded border px-2 py-0.5 " +
                  (costRange === r
                    ? "border-emerald-500/50 bg-emerald-500/10 text-emerald-200"
                    : "border-zinc-700 bg-zinc-800/40 text-zinc-300 hover:bg-zinc-800")
                }
              >
                {r === "24h" ? t.cost24h : r === "7d" ? t.cost7d : t.cost30d}
              </button>
            ))}
          </div>
        </div>

        <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
          {/* Totals */}
          <div className="lg:col-span-1 rounded-lg border border-zinc-700 bg-zinc-900/30 p-3">
            <h4 className="mb-2 text-xs font-semibold uppercase text-zinc-400">{t.costTotalsTitle}</h4>
            {!cost || cost.totals.length === 0 ? (
              <p className="text-xs text-zinc-500">{t.empty}</p>
            ) : (
              <table className="w-full text-xs text-zinc-200">
                <thead>
                  <tr className="border-b border-zinc-800 text-[10px] uppercase text-zinc-400">
                    <th className="py-1 text-left">{t.colProvider}</th>
                    <th className="py-1 text-right">{t.costUSD}</th>
                    <th className="py-1 text-right">{t.costCalls}</th>
                    <th className="py-1 text-right">{t.costTokens}</th>
                    <th className="py-1 text-right">{t.costDays}</th>
                  </tr>
                </thead>
                <tbody>
                  {cost.totals.map((row) => (
                    <tr key={row.provider} className="border-b border-zinc-800/40">
                      <td className="py-1 font-medium uppercase">{row.provider}</td>
                      <td className="py-1 text-right tabular-nums">{fmtUSD(row.cost_usd)}</td>
                      <td className="py-1 text-right tabular-nums">{fmtNum(row.calls)}</td>
                      <td className="py-1 text-right tabular-nums">{fmtNum(row.total_tokens)}</td>
                      <td className="py-1 text-right tabular-nums">{row.days_in_window}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </div>

          {/* Per-day stacked area chart */}
          <div className="lg:col-span-2 rounded-lg border border-zinc-700 bg-zinc-900/30 p-3">
            <h4 className="mb-2 text-xs font-semibold uppercase text-zinc-400">{t.costDailyTitle}</h4>
            {costDailyWide.length === 0 ? (
              <p className="text-xs text-zinc-500">{t.empty}</p>
            ) : (
              <div className="h-56">
                <ResponsiveContainer width="100%" height="100%">
                  <AreaChart data={costDailyWide} margin={{ left: 0, right: 8, top: 8, bottom: 0 }}>
                    <CartesianGrid stroke="#3f3f46" strokeDasharray="3 3" />
                    <XAxis dataKey="day" stroke="#a1a1aa" fontSize={10} />
                    <YAxis stroke="#a1a1aa" fontSize={10} tickFormatter={(v: number) => `$${v.toFixed(2)}`} />
                    <Tooltip
                      contentStyle={{ background: "#18181b", border: "1px solid #3f3f46" }}
                      formatter={(v: number) => `$${v.toFixed(4)}`}
                    />
                    <Legend wrapperStyle={{ fontSize: 10 }} />
                    {costProviders.map((p, idx) => (
                      <Area
                        key={p}
                        type="monotone"
                        dataKey={p}
                        stackId="usd"
                        stroke={STACK_COLORS[idx % STACK_COLORS.length]}
                        fill={STACK_COLORS[idx % STACK_COLORS.length]}
                        fillOpacity={0.35}
                      />
                    ))}
                  </AreaChart>
                </ResponsiveContainer>
              </div>
            )}
          </div>
        </div>
      </div>
    </section>
  );
}

const STACK_COLORS = [
  "#34d399",
  "#60a5fa",
  "#f472b6",
  "#fbbf24",
  "#a78bfa",
  "#f87171",
];
