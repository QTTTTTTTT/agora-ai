// AgentReputationSection — S8.4 per-fund agent reputation UI.
//
// Renders the rolling per-agent stats table (sorted by avg α)
// + a recent settled-outcomes list. The operator can filter by
// agent role (analyst / advocate / pm / researcher) and drill
// into the per-symbol forward-return outcomes feeding the
// summary. Read-only — the rebuild trigger lives in the Admin
// page.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listFundAgentReputationOutcomes,
  listFundAgentReputationStats,
  type AgentReputationKind,
  type AgentReputationOutcome,
  type AgentReputationStats,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface AgentReputationMessages {
  title: string;
  subtitle: string;
  loading: string;
  error: string;
  retry: string;
  empty: string;
  kindFilter: string;
  kindAll: string;
  kindAnalyst: string;
  kindAdvocate: string;
  kindPM: string;
  kindResearcher: string;
  agentColumn: string;
  kindColumn: string;
  categoryColumn: string;
  decisionsColumn: string;
  hitRateColumn: string;
  avgAlphaColumn: string;
  avgConfidenceColumn: string;
  lastDecisionColumn: string;
  outcomesTitle: string;
  outcomesEmpty: string;
  outcomeSymbol: string;
  outcomeHorizon: string;
  outcomeDirection: string;
  outcomeAlpha: string;
  outcomeRealised: string;
  outcomeBenchmark: string;
  outcomeAsOf: string;
  horizonDays: string;
}

const messages: Record<Language, AgentReputationMessages> = {
  "zh-CN": {
    title: "智能体声望榜",
    subtitle:
      "每位分析师 / 多空研究员 / PM 在历史上对每只股票的看多看空判断，是否真的跑赢了基准？后台 backfill 把每次发声折算成 1d / 5d / 21d 的 realised alpha；下表按平均 α 从高到低排序，可作为 PM 后续加权与奖惩的依据。",
    loading: "加载中…",
    error: "加载失败",
    retry: "重试",
    empty: "暂无声望数据",
    kindFilter: "角色",
    kindAll: "全部",
    kindAnalyst: "分析师",
    kindAdvocate: "多空研究员",
    kindPM: "PM",
    kindResearcher: "通用研究员",
    agentColumn: "智能体",
    kindColumn: "角色",
    categoryColumn: "类别",
    decisionsColumn: "决策次数",
    hitRateColumn: "命中率",
    avgAlphaColumn: "平均 α",
    avgConfidenceColumn: "平均置信度",
    lastDecisionColumn: "最近一次发声",
    outcomesTitle: "近期单笔结算",
    outcomesEmpty: "暂无单笔结算记录",
    outcomeSymbol: "标的",
    outcomeHorizon: "持仓窗口",
    outcomeDirection: "方向",
    outcomeAlpha: "Alpha",
    outcomeRealised: "实际收益",
    outcomeBenchmark: "基准收益",
    outcomeAsOf: "发声日期",
    horizonDays: "{n} 天",
  },
  "en-US": {
    title: "Agent reputation ledger",
    subtitle:
      "Did each analyst / advocate / PM call actually beat the benchmark over time? A nightly backfill turns every panel + debate into 1d / 5d / 21d realised-alpha outcomes. Sorted by average α; the PM uses this signal to up- or down-weight each agent going forward.",
    loading: "Loading…",
    error: "Failed to load",
    retry: "Retry",
    empty: "No reputation data yet",
    kindFilter: "Role",
    kindAll: "All",
    kindAnalyst: "Analyst",
    kindAdvocate: "Advocate",
    kindPM: "PM",
    kindResearcher: "Researcher",
    agentColumn: "Agent",
    kindColumn: "Role",
    categoryColumn: "Category",
    decisionsColumn: "Decisions",
    hitRateColumn: "Hit rate",
    avgAlphaColumn: "Avg α",
    avgConfidenceColumn: "Avg confidence",
    lastDecisionColumn: "Last call",
    outcomesTitle: "Recent settled outcomes",
    outcomesEmpty: "No settled outcomes yet",
    outcomeSymbol: "Symbol",
    outcomeHorizon: "Horizon",
    outcomeDirection: "Direction",
    outcomeAlpha: "Alpha",
    outcomeRealised: "Realised return",
    outcomeBenchmark: "Benchmark return",
    outcomeAsOf: "Call date",
    horizonDays: "{n}d",
  },
};

