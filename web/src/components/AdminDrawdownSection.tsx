// AdminDrawdownSection — P3-5 admin web component.
//
// What it does
//
//   - Operator types a fund_id and clicks "Load" to bring up
//     that fund's drawdown status: peak NAV, current NAV,
//     current DD, and the configured tier list.
//   - Tier editor: add / update / delete tiers (1..5) with
//     dd_pct, action, trim_ratio, cooldown, auto_execute, note.
//   - "Run check now" fires an on-demand evaluation; if a tier
//     breaches, a `proposed` event lands in the events table.
//   - Events table at the bottom lists recent breach events for
//     this fund (status filter: proposed by default). Each row
//     exposes approve / dismiss / re-open actions; the note
//     lands on the audit chain.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  ApiError,
  formatApiError,
  getAdminDrawdownPolicy,
  getAdminDrawdownStatus,
  upsertAdminDrawdownTier,
  deleteAdminDrawdownTier,
  triggerAdminDrawdownCheck,
  listAdminDrawdownEvents,
  reviewAdminDrawdownEvent,
  type DrawdownPolicy,
  type DrawdownEvent,
  type DrawdownEventStatus,
  type DrawdownStatus,
  type DrawdownAction,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Messages {
  panelTitle: string;
  panelSubtitle: string;
  refresh: string;
  listEmpty: string;
  fundIdLabel: string;
  fundIdPlaceholder: string;
  loadFundButton: string;
  statusTitle: string;
  peakNavLabel: string;
  currentNavLabel: string;
  currentDDLabel: string;
  hasPolicyTrue: string;
  hasPolicyFalse: string;
  breachedTierLabel: string;
  triggerCheckButton: string;
  triggerCheckRunning: string;
  triggerCheckNoBreach: string;
  triggerCheckBreached: string;
  triggerCheckError: string;
  tiersTitle: string;
  tierLabel: string;
  ddPctLabel: string;
  actionLabel: string;
  trimRatioLabel: string;
  cooldownLabel: string;
  autoExecuteLabel: string;
  noteLabel: string;
  addTierButton: string;
  saveTierButton: string;
  saveTierSubmitting: string;
  deleteTierButton: string;
  deleteConfirm: string;
  actionTrimProportional: string;
  actionFlatten: string;
  actionDefensiveOnly: string;
  eventsTitle: string;
  detectedAtLabel: string;
  statusLabel: string;
  statusProposed: string;
  statusApproved: string;
  statusExecuted: string;
  statusDismissed: string;
  statusSuperseded: string;
  trimPlanTitle: string;
  trimPlanEmpty: string;
  eventActionApprove: string;
  eventActionDismiss: string;
  eventActionReopen: string;
  reviewDialogTitle: string;
  reviewNoteLabel: string;
  reviewSubmit: string;
  reviewSubmitting: string;
  reviewError: string;
}

