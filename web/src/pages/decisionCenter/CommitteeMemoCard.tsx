import React from "react";
import type { AppLanguage } from "../../lib/preferences";
import type { CommitteeMemo } from "../../lib/api";
import { MetaBadge } from "./MetaBadge";
import { humanizeValue } from "./helpers";

export interface CommitteeMemoCardLabels {
  committeeMemo: string;
  committeeMemoSubtitle: string;
  meetingSummary: string;
  marketBackground: string;
  participants: string;
  agentViews: string;
  keyContentions: string;
  finalDecision: string;
  riskOpinion: string;
  traderSuggestions: string;
  noCommitteeMemo: string;
  noContentions: string;
  discussionFallback: string;
  researchUnavailable: string;
  traceNoExecution: string;
  stance: string;
  portfolioManager: string;
  opposedBy: string;
  unknown: string;
}

interface CommitteeMemoCardProps {
  memo: CommitteeMemo | undefined;
  discussionSummary: string;
  language: AppLanguage;
  labels: CommitteeMemoCardLabels;
  planStatusMeta: (status: string, riskReview?: unknown) => { label: string; badge: string };
  riskVerdictMeta: (value: string) => { label: string; badge: string };
  actionMeta: (action: string) => { label: string; color: string };
}

function CommitteeMemoCardInner({
  memo,
  discussionSummary,
  language,
  labels,
  planStatusMeta,
  riskVerdictMeta,
  actionMeta,
}: CommitteeMemoCardProps) {
  return (
    <div className="rounded-2xl border border-indigo-100 bg-gradient-to-br from-white to-indigo-50/40 p-6 shadow-sm">
      <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h3 className="text-sm font-semibold uppercase tracking-wider text-indigo-700">{labels.committeeMemo}</h3>
          <p className="mt-2 text-sm text-gray-600">{labels.committeeMemoSubtitle}</p>
        </div>
        {memo?.traceLinks?.length ? (
          <div className="flex flex-wrap gap-2">
            {memo.traceLinks.map((link, index) => (
              <span key={`${link.target ?? link.label ?? "trace"}-${index}`} className="rounded-full bg-white px-3 py-1 text-xs font-medium text-indigo-700 shadow-sm">
                {link.label || link.target}
              </span>
            ))}
          </div>
        ) : null}
      </div>

      {memo ? (
        <div className="mt-5 space-y-4">
          <div className="grid gap-4 lg:grid-cols-2">
            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <p className="text-sm font-semibold text-gray-900">{labels.meetingSummary}</p>
              <p className="mt-2 whitespace-pre-line text-sm leading-7 text-gray-700">{memo.summary || discussionSummary || labels.discussionFallback}</p>
            </div>
            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <p className="text-sm font-semibold text-gray-900">{labels.marketBackground}</p>
              <p className="mt-2 whitespace-pre-line text-sm leading-7 text-gray-700">{memo.marketBackground || labels.researchUnavailable}</p>
            </div>
          </div>

          {memo.participants?.length ? (
            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <p className="text-sm font-semibold text-gray-900">{labels.participants}</p>
              <div className="mt-3 flex flex-wrap gap-2">
                {memo.participants.map((participant, index) => (
                  <span key={`${participant.agentId ?? participant.role ?? "participant"}-${index}`} className="rounded-full bg-gray-100 px-3 py-1 text-xs text-gray-700">
                    {participant.name || participant.agentId || labels.unknown} · {humanizeValue(participant.role, labels.unknown)}
                  </span>
                ))}
              </div>
            </div>
          ) : null}

          <div className="grid gap-4 xl:grid-cols-[minmax(0,1.5fr)_minmax(280px,1fr)]">
            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <p className="text-sm font-semibold text-gray-900">{labels.agentViews}</p>
              {memo.agentViews?.length ? (
                <div className="mt-3 space-y-3">
                  {memo.agentViews.map((view, index) => (
                    <div key={`${view.agentId ?? "agent"}-${view.stance ?? index}`} className="rounded-lg border border-gray-100 bg-gray-50 p-3">
                      <div className="flex flex-wrap items-center gap-2">
                        <span className="font-medium text-gray-900">{view.agentId || humanizeValue(view.role, labels.unknown)}</span>
                        {view.role ? <MetaBadge>{humanizeValue(view.role)}</MetaBadge> : null}
                        {view.stance ? <MetaBadge>{labels.stance}: {humanizeValue(view.stance)}</MetaBadge> : null}
                        {view.symbols?.slice(0, 4).map((symbol) => <MetaBadge key={`${view.agentId}-${symbol}`}>{symbol}</MetaBadge>)}
                      </div>
                      {view.viewpoint ? <p className="mt-2 text-sm leading-6 text-gray-700">{view.viewpoint}</p> : null}
                      {view.evidence?.length ? (
                        <ul className="mt-2 list-disc space-y-1 pl-5 text-xs leading-5 text-gray-600">
                          {view.evidence.slice(0, 3).map((item) => <li key={item}>{item}</li>)}
                        </ul>
                      ) : null}
                    </div>
                  ))}
                </div>
              ) : (
                <p className="mt-3 text-sm text-gray-500">{labels.discussionFallback}</p>
              )}
            </div>

            <div className="space-y-4">
              <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
                <p className="text-sm font-semibold text-gray-900">{labels.keyContentions}</p>
                {memo.contentions?.length ? (
                  <ul className="mt-2 list-disc space-y-2 pl-5 text-sm leading-6 text-red-700">
                    {memo.contentions.map((item) => <li key={item}>{item}</li>)}
                  </ul>
                ) : (
                  <p className="mt-2 text-sm text-gray-500">{labels.noContentions}</p>
                )}
              </div>

              <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
                <div className="flex flex-wrap items-center justify-between gap-2">
                  <p className="text-sm font-semibold text-gray-900">{labels.finalDecision}</p>
                  {memo.finalDecision?.status ? (
                    <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${planStatusMeta(memo.finalDecision.status).badge}`}>
                      {planStatusMeta(memo.finalDecision.status).label}
                    </span>
                  ) : null}
                </div>
                {memo.finalDecision?.pm ? <p className="mt-2 text-xs text-gray-500">{labels.portfolioManager}: {memo.finalDecision.pm}</p> : null}
                {memo.finalDecision?.reasoning ? <p className="mt-2 whitespace-pre-line text-sm leading-6 text-gray-700">{memo.finalDecision.reasoning}</p> : null}
                {memo.finalDecision?.actions?.length ? (
                  <div className="mt-3 flex flex-wrap gap-2">
                    {memo.finalDecision.actions.map((item) => <MetaBadge key={item}>{item}</MetaBadge>)}
                  </div>
                ) : null}
              </div>
            </div>
          </div>

          <div className="grid gap-4 lg:grid-cols-2">
            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <div className="flex flex-wrap items-center justify-between gap-2">
                <p className="text-sm font-semibold text-gray-900">{labels.riskOpinion}</p>
                {memo.riskOpinion?.verdict ? (
                  <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${riskVerdictMeta(memo.riskOpinion.verdict).badge}`}>
                    {riskVerdictMeta(memo.riskOpinion.verdict).label}
                  </span>
                ) : null}
              </div>
              {memo.riskOpinion?.summary ? <p className="mt-2 whitespace-pre-line text-sm leading-6 text-gray-700">{memo.riskOpinion.summary}</p> : null}
              {[...(memo.riskOpinion?.rejections ?? []), ...(memo.riskOpinion?.warnings ?? []), ...(memo.riskOpinion?.suggestions ?? [])].length ? (
                <ul className="mt-2 list-disc space-y-1 pl-5 text-xs leading-5 text-gray-600">
                  {[...(memo.riskOpinion?.rejections ?? []), ...(memo.riskOpinion?.warnings ?? []), ...(memo.riskOpinion?.suggestions ?? [])].slice(0, 6).map((item) => <li key={item}>{item}</li>)}
                </ul>
              ) : null}
            </div>

            <div className="rounded-xl border border-white/80 bg-white p-4 shadow-sm">
              <p className="text-sm font-semibold text-gray-900">{labels.traderSuggestions}</p>
              {memo.traderSuggestions?.length ? (
                <div className="mt-3 space-y-2">
                  {memo.traderSuggestions.map((item, index) => {
                    const meta = actionMeta(item.action ?? "");
                    return (
                      <div key={`${item.planActionId ?? item.symbol ?? "trade"}-${index}`} className="rounded-lg bg-gray-50 px-3 py-2 text-sm">
                        <div className="flex flex-wrap items-center gap-2">
                          <span className="font-medium text-gray-900">{item.symbol || labels.unknown}</span>
                          {item.action ? <span className={`font-medium ${meta.color}`}>{meta.label}</span> : null}
                        </div>
                        <p className="mt-1 text-gray-700">{item.instruction || labels.traceNoExecution}</p>
                        {item.opposedBy?.length ? <p className="mt-1 text-xs text-red-600">{labels.opposedBy}: {item.opposedBy.join(language === "en-US" ? ", " : "、")}</p> : null}
                      </div>
                    );
                  })}
                </div>
              ) : (
                <p className="mt-3 text-sm text-gray-500">{labels.traceNoExecution}</p>
              )}
            </div>
          </div>
        </div>
      ) : (
        <div className="mt-5 rounded-xl border border-dashed border-indigo-200 bg-white/70 p-5 text-sm text-gray-500">{labels.noCommitteeMemo}</div>
      )}
    </div>
  );
}

export const CommitteeMemoCard = React.memo(CommitteeMemoCardInner);
