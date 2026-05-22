import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  AttributionLesson,
  AttributionResponse,
  AttributionSleeveRegimeStat,
  fetchStrategyAttribution,
  formatApiError,
  refreshStrategyAttribution,
} from "../lib/api";
import { formatDateTimeForLanguage, formatNumberForLanguage, type AppLanguage } from "../lib/preferences";

interface PanelProps {
  fundId?: string;
  language: AppLanguage;
}

interface PanelCopy {
  title: string;
  subtitle: string;
  refresh: string;
  refreshing: string;
  reload: string;
  loading: string;
  emptyStats: string;
  emptyStatsHint: string;
  emptyLessons: string;
  emptyLessonsHint: string;
  unavailableTitle: string;
  unavailableHint: string;
  errorPrefix: string;
  windowLabel: string;
  generatedAtLabel: string;
  bySleeveRegimeHeader: string;
  sleeveCol: string;
  regimeCol: string;
  tradesCol: string;
  winRateCol: string;
  pnlCol: string;
  avgHoldCol: string;
  recentLessons: string;
  severityLabel: Record<string, string>;
  kindLabel: Record<string, string>;
  unspecified: string;
}

const COPY: Record<AppLanguage, PanelCopy> = {
  "zh-CN": {
    title: "策略归因学习 / Strategy Attribution",
    subtitle:
      "归因 Agent 每日复盘后按 sleeve × regime 给已闭合的交易打分；当胜率长期低于 35% 会落入 loser lesson 并自动 mute。即使尚无成交闭环，也会告诉你当前在观察哪些仓位。",
    refresh: "立即重跑归因",
    refreshing: "重跑中…",
    reload: "刷新",
    loading: "加载中…",
    emptyStats: "暂无 sleeve × regime 数据",
    emptyStatsHint: "等首次卖出闭合一个 lot，归因表就会出现。",
    emptyLessons: "暂无学习记录",
    emptyLessonsHint: "归因 Agent 还没有可写的 lesson；点击右上角“立即重跑”可以触发一次写入。",
    unavailableTitle: "归因服务未配置",
    unavailableHint: "服务端没有挂载 attribution.Service（构建过老或 DB 未就绪）。",
    errorPrefix: "加载失败：",
    windowLabel: "回看窗口",
    generatedAtLabel: "数据时间",
    bySleeveRegimeHeader: "Sleeve × Regime 表现",
    sleeveCol: "Sleeve",
    regimeCol: "Regime",
    tradesCol: "笔数",
    winRateCol: "胜率",
    pnlCol: "累计 P&L",
    avgHoldCol: "平均持仓 (天)",
    recentLessons: "最近 lesson",
    severityLabel: {
      info: "Info",
      warning: "Warning",
      critical: "Critical",
    },
    kindLabel: {
      sleeve_regime_loser: "亏损警告",
      sleeve_regime_winner: "盈利洞察",
      insufficient_data: "数据不足",
    },
    unspecified: "(未指定)",
  },
  "en-US": {
    title: "Strategy attribution",
    subtitle:
      "The attribution agent scores closed lots after each daily review, bucketed by sleeve × regime. Combinations that drop below 35% win-rate become loser lessons and auto-mute. Even when no roundtrip has closed yet, the agent tells you which positions it is currently watching.",
    refresh: "Run attribution now",
    refreshing: "Running…",
    reload: "Reload",
    loading: "Loading…",
    emptyStats: "No sleeve × regime data yet",
    emptyStatsHint: "Numbers will appear once the first sell closes a lot.",
    emptyLessons: "No learning records yet",
    emptyLessonsHint: "Click 'Run attribution now' to force a write — even an 'insufficient data' lesson is proof the agent is active.",
    unavailableTitle: "Attribution service not configured",
    unavailableHint: "The server hasn't wired attribution.Service (older build or DB not ready).",
    errorPrefix: "Load failed: ",
    windowLabel: "Lookback",
    generatedAtLabel: "Generated",
    bySleeveRegimeHeader: "Sleeve × regime performance",
    sleeveCol: "Sleeve",
    regimeCol: "Regime",
    tradesCol: "Trades",
    winRateCol: "Win rate",
    pnlCol: "Total P&L",
    avgHoldCol: "Avg hold (days)",
    recentLessons: "Recent lessons",
    severityLabel: {
      info: "Info",
      warning: "Warning",
      critical: "Critical",
    },
    kindLabel: {
      sleeve_regime_loser: "Loser",
      sleeve_regime_winner: "Winner",
      insufficient_data: "No data",
    },
    unspecified: "(unspecified)",
  },
};

