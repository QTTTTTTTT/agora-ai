// AdminAgentReputationSection — cross-fund admin view +
// rebuild trigger for the S8.4 agent reputation ledger.
//
// Operator filters by fund_id + role, sees the rolling stats
// table ordered by avg α. The rebuild button kicks one
// synchronous backfill wave (all funds or just one) — useful
// when ops just wired a new realised-return source and wants
// the table refreshed without waiting for the nightly loop.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  formatApiError,
  listAdminAgentReputationStats,
  rebuildAgentReputation,
  type AgentReputationKind,
  type AgentReputationStats,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  fundFilter: string;
  fundFilterPlaceholder: string;
  kindFilter: string;
  kindAll: string;
  kindAnalyst: string;
  kindAdvocate: string;
  kindPM: string;
  kindResearcher: string;
  rebuildAllButton: string;
  rebuildFundButton: string;
  rebuildRunning: string;
  rebuildSuccess: string;
  rebuildError: string;
  colFund: string;
  colAgent: string;
  colKind: string;
  colCategory: string;
  colDecisions: string;
  colHitRate: string;
  colAvgAlpha: string;
  colAvgConfidence: string;
  colLast: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "智能体声望榜（全部基金）",
    panelSubtitle:
      "Cross-fund 视角：所有基金所有 agent 的 rolling avg α。可按 fund / 角色过滤。后台 backfill 每 24 小时跑一轮；如刚配好 price feed 想立刻看效果，可用下方按钮强制重算。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无声望数据",
    error: "加载失败",
    fundFilter: "Fund ID",
    fundFilterPlaceholder: "留空 = 全部基金",
    kindFilter: "角色",
    kindAll: "全部",
    kindAnalyst: "分析师",
    kindAdvocate: "多空研究员",
    kindPM: "PM",
    kindResearcher: "通用研究员",
    rebuildAllButton: "重算全部基金声望",
    rebuildFundButton: "重算本基金声望",
    rebuildRunning: "正在重算…",
    rebuildSuccess: "完成：写入 {n} 条结算",
    rebuildError: "重算失败",
    colFund: "基金",
    colAgent: "智能体",
    colKind: "角色",
    colCategory: "类别",
    colDecisions: "决策次数",
    colHitRate: "命中率",
    colAvgAlpha: "平均 α",
    colAvgConfidence: "平均置信度",
    colLast: "最近一次发声",
  },
  "en-US": {
    panelTitle: "Agent reputation ledger (cross-fund)",
    panelSubtitle:
      "Operator view: rolling per-agent avg α across every fund. Filter by fund and role. The backfill loop runs every 24h; use the buttons below to force a rebuild after wiring a new realised-return source.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No reputation data yet",
    error: "Failed to load",
    fundFilter: "Fund ID",
    fundFilterPlaceholder: "leave blank for all funds",
    kindFilter: "Role",
    kindAll: "All",
    kindAnalyst: "Analyst",
    kindAdvocate: "Advocate",
    kindPM: "PM",
    kindResearcher: "Researcher",
    rebuildAllButton: "Rebuild all funds",
    rebuildFundButton: "Rebuild this fund",
    rebuildRunning: "Rebuilding…",
    rebuildSuccess: "Done: wrote {n} outcomes",
    rebuildError: "Rebuild failed",
    colFund: "Fund",
    colAgent: "Agent",
    colKind: "Role",
    colCategory: "Category",
    colDecisions: "Decisions",
    colHitRate: "Hit rate",
    colAvgAlpha: "Avg α",
    colAvgConfidence: "Avg confidence",
    colLast: "Last call",
  },
};

const KIND_OPTIONS: AgentReputationKind[] = ["analyst", "advocate", "pm", "researcher"];

interface AdminAgentReputationSectionProps {
  language?: Language;
}

