import React, { useEffect, useMemo, useState } from "react";
import { apiGet, fetchNextWorkflowRun, formatApiError, NextWorkflowRun } from "../lib/api";
import { formatDateTimeForLanguage, type AppLanguage } from "../lib/preferences";
import type { ApiPlan } from "../pages/decisionCenter/types";

interface BannerProps {
  fundId?: string;
  language: AppLanguage;
}

interface BannerCopy {
  title: string;
  inWindow: string;
  countdownPrefix: string;
  countdownUnitDay: string;
  countdownUnitHour: string;
  countdownUnitMinute: string;
  triggerAt: string;
  tradingDate: string;
  timezone: string;
  scheduleHeader: string;
  intervalScheduleHeader: string;
  intervalSummary: (minutes: number, slots: number) => string;
  slotPast: string;
  slotUpcoming: string;
  slotRunning: string;
  slotPlanCompleted: string;
  slotPlanRejected: string;
  slotPlanPending: string;
  slotPlanMissing: string;
  loading: string;
  errorPrefix: string;
  unavailableTitle: string;
  unavailableHint: string;
  stepLabels: Record<string, string>;
}

const COPY: Record<AppLanguage, BannerCopy> = {
  "zh-CN": {
    title: "下次自动决策",
    inWindow: "当前在交易窗口，agent 正在/即将运行",
    countdownPrefix: "还有",
    countdownUnitDay: "天",
    countdownUnitHour: "小时",
    countdownUnitMinute: "分",
    triggerAt: "起跑时间",
    tradingDate: "交易日",
    timezone: "时区",
    scheduleHeader: "当日完整时间表",
    intervalScheduleHeader: "当日决策触发槽",
    intervalSummary: (minutes, slots) =>
      `每 ${minutes} 分钟一次 · 当日共 ${slots} 个触发槽`,
    slotPast: "已过",
    slotUpcoming: "下次",
    slotRunning: "运行中",
    slotPlanCompleted: "已出方案",
    slotPlanRejected: "已驳回",
    slotPlanPending: "待审批",
    slotPlanMissing: "未触发",
    loading: "正在读取下次自动决策时间…",
    errorPrefix: "读取失败",
    unavailableTitle: "排程信息不可用",
    unavailableHint: "服务端 calendar 服务未就绪，下次自动决策时间暂不可见。",
    stepLabels: {
      macroBrief: "宏观简报",
      researchParallel: "并行研究",
      quantSignals: "量化信号",
      roundtable: "圆桌会议",
      pmPlan: "PM 出方案",
      riskReview: "风控复核",
      userApproval: "用户审批",
      tradeExecution: "下单执行",
      settlement: "结算",
      dailyReview: "日终复盘",
    },
  },
  "en-US": {
    title: "Next automated decision",
    inWindow: "Inside the trading window; the agent is running or about to.",
    countdownPrefix: "in",
    countdownUnitDay: "d",
    countdownUnitHour: "h",
    countdownUnitMinute: "m",
    triggerAt: "Trigger at",
    tradingDate: "Trading date",
    timezone: "Timezone",
    scheduleHeader: "Full day schedule",
    intervalScheduleHeader: "Today's decision slots",
    intervalSummary: (minutes, slots) =>
      `Every ${minutes} min · ${slots} slots today`,
    slotPast: "past",
    slotUpcoming: "next",
    slotRunning: "running",
    slotPlanCompleted: "plan ready",
    slotPlanRejected: "rejected",
    slotPlanPending: "awaiting review",
    slotPlanMissing: "no plan",
    loading: "Loading next-run schedule…",
    errorPrefix: "Failed to load",
    unavailableTitle: "Schedule unavailable",
    unavailableHint: "Backend calendar service is not wired; next-run preview is hidden.",
    stepLabels: {
      macroBrief: "Macro brief",
      researchParallel: "Research",
      quantSignals: "Quant signals",
      roundtable: "Roundtable",
      pmPlan: "PM plan",
      riskReview: "Risk review",
      userApproval: "User approval",
      tradeExecution: "Trade exec",
      settlement: "Settlement",
      dailyReview: "Daily review",
    },
  },
};

