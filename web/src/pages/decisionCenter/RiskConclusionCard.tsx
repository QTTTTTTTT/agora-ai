import React from "react";
import { type RiskExplanation } from "../../lib/api";
import { formatNumberForLanguage, type AppLanguage } from "../../lib/preferences";
import { humanizeValue } from "./helpers";
import type { RiskReviewView } from "./types";

export interface RiskConclusionLabels {
  riskConclusion: string;
  riskExplanation: string;
  blockingReasons: string;
  riskWarnings: string;
  adjustmentAdvice: string;
  userImpact: string;
  currentValue: string;
  thresholdValue: string;
  noRiskChecks: string;
  noRiskConclusion: string;
  missingCheckDetail: string;
  checkName: (index: number) => string;
  unknown: string;
}

interface RiskConclusionCardProps {
  riskExplanation?: RiskExplanation;
  riskReview: RiskReviewView | null;
  language: AppLanguage;
  labels: RiskConclusionLabels;
  riskVerdictMeta: (value: string) => { label: string; badge: string };
  checkMeta: (result: string) => { label: string; badge: string };
}

// RiskConclusionCard renders the largest static block in DecisionCenter
// (~95 lines of JSX, optionally nested .map() over rule-by-rule check
// breakdowns). Extracting it and wrapping in React.memo means:
//
//  1. Typing in the approval reject-reason textarea no longer forces
//     React to walk this entire subtree.
//  2. The inner `checks.map(...)` only runs when the underlying
//     riskExplanation/riskReview reference changes — i.e. the user
//     switches plans or the backend pushes a new decision trace.
//
// The component intentionally takes the lookup callbacks (riskVerdictMeta,
// checkMeta) as props rather than re-deriving them from `copy` here. That
// keeps the language/copy logic centralised in DecisionCenter and matches
// the pattern used by ActionListCard/TraceboardCard/CommitteeMemoCard.
function RiskConclusionCardInner({
  riskExplanation,
  riskReview,
  language,
  labels,
  riskVerdictMeta,
  checkMeta,
}: RiskConclusionCardProps) {
  return (
    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <div className="flex items-center justify-between gap-4">
        <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{labels.riskConclusion}</h3>
        {riskExplanation?.verdict || riskReview ? (
          <span
            className={`rounded-full border px-3 py-1 text-xs font-medium ${riskVerdictMeta(riskExplanation?.verdict ?? riskReview?.verdict ?? "pending").badge}`}
          >
            {riskVerdictMeta(riskExplanation?.verdict ?? riskReview?.verdict ?? "pending").label}
          </span>
        ) : null}
      </div>
      {riskExplanation ? (
        <div className="mt-4 space-y-4">
          <div
            className={`rounded-xl border p-4 text-sm leading-7 ${
              riskExplanation.severity === "block"
                ? "border-red-200 bg-red-50 text-red-800"
                : riskExplanation.severity === "warning"
                  ? "border-amber-200 bg-amber-50 text-amber-800"
                  : "border-emerald-200 bg-emerald-50 text-emerald-800"
            }`}
          >
            <p className="font-semibold">{labels.riskExplanation}</p>
            <p className="mt-1 whitespace-pre-line">{riskExplanation.summary || riskReview?.note || labels.noRiskConclusion}</p>
          </div>

          {riskExplanation.blockingReasons?.length ? (
            <div className="rounded-xl border border-red-100 bg-red-50/70 p-4">
              <p className="text-sm font-semibold text-red-800">{labels.blockingReasons}</p>
              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm leading-6 text-red-700">
                {riskExplanation.blockingReasons.map((item) => (
                  <li key={item}>{item}</li>
                ))}
              </ul>
            </div>
          ) : null}

          {riskExplanation.warnings?.length ? (
            <div className="rounded-xl border border-amber-100 bg-amber-50/70 p-4">
              <p className="text-sm font-semibold text-amber-800">{labels.riskWarnings}</p>
              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm leading-6 text-amber-700">
                {riskExplanation.warnings.map((item) => (
                  <li key={item}>{item}</li>
                ))}
              </ul>
            </div>
          ) : null}

          {riskExplanation.adjustmentAdvice?.length ? (
            <div className="rounded-xl border border-blue-100 bg-blue-50/70 p-4">
              <p className="text-sm font-semibold text-blue-800">{labels.adjustmentAdvice}</p>
              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm leading-6 text-blue-700">
                {riskExplanation.adjustmentAdvice.map((item) => (
                  <li key={item}>{item}</li>
                ))}
              </ul>
            </div>
          ) : null}

          {riskExplanation.checks?.length ? (
            <div className="space-y-3">
              {riskExplanation.checks.map((check, index) => {
                const meta = checkMeta(check.status ?? check.severity ?? "pass");
                return (
                  <div key={`${check.ruleCode ?? check.ruleName ?? "risk"}-${index}`} className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                    <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                      <div>
                        <p className="font-medium text-gray-900">{check.ruleName || humanizeValue(check.ruleCode, labels.checkName(index + 1))}</p>
                        <p className="mt-1 text-sm text-gray-600">{check.explanation || labels.missingCheckDetail}</p>
                      </div>
                      <span className={`inline-flex rounded-full border px-3 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                    </div>
                    <div className="mt-3 flex flex-wrap gap-3 text-xs text-gray-500">
                      {typeof check.current === "number" ? (
                        <span>
                          {labels.currentValue}: {formatNumberForLanguage(check.current, language, { maximumFractionDigits: 4 })}
                        </span>
                      ) : null}
                      {typeof check.threshold === "number" ? (
                        <span>
                          {labels.thresholdValue}: {formatNumberForLanguage(check.threshold, language, { maximumFractionDigits: 4 })}
                        </span>
                      ) : null}
                    </div>
                    {check.userImpact ? (
                      <p className="mt-2 text-sm text-gray-700">
                        <span className="font-medium text-gray-900">{labels.userImpact}:</span> {check.userImpact}
                      </p>
                    ) : null}
                    {check.adjustmentHint ? (
                      <p className="mt-1 text-sm text-blue-700">
                        <span className="font-medium">{labels.adjustmentAdvice}:</span> {check.adjustmentHint}
                      </p>
                    ) : null}
                  </div>
                );
              })}
            </div>
          ) : null}
        </div>
      ) : riskReview ? (
        <>
          <div className="mt-4 space-y-3">
            {riskReview.checks.length > 0 ? (
              riskReview.checks.map((check) => {
                const meta = checkMeta(check.result);
                return (
                  <div key={check.id} className="flex flex-col gap-3 rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 sm:flex-row sm:items-start sm:justify-between">
                    <div>
                      <p className="font-medium text-gray-900">{check.name}</p>
                      <p className="mt-1 text-sm text-gray-600">{check.detail}</p>
                    </div>
                    <span className={`inline-flex rounded-full border px-3 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                  </div>
                );
              })
            ) : (
              <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{labels.noRiskChecks}</div>
            )}
          </div>
          <div className="mt-4 whitespace-pre-line rounded-xl bg-gray-50 p-4 text-sm leading-7 text-gray-700">{riskReview.note}</div>
        </>
      ) : (
        <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{labels.noRiskConclusion}</div>
      )}
    </div>
  );
}

export const RiskConclusionCard = React.memo(RiskConclusionCardInner);
