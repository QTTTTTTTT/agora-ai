// AdminReconSection — P1-3 admin web component.
//
// What it does
//
//   - Lists recent reconciliation runs from
//     /api/admin/reconciliation/runs and renders a roll-up table
//     (date · trigger · break counts).
//   - Click-to-expand drills into one run and shows its breaks
//     ordered by severity. Each break exposes acknowledge /
//     resolve / ignore actions; the resolution note lands on the
//     audit chain.
//   - A small "Trigger run" form fires an on-demand mock-provider
//     run for ad-hoc inspection or demo. Real-broker statements
//     will arrive via CSV upload later — this is the wedge.
//
// Why a self-contained component
//
//   Recon is a sysadmin concern (per-fund or per-platform).
//   We mount it on the global Admin page next to AdminFXSection.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ApiError,
  formatApiError,
  listAdminReconRuns,
  getAdminReconRun,
  triggerAdminReconRun,
  resolveAdminReconBreak,
  type ReconciliationRun,
  type ReconciliationBreak,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

type Severity = "critical" | "warning" | "info";
type BreakStatus = "open" | "acknowledged" | "resolved" | "ignored";

const RUNS_REFRESH_MS = 30_000;

const messages: Record<
  Language,
  {
    title: string;
    subtitle: string;
    refresh: string;
    loading: string;
    empty: string;
    errorLoad: string;
    runDate: string;
    triggerSource: string;
    status: string;
    breakCount: string;
    breakCountCritical: string;
    breakCountWarning: string;
    breakCountInfo: string;
    severityCritical: string;
    severityWarning: string;
    severityInfo: string;
    statusOpen: string;
    statusAcknowledged: string;
    statusResolved: string;
    statusIgnored: string;
    statusCompleted: string;
    statusFailed: string;
    statusPending: string;
    triggerSourceManual: string;
    triggerSourceScheduled: string;
    triggerSourceReplay: string;
    triggerRunButton: string;
    triggerRunFundIdLabel: string;
    triggerRunFundIdPlaceholder: string;
    triggerRunDriftQtyLabel: string;
    triggerRunDriftCashLabel: string;
    triggerRunDriftPriceLabel: string;
    triggerRunSubmit: string;
    triggerRunSubmitting: string;
    triggerRunSuccess: string;
    breakActionAcknowledge: string;
    breakActionResolve: string;
    breakActionIgnore: string;
    breakActionReopen: string;
    breakResolveNoteLabel: string;
    breakResolveSubmit: string;
    breakResolveSubmitting: string;
    breakDrillTitle: string;
    breakDrillNoBreaks: string;
    breakInternalLabel: string;
    breakBrokerLabel: string;
    breakDiffLabel: string;
    breakDiffPctLabel: string;
    breakSymbolLabel: string;
    breakCurrencyLabel: string;
    breakDescriptionLabel: string;
    breakTypeLabel: string;
    expand: string;
    collapse: string;
  }