// Returns "1d 04h 23m", "4h 23m", "23m", or "<1m" — whichever the
// largest non-zero unit is. Keeps the banner compact.
function formatCountdown(ms: number, copy: BannerCopy): string {
  if (ms <= 0) {
    return `<1${copy.countdownUnitMinute}`;
  }
  const totalMinutes = Math.floor(ms / 60_000);
  const days = Math.floor(totalMinutes / (60 * 24));
  const hours = Math.floor((totalMinutes % (60 * 24)) / 60);
  const minutes = totalMinutes % 60;
  const parts: string[] = [];
  if (days > 0) parts.push(`${days}${copy.countdownUnitDay}`);
  if (hours > 0 || days > 0) parts.push(`${String(hours).padStart(days > 0 ? 2 : 1, "0")}${copy.countdownUnitHour}`);
  parts.push(`${String(minutes).padStart(2, "0")}${copy.countdownUnitMinute}`);
  return parts.join(" ");
}

const STEP_KEYS = [
  "macroBrief",
  "researchParallel",
  "quantSignals",
  "roundtable",
  "pmPlan",
  "riskReview",
  "userApproval",
  "tradeExecution",
  "settlement",
  "dailyReview",
] as const;

// SlotPlanInfo tags one daily decision slot with the plan that
// was produced by it (if any). The pairing rule:
//   plan.created_at ∈ [slot, slot + intervalMinutes)
// matches the scheduler's invariant — the PM agent always
// produces its plan after the slot fires and before the next
// slot. A plan late by more than a full interval is most likely
// a stuck workflow, so we surface it as "running" rather than
// pairing it with the next slot.
interface SlotPlanInfo {
  plan: ApiPlan | null;
  // true when this slot has already passed but no plan was
  // produced before the next slot (or before "now" if it's the
  // most recent past slot). Used to surface skipped slots.
  missing: boolean;
}

function buildSlotPlanMap(
  slots: string[],
  intervalMinutes: number,
  plans: ApiPlan[],
  now: number,
): Map<string, SlotPlanInfo> {
  const out = new Map<string, SlotPlanInfo>();
  // Pre-sort plans by created_at ASC so we can scan once.
  const sortedPlans = plans
    .filter((plan) => typeof plan.createdAt === "string" && plan.createdAt.length > 0)
    .slice()
    .sort((a, b) => a.createdAt.localeCompare(b.createdAt));
  const intervalMs = Math.max(intervalMinutes, 1) * 60 * 1000;
  for (const slotISO of slots) {
    const slotMs = new Date(slotISO).getTime();
    const windowEnd = slotMs + intervalMs;
    const matched = sortedPlans.find((plan) => {
      const created = new Date(plan.createdAt).getTime();
      return created >= slotMs && created < windowEnd;
    });
    const isPast = slotMs <= now;
    // A slot is "missing" only when it's a fully-past slot
    // (its window has elapsed) with no plan — that's an actual
    // skip. The most recent past slot whose window hasn't
    // elapsed yet might still produce a plan, so we don't flag
    // it as missing.
    const windowElapsed = windowEnd <= now;
    out.set(slotISO, {
      plan: matched ?? null,
      missing: isPast && windowElapsed && !matched,
    });
  }
  return out;
}

