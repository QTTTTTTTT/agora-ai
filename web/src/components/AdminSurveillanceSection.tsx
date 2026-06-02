// AdminSurveillanceSection — P1-7 admin web component.
//
// What it does
//
//   - Lists recent surveillance events from
//     /api/admin/surveillance/events and renders a table sorted by
//     severity and detection time. Filter buttons let the operator
//     focus on `open` items by default and still drill into closed
//     history.
//   - Click-to-expand a row shows the rule-specific metadata, the
//     contributing trade IDs, and the detection window.
//   - Per-event actions transition the lifecycle (start review /
//     clear / escalate / re-open) with an optional note that lands
//     on the audit chain.
//   - A small "Trigger scan" form fires an on-demand scan for the
//     given fund + trading day, useful for spot checks and demo.
//
// Why a separate component
//
// Surveillance and reconciliation are both compliance views, but
// they have different lifecycles (recon: open → resolved; surveillance:
// open → cleared/escalated) and different rule vocabularies. Keeping
// them separate keeps each one's filter UI and severity logic
// self-contained.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ApiError,
  formatApiError,
  listAdminSurveillanceEvents,
  listAdminSurveillanceRuns,
  reviewAdminSurveillanceEvent,
  triggerAdminSurveillanceScan,
  type SurveillanceEvent,
  type SurveillanceRun,
  type SurveillanceEventStatus,
  type SurveillanceRuleCode,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

type Severity = "critical" | "warning" | "info";

const REFRESH_MS = 30_000;

