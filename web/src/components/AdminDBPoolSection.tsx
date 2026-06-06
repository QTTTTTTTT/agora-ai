// AdminDBPoolSection — W13-1 ops surface for the database
// connection pool, mirroring AdminMemReembedSection (W9-2) and
// AdminEmbedQuotaSection (W11-2).
//
// The Prometheus exporter (`fundai_db_*`) already emits every
// number this panel renders. This UI exists for the SRE-on-pager
// case "I just got a `pool exhausted` alert and I need to know,
// in one screen, whether we're saturated or just churning". A
// browser tab is faster than `curl | jq` at 2 AM.
//
// Auto-refreshes every 30s. The endpoint is super-admin gated
// server-side; we don't recheck perms locally — the API will
// 403 otherwise, which is the correct gate of record.
//
// Two design choices worth pinning:
//
//   1. Negative-encoded "undefined" from the Go side. The handler
//      sends `-1` for utilizationPct / waitAvgSeconds when the
//      underlying value is meaningless (no MaxOpen, no waits).
//      We render those cells as "—" rather than treating -1 as
//      a number to colour. Mistaking "undefined" for "good" or
//      "bad" would confuse the very alert this panel triages.
//   2. Two thresholds. Utilization > 80% → warn, > 95% → critical
//      mirror the runbook §0 guidance for the existing
//      `fundai_db_pool_in_use / max` Prometheus alert. Wait-count
//      growth gets its own pair (≥ 1 → warn, sustained → critical
//      via the existing alert; the panel stays neutral and just
//      shows the live value).

import { useCallback, useEffect, useState } from "react";

import {
  fetchAdminDBPoolStatus,
  formatApiError,
  type AdminDBPoolStatus,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

const REFRESH_MS = 30_000;

const UTILIZATION_WARN_PCT = 80;
const UTILIZATION_CRITICAL_PCT = 95;

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  refreshLabel: string;
  loadingText: string;
  errorPrefix: string;
  utilizationLabel: string;
  utilizationHintHealthy: string;
  utilizationHintWarn: string;
  utilizationHintCritical: string;
  utilizationHintUnknown: string;
  inUseLabel: string;
  idleLabel: string;
  openLabel: string;
  capLabelMaxOpen: string;
  capLabelMaxIdle: string;
  capLabelLifetime: string;
  waitCountLabel: string;
  waitAvgLabel: string;
  waitAvgUnknownText: string;
  waitDurationLabel: string;
  closedHeader: string;
  closedIdleLabel: string;
  closedIdleTimeLabel: string;
  closedLifetimeLabel: string;
  observedAtLabel: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "数据库连接池",
    subtitle:
      "应用进程的 SQL 连接池实时状态；用于在「pool exhausted」告警触发时，快速判断是「正在饱和」还是「在正常 churn」。仅管理员可见。",
    refreshLabel: "刷新",
    loadingText: "加载中…",
    errorPrefix: "加载失败",
    utilizationLabel: "使用率",
    utilizationHintHealthy: "连接池健康。",
    utilizationHintWarn: "在用连接接近上限，请关注。",
    utilizationHintCritical: "在用连接已逼近上限，需立即处理。",
    utilizationHintUnknown: "未配置 MaxOpen，使用率未知。",
    inUseLabel: "在用",
    idleLabel: "空闲",
    openLabel: "已建立",
    capLabelMaxOpen: "MaxOpen",
    capLabelMaxIdle: "MaxIdle (配置)",
    capLabelLifetime: "ConnMaxLifetime",
    waitCountLabel: "累计等待次数",
    waitAvgLabel: "平均等待",
    waitAvgUnknownText: "无等待",
    waitDurationLabel: "累计等待时长",
    closedHeader: "已关闭连接（按原因）",
    closedIdleLabel: "MaxIdleClosed",
    closedIdleTimeLabel: "MaxIdleTimeClosed",
    closedLifetimeLabel: "MaxLifetimeClosed",
    observedAtLabel: "数据时间",
  },
  "en-US": {
    title: "Database connection pool",
    subtitle:
      "Live state of the application's SQL connection pool. Helps an on-caller distinguish 'we're saturated' from 'we're churning' the moment a pool-exhaustion alert fires. Admin only.",
    refreshLabel: "Refresh",
    loadingText: "Loading…",
    errorPrefix: "Load failed",
    utilizationLabel: "Utilization",
    utilizationHintHealthy: "Pool is healthy.",
    utilizationHintWarn: "In-use connections approaching cap — watch closely.",
    utilizationHintCritical: "In-use connections at cap — investigate now.",
    utilizationHintUnknown: "MaxOpen not configured — utilization undefined.",
    inUseLabel: "In use",
    idleLabel: "Idle",
    openLabel: "Open",
    capLabelMaxOpen: "MaxOpen",
    capLabelMaxIdle: "MaxIdle (configured)",
    capLabelLifetime: "ConnMaxLifetime",
    waitCountLabel: "Lifetime wait events",
    waitAvgLabel: "Average wait",
    waitAvgUnknownText: "No waits yet",
    waitDurationLabel: "Lifetime wait duration",
    closedHeader: "Closed connections (by reason)",
    closedIdleLabel: "MaxIdleClosed",
    closedIdleTimeLabel: "MaxIdleTimeClosed",
    closedLifetimeLabel: "MaxLifetimeClosed",
    observedAtLabel: "As of",
  },
};

