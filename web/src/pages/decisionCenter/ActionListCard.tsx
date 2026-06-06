import React from "react";
import type { MarketResearch, PortfolioQuote } from "../../lib/api";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  type AppLanguage,
  type DisplayCurrency,
} from "../../lib/preferences";
import { MetaBadge } from "./MetaBadge";
import { humanizeValue, metaItems, pickLocalizedText } from "./helpers";
import type { ApiPlanAction } from "./types";

export interface ActionListCardLabels {
  actionList: string;
  totalRows: string;
  noActions: string;
  columns: {
    instrument: string;
    profile: string;
    action: string;
    quantity: string;
    price: string;
    livePrice: string;
    amount: string;
    stopLoss: string;
    takeProfit: string;
    confidence: string;
    supporters: string;
  };
  livePriceDriftWarning: (percent: string) => string;
  livePriceUnavailable: string;
  none: string;
  actionReasonMissing: string;
  executionStatus: string;
  contractMultiplier: string;
  expiryDate: string;
  opposedBy: string;
  /**
   * W13-4 — joiner for inline lists (e.g. opposedBy =
   * ["Risk","Compliance"] → "Risk, Compliance" in en, "Risk、Compliance"
   * in zh). See the en-US/zh-CN decisionCenter bundle.
   */
  listSeparator: string;
  reduceOnly: string;
  marketResearch: string;
  researchSummary: string;
  researchSignals: string;
  researchNews: string;
  researchQuote: string;
  researchUnavailable: string;
  quoteStaleBadge: string;
  quoteStaleHint: string;
  newsLanguageZh: string;
  newsLanguageEn: string;
  researchNotes: string;
}

interface ActionListCardProps {
  actions: ApiPlanAction[] | undefined;
  researchBySymbol: Record<string, MarketResearch>;
  liveQuotesBySymbol?: Record<string, PortfolioQuote>;
  language: AppLanguage;
  displayCurrency: DisplayCurrency;
  labels: ActionListCardLabels;
  actionMeta: (action: string) => { label: string; color: string };
  positionSideLabel: (value?: string) => string;
  openCloseLabel: (value?: string) => string;
  actionExecutionStatusLabel: (value?: string) => string;
  formatPercent: (value?: number, digits?: number) => string;
  formatQuantity: (value?: number) => string;
}

// LIVE_PRICE_DRIFT_THRESHOLD is the relative gap above which we highlight
// the action's planning price as "stale relative to current market". Set
// to 5% per the plan; an action whose entry price drifts more than this
// is worth a human glance before approval.
const LIVE_PRICE_DRIFT_THRESHOLD = 0.05;