interface AgentReputationSectionProps {
  fundId?: string;
  language?: Language;
}

const KIND_OPTIONS: AgentReputationKind[] = ["analyst", "advocate", "pm", "researcher"];

function kindLabel(m: AgentReputationMessages, k: AgentReputationKind): string {
  switch (k) {
    case "analyst":
      return m.kindAnalyst;
    case "advocate":
      return m.kindAdvocate;
    case "pm":
      return m.kindPM;
    case "researcher":
      return m.kindResearcher;
    default:
      return String(k);
  }
}

function alphaTone(alpha: number): string {
  if (alpha > 0) return "text-emerald-700";
  if (alpha < 0) return "text-rose-700";
  return "text-gray-600";
}

function directionTone(direction: string): string {
  switch (direction) {
    case "bullish":
      return "bg-emerald-50 text-emerald-700 border-emerald-200";
    case "bearish":
      return "bg-rose-50 text-rose-700 border-rose-200";
    default:
      return "bg-gray-50 text-gray-700 border-gray-200";
  }
}

function formatPercent(value: number, digits = 2): string {
  return `${(value * 100).toFixed(digits)}%`;
}

function formatTime(value?: string): string {
  if (!value) return "";
  try {
    const d = new Date(value);
    if (Number.isNaN(d.getTime())) return value;
    return d.toLocaleDateString();
  } catch {
    return value;
  }
}

function interpolate(template: string, vars: Record<string, string | number>): string {
  return template.replace(/\{(\w+)\}/g, (_, k) => String(vars[k] ?? ""));
}