const SEVERITY_STYLES: Record<string, string> = {
  critical: "bg-red-100 text-red-700 border-red-200",
  warning: "bg-amber-100 text-amber-700 border-amber-200",
  info: "bg-indigo-100 text-indigo-700 border-indigo-200",
};

function formatPercent(value: number, language: AppLanguage): string {
  return `${formatNumberForLanguage(value * 100, language, { maximumFractionDigits: 1 })}%`;
}

function formatPnL(value: number, language: AppLanguage): string {
  const formatted = formatNumberForLanguage(value, language, { maximumFractionDigits: 2 });
  return value > 0 ? `+${formatted}` : formatted;
}

function lessonHeadline(lesson: AttributionLesson, copy: PanelCopy): string {
  return copy.kindLabel[lesson.kind] ?? lesson.kind;
}

function isUnavailableError(message: string): boolean {
  // The backend maps the missing-service case to 503 with the
  // 'attribution_unavailable' code; surfacing that as a friendly
  // banner keeps the dashboard from screaming a generic error
  // in dev environments where the optional service is off.
  return message.toLowerCase().includes("attribution_unavailable");
}

const StrategyAttributionPanel: React.FC<PanelProps> = ({ fundId, language }) => {
  const copy = COPY[language];
  const [data, setData] = useState<AttributionResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [refreshing, setRefreshing] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [unavailable, setUnavailable] = useState(false);

  const load = useCallback(async () => {
    if (!fundId) {
      return;
    }
    setLoading(true);
    setError(null);
    setUnavailable(false);
    try {
      const resp = await fetchStrategyAttribution(fundId);
      setData(resp);
    } catch (err) {
      const message = formatApiError(err, copy.errorPrefix);
      if (isUnavailableError(message)) {
        setUnavailable(true);
      } else {
        setError(message);
      }
    } finally {
      setLoading(false);
    }
  }, [copy.errorPrefix, fundId]);

  useEffect(() => {
    void load();
  }, [load]);

  const runRefresh = useCallback(async () => {
    if (!fundId) {
      return;
    }
    setRefreshing(true);
    setError(null);
    try {
      const resp = await refreshStrategyAttribution(fundId);
      setData(resp);
    } catch (err) {
      const message = formatApiError(err, copy.errorPrefix);
      if (isUnavailableError(message)) {
        setUnavailable(true);
      } else {
        setError(message);
      }
    } finally {
      setRefreshing(false);
    }
  }, [copy.errorPrefix, fundId]);

  // Sort the cross-tab by tradesCount DESC so the most-active
  // combinations bubble to the top. Trades=0 rows still appear
  // below the active rows so the user can see "we observed this
  // sleeve in this regime once but it didn't close yet".
  const sortedSleeveRegime: AttributionSleeveRegimeStat[] = useMemo(() => {
    const rows = data?.bySleeveRegime ?? [];
    return [...rows].sort((a, b) => b.tradeCount - a.tradeCount || b.totalPnl - a.totalPnl);
  }, [data?.bySleeveRegime]);

  return (
    <section className="rounded-xl bg-white p-5 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-base font-semibold text-gray-900">{copy.title}</h2>
          <p className="mt-1 max-w-2xl text-xs text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="flex flex-col gap-2 sm:flex-row">
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading || !fundId}
            className="rounded-md border border-gray-200 px-3 py-1.5 text-xs font-semibold text-gray-700 hover:bg-gray-50 disabled:opacity-60"
          >
            {copy.reload}
          </button>
          <button
            type="button"
            onClick={() => void runRefresh()}
            disabled={refreshing || !fundId}
            className="rounded-md bg-indigo-600 px-3 py-1.5 text-xs font-semibold text-white hover:bg-indigo-700 disabled:opacity-60"
          >
            {refreshing ? copy.refreshing : copy.refresh}
          </button>
        </div>
      </div>

      {loading ? <p className="mt-4 text-sm text-gray-500">{copy.loading}</p> : null}

      {error ? (
        <div className="mt-4 rounded-md border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">
          {copy.errorPrefix}
          {error}
        </div>
      ) : null}

      {unavailable ? (
        <div className="mt-4 rounded-md border border-amber-200 bg-amber-50 px-3 py-2 text-sm text-amber-700">
          <strong>{copy.unavailableTitle}.</strong> {copy.unavailableHint}
        </div>
      ) : null}

      {data && !loading && !error && !unavailable ? (
        <>
          <div className="mt-4 flex flex-wrap gap-x-6 gap-y-1 text-xs text-gray-500">
            <span>
              {copy.windowLabel}: {data.windowDays} days
            </span>
            <span>
              {copy.generatedAtLabel}: {formatDateTimeForLanguage(data.generatedAt, language)}
            </span>
          </div>

          <div className="mt-5">
            <h3 className="text-sm font-semibold text-gray-900">{copy.bySleeveRegimeHeader}</h3>
            {sortedSleeveRegime.length === 0 ? (
              <div className="mt-3 rounded-md border border-dashed border-gray-200 px-4 py-5 text-center text-sm text-gray-500">
                <p>{copy.emptyStats}</p>
                <p className="mt-1 text-xs text-gray-400">{copy.emptyStatsHint}</p>
              </div>
            ) : (
              <div className="mt-3 overflow-x-auto">
                <table className="w-full min-w-[640px] divide-y divide-gray-100 text-sm">
                  <thead className="bg-gray-50 text-xs uppercase tracking-wide text-gray-500">
                    <tr>
                      <th className="px-3 py-2 text-left">{copy.sleeveCol}</th>
                      <th className="px-3 py-2 text-left">{copy.regimeCol}</th>
                      <th className="px-3 py-2 text-right">{copy.tradesCol}</th>
                      <th className="px-3 py-2 text-right">{copy.winRateCol}</th>
                      <th className="px-3 py-2 text-right">{copy.pnlCol}</th>
                      <th className="px-3 py-2 text-right">{copy.avgHoldCol}</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-gray-100">
                    {sortedSleeveRegime.map((row, idx) => (
                      <tr key={`${row.sleeve}-${row.regime}-${idx}`} className="hover:bg-gray-50">
                        <td className="px-3 py-2 font-medium text-gray-900">{row.sleeve || copy.unspecified}</td>
                        <td className="px-3 py-2 text-gray-700">{row.regime || copy.unspecified}</td>
                        <td className="px-3 py-2 text-right text-gray-700">{row.tradeCount}</td>
                        <td className="px-3 py-2 text-right text-gray-700">{formatPercent(row.winRate, language)}</td>
                        <td className={`px-3 py-2 text-right font-medium ${row.totalPnl >= 0 ? "text-emerald-700" : "text-red-700"}`}>
                          {formatPnL(row.totalPnl, language)}
                        </td>
                        <td className="px-3 py-2 text-right text-gray-700">
                          {formatNumberForLanguage(row.avgHoldingDays, language, { maximumFractionDigits: 1 })}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </div>

          <div className="mt-6">
            <h3 className="text-sm font-semibold text-gray-900">{copy.recentLessons}</h3>
            {data.lessons.length === 0 ? (
              <div className="mt-3 rounded-md border border-dashed border-gray-200 px-4 py-5 text-center text-sm text-gray-500">
                <p>{copy.emptyLessons}</p>
                <p className="mt-1 text-xs text-gray-400">{copy.emptyLessonsHint}</p>
              </div>
            ) : (
              <ol className="mt-3 space-y-3">
                {data.lessons.map((lesson, idx) => (
                  <li
                    key={`${lesson.kind}-${lesson.createdAt}-${idx}`}
                    className={`rounded-lg border px-4 py-3 ${
                      SEVERITY_STYLES[lesson.severity] ?? "border-gray-200 bg-gray-50 text-gray-700"
                    }`}
                  >
                    <div className="flex flex-wrap items-baseline justify-between gap-2">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="rounded-full bg-white/70 px-2 py-0.5 text-[11px] font-semibold uppercase tracking-wide">
                          {lessonHeadline(lesson, copy)}
                        </span>
                        <span className="rounded-full bg-white/40 px-2 py-0.5 text-[11px] font-medium">
                          {copy.severityLabel[lesson.severity] ?? lesson.severity}
                        </span>
                        {(lesson.tags ?? []).slice(0, 4).map((tag) => (
                          <span key={tag} className="rounded-full bg-white/30 px-2 py-0.5 text-[11px]">
                            {tag}
                          </span>
                        ))}
                      </div>
                      <span className="text-xs">{formatDateTimeForLanguage(lesson.createdAt, language)}</span>
                    </div>
                    <p className="mt-2 text-sm font-medium">{lesson.title}</p>
                    {lesson.body ? <p className="mt-1 whitespace-pre-line text-sm opacity-90">{lesson.body}</p> : null}
                  </li>
                ))}
              </ol>
            )}
          </div>
        </>
      ) : null}
    </section>
  );
};

export default StrategyAttributionPanel;