function ActionListCardInner({
  actions,
  researchBySymbol,
  liveQuotesBySymbol,
  language,
  displayCurrency,
  labels,
  actionMeta,
  positionSideLabel,
  openCloseLabel,
  actionExecutionStatusLabel,
  formatPercent,
  formatQuantity,
}: ActionListCardProps) {
  const hasActions = actions && actions.length > 0;
  return (
    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <div className="flex items-center justify-between gap-4">
        <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{labels.actionList}</h3>
        <span className="text-xs text-gray-500">
          {formatNumberForLanguage(actions?.length ?? 0, language)} {labels.totalRows}
        </span>
      </div>
      {hasActions ? (
        <div className="mt-4 overflow-x-auto">
          <table className="min-w-full text-sm">
            <thead>
              <tr className="border-b border-gray-200 text-left text-xs text-gray-500">
                <th className="px-3 py-2 font-medium">{labels.columns.instrument}</th>
                <th className="px-3 py-2 font-medium">{labels.columns.profile}</th>
                <th className="px-3 py-2 font-medium">{labels.columns.action}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.quantity}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.price}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.livePrice}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.amount}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.stopLoss}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.takeProfit}</th>
                <th className="px-3 py-2 text-right font-medium">{labels.columns.confidence}</th>
                <th className="px-3 py-2 font-medium">{labels.columns.supporters}</th>
              </tr>
            </thead>
            <tbody>
              {actions!.map((action, index) => {
                const meta = actionMeta(action.action);
                const priceCurrency = action.quoteCurrency || "USD";
                const amountCurrency = action.settlementCurrency || action.quoteCurrency || "USD";
                return (
                  <tr key={action.id ?? `${action.symbol}-${index}`} className="border-b border-gray-100 align-top last:border-b-0">
                    <td className="px-3 py-3">
                      <p className="font-semibold text-gray-900">{action.symbol}</p>
                      <p className="mt-1 text-xs text-gray-500">{action.instrumentKey || action.symbol}</p>
                    </td>
                    <td className="px-3 py-3">
                      <div className="flex flex-wrap gap-1.5">
                        {metaItems(action.market, action.exchange, action.assetClass).map((item) => (
                          <MetaBadge key={item}>{humanizeValue(item)}</MetaBadge>
                        ))}
                        {action.positionSide ? <MetaBadge>{positionSideLabel(action.positionSide)}</MetaBadge> : null}
                        {action.openClose ? <MetaBadge>{openCloseLabel(action.openClose)}</MetaBadge> : null}
                        {typeof action.leverage === "number" ? <MetaBadge>{formatNumberForLanguage(action.leverage, language)}x</MetaBadge> : null}
                        {action.expiryDate ? <MetaBadge>{formatDateForLanguage(action.expiryDate, language)}</MetaBadge> : null}
                      </div>
                    </td>
                    <td className={`px-3 py-3 font-medium ${meta.color}`}>{meta.label}</td>
                    <td className="px-3 py-3 text-right text-gray-700">{formatQuantity(action.quantity)}</td>
                    <td className="px-3 py-3 text-right text-gray-700">{formatMoneyForDisplay(action.price ?? 0, priceCurrency, displayCurrency, language)}</td>
                    <td className="px-3 py-3 text-right text-gray-700">{renderLivePriceCell(action, liveQuotesBySymbol, priceCurrency, displayCurrency, language, labels)}</td>
                    <td className="px-3 py-3 text-right text-gray-700">{formatMoneyForDisplay(action.amount ?? 0, amountCurrency, displayCurrency, language)}</td>
                    <td className="px-3 py-3 text-right text-red-600">{typeof action.stopLoss === "number" ? formatMoneyForDisplay(action.stopLoss, priceCurrency, displayCurrency, language) : "—"}</td>
                    <td className="px-3 py-3 text-right text-emerald-600">{typeof action.takeProfit === "number" ? formatMoneyForDisplay(action.takeProfit, priceCurrency, displayCurrency, language) : "—"}</td>
                    <td className="px-3 py-3 text-right text-gray-700">{formatPercent(action.confidence, 0)}</td>
                    <td className="px-3 py-3">
                      <div className="flex flex-wrap gap-1.5">
                        {(action.supportedBy ?? []).length > 0 ? (
                          action.supportedBy?.map((item) => (
                            <span key={item} className="rounded-full bg-indigo-50 px-2 py-1 text-xs text-indigo-700">
                              {item}
                            </span>
                          ))
                        ) : (
                          <span className="text-xs text-gray-400">{labels.none}</span>
                        )}
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>

          <div className="mt-4 space-y-3">
            {actions!.map((action, index) => {
              const research = researchBySymbol[action.symbol.trim()];
              const quoteCurrency = research?.quote?.quoteCurrency || action.quoteCurrency || "USD";
              return (
                <div key={`${action.id ?? index}-reason`} className="rounded-xl bg-gray-50 px-4 py-3 text-sm text-gray-700">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="font-semibold text-gray-900">{action.symbol}</span>
                    {metaItems(action.market, action.exchange, action.assetClass).map((item) => (
                      <MetaBadge key={`${action.id ?? index}-${item}`}>{humanizeValue(item)}</MetaBadge>
                    ))}
                    {action.positionSide ? <MetaBadge>{positionSideLabel(action.positionSide)}</MetaBadge> : null}
                    {action.openClose ? <MetaBadge>{openCloseLabel(action.openClose)}</MetaBadge> : null}
                    {action.marginMode ? <MetaBadge>{humanizeValue(action.marginMode)}</MetaBadge> : null}
                    {action.reduceOnly ? <MetaBadge>{labels.reduceOnly}</MetaBadge> : null}
                  </div>
                  <p className="mt-2 whitespace-pre-line">{pickLocalizedText(language, action.reasoning, action.reasoningZh, action.reasoningEn) || labels.actionReasonMissing}</p>
                  <div className="mt-2 flex flex-wrap gap-3 text-xs text-gray-500">
                    {action.executionStatus ? <span>{labels.executionStatus}: {actionExecutionStatusLabel(action.executionStatus)}</span> : null}
                    {action.contractMultiplier ? <span>{labels.contractMultiplier}: {formatNumberForLanguage(action.contractMultiplier, language)}</span> : null}
                    {action.expiryDate ? <span>{labels.expiryDate}: {formatDateForLanguage(action.expiryDate, language)}</span> : null}
                  </div>
                  {(action.opposedBy ?? []).length ? (
                    <p className="mt-2 text-xs text-red-600">{labels.opposedBy}: {action.opposedBy?.join(labels.listSeparator)}</p>
                  ) : null}
                  <div className="mt-4 rounded-xl border border-gray-200 bg-white p-4">
                    <h4 className="text-xs font-semibold uppercase tracking-wider text-gray-500">{labels.marketResearch}</h4>
                    {research ? (
                      <div className="mt-3 space-y-3">
                        {research.summary ? <p className="text-sm text-gray-700"><span className="font-medium text-gray-900">{labels.researchSummary}:</span> {research.summary}</p> : null}
                        {research.quote ? (
                          <p className="text-sm text-gray-700">
                            <span className="font-medium text-gray-900">{labels.researchQuote}:</span>{" "}
                            {formatMoneyForDisplay(research.quote.price, quoteCurrency, displayCurrency, language)} · {research.quote.source} · {formatDateTimeForLanguage(research.quote.asOf, language)}
                            {research.quote.isStale ? (
                              <span
                                className="ml-2 inline-flex items-center rounded-full bg-amber-100 px-2 py-0.5 text-xs font-medium text-amber-700"
                                title={labels.quoteStaleHint}
                              >
                                {labels.quoteStaleBadge}
                              </span>
                            ) : null}
                          </p>
                        ) : null}
                        {research.signals?.length ? (
                          <div>
                            <p className="text-sm font-medium text-gray-900">{labels.researchSignals}</p>
                            <ul className="mt-1 list-disc space-y-1 pl-5 text-sm text-gray-700">
                              {research.signals.slice(0, 4).map((signal) => (
                                <li key={signal}>{signal}</li>
                              ))}
                            </ul>
                          </div>
                        ) : null}
                        {research.news?.length ? (
                          <div>
                            <p className="text-sm font-medium text-gray-900">{labels.researchNews}</p>
                            <ul className="mt-1 space-y-2 text-sm text-gray-700">
                              {research.news.slice(0, 3).map((item) => {
                                const localizedTitle = pickLocalizedText(language, item.title, item.titleZh, item.titleEn);
                                const localizedSummary = pickLocalizedText(language, item.summary, item.summaryZh, item.summaryEn);
                                const languageTag =
                                  item.language === "zh"
                                    ? labels.newsLanguageZh
                                    : item.language === "en"
                                      ? labels.newsLanguageEn
                                      : "";
                                return (
                                  <li key={`${action.symbol}-${item.url || localizedTitle || item.title}`} className="flex flex-col gap-1">
                                    <div className="flex items-baseline gap-2">
                                      {languageTag ? (
                                        <span className="inline-flex items-center rounded bg-gray-100 px-1.5 py-0.5 text-[10px] font-semibold uppercase tracking-wider text-gray-500">
                                          {languageTag}
                                        </span>
                                      ) : null}
                                      {item.url ? (
                                        <a
                                          href={item.url}
                                          target="_blank"
                                          rel="noopener noreferrer"
                                          className="text-indigo-600 hover:underline"
                                        >
                                          {localizedTitle || item.title}
                                        </a>
                                      ) : (
                                        <span>{localizedTitle || item.title}</span>
                                      )}
                                    </div>
                                    {localizedSummary && localizedSummary !== (localizedTitle || item.title) ? (
                                      <p className="pl-1 text-xs text-gray-500 line-clamp-2">{localizedSummary}</p>
                                    ) : null}
                                  </li>
                                );
                              })}
                            </ul>
                          </div>
                        ) : null}
                        {research.providerNotes?.length ? (
                          <div className="rounded-lg border border-amber-200 bg-amber-50 px-3 py-2">
                            <p className="text-xs font-medium text-amber-900">{labels.researchNotes}</p>
                            <ul className="mt-1 list-disc space-y-0.5 pl-4 text-xs text-amber-800">
                              {research.providerNotes.slice(0, 4).map((note, noteIndex) => (
                                <li key={`${action.symbol}-note-${noteIndex}`}>{note}</li>
                              ))}
                            </ul>
                          </div>
                        ) : null}
                      </div>
                    ) : (
                      <p className="mt-3 text-sm text-gray-500">{labels.researchUnavailable}</p>
                    )}
                  </div>
                </div>
              );
            })}
          </div>
        </div>
      ) : (
        <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-6 text-sm text-gray-500">{labels.noActions}</div>
      )}
    </div>
  );
}

