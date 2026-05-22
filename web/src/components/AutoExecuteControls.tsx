/**
 * AutoExecuteControls
 *
 * Self-contained UI for the per-fund auto-execute toggle + guardrail
 * editor. Two consumers:
 *
 *   - <CompaniesAutoExecuteToggle> on the Companies page, rendered next
 *     to each fund's row. Shows a small switch + gear icon; clicking
 *     the gear opens the settings modal.
 *   - <FundLayoutAutoExecuteBadge> on the per-fund layout header, shows
 *     a constant "自动决策：开/关" badge plus the same gear/modal.
 *
 * Both share the modal, the AutoExecuteConfig type, and the API call
 * that PATCHes the fund. State is kept local — the parent passes in
 * the current Fund object (or just the relevant subset) and an
 * onUpdated(updatedFund) callback so its own state can stay in sync.
 *
 * The persistence path goes through PUT /api/funds/{id} with body
 * {autoExecute: {...}} — the server already supports this via the
 * existing FundConfig.AutoExecute pointer.
 *
 * Copy is Chinese to match the rest of the app; English is shown
 * underneath in the modal description for translator parity.
 */

import React, { useEffect, useMemo, useState } from "react";
import { apiPut, formatApiError } from "../lib/api";

export interface AutoExecuteConfig {
  enabled: boolean;
  maxOrderPctOfAssets?: number;
  maxDailyPctOfAssets?: number;
  minConfidence?: number;
  slippageBouncePolicy?: "bounce_to_user" | "reject" | "force_execute";
  allowedMarkets?: string[];
  /**
   * Per-fund decision interval in minutes. When set, the scheduler
   * emits a new decision run every N minutes within the fund market's
   * trading windows (auction + regular segments, e.g. for A-share:
   * 9:00, 9:30, 10:00, 10:30, 11:00 + 13:00, 13:30, 14:00, 14:30 with
   * 30-min interval). Omitted / null = legacy one-shot daily trigger.
   * Server clamps to [5, 720] minutes.
   */
  decisionIntervalMinutes?: number | null;
}

export interface MinimalFund {
  id: string;
  name: string;
  autoExecute?: AutoExecuteConfig | null;
  /**
   * Phase 2B research tier toggle. "advanced" routes the daily
   * roundtable through the multi-agent bull/bear/quant debate;
   * anything else (default empty / "standard") uses the cheaper
   * text-concat roundtable. The server always emits a canonical
   * value on reads, so the UI doesn't have to disambiguate.
   */
  researchTier?: string;
  /**
   * Canonical market code (e.g. "a_share", "us_equity", "crypto",
   * "futures"). Used by the auto-execute modal to render a
   * market-appropriate slot preview under the decision-interval
   * picker. Optional because some callers (e.g. older list views)
   * may not surface this field; the preview falls back to a
   * generic 9:30–15:00 window when absent.
   */
  market?: string;
}

export type ResearchTier = "standard" | "advanced";

const RESEARCH_TIER_LABEL_ZH: Record<ResearchTier, string> = {
  standard: "标准（快、便宜，文本汇总）",
  advanced: "深度辩论（多 agent 多轮，慢、贵，更适合波动/政策驱动）",
};

// Bounds mirrored from server (marketcalendar.{Min,Max}DecisionIntervalMinutes).
// Keep in sync; values outside the envelope get clamped server-side.
const MIN_INTERVAL_MINUTES = 5;
const MAX_INTERVAL_MINUTES = 12 * 60;

// FormState extends AutoExecuteConfig with the local UI-only fields
// needed to drive the decision-interval picker (amount + unit + a
// "recurring vs one-shot" toggle). The submit path collapses them
// back into the integer-minute count the API expects.
interface FormState {
  enabled: boolean;
  maxOrderPctOfAssets: number;
  maxDailyPctOfAssets: number;
  minConfidence: number;
  slippageBouncePolicy: NonNullable<AutoExecuteConfig["slippageBouncePolicy"]>;
  allowedMarkets: string[];
  // intervalEnabled = false → fall back to the legacy one-shot daily
  // trigger (PreOpen − 30 min). true → fire every intervalAmount of
  // intervalUnit inside the market's trading windows.
  intervalEnabled: boolean;
  intervalUnit: "minutes" | "hours";
  // Displayed-as-entered magnitude. Float so "1.5 hours" round-trips
  // through the form before getting rounded to whole minutes on submit.
  intervalAmount: number;
}

