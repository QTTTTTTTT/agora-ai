// AdminEmbedQuotaPerFundSection — W14-3 per-fund drill-down for
// the embedquota observability surface.
//
// The W11-2 AdminEmbedQuotaSection answers "is the limiter
// healthy in aggregate?". This panel answers the next question
// an on-caller asks once that aggregate goes red:
//
//   "Which fund is causing the throttle?"
//
// We render a sorted table with one row per fund, ranked
// implicitly by lex order of FundID (matching the snapshot's
// own sort) so dashboards stay stable across refreshes.
//
// The panel is opt-in: the recorder is only present when
// EMBED_QUOTA_OBS_ENABLED=true on the server. When the
// recorder is nil the endpoint returns enabled=false; we render
// a neutral disabled state instead of an error, mirroring the
// W9-2 / W11-2 / W13-1 panels.
//
// Auto-refreshes every 30s, same cadence as the sibling panels.

import { useCallback, useEffect, useState } from "react";

import {
  fetchAdminEmbedQuotaPerFund,
  formatApiError,
  type AdminEmbedQuotaFundEntry,
  type AdminEmbedQuotaPerFundStatus,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

const REFRESH_MS = 30_000;

// Tail-latency tones mirror the W11-2 sibling: <0.5s healthy,
// 0.5..5s warn, >=5s critical. Centralised here (rather than
// imported) so we don't widen the API surface of the parent
// component just for two numeric thresholds.
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
  emptyTitle: string;
  emptyHint: string;
  fundsLabel: string;
  observedAtLabel: string;
  cardinalityNotice: string;
  // Column headers.
  fundIdHeader: string;
  tokensTodayHeader: string;
  callTokensP99Header: string;
  waitP99Header: string;
  throttledHeader: string;
  exhaustedHeader: string;
  lastSeenHeader: string;
  // Row hints / tooltips.
  overflowFundLabel: string;
  overflowTooltip: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "嵌入配额 · 按 fund 拆分",
    subtitle:
      "聚合面板已经告诉你「正在 throttle」；这张表告诉你 throttle 来自哪只基金。需要在服务端开 EMBED_QUOTA_OBS_ENABLED=true 才会有数据。",
    refreshLabel: "刷新",
    loadingText: "加载中…",
    errorPrefix: "加载失败",
    disabledTitle: "Per-fund 观测未启用",
    disabledHint:
      "未配置 embedquotaobs.Recorder（环境变量 EMBED_QUOTA_OBS_ENABLED 未开启）；启用后会按 fund_id 维度记录调用直方图与配额耗用。",
    emptyTitle: "暂无数据",
    emptyHint: "Recorder 已启动但还没有任何调用落库；当 PM 走召回 / 嵌入回填路径后会出现。",
    fundsLabel: "活跃 fund 数",
    observedAtLabel: "数据时间",
    cardinalityNotice:
      "按 cardinality 预算最多展示 200 只基金；超出部分会汇入 _overflow，作为「观测器达到上限」的告警信号。",
    fundIdHeader: "Fund ID",
    tokensTodayHeader: "今日 token",
    callTokensP99Header: "单次 token p99",
    waitP99Header: "等待 p99",
    throttledHeader: "Throttle 次数",
    exhaustedHeader: "Exhaust 次数",
    lastSeenHeader: "最近一次",
    overflowFundLabel: "（汇总桶）",
    overflowTooltip:
      "_overflow 表示活跃基金数已超过 MaxFunds 预算；这是「观测器达上限」的告警，不是真实基金。",
  },
  "en-US": {
    title: "Embed quota · per fund",
    subtitle:
      "The aggregate panel already says 'throttle is firing'; this table says which fund is driving it. Data appears only when EMBED_QUOTA_OBS_ENABLED=true on the server.",
    refreshLabel: "Refresh",
    loadingText: "Loading…",
    errorPrefix: "Load failed",
    disabledTitle: "Per-fund observability disabled",
    disabledHint:
      "embedquotaobs.Recorder is not wired (EMBED_QUOTA_OBS_ENABLED is not 'true'). Enable it to start recording per-fund call histograms and quota burn.",
    emptyTitle: "No funds yet",
    emptyHint:
      "Recorder is up but has not seen any calls yet; entries appear once a PM exercises the recall / embed-backfill path.",
    fundsLabel: "Active funds",
    observedAtLabel: "Observed at",
    cardinalityNotice:
      "Up to 200 funds tracked per cardinality budget; excess collapses into _overflow as the 'recorder at capacity' alarm.",
    fundIdHeader: "Fund ID",
    tokensTodayHeader: "Tokens today",
    callTokensP99Header: "Call tokens p99",
    waitP99Header: "Wait p99",
    throttledHeader: "Throttled",
    exhaustedHeader: "Exhausted",
    lastSeenHeader: "Last seen",
    overflowFundLabel: "(overflow bucket)",
    overflowTooltip:
      "_overflow is synthetic: the recorder has more active funds than its MaxFunds budget. Treat as a cardinality alarm, not a real fund.",
  },
};

// Synthetic fund ID emitted by embedquotaobs when MaxFunds is
// exceeded. Kept literal here rather than imported so the FE
// bundle doesn't need to depend on a backend constant.
const OVERFLOW_FUND_ID = "_overflow";

function formatTokens(n: number): string {
  if (!Number.isFinite(n)) return "—";
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

function formatSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds <= 0) return "0";
  if (seconds < 1) return `${(seconds * 1000).toFixed(0)} ms`;
  if (seconds < 60) return `${seconds.toFixed(1)} s`;
  return `${(seconds / 60).toFixed(1)} min`;
}

