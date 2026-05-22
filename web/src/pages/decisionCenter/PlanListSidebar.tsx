import React from "react";
import { formatDateTimeForLanguage, formatNumberForLanguage, type AppLanguage } from "../../lib/preferences";
import { pickLocalizedText } from "./helpers";
import type { ApiPlan } from "./types";

export interface PlanListSidebarLabels {
  pendingPlans: string;
  noPendingPlans: string;
  historyPlans: string;
  collapse: string;
  expand: string;
  noHistoryPlans: string;
  actionCount: string;
  expectedReturn: string;
  riskScore: string;
}

interface PlanListSidebarProps {
  pendingPlans: ApiPlan[];
  historyPlans: ApiPlan[];
  selectedId: string | null;
  showHistory: boolean;
  language: AppLanguage;
  labels: PlanListSidebarLabels;
  planStatusMeta: (status: string, riskReview?: unknown) => { label: string; badge: string };
  planTitle: (plan: ApiPlan) => string;
  riskScoreTone: (value?: number) => string;
  formatPercent: (value?: number, digits?: number) => string;
  onSelectPlan: (planId: string) => void;
  onToggleHistory: () => void;
}

function PlanListSidebarInner({
  pendingPlans,
  historyPlans,
  selectedId,
  showHistory,
  language,
  labels,
  planStatusMeta,
  planTitle,
  riskScoreTone,
  formatPercent,
  onSelectPlan,
  onToggleHistory,
}: PlanListSidebarProps) {
  return (
    <aside className="space-y-4">
      <div>
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{labels.pendingPlans}</h2>
        {pendingPlans.length === 0 ? (
          <div className="rounded-xl border border-dashed border-gray-200 bg-white p-6 text-sm text-gray-500">{labels.noPendingPlans}</div>
        ) : (
          <div className="space-y-3">
            {pendingPlans.map((plan) => {
              const meta = planStatusMeta(plan.status, plan.riskReview);
              const isSelected = selectedId === plan.id;
              return (
                <button
                  key={plan.id}
                  onClick={() => onSelectPlan(plan.id)}
                  className={`w-full rounded-xl border bg-white p-4 text-left shadow-sm transition hover:border-indigo-300 hover:bg-indigo-50/30 ${
                    isSelected ? "border-indigo-500 ring-1 ring-indigo-500/30" : "border-gray-200"
                  }`}
                >
                  <div className="flex items-start justify-between gap-3">
                    <p className="font-semibold text-gray-900">{planTitle(plan)}</p>
                    <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                  </div>
                  <p className="mt-2 text-xs text-gray-500">{formatDateTimeForLanguage(plan.tradingDate ?? plan.createdAt, language)}</p>
                  <div className="mt-4 grid grid-cols-3 gap-2 text-center text-xs">
                    <div className="rounded-lg bg-gray-50 px-2 py-2">
                      <p className="font-semibold text-gray-900">{formatNumberForLanguage(plan.actions?.length ?? 0, language)}</p>
                      <p className="mt-1 text-gray-500">{labels.actionCount}</p>
                    </div>
                    <div className="rounded-lg bg-gray-50 px-2 py-2">
                      <p className="font-semibold text-emerald-600">{formatPercent(plan.expectedReturn)}</p>
                      <p className="mt-1 text-gray-500">{labels.expectedReturn}</p>
                    </div>
                    <div className="rounded-lg bg-gray-50 px-2 py-2">
                      <p className={`font-semibold ${riskScoreTone(plan.riskScore)}`}>{formatPercent(plan.riskScore, 0)}</p>
                      <p className="mt-1 text-gray-500">{labels.riskScore}</p>
                    </div>
                  </div>
                </button>
              );
            })}
          </div>
        )}
      </div>

      <div className="rounded-xl border border-gray-200 bg-white p-4 shadow-sm">
        <button
          onClick={onToggleHistory}
          className="flex w-full items-center justify-between text-sm font-semibold uppercase tracking-wider text-gray-500 hover:text-gray-700"
        >
          <span>{labels.historyPlans}</span>
          <span>{showHistory ? labels.collapse : labels.expand}</span>
        </button>
        {showHistory ? (
          historyPlans.length === 0 ? (
            <p className="mt-4 text-sm text-gray-500">{labels.noHistoryPlans}</p>
          ) : (
            <div className="mt-4 space-y-3">
              {historyPlans.map((plan) => {
                const meta = planStatusMeta(plan.status, plan.riskReview);
                const isSelected = selectedId === plan.id;
                return (
                  <button
                    key={plan.id}
                    onClick={() => onSelectPlan(plan.id)}
                    className={`w-full rounded-xl border p-4 text-left transition ${
                      isSelected ? "border-indigo-500 bg-indigo-50/40" : "border-gray-200 hover:border-gray-300"
                    }`}
                  >
                    <div className="flex items-start justify-between gap-3">
                      <p className="font-medium text-gray-900">{planTitle(plan)}</p>
                      <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                    </div>
                    <p className="mt-2 text-xs text-gray-500">{formatDateTimeForLanguage(plan.updatedAt, language)}</p>
                    {plan.status === "rejected" && pickLocalizedText(language, plan.reasoning, plan.reasoningZh, plan.reasoningEn) ? (
                      <p className="mt-2 line-clamp-2 text-xs text-red-600">{pickLocalizedText(language, plan.reasoning, plan.reasoningZh, plan.reasoningEn)}</p>
                    ) : null}
                  </button>
                );
              })}
            </div>
          )
        ) : null}
      </div>
    </aside>
  );
}

export const PlanListSidebar = React.memo(PlanListSidebarInner);