function kindLabel(m: Messages, k: AgentReputationKind): string {
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

export default function AdminAgentReputationSection({
  language = "zh-CN",
}: AdminAgentReputationSectionProps) {
  const m = useMemo(() => messages[language], [language]);
  const [fundId, setFundId] = useState("");
  const [kind, setKind] = useState<AgentReputationKind | "">("");
  const [stats, setStats] = useState<AgentReputationStats[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [rebuilding, setRebuilding] = useState(false);
  const [rebuildStatus, setRebuildStatus] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminAgentReputationStats({
        fundId: fundId.trim() || undefined,
        kind: kind || undefined,
        limit: 200,
      });
      setStats(resp.stats ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [fundId, kind, m.error]);

  useEffect(() => {
    load().catch(() => {});
  }, [load]);

  const handleRebuild = useCallback(
    async (scope: "all" | "fund") => {
      setRebuilding(true);
      setRebuildStatus(null);
      try {
        const resp = await rebuildAgentReputation(
          scope === "fund" && fundId.trim() ? { fund_id: fundId.trim() } : {},
        );
        setRebuildStatus(interpolate(m.rebuildSuccess, { n: resp.outcomes_written }));
        load().catch(() => {});
      } catch (err) {
        setRebuildStatus(formatApiError(err, m.rebuildError));
      } finally {
        setRebuilding(false);
      }
    },
    [fundId, m.rebuildSuccess, m.rebuildError, load],
  );

  return (
    <section className="space-y-4 rounded-2xl border border-gray-100 bg-white p-6 shadow-sm">
      <header className="space-y-1">
        <h2 className="text-lg font-semibold text-gray-900">{m.panelTitle}</h2>
        <p className="text-sm text-gray-500">{m.panelSubtitle}</p>
      </header>

      <div className="flex flex-wrap items-end gap-3">
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">{m.fundFilter}</span>
          <input
            value={fundId}
            onChange={(e) => setFundId(e.target.value)}
            placeholder={m.fundFilterPlaceholder}
            className="w-72 rounded-lg border border-gray-200 px-3 py-2"
          />
        </label>
        <label className="flex flex-col gap-1 text-sm">
          <span className="text-gray-700">{m.kindFilter}</span>
          <select
            value={kind}
            onChange={(e) => setKind(e.target.value as AgentReputationKind | "")}
            className="rounded-lg border border-gray-200 px-3 py-2"
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
          className="rounded-lg border border-gray-200 bg-white px-3 py-2 text-sm text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {loading ? m.loading : m.refresh}
        </button>
        <button
          type="button"
          onClick={() => handleRebuild("all")}
          disabled={rebuilding}
          className="rounded-lg bg-indigo-600 px-3 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:bg-indigo-300"
        >
          {rebuilding ? m.rebuildRunning : m.rebuildAllButton}
        </button>
        <button
          type="button"
          onClick={() => handleRebuild("fund")}
          disabled={rebuilding || !fundId.trim()}
          className="rounded-lg border border-indigo-600 bg-white px-3 py-2 text-sm font-medium text-indigo-700 hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-50"
        >
          {rebuilding ? m.rebuildRunning : m.rebuildFundButton}
        </button>
      </div>

      {rebuildStatus ? (
        <div className="rounded-lg border border-gray-200 bg-gray-50 px-4 py-2 text-sm text-gray-700">
          {rebuildStatus}
        </div>
      ) : null}

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
                <th className="px-3 py-2 font-medium">{m.colFund}</th>
                <th className="px-3 py-2 font-medium">{m.colAgent}</th>
                <th className="px-3 py-2 font-medium">{m.colKind}</th>
                <th className="px-3 py-2 font-medium">{m.colCategory}</th>
                <th className="px-3 py-2 text-right font-medium">{m.colDecisions}</th>
                <th className="px-3 py-2 text-right font-medium">{m.colHitRate}</th>
                <th className="px-3 py-2 text-right font-medium">{m.colAvgAlpha}</th>
                <th className="px-3 py-2 text-right font-medium">{m.colAvgConfidence}</th>
                <th className="px-3 py-2 font-medium">{m.colLast}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100">
              {stats.map((s) => (
                <tr key={`${s.fund_id}-${s.agent_id}`} className="hover:bg-gray-50">
                  <td className="px-3 py-2 font-mono text-xs text-gray-700">{s.fund_id}</td>
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
                  <td className="px-3 py-2 text-right text-gray-700">{s.avg_confidence.toFixed(0)}%</td>
                  <td className="px-3 py-2 text-xs text-gray-500">{formatTime(s.last_decision_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}