> = {
  "zh-CN": {
    title: "日终对账",
    subtitle:
      "系统每天自动按 mock 券商对账单与持仓 / 现金 / 成交进行 diff，差异（breaks）写入审计链。运维 acknowledge / resolve 后留痕。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "暂无对账运行记录",
    errorLoad: "加载对账记录失败",
    runDate: "业务日",
    triggerSource: "触发方式",
    status: "状态",
    breakCount: "差异数",
    breakCountCritical: "严重",
    breakCountWarning: "警告",
    breakCountInfo: "提示",
    severityCritical: "严重",
    severityWarning: "警告",
    severityInfo: "提示",
    statusOpen: "待处理",
    statusAcknowledged: "已确认",
    statusResolved: "已解决",
    statusIgnored: "已忽略",
    statusCompleted: "已完成",
    statusFailed: "失败",
    statusPending: "运行中",
    triggerSourceManual: "手动",
    triggerSourceScheduled: "日终",
    triggerSourceReplay: "重放",
    triggerRunButton: "手动触发对账",
    triggerRunFundIdLabel: "基金 ID",
    triggerRunFundIdPlaceholder: "输入基金 UUID",
    triggerRunDriftQtyLabel: "演示用：人为持仓数量偏移",
    triggerRunDriftCashLabel: "演示用：人为现金偏移",
    triggerRunDriftPriceLabel: "演示用：人为成交价格偏移",
    triggerRunSubmit: "提交",
    triggerRunSubmitting: "提交中…",
    triggerRunSuccess: "运行已完成",
    breakActionAcknowledge: "确认",
    breakActionResolve: "标记已解决",
    breakActionIgnore: "忽略",
    breakActionReopen: "重开",
    breakResolveNoteLabel: "备注（写明原因，便于审计回溯）",
    breakResolveSubmit: "确定",
    breakResolveSubmitting: "处理中…",
    breakDrillTitle: "差异明细",
    breakDrillNoBreaks: "本次运行无差异",
    breakInternalLabel: "内部值",
    breakBrokerLabel: "券商值",
    breakDiffLabel: "差值",
    breakDiffPctLabel: "差值 %",
    breakSymbolLabel: "标的",
    breakCurrencyLabel: "币种",
    breakDescriptionLabel: "说明",
    breakTypeLabel: "类型",
    expand: "展开",
    collapse: "收起",
  },
  "en-US": {
    title: "Daily reconciliation",
    subtitle:
      "Internal positions / cash / trades are diffed against a (mock) broker statement nightly. Breaks land on the audit chain — operators acknowledge or resolve from this panel.",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No reconciliation runs yet.",
    errorLoad: "Failed to load reconciliation runs",
    runDate: "As-of",
    triggerSource: "Trigger",
    status: "Status",
    breakCount: "Breaks",
    breakCountCritical: "Critical",
    breakCountWarning: "Warning",
    breakCountInfo: "Info",
    severityCritical: "critical",
    severityWarning: "warning",
    severityInfo: "info",
    statusOpen: "open",
    statusAcknowledged: "acknowledged",
    statusResolved: "resolved",
    statusIgnored: "ignored",
    statusCompleted: "completed",
    statusFailed: "failed",
    statusPending: "running",
    triggerSourceManual: "manual",
    triggerSourceScheduled: "scheduled",
    triggerSourceReplay: "replay",
    triggerRunButton: "Trigger run",
    triggerRunFundIdLabel: "Fund ID",
    triggerRunFundIdPlaceholder: "UUID of the fund",
    triggerRunDriftQtyLabel: "Demo: synthetic position drift",
    triggerRunDriftCashLabel: "Demo: synthetic cash drift",
    triggerRunDriftPriceLabel: "Demo: synthetic trade price drift",
    triggerRunSubmit: "Submit",
    triggerRunSubmitting: "Submitting…",
    triggerRunSuccess: "Run completed.",
    breakActionAcknowledge: "Acknowledge",
    breakActionResolve: "Mark resolved",
    breakActionIgnore: "Ignore",
    breakActionReopen: "Re-open",
    breakResolveNoteLabel: "Note (recorded on the audit chain)",
    breakResolveSubmit: "Confirm",
    breakResolveSubmitting: "Submitting…",
    breakDrillTitle: "Break details",
    breakDrillNoBreaks: "No breaks for this run.",
    breakInternalLabel: "Internal",
    breakBrokerLabel: "Broker",
    breakDiffLabel: "Diff",
    breakDiffPctLabel: "Diff %",
    breakSymbolLabel: "Symbol",
    breakCurrencyLabel: "Currency",
    breakDescriptionLabel: "Description",
    breakTypeLabel: "Type",
    expand: "Show",
    collapse: "Hide",
  },
};

interface AdminReconSectionProps {
  language?: Language;
}