const DEFAULT_FORM: FormState = {
  enabled: false,
  maxOrderPctOfAssets: 0.05,
  maxDailyPctOfAssets: 0.20,
  minConfidence: 0.60,
  slippageBouncePolicy: "bounce_to_user",
  allowedMarkets: [],
  intervalEnabled: false,
  intervalUnit: "minutes",
  intervalAmount: 30,
};

// decodeInterval turns a server-side minute count (or null/undefined =
// "one-shot daily mode") into the UI-friendly amount/unit pair.
// Prefers "hours" when the value divides cleanly so 60 → "1 hour"
// rather than "60 minutes".
function decodeInterval(minutes: number | null | undefined): Pick<FormState, "intervalEnabled" | "intervalUnit" | "intervalAmount"> {
  if (minutes === null || minutes === undefined) {
    return {
      intervalEnabled: false,
      intervalUnit: DEFAULT_FORM.intervalUnit,
      intervalAmount: DEFAULT_FORM.intervalAmount,
    };
  }
  if (minutes >= 60 && minutes % 60 === 0) {
    return { intervalEnabled: true, intervalUnit: "hours", intervalAmount: minutes / 60 };
  }
  return { intervalEnabled: true, intervalUnit: "minutes", intervalAmount: minutes };
}

// encodeInterval converts the UI inputs back into the integer-minute
// count the server expects. Returns 0 as the "interval mode disabled"
// sentinel — Go's *int with json:"omitempty" can't distinguish "null"
// from "absent", so we use a positive value to opt in and 0 to opt
// out. The backend's mergeFundAutoExecute treats 0 as "clear the
// previously persisted interval" so the user can revert in one save.
function encodeInterval(form: Pick<FormState, "intervalEnabled" | "intervalUnit" | "intervalAmount">): number {
  if (!form.intervalEnabled) return 0;
  const amount = Number.isFinite(form.intervalAmount) ? form.intervalAmount : 0;
  let minutes = form.intervalUnit === "hours" ? Math.round(amount * 60) : Math.round(amount);
  if (minutes < MIN_INTERVAL_MINUTES) minutes = MIN_INTERVAL_MINUTES;
  if (minutes > MAX_INTERVAL_MINUTES) minutes = MAX_INTERVAL_MINUTES;
  return minutes;
}

function mergeFormDefaults(cfg?: AutoExecuteConfig | null): FormState {
  return {
    enabled: !!cfg?.enabled,
    maxOrderPctOfAssets: cfg?.maxOrderPctOfAssets ?? DEFAULT_FORM.maxOrderPctOfAssets,
    maxDailyPctOfAssets: cfg?.maxDailyPctOfAssets ?? DEFAULT_FORM.maxDailyPctOfAssets,
    minConfidence: cfg?.minConfidence ?? DEFAULT_FORM.minConfidence,
    slippageBouncePolicy: cfg?.slippageBouncePolicy ?? DEFAULT_FORM.slippageBouncePolicy,
    allowedMarkets: cfg?.allowedMarkets ?? DEFAULT_FORM.allowedMarkets,
    ...decodeInterval(cfg?.decisionIntervalMinutes ?? null),
  };
}

// MarketWindow describes one contiguous trading window (label + start
// + end in HH:mm). Used by the modal to compute & render a slot
// preview that mirrors how the server actually schedules triggers.
// Times are LOCAL to the market — the user reads them as wall-clock,
// not UTC. Keep this table in sync with marketcalendar.SessionForDate
// (server/internal/marketcalendar/service.go).
interface MarketWindow {
  label: string;
  start: string; // "HH:mm" local time
  end: string;   // "HH:mm" local time (exclusive of slot generation)
}
interface MarketProfile {
  zoneLabel: string; // human-readable timezone hint
  windows: MarketWindow[];
}

const MARKET_PROFILES: Record<string, MarketProfile> = {
  a_share: {
    zoneLabel: "Asia/Shanghai",
    windows: [
      { label: "上午（含集合竞价）", start: "09:00", end: "11:30" },
      { label: "下午", start: "13:00", end: "15:00" },
    ],
  },
  us_equity: {
    zoneLabel: "America/New_York",
    windows: [{ label: "盘前 + 常规交易", start: "09:00", end: "16:00" }],
  },
  crypto: {
    zoneLabel: "UTC",
    windows: [{ label: "24h", start: "08:30", end: "21:00" }],
  },
  futures: {
    zoneLabel: "America/Chicago",
    windows: [{ label: "常规", start: "08:00", end: "15:00" }],
  },
};

