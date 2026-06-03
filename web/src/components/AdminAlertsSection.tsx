// AdminAlertsSection — Sprint 12.3 inspection + acknowledgement UI
// for alertmanager-ingested events. Pairs with admin_alerts.go and
// the alerts.yml rules under /prometheus.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  acknowledgeAdminAlert,
  fetchAdminAlerts,
  formatApiError,
  type AdminAlertEvent,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  statusFilterLabel: string;
  statusAll: string;
  statusFiring: string;
  statusResolved: string;
  refresh: string;
  loading: string;
  emptyMessage: string;
  errorPrefix: string;
  firingCountLabel: string;
  criticalCountLabel: string;
  unackedCountLabel: string;
  colReceived: string;
  colSeverity: string;
  colName: string;
  colComponent: string;
  colStatus: string;
  colSummary: string;
  colAcked: string;
  colActions: string;
  ack: string;
  acking: string;
  ackedAt: (who: string, when: string) => string;
  ackPromptTitle: string;
  ackPromptPlaceholder: string;
  ackPromptConfirm: string;
  ackPromptCancel: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "告警面板",
    subtitle: "Alertmanager 推送的最近事件。点击行后可查看标签 / 注解；可对未确认告警标记 ack。",
    statusFilterLabel: "状态",
    statusAll: "全部",
    statusFiring: "Firing",
    statusResolved: "已恢复",
    refresh: "刷新",
    loading: "加载中…",
    emptyMessage: "暂无事件。",
    errorPrefix: "加载失败",
    firingCountLabel: "正在触发",
    criticalCountLabel: "Critical",
    unackedCountLabel: "未确认",
    colReceived: "接收时间",
    colSeverity: "严重度",
    colName: "告警",
    colComponent: "组件",
    colStatus: "状态",
    colSummary: "概要",
    colAcked: "确认",
    colActions: "操作",
    ack: "标记已知",
    acking: "提交中…",
    ackedAt: (who, when) => `已确认 · ${who.slice(0, 6)} · ${when}`,
    ackPromptTitle: "确认告警",
    ackPromptPlaceholder: "备注（可选，用于事故回顾）",
    ackPromptConfirm: "确认",
    ackPromptCancel: "取消",
  },
  "en-US": {
    title: "Alerts dashboard",
    subtitle: "Recent events pushed by alertmanager. Click a row for labels / annotations. Unacknowledged alerts can be acked here.",
    statusFilterLabel: "Status",
    statusAll: "All",
    statusFiring: "Firing",
    statusResolved: "Resolved",
    refresh: "Refresh",
    loading: "Loading…",
    emptyMessage: "No events.",
    errorPrefix: "Load failed",
    firingCountLabel: "Firing",
    criticalCountLabel: "Critical",
    unackedCountLabel: "Unacked",
    colReceived: "Received",
    colSeverity: "Severity",
    colName: "Alert",
    colComponent: "Component",
    colStatus: "Status",
    colSummary: "Summary",
    colAcked: "Ack",
    colActions: "Actions",
    ack: "Acknowledge",
    acking: "Submitting…",
    ackedAt: (who, when) => `Acked · ${who.slice(0, 6)} · ${when}`,
    ackPromptTitle: "Acknowledge alert",
    ackPromptPlaceholder: "Optional note (used in incident retro)",
    ackPromptConfirm: "Confirm",
    ackPromptCancel: "Cancel",
  },
};

function severityClass(sev: string): string {
  switch (sev) {
    case "critical":
      return "bg-rose-500/20 text-rose-200 border-rose-500/40";
    case "warning":
      return "bg-amber-500/20 text-amber-200 border-amber-500/40";
    case "info":
      return "bg-sky-500/20 text-sky-200 border-sky-500/40";
    default:
      return "bg-zinc-700/40 text-zinc-300 border-zinc-600";
  }
}

function statusClass(status: string): string {
  return status === "firing"
    ? "bg-rose-500/15 text-rose-200"
    : "bg-emerald-500/15 text-emerald-200";
}

