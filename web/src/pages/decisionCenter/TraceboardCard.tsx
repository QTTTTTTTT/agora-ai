import React from "react";
import type { DecisionTrace, DecisionTraceReviewEntry, DecisionTraceStep } from "../../lib/api";
import {
  formatDateTimeForLanguage,
  formatMoneyForDisplay,
  type AppLanguage,
  type DisplayCurrency,
} from "../../lib/preferences";
import { renderLesson } from "../../lib/lessonRenderer";
import { MetaBadge } from "./MetaBadge";
import { humanizeValue } from "./helpers";
import type { ExecutionTraceView } from "./types";

export interface TraceboardCardLabels {
  traceboard: string;
  traceboardSubtitle: string;
  loadingTraceboard: string;
  loadingTraceboardHint: string;
  workflowTimeline: string;
  workflowState: string;
  workflowCurrentStep: string;
  workflowStartedAt: string;
  workflowCompletedAt: string;
  workflowUpdatedAt: string;
  workflowError: string;
  workflowNoData: string;
  discussionTrace: string;
  discussionSummary: string;
  discussionConsensus: string;
  discussionReasoning: string;
  discussionSnapshot: string;
  discussionFallback: string;
  executionTrace: string;
  executionStatusLabel: string;
  tradeId: string;
  tradeSide: string;
  tradeStatus: string;
  filledQuantity: string;
  filledPrice: string;
  orderType: string;
  executedAt: string;
  traceNoTrades: string;
  traceNoExecution: string;
  reviewMemory: string;
  reviewLayer: string;
  reviewUpdatedAt: string;
  reviewNoEntries: string;
  notRecorded: string;
}

interface TraceboardCardProps {
  decisionTrace: DecisionTrace | null | undefined;
  workflowSteps: DecisionTraceStep[];
  discussionSummary: string;
  discussionConsensus: string[];
  discussionReasoning: string;
  discussionSnapshot: string;
  executionTraceRows: ExecutionTraceView[];
  reviewEntries: DecisionTraceReviewEntry[];
  isInitialLoading: boolean;
  language: AppLanguage;
  displayCurrency: DisplayCurrency;
  labels: TraceboardCardLabels;
  workflowStepLabel: (value?: string) => string;
  traceStepStatusMeta: (value?: string) => { label: string; badge: string };
  actionMeta: (action: string) => { label: string; color: string };
  actionExecutionStatusLabel: (value?: string) => string;
  formatQuantity: (value?: number) => string;
}