const FALLBACK_MARKET_PROFILE: MarketProfile = {
  zoneLabel: "市场本地时间",
  windows: [{ label: "常规", start: "08:45", end: "15:00" }],
};

// computeSlotPreview generates the same slot list the server would
// produce for the given (market, interval) pair. Used purely for UI
// hint rendering — never sent over the wire — so it does not need to
// be cryptographically aligned with marketcalendar; close enough for
// the user to sanity-check is fine.
function computeSlotPreview(marketKey: string | undefined, intervalMinutes: number): { profile: MarketProfile; slots: string[][] } {
  const profile = (marketKey && MARKET_PROFILES[marketKey.toLowerCase()]) || FALLBACK_MARKET_PROFILE;
  const slots = profile.windows.map((w) => generateSlots(w.start, w.end, intervalMinutes));
  return { profile, slots };
}

function generateSlots(start: string, end: string, intervalMinutes: number): string[] {
  const startMin = hhmmToMinutes(start);
  const endMin = hhmmToMinutes(end);
  if (!Number.isFinite(startMin) || !Number.isFinite(endMin) || intervalMinutes <= 0 || endMin <= startMin) {
    return [];
  }
  const out: string[] = [];
  for (let t = startMin; t < endMin; t += intervalMinutes) {
    out.push(minutesToHHmm(t));
  }
  return out;
}

function hhmmToMinutes(hhmm: string): number {
  const m = /^([0-2]?\d):([0-5]\d)$/.exec(hhmm.trim());
  if (!m) return NaN;
  return parseInt(m[1], 10) * 60 + parseInt(m[2], 10);
}

function minutesToHHmm(min: number): string {
  const h = Math.floor(min / 60).toString().padStart(2, "0");
  const m = Math.floor(min % 60).toString().padStart(2, "0");
  return `${h}:${m}`;
}

const POLICY_LABEL_ZH: Record<NonNullable<AutoExecuteConfig["slippageBouncePolicy"]>, string> = {
  bounce_to_user: "退回人工审批（默认）",
  reject: "直接拒绝该方案",
  force_execute: "强制按实时价成交",
};

interface ToggleProps {
  enabled: boolean;
  disabled?: boolean;
  onToggle: (next: boolean) => void;
  label?: string;
  size?: "sm" | "md";
}

function SwitchToggle({ enabled, disabled, onToggle, label, size = "md" }: ToggleProps): JSX.Element {
  const widthClass = size === "sm" ? "h-5 w-9" : "h-6 w-11";
  const knobBase = size === "sm" ? "h-4 w-4" : "h-5 w-5";
  const translate = enabled
    ? size === "sm"
      ? "translate-x-4"
      : "translate-x-5"
    : "translate-x-0.5";
  return (
    <button
      type="button"
      role="switch"
      aria-checked={enabled}
      aria-label={label}
      disabled={disabled}
      onClick={() => !disabled && onToggle(!enabled)}
      className={`relative inline-flex flex-shrink-0 cursor-pointer rounded-full border border-transparent transition-colors duration-200 focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:ring-offset-2 ${widthClass} ${
        enabled ? "bg-indigo-600" : "bg-gray-300"
      } ${disabled ? "cursor-not-allowed opacity-60" : ""}`}
    >
      <span
        className={`pointer-events-none inline-block transform rounded-full bg-white shadow-sm transition duration-200 ${knobBase} ${translate}`}
      />
    </button>
  );
}

function GearIcon({ className }: { className?: string }): JSX.Element {
  return (
    <svg viewBox="0 0 20 20" fill="none" stroke="currentColor" strokeWidth="1.5" className={className ?? "h-4 w-4"}>
      <circle cx="10" cy="10" r="2.5" />
      <path d="M10 1.5v2M10 16.5v2M3.5 10h-2M18.5 10h-2M5 5l-1.4-1.4M16.4 16.4L15 15M5 15l-1.4 1.4M16.4 3.6L15 5" />
    </svg>
  );
}

interface SettingsModalProps {
  open: boolean;
  fund: MinimalFund;
  onClose: () => void;
  onSaved: (updated: MinimalFund) => void;
}

