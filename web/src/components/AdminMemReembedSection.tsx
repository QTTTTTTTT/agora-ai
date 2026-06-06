// AdminMemReembedSection — W9-2 ops surface for the memory
// re-embed queue introduced by W6-1.
//
// The Prometheus exporter (W7-1) already exposes the same
// numbers, but the SRE-on-pager case is "I just got an alert,
// I have a browser, I don't have a Grafana dashboard handy" —
// this panel shows `pending` (the gauge that triggers the
// alert), the lifetime counters, and the latest error time so
// the operator can immediately see whether the queue is piling
// up or just crawling forward.
//
// Auto-refreshes every 30s. The endpoint is super-admin gated
// server-side; this component does not check perms locally
// (the API call will 403 otherwise) — that's intentional: the
// gate of record lives next to the data.

import { useCallback, useEffect, useState } from "react";

import {
  fetchAdminMemReembedStatus,
  formatApiError,
  type AdminMemReembedStatus,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

const REFRESH_MS = 30_000;

const PENDING_WARN_THRESHOLD = 50;
const PENDING_CRITICAL_THRESHOLD = 200;

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
  pendingLabel: string;
  pendingHintHealthy: string;
  pendingHintWarn: string;
  pendingHintCritical: string;
  embeddedLabel: string;
  retriedLabel: string;
  deadLetterLabel: string;
  lastErrorLabel: string;
  lastErrorNeverText: string;
  observedAtLabel: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "记忆重嵌入队列",
    subtitle:
      "由 memreembed 队列驱动的后台再嵌入流程；待处理数代表当前积压。仅管理员可见。",
    refreshLabel: "刷新",
    loadingText: "加载中…",
    errorPrefix: "加载失败",
    disabledTitle: "队列未启用",
    disabledHint:
      "此实例未配置 memreembed 队列；如果该子系统应当启用，请检查启动日志。",
    pendingLabel: "待处理",
    pendingHintHealthy: "队列健康。",
    pendingHintWarn: "积压上升中，请关注。",
    pendingHintCritical: "积压严重，需立即处理。",
    embeddedLabel: "已嵌入",
    retriedLabel: "重试次数",
    deadLetterLabel: "死信数",
    lastErrorLabel: "最近一次错误",
    lastErrorNeverText: "无（本进程启动以来）",
    observedAtLabel: "数据时间",
  },
  "en-US": {
    title: "Memory re-embed queue",
    subtitle:
      "Background re-embedding queue driven by memreembed; the pending count reflects current backlog. Admin only.",
    refreshLabel: "Refresh",
    loadingText: "Loading…",
    errorPrefix: "Load failed",
    disabledTitle: "Queue disabled",
    disabledHint:
      "This deployment is not running the memreembed queue. If this is unexpected, check the startup logs.",
    pendingLabel: "Pending",
    pendingHintHealthy: "Queue is healthy.",
    pendingHintWarn: "Backlog is growing — keep an eye on it.",
    pendingHintCritical: "Backlog is critical — investigate now.",
    embeddedLabel: "Embedded",
    retriedLabel: "Retries",
    deadLetterLabel: "Dead-lettered",
    lastErrorLabel: "Last error",
    lastErrorNeverText: "None since process start",
    observedAtLabel: "As of",
  },
};

function formatLocalIso(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function pendingTone(pending: number): {
  color: string;
  hintKey: "pendingHintHealthy" | "pendingHintWarn" | "pendingHintCritical";
} {
  if (pending >= PENDING_CRITICAL_THRESHOLD) {
    return { color: "text-rose-300", hintKey: "pendingHintCritical" };
  }
  if (pending >= PENDING_WARN_THRESHOLD) {
    return { color: "text-amber-300", hintKey: "pendingHintWarn" };
  }
  return { color: "text-emerald-300", hintKey: "pendingHintHealthy" };
}

export function AdminMemReembedSection({ language }: Props) {
  const t = messages[language];
  const [status, setStatus] = useState<AdminMemReembedStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setStatus(await fetchAdminMemReembedStatus());
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

  const tone = status ? pendingTone(status.pending) : null;

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

      {status && status.enabled && tone && (
        <>
          <div className="mt-6 grid grid-cols-2 gap-3 md:grid-cols-4">
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.pendingLabel}
              </div>
              <div
                className={`mt-1 text-2xl font-semibold tabular-nums ${tone.color}`}
              >
                {status.pending}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {t[tone.hintKey]}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.embeddedLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.embeddedTotal}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.retriedLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.retriedTotal}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.deadLetterLabel}
              </div>
              <div
                className={`mt-1 text-2xl font-semibold tabular-nums ${
                  status.deadLetterTotal > 0
                    ? "text-amber-300"
                    : "text-zinc-100"
                }`}
              >
                {status.deadLetterTotal}
              </div>
            </div>
          </div>

          <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-2">
            <div className="rounded-lg border border-zinc-700 bg-zinc-900/30 p-3 text-sm">
              <div className="text-xs uppercase text-zinc-400">
                {t.lastErrorLabel}
              </div>
              <div className="mt-1 text-zinc-200">
                {status.lastErrorTime
                  ? formatLocalIso(status.lastErrorTime)
                  : t.lastErrorNeverText}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-900/30 p-3 text-sm">
              <div className="text-xs uppercase text-zinc-400">
                {t.observedAtLabel}
              </div>
              <div className="mt-1 text-zinc-200">
                {formatLocalIso(status.observedAt)}
              </div>
            </div>
          </div>
        </>
      )}
    </section>
  );
}

export default AdminMemReembedSection;