const NextRunBanner: React.FC<BannerProps> = ({ fundId, language }) => {
  const copy = COPY[language];
  const [data, setData] = useState<NextWorkflowRun | null>(null);
  const [plans, setPlans] = useState<ApiPlan[]>([]);
  const [unavailable, setUnavailable] = useState(false);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [now, setNow] = useState(() => Date.now());
  // Default-expanded: users complained the cadence was invisible
  // because the "Next decision at 13:30" headline made the 30-min
  // interval look like a once-a-day schedule. Showing the slot
  // grid by default makes the past/upcoming runs obvious.
  const [showSchedule, setShowSchedule] = useState(true);

  useEffect(() => {
    if (!fundId) {
      setLoading(false);
      return;
    }
    let cancelled = false;
    setLoading(true);
    setError(null);
    setUnavailable(false);
    // Fetch next-run + today's plans in parallel so we can show
    // a slot ↔ plan map. plans is non-critical: failures fall
    // back to "no plan info" which still renders the past/next
    // labels correctly.
    Promise.all([
      fetchNextWorkflowRun(fundId),
      apiGet<ApiPlan[]>(`/api/funds/${fundId}/plans?limit=50`).catch((err) => {
        console.warn("plans fetch failed in NextRunBanner", err);
        return [] as ApiPlan[];
      }),
    ])
      .then(([response, planList]) => {
        if (cancelled) return;
        if (response == null) {
          setUnavailable(true);
        } else {
          setData(response);
        }
        setPlans(Array.isArray(planList) ? planList : []);
      })
      .catch((err) => {
        if (!cancelled) setError(formatApiError(err, copy.errorPrefix));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [fundId, copy.errorPrefix]);

  useEffect(() => {
    const handle = window.setInterval(() => setNow(Date.now()), 30_000);
    return () => window.clearInterval(handle);
  }, []);

  // IMPORTANT: every hook must be called on every render path,
  // otherwise React throws #310 ("Rendered more hooks than
  // during the previous render"). The four early returns
  // below (no fundId / loading / error / unavailable) used to
  // skip past this useMemo — when the banner transitioned from
  // loading=true to loading=false the hook count jumped and
  // the whole page crashed. Keep all hooks ABOVE any
  // conditional return.
  const intervalSlots = data && Array.isArray(data.slots) ? data.slots : null;
  const intervalMinutes = data && typeof data.intervalMinutes === "number" ? data.intervalMinutes : null;
  const slotPlanMap = useMemo(() => {
    if (!intervalSlots || intervalMinutes == null) return new Map<string, SlotPlanInfo>();
    return buildSlotPlanMap(intervalSlots, intervalMinutes, plans, now);
  }, [intervalSlots, intervalMinutes, plans, now]);

  if (!fundId) return null;
  if (loading) {
    return (
      <div className="rounded-xl border border-indigo-100 bg-indigo-50/60 px-4 py-3 text-sm text-indigo-700">
        {copy.loading}
      </div>
    );
  }
  if (error) {
    return (
      <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-800">
        {error}
      </div>
    );
  }
  if (unavailable || !data) {
    return (
      <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 text-sm text-gray-600">
        <span className="font-semibold">{copy.unavailableTitle}</span>
        <span className="ml-2 text-gray-500">{copy.unavailableHint}</span>
      </div>
    );
  }

  const triggerMs = new Date(data.nextTriggerAt).getTime();
  const inWindow = data.currentlyInWindow;
  const countdown = inWindow ? null : formatCountdown(triggerMs - now, copy);
  // Two render modes for the collapsible panel:
  //   1. interval mode  — fund runs every N minutes; show all daily
  //      slots so the operator can confirm cadence + see how many
  //      ticks remain today.
  //   2. legacy 10-step — show the fixed once-a-day workflow timeline.
  const hasIntervalView = intervalSlots !== null && intervalMinutes !== null;
  const hasStepView = !hasIntervalView && data.steps != null;
  const expandable = hasIntervalView || hasStepView;
  const panelHeader = hasIntervalView ? copy.intervalScheduleHeader : copy.scheduleHeader;

  return (
    <div className="rounded-xl border border-indigo-200 bg-gradient-to-r from-indigo-50 via-white to-emerald-50 px-5 py-4 shadow-sm">
      <div className="flex flex-wrap items-baseline justify-between gap-3">
        <div>
          <p className="text-xs font-semibold uppercase tracking-[0.2em] text-indigo-700">{copy.title}</p>
          {inWindow ? (
            <p className="mt-1 text-base font-semibold text-emerald-700">{copy.inWindow}</p>
          ) : (
            <p className="mt-1 text-base font-semibold text-gray-900">
              <span className="text-indigo-700">{copy.countdownPrefix} {countdown}</span>
              <span className="ml-3 text-sm font-normal text-gray-600">
                {copy.triggerAt}: {formatDateTimeForLanguage(data.nextTriggerAt, language)}
              </span>
            </p>
          )}
          <p className="mt-1 text-xs text-gray-500">
            {copy.tradingDate}: {data.tradingDate}
            {data.timezone ? <span className="ml-3">{copy.timezone}: {data.timezone}</span> : null}
            {hasIntervalView && intervalSlots && intervalMinutes != null ? (
              <span className="ml-3 font-medium text-indigo-700">
                {copy.intervalSummary(intervalMinutes, intervalSlots.length)}
              </span>
            ) : null}
          </p>
        </div>
        {expandable ? (
          <button
            type="button"
            onClick={() => setShowSchedule((v) => !v)}
            className="rounded-md border border-indigo-300 bg-white px-3 py-1.5 text-xs font-semibold text-indigo-700 hover:bg-indigo-100"
          >
            {showSchedule ? "▲" : "▼"} {panelHeader}
          </button>
        ) : null}
      </div>
      {showSchedule && hasIntervalView && intervalSlots ? (
        <div className="mt-4 grid gap-2 sm:grid-cols-3 lg:grid-cols-6">
          {intervalSlots.map((slot) => {
            const slotMs = new Date(slot).getTime();
            const past = slotMs <= now;
            const isNext = !past && slotMs === triggerMs;
            const info = slotPlanMap.get(slot);
            const plan = info?.plan ?? null;
            // Pick the dominant visual tone and the secondary
            // status label. Priority:
            //   1. Has plan → tint + plan-status label (overrides
            //      "past" so a completed slot is obviously
            //      green/red, not grey).
            //   2. Past + missing → amber warning (skipped slot).
            //   3. Next upcoming → indigo.
            //   4. Future → neutral.
            //   5. Past + no info yet → neutral grey (legacy).
            const planStatus = plan?.status ?? "";
            let tone = "border-indigo-100 bg-white text-gray-700";
            let badgeLabel = "";
            let badgeTone = "text-gray-500";
            if (plan) {
              if (planStatus === "completed" || planStatus === "approved") {
                tone = "border-emerald-300 bg-emerald-50 text-emerald-800";
                badgeLabel = copy.slotPlanCompleted;
                badgeTone = "text-emerald-700";
              } else if (planStatus === "rejected") {
                tone = "border-red-300 bg-red-50 text-red-800";
                badgeLabel = copy.slotPlanRejected;
                badgeTone = "text-red-700";
              } else if (planStatus === "pending" || planStatus === "pending_user") {
                tone = "border-amber-300 bg-amber-50 text-amber-800";
                badgeLabel = copy.slotPlanPending;
                badgeTone = "text-amber-700";
              } else {
                tone = "border-gray-300 bg-gray-50 text-gray-700";
                badgeLabel = planStatus;
                badgeTone = "text-gray-600";
              }
            } else if (past && info?.missing) {
              tone = "border-amber-300 bg-amber-50 text-amber-800";
              badgeLabel = copy.slotPlanMissing;
              badgeTone = "text-amber-700";
            } else if (isNext) {
              tone = "border-indigo-400 bg-indigo-50 text-indigo-800";
              badgeLabel = copy.slotUpcoming;
              badgeTone = "text-indigo-700";
            } else if (past) {
              tone = "border-gray-200 bg-gray-50 text-gray-500";
              badgeLabel = copy.slotPast;
              badgeTone = "text-gray-500";
            }
            return (
              <div key={slot} className={`rounded-md border px-2.5 py-2 text-xs ${tone}`}>
                <div className={`flex items-center justify-between font-semibold text-[11px] uppercase tracking-wide ${badgeTone}`}>
                  <span>{badgeLabel || (past ? copy.slotPast : "")}</span>
                  {plan ? <span className="font-mono text-[10px] opacity-70">#{plan.id.slice(0, 6)}</span> : null}
                </div>
                <div className="mt-0.5 font-medium">{formatDateTimeForLanguage(slot, language)}</div>
                {plan ? (
                  <div className="mt-1 text-[10px] text-gray-500">
                    {language === "zh-CN" ? "出方案于 " : "PM at "}
                    {formatDateTimeForLanguage(plan.createdAt, language).split(" ").slice(-1)[0]}
                  </div>
                ) : null}
              </div>
            );
          })}
        </div>
      ) : null}
      {showSchedule && hasStepView && data.steps ? (
        <div className="mt-4 grid gap-2 sm:grid-cols-2 lg:grid-cols-5">
          {STEP_KEYS.map((key) => {
            const value = data.steps?.[key];
            if (!value) return null;
            const past = new Date(value).getTime() <= now;
            return (
              <div
                key={key}
                className={`rounded-md border px-2.5 py-2 text-xs ${past ? "border-gray-200 bg-gray-50 text-gray-400" : "border-indigo-100 bg-white text-gray-700"}`}
              >
                <div className="font-semibold text-[11px] uppercase tracking-wide text-gray-500">{copy.stepLabels[key]}</div>
                <div className="mt-0.5">{formatDateTimeForLanguage(value, language)}</div>
              </div>
            );
          })}
        </div>
      ) : null}
    </div>
  );
};

export default NextRunBanner;
