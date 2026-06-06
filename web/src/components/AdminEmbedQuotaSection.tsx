// AdminEmbedQuotaSection — W11-2 ops surface for the
// embedquota.Limiter introduced by W4-23 and instrumented over
// Waves 6→10.
//
// The Prometheus exporter (W6-2 / W8-1 / W9-1 / W10-1) already
// exposes every number this panel renders, but the
// SRE-on-pager case is "I just got a throttle alert, I have a
// browser, I want one screen that tells me whether throttle is
// happening because of more calls or fatter calls" — that's
// the question this panel is built to answer.
//
// Auto-refreshes every 30s. The endpoint is super-admin gated
// server-side; we don't recheck perms locally — the API call
// will 403 otherwise, which is the right gate of record.

import { useCallback, useEffect, useState } from "react";

import {
  fetchAdminEmbedQuotaStatus,
  formatApiError,
  type AdminEmbedQuotaStatus,
} from "../lib/api";
import { computeHistoryView } from "../lib/embedQuotaHistory";

type Language = "zh-CN" | "en-US";

const REFRESH_MS = 30_000;

// Threshold mirrors the runbook (PROMETHEUS_QUERIES.md §2):
// > 0.85 → warn, > 0.95 → critical. We surface a hint and a
// colour, never an actual page — alerting lives in Prometheus.
const TOKENS_SHARE_WARN = 0.85;
const TOKENS_SHARE_CRITICAL = 0.95;