const messages: Record<Language, Messages> = {
  "zh-CN": {
    panelTitle: "回撤软熔断（Drawdown soft circuit breaker）",
    panelSubtitle:
      "按基金配置 DD 分级阈值，每 5 分钟自动评估；超阈值时记录建议降仓事件，由运维确认后通过审批流出单。auto_execute 打开的层会经审计链直接挂单（仍走风控）。",
    refresh: "刷新",
    listEmpty: "暂无回撤事件",
    fundIdLabel: "基金 ID",
    fundIdPlaceholder: "输入基金 UUID 查看 / 配置",
    loadFundButton: "加载",
    statusTitle: "当前 DD 状态",
    peakNavLabel: "区间峰值 NAV",
    currentNavLabel: "当前 NAV",
    currentDDLabel: "当前回撤",
    hasPolicyTrue: "已配置阈值",
    hasPolicyFalse: "未配置阈值",
    breachedTierLabel: "当前已触发档位",
    triggerCheckButton: "立即检查",
    triggerCheckRunning: "检查中…",
    triggerCheckNoBreach: "未触发任何档位",
    triggerCheckBreached: "已触发：",
    triggerCheckError: "检查失败",
    tiersTitle: "阈值配置（最多 5 档，由轻到重）",
    tierLabel: "档位",
    ddPctLabel: "DD 阈值（负数，例如 -0.05 表示 -5%）",
    actionLabel: "动作",
    trimRatioLabel: "降仓比例（trim_proportional 时生效）",
    cooldownLabel: "冷却时间（小时）",
    autoExecuteLabel: "自动执行（auto_execute）",
    noteLabel: "备注",
    addTierButton: "新增 / 修改档位",
    saveTierButton: "保存",
    saveTierSubmitting: "保存中…",
    deleteTierButton: "删除",
    deleteConfirm: "确定删除这一档？",
    actionTrimProportional: "按比例降仓",
    actionFlatten: "清仓",
    actionDefensiveOnly: "仅防守（拒绝新多头）",
    eventsTitle: "回撤事件",
    detectedAtLabel: "检测时间",
    statusLabel: "状态",
    statusProposed: "待审批",
    statusApproved: "已批准",
    statusExecuted: "已执行",
    statusDismissed: "已驳回",
    statusSuperseded: "已被覆盖",
    trimPlanTitle: "降仓计划",
    trimPlanEmpty: "本档位无具体降仓订单（defensive_only）",
    eventActionApprove: "批准并下单",
    eventActionDismiss: "驳回",
    eventActionReopen: "重开",
    reviewDialogTitle: "处理回撤事件",
    reviewNoteLabel: "备注（写明依据，便于审计）",
    reviewSubmit: "确定",
    reviewSubmitting: "处理中…",
    reviewError: "处理失败",
  },
  "en-US": {
    panelTitle: "Drawdown soft circuit breaker",
    panelSubtitle:
      "Per-fund tiered DD thresholds evaluated every 5 minutes. Breaches are recorded as proposed trim plans for operator review; tiers flagged auto_execute go straight to the order pipeline (still through risk gates + audit chain).",
    refresh: "Refresh",
    listEmpty: "No drawdown events.",
    fundIdLabel: "Fund ID",
    fundIdPlaceholder: "UUID of the fund to view / configure",
    loadFundButton: "Load",
    statusTitle: "Current drawdown status",
    peakNavLabel: "Peak NAV (lookback)",
    currentNavLabel: "Current NAV",
    currentDDLabel: "Current drawdown",
    hasPolicyTrue: "Policy configured",
    hasPolicyFalse: "No policy configured",
    breachedTierLabel: "Tier currently breached",
    triggerCheckButton: "Run check now",
    triggerCheckRunning: "Checking…",
    triggerCheckNoBreach: "No tier breached.",
    triggerCheckBreached: "Breached:",
    triggerCheckError: "Check failed",
    tiersTitle: "Tier configuration (max 5, mildest → hardest)",
    tierLabel: "Tier",
    ddPctLabel: "DD threshold (negative; -0.05 = -5%)",
    actionLabel: "Action",
    trimRatioLabel: "Trim ratio (trim_proportional only)",
    cooldownLabel: "Cooldown (hours)",
    autoExecuteLabel: "Auto-execute",
    noteLabel: "Note",
    addTierButton: "Add / update tier",
    saveTierButton: "Save",
    saveTierSubmitting: "Saving…",
    deleteTierButton: "Delete",
    deleteConfirm: "Delete this tier?",
    actionTrimProportional: "Trim proportional",
    actionFlatten: "Flatten",
    actionDefensiveOnly: "Defensive only (reject new longs)",
    eventsTitle: "Drawdown events",
    detectedAtLabel: "Detected",
    statusLabel: "Status",
    statusProposed: "proposed",
    statusApproved: "approved",
    statusExecuted: "executed",
    statusDismissed: "dismissed",
    statusSuperseded: "superseded",
    trimPlanTitle: "Trim plan",
    trimPlanEmpty: "No trim orders for this tier (defensive_only).",
    eventActionApprove: "Approve & queue orders",
    eventActionDismiss: "Dismiss",
    eventActionReopen: "Re-open",
    reviewDialogTitle: "Review drawdown event",
    reviewNoteLabel: "Note (recorded on the audit chain)",
    reviewSubmit: "Confirm",
    reviewSubmitting: "Submitting…",
    reviewError: "Failed to update",
  },
};

interface Props {
  language?: Language;
}

const actionLabel = (m: Messages, action: DrawdownAction): string => {
  switch (action) {
    case "trim_proportional":
      return m.actionTrimProportional;
    case "flatten":
      return m.actionFlatten;
    case "defensive_only":
      return m.actionDefensiveOnly;
    default:
      return action;
  }
};

const statusLabel = (m: Messages, s: DrawdownEventStatus): string => {
  switch (s) {
    case "proposed":
      return m.statusProposed;
    case "approved":
      return m.statusApproved;
    case "executed":
      return m.statusExecuted;
    case "dismissed":
      return m.statusDismissed;
    case "superseded":
      return m.statusSuperseded;
    default:
      return s;
  }
};

