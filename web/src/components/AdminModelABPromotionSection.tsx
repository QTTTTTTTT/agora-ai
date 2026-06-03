// AdminModelABPromotionSection — Sprint 13.3 admin UI for the
// model-A/B auto-promotion drafts produced by the nightly scanner
// (cmd/server/promotion_scan_loop.go). Shows the pending queue
// with an "apply" / "reject" affordance, plus a manual "scan now"
// button for impatient operators.

import { useCallback, useEffect, useState } from "react";

import {
  applyModelABPromotionDraft,
  fetchModelABPromotionDraft,
  fetchModelABPromotionDrafts,
  formatApiError,
  rejectModelABPromotionDraft,
  scanModelABPromotionDrafts,
  type ModelABPromotionDraft,
  type ModelABPromotionStatus,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  scanNow: string;
  scanRunning: string;
  scanResult: (n: number) => string;
  refresh: string;
  loading: string;
  emptyMessage: string;
  errorPrefix: string;
  statusFilterLabel: string;
  statusPending: string;
  statusApplied: string;
  statusRejected: string;
  statusSuperseded: string;
  statusAll: string;
  colEvaluatedAt: string;
  colExperiment: string;
  colRecommendation: string;
  colStreak: string;
  colStatus: string;
  colActions: string;
  detailsButton: string;
  applyButton: string;
  rejectButton: string;
  applyConfirmTitle: string;
  applyConfirmMessage: (label: string, exp: string) => string;
  applyConfirmConfirm: string;
  applyConfirmCancel: string;
  applyResultClosed: string;
  applyResultPartial: (warning: string) => string;
  rejectPromptTitle: string;
  rejectPromptPlaceholder: string;
  rejectPromptConfirm: string;
  rejectPromptCancel: string;
  detailsTitle: string;
  detailsCriteria: string;
  detailsReport: string;
  detailsClose: string;
  busy: string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "模型 A/B 自动晋升",
    subtitle: "每晚扫描运行中的模型 A/B 实验，当 B 臂连续若干天达标会生成一份草案，等待管理员一键采纳。",
    scanNow: "立即扫描",
    scanRunning: "扫描中…",
    scanResult: (n) => `已生成 ${n} 份草案`,
    refresh: "刷新",
    loading: "加载中…",
    emptyMessage: "暂无草案。",
    errorPrefix: "加载失败",
    statusFilterLabel: "状态",
    statusPending: "Pending",
    statusApplied: "Applied",
    statusRejected: "Rejected",
    statusSuperseded: "Superseded",
    statusAll: "全部",
    colEvaluatedAt: "评估时间",
    colExperiment: "实验",
    colRecommendation: "推荐",
    colStreak: "连胜天数",
    colStatus: "状态",
    colActions: "操作",
    detailsButton: "详情",
    applyButton: "采纳",
    rejectButton: "驳回",
    applyConfirmTitle: "确认采纳推荐",
    applyConfirmMessage: (label, exp) =>
      `将把实验 ${exp.slice(0, 8)} 标记为完成，并把推荐结果"${label}"作为采纳决策记入审计。本操作不会自动改动任意基金的模型配置——请通过基金侧的覆盖配置完成最终切换。`,
    applyConfirmConfirm: "确认采纳",
    applyConfirmCancel: "取消",
    applyResultClosed: "已采纳并关闭实验。",
    applyResultPartial: (warning) => `草案已采纳，但实验关闭失败：${warning}`,
    rejectPromptTitle: "驳回推荐",
    rejectPromptPlaceholder: "请填写驳回原因（用于合规审计）",
    rejectPromptConfirm: "确认驳回",
    rejectPromptCancel: "取消",
    detailsTitle: "推荐详情",
    detailsCriteria: "采用的判定标准",
    detailsReport: "报告快照",
    detailsClose: "关闭",
    busy: "提交中…",
  },
  "en-US": {
    title: "Model A/B auto-promotion",
    subtitle: "Each night the scanner inspects running model A/B experiments; when a non-primary arm beats the primary on the configured criteria for the required streak it leaves a draft here for one-click apply.",
    scanNow: "Scan now",
    scanRunning: "Scanning…",
    scanResult: (n) => `${n} draft(s) upserted`,
    refresh: "Refresh",
    loading: "Loading…",
    emptyMessage: "No drafts.",
    errorPrefix: "Load failed",
    statusFilterLabel: "Status",
    statusPending: "Pending",
    statusApplied: "Applied",
    statusRejected: "Rejected",
    statusSuperseded: "Superseded",
    statusAll: "All",
    colEvaluatedAt: "Evaluated at",
    colExperiment: "Experiment",
    colRecommendation: "Recommendation",
    colStreak: "Streak (days)",
    colStatus: "Status",
    colActions: "Actions",
    detailsButton: "Details",
    applyButton: "Apply",
    rejectButton: "Reject",
    applyConfirmTitle: "Apply recommendation",
    applyConfirmMessage: (label, exp) =>
      `Experiment ${exp.slice(0, 8)} will be marked complete and recommendation "${label}" recorded as the human decision. This action does NOT auto-rewrite any fund's model configuration — follow up via the per-fund model overrides.`,
    applyConfirmConfirm: "Confirm apply",
    applyConfirmCancel: "Cancel",
    applyResultClosed: "Applied and experiment closed.",
    applyResultPartial: (warning) => `Draft applied, but experiment close failed: ${warning}`,
    rejectPromptTitle: "Reject recommendation",
    rejectPromptPlaceholder: "Rejection reason (used in incident retro)",
    rejectPromptConfirm: "Confirm reject",
    rejectPromptCancel: "Cancel",
    detailsTitle: "Recommendation details",
    detailsCriteria: "Criteria used",
    detailsReport: "Report snapshot",
    detailsClose: "Close",
    busy: "Submitting…",
  },
};