interface Messages {
  title: string;
  subtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  error: string;
  detectedAt: string;
  rule: string;
  severity: string;
  status: string;
  symbol: string;
  summary: string;
  triggerScan: string;
  triggerScanDialog: string;
  fundId: string;
  fundIdPlaceholder: string;
  asOf: string;
  sessionClose: string;
  triggerSubmit: string;
  triggerSubmitting: string;
  triggerSuccess: string;
  triggerError: string;
  severityCritical: string;
  severityWarning: string;
  severityInfo: string;
  statusOpen: string;
  statusReviewing: string;
  statusCleared: string;
  statusEscalated: string;
  triggerSourceManual: string;
  triggerSourceScheduled: string;
  ruleWashTrade: string;
  ruleMarkingClose: string;
  ruleSelfTradePair: string;
  ruleRapidFire: string;
  ruleLayering: string;
  actionAcknowledge: string;
  actionClear: string;
  actionEscalate: string;
  actionReopen: string;
  reviewDialog: string;
  reviewNote: string;
  reviewSubmit: string;
  reviewSubmitting: string;
  detailMetadata: string;
  detailTradeIDs: string;
  detailWindow: string;
  filterAll: string;
  filterOpen: string;
  filterCritical: string;
  runsTitle: string;
  runsTradeCount: string;
  runsEventCount: string;
  runsDuration: string;
  closeButton: string;
  cancelButton: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    title: "交易监控（Trade Surveillance）",
    subtitle:
      "每小时扫描当日成交，识别 wash trade / marking close / self-trade 等可疑模式；命中即写审计链，由合规复核 cleared / escalated。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无监控事件",
    error: "加载监控事件失败",
    detectedAt: "检测时间",
    rule: "规则",
    severity: "级别",
    status: "状态",
    symbol: "标的",
    summary: "说明",
    triggerScan: "手动触发扫描",
    triggerScanDialog: "手动触发监控扫描",
    fundId: "基金 ID",
    fundIdPlaceholder: "请输入要扫描的基金 UUID",
    asOf: "业务日（YYYY-MM-DD，默认今日 UTC）",
    sessionClose: "收盘时间 UTC（HH:MM，默认 20:00）",
    triggerSubmit: "提交扫描",
    triggerSubmitting: "扫描中…",
    triggerSuccess: "扫描已完成",
    triggerError: "扫描失败",
    severityCritical: "严重",
    severityWarning: "警告",
    severityInfo: "提示",
    statusOpen: "待复核",
    statusReviewing: "复核中",
    statusCleared: "已澄清",
    statusEscalated: "已上报",
    triggerSourceManual: "手动",
    triggerSourceScheduled: "定时调度",
    ruleWashTrade: "洗售（wash trade）",
    ruleMarkingClose: "尾盘 marking close",
    ruleSelfTradePair: "自成交对（self-cross）",
    ruleRapidFire: "快速反向交易",
    ruleLayering: "可疑分层下单",
    actionAcknowledge: "开始复核",
    actionClear: "澄清结案",
    actionEscalate: "上报合规",
    actionReopen: "重开复核",
    reviewDialog: "处理监控事件",
    reviewNote: "复核备注（写明依据 / 上报理由，便于审计）",
    reviewSubmit: "确定",
    reviewSubmitting: "处理中…",
    detailMetadata: "触发证据",
    detailTradeIDs: "相关成交",
    detailWindow: "检测窗口",
    filterAll: "全部",
    filterOpen: "待复核",
    filterCritical: "仅严重",
    runsTitle: "最近扫描运行",
    runsTradeCount: "扫描成交数",
    runsEventCount: "检出事件数",
    runsDuration: "耗时",
    closeButton: "关闭",
    cancelButton: "取消",
  },
  "en-US": {
    title: "Trade surveillance",
    subtitle:
      "Hourly scan of intraday fills for wash trades, marking-the-close, and self-cross patterns. Hits land on the audit chain — compliance reviews, clears, or escalates from this panel.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No surveillance events yet.",
    error: "Failed to load surveillance events",
    detectedAt: "Detected",
    rule: "Rule",
    severity: "Severity",
    status: "Status",
    symbol: "Symbol",
    summary: "Summary",
    triggerScan: "Trigger scan",
    triggerScanDialog: "Trigger surveillance scan",
    fundId: "Fund ID",
    fundIdPlaceholder: "UUID of the fund to scan",
    asOf: "As-of (YYYY-MM-DD, defaults to today UTC)",
    sessionClose: "Session close UTC (HH:MM, defaults to 20:00)",
    triggerSubmit: "Run scan",
    triggerSubmitting: "Scanning…",
    triggerSuccess: "Scan completed.",
    triggerError: "Scan failed",
    severityCritical: "critical",
    severityWarning: "warning",
    severityInfo: "info",
    statusOpen: "open",
    statusReviewing: "reviewing",
    statusCleared: "cleared",
    statusEscalated: "escalated",
    triggerSourceManual: "manual",
    triggerSourceScheduled: "scheduled",
    ruleWashTrade: "Wash trade",
    ruleMarkingClose: "Marking the close",
    ruleSelfTradePair: "Self-trade pair",
    ruleRapidFire: "Rapid-fire reversal",
    ruleLayering: "Layering suspect",
    actionAcknowledge: "Start review",
    actionClear: "Clear",
    actionEscalate: "Escalate",
    actionReopen: "Re-open",
    reviewDialog: "Review surveillance event",
    reviewNote: "Review note (recorded on the audit chain)",
    reviewSubmit: "Confirm",
    reviewSubmitting: "Submitting…",
    detailMetadata: "Detection evidence",
    detailTradeIDs: "Contributing trades",
    detailWindow: "Detection window",
    filterAll: "All",
    filterOpen: "Open",
    filterCritical: "Critical only",
    runsTitle: "Recent scan runs",
    runsTradeCount: "Trades scanned",
    runsEventCount: "Events detected",
    runsDuration: "Duration",
    closeButton: "Close",
    cancelButton: "Cancel",
  },
};

interface Props {
  language?: Language;
}

const ruleLabel = (m: Messages, code: SurveillanceRuleCode): string => {
  switch (code) {
    case "wash_trade":
      return m.ruleWashTrade;
    case "marking_close":
      return m.ruleMarkingClose;
    case "self_trade_pair":
      return m.ruleSelfTradePair;
    case "rapid_fire_reversal":
      return m.ruleRapidFire;
    case "layering_suspect":
      return m.ruleLayering;
    default:
      return code;
  }
};