export default function AgentReputationSection({
  fundId,
  language = "zh-CN",
}: AgentReputationSectionProps) {
  const m = useMemo(() => messages[language], [language]);
  const [kind, setKind] = useState<AgentReputationKind | "">("");
  const [stats, setStats] = useState<AgentReputationStats[]>([]);
  const [outcomes, setOutcomes] = useState<AgentReputationOutcome[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    if (!fundId) return;
    setLoading(true);
    setError(null);
    try {
      const [s, o] = await Promise.all([
        listFundAgentReputationStats(fundId, { kind: kind || undefined, limit: 100 }),
        listFundAgentReputationOutcomes(fundId, { limit: 50 }),
      ]);
      setStats(s.stats ?? []);
      setOutcomes(o.outcomes ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [fundId, kind, m.error]);

  useEffect(() => {
    load().catch(() => {});
  }, [load]);

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="space-y-1">
        <h2 className="text-lg font-semibold text-gray-900">{m.title}</h2>
        <p className="text-sm text-gray-500">{m.subtitle}</p>
      </header>

      <div className="flex flex-wrap items-center gap-3">
        <label className="flex items-center gap-2 text-sm">
          <span className="text-gray-700">{m.kindFilter}</span>
          <select
            value={kind}
            onChange={(e) => setKind(e.target.value as AgentReputationKind | "")}
            className="rounded-lg border border-gray-200 px-3 py-1.5 text-sm"
          >
            <option value="">{m.kindAll}</option>
            {KIND_OPTIONS.map((k) => (
              <option key={k} value={k}>
                {kindLabel(m, k)}
              </option>
            ))}
          </select>
        </label>
        <button
          type="button"
          onClick={() => load().catch(() => {})}
          disabled={loading}
          className="rounded-lg border border-gray-200 bg-white px-3 py-1.5 text-sm text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {loading ? m.loading : m.retry}
        </button>
      </div>

      {error ? (
        <div className="rounded-lg border border-rose-200 bg-rose-50 px-4 py-3 text-sm text-rose-700">
          {error}
        </div>
      ) : null}

      {stats.length === 0 && !loading ? (
        <p className="text-sm text-gray-500">{m.empty}</p>
      ) : (
        <div className="overflow-x-auto">
          <table className="min-w-full divide-y divide-gray-100 text-sm">
            <thead>
              <tr className="text-left text-xs uppercase tracking-wide text-gray-500">
                <th className="px-3 py-2 font-medium">{m.agentColumn}</th>
                <th className="px-3 py-2 font-medium">{m.kindColumn}</th>
                <th className="px-3 py-2 font-medium">{m.categoryColumn}</th>
                <th className="px-3 py-2 text-right font-medium">{m.decisionsColumn}</th>
                <th className="px-3 py-2 text-right font-medium">{m.hitRateColumn}</th>
                <th className="px-3 py-2 text-right font-medium">{m.avgAlphaColumn}</th>
                <th className="px-3 py-2 text-right font-medium">{m.avgConfidenceColumn}</th>
                <th className="px-3 py-2 font-medium">{m.lastDecisionColumn}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {stats.map((s) => (
                <tr key={`${s.fund_id}-${s.agent_id}`} className="hover:bg-gray-50">
                  <td className="px-3 py-2">
                    <div className="font-medium text-gray-900">{s.agent_name || s.agent_id}</div>
                    <div className="text-xs text-gray-500">{s.agent_id}</div>
                  </td>
                  <td className="px-3 py-2 text-gray-700">{kindLabel(m, s.agent_kind)}</td>
                  <td className="px-3 py-2 text-gray-700">{s.category || "—"}</td>
                  <td className="px-3 py-2 text-right text-gray-900">{s.decisions_count}</td>
                  <td className="px-3 py-2 text-right text-gray-900">{formatPercent(s.hit_rate)}</td>
                  <td className={`px-3 py-2 text-right font-medium ${alphaTone(s.avg_alpha)}`}>
                    {formatPercent(s.avg_alpha)}
                  </td>
                  <td className="px-3 py-2 text-right text-gray-700">
                    {s.avg_confidence.toFixed(0)}%
                  </td>
                  <td className="px-3 py-2 text-xs text-gray-500">{formatTime(s.last_decision_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div>
        <h3 className="mb-2 text-sm font-semibold text-gray-900">{m.outcomesTitle}</h3>
        {outcomes.length === 0 && !loading ? (
          <p className="text-sm text-gray-500">{m.outcomesEmpty}</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-gray-100 text-sm">
              <thead>
                <tr className="text-left text-xs uppercase tracking-wide text-gray-500">
                  <th className="px-3 py-2 font-medium">{m.outcomeSymbol}</th>
                  <th className="px-3 py-2 font-medium">{m.agentColumn}</th>
                  <th className="px-3 py-2 font-medium">{m.outcomeDirection}</th>
                  <th className="px-3 py-2 font-medium">{m.outcomeHorizon}</th>
                  <th className="px-3 py-2 text-right font-medium">{m.outcomeRealised}</th>
                  <th className="px-3 py-2 text-right font-medium">{m.outcomeBenchmark}</th>
                  <th className="px-3 py-2 text-right font-medium">{m.outcomeAlpha}</th>
                  <th className="px-3 py-2 font-medium">{m.outcomeAsOf}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100">
                {outcomes.map((o) => (
                  <tr key={o.id} className="hover:bg-gray-50">
                    <td className="px-3 py-2 font-medium text-gray-900">{o.symbol}</td>
                    <td className="px-3 py-2 text-gray-700">{o.agent_name || o.agent_id}</td>
                    <td className="px-3 py-2">
                      <span className={`rounded-full border px-2 py-0.5 text-xs ${directionTone(o.direction)}`}>
                        {o.direction}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-gray-700">
                      {interpolate(m.horizonDays, { n: o.horizon_days })}
                    </td>
                    <td className="px-3 py-2 text-right text-gray-700">{formatPercent(o.realised_return)}</td>
                    <td className="px-3 py-2 text-right text-gray-700">{formatPercent(o.benchmark_return)}</td>
                    <td className={`px-3 py-2 text-right font-medium ${alphaTone(o.alpha)}`}>
                      {formatPercent(o.alpha)}
                    </td>
                    <td className="px-3 py-2 text-xs text-gray-500">{formatTime(o.asof)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
}