const formatPct = (v: number | undefined): string => {
  if (v === undefined || Number.isNaN(v)) return "—";
  return `${(v * 100).toFixed(2)}%`;
};

const formatNum = (v: number | undefined, fractionDigits = 4): string => {
  if (v === undefined || Number.isNaN(v)) return "—";
  return v.toLocaleString(undefined, { maximumFractionDigits: fractionDigits });
};

const formatTimestamp = (iso: string | undefined): string => {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
};

interface TierDraft {
  tier: number;
  dd_pct: number;
  action: DrawdownAction;
  trim_ratio: number;
  cooldown_hours: number;
  auto_execute: boolean;
  note: string;
}

const blankDraft: TierDraft = {
  tier: 1,
  dd_pct: -0.05,
  action: "trim_proportional",
  trim_ratio: 0.25,
  cooldown_hours: 24,
  auto_execute: false,
  note: "",
};

export function AdminDrawdownSection({ language = "zh-CN" }: Props) {
  const m = useMemo<Messages>(() => messages[language] ?? messages["zh-CN"], [language]);
  const [fundIDInput, setFundIDInput] = useState("");
  const [activeFundID, setActiveFundID] = useState("");
  const [policy, setPolicy] = useState<DrawdownPolicy | null>(null);
  const [status, setStatus] = useState<DrawdownStatus | null>(null);
  const [events, setEvents] = useState<DrawdownEvent[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(false);
  const [draft, setDraft] = useState<TierDraft>(blankDraft);
  const [savingTier, setSavingTier] = useState(false);
  const [checking, setChecking] = useState(false);
  const [checkResult, setCheckResult] = useState<string | null>(null);
  const [reviewing, setReviewing] = useState<{ event: DrawdownEvent; status: Exclude<DrawdownEventStatus, "executed"> } | null>(null);
  const [reviewNote, setReviewNote] = useState("");
  const [reviewSubmitting, setReviewSubmitting] = useState(false);
  const [reviewErr, setReviewErr] = useState<string | null>(null);

  const fetchAll = useCallback(async (fundID: string) => {
    if (!fundID) return;
    setLoading(true);
    setError(null);
    try {
      const [policyResp, statusResp, eventsResp] = await Promise.all([
        getAdminDrawdownPolicy(fundID),
        getAdminDrawdownStatus(fundID),
        listAdminDrawdownEvents({ fundId: fundID, limit: 50 }),
      ]);
      setPolicy(policyResp.policy);
      setStatus(statusResp.status);
      setEvents(eventsResp.events ?? []);
    } catch (err) {
      setError(formatApiError(err, m.reviewError));
    } finally {
      setLoading(false);
    }
  }, [m.reviewError]);

  useEffect(() => {
    if (!activeFundID) return;
    fetchAll(activeFundID).catch(() => {});
  }, [activeFundID, fetchAll]);

  const handleLoadFund = () => {
    const v = fundIDInput.trim();
    if (!v) return;
    setActiveFundID(v);
  };

  const submitTier = async () => {
    if (!activeFundID) return;
    setSavingTier(true);
    setError(null);
    try {
      await upsertAdminDrawdownTier(activeFundID, {
        ...draft,
      });
      setDraft(blankDraft);
      await fetchAll(activeFundID);
    } catch (err) {
      setError(formatApiError(err, m.reviewError));
    } finally {
      setSavingTier(false);
    }
  };

  const submitDelete = async (tier: number) => {
    if (!activeFundID) return;
    if (!window.confirm(m.deleteConfirm)) return;
    try {
      await deleteAdminDrawdownTier(activeFundID, tier);
      await fetchAll(activeFundID);
    } catch (err) {
      setError(formatApiError(err, m.reviewError));
    }
  };

  const submitCheck = async () => {
    if (!activeFundID) return;
    setChecking(true);
    setCheckResult(null);
    try {
      const resp = await triggerAdminDrawdownCheck(activeFundID);
      if (resp.breach && resp.event) {
        setCheckResult(`${m.triggerCheckBreached} tier ${resp.event.tier}`);
      } else if (resp.breach) {
        setCheckResult(`${m.triggerCheckBreached} (${resp.event_id ?? "?"})`);
      } else {
        setCheckResult(m.triggerCheckNoBreach);
      }
      await fetchAll(activeFundID);
    } catch (err) {
      setCheckResult(`${m.triggerCheckError}: ${formatApiError(err, m.triggerCheckError)}`);
    } finally {
      setChecking(false);
    }
  };

  const openReview = (ev: DrawdownEvent, target: Exclude<DrawdownEventStatus, "executed">) => {
    setReviewing({ event: ev, status: target });
    setReviewNote("");
    setReviewErr(null);
  };

  const submitReview = async () => {
    if (!reviewing) return;
    setReviewSubmitting(true);
    setReviewErr(null);
    try {
      await reviewAdminDrawdownEvent(reviewing.event.id, reviewing.status, reviewNote.trim() || undefined);
      setReviewing(null);
      await fetchAll(activeFundID);
    } catch (err) {
      if (err instanceof ApiError) {
        setReviewErr(formatApiError(err, m.reviewError));
      } else {
        setReviewErr(String(err));
      }
    } finally {
      setReviewSubmitting(false);
    }
  };

  const tiersToRender = policy?.tiers ?? [];

  return (
    <section className="rounded-lg border border-slate-200 bg-white p-4">
      <header className="flex items-start justify-between gap-4 pb-3">
        <div>
          <h2 className="text-lg font-semibold text-slate-900">{m.panelTitle}</h2>
          <p className="text-sm text-slate-500">{m.panelSubtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => activeFundID && fetchAll(activeFundID)}
          disabled={!activeFundID}
          className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
        >
          {m.refresh}
        </button>
      </header>

      <div className="flex flex-wrap items-end gap-2 pb-3">
        <label className="flex-1 min-w-[260px] text-sm">
          <span className="block text-xs font-semibold text-slate-600">{m.fundIdLabel}</span>
          <input
            type="text"
            value={fundIDInput}
            onChange={(e) => setFundIDInput(e.target.value)}
            placeholder={m.fundIdPlaceholder}
            className="mt-1 w-full rounded border border-slate-300 p-2 text-sm"
            onKeyDown={(e) => {
              if (e.key === "Enter") handleLoadFund();
            }}
          />
        </label>
        <button
          type="button"
          onClick={handleLoadFund}
          className="h-9 rounded border border-slate-300 px-3 text-sm hover:bg-slate-50"
        >
          {m.loadFundButton}
        </button>
      </div>

      {error && (
        <div className="mb-2 rounded border border-red-300 bg-red-50 p-2 text-sm text-red-700">
          {error}
        </div>
      )}

      {activeFundID && status && (
        <div className="mb-4 rounded border border-slate-200 bg-slate-50 p-3 text-sm">
          <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.statusTitle}</h3>
          <div className="grid grid-cols-2 gap-x-4 gap-y-1 md:grid-cols-4">
            <div>
              <span className="text-xs text-slate-500">{m.peakNavLabel}</span>
              <div className="tabular-nums">{formatNum(status.peak_nav, 6)}</div>
            </div>
            <div>
              <span className="text-xs text-slate-500">{m.currentNavLabel}</span>
              <div className="tabular-nums">{formatNum(status.current_nav, 6)}</div>
            </div>
            <div>
              <span className="text-xs text-slate-500">{m.currentDDLabel}</span>
              <div
                className={`tabular-nums ${
                  status.current_dd_pct < -0.1
                    ? "text-red-700 font-semibold"
                    : status.current_dd_pct < -0.05
                    ? "text-amber-700"
                    : "text-slate-700"
                }`}
              >
                {formatPct(status.current_dd_pct)}
              </div>
            </div>
            <div>
              <span className="text-xs text-slate-500">
                {status.has_policy ? m.hasPolicyTrue : m.hasPolicyFalse}
              </span>
              <div>
                {status.breached_tier ? (
                  <span className="text-red-700">
                    {m.breachedTierLabel}: {status.breached_tier} (
                    {actionLabel(m, status.breached_action ?? "trim_proportional")})
                  </span>
                ) : (
                  <span className="text-slate-500">—</span>
                )}
              </div>
            </div>
          </div>
          <div className="mt-3 flex items-center gap-2">
            <button
              type="button"
              onClick={submitCheck}
              disabled={checking || !status.has_policy}
              className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
            >
              {checking ? m.triggerCheckRunning : m.triggerCheckButton}
            </button>
            {checkResult && <span className="text-xs text-slate-600">{checkResult}</span>}
          </div>
        </div>
      )}

      {activeFundID && (
        <div className="mb-4">
          <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.tiersTitle}</h3>
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-slate-200 text-sm">
              <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
                <tr>
                  <th className="px-2 py-1">{m.tierLabel}</th>
                  <th className="px-2 py-1">{m.ddPctLabel}</th>
                  <th className="px-2 py-1">{m.actionLabel}</th>
                  <th className="px-2 py-1">{m.trimRatioLabel}</th>
                  <th className="px-2 py-1">{m.cooldownLabel}</th>
                  <th className="px-2 py-1">{m.autoExecuteLabel}</th>
                  <th className="px-2 py-1">{m.noteLabel}</th>
                  <th className="px-2 py-1"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-200 text-slate-700">
                {tiersToRender.map((t) => (
                  <tr key={t.tier}>
                    <td className="px-2 py-1 tabular-nums">{t.tier}</td>
                    <td className="px-2 py-1 tabular-nums">{formatPct(t.dd_pct)}</td>
                    <td className="px-2 py-1">{actionLabel(m, t.action)}</td>
                    <td className="px-2 py-1 tabular-nums">{formatPct(t.trim_ratio)}</td>
                    <td className="px-2 py-1 tabular-nums">{t.cooldown_hours}h</td>
                    <td className="px-2 py-1">{t.auto_execute ? "✓" : ""}</td>
                    <td className="px-2 py-1 text-xs text-slate-500">{t.note ?? ""}</td>
                    <td className="px-2 py-1">
                      <button
                        type="button"
                        onClick={() => setDraft({
                          tier: t.tier,
                          dd_pct: t.dd_pct,
                          action: t.action,
                          trim_ratio: t.trim_ratio,
                          cooldown_hours: t.cooldown_hours,
                          auto_execute: t.auto_execute,
                          note: t.note ?? "",
                        })}
                        className="text-xs text-blue-600 underline"
                      >
                        edit
                      </button>{" "}
                      <button
                        type="button"
                        onClick={() => submitDelete(t.tier)}
                        className="text-xs text-red-600 underline"
                      >
                        {m.deleteTierButton}
                      </button>
                    </td>
                  </tr>
                ))}
                {tiersToRender.length === 0 && (
                  <tr>
                    <td colSpan={8} className="px-2 py-2 text-center text-slate-400">
                      {m.hasPolicyFalse}
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </div>

          {/* Tier editor */}
          <div className="mt-3 rounded border border-slate-200 bg-slate-50 p-3">
            <h4 className="mb-2 text-xs font-semibold uppercase text-slate-600">
              {m.addTierButton}
            </h4>
            <div className="grid grid-cols-2 gap-2 md:grid-cols-4">
              <label className="text-xs">
                <span className="block text-slate-500">{m.tierLabel}</span>
                <input
                  type="number"
                  value={draft.tier}
                  min={1}
                  max={5}
                  onChange={(e) => setDraft({ ...draft, tier: Number(e.target.value) })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm tabular-nums"
                />
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.ddPctLabel}</span>
                <input
                  type="number"
                  step="0.0001"
                  value={draft.dd_pct}
                  onChange={(e) => setDraft({ ...draft, dd_pct: Number(e.target.value) })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm tabular-nums"
                />
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.actionLabel}</span>
                <select
                  value={draft.action}
                  onChange={(e) => setDraft({ ...draft, action: e.target.value as DrawdownAction })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm"
                >
                  <option value="trim_proportional">{m.actionTrimProportional}</option>
                  <option value="flatten">{m.actionFlatten}</option>
                  <option value="defensive_only">{m.actionDefensiveOnly}</option>
                </select>
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.trimRatioLabel}</span>
                <input
                  type="number"
                  step="0.01"
                  min={0}
                  max={1}
                  value={draft.trim_ratio}
                  onChange={(e) => setDraft({ ...draft, trim_ratio: Number(e.target.value) })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm tabular-nums"
                />
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.cooldownLabel}</span>
                <input
                  type="number"
                  min={0}
                  max={720}
                  value={draft.cooldown_hours}
                  onChange={(e) => setDraft({ ...draft, cooldown_hours: Number(e.target.value) })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm tabular-nums"
                />
              </label>
              <label className="text-xs">
                <span className="block text-slate-500">{m.autoExecuteLabel}</span>
                <input
                  type="checkbox"
                  checked={draft.auto_execute}
                  onChange={(e) => setDraft({ ...draft, auto_execute: e.target.checked })}
                  className="mt-2"
                />
              </label>
              <label className="col-span-2 text-xs md:col-span-2">
                <span className="block text-slate-500">{m.noteLabel}</span>
                <input
                  type="text"
                  value={draft.note}
                  onChange={(e) => setDraft({ ...draft, note: e.target.value })}
                  className="mt-1 w-full rounded border border-slate-300 p-1 text-sm"
                />
              </label>
            </div>
            <div className="mt-2 flex justify-end">
              <button
                type="button"
                onClick={submitTier}
                disabled={savingTier || !activeFundID}
                className="rounded bg-slate-900 px-3 py-1 text-sm text-white disabled:opacity-50"
              >
                {savingTier ? m.saveTierSubmitting : m.saveTierButton}
              </button>
            </div>
          </div>
        </div>
      )}

      {activeFundID && (
        <div>
          <h3 className="mb-2 text-sm font-semibold text-slate-800">{m.eventsTitle}</h3>
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-slate-200 text-sm">
              <thead className="bg-slate-50 text-left text-xs font-semibold uppercase text-slate-600">
                <tr>
                  <th className="px-2 py-1">{m.detectedAtLabel}</th>
                  <th className="px-2 py-1">{m.tierLabel}</th>
                  <th className="px-2 py-1">{m.currentDDLabel}</th>
                  <th className="px-2 py-1">{m.actionLabel}</th>
                  <th className="px-2 py-1">{m.trimPlanTitle}</th>
                  <th className="px-2 py-1">{m.statusLabel}</th>
                  <th className="px-2 py-1"></th>
                </tr>
              </thead>
              <tbody className="divide-y divide-slate-200 text-slate-700">
                {events.length === 0 && (
                  <tr>
                    <td colSpan={7} className="px-2 py-2 text-center text-slate-400">
                      {m.listEmpty}
                    </td>
                  </tr>
                )}
                {events.map((ev) => (
                  <tr key={ev.id} className="align-top">
                    <td className="px-2 py-1 text-xs tabular-nums">{formatTimestamp(ev.detected_at)}</td>
                    <td className="px-2 py-1 tabular-nums">{ev.tier}</td>
                    <td className="px-2 py-1 tabular-nums">{formatPct(ev.current_dd_pct)}</td>
                    <td className="px-2 py-1">{actionLabel(m, ev.action)}</td>
                    <td className="px-2 py-1 text-xs">
                      {ev.trim_plan.length === 0
                        ? m.trimPlanEmpty
                        : `${ev.trim_plan.length} order${ev.trim_plan.length === 1 ? "" : "s"}`}
                    </td>
                    <td className="px-2 py-1 text-xs">{statusLabel(m, ev.status)}</td>
                    <td className="px-2 py-1 text-right text-xs space-x-1">
                      {ev.status === "proposed" && (
                        <>
                          <button
                            type="button"
                            onClick={() => openReview(ev, "approved")}
                            className="text-emerald-700 underline"
                          >
                            {m.eventActionApprove}
                          </button>
                          <button
                            type="button"
                            onClick={() => openReview(ev, "dismissed")}
                            className="text-red-700 underline"
                          >
                            {m.eventActionDismiss}
                          </button>
                        </>
                      )}
                      {(ev.status === "dismissed" || ev.status === "superseded") && (
                        <button
                          type="button"
                          onClick={() => openReview(ev, "proposed")}
                          className="text-slate-500 underline"
                        >
                          {m.eventActionReopen}
                        </button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {!activeFundID && !loading && (
        <p className="text-sm text-slate-400">{m.fundIdPlaceholder}</p>
      )}

      {/* Review dialog */}
      {reviewing && (
        <div
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/40"
          onClick={() => setReviewing(null)}
          role="dialog"
          aria-modal="true"
        >
          <div
            className="w-full max-w-md rounded-lg bg-white p-4 shadow-lg"
            onClick={(e) => e.stopPropagation()}
          >
            <h3 className="text-base font-semibold text-slate-900">{m.reviewDialogTitle}</h3>
            <p className="mt-1 text-xs text-slate-500">
              tier {reviewing.event.tier} → {statusLabel(m, reviewing.status)}
            </p>
            <textarea
              value={reviewNote}
              onChange={(e) => setReviewNote(e.target.value)}
              placeholder={m.reviewNoteLabel}
              className="mt-3 w-full rounded border border-slate-300 p-2 text-sm"
              rows={3}
            />
            {reviewErr && <p className="mt-2 text-sm text-red-600">{reviewErr}</p>}
            <div className="mt-3 flex justify-end gap-2">
              <button
                type="button"
                className="rounded border border-slate-300 px-3 py-1 text-sm"
                onClick={() => setReviewing(null)}
                disabled={reviewSubmitting}
              >
                {m.refresh}
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
          </div>
        </div>
      )}
    </section>
  );
}

export default AdminDrawdownSection;