const severityLabel = (m: Messages, sev: Severity): string =>
  sev === "critical" ? m.severityCritical : sev === "warning" ? m.severityWarning : m.severityInfo;

const statusLabel = (m: Messages, status: SurveillanceEventStatus): string => {
  switch (status) {
    case "open":
      return m.statusOpen;
    case "reviewing":
      return m.statusReviewing;
    case "cleared":
      return m.statusCleared;
    case "escalated":
      return m.statusEscalated;
    default:
      return status;
  }
};

// severityClass returns a tailwind-ish className suffix the
// caller composes into their own styling. Keep as a string so
// we can swap classes without touching component logic.
const severityClass = (sev: Severity): string => {
  if (sev === "critical") return "text-red-700 font-semibold";
  if (sev === "warning") return "text-amber-700";
  return "text-slate-600";
};

const formatTimestamp = (iso: string | undefined): string => {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
};

// AdminSurveillanceSection is mounted alongside the recon and FX
// admin sections on the global Admin page.
export function AdminSurveillanceSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(() => messages[language] ?? messages["zh-CN"], [language]);

  const [events, setEvents] = useState<SurveillanceEvent[]>([]);
  const [runs, setRuns] = useState<SurveillanceRun[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [filter, setFilter] = useState<"all" | "open" | "critical">("open");
  const [expandedID, setExpandedID] = useState<string | null>(null);
  const [reviewing, setReviewing] = useState<{ event: SurveillanceEvent; status: SurveillanceEventStatus } | null>(null);
  const [reviewNote, setReviewNote] = useState("");
  const [reviewSubmitting, setReviewSubmitting] = useState(false);
  const [reviewError, setReviewError] = useState<string | null>(null);
  const [scanOpen, setScanOpen] = useState(false);
  const [scanFundID, setScanFundID] = useState("");
  const [scanAsOf, setScanAsOf] = useState("");
  const [scanClose, setScanClose] = useState("");
  const [scanSubmitting, setScanSubmitting] = useState(false);
  const [scanError, setScanError] = useState<string | null>(null);
  const [scanSuccess, setScanSuccess] = useState<string | null>(null);

  // Fetcher: events list is the primary read; runs roll-up is a
  // small secondary query that helps the operator see whether the
  // scheduler is alive.
  const fetchAll = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [evResp, runsResp] = await Promise.all([
        listAdminSurveillanceEvents({
          status: filter === "open" ? "open" : undefined,
          severity: filter === "critical" ? "critical" : undefined,
          limit: 100,
        }),
        listAdminSurveillanceRuns({ limit: 10 }),
      ]);
      setEvents(evResp.events ?? []);
      setRuns(runsResp.runs ?? []);
    } catch (err) {
      setError(formatApiError(err, m.error));
    } finally {
      setLoading(false);
    }
  }, [filter, m.error]);

  useEffect(() => {
    fetchAll().catch(() => {});
    const interval = setInterval(() => {
      fetchAll().catch(() => {});
    }, REFRESH_MS);
    return () => clearInterval(interval);
  }, [fetchAll]);

  const openReviewDialog = (event: SurveillanceEvent, target: SurveillanceEventStatus) => {
    setReviewing({ event, status: target });
    setReviewNote("");
    setReviewError(null);
  };

  const submitReview = async () => {
    if (!reviewing) return;
    setReviewSubmitting(true);
    setReviewError(null);
    try {
      await reviewAdminSurveillanceEvent(reviewing.event.id, reviewing.status, reviewNote.trim() || undefined);
      setReviewing(null);
      await fetchAll();
    } catch (err) {
      if (err instanceof ApiError) {
        setReviewError(formatApiError(err, m.error));
      } else {
        setReviewError(String(err));
      }
    } finally {
      setReviewSubmitting(false);
    }
  };

  const submitScan = async () => {
    setScanSubmitting(true);
    setScanError(null);
    setScanSuccess(null);
    try {
      const resp = await triggerAdminSurveillanceScan({
        fund_id: scanFundID.trim(),
        as_of_date: scanAsOf.trim() || undefined,
        session_close_utc: scanClose.trim() || undefined,
      });
      setScanSuccess(`${m.triggerSuccess} (${resp.events.length})`);
      await fetchAll();
    } catch (err) {
      setScanError(formatApiError(err, m.triggerError));
    } finally {
      setScanSubmitting(false);
    }
  };

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4">
      <header className="flex items-start justify-between gap-4 pb-3">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">{m.title}</h2>
          <p className="text-sm text-slate-500">{m.subtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={() => setScanOpen(true)}
            className="rounded border border-slate-300 px-3 py-1 text-sm text-slate-700 hover:bg-slate-50"
          >
            {m.triggerScan}
          </button>
          <button
            type="button"
            onClick={() => fetchAll()}
            className="rounded bg-slate-900 px-3 py-1 text-sm text-white hover:bg-slate-800"
          >
            {m.refresh}
          </button>
        </div>
      </header>

      <div className="flex items-center gap-2 pb-2 text-xs">
        {(["all", "open", "critical"] as const).map((opt) => (
          <button
            key={opt}
            type="button"
            onClick={() => setFilter(opt)}
            className={`rounded px-2 py-1 ${
              filter === opt ? "bg-slate-900 text-white" : "bg-slate-100 text-slate-700"
            }`}
          >
            {opt === "all" ? m.filterAll : opt === "open" ? m.filterOpen : m.filterCritical}
          </button>
        ))}
      </div>

      {error && (
        <div className="mb-2 rounded border border-red-300 bg-red-50 p-2 text-sm text-red-700">
          {m.error}: {error}
        </div>
      )}

      <div className="overflow-x-auto">
        <table className="min-w-full divide-y divide-slate-200 text-sm">
          <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
            <tr>
              <th className="px-3 py-2">{m.detectedAt}</th>
              <th className="px-3 py-2">{m.rule}</th>
              <th className="px-3 py-2">{m.severity}</th>
              <th className="px-3 py-2">{m.symbol}</th>
              <th className="px-3 py-2">{m.summary}</th>
              <th className="px-3 py-2">{m.status}</th>
              <th className="px-3 py-2"></th>
            </tr>
          </thead>
          <tbody className="divide-y divide-slate-200 text-slate-700">
            {events.length === 0 && !loading && (
              <tr>
                <td colSpan={7} className="px-3 py-4 text-center text-slate-400">
                  {m.empty}
                </td>
              </tr>
            )}
            {loading && events.length === 0 && (
              <tr>
                <td colSpan={7} className="px-3 py-4 text-center text-slate-400">
                  {m.loading}
                </td>
              </tr>
            )}
            {events.map((ev) => (
              <SurveillanceEventRow
                key={ev.id}
                event={ev}
                expanded={expandedID === ev.id}
                onToggle={() => setExpandedID(expandedID === ev.id ? null : ev.id)}
                onAction={(target) => openReviewDialog(ev, target)}
                m={m}
              />
            ))}
          </tbody>
        </table>
      </div>

      {runs.length > 0 && (
        <div className="mt-4 border-t border-slate-200 pt-3">
          <h3 className="text-sm font-semibold text-slate-800">{m.runsTitle}</h3>
          <table className="mt-2 min-w-full divide-y divide-slate-100 text-xs">
            <thead className="text-slate-500">
              <tr>
                <th className="px-2 py-1 text-left">{m.detectedAt}</th>
                <th className="px-2 py-1 text-left">Trigger</th>
                <th className="px-2 py-1 text-right">{m.runsTradeCount}</th>
                <th className="px-2 py-1 text-right">{m.runsEventCount}</th>
                <th className="px-2 py-1 text-right">{m.runsDuration}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-slate-100 text-slate-700">
              {runs.map((run) => (
                <tr key={run.id}>
                  <td className="px-2 py-1">{formatTimestamp(run.started_at)}</td>
                  <td className="px-2 py-1">
                    {run.trigger_source === "manual" ? m.triggerSourceManual : m.triggerSourceScheduled}
                  </td>
                  <td className="px-2 py-1 text-right tabular-nums">{run.trade_count}</td>
                  <td className="px-2 py-1 text-right tabular-nums">
                    {run.event_count_total}
                    {run.event_count_critical > 0 && (
                      <span className="ml-1 text-red-600">({run.event_count_critical})</span>
                    )}
                  </td>
                  <td className="px-2 py-1 text-right tabular-nums">{run.duration_ms} ms</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      {/* Review dialog */}
      {reviewing && (
        <Dialog onClose={() => setReviewing(null)} title={m.reviewDialog}>
          <p className="text-sm text-slate-600">{reviewing.event.summary}</p>
          <p className="mt-2 text-xs text-slate-500">
            {ruleLabel(m, reviewing.event.rule_code)} · {reviewing.event.fund_id} · → {statusLabel(m, reviewing.status)}
          </p>
          <textarea
            value={reviewNote}
            onChange={(e) => setReviewNote(e.target.value)}
            placeholder={m.reviewNote}
            className="mt-3 w-full rounded border border-slate-300 p-2 text-sm"
            rows={3}
          />
          {reviewError && <p className="mt-2 text-sm text-red-600">{reviewError}</p>}
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              className="rounded border border-slate-300 px-3 py-1 text-sm"
              onClick={() => setReviewing(null)}
              disabled={reviewSubmitting}
            >
              {m.cancelButton}
            </button>
            <button
              type="button"
              className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
              onClick={() => submitReview()}
              disabled={reviewSubmitting}
            >
              {reviewSubmitting ? m.reviewSubmitting : m.reviewSubmit}
            </button>
          </div>
        </Dialog>
      )}

      {/* Trigger scan dialog */}
      {scanOpen && (
        <Dialog onClose={() => setScanOpen(false)} title={m.triggerScanDialog}>
          <label className="block text-sm">
            <span className="block text-xs font-semibold text-slate-600">{m.fundId}</span>
            <input
              type="text"
              value={scanFundID}
              onChange={(e) => setScanFundID(e.target.value)}
              placeholder={m.fundIdPlaceholder}
              className="mt-1 w-full rounded border border-slate-300 p-2 text-sm"
            />
          </label>
          <label className="mt-3 block text-sm">
            <span className="block text-xs font-semibold text-slate-600">{m.asOf}</span>
            <input
              type="text"
              value={scanAsOf}
              onChange={(e) => setScanAsOf(e.target.value)}
              placeholder="2026-06-01"
              className="mt-1 w-full rounded border border-slate-300 p-2 text-sm"
            />
          </label>
          <label className="mt-3 block text-sm">
            <span className="block text-xs font-semibold text-slate-600">{m.sessionClose}</span>
            <input
              type="text"
              value={scanClose}
              onChange={(e) => setScanClose(e.target.value)}
              placeholder="20:00"
              className="mt-1 w-full rounded border border-slate-300 p-2 text-sm"
            />
          </label>
          {scanError && <p className="mt-2 text-sm text-red-600">{scanError}</p>}
          {scanSuccess && <p className="mt-2 text-sm text-green-700">{scanSuccess}</p>}
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              className="rounded border border-slate-300 px-3 py-1 text-sm"
              onClick={() => setScanOpen(false)}
              disabled={scanSubmitting}
            >
              {m.cancelButton}
            </button>
            <button
              type="button"
              className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
              onClick={() => submitScan()}
              disabled={scanSubmitting || !scanFundID.trim()}
            >
              {scanSubmitting ? m.triggerSubmitting : m.triggerSubmit}
            </button>
          </div>
        </Dialog>
      )}
    </section>
  );
}

interface RowProps {
  event: SurveillanceEvent;
  expanded: boolean;
  onToggle: () => void;
  onAction: (status: SurveillanceEventStatus) => void;
  m: Messages;
}

function SurveillanceEventRow({ event, expanded, onToggle, onAction, m }: RowProps) {
  return (
    <>
      <tr
        className="cursor-pointer hover:bg-slate-50"
        onClick={(e) => {
          // Action buttons stop the click from bubbling, so the
          // row only toggles when the user clicks the body.
          if ((e.target as HTMLElement).tagName !== "BUTTON") onToggle();
        }}
      >
        <td className="px-3 py-2 text-xs text-slate-500">{formatTimestamp(event.detected_at)}</td>
        <td className="px-3 py-2 text-sm">{ruleLabel(m, event.rule_code)}</td>
        <td className={`px-3 py-2 text-xs ${severityClass(event.severity as Severity)}`}>
          {severityLabel(m, event.severity as Severity)}
        </td>
        <td className="px-3 py-2 text-sm tabular-nums">{event.symbol ?? "—"}</td>
        <td className="px-3 py-2 text-xs text-slate-600">{event.summary}</td>
        <td className="px-3 py-2 text-xs">{statusLabel(m, event.status)}</td>
        <td className="px-3 py-2 text-right text-xs">
          {event.status === "open" && (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onAction("reviewing");
              }}
              className="text-blue-600 underline"
            >
              {m.actionAcknowledge}
            </button>
          )}
          {event.status === "reviewing" && (
            <span className="space-x-1">
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onAction("cleared");
                }}
                className="text-emerald-700 underline"
              >
                {m.actionClear}
              </button>
              <button
                type="button"
                onClick={(e) => {
                  e.stopPropagation();
                  onAction("escalated");
                }}
                className="text-red-700 underline"
              >
                {m.actionEscalate}
              </button>
            </span>
          )}
          {(event.status === "cleared" || event.status === "escalated") && (
            <button
              type="button"
              onClick={(e) => {
                e.stopPropagation();
                onAction("open");
              }}
              className="text-slate-500 underline"
            >
              {m.actionReopen}
            </button>
          )}
        </td>
      </tr>
      {expanded && (
        <tr className="bg-slate-50">
          <td colSpan={7} className="px-4 py-3 text-xs text-slate-700">
            <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
              <div>
                <div className="font-semibold text-slate-600">{m.detailWindow}</div>
                <div className="tabular-nums">
                  {formatTimestamp(event.window_start)} → {formatTimestamp(event.window_end)}
                </div>
              </div>
              <div>
                <div className="font-semibold text-slate-600">{m.detailTradeIDs}</div>
                <ul className="list-disc pl-4 font-mono text-[11px]">
                  {event.trade_ids.map((id) => (
                    <li key={id}>{id}</li>
                  ))}
                </ul>
              </div>
              <div>
                <div className="font-semibold text-slate-600">{m.detailMetadata}</div>
                <pre className="overflow-x-auto whitespace-pre-wrap rounded bg-white p-1 font-mono text-[11px]">
                  {JSON.stringify(event.metadata ?? {}, null, 2)}
                </pre>
              </div>
            </div>
            {event.review_note && (
              <div className="mt-2 text-xs">
                <span className="font-semibold text-slate-600">Review note:</span> {event.review_note}
              </div>
            )}
          </td>
        </tr>
      )}
    </>
  );
}

interface DialogProps {
  title: string;
  onClose: () => void;
  children: React.ReactNode;
}

function Dialog({ title, onClose, children }: DialogProps) {
  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
      onClick={onClose}
      role="dialog"
      aria-modal="true"
    >
      <div
        className="w-full max-w-md rounded-lg bg-white p-4 shadow-lg"
        onClick={(e) => e.stopPropagation()}
      >
        <h3 className="text-base font-semibold text-slate-900">{title}</h3>
        <div className="mt-2">{children}</div>
      </div>
    </div>
  );
}

export default AdminSurveillanceSection;
