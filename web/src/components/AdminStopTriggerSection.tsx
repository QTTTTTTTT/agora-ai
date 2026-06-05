// AdminStopTriggerSection.tsx — admin view of pending stop
// orders + the trigger poller's run state.
//
// WHY THIS EXISTS
// ---------------
// The server has a stop-trigger poller that watches market prices
// and fires when a stop / trailing-stop level is breached. It
// exposes:
//
//   GET /api/admin/stop-trigger/status — poller snapshot +
//     list of currently pending stops across all funds.
//   POST /api/admin/stop-trigger/tick — force a one-shot tick
//     (useful for ops "I changed the trail amount, did it
//     actually re-arm correctly?" verifications).
//
// Until now there was no UI; ops had to curl the JSON or read
// from server logs. This section surfaces:
//
//   - poller enabled / disabled badge,
//   - last run timestamp + interval,
//   - count of pending stops,
//   - per-fund pending stops table (symbol / side / qty /
//     stop price / trail amount or %),
//   - "Force tick" button that triggers a manual run and
//     re-fetches the snapshot.
//
// Wraps the existing admin page conventions (gray-100 chrome,
// indigo accents, dark-mode classes throughout).
//
// SCOPE
// -----
// Read-only + tick. Editing trail params still happens via the
// trade-history modify-order flow on the per-fund page; the
// admin view is for "I want to know what's currently armed".

import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiGet, apiPost, formatApiError } from "../lib/api";

interface PendingStop {
  brokerOrderId: string;
  clientOrderId: string;
  fundId: string;
  symbol: string;
  instrumentKey: string;
  side: string;
  orderType: string;
  quantity: number;
  stopPrice: number;
  currentStopPrice: number;
  trailingHighWater?: number;
  trailingLowWater?: number;
  trailAmount?: number;
  trailPercent?: number;
  placedAt: string;
}

interface PollerSnapshot {
  enabled?: boolean;
  intervalMs?: number;
  lastRanAt?: string;
  lastTickedAt?: string;
  totalRuns?: number;
  totalTriggers?: number;
  lastError?: string;
}

interface StopTriggerStatus {
  enabled: boolean;
  poller?: PollerSnapshot;
  pendingStops?: PendingStop[];
  pendingCount?: number;
}

interface Props {
  language: "zh-CN" | "en-US";
}

