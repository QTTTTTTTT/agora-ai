import React, { useEffect, useMemo, useState } from "react";
import { formatDateTimeForLanguage, formatNumberForLanguage, type AppLanguage } from "../../lib/preferences";
import Pagination from "../../components/Pagination";
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

// HISTORY_PAGE_SIZE keeps each card column compact enough that the
// committee never has to scroll a fund's whole rejection log inside
// the sidebar — they can page through 8 entries at a time and still
// see the action panel on the right without the page going wild.
const HISTORY_PAGE_SIZE = 8;

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
  const [historyPage, setHistoryPage] = useState(0);
  const historyPageCount = historyPlans.length === 0 ? 0 : Math.ceil(historyPlans.length / HISTORY_PAGE_SIZE);

  // Clamp the page index when the list shrinks (e.g. after a fund
  // switch wipes the historical buffer) so we never render an empty
  // page with a "Next" button that points nowhere.
  useEffect(() => {
    if (historyPageCount === 0 && historyPage !== 0) {
      setHistoryPage(0);
      return;
    }
    if (historyPage >= historyPageCount && historyPageCount > 0) {
      setHistoryPage(historyPageCount - 1);
    }
  }, [historyPageCount, historyPage]);

  // Keep the selected plan visible: if the operator clicks an item on
  // page 3 and the parent then re-fetches plans (status changed,
  // approval landed) the new sort might push that item to page 4.
  // We snap the pager so the selected card stays on screen.
  useEffect(() => {
    if (!selectedId || historyPlans.length === 0) return;
    const index = historyPlans.findIndex((plan) => plan.id === selectedId);
    if (index < 0) return;
    const targetPage = Math.floor(index / HISTORY_PAGE_SIZE);
    if (targetPage !== historyPage) {
      setHistoryPage(targetPage);
    }
    // We deliberately don't depend on `historyPage` here — that would
    // bounce the page back any time the user manually advances away
    // from the selected plan. We only correct on plan-list mutations.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [selectedId, historyPlans]);

  const visibleHistoryPlans = useMemo(() => {
    if (historyPlans.length === 0) return [] as ApiPlan[];
    const start = historyPage * HISTORY_PAGE_SIZE;
    return historyPlans.slice(start, start + HISTORY_PAGE_SIZE);
  }, [historyPlans, historyPage]);

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
            <>
              <div className="mt-4 space-y-3">
                {visibleHistoryPlans.map((plan) => {
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
              {historyPageCount > 1 ? (
                <div className="mt-4 border-t border-gray-100 pt-3">
                  <Pagination
                    page={historyPage}
                    pageCount={historyPageCount}
                    pageSize={HISTORY_PAGE_SIZE}
                    totalItems={historyPlans.length}
                    language={language}
                    onPageChange={setHistoryPage}
                    align="center"
                    size="sm"
                  />
                </div>
              ) : null}
            </>
          )
        ) : null}
      </div>
    </aside>
  );
}

export const PlanListSidebar = React.memo(PlanListSidebarInner);
