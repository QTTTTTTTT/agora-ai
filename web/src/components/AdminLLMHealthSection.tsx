// AdminLLMHealthSection — Sprint 11.4 admin dashboard for the
// decision_source / fallback_reason aggregates introduced by S11.1.
//
// Sections rendered:
//
//   1. Per-source counts within the selected window (24h default,
//      adjustable up to 30d).
//   2. Per-(category, provider) counts restricted to fallback_* rows.
//   3. Most recent fallback rows with the FULL raw provider summary
//      shown — this is the one and only user-visible surface where
//      Detail.Summary is exposed, and it is gated behind requireAdmin
//      on the server side (admin_llm_health.go).
//
// Caveats:
//   - The fallback ratio in the header is computed client-side as
//     `sum(fallback_*) / sum(all)`. The SRE alert query reads
//     fundai_pm_decision_total directly from Prometheus rather than
//     this admin endpoint so the alert path stays operational even
//     when the admin API is down.
//   - No write paths — this section is observation-only.

import { useCallback, useEffect, useMemo, useState } from "react";
import { Link } from "react-router-dom";

import {
  fetchLLMHealthRecentFallbacks,
  fetchLLMHealthSummary,
  formatApiError,
  type LLMHealthRecentFallback,
  type LLMHealthSummary,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  windowLabel: string;
  loadingText: string;
  errorPrefix: string;
  emptyMessage: string;
  fallbackRateLabel: string;
  totalPlansLabel: string;
  sourceTableTitle: string;
  sourceColSource: string;
  sourceColCount: string;
  categoryTableTitle: string;
  categoryColCategory: string;
  categoryColProvider: string;
  categoryColCount: string;
  recentTableTitle: string;
  recentColPlan: string;
  recentColFund: string;
  recentColSource: string;
  recentColCategory: string;
  recentColProvider: string;
  recentColModel: string;
  recentColSummary: string;
  recentColAt: string;
  refreshLabel: string;
  windowOptions: { label: string; value: number }[];
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "LLM 健康面板",
    subtitle: "近窗口内 PM 计划按决策来源 / 失败类别的聚合。仅管理员可见。",
    windowLabel: "时间窗口",
    loadingText: "加载中…",
    errorPrefix: "加载失败",
    emptyMessage: "当前窗口暂无数据。",
    fallbackRateLabel: "兜底占比",
    totalPlansLabel: "总计划数",
    sourceTableTitle: "按来源",
    sourceColSource: "决策来源",
    sourceColCount: "数量",
    categoryTableTitle: "按失败类别",
    categoryColCategory: "类别",
    categoryColProvider: "供应商",
    categoryColCount: "数量",
    recentTableTitle: "最近兜底",
    recentColPlan: "计划",
    recentColFund: "基金",
    recentColSource: "来源",
    recentColCategory: "类别",
    recentColProvider: "供应商",
    recentColModel: "模型",
    recentColSummary: "原始错误摘要",
    recentColAt: "时间",
    refreshLabel: "刷新",
    windowOptions: [
      { label: "1 小时", value: 1 },
      { label: "6 小时", value: 6 },
      { label: "24 小时", value: 24 },
      { label: "7 天", value: 24 * 7 },
      { label: "30 天", value: 24 * 30 },
    ],
  },
  "en-US": {
    title: "LLM health dashboard",
    subtitle:
      "Aggregates of PM plans by decision source and fallback category in the selected window. Admin only.",
    windowLabel: "Time window",
    loadingText: "Loading…",
    errorPrefix: "Load failed",
    emptyMessage: "No data in the selected window.",
    fallbackRateLabel: "Fallback share",
    totalPlansLabel: "Total plans",
    sourceTableTitle: "By source",
    sourceColSource: "Decision source",
    sourceColCount: "Count",
    categoryTableTitle: "By failure category",
    categoryColCategory: "Category",
    categoryColProvider: "Provider",
    categoryColCount: "Count",
    recentTableTitle: "Recent fallbacks",
    recentColPlan: "Plan",
    recentColFund: "Fund",
    recentColSource: "Source",
    recentColCategory: "Category",
    recentColProvider: "Provider",
    recentColModel: "Model",
    recentColSummary: "Raw provider summary",
    recentColAt: "At",
    refreshLabel: "Refresh",
    windowOptions: [
      { label: "1 hour", value: 1 },
      { label: "6 hours", value: 6 },
      { label: "24 hours", value: 24 },
      { label: "7 days", value: 24 * 7 },
      { label: "30 days", value: 24 * 30 },
    ],
  },
};