function TraceboardCardInner({
  decisionTrace,
  workflowSteps,
  discussionSummary,
  discussionConsensus,
  discussionReasoning,
  discussionSnapshot,
  executionTraceRows,
  reviewEntries,
  isInitialLoading,
  language,
  displayCurrency,
  labels,
  workflowStepLabel,
  traceStepStatusMeta,
  actionMeta,
  actionExecutionStatusLabel,
  formatQuantity,
}: TraceboardCardProps) {
  return (
    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <div className="flex items-center justify-between gap-4">
        <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{labels.traceboard}</h3>
        {isInitialLoading ? <span className="text-xs text-gray-500">{labels.loadingTraceboard}</span> : null}
      </div>
      <p className="mt-2 text-sm text-gray-500">{labels.traceboardSubtitle}</p>

      <div className="mt-5 grid gap-4 xl:grid-cols-2">
        {isInitialLoading ? (
          <div className="xl:col-span-2 rounded-xl border border-blue-100 bg-blue-50/70 px-4 py-3 text-sm text-blue-800">
            <p className="font-medium">{labels.loadingTraceboard}</p>
            <p className="mt-1 text-blue-700">{labels.loadingTraceboardHint}</p>
          </div>
        ) : null}
        <div className="rounded-xl border border-gray-200 bg-gray-50 p-4">
          <div className="flex items-center justify-between gap-3">
            <h4 className="text-sm font-semibold text-gray-900">{labels.workflowTimeline}</h4>
            {decisionTrace?.run?.state ? (
              <span className={`rounded-full border px-3 py-1 text-xs font-medium ${traceStepStatusMeta(decisionTrace.run.state).badge}`}>
                {labels.workflowState}: {traceStepStatusMeta(decisionTrace.run.state).label}
              </span>
            ) : null}
          </div>
          {decisionTrace?.run ? (
            <>
              <div className="mt-3 flex flex-wrap gap-3 text-xs text-gray-500">
                {decisionTrace.run.runId ? <span>Run ID: {decisionTrace.run.runId.slice(0, 8)}</span> : null}
                {decisionTrace.run.step ? <span>{labels.workflowCurrentStep}: {workflowStepLabel(decisionTrace.run.step)}</span> : null}
                {decisionTrace.run.startedAt ? <span>{labels.workflowStartedAt}: {formatDateTimeForLanguage(decisionTrace.run.startedAt, language)}</span> : null}
                {decisionTrace.run.completedAt ? <span>{labels.workflowCompletedAt}: {formatDateTimeForLanguage(decisionTrace.run.completedAt, language)}</span> : null}
              </div>
              <div className="mt-4 space-y-3">
                {workflowSteps.map((step: DecisionTraceStep, index) => {
                  const meta = traceStepStatusMeta(step.status);
                  const isLast = index === workflowSteps.length - 1;
                  return (
                    <div key={`${step.step ?? index}`} className="flex gap-3">
                      <div className="flex flex-col items-center">
                        <span className={`mt-1 h-3 w-3 rounded-full border ${meta.badge}`} />
                        {!isLast ? <span className="mt-1 h-full min-h-8 w-px bg-gray-200" /> : null}
                      </div>
                      <div className="min-w-0 flex-1 rounded-lg bg-white px-4 py-3">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <p className="font-medium text-gray-900">{workflowStepLabel(step.step)}</p>
                          <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                        </div>
                        <div className="mt-2 flex flex-wrap gap-3 text-xs text-gray-500">
                          {step.startedAt ? <span>{labels.workflowStartedAt}: {formatDateTimeForLanguage(step.startedAt, language)}</span> : null}
                          {step.endedAt ? <span>{labels.workflowCompletedAt}: {formatDateTimeForLanguage(step.endedAt, language)}</span> : null}
                          {step.updatedAt ? <span>{labels.workflowUpdatedAt}: {formatDateTimeForLanguage(step.updatedAt, language)}</span> : null}
                        </div>
                        {step.error ? <p className="mt-2 text-xs text-red-600">{labels.workflowError}: {step.error}</p> : null}
                      </div>
                    </div>
                  );
                })}
                {workflowSteps.length === 0 ? <div className="rounded-lg border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{labels.workflowNoData}</div> : null}
              </div>
            </>
          ) : (
            <div className="mt-4 rounded-lg border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{labels.workflowNoData}</div>
          )}
        </div>

        <div className="space-y-4">
          <div className="rounded-xl border border-gray-200 bg-gray-50 p-4">
            <h4 className="text-sm font-semibold text-gray-900">{labels.discussionTrace}</h4>
            <div className="mt-3 space-y-3">
              <div className="rounded-lg bg-white p-4 text-sm text-gray-700">
                <p className="font-medium text-gray-900">{labels.discussionSummary}</p>
                <p className="mt-2 whitespace-pre-line leading-7">{discussionSummary || labels.discussionFallback}</p>
              </div>
              {discussionConsensus.length ? (
                <div className="rounded-lg bg-white p-4">
                  <p className="text-sm font-medium text-gray-900">{labels.discussionConsensus}</p>
                  <ul className="mt-2 list-disc space-y-2 pl-5 text-sm leading-7 text-gray-700">
                    {discussionConsensus.map((item) => (
                      <li key={item}>{item}</li>
                    ))}
                  </ul>
                </div>
              ) : null}
              {discussionReasoning && discussionReasoning !== discussionSummary ? (
                <div className="rounded-lg bg-white p-4 text-sm text-gray-700">
                  <p className="font-medium text-gray-900">{labels.discussionReasoning}</p>
                  <p className="mt-2 whitespace-pre-line leading-7">{discussionReasoning}</p>
                </div>
              ) : null}
              {discussionSnapshot && !discussionSummary && !discussionConsensus.length && !discussionReasoning ? (
                <div className="rounded-lg bg-white p-4 text-sm text-gray-700">
                  <p className="font-medium text-gray-900">{labels.discussionSnapshot}</p>
                  <pre className="mt-2 overflow-x-auto whitespace-pre-wrap break-words rounded-lg bg-gray-50 p-3 text-xs leading-6 text-gray-600">{discussionSnapshot}</pre>
                </div>
              ) : null}
            </div>
          </div>

          <div className="rounded-xl border border-gray-200 bg-gray-50 p-4">
            <div className="flex items-center justify-between gap-3">
              <h4 className="text-sm font-semibold text-gray-900">{labels.executionTrace}</h4>
              {decisionTrace?.execution?.status ? (
                <span className={`rounded-full border px-3 py-1 text-xs font-medium ${traceStepStatusMeta(decisionTrace.execution.status).badge}`}>
                  {labels.executionStatusLabel}: {traceStepStatusMeta(decisionTrace.execution.status).label}
                </span>
              ) : null}
            </div>
            {executionTraceRows.length > 0 ? (
              <div className="mt-4 space-y-3">
                {executionTraceRows.map((item) => {
                  const actionMetaValue = actionMeta(item.action);
                  const statusMeta = traceStepStatusMeta(item.executionStatus);
                  return (
                    <div key={item.actionId} className="rounded-lg bg-white p-4">
                      <div className="flex flex-wrap items-center justify-between gap-3">
                        <div className="flex items-center gap-2">
                          <span className="font-semibold text-gray-900">{item.symbol}</span>
                          <span className={`text-sm font-medium ${actionMetaValue.color}`}>{actionMetaValue.label}</span>
                        </div>
                        <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${statusMeta.badge}`}>{statusMeta.label}</span>
                      </div>
                      {item.trades.length > 0 ? (
                        <div className="mt-3 space-y-2">
                          {item.trades.map((trade) => (
                            <div key={trade.id} className="rounded-lg border border-gray-200 bg-gray-50 px-3 py-3 text-sm text-gray-700">
                              <div className="flex flex-wrap gap-x-4 gap-y-2">
                                <span><span className="text-gray-500">{labels.tradeId}:</span> {trade.id}</span>
                                <span><span className="text-gray-500">{labels.tradeSide}:</span> {humanizeValue(trade.side, labels.notRecorded)}</span>
                                <span><span className="text-gray-500">{labels.tradeStatus}:</span> {actionExecutionStatusLabel(trade.status)}</span>
                                <span><span className="text-gray-500">{labels.filledQuantity}:</span> {formatQuantity(trade.filledQty)}</span>
                                <span><span className="text-gray-500">{labels.filledPrice}:</span> {typeof trade.filledPrice === "number" ? formatMoneyForDisplay(trade.filledPrice, trade.quoteCurrency || "USD", displayCurrency, language) : "—"}</span>
                                <span><span className="text-gray-500">{labels.orderType}:</span> {humanizeValue(trade.orderType, labels.notRecorded)}</span>
                                {trade.executedAt ? <span><span className="text-gray-500">{labels.executedAt}:</span> {formatDateTimeForLanguage(trade.executedAt, language)}</span> : null}
                              </div>
                            </div>
                          ))}
                        </div>
                      ) : (
                        <p className="mt-3 text-sm text-gray-500">{labels.traceNoTrades}</p>
                      )}
                    </div>
                  );
                })}
              </div>
            ) : (
              <div className="mt-4 rounded-lg border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{labels.traceNoExecution}</div>
            )}
          </div>
        </div>
      </div>

      <div className="mt-4 rounded-xl border border-gray-200 bg-gray-50 p-4">
        <h4 className="text-sm font-semibold text-gray-900">{labels.reviewMemory}</h4>
        {reviewEntries.length > 0 ? (
          <div className="mt-4 grid gap-3 lg:grid-cols-2">
            {reviewEntries.map((entry) => {
              // Server migration 085: structured-i18n render path.
              // Falls back to legacy title/content when templateKey is
              // missing or unknown (LocaleId fallback handled inside
              // renderLesson — we never see "{field}"-shaped output).
              const rendered = renderLesson(language, entry.templateKey, entry.payload);
              const renderedTitle = rendered?.title.trim();
              const renderedBody = rendered?.body.trim();
              return (
              <div key={entry.id} className="rounded-lg bg-white p-4">
                <div className="flex flex-wrap items-center gap-2">
                  <p className="font-medium text-gray-900">{renderedTitle || entry.title?.trim() || humanizeValue(entry.layer, labels.reviewMemory)}</p>
                  <MetaBadge>{labels.reviewLayer}: {humanizeValue(entry.layer, labels.notRecorded)}</MetaBadge>
                  {entry.agentId ? <MetaBadge>{entry.agentId}</MetaBadge> : null}
                </div>
                <p className="mt-3 whitespace-pre-line text-sm leading-7 text-gray-700">{renderedBody || entry.content}</p>
                <div className="mt-3 flex flex-wrap gap-2">
                  {(entry.tags ?? []).map((tag) => (
                    <span key={`${entry.id}-${tag}`} className="rounded-full bg-indigo-50 px-2 py-1 text-xs text-indigo-700">{tag}</span>
                  ))}
                </div>
                <p className="mt-3 text-xs text-gray-500">{labels.reviewUpdatedAt}: {formatDateTimeForLanguage(entry.updatedAt, language)}</p>
              </div>
              );
            })}
          </div>
        ) : (
          <div className="mt-4 rounded-lg border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{labels.reviewNoEntries}</div>
        )}
      </div>
    </div>
  );
}

export const TraceboardCard = React.memo(TraceboardCardInner);