const AdminStopTriggerSection: React.FC<Props> = ({ language }) => {
  const isEnglish = language === "en-US";
  const [status, setStatus] = useState<StopTriggerStatus | null>(null);
  const [loading, setLoading] = useState(false);
  const [ticking, setTicking] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const copy = isEnglish
    ? {
        title: "Stop trigger watch",
        subtitle: "Pending stops, trailing-stop high/low water, and the poller run state.",
        statusEnabled: "Poller enabled",
        statusDisabled: "Poller disabled",
        kPending: "Pending stops",
        kTotalRuns: "Total runs",
        kTotalTriggers: "Total triggers",
        kInterval: "Interval",
        kLastRun: "Last run",
        kLastTick: "Last tick",
        forceTick: "Force tick",
        ticking: "Ticking…",
        refresh: "Refresh",
        loadError: "Failed to load stop-trigger status",
        tickFailed: "Force tick failed",
        emptyTitle: "No pending stops",
        emptyDescription:
          "All current orders are filled, cancelled, or have no stop attached.",
        col: {
          fund: "Fund",
          symbol: "Symbol",
          side: "Side",
          qty: "Qty",
          stop: "Stop @",
          current: "Current @",
          trail: "Trail",
          highLow: "High / low water",
          placed: "Placed",
        },
      }
    : {
        title: "止损触发监控",
        subtitle: "查看当前挂单的止损 / 跟踪止损水位与轮询服务状态。",
        statusEnabled: "轮询已启用",
        statusDisabled: "轮询已停用",
        kPending: "挂单数",
        kTotalRuns: "累计轮询次数",
        kTotalTriggers: "累计触发数",
        kInterval: "轮询间隔",
        kLastRun: "上次运行",
        kLastTick: "上次手动触发",
        forceTick: "立即触发一次",
        ticking: "触发中…",
        refresh: "刷新",
        loadError: "无法加载止损触发状态",
        tickFailed: "手动触发失败",
        emptyTitle: "暂无挂起的止损单",
        emptyDescription: "当前所有订单已成交、已取消或未携带止损。",
        col: {
          fund: "基金",
          symbol: "标的",
          side: "方向",
          qty: "数量",
          stop: "止损价",
          current: "当前止损",
          trail: "Trail",
          highLow: "高 / 低水位",
          placed: "下单时间",
        },
      };

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const res = await apiGet<StopTriggerStatus>("/api/admin/stop-trigger/status");
      setStatus(res);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError]);

  useEffect(() => {
    void load();
  }, [load]);

  const tick = useCallback(async () => {
    setTicking(true);
    setError(null);
    try {
      await apiPost("/api/admin/stop-trigger/tick");
      await load();
    } catch (err) {
      setError(formatApiError(err, copy.tickFailed));
    } finally {
      setTicking(false);
    }
  }, [copy.tickFailed, load]);

  const trailLabel = (s: PendingStop): string => {
    if (s.trailPercent && s.trailPercent !== 0) return `${(s.trailPercent * 100).toFixed(2)}%`;
    if (s.trailAmount && s.trailAmount !== 0) return s.trailAmount.toFixed(4);
    return "—";
  };

  const pendingStops = useMemo(() => status?.pendingStops ?? [], [status]);

  return (
    <section className="rounded-2xl border border-amber-200 bg-amber-50 px-5 py-5 shadow-sm dark:border-amber-900/40 dark:bg-amber-950/20">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <div className="flex items-center gap-2">
            <h2 className="text-lg font-semibold text-amber-900 dark:text-amber-200">{copy.title}</h2>
            <span
              className={`rounded-full px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide ${
                status?.enabled
                  ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-200"
                  : "bg-gray-200 text-gray-600 dark:bg-slate-700 dark:text-slate-300"
              }`}
            >
              {status?.enabled ? copy.statusEnabled : copy.statusDisabled}
            </span>
          </div>
          <p className="mt-1 text-sm text-amber-800 dark:text-amber-300">{copy.subtitle}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading || ticking}
            className="rounded-lg border border-amber-300 bg-white px-3 py-1.5 text-xs font-medium text-amber-900 hover:bg-amber-100 disabled:cursor-not-allowed disabled:opacity-60 dark:border-amber-700 dark:bg-slate-900 dark:text-amber-200 dark:hover:bg-slate-800"
          >
            {copy.refresh}
          </button>
          <button
            type="button"
            onClick={() => void tick()}
            disabled={loading || ticking || !status?.enabled}
            className="rounded-lg bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {ticking ? copy.ticking : copy.forceTick}
          </button>
        </div>
      </div>

      {error ? (
        <p className="mt-3 rounded-lg border border-red-300 bg-red-50 px-3 py-2 text-xs text-red-700 dark:border-red-800 dark:bg-red-950/30 dark:text-red-300">
          {error}
        </p>
      ) : null}

      {status?.poller ? (
        <dl className="mt-4 grid grid-cols-2 gap-3 sm:grid-cols-5">
          <Stat label={copy.kPending} value={String(status.pendingCount ?? 0)} />
          <Stat label={copy.kTotalRuns} value={String(status.poller.totalRuns ?? 0)} />
          <Stat label={copy.kTotalTriggers} value={String(status.poller.totalTriggers ?? 0)} />
          <Stat
            label={copy.kInterval}
            value={status.poller.intervalMs ? `${(status.poller.intervalMs / 1000).toFixed(1)}s` : "—"}
          />
          <Stat
            label={copy.kLastRun}
            value={status.poller.lastRanAt ? new Date(status.poller.lastRanAt).toLocaleTimeString() : "—"}
          />
        </dl>
      ) : null}

      <div className="mt-4">
        {pendingStops.length === 0 ? (
          <div className="rounded-lg border border-dashed border-amber-300 bg-white px-4 py-6 text-center text-sm text-amber-800 dark:border-amber-800 dark:bg-slate-900 dark:text-amber-300">
            <p className="font-medium">{copy.emptyTitle}</p>
            <p className="mt-1 text-xs">{copy.emptyDescription}</p>
          </div>
        ) : (
          <div className="overflow-x-auto rounded-lg border border-amber-200 bg-white dark:border-amber-900/40 dark:bg-slate-900">
            <table className="min-w-full divide-y divide-amber-100 dark:divide-amber-900/40">
              <thead className="bg-amber-50 text-xs uppercase tracking-wide text-amber-800 dark:bg-amber-950/40 dark:text-amber-200">
                <tr>
                  <th className="px-3 py-2 text-left">{copy.col.fund}</th>
                  <th className="px-3 py-2 text-left">{copy.col.symbol}</th>
                  <th className="px-3 py-2 text-left">{copy.col.side}</th>
                  <th className="px-3 py-2 text-right">{copy.col.qty}</th>
                  <th className="px-3 py-2 text-right">{copy.col.stop}</th>
                  <th className="px-3 py-2 text-right">{copy.col.current}</th>
                  <th className="px-3 py-2 text-right">{copy.col.trail}</th>
                  <th className="px-3 py-2 text-right">{copy.col.highLow}</th>
                  <th className="px-3 py-2 text-left">{copy.col.placed}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-amber-100 text-sm dark:divide-amber-900/40">
                {pendingStops.map((s) => (
                  <tr key={s.brokerOrderId}>
                    <td className="px-3 py-2 font-mono text-xs text-gray-600 dark:text-slate-300">
                      {s.fundId.slice(0, 8)}
                    </td>
                    <td className="px-3 py-2 font-medium text-gray-900 dark:text-slate-100">
                      {s.symbol}
                    </td>
                    <td className="px-3 py-2">
                      <span
                        className={`rounded-full px-2 py-0.5 text-[11px] font-medium ${
                          s.side.toLowerCase() === "buy"
                            ? "bg-emerald-100 text-emerald-700 dark:bg-emerald-900/40 dark:text-emerald-200"
                            : "bg-red-100 text-red-700 dark:bg-red-900/40 dark:text-red-200"
                        }`}
                      >
                        {s.side}
                      </span>
                    </td>
                    <td className="px-3 py-2 text-right text-gray-700 dark:text-slate-200">
                      {s.quantity.toLocaleString()}
                    </td>
                    <td className="px-3 py-2 text-right text-gray-700 dark:text-slate-200">
                      {s.stopPrice.toFixed(4)}
                    </td>
                    <td className="px-3 py-2 text-right text-gray-700 dark:text-slate-200">
                      {s.currentStopPrice.toFixed(4)}
                    </td>
                    <td className="px-3 py-2 text-right text-gray-700 dark:text-slate-200">
                      {trailLabel(s)}
                    </td>
                    <td className="px-3 py-2 text-right text-xs text-gray-500 dark:text-slate-400">
                      {s.trailingHighWater ? `H ${s.trailingHighWater.toFixed(2)}` : ""}
                      {s.trailingHighWater && s.trailingLowWater ? " · " : ""}
                      {s.trailingLowWater ? `L ${s.trailingLowWater.toFixed(2)}` : ""}
                    </td>
                    <td className="px-3 py-2 text-xs text-gray-500 dark:text-slate-400">
                      {new Date(s.placedAt).toLocaleString()}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </section>
  );
};

const Stat: React.FC<{ label: string; value: string }> = ({ label, value }) => (
  <div className="rounded-lg border border-amber-200 bg-white px-3 py-2 dark:border-amber-900/40 dark:bg-slate-900">
    <div className="text-[10px] uppercase tracking-wide text-amber-700 dark:text-amber-300">{label}</div>
    <div className="text-sm font-semibold text-amber-900 dark:text-amber-100">{value}</div>
  </div>
);

export default AdminStopTriggerSection;