function fallbackShare(summary: LLMHealthSummary | null): number {
  if (!summary) return 0;
  const total = summary.sources.reduce((acc, row) => acc + row.count, 0);
  if (total === 0) return 0;
  const fallback = summary.sources
    .filter((row) => row.source.startsWith("fallback_"))
    .reduce((acc, row) => acc + row.count, 0);
  return fallback / total;
}

export function AdminLLMHealthSection({ language }: Props) {
  const t = messages[language];
  const [windowHours, setWindowHours] = useState(24);
  const [summary, setSummary] = useState<LLMHealthSummary | null>(null);
  const [recent, setRecent] = useState<LLMHealthRecentFallback[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [sumResp, recentResp] = await Promise.all([
        fetchLLMHealthSummary(windowHours),
        fetchLLMHealthRecentFallbacks(windowHours, 50),
      ]);
      setSummary(sumResp);
      setRecent(recentResp.items ?? []);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [windowHours, t.errorPrefix]);

  useEffect(() => {
    reload();
  }, [reload]);

  const fallbackPct = useMemo(() => fallbackShare(summary), [summary]);
  const total = summary
    ? summary.sources.reduce((acc, row) => acc + row.count, 0)
    : 0;

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <div className="flex items-center gap-3 text-sm text-zinc-300">
          <label className="flex items-center gap-2">
            {t.windowLabel}
            <select
              value={windowHours}
              onChange={(e) => setWindowHours(Number(e.target.value))}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
            >
              {t.windowOptions.map((opt) => (
                <option key={opt.value} value={opt.value}>
                  {opt.label}
                </option>
              ))}
            </select>
          </label>
          <button
            type="button"
            onClick={reload}
            disabled={loading}
            className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
          >
            {t.refreshLabel}
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 rounded-md border border-rose-700 bg-rose-900/30 p-3 text-sm text-rose-200">
          {t.errorPrefix}: {error}
        </div>
      )}

      <div className="mt-6 grid grid-cols-1 gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.totalPlansLabel}</div>
          <div className="mt-1 text-2xl font-semibold text-zinc-100">{total}</div>
        </div>
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.fallbackRateLabel}</div>
          <div className={`mt-1 text-2xl font-semibold ${fallbackPct > 0.05 ? "text-amber-300" : "text-emerald-300"}`}>
            {(fallbackPct * 100).toFixed(1)}%
          </div>
        </div>
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.windowLabel}</div>
          <div className="mt-1 text-2xl font-semibold text-zinc-100">{windowHours}h</div>
        </div>
      </div>

      <div className="mt-6 grid grid-cols-1 gap-6 md:grid-cols-2">
        <div className="rounded-lg border border-zinc-700 bg-zinc-900/30 p-4">
          <h3 className="text-sm font-semibold text-zinc-100">{t.sourceTableTitle}</h3>
          {loading ? (
            <p className="mt-3 text-sm text-zinc-400">{t.loadingText}</p>
          ) : !summary || summary.sources.length === 0 ? (
            <p className="mt-3 text-sm text-zinc-400">{t.emptyMessage}</p>
          ) : (
            <table className="mt-3 w-full text-sm text-zinc-200">
              <thead>
                <tr className="border-b border-zinc-700 text-xs uppercase text-zinc-400">
                  <th className="py-1 text-left">{t.sourceColSource}</th>
                  <th className="py-1 text-right">{t.sourceColCount}</th>
                </tr>
              </thead>
              <tbody>
                {summary.sources.map((row) => (
                  <tr key={row.source} className="border-b border-zinc-800/40">
                    <td className="py-1">{row.source}</td>
                    <td className="py-1 text-right tabular-nums">{row.count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>

        <div className="rounded-lg border border-zinc-700 bg-zinc-900/30 p-4">
          <h3 className="text-sm font-semibold text-zinc-100">{t.categoryTableTitle}</h3>
          {loading ? (
            <p className="mt-3 text-sm text-zinc-400">{t.loadingText}</p>
          ) : !summary || summary.categories.length === 0 ? (
            <p className="mt-3 text-sm text-zinc-400">{t.emptyMessage}</p>
          ) : (
            <table className="mt-3 w-full text-sm text-zinc-200">
              <thead>
                <tr className="border-b border-zinc-700 text-xs uppercase text-zinc-400">
                  <th className="py-1 text-left">{t.categoryColCategory}</th>
                  <th className="py-1 text-left">{t.categoryColProvider}</th>
                  <th className="py-1 text-right">{t.categoryColCount}</th>
                </tr>
              </thead>
              <tbody>
                {summary.categories.map((row, idx) => (
                  <tr key={`${row.category}-${row.provider ?? ""}-${idx}`} className="border-b border-zinc-800/40">
                    <td className="py-1">{row.category}</td>
                    <td className="py-1 text-zinc-400">{row.provider || "-"}</td>
                    <td className="py-1 text-right tabular-nums">{row.count}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </div>
      </div>

      <div className="mt-6 rounded-lg border border-zinc-700 bg-zinc-900/30 p-4">
        <h3 className="text-sm font-semibold text-zinc-100">{t.recentTableTitle}</h3>
        {loading ? (
          <p className="mt-3 text-sm text-zinc-400">{t.loadingText}</p>
        ) : recent.length === 0 ? (
          <p className="mt-3 text-sm text-zinc-400">{t.emptyMessage}</p>
        ) : (
          <div className="mt-3 overflow-x-auto">
            <table className="w-full text-xs text-zinc-200">
              <thead>
                <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                  <th className="px-2 py-1 text-left">{t.recentColAt}</th>
                  <th className="px-2 py-1 text-left">{t.recentColFund}</th>
                  <th className="px-2 py-1 text-left">{t.recentColPlan}</th>
                  <th className="px-2 py-1 text-left">{t.recentColSource}</th>
                  <th className="px-2 py-1 text-left">{t.recentColCategory}</th>
                  <th className="px-2 py-1 text-left">{t.recentColProvider}</th>
                  <th className="px-2 py-1 text-left">{t.recentColModel}</th>
                  <th className="px-2 py-1 text-left">{t.recentColSummary}</th>
                </tr>
              </thead>
              <tbody>
                {recent.map((row) => (
                  <tr key={row.plan_id} className="border-b border-zinc-800/40">
                    <td className="whitespace-nowrap px-2 py-1 text-zinc-400">
                      {row.created_at.slice(0, 19).replace("T", " ")}
                    </td>
                    <td className="whitespace-nowrap px-2 py-1">
                      {/* S12-alt — fund tag jumps to that fund's */}
                      {/* decision center; admin retains access via */}
                      {/* their existing permissions. */}
                      <Link
                        to={`/funds/${encodeURIComponent(row.fund_id)}/decisions`}
                        className="text-sky-300 underline-offset-2 hover:underline"
                        title={row.fund_id}
                      >
                        {row.fund_id.slice(0, 8)}
                      </Link>
                    </td>
                    <td className="whitespace-nowrap px-2 py-1">
                      {/* S12-alt — plan tag deep-links straight to */}
                      {/* the offending plan in the decision center. */}
                      <Link
                        to={`/funds/${encodeURIComponent(row.fund_id)}/decisions?planId=${encodeURIComponent(row.plan_id)}`}
                        className="text-sky-300 underline-offset-2 hover:underline"
                        title={row.plan_id}
                      >
                        {row.plan_id.slice(0, 8)}
                      </Link>
                    </td>
                    <td className="px-2 py-1 text-amber-300">{row.source}</td>
                    <td className="px-2 py-1">{row.category || "-"}</td>
                    <td className="px-2 py-1 text-zinc-400">{row.provider || "-"}</td>
                    <td className="px-2 py-1 text-zinc-400">{row.model || "-"}</td>
                    <td className="px-2 py-1 text-zinc-300">{row.summary || "-"}</td>
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