function statusClass(s: string): string {
  switch (s) {
    case "pending":
      return "bg-amber-500/15 text-amber-200 border-amber-500/40";
    case "applied":
      return "bg-emerald-500/15 text-emerald-200 border-emerald-500/40";
    case "rejected":
      return "bg-rose-500/15 text-rose-200 border-rose-500/40";
    case "superseded":
      return "bg-zinc-700/40 text-zinc-300 border-zinc-600";
    default:
      return "bg-zinc-700/40 text-zinc-300 border-zinc-600";
  }
}

function formatTime(s?: string): string {
  if (!s) return "";
  return s.slice(0, 19).replace("T", " ");
}

export function AdminModelABPromotionSection({ language }: Props) {
  const t = messages[language];
  const [status, setStatus] = useState<"" | ModelABPromotionStatus>("pending");
  const [drafts, setDrafts] = useState<ModelABPromotionDraft[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [scanning, setScanning] = useState(false);
  const [scanMessage, setScanMessage] = useState<string | null>(null);

  const [applyingDraft, setApplyingDraft] = useState<ModelABPromotionDraft | null>(null);
  const [rejectingDraft, setRejectingDraft] = useState<ModelABPromotionDraft | null>(null);
  const [detailsDraft, setDetailsDraft] = useState<ModelABPromotionDraft | null>(null);
  const [rejectReason, setRejectReason] = useState("");
  const [busy, setBusy] = useState(false);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchModelABPromotionDrafts({
        status: status || undefined,
        limit: 100,
      });
      setDrafts(resp.items ?? []);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [status, t.errorPrefix]);

  useEffect(() => {
    reload();
  }, [reload]);

  const onScanNow = async () => {
    setScanning(true);
    setScanMessage(null);
    setError(null);
    try {
      const resp = await scanModelABPromotionDrafts();
      setScanMessage(t.scanResult(resp.drafts_upserted));
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setScanning(false);
    }
  };

  const onApply = async (draft: ModelABPromotionDraft) => {
    setBusy(true);
    setError(null);
    try {
      const resp = await applyModelABPromotionDraft(draft.id);
      setApplyingDraft(null);
      setScanMessage(resp.experiment_closed ? t.applyResultClosed : t.applyResultPartial(resp.warning ?? ""));
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setBusy(false);
    }
  };

  const onReject = async (draft: ModelABPromotionDraft) => {
    setBusy(true);
    setError(null);
    try {
      await rejectModelABPromotionDraft(draft.id, { reason: rejectReason });
      setRejectingDraft(null);
      setRejectReason("");
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setBusy(false);
    }
  };

  const onOpenDetails = async (draft: ModelABPromotionDraft) => {
    // Re-fetch with the report snapshot included.
    try {
      const full = await fetchModelABPromotionDraft(draft.id);
      setDetailsDraft(full);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    }
  };

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 max-w-3xl text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <div className="flex items-center gap-3 text-sm text-zinc-300">
          <label className="flex items-center gap-2">
            {t.statusFilterLabel}
            <select
              value={status}
              onChange={(e) => setStatus(e.target.value as "" | ModelABPromotionStatus)}
              className="rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
            >
              <option value="">{t.statusAll}</option>
              <option value="pending">{t.statusPending}</option>
              <option value="applied">{t.statusApplied}</option>
              <option value="rejected">{t.statusRejected}</option>
              <option value="superseded">{t.statusSuperseded}</option>
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
          <button
            type="button"
            onClick={onScanNow}
            disabled={scanning}
            className="rounded-md border border-emerald-500/40 bg-emerald-500/20 px-3 py-1 text-sm text-emerald-100 hover:bg-emerald-500/30 disabled:opacity-50"
          >
            {scanning ? t.scanRunning : t.scanNow}
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 rounded-md border border-rose-700 bg-rose-900/30 p-3 text-sm text-rose-200">
          {t.errorPrefix}: {error}
        </div>
      )}
      {scanMessage && (
        <div className="mt-4 rounded-md border border-emerald-700 bg-emerald-900/30 p-3 text-sm text-emerald-200">
          {scanMessage}
        </div>
      )}

      <div className="mt-6 overflow-x-auto rounded-lg border border-zinc-700 bg-zinc-900/30">
        {loading ? (
          <p className="p-4 text-sm text-zinc-400">{t.loading}</p>
        ) : drafts.length === 0 ? (
          <p className="p-4 text-sm text-zinc-400">{t.emptyMessage}</p>
        ) : (
          <table className="w-full text-xs text-zinc-200">
            <thead>
              <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                <th className="px-2 py-1 text-left">{t.colEvaluatedAt}</th>
                <th className="px-2 py-1 text-left">{t.colExperiment}</th>
                <th className="px-2 py-1 text-left">{t.colRecommendation}</th>
                <th className="px-2 py-1 text-left">{t.colStreak}</th>
                <th className="px-2 py-1 text-left">{t.colStatus}</th>
                <th className="px-2 py-1 text-left">{t.colActions}</th>
              </tr>
            </thead>
            <tbody>
              {drafts.map((d) => (
                <tr key={d.id} className="border-b border-zinc-800/40">
                  <td className="whitespace-nowrap px-2 py-1 text-zinc-400">{formatTime(d.evaluated_at)}</td>
                  <td className="whitespace-nowrap px-2 py-1 font-mono text-zinc-300">
                    {d.experiment_id.slice(0, 8)}
                  </td>
                  <td className="px-2 py-1">
                    <div className="text-zinc-100">{d.recommended_arm_label}</div>
                    <div className="text-[10px] text-zinc-500">
                      vs {d.primary_arm_label}
                    </div>
                  </td>
                  <td className="px-2 py-1 text-zinc-300">{d.streak_days}d</td>
                  <td className="px-2 py-1">
                    <span className={`rounded border px-1.5 py-0.5 text-[10px] uppercase ${statusClass(String(d.status))}`}>
                      {d.status}
                    </span>
                  </td>
                  <td className="px-2 py-1">
                    <div className="flex gap-2">
                      <button
                        type="button"
                        onClick={() => onOpenDetails(d)}
                        className="rounded border border-zinc-600 bg-zinc-800 px-2 py-0.5 text-[10px] text-zinc-100 hover:bg-zinc-700"
                      >
                        {t.detailsButton}
                      </button>
                      {d.status === "pending" && (
                        <>
                          <button
                            type="button"
                            onClick={() => setApplyingDraft(d)}
                            className="rounded border border-emerald-500/40 bg-emerald-500/15 px-2 py-0.5 text-[10px] text-emerald-200 hover:bg-emerald-500/25"
                          >
                            {t.applyButton}
                          </button>
                          <button
                            type="button"
                            onClick={() => {
                              setRejectingDraft(d);
                              setRejectReason("");
                            }}
                            className="rounded border border-rose-500/40 bg-rose-500/15 px-2 py-0.5 text-[10px] text-rose-200 hover:bg-rose-500/25"
                          >
                            {t.rejectButton}
                          </button>
                        </>
                      )}
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {applyingDraft && (
        <Modal onClose={() => !busy && setApplyingDraft(null)}>
          <h3 className="text-sm font-semibold text-zinc-100">{t.applyConfirmTitle}</h3>
          <p className="mt-3 text-sm text-zinc-300">
            {t.applyConfirmMessage(applyingDraft.recommended_arm_label, applyingDraft.experiment_id)}
          </p>
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setApplyingDraft(null)}
              disabled={busy}
              className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100"
            >
              {t.applyConfirmCancel}
            </button>
            <button
              type="button"
              onClick={() => onApply(applyingDraft)}
              disabled={busy}
              className="rounded-md bg-emerald-500 px-3 py-1 text-sm font-medium text-zinc-900 hover:bg-emerald-400 disabled:opacity-50"
            >
              {busy ? t.busy : t.applyConfirmConfirm}
            </button>
          </div>
        </Modal>
      )}

      {rejectingDraft && (
        <Modal onClose={() => !busy && setRejectingDraft(null)}>
          <h3 className="text-sm font-semibold text-zinc-100">{t.rejectPromptTitle}</h3>
          <textarea
            value={rejectReason}
            onChange={(e) => setRejectReason(e.target.value)}
            placeholder={t.rejectPromptPlaceholder}
            className="mt-3 h-24 w-full rounded-md border border-zinc-700 bg-zinc-800 p-2 text-sm text-zinc-100"
          />
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setRejectingDraft(null)}
              disabled={busy}
              className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100"
            >
              {t.rejectPromptCancel}
            </button>
            <button
              type="button"
              onClick={() => onReject(rejectingDraft)}
              disabled={busy || !rejectReason.trim()}
              className="rounded-md bg-rose-500 px-3 py-1 text-sm font-medium text-zinc-900 hover:bg-rose-400 disabled:opacity-50"
            >
              {busy ? t.busy : t.rejectPromptConfirm}
            </button>
          </div>
        </Modal>
      )}

      {detailsDraft && (
        <Modal onClose={() => setDetailsDraft(null)}>
          <h3 className="text-sm font-semibold text-zinc-100">{t.detailsTitle}</h3>
          <div className="mt-3">
            <div className="text-[10px] uppercase text-zinc-500">{t.detailsCriteria}</div>
            <pre className="mt-1 overflow-x-auto rounded bg-zinc-950/70 p-2 text-[11px] text-zinc-200">
              {JSON.stringify(detailsDraft.criteria_payload ?? {}, null, 2)}
            </pre>
          </div>
          <div className="mt-3">
            <div className="text-[10px] uppercase text-zinc-500">{t.detailsReport}</div>
            <pre className="mt-1 max-h-60 overflow-y-auto rounded bg-zinc-950/70 p-2 text-[11px] text-zinc-200">
              {JSON.stringify(detailsDraft.report_snapshot ?? {}, null, 2)}
            </pre>
          </div>
          <div className="mt-3 flex justify-end">
            <button
              type="button"
              onClick={() => setDetailsDraft(null)}
              className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100"
            >
              {t.detailsClose}
            </button>
          </div>
        </Modal>
      )}
    </section>
  );
}

function Modal({ children, onClose }: { children: React.ReactNode; onClose: () => void }) {
  return (
    <div className="fixed inset-0 z-40 flex items-center justify-center bg-black/50" onClick={onClose}>
      <div
        className="w-full max-w-xl rounded-lg border border-zinc-700 bg-zinc-900 p-4"
        onClick={(e) => e.stopPropagation()}
      >
        {children}
      </div>
    </div>
  );
}
