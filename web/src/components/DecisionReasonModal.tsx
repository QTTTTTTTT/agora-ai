// DecisionReasonModal.tsx — "Why this decision?" drill-down dialog.
//
// WHY THIS EXISTS
// ---------------
// DecisionCenter shows a plan's PM reasoning, action reasons,
// discussion summary, risk review, and blocking reasons in
// SEPARATE cards scattered down the page. When an operator
// reviews a plan and asks "why is the team buying AAPL right
// now?" they end up scrolling through five sections, copy-
// pasting fragments into a notes app, and missing context that's
// implicit in the cross-references between sections.
//
// This modal aggregates EVERYTHING reason-shaped into a single
// scrollable view, lightly grouped by source (PM thesis →
// Strategy summary → Per-action reasons → Discussion / debate
// summary → Risk review → Blocking reasons → Raw JSON). The
// page passes the full plan + decision trace; we extract what
// to show with simple optional-chain reads — no extra API call.
//
// USAGE
// -----
// Pass open / onClose state and the data sources the page
// already has on hand:
//
//   <DecisionReasonModal
//     open={drillOpen}
//     onClose={() => setDrillOpen(false)}
//     language={language}
//     plan={selectedPlanDetail}
//     decisionTrace={selectedDecisionTrace}
//     blockingReasons={blockingReasons}
//     riskReview={selectedPlanDetail?.riskReview}
//   />
//
// The modal manages its own focus / Esc / overlay-click close.
//
// SCOPE
// -----
// Starter version is a single-pane scroll; future iterations
// can:
//   - render plan.actions with collapsible payload viewers,
//   - add per-section copy-to-clipboard shortcuts,
//   - embed the analyst panel votes from
//     selectedDecisionTrace.discussion.votes,
//   - add a "diff vs previous plan" view so the operator can
//     see what changed.

import React, { useEffect } from "react";

// We deliberately type the plan / trace inputs as `unknown`
// shaped via optional reads. The DecisionCenter API surface
// has lots of derived types (DecisionTrace, ApiPlan, etc.)
// from @fundai/api-client and depends on the latest server
// schema; threading those generics through this drill-down
// modal would couple it tightly to a moving target. Shaped-
// access via `any`-cast keeps the modal future-proof at the
// cost of mild type erosion at the boundary — every read goes
// through pickLang / optional chaining so a missing field
// renders as "no content" rather than crashing.

export interface DecisionReasonModalProps {
  open: boolean;
  onClose: () => void;
  language: "zh-CN" | "en-US";
  plan?: unknown;
  decisionTrace?: unknown;
  blockingReasons?: readonly string[];
}

function pickLang(
  language: "zh-CN" | "en-US",
  base?: string | null,
  zh?: string | null,
  en?: string | null,
): string | null {
  if (language === "zh-CN") {
    return (zh && zh.trim()) || (base && base.trim()) || (en && en.trim()) || null;
  }
  return (en && en.trim()) || (base && base.trim()) || (zh && zh.trim()) || null;
}

function coerceString(value: unknown): string | null {
  if (typeof value === "string") return value;
  if (Array.isArray(value)) {
    return value.map((v) => (typeof v === "string" ? v : String(v))).join("\n");
  }
  return null;
}

function coerceArray(value: unknown): string[] | null {
  if (Array.isArray(value)) {
    return value.map((v) => (typeof v === "string" ? v : String(v))).filter((v) => v.length > 0);
  }
  if (typeof value === "string" && value.length > 0) return [value];
  return null;
}