function formatLocalIso(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleString();
}

function formatPct(pct: number): string {
  if (!Number.isFinite(pct) || pct < 0) return "—";
  return `${pct.toFixed(1)}%`;
}

function formatSeconds(seconds: number): string {
  if (!Number.isFinite(seconds) || seconds < 0) return "—";
  if (seconds === 0) return "0";
  if (seconds < 0.001) return `${(seconds * 1_000_000).toFixed(0)} µs`;
  if (seconds < 1) return `${(seconds * 1000).toFixed(1)} ms`;
  if (seconds < 60) return `${seconds.toFixed(2)} s`;
  return `${(seconds / 60).toFixed(1)} min`;
}

function utilizationTone(pct: number): {
  color: string;
  hintKey:
    | "utilizationHintHealthy"
    | "utilizationHintWarn"
    | "utilizationHintCritical"
    | "utilizationHintUnknown";
} {
  // "Unknown" first, before the threshold checks — otherwise -1
  // would short-circuit into "healthy".
  if (!Number.isFinite(pct) || pct < 0) {
    return { color: "text-zinc-400", hintKey: "utilizationHintUnknown" };
  }
  if (pct >= UTILIZATION_CRITICAL_PCT) {
    return { color: "text-rose-300", hintKey: "utilizationHintCritical" };
  }
  if (pct >= UTILIZATION_WARN_PCT) {
    return { color: "text-amber-300", hintKey: "utilizationHintWarn" };
  }
  return { color: "text-emerald-300", hintKey: "utilizationHintHealthy" };
}

export function AdminDBPoolSection({ language }: Props) {
  const t = messages[language];
  const [status, setStatus] = useState<AdminDBPoolStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      setStatus(await fetchAdminDBPoolStatus());
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

  const tone = status ? utilizationTone(status.utilizationPct) : null;

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

      {status && tone && (
        <>
          <div className="mt-6 grid grid-cols-2 gap-3 md:grid-cols-4">
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.utilizationLabel}
              </div>
              <div
                className={`mt-1 text-2xl font-semibold tabular-nums ${tone.color}`}
              >
                {formatPct(status.utilizationPct)}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {t[tone.hintKey]}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.inUseLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.inUseConnections}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                / {status.maxOpenConnections > 0 ? status.maxOpenConnections : "∞"} {t.capLabelMaxOpen}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.idleLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.idleConnections}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                / {status.maxIdleConnsConfig} {t.capLabelMaxIdle}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.openLabel}
              </div>
              <div className="mt-1 text-2xl font-semibold tabular-nums text-zinc-100">
                {status.openConnections}
              </div>
              <div className="mt-1 text-xs text-zinc-500">
                {t.capLabelLifetime} {status.connMaxLifetime}
              </div>
            </div>
          </div>

          <div className="mt-3 grid grid-cols-2 gap-3 md:grid-cols-3">
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.waitCountLabel}
              </div>
              <div
                className={`mt-1 text-xl font-semibold tabular-nums ${
                  status.waitCount > 0 ? "text-amber-300" : "text-zinc-100"
                }`}
              >
                {status.waitCount}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.waitAvgLabel}
              </div>
              <div className="mt-1 text-xl font-semibold tabular-nums text-zinc-100">
                {status.waitAvgSeconds < 0 || !Number.isFinite(status.waitAvgSeconds)
                  ? t.waitAvgUnknownText
                  : formatSeconds(status.waitAvgSeconds)}
              </div>
            </div>
            <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
              <div className="text-xs uppercase text-zinc-400">
                {t.waitDurationLabel}
              </div>
              <div className="mt-1 text-xl font-semibold tabular-nums text-zinc-100">
                {status.waitDurationHuman || "0s"}
              </div>
            </div>
          </div>

          <div className="mt-3 rounded-lg border border-zinc-700 bg-zinc-800/40 p-4">
            <div className="text-xs uppercase text-zinc-400">{t.closedHeader}</div>
            <div className="mt-2 grid grid-cols-3 gap-3 text-sm">
              <div>
                <div className="text-xs text-zinc-500">{t.closedIdleLabel}</div>
                <div className="mt-1 font-semibold tabular-nums text-zinc-100">
                  {status.maxIdleClosedTotal}
                </div>
              </div>
              <div>
                <div className="text-xs text-zinc-500">{t.closedIdleTimeLabel}</div>
                <div className="mt-1 font-semibold tabular-nums text-zinc-100">
                  {status.maxIdleTimeClosedTotal}
                </div>
              </div>
              <div>
                <div className="text-xs text-zinc-500">{t.closedLifetimeLabel}</div>
                <div className="mt-1 font-semibold tabular-nums text-zinc-100">
                  {status.maxLifetimeClosedTotal}
                </div>
              </div>
            </div>
          </div>

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

export default AdminDBPoolSection;