export default function AdminReconSection({ language = "zh-CN" }: AdminReconSectionProps) {
  const t = messages[language];

  const [runs, setRuns] = useState<ReconciliationRun[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadError, setLoadError] = useState<string | null>(null);

  const [expandedRunID, setExpandedRunID] = useState<string | null>(null);
  const [drilledBreaks, setDrilledBreaks] = useState<Record<string, ReconciliationBreak[]>>({});
  const [drillLoading, setDrillLoading] = useState<string | null>(null);

  const [resolveTarget, setResolveTarget] = useState<{
    breakID: string;
    status: BreakStatus;
  } | null>(null);
  const [resolveNote, setResolveNote] = useState("");
  const [resolveSubmitting, setResolveSubmitting] = useState(false);
  const [resolveError, setResolveError] = useState<string | null>(null);

  const [triggerFundID, setTriggerFundID] = useState("");
  const [triggerDriftQty, setTriggerDriftQty] = useState("");
  const [triggerDriftCash, setTriggerDriftCash] = useState("");
  const [triggerDriftPrice, setTriggerDriftPrice] = useState("");
  const [triggerSubmitting, setTriggerSubmitting] = useState(false);
  const [triggerError, setTriggerError] = useState<string | null>(null);
  const [triggerSuccessAt, setTriggerSuccessAt] = useState<string | null>(null);

  const refresh = useCallback(async () => {
    try {
      setLoadError(null);
      const res = await listAdminReconRuns({ limit: 50 });
      setRuns(res.runs || []);
    } catch (err) {
      setLoadError(formatApiError(err as ApiError, t.errorLoad));
    } finally {
      setLoading(false);
    }
  }, [t.errorLoad]);

  useEffect(() => {
    let alive = true;
    void (async () => {
      await refresh();
      if (!alive) return;
    })();
    const id = window.setInterval(() => {
      void refresh();
    }, RUNS_REFRESH_MS);
    return () => {
      alive = false;
      window.clearInterval(id);
    };
  }, [refresh]);

  const expandRun = useCallback(
    async (runID: string) => {
      if (expandedRunID === runID) {
        setExpandedRunID(null);
        return;
      }
      setExpandedRunID(runID);
      if (drilledBreaks[runID]) return;
      try {
        setDrillLoading(runID);
        const res = await getAdminReconRun(runID);
        setDrilledBreaks((m) => ({ ...m, [runID]: res.breaks || [] }));
      } catch {
        // Soft-fail: the row stays expanded with empty breaks; the
        // top-level error banner already reflects load issues.
      } finally {
        setDrillLoading(null);
      }
    },
    [expandedRunID, drilledBreaks],
  );

  const onResolveSubmit = useCallback(async () => {
    if (!resolveTarget) return;
    try {
      setResolveSubmitting(true);
      setResolveError(null);
      await resolveAdminReconBreak(resolveTarget.breakID, resolveTarget.status, resolveNote);
      // Mark the in-memory break as updated.
      setDrilledBreaks((m) => {
        const next: typeof m = {};
        for (const [runID, list] of Object.entries(m)) {
          next[runID] = list.map((b) => {
            if (b.id !== resolveTarget.breakID) return b;
            return { ...b, status: resolveTarget.status, resolution_note: resolveNote };
          });
        }
        return next;
      });
      setResolveTarget(null);
      setResolveNote("");
    } catch (err) {
      setResolveError(formatApiError(err as ApiError, "resolve failed"));
    } finally {
      setResolveSubmitting(false);
    }
  }, [resolveTarget, resolveNote]);

  const onTriggerSubmit = useCallback(async () => {
    if (!triggerFundID.trim()) return;
    try {
      setTriggerSubmitting(true);
      setTriggerError(null);
      setTriggerSuccessAt(null);
      const res = await triggerAdminReconRun({
        fund_id: triggerFundID.trim(),
        use_mock_provider: true,
        mock_drift_qty: parseFloatOrZero(triggerDriftQty),
        mock_drift_cash: parseFloatOrZero(triggerDriftCash),
        mock_drift_price: parseFloatOrZero(triggerDriftPrice),
      });
      setTriggerSuccessAt(new Date().toISOString());
      setRuns((current) => [res.run, ...current]);
      setExpandedRunID(res.run.id);
      setDrilledBreaks((m) => ({ ...m, [res.run.id]: res.breaks || [] }));
    } catch (err) {
      setTriggerError(formatApiError(err as ApiError, "trigger failed"));
    } finally {
      setTriggerSubmitting(false);
    }
  }, [triggerFundID, triggerDriftQty, triggerDriftCash, triggerDriftPrice]);

  const severityLabel = useCallback(
    (s: Severity) => {
      switch (s) {
        case "critical":
          return t.severityCritical;
        case "warning":
          return t.severityWarning;
        case "info":
          return t.severityInfo;
      }
    },
    [t],
  );

  const statusLabel = useCallback(
    (s: BreakStatus | string) => {
      switch (s) {
        case "open":
          return t.statusOpen;
        case "acknowledged":
          return t.statusAcknowledged;
        case "resolved":
          return t.statusResolved;
        case "ignored":
          return t.statusIgnored;
        case "completed":
          return t.statusCompleted;
        case "failed":
          return t.statusFailed;
        case "pending":
          return t.statusPending;
        default:
          return s;
      }
    },
    [t],
  );

  const triggerSourceLabel = useCallback(
    (s: string) => {
      switch (s) {
        case "manual":
          return t.triggerSourceManual;
        case "scheduled":
          return t.triggerSourceScheduled;
        case "replay":
          return t.triggerSourceReplay;
        default:
          return s;
      }
    },
    [t],
  );

  const sortedRuns = useMemo(() => {
    return [...runs].sort((a, b) => (a.run_date < b.run_date ? 1 : a.run_date > b.run_date ? -1 : 0));
  }, [runs]);

  return (
    <section className="recon-admin-section" aria-labelledby="admin-recon-title">
      <header style={{ display: "flex", flexDirection: "column", gap: 4 }}>
        <h2 id="admin-recon-title" style={{ margin: 0 }}>
          {t.title}
        </h2>
        <p style={{ margin: 0, color: "#6b7280", fontSize: 14 }}>{t.subtitle}</p>
      </header>

      {/* Trigger form */}
      <details
        style={{
          marginTop: 12,
          padding: 12,
          border: "1px solid #e5e7eb",
          borderRadius: 8,
          background: "#fafafa",
        }}
      >
        <summary style={{ cursor: "pointer", fontWeight: 600 }}>{t.triggerRunButton}</summary>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(220px, 1fr))", gap: 8, marginTop: 12 }}>
          <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            <span style={{ fontSize: 12 }}>{t.triggerRunFundIdLabel}</span>
            <input
              type="text"
              value={triggerFundID}
              onChange={(e) => setTriggerFundID(e.target.value)}
              placeholder={t.triggerRunFundIdPlaceholder}
              style={{ padding: "6px 8px", border: "1px solid #d1d5db", borderRadius: 4 }}
            />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            <span style={{ fontSize: 12 }}>{t.triggerRunDriftQtyLabel}</span>
            <input
              type="number"
              value={triggerDriftQty}
              onChange={(e) => setTriggerDriftQty(e.target.value)}
              placeholder="0"
              style={{ padding: "6px 8px", border: "1px solid #d1d5db", borderRadius: 4 }}
            />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            <span style={{ fontSize: 12 }}>{t.triggerRunDriftCashLabel}</span>
            <input
              type="number"
              value={triggerDriftCash}
              onChange={(e) => setTriggerDriftCash(e.target.value)}
              placeholder="0"
              style={{ padding: "6px 8px", border: "1px solid #d1d5db", borderRadius: 4 }}
            />
          </label>
          <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
            <span style={{ fontSize: 12 }}>{t.triggerRunDriftPriceLabel}</span>
            <input
              type="number"
              value={triggerDriftPrice}
              onChange={(e) => setTriggerDriftPrice(e.target.value)}
              placeholder="0"
              style={{ padding: "6px 8px", border: "1px solid #d1d5db", borderRadius: 4 }}
            />
          </label>
        </div>
        <div style={{ marginTop: 12, display: "flex", alignItems: "center", gap: 12 }}>
          <button
            type="button"
            onClick={onTriggerSubmit}
            disabled={triggerSubmitting || !triggerFundID.trim()}
            style={{
              padding: "6px 12px",
              borderRadius: 4,
              border: "1px solid #2563eb",
              background: triggerSubmitting ? "#93c5fd" : "#2563eb",
              color: "#fff",
              cursor: triggerSubmitting ? "default" : "pointer",
            }}
          >
            {triggerSubmitting ? t.triggerRunSubmitting : t.triggerRunSubmit}
          </button>
          {triggerError && <span style={{ color: "#dc2626", fontSize: 13 }}>{triggerError}</span>}
          {triggerSuccessAt && <span style={{ color: "#16a34a", fontSize: 13 }}>{t.triggerRunSuccess}</span>}
        </div>
      </details>

      {/* Runs table */}
      <div style={{ marginTop: 16 }}>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <strong>{t.title}</strong>
          <button
            type="button"
            onClick={() => void refresh()}
            style={{
              padding: "4px 8px",
              border: "1px solid #d1d5db",
              borderRadius: 4,
              background: "#fff",
              cursor: "pointer",
            }}
          >
            {t.refresh}
          </button>
        </div>
        {loading && <p style={{ color: "#6b7280" }}>{t.loading}</p>}
        {loadError && <p style={{ color: "#dc2626" }}>{loadError}</p>}
        {!loading && !loadError && sortedRuns.length === 0 && <p style={{ color: "#6b7280" }}>{t.empty}</p>}
        {sortedRuns.length > 0 && (
          <table style={{ width: "100%", borderCollapse: "collapse", marginTop: 8, fontSize: 14 }}>
            <thead>
              <tr style={{ borderBottom: "1px solid #e5e7eb", textAlign: "left" }}>
                <th style={{ padding: 6 }}>{t.runDate}</th>
                <th style={{ padding: 6 }}>{t.triggerSource}</th>
                <th style={{ padding: 6 }}>{t.status}</th>
                <th style={{ padding: 6, textAlign: "right" }}>{t.breakCountCritical}</th>
                <th style={{ padding: 6, textAlign: "right" }}>{t.breakCountWarning}</th>
                <th style={{ padding: 6, textAlign: "right" }}>{t.breakCountInfo}</th>
                <th style={{ padding: 6, textAlign: "right" }}>{t.breakCount}</th>
                <th style={{ padding: 6 }} aria-label="actions" />
              </tr>
            </thead>
            <tbody>
              {sortedRuns.map((run) => (
                <ReconRunRow
                  key={run.id}
                  run={run}
                  expanded={expandedRunID === run.id}
                  drillLoading={drillLoading === run.id}
                  breaks={drilledBreaks[run.id]}
                  onToggle={() => void expandRun(run.id)}
                  language={language}
                  severityLabel={severityLabel}
                  statusLabel={statusLabel}
                  triggerSourceLabel={triggerSourceLabel}
                  onAct={(breakID, status) => {
                    setResolveTarget({ breakID, status });
                    setResolveNote("");
                    setResolveError(null);
                  }}
                  t={t}
                />
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* Resolve modal */}
      {resolveTarget && (
        <div
          role="dialog"
          aria-modal="true"
          aria-labelledby="recon-resolve-title"
          style={{
            position: "fixed",
            inset: 0,
            background: "rgba(0,0,0,0.4)",
            display: "flex",
            alignItems: "center",
            justifyContent: "center",
            zIndex: 50,
          }}
          onClick={() => !resolveSubmitting && setResolveTarget(null)}
        >
          <div
            onClick={(e) => e.stopPropagation()}
            style={{
              background: "#fff",
              padding: 16,
              borderRadius: 8,
              minWidth: 320,
              maxWidth: 420,
              display: "flex",
              flexDirection: "column",
              gap: 12,
            }}
          >
            <h3 id="recon-resolve-title" style={{ margin: 0 }}>
              {statusLabel(resolveTarget.status)}
            </h3>
            <label style={{ display: "flex", flexDirection: "column", gap: 4 }}>
              <span style={{ fontSize: 12 }}>{t.breakResolveNoteLabel}</span>
              <textarea
                value={resolveNote}
                onChange={(e) => setResolveNote(e.target.value)}
                rows={3}
                style={{ padding: 6, border: "1px solid #d1d5db", borderRadius: 4 }}
              />
            </label>
            {resolveError && <span style={{ color: "#dc2626", fontSize: 13 }}>{resolveError}</span>}
            <div style={{ display: "flex", justifyContent: "flex-end", gap: 8 }}>
              <button
                type="button"
                onClick={() => setResolveTarget(null)}
                disabled={resolveSubmitting}
                style={{
                  padding: "6px 12px",
                  border: "1px solid #d1d5db",
                  borderRadius: 4,
                  background: "#fff",
                  cursor: resolveSubmitting ? "default" : "pointer",
                }}
              >
                {t.collapse}
              </button>
              <button
                type="button"
                onClick={() => void onResolveSubmit()}
                disabled={resolveSubmitting}
                style={{
                  padding: "6px 12px",
                  border: "1px solid #2563eb",
                  background: resolveSubmitting ? "#93c5fd" : "#2563eb",
                  color: "#fff",
                  borderRadius: 4,
                  cursor: resolveSubmitting ? "default" : "pointer",
                }}
              >
                {resolveSubmitting ? t.breakResolveSubmitting : t.breakResolveSubmit}
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  );
}

interface ReconRunRowProps {
  run: ReconciliationRun;
  expanded: boolean;
  drillLoading: boolean;
  breaks?: ReconciliationBreak[];
  onToggle: () => void;
  language: Language;
  severityLabel: (s: Severity) => string;
  statusLabel: (s: string) => string;
  triggerSourceLabel: (s: string) => string;
  onAct: (breakID: string, status: BreakStatus) => void;
  t: (typeof messages)[Language];
}

function ReconRunRow({
  run,
  expanded,
  drillLoading,
  breaks,
  onToggle,
  severityLabel,
  statusLabel,
  triggerSourceLabel,
  onAct,
  t,
}: ReconRunRowProps) {
  return (
    <>
      <tr style={{ borderBottom: "1px solid #f3f4f6" }}>
        <td style={{ padding: 6 }}>{run.run_date}</td>
        <td style={{ padding: 6 }}>{triggerSourceLabel(run.trigger_source)}</td>
        <td style={{ padding: 6 }}>
          <StatusBadge status={run.status} statusLabel={statusLabel} />
        </td>
        <td style={{ padding: 6, textAlign: "right", color: run.break_count_critical > 0 ? "#dc2626" : undefined }}>
          {run.break_count_critical}
        </td>
        <td style={{ padding: 6, textAlign: "right", color: run.break_count_warning > 0 ? "#d97706" : undefined }}>
          {run.break_count_warning}
        </td>
        <td style={{ padding: 6, textAlign: "right" }}>{run.break_count_info}</td>
        <td style={{ padding: 6, textAlign: "right", fontWeight: 600 }}>{run.break_count_total}</td>
        <td style={{ padding: 6, textAlign: "right" }}>
          <button
            type="button"
            onClick={onToggle}
            style={{
              padding: "2px 8px",
              border: "1px solid #d1d5db",
              background: "#fff",
              borderRadius: 4,
              cursor: "pointer",
              fontSize: 12,
            }}
          >
            {expanded ? t.collapse : t.expand}
          </button>
        </td>
      </tr>
      {expanded && (
        <tr>
          <td colSpan={8} style={{ background: "#fafafa", padding: 12 }}>
            <ReconRunDrillDown
              breaks={breaks}
              drillLoading={drillLoading}
              severityLabel={severityLabel}
              statusLabel={statusLabel}
              onAct={onAct}
              t={t}
            />
          </td>
        </tr>
      )}
    </>
  );
}

function ReconRunDrillDown({
  breaks,
  drillLoading,
  severityLabel,
  statusLabel,
  onAct,
  t,
}: {
  breaks?: ReconciliationBreak[];
  drillLoading: boolean;
  severityLabel: (s: Severity) => string;
  statusLabel: (s: string) => string;
  onAct: (breakID: string, status: BreakStatus) => void;
  t: (typeof messages)[Language];
}) {
  if (drillLoading) return <p style={{ color: "#6b7280" }}>{t.loading}</p>;
  if (!breaks || breaks.length === 0) return <p style={{ color: "#6b7280" }}>{t.breakDrillNoBreaks}</p>;
  return (
    <table style={{ width: "100%", borderCollapse: "collapse", fontSize: 13 }}>
      <thead>
        <tr style={{ borderBottom: "1px solid #e5e7eb", textAlign: "left" }}>
          <th style={{ padding: 4 }}>{t.breakTypeLabel}</th>
          <th style={{ padding: 4 }}>{t.breakSymbolLabel}</th>
          <th style={{ padding: 4 }}>{t.breakCurrencyLabel}</th>
          <th style={{ padding: 4, textAlign: "right" }}>{t.breakInternalLabel}</th>
          <th style={{ padding: 4, textAlign: "right" }}>{t.breakBrokerLabel}</th>
          <th style={{ padding: 4, textAlign: "right" }}>{t.breakDiffLabel}</th>
          <th style={{ padding: 4, textAlign: "right" }}>{t.breakDiffPctLabel}</th>
          <th style={{ padding: 4 }}>{t.status}</th>
          <th style={{ padding: 4 }} aria-label="actions" />
        </tr>
      </thead>
      <tbody>
        {breaks.map((b) => (
          <tr key={b.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
            <td style={{ padding: 4 }}>
              <SeverityDot s={b.severity} label={severityLabel(b.severity)} /> {b.break_type}
            </td>
            <td style={{ padding: 4 }}>{b.symbol || ""}</td>
            <td style={{ padding: 4 }}>{b.currency || ""}</td>
            <td style={{ padding: 4, textAlign: "right" }}>{formatNullable(b.internal_value)}</td>
            <td style={{ padding: 4, textAlign: "right" }}>{formatNullable(b.broker_value)}</td>
            <td style={{ padding: 4, textAlign: "right" }}>{formatNullable(b.diff_value)}</td>
            <td style={{ padding: 4, textAlign: "right" }}>{formatNullablePct(b.diff_percent)}</td>
            <td style={{ padding: 4 }}>{statusLabel(b.status)}</td>
            <td style={{ padding: 4, textAlign: "right" }}>
              {b.status === "open" ? (
                <span style={{ display: "inline-flex", gap: 4 }}>
                  <ActionButton onClick={() => onAct(b.id, "acknowledged")} label={t.breakActionAcknowledge} />
                  <ActionButton onClick={() => onAct(b.id, "resolved")} label={t.breakActionResolve} />
                  <ActionButton onClick={() => onAct(b.id, "ignored")} label={t.breakActionIgnore} />
                </span>
              ) : (
                <ActionButton onClick={() => onAct(b.id, "open")} label={t.breakActionReopen} />
              )}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function ActionButton({ onClick, label }: { onClick: () => void; label: string }) {
  return (
    <button
      type="button"
      onClick={onClick}
      style={{
        padding: "2px 6px",
        border: "1px solid #d1d5db",
        background: "#fff",
        borderRadius: 4,
        cursor: "pointer",
        fontSize: 11,
      }}
    >
      {label}
    </button>
  );
}

function StatusBadge({ status, statusLabel }: { status: string; statusLabel: (s: string) => string }) {
  const color = status === "completed" ? "#16a34a" : status === "failed" ? "#dc2626" : "#2563eb";
  return (
    <span style={{ color, fontWeight: 600, fontSize: 12 }}>{statusLabel(status)}</span>
  );
}

function SeverityDot({ s, label }: { s: Severity; label: string }) {
  const color = s === "critical" ? "#dc2626" : s === "warning" ? "#d97706" : "#6b7280";
  return (
    <span aria-label={label} style={{ color, fontWeight: 700, marginRight: 4 }}>
      ●
    </span>
  );
}

function formatNullable(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  if (Number.isFinite(v)) return v.toFixed(4);
  return String(v);
}

function formatNullablePct(v: number | null | undefined): string {
  if (v === null || v === undefined) return "—";
  if (Number.isFinite(v)) return `${v.toFixed(2)}%`;
  return String(v);
}

function parseFloatOrZero(s: string): number {
  if (!s.trim()) return 0;
  const n = Number(s);
  return Number.isFinite(n) ? n : 0;
}