// Same idea for wait-time tail (runbook §3): p99 < 0.5s is
// healthy, 0.5..5s deserves a warn tone, > 5s is hot.
const WAIT_P99_WARN_SECONDS = 0.5;
const WAIT_P99_CRITICAL_SECONDS = 5;

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  refreshLabel: string;
  loadingText: string;
  errorPrefix: string;
  disabledTitle: string;
  disabledHint: string;
  statusLabel: string;
  statusValues: Record<string, string>;
  tokensLabel: string;
  tokensHintHealthy: string;
  tokensHintWarn: string;
  tokensHintCritical: string;
  callsLabel: string;
  throttledLabel: string;
  exhaustedLabel: string;
  waitP99Label: string;
  waitP99HintHealthy: string;
  waitP99HintWarn: string;
  waitP99HintCritical: string;
  tokensP99Label: string;
  tokensP99Hint: string;
  observedAtLabel: string;
  historyLabel: string;
  historyHint: string;
  historyEmpty: string;
  historyTodayMarker: string;
  historyCapMarker: string;
  historyCapTooltip: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "嵌入配额限流",
    subtitle:
      "PM / 记忆嵌入管线对外部 LLM 提供商的限流与配额状态；用于在告警触发时快速定位是「次数太多」还是「单次太胖」。仅管理员可见。",
    refreshLabel: "刷新",
    loadingText: "加载中…",
    errorPrefix: "加载失败",
    disabledTitle: "限流器未启用",
    disabledHint:
      "本实例未配置 embedquota.Limiter；如果应当启用，请检查启动日志与环境变量。",
    statusLabel: "状态",
    statusValues: {
      ok: "正常",
      throttled: "正在限流",
      near_limit: "接近上限",
      exhausted: "已耗尽",
      unavailable: "不可用",
    },
    tokensLabel: "今日 token 占比",
    tokensHintHealthy: "预算充足。",
    tokensHintWarn: "预算消耗偏快，请关注。",
    tokensHintCritical: "预算即将耗尽，需立即处理。",
    callsLabel: "近 60s 调用 / 上限",
    throttledLabel: "累计 throttle 次数",
    exhaustedLabel: "累计 exhausted 次数",
    waitP99Label: "Acquire 等待 p99",
    waitP99HintHealthy: "尾部健康。",
    waitP99HintWarn: "存在持续 throttle，关注调用频率。",
    waitP99HintCritical: "尾部明显恶化，下游用户体感会受影响。",
    tokensP99Label: "单次 token p99",
    tokensP99Hint: "与等待 p99 配对：判断 throttle 是因「次数」还是「单次」。",
    observedAtLabel: "数据时间",
    historyLabel: "近 7 日 token 用量",
    historyHint:
      "每根柱子是一天的累计 token；对比今日与昨日，能快速判断「今天是不是用得反常多」。",
    historyEmpty: "暂无历史数据",
    historyTodayMarker: "今日",
    historyCapMarker: "日上限",
    historyCapTooltip: "日上限 {{tokens}} tokens",
  },
  "en-US": {
    title: "Embed quota limiter",
    subtitle:
      "Rate-limit and daily-quota state for the PM / memory embed pipeline against the external LLM provider. Helps an on-caller distinguish 'too many calls' from 'too-large calls' when a throttle alert fires. Admin only.",
    refreshLabel: "Refresh",
    loadingText: "Loading…",
    errorPrefix: "Load failed",
    disabledTitle: "Limiter disabled",
    disabledHint:
      "This deployment does not run an embedquota.Limiter. If this is unexpected, check the startup logs and env vars.",
    statusLabel: "Status",
    statusValues: {
      ok: "OK",
      throttled: "Throttled",
      near_limit: "Near limit",
      exhausted: "Exhausted",
      unavailable: "Unavailable",
    },
    tokensLabel: "Daily token share",
    tokensHintHealthy: "Plenty of budget.",
    tokensHintWarn: "Burning faster than expected — keep an eye on it.",
    tokensHintCritical: "Budget nearly exhausted — investigate now.",
    callsLabel: "Calls in last 60s / cap",
    throttledLabel: "Lifetime throttle events",
    exhaustedLabel: "Lifetime exhausted events",
    waitP99Label: "Acquire wait p99",
    waitP99HintHealthy: "Tail is healthy.",
    waitP99HintWarn: "Sustained throttling — check call rate upstream.",
    waitP99HintCritical: "Tail is clearly degraded — UX downstream is impacted.",
    tokensP99Label: "Per-call tokens p99",
    tokensP99Hint:
      "Pairs with the wait p99: tells you whether throttle is from 'too many' or 'too large'.",
    observedAtLabel: "As of",
    historyLabel: "Last 7 days of token usage",
    historyHint:
      "Each bar is one day's cumulative tokens; comparing today to yesterday surfaces sudden over-usage.",
    historyEmpty: "No history yet",
    historyTodayMarker: "Today",
    historyCapMarker: "Daily cap",
    historyCapTooltip: "Daily cap {{tokens}} tokens",
  },
};

function formatLocalIso(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function formatPercent(share: number): string {
  if (!Number.isFinite(share)) return "—";
  return `${(share * 100).toFixed(1)}%`;
}

function formatSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0";
  if (seconds < 1) return `${(seconds * 1000).toFixed(0)} ms`;
  if (seconds < 60) return `${seconds.toFixed(1)} s`;
  if (seconds < 3600) return `${(seconds / 60).toFixed(1)} min`;
  return `${(seconds / 3600).toFixed(1)} h`;
}