export function AutoExecuteSettingsModal({ open, fund, onClose, onSaved }: SettingsModalProps): JSX.Element | null {
  const [form, setForm] = useState<FormState>(mergeFormDefaults(fund.autoExecute));
  const [researchTier, setResearchTier] = useState<ResearchTier>(
    fund.researchTier === "advanced" ? "advanced" : "standard",
  );
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (open) {
      setForm(mergeFormDefaults(fund.autoExecute));
      setResearchTier(fund.researchTier === "advanced" ? "advanced" : "standard");
      setError(null);
    }
  }, [open, fund.autoExecute, fund.researchTier]);

  if (!open) return null;

  const handleSubmit = async (event: React.FormEvent) => {
    event.preventDefault();
    setError(null);
    setSaving(true);
    try {
      const payload: AutoExecuteConfig = {
        enabled: form.enabled,
        maxOrderPctOfAssets: form.maxOrderPctOfAssets,
        maxDailyPctOfAssets: form.maxDailyPctOfAssets,
        minConfidence: form.minConfidence,
        slippageBouncePolicy: form.slippageBouncePolicy,
        allowedMarkets: form.allowedMarkets,
        // Always sent explicitly: integer when interval mode is on,
        // null when the user wants legacy one-shot daily. Sending null
        // lets the backend's mergeFundAutoExecute clear a previously
        // persisted interval; omitting the field would silently keep it.
        decisionIntervalMinutes: encodeInterval(form),
      };
      const updated = await apiPut<MinimalFund>(`/api/funds/${fund.id}`, {
        autoExecute: payload,
        researchTier,
      });
      onSaved(updated);
      onClose();
    } catch (err) {
      setError(formatApiError(err, "保存自动决策设置失败"));
    } finally {
      setSaving(false);
    }
  };

  const previewIntervalMinutes = encodeInterval(form);
  const slotPreview = previewIntervalMinutes > 0
    ? computeSlotPreview(fund.market, previewIntervalMinutes)
    : null;

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4" role="dialog" aria-modal>
      {/*
       * Three-section layout: fixed header / scrollable body / sticky
       * footer. max-h-[90vh] caps the dialog so the footer ("取消" +
       * "保存设置") is always visible even on short laptop screens —
       * before this, long-form content (适用范围说明 + 4 输入 + 白名单 +
       * 研究深度) pushed the action buttons off the viewport.
       *
       * min-h-0 on the middle <div> is required so flex-1 + overflow
       * scroll actually kicks in (otherwise the child decides height).
       */}
      <div className="flex max-h-[90vh] w-full max-w-lg flex-col overflow-hidden rounded-2xl bg-white shadow-2xl">
        <div className="shrink-0 border-b border-gray-100 px-6 py-4">
          <h3 className="text-base font-semibold text-gray-900">自动决策设置 · {fund.name}</h3>
          <p className="mt-1 text-xs text-gray-500">
            打开后，团队产出的方案在通过下列护栏时无需人工审批直接执行；任一条不满足都会自动回到人工审批。
          </p>
        </div>

        <form onSubmit={handleSubmit} className="flex min-h-0 flex-1 flex-col text-sm">
          <div className="min-h-0 flex-1 space-y-5 overflow-y-auto px-6 py-5">
          <label className="flex items-center justify-between gap-3 rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
            <span className="font-medium text-gray-800">启用自动决策执行</span>
            <SwitchToggle
              enabled={form.enabled}
              onToggle={(next) => setForm((prev) => ({ ...prev, enabled: next }))}
              label="启用自动决策"
            />
          </label>

          <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-xs leading-relaxed text-amber-900">
            <p className="mb-2 font-semibold text-amber-900">适用范围</p>
            <ul className="list-disc space-y-1 pl-5">
              <li>仅对单笔不超过 {(form.maxOrderPctOfAssets * 100).toFixed(1)}% NAV 的方案自动放行（超过仍需人工审批）。</li>
              <li>单日累计自动放行不超过 {(form.maxDailyPctOfAssets * 100).toFixed(1)}% NAV（超过自动退回人工）。</li>
              <li>PM 决策置信度需 ≥ {(form.minConfidence * 100).toFixed(0)}% 才放行。</li>
              <li>滑点超出阈值时按所选策略处理（默认退回人工，可改为拒绝或按实时价成交）。</li>
              <li>不影响硬风控规则（手数、T+1、单笔 NAV 上限、滑点 guard 等照常生效）。</li>
              <li>可随时关闭，已生成但未执行的方案不受影响。</li>
            </ul>
          </div>

          <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
            <label className="block">
              <span className="text-xs font-medium text-gray-700">单笔上限（% NAV）</span>
              <input
                type="number"
                min={0}
                max={100}
                step={0.5}
                value={(form.maxOrderPctOfAssets * 100).toFixed(2)}
                onChange={(e) =>
                  setForm((prev) => ({ ...prev, maxOrderPctOfAssets: clampPct(parseFloat(e.target.value)) }))
                }
                className="mt-1 block w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
              />
              <span className="mt-1 block text-[11px] text-gray-400">默认 5%；建议 ≤ 10%</span>
            </label>

            <label className="block">
              <span className="text-xs font-medium text-gray-700">单日累计上限（% NAV）</span>
              <input
                type="number"
                min={0}
                max={100}
                step={1}
                value={(form.maxDailyPctOfAssets * 100).toFixed(2)}
                onChange={(e) =>
                  setForm((prev) => ({ ...prev, maxDailyPctOfAssets: clampPct(parseFloat(e.target.value)) }))
                }
                className="mt-1 block w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
              />
              <span className="mt-1 block text-[11px] text-gray-400">默认 20%；建议 ≤ 30%</span>
            </label>

            <label className="block">
              <span className="text-xs font-medium text-gray-700">置信度下限（0~1）</span>
              <input
                type="number"
                min={0}
                max={1}
                step={0.05}
                value={form.minConfidence.toFixed(2)}
                onChange={(e) =>
                  setForm((prev) => ({ ...prev, minConfidence: clampUnit(parseFloat(e.target.value)) }))
                }
                className="mt-1 block w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
              />
              <span className="mt-1 block text-[11px] text-gray-400">默认 0.60；过低可能放行高风险方案</span>
            </label>

            <label className="block">
              <span className="text-xs font-medium text-gray-700">滑点超阈值策略</span>
              <select
                value={form.slippageBouncePolicy}
                onChange={(e) =>
                  setForm((prev) => ({
                    ...prev,
                    slippageBouncePolicy: e.target.value as FormState["slippageBouncePolicy"],
                  }))
                }
                className="mt-1 block w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
              >
                {Object.entries(POLICY_LABEL_ZH).map(([value, label]) => (
                  <option key={value} value={value}>
                    {label}
                  </option>
                ))}
              </select>
              <span className="mt-1 block text-[11px] text-gray-400">默认退回人工，最稳妥</span>
            </label>
          </div>

          <label className="block">
            <span className="text-xs font-medium text-gray-700">市场白名单（可选）</span>
            <input
              type="text"
              value={form.allowedMarkets.join(", ")}
              onChange={(e) =>
                setForm((prev) => ({
                  ...prev,
                  allowedMarkets: e.target.value
                    .split(/[\s,，]+/)
                    .map((s) => s.trim().toLowerCase())
                    .filter(Boolean),
                }))
              }
              placeholder="留空 = 不限制；多个用逗号分隔，例如 a_share, us_equity"
              className="mt-1 block w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
            />
            <span className="mt-1 block text-[11px] text-gray-400">仅在白名单的市场允许自动放行</span>
          </label>

          {/*
           * Decision-interval control. Off → legacy one-shot daily
           * trigger (PreOpen − 30 min). On → workflow re-runs every
           * intervalAmount of intervalUnit inside the market's
           * trading windows (auction + regular segments). The slot
           * preview underneath shows the exact wall-clock times the
           * server will fire at, mirrored from marketcalendar so the
           * user can sanity-check before saving. Server clamps the
           * value to [5 min, 12h]; UI keeps the inputs in the same
           * envelope.
           */}
          <div className="block rounded-xl border border-sky-100 bg-sky-50/40 px-4 py-3">
            <label className="flex items-center justify-between gap-2">
              <span className="text-xs font-semibold text-sky-900">决策间隔</span>
              <span className="flex items-center gap-1.5 text-[11px] text-sky-900/80">
                <input
                  type="checkbox"
                  checked={form.intervalEnabled}
                  onChange={(e) =>
                    setForm((prev) => ({ ...prev, intervalEnabled: e.target.checked }))
                  }
                  className="h-3.5 w-3.5 rounded border-sky-300 text-sky-600 focus:ring-sky-500"
                />
                启用日内循环决策
              </span>
            </label>
            <div className="mt-2 grid grid-cols-[1fr_1fr] gap-2">
              <input
                type="number"
                min={form.intervalUnit === "hours" ? 1 : MIN_INTERVAL_MINUTES}
                max={form.intervalUnit === "hours" ? MAX_INTERVAL_MINUTES / 60 : MAX_INTERVAL_MINUTES}
                step={form.intervalUnit === "hours" ? 0.5 : 5}
                disabled={!form.intervalEnabled}
                value={Number.isFinite(form.intervalAmount) ? form.intervalAmount : 0}
                onChange={(e) =>
                  setForm((prev) => ({
                    ...prev,
                    intervalAmount: Math.max(0, parseFloat(e.target.value) || 0),
                  }))
                }
                className="block w-full rounded-lg border border-sky-200 bg-white px-3 py-2 text-sm focus:border-sky-500 focus:outline-none focus:ring-1 focus:ring-sky-500 disabled:bg-gray-100 disabled:text-gray-400"
              />
              <select
                disabled={!form.intervalEnabled}
                value={form.intervalUnit}
                onChange={(e) =>
                  setForm((prev) => ({
                    ...prev,
                    intervalUnit: e.target.value as FormState["intervalUnit"],
                  }))
                }
                className="block w-full rounded-lg border border-sky-200 bg-white px-3 py-2 text-sm focus:border-sky-500 focus:outline-none focus:ring-1 focus:ring-sky-500 disabled:bg-gray-100 disabled:text-gray-400"
              >
                <option value="minutes">分钟</option>
                <option value="hours">小时</option>
              </select>
            </div>
            <p className="mt-2 text-[11px] leading-relaxed text-sky-900/80">
              {form.intervalEnabled
                ? `每 ${previewIntervalMinutes} 分钟在交易时段内触发一次决策（结合 ${fund.market || "市场"} 的开盘/午休时间，开盘前的集合竞价窗口也会算入）。改动后下一个触发时刻生效。`
                : "未启用 → 每个交易日只触发一次（盘前 30 分钟启动），与历史行为一致。"}
            </p>
            {slotPreview ? (
              <div className="mt-2 rounded-lg border border-sky-100 bg-white px-3 py-2 text-[11px] text-sky-900/80">
                <div className="mb-1 font-medium text-sky-900">
                  本市场触发时刻预览（{slotPreview.profile.zoneLabel}）
                </div>
                <ul className="space-y-1">
                  {slotPreview.profile.windows.map((window, idx) => (
                    <li key={window.label}>
                      <span className="text-sky-700">{window.label}：</span>
                      <span className="font-mono">
                        {slotPreview.slots[idx].length === 0
                          ? "（间隔超过本窗口长度，无触发）"
                          : slotPreview.slots[idx].join(" · ")}
                      </span>
                    </li>
                  ))}
                </ul>
              </div>
            ) : null}
          </div>

          <label className="block rounded-xl border border-indigo-100 bg-indigo-50/50 px-4 py-3">
            <span className="text-xs font-semibold text-indigo-800">研究深度</span>
            <select
              value={researchTier}
              onChange={(e) => setResearchTier(e.target.value as ResearchTier)}
              className="mt-1 block w-full rounded-lg border border-indigo-200 bg-white px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
            >
              {(Object.keys(RESEARCH_TIER_LABEL_ZH) as ResearchTier[]).map((value) => (
                <option key={value} value={value}>
                  {RESEARCH_TIER_LABEL_ZH[value]}
                </option>
              ))}
            </select>
            <span className="mt-2 block text-[11px] leading-relaxed text-indigo-900/80">
              深度辩论会让 Bull / Bear / Quant 三个研究员对每个标的进行 2 轮辩论后再给 PM；适合波动大、消息驱动的标的。LLM 调用成本约为标准模式 3~5 倍。
            </span>
          </label>

          {error ? (
            <div className="rounded-lg border border-rose-200 bg-rose-50 px-3 py-2 text-xs text-rose-700">{error}</div>
          ) : null}
          </div>

          <div className="shrink-0 flex items-center justify-end gap-2 border-t border-gray-100 bg-white px-6 py-3">
            <button
              type="button"
              onClick={onClose}
              disabled={saving}
              className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-600 hover:bg-gray-50"
            >
              取消
            </button>
            <button
              type="submit"
              disabled={saving}
              className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:opacity-60"
            >
              {saving ? "保存中…" : "保存设置"}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