function waitTone(p99Seconds: number): string {
  if (p99Seconds >= WAIT_P99_CRITICAL_SECONDS) return "text-rose-300";
  if (p99Seconds >= WAIT_P99_WARN_SECONDS) return "text-amber-300";
  return "text-emerald-300";
}

function formatRelativeTime(iso: string, language: Language): string {
  const t = Date.parse(iso);
  if (!Number.isFinite(t)) return "—";
  const deltaSec = Math.max(0, Math.round((Date.now() - t) / 1000));
  if (deltaSec < 60) return language === "zh-CN" ? `${deltaSec} 秒前` : `${deltaSec}s ago`;
  if (deltaSec < 3600) {
    const m = Math.round(deltaSec / 60);
    return language === "zh-CN" ? `${m} 分钟前` : `${m}m ago`;
  }
  if (deltaSec < 86400) {
    const h = Math.round(deltaSec / 3600);
    return language === "zh-CN" ? `${h} 小时前` : `${h}h ago`;
  }
  const d = Math.round(deltaSec / 86400);
  return language === "zh-CN" ? `${d} 天前` : `${d}d ago`;
}

function FundRow({
  entry,
  copy,
  language,
}: {
  entry: AdminEmbedQuotaFundEntry;
  copy: Copy;
  language: Language;
}) {
  const isOverflow = entry.fundId === OVERFLOW_FUND_ID;
  const fundDisplay = isOverflow ? (
    <span title={copy.overflowTooltip} className="text-amber-300">
      {entry.fundId} <span className="text-xs text-slate-500">{copy.overflowFundLabel}</span>
    </span>
  ) : (
    <span className="font-mono">{entry.fundId}</span>
  );
  return (
    <tr className="border-t border-slate-700/50">
      <td className="px-3 py-2 text-slate-200">{fundDisplay}</td>
      <td className="px-3 py-2 text-right text-slate-200">{formatTokens(entry.tokensTodayUsed)}</td>
      <td className="px-3 py-2 text-right text-slate-200">{formatTokens(entry.callTokensP99)}</td>
      <td className={`px-3 py-2 text-right ${waitTone(entry.acquireWaitP99Seconds)}`}>
        {formatSeconds(entry.acquireWaitP99Seconds)}
      </td>
      <td className="px-3 py-2 text-right text-slate-200">{entry.throttledTotal}</td>
      <td className="px-3 py-2 text-right text-slate-200">{entry.exhaustedTotal}</td>
      <td className="px-3 py-2 text-right text-slate-400">
        {formatRelativeTime(entry.lastSeenAt, language)}
      </td>
    </tr>
  );
}

export function AdminEmbedQuotaPerFundSection({ language }: Props) {
  const copy = messages[language];
  const [data, setData] = useState<AdminEmbedQuotaPerFundStatus | null>(null);
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    try {
      const next = await fetchAdminEmbedQuotaPerFund();
      setData(next);
      setError(null);
    } catch (e) {
      setError(formatApiError(e, copy.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [copy.errorPrefix]);

  useEffect(() => {
    void reload();
    const t = setInterval(() => void reload(), REFRESH_MS);
    return () => clearInterval(t);
  }, [reload]);

  return (
    <section className="rounded-2xl border border-slate-700/60 bg-slate-900/40 px-5 py-5 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h3 className="text-base font-semibold text-slate-100">{copy.title}</h3>
          <p className="mt-1 text-sm text-slate-400">{copy.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void reload()}
          disabled={loading}
          className="rounded-md border border-slate-600 px-3 py-1 text-xs text-slate-200 hover:border-slate-400 hover:text-white disabled:opacity-50"
        >
          {loading ? copy.loadingText : copy.refreshLabel}
        </button>
      </div>

      {error && (
        <p className="mt-3 rounded-md border border-rose-500/40 bg-rose-500/10 px-3 py-2 text-sm text-rose-200">
          {copy.errorPrefix}: {error}
        </p>
      )}

      {data && !data.enabled && (
        <div className="mt-4 rounded-md border border-slate-700/60 bg-slate-800/40 px-4 py-3">
          <p className="text-sm font-medium text-slate-200">{copy.disabledTitle}</p>
          <p className="mt-1 text-xs text-slate-400">{copy.disabledHint}</p>
        </div>
      )}

      {data && data.enabled && data.funds.length === 0 && (
        <div className="mt-4 rounded-md border border-slate-700/60 bg-slate-800/40 px-4 py-3">
          <p className="text-sm font-medium text-slate-200">{copy.emptyTitle}</p>
          <p className="mt-1 text-xs text-slate-400">{copy.emptyHint}</p>
        </div>
      )}

      {data && data.enabled && data.funds.length > 0 && (
        <>
          <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-slate-400">
            <span>
              {copy.fundsLabel}: <span className="text-slate-200">{data.funds.length}</span>
            </span>
            <span>
              {copy.observedAtLabel}: <span className="text-slate-200">{data.observedAt}</span>
            </span>
          </div>
          <div className="mt-3 overflow-x-auto">
            <table className="min-w-full border-separate border-spacing-0 text-sm">
              <thead>
                <tr className="text-left text-xs uppercase tracking-wide text-slate-500">
                  <th className="px-3 py-2 font-normal">{copy.fundIdHeader}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.tokensTodayHeader}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.callTokensP99Header}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.waitP99Header}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.throttledHeader}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.exhaustedHeader}</th>
                  <th className="px-3 py-2 text-right font-normal">{copy.lastSeenHeader}</th>
                </tr>
              </thead>
              <tbody>
                {data.funds.map((entry) => (
                  <FundRow key={entry.fundId} entry={entry} copy={copy} language={language} />
                ))}
              </tbody>
            </table>
          </div>
          <p className="mt-3 text-xs text-slate-500">{copy.cardinalityNotice}</p>
        </>
      )}
    </section>
  );
}