function formatTokens(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

function tokensTone(share: number): {
  color: string;
  hintKey: "tokensHintHealthy" | "tokensHintWarn" | "tokensHintCritical";
} {
  if (share >= TOKENS_SHARE_CRITICAL) {
    return { color: "text-rose-300", hintKey: "tokensHintCritical" };
  }
  if (share >= TOKENS_SHARE_WARN) {
    return { color: "text-amber-300", hintKey: "tokensHintWarn" };
  }
  return { color: "text-emerald-300", hintKey: "tokensHintHealthy" };
}

function waitTone(p99Seconds: number): {
  color: string;
  hintKey: "waitP99HintHealthy" | "waitP99HintWarn" | "waitP99HintCritical";
} {
  if (p99Seconds >= WAIT_P99_CRITICAL_SECONDS) {
    return { color: "text-rose-300", hintKey: "waitP99HintCritical" };
  }
  if (p99Seconds >= WAIT_P99_WARN_SECONDS) {
    return { color: "text-amber-300", hintKey: "waitP99HintWarn" };
  }
  return { color: "text-emerald-300", hintKey: "waitP99HintHealthy" };
}

// formatHistoryDayLabel renders the YYYY-MM-DD value in the
// limiter's UTC frame as a short MM-DD label suited for a 7-bar
// sparkline. Locale-aware long-form would overflow the card
// width on phones; we keep it compact and let the title attribute
// (set on the bar) hold the full ISO day for hover.
function formatHistoryDayLabel(day: string): string {
  // Avoid `new Date(day)` since that's parsed as local time and
  // can shift by ±1 day for users in non-UTC time zones —
  // the limiter's "today" is UTC.
  return day.slice(5);
}


function statusTone(statusKey: string): string {
  switch (statusKey) {
    case "ok":
      return "text-emerald-300";
    case "near_limit":
    case "throttled":
      return "text-amber-300";
    case "exhausted":
      return "text-rose-300";
    default:
      return "text-zinc-400";
  }
}

export function AdminEmbedQuotaSection({ language }: Props) {
  const t = messages[language];
  const [status, setStatus] = useState<AdminEmbedQuotaStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setStatus(await fetchAdminEmbedQuotaStatus());
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [t.errorPrefix]);

  useEffect(() => {
    void reload();
    const id = window.setInterval(() => {
      void reload();
    }, REFRESH_MS);
    return () => window.clearInterval(id);
  }, [reload]);

  const tokensInfo = status ? tokensTone(status.tokensTodayShare) : null;
  const waitInfo = status ? waitTone(status.acquireWaitP99Seconds) : null;
  const statusLabel =
    status && t.statusValues[status.status]
      ? t.statusValues[status.status]
      : status?.status ?? "";

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void reload()}
          disabled={loading}
          className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
        >
          {loading ? t.loadingText : t.refreshLabel}
        </button>
      </div>

      {error && (
        <div className="mt-4 rounded-md border border-rose-700 bg-rose-900/30 p-3 text-sm text-rose-200">
          {t.errorPrefix}: {error}
        </div>
      )}

      {status && !status.enabled && (
        <div className="mt-4 rounded-md border border-zinc-600 bg-zinc-800/40 p-4 text-sm text-zinc-300">
          <div className="font-semibold text-zinc-100">{t.disabledTitle}</div>
          <p className="mt-1 text-zinc-400">{t.disabledHint}</p>
        </div>
      )}

      {status && status.enabled && tokensInfo && waitInfo && (
        <>
          <div className="mt-6 grid grid-cols-2 gap-3 md:grid-cols-3">
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.statusLabel}
              </div>
              <div
                className={`mt-1 text-2xl font-semibold ${statusTone(status.status)}`}
              >
                {statusLabel}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.tokensLabel}
              </div>
              <div
                className={`mt-1 text-2xl font-semibold tabular-nums ${tokensInfo.color}`}
              >
                {formatPercent(status.tokensTodayShare)}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {formatTokens(status.tokensTodayUsed)} /{" "}
                {formatTokens(status.tokensDailyMax)} ·{" "}
                {t[tokensInfo.hintKey]}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.callsLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.callsLastMinute} / {status.callsPerMinuteMax}
              </div>
            </div>
          </div>

          <div className="mt-3 grid grid-cols-2 gap-3 md:grid-cols-4">
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.throttledLabel}
              </div>
              <div
                className={`mt-1 text-xl font-semibold tabular-nums ${
                  status.throttledTotal > 0 ? "text-amber-300" : "text-zinc-100"
                }`}
              >
                {status.throttledTotal}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.exhaustedLabel}
              </div>
              <div
                className={`mt-1 text-xl font-semibold tabular-nums ${
                  status.exhaustedTotal > 0 ? "text-rose-300" : "text-zinc-100"
                }`}
              >
                {status.exhaustedTotal}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.waitP99Label}
              </div>
              <div
                className={`mt-1 text-xl font-semibold tabular-nums ${waitInfo.color}`}
              >
                {formatSeconds(status.acquireWaitP99Seconds)}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {t[waitInfo.hintKey]}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.tokensP99Label}
              </div>
              <div className="mt-1 text-xl font-semibold tabular-nums text-zinc-100">
                {formatTokens(status.callTokensP99)}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {t.tokensP99Hint}
              </div>
            </div>
          </div>

          {status.tokenHistory && status.tokenHistory.length > 0 && (() => {
            // Compute view once per render — cheap, but doing it
            // twice (bars + labels) would mask a future regression
            // where bar order drifts from label order.
            const view = computeHistoryView(
              status.tokenHistory,
              status.tokensDailyMax,
            );
            const showCapLine = view.capRatio > 0;
            // Round once so layout doesn't jitter between renders
            // when capRatio is a non-terminating fraction.
            const capPct = Math.round(view.capRatio * 100);
            return (
              <div className="mt-3 rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
                <div className="flex items-baseline justify-between">
                  <div className="text-xs uppercase text-zinc-400">
                    {t.historyLabel}
                  </div>
                  <div className="text-xs text-zinc-500">{t.historyHint}</div>
                </div>
                <div className="relative mt-3 flex h-16 items-end gap-1">
                  {showCapLine && (
                    // Dashed reference line at daily cap. `pointer-events-none`
                    // so it never intercepts the bar `title` tooltips.
                    <div
                      className="pointer-events-none absolute inset-x-0 border-t border-dashed border-rose-400/70"
                      style={{ bottom: `${capPct}%` }}
                      title={t.historyCapTooltip.replace(
                        "{{tokens}}",
                        formatTokens(status.tokensDailyMax),
                      )}
                    >
                      <span className="absolute -top-3 right-0 rounded bg-rose-500/20 px-1 text-[10px] font-medium text-rose-200">
                        {t.historyCapMarker} · {formatTokens(status.tokensDailyMax)}
                      </span>
                    </div>
                  )}
                  {view.bars.map((bar) => {
                    const heightPct = Math.max(2, Math.round(bar.ratio * 100));
                    const fill = bar.isToday
                      ? "bg-amber-400"
                      : bar.tokens > 0
                        ? "bg-emerald-500/70"
                        : "bg-zinc-700";
                    return (
                      <div
                        key={bar.day}
                        className="relative z-10 flex flex-1 flex-col items-center justify-end"
                        title={`${bar.day} · ${formatTokens(bar.tokens)} tokens${
                          bar.isToday ? ` · ${t.historyTodayMarker}` : ""
                        }`}
                      >
                        <div
                          className={`w-full rounded-t-sm ${fill}`}
                          style={{ height: `${heightPct}%` }}
                        />
                      </div>
                    );
                  })}
                </div>
                <div className="mt-1 flex gap-1">
                  {view.bars.map((bar) => (
                    <div
                      key={`label-${bar.day}`}
                      className={`flex-1 text-center text-[10px] ${
                        bar.isToday ? "font-semibold text-amber-300" : "text-zinc-500"
                      }`}
                    >
                      {formatHistoryDayLabel(bar.day)}
                    </div>
                  ))}
                </div>
              </div>
            );
          })()}

          <div className="mt-4 rounded-lg border border-zinc-700 bg-zinc-900/30 p-3 text-sm">
            <div className="text-xs uppercase text-zinc-400">
              {t.observedAtLabel}
            </div>
            <div className="mt-1 text-zinc-200">
              {formatLocalIso(status.observedAt)}
            </div>
          </div>
        </>
      )}
    </section>
  );
}

export default AdminEmbedQuotaSection;