function formatTime(s: string): string {
  if (!s) return "";
  return s.slice(0, 19).replace("T", " ");
}

export function AdminAlertsSection({ language }: Props) {
  const t = messages[language];
  const [status, setStatus] = useState<"" | "firing" | "resolved">("firing");
  const [events, setEvents] = useState<AdminAlertEvent[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [expandedId, setExpandedId] = useState<string | null>(null);
  const [ackingId, setAckingId] = useState<string | null>(null);
  const [ackNote, setAckNote] = useState("");
  const [ackBusy, setAckBusy] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchAdminAlerts({ status: status || undefined, limit: 200 });
      setEvents(resp.events ?? []);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [status, t.errorPrefix]);

  useEffect(() => {
    reload();
  }, [reload]);

  const counts = useMemo(() => {
    let firing = 0;
    let critical = 0;
    let unacked = 0;
    for (const ev of events) {
      if (ev.status === "firing") firing++;
      if (ev.severity === "critical") critical++;
      if (!ev.acknowledgedBy) unacked++;
    }
    return { firing, critical, unacked };
  }, [events]);

  const onAck = async (id: string) => {
    setAckBusy(true);
    try {
      await acknowledgeAdminAlert(id, { note: ackNote });
      setAckingId(null);
      setAckNote("");
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setAckBusy(false);
    }
  };

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <div className="flex items-center gap-3 text-sm text-zinc-300">
          <label className="flex items-center gap-2">
            {t.statusFilterLabel}
            <select
              value={status}
              onChange={(e) => setStatus(e.target.value as "" | "firing" | "resolved")}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
            >
              <option value="">{t.statusAll}</option>
              <option value="firing">{t.statusFiring}</option>
              <option value="resolved">{t.statusResolved}</option>
            </select>
          </label>
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
          {t.errorPrefix}: {error}
        </div>
      )}

      <div className="mt-6 grid grid-cols-1 gap-3 md:grid-cols-3">
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.firingCountLabel}</div>
          <div className={`mt-1 text-2xl font-semibold ${counts.firing > 0 ? "text-rose-300" : "text-emerald-300"}`}>
            {counts.firing}
          </div>
        </div>
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.criticalCountLabel}</div>
          <div className={`mt-1 text-2xl font-semibold ${counts.critical > 0 ? "text-rose-300" : "text-zinc-100"}`}>
            {counts.critical}
          </div>
        </div>
        <div className="rounded-lg border border-zinc-700 bg-zinc-800/60 p-4">
          <div className="text-xs uppercase text-zinc-400">{t.unackedCountLabel}</div>
          <div className={`mt-1 text-2xl font-semibold ${counts.unacked > 0 ? "text-amber-300" : "text-zinc-100"}`}>
            {counts.unacked}
          </div>
        </div>
      </div>

      <div className="mt-6 overflow-x-auto rounded-lg border border-zinc-700 bg-zinc-900/30">
        {loading ? (
          <p className="p-4 text-sm text-zinc-400">{t.loading}</p>
        ) : events.length === 0 ? (
          <p className="p-4 text-sm text-zinc-400">{t.emptyMessage}</p>
        ) : (
          <table className="w-full text-xs text-zinc-200">
            <thead>
              <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                <th className="px-2 py-1 text-left">{t.colReceived}</th>
                <th className="px-2 py-1 text-left">{t.colSeverity}</th>
                <th className="px-2 py-1 text-left">{t.colName}</th>
                <th className="px-2 py-1 text-left">{t.colComponent}</th>
                <th className="px-2 py-1 text-left">{t.colStatus}</th>
                <th className="px-2 py-1 text-left">{t.colSummary}</th>
                <th className="px-2 py-1 text-left">{t.colAcked}</th>
                <th className="px-2 py-1 text-left">{t.colActions}</th>
              </tr>
            </thead>
            <tbody>
              {events.map((ev) => {
                const isOpen = expandedId === ev.id;
                return (
                  <>
                    <tr
                      key={ev.id}
                      className="cursor-pointer border-b border-zinc-800/40 hover:bg-zinc-800/30"
                      onClick={() => setExpandedId(isOpen ? null : ev.id)}
                    >
                      <td className="whitespace-nowrap px-2 py-1 text-zinc-400">{formatTime(ev.receivedAt)}</td>
                      <td className="px-2 py-1">
                        <span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${severityClass(ev.severity)}`}>
                          {ev.severity}
                        </span>
                      </td>
                      <td className="px-2 py-1 font-medium">{ev.alertName}</td>
                      <td className="px-2 py-1 text-zinc-400">{ev.component || "-"}</td>
                      <td className="px-2 py-1">
                        <span className={`rounded px-1.5 py-0.5 text-[10px] uppercase ${statusClass(ev.status)}`}>
                          {ev.status}
                        </span>
                      </td>
                      <td className="px-2 py-1 text-zinc-300">{ev.summary || "-"}</td>
                      <td className="px-2 py-1 text-zinc-400">
                        {ev.acknowledgedBy && ev.acknowledgedAt
                          ? t.ackedAt(ev.acknowledgedBy, formatTime(ev.acknowledgedAt))
                          : "-"}
                      </td>
                      <td className="px-2 py-1">
                        {!ev.acknowledgedBy && (
                          <button
                            type="button"
                            onClick={(e) => {
                              e.stopPropagation();
                              setAckingId(ev.id);
                              setAckNote("");
                            }}
                            className="rounded border border-amber-500/40 bg-amber-500/15 px-2 py-0.5 text-[10px] text-amber-200 hover:bg-amber-500/25"
                          >
                            {t.ack}
                          </button>
                        )}
                      </td>
                    </tr>
                    {isOpen && (
                      <tr className="border-b border-zinc-800/40 bg-zinc-900/50">
                        <td colSpan={8} className="px-3 py-3">
                          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
                            <div>
                              <div className="text-[10px] uppercase text-zinc-500">labels</div>
                              <pre className="mt-1 overflow-x-auto rounded bg-zinc-950/70 p-2 text-[11px] text-zinc-200">
                                {JSON.stringify(ev.labels ?? {}, null, 2)}
                              </pre>
                            </div>
                            <div>
                              <div className="text-[10px] uppercase text-zinc-500">annotations</div>
                              <pre className="mt-1 overflow-x-auto rounded bg-zinc-950/70 p-2 text-[11px] text-zinc-200">
                                {JSON.stringify(ev.annotations ?? {}, null, 2)}
                              </pre>
                            </div>
                          </div>
                          {ev.description && (
                            <p className="mt-3 whitespace-pre-line text-[11px] text-zinc-300">{ev.description}</p>
                          )}
                          {ev.acknowledgementNote && (
                            <p className="mt-3 rounded border border-emerald-500/30 bg-emerald-500/10 p-2 text-[11px] text-emerald-200">
                              {ev.acknowledgementNote}
                            </p>
                          )}
                        </td>
                      </tr>
                    )}
                  </>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {ackingId && (
        <div
          className="fixed inset-0 z-40 flex items-center justify-center bg-black/50"
          onClick={() => !ackBusy && setAckingId(null)}
        >
          <div
            className="w-full max-w-md rounded-lg border border-zinc-700 bg-zinc-900 p-4"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="text-sm font-semibold text-zinc-100">{t.ackPromptTitle}</h3>
            <textarea
              value={ackNote}
              onChange={(e) => setAckNote(e.target.value)}
              placeholder={t.ackPromptPlaceholder}
              className="mt-3 h-24 w-full rounded-md border border-zinc-700 bg-zinc-800 p-2 text-sm text-zinc-100"
            />
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                onClick={() => setAckingId(null)}
                disabled={ackBusy}
                className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 disabled:opacity-50"
              >
                {t.ackPromptCancel}
              </button>
              <button
                type="button"
                onClick={() => onAck(ackingId)}
                disabled={ackBusy}
                className="rounded-md bg-amber-500 px-3 py-1 text-sm font-medium text-zinc-900 hover:bg-amber-400 disabled:opacity-50"
              >
                {ackBusy ? t.acking : t.ackPromptConfirm}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}