export const DecisionReasonModal: React.FC<DecisionReasonModalProps> = ({
  open,
  onClose,
  language,
  plan,
  decisionTrace,
  blockingReasons,
}) => {
  // Esc-to-close handler. We attach it only when open so we
  // don't burn a global keydown listener on every page that
  // imports this component.
  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        e.preventDefault();
        onClose();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [open, onClose]);

  if (!open) return null;

  const isEnglish = language === "en-US";

  const copy = isEnglish
    ? {
        title: "Why this decision?",
        subtitle: "All reason and reasoning sources for this plan, aggregated.",
        close: "Close",
        sectionThesis: "PM thesis",
        sectionStrategy: "Strategy summary",
        sectionActions: "Per-action reasons",
        sectionDiscussion: "Debate / discussion",
        sectionMemo: "Committee memo",
        sectionRisk: "Risk review",
        sectionBlocking: "Blocking reasons",
        sectionRawJson: "Raw JSON (for debugging)",
        empty: "No content recorded.",
        noReasons: "No reasons recorded yet for this plan.",
      }
    : {
        title: "决策依据",
        subtitle: "本计划相关的所有原因与说明集中查看。",
        close: "关闭",
        sectionThesis: "组合经理论点",
        sectionStrategy: "策略概述",
        sectionActions: "各动作原因",
        sectionDiscussion: "讨论 / 辩论",
        sectionMemo: "委员会备忘",
        sectionRisk: "风控复核",
        sectionBlocking: "阻塞原因",
        sectionRawJson: "原始 JSON（调试用）",
        empty: "暂无内容。",
        noReasons: "本计划尚未记录任何原因说明。",
      };

  // Loose-shape reads — see the comment on the props type for
  // why the modal accepts `unknown` and reaches into fields
  // through any-casts. `coerceString` and `coerceArray` collapse
  // the various server shapes (string vs string[] for consensus,
  // object vs string for fallbackReason, etc.) to the simple
  // forms this UI renders.
  const planAny = plan as Record<string, unknown> | null | undefined;
  const traceAny = decisionTrace as Record<string, unknown> | null | undefined;
  const planReasoning = planAny ?? {};
  const discussion = (traceAny?.["discussion"] as Record<string, unknown> | undefined) ?? {};
  const memoObj = (traceAny?.["memo"] as Record<string, unknown> | undefined) ?? {};
  const riskObj = (traceAny?.["risk"] as Record<string, unknown> | undefined) ?? {};
  const riskReview = (planReasoning["riskReview"] as Record<string, unknown> | undefined) ?? {};
  const planActions = (planReasoning["actions"] as Array<Record<string, unknown>> | undefined) ?? [];

  const thesis = pickLang(
    language,
    coerceString(planReasoning["reasoning"]),
    coerceString(planReasoning["reasoningZh"]),
    coerceString(planReasoning["reasoningEn"]),
  );
  const strategy = pickLang(
    language,
    coerceString(planReasoning["strategySummary"]),
    coerceString(planReasoning["strategySummaryZh"]),
    coerceString(planReasoning["strategySummaryEn"]),
  );
  const discussionReasoning = pickLang(
    language,
    coerceString(discussion["reasoning"]),
    coerceString(discussion["reasoningZh"]),
    coerceString(discussion["reasoningEn"]),
  );
  const discussionConsensus = coerceString(discussion["consensus"])?.trim();
  const memo = coerceString(memoObj["body"])?.trim();
  const risk = (coerceString(riskReview["notes"]) ?? coerceString(riskObj["explanation"]) ?? "").trim();
  const verdict = (coerceString(riskReview["verdict"]) ?? coerceString(riskObj["verdict"]) ?? "").trim();
  const fallbackReason = coerceString(planReasoning["fallbackReason"])?.trim();
  const blocking = (
    blockingReasons && blockingReasons.length > 0
      ? blockingReasons
      : (coerceArray(riskReview["blockingReasons"]) ?? [])
  ).filter(Boolean);
  const actionsWithReasons = planActions.map((a, i) => {
    const ar = a as Record<string, unknown>;
    const symbol = coerceString(ar["symbol"]) ?? "?";
    const action = coerceString(ar["action"]) ?? "?";
    return {
      id: coerceString(ar["id"]) ?? `${symbol}-${action}-${i}`,
      symbol,
      action,
      reason: pickLang(
        language,
        coerceString(ar["reasoning"]),
        coerceString(ar["reasoningZh"]),
        coerceString(ar["reasoningEn"]),
      ),
      payload: ar["payload"],
    };
  });

  const hasAnyReason =
    thesis ||
    strategy ||
    discussionReasoning ||
    discussionConsensus ||
    memo ||
    risk ||
    blocking.length > 0 ||
    fallbackReason ||
    actionsWithReasons.some((a) => a.reason);

  // Raw JSON dump of the plan + decision trace for the
  // "I'll figure it out from the source" use case.
  const rawJson = JSON.stringify({ plan, decisionTrace, blockingReasons }, null, 2);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-label={copy.title}
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4"
      onClick={onClose}
    >
      <div
        className="flex max-h-[90vh] w-full max-w-3xl flex-col overflow-hidden rounded-2xl bg-white shadow-2xl dark:bg-slate-900"
        onClick={(e) => e.stopPropagation()}
      >
        <div className="flex items-start justify-between gap-4 border-b border-gray-200 px-6 py-4 dark:border-slate-700">
          <div>
            <h2 className="text-lg font-semibold text-gray-900 dark:text-slate-100">{copy.title}</h2>
            <p className="mt-0.5 text-xs text-gray-500 dark:text-slate-400">{copy.subtitle}</p>
          </div>
          <button
            type="button"
            onClick={onClose}
            aria-label={copy.close}
            className="rounded-md p-1.5 text-gray-400 transition hover:bg-gray-100 hover:text-gray-700 dark:hover:bg-slate-800 dark:hover:text-slate-200"
          >
            ×
          </button>
        </div>
        <div className="flex-1 overflow-y-auto px-6 py-4">
          {!hasAnyReason ? (
            <p className="rounded-xl bg-gray-50 p-4 text-sm text-gray-500 dark:bg-slate-800 dark:text-slate-400">
              {copy.noReasons}
            </p>
          ) : (
            <div className="space-y-5 text-sm leading-7 text-gray-700 dark:text-slate-200">
              {thesis ? (
                <Section title={copy.sectionThesis}>
                  <p className="whitespace-pre-line">{thesis}</p>
                </Section>
              ) : null}
              {strategy ? (
                <Section title={copy.sectionStrategy}>
                  <p className="whitespace-pre-line">{strategy}</p>
                </Section>
              ) : null}
              {actionsWithReasons.length > 0 ? (
                <Section title={copy.sectionActions}>
                  <ul className="space-y-2">
                    {actionsWithReasons.map((a) => (
                      <li
                        key={a.id}
                        className="rounded-lg border border-gray-200 p-3 dark:border-slate-700"
                      >
                        <div className="flex items-center gap-2">
                          <span className="rounded-full bg-indigo-100 px-2 py-0.5 text-xs font-medium text-indigo-700 dark:bg-indigo-900/40 dark:text-indigo-200">
                            {a.action.toUpperCase()}
                          </span>
                          <span className="text-sm font-medium text-gray-900 dark:text-slate-100">{a.symbol}</span>
                        </div>
                        <p className="mt-1.5 whitespace-pre-line text-xs leading-relaxed text-gray-600 dark:text-slate-300">
                          {a.reason ?? copy.empty}
                        </p>
                      </li>
                    ))}
                  </ul>
                </Section>
              ) : null}
              {discussionReasoning || discussionConsensus ? (
                <Section title={copy.sectionDiscussion}>
                  {discussionConsensus ? (
                    <p className="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-slate-400">
                      {discussionConsensus}
                    </p>
                  ) : null}
                  {discussionReasoning ? (
                    <p className="mt-1 whitespace-pre-line">{discussionReasoning}</p>
                  ) : null}
                </Section>
              ) : null}
              {memo ? (
                <Section title={copy.sectionMemo}>
                  <p className="whitespace-pre-line">{memo}</p>
                </Section>
              ) : null}
              {risk || verdict ? (
                <Section title={copy.sectionRisk}>
                  {verdict ? (
                    <p className="text-xs font-semibold uppercase tracking-wide text-gray-500 dark:text-slate-400">
                      {verdict}
                    </p>
                  ) : null}
                  {risk ? <p className="mt-1 whitespace-pre-line">{risk}</p> : null}
                </Section>
              ) : null}
              {blocking.length > 0 ? (
                <Section title={copy.sectionBlocking}>
                  <ul className="list-disc space-y-1 pl-5 text-amber-700 dark:text-amber-300">
                    {blocking.map((reason, i) => (
                      <li key={`${reason}-${i}`}>{reason}</li>
                    ))}
                  </ul>
                </Section>
              ) : null}
              {fallbackReason ? (
                <Section title={isEnglish ? "Fallback reason" : "回退原因"}>
                  <p className="whitespace-pre-line">{fallbackReason}</p>
                </Section>
              ) : null}
              <Section title={copy.sectionRawJson}>
                <pre className="max-h-72 overflow-auto rounded-lg bg-slate-900 p-3 text-[11px] leading-relaxed text-slate-200">
                  {rawJson}
                </pre>
              </Section>
            </div>
          )}
        </div>
      </div>
    </div>
  );
};

const Section: React.FC<{ title: string; children: React.ReactNode }> = ({ title, children }) => (
  <div>
    <h3 className="mb-2 text-xs font-semibold uppercase tracking-wider text-gray-500 dark:text-slate-400">
      {title}
    </h3>
    {children}
  </div>
);