function clampPct(value: number): number {
  if (!Number.isFinite(value)) return 0;
  const fraction = value / 100;
  if (fraction < 0) return 0;
  if (fraction > 1) return 1;
  return fraction;
}

function clampUnit(value: number): number {
  if (!Number.isFinite(value)) return 0;
  if (value < 0) return 0;
  if (value > 1) return 1;
  return value;
}

interface InlineToggleProps {
  fund: MinimalFund;
  onUpdated: (updated: MinimalFund) => void;
  size?: "sm" | "md";
  showLabel?: boolean;
}

// Inline switch + gear icon block used on the Companies page.
export function AutoExecuteInlineToggle({ fund, onUpdated, size = "sm", showLabel = true }: InlineToggleProps): JSX.Element {
  const [open, setOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const cfg = useMemo(() => mergeFormDefaults(fund.autoExecute), [fund.autoExecute]);

  const handleQuickToggle = async (next: boolean) => {
    setError(null);
    setSaving(true);
    try {
      const payload: AutoExecuteConfig = {
        enabled: next,
        maxOrderPctOfAssets: cfg.maxOrderPctOfAssets,
        maxDailyPctOfAssets: cfg.maxDailyPctOfAssets,
        minConfidence: cfg.minConfidence,
        slippageBouncePolicy: cfg.slippageBouncePolicy,
        allowedMarkets: cfg.allowedMarkets,
      };
      // Preserve the user's decision-interval override across
      // enable/disable toggles so flipping the master switch off and
      // back on does not silently revert a custom intraday cadence
      // set behind the gear icon.
      const persisted = fund.autoExecute?.decisionIntervalMinutes;
      if (persisted !== undefined && persisted !== null && persisted > 0) {
        payload.decisionIntervalMinutes = persisted;
      }
      const updated = await apiPut<MinimalFund>(`/api/funds/${fund.id}`, {
        autoExecute: payload,
      });
      onUpdated(updated);
    } catch (err) {
      setError(formatApiError(err, "切换自动决策失败"));
    } finally {
      setSaving(false);
    }
  };

  return (
    <div className="flex items-center gap-2" onClick={(e) => e.stopPropagation()} onMouseDown={(e) => e.stopPropagation()}>
      {showLabel ? (
        <span className="text-[11px] text-gray-500" title="自动决策">
          自动决策
        </span>
      ) : null}
      <SwitchToggle
        enabled={cfg.enabled}
        onToggle={(next) => void handleQuickToggle(next)}
        disabled={saving}
        size={size}
        label="切换自动决策"
      />
      <button
        type="button"
        onClick={(e) => {
          e.preventDefault();
          e.stopPropagation();
          setOpen(true);
        }}
        className="rounded-md p-1 text-gray-400 transition hover:bg-gray-100 hover:text-gray-700"
        title="自动决策设置"
        aria-label="自动决策设置"
      >
        <GearIcon />
      </button>
      {error ? <span className="hidden text-[11px] text-rose-600 sm:inline">{error}</span> : null}
      <AutoExecuteSettingsModal open={open} fund={fund} onClose={() => setOpen(false)} onSaved={onUpdated} />
    </div>
  );
}

interface BadgeProps {
  fund: MinimalFund;
  onUpdated: (updated: MinimalFund) => void;
}

// Persistent badge for the FundLayout header. Always visible, clickable.
export function AutoExecuteHeaderBadge({ fund, onUpdated }: BadgeProps): JSX.Element {
  const [open, setOpen] = useState(false);
  const cfg = mergeFormDefaults(fund.autoExecute);
  const enabledTone = cfg.enabled
    ? "bg-emerald-50 text-emerald-700 ring-emerald-200"
    : "bg-gray-50 text-gray-500 ring-gray-200";
  return (
    <>
      <button
        type="button"
        onClick={() => setOpen(true)}
        className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-[11px] font-medium ring-1 transition hover:brightness-95 ${enabledTone}`}
        title="自动决策设置"
      >
        <span className={`h-1.5 w-1.5 rounded-full ${cfg.enabled ? "bg-emerald-500" : "bg-gray-400"}`} />
        自动决策：{cfg.enabled ? "开" : "关"}
        <GearIcon className="h-3 w-3" />
      </button>
      <AutoExecuteSettingsModal open={open} fund={fund} onClose={() => setOpen(false)} onSaved={onUpdated} />
    </>
  );
}