export const ActionListCard = React.memo(ActionListCardInner);

// renderLivePriceCell renders the "现价 / Live" cell for a single plan
// action. It looks up the matching live quote, formats it in the same
// currency as the plan's planning price, and overlays a "drift X%"
// warning when the gap exceeds LIVE_PRICE_DRIFT_THRESHOLD so the
// approver knows the action's stated cost may not match reality at
// execution time.
function renderLivePriceCell(
  action: ApiPlanAction,
  liveQuotesBySymbol: Record<string, PortfolioQuote> | undefined,
  priceCurrency: string,
  displayCurrency: DisplayCurrency,
  language: AppLanguage,
  labels: ActionListCardLabels,
): JSX.Element {
  const lookupKey = action.symbol?.trim().toUpperCase() ?? "";
  const quote = liveQuotesBySymbol?.[lookupKey];
  if (!quote || !Number.isFinite(quote.currentPrice) || quote.currentPrice <= 0) {
    return <span className="text-xs text-gray-400">{labels.livePriceUnavailable}</span>;
  }
  const plannedPrice = action.price ?? 0;
  const live = quote.currentPrice;
  const drift = plannedPrice > 0 ? (live - plannedPrice) / plannedPrice : 0;
  const driftPct = drift * 100;
  const driftSignificant = Math.abs(drift) >= LIVE_PRICE_DRIFT_THRESHOLD;
  return (
    <div className="flex flex-col items-end gap-0.5">
      <span className={driftSignificant ? "font-medium text-amber-700" : "text-gray-700"}>
        {formatMoneyForDisplay(live, priceCurrency, displayCurrency, language)}
      </span>
      {plannedPrice > 0 ? (
        <span className={`text-xs ${driftSignificant ? "text-amber-700" : driftPct >= 0 ? "text-emerald-600" : "text-red-600"}`}>
          {driftPct >= 0 ? "+" : ""}{driftPct.toFixed(1)}%
          {quote.isStale ? " · " + labels.livePriceUnavailable : ""}
        </span>
      ) : null}
      {driftSignificant ? (
        <span className="text-[10px] text-amber-700" title={labels.livePriceDriftWarning(driftPct.toFixed(1))}>
          {labels.livePriceDriftWarning(driftPct.toFixed(1))}
        </span>
      ) : null}
    </div>
  );
}
