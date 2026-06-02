import React, { useEffect, useMemo, useState } from "react";

import {
  fetchFundCorpActions,
  formatApiError,
  type CorpActionApplication,
} from "../lib/api";

interface CorpActionTimelineProps {
  fundId: string;
  language?: "zh-CN" | "en-US";
  /** Maximum rows to fetch. Server caps at 200; default 50. */
  limit?: number;
  /** Initial expand state. Defaults to collapsed because most fund
   *  views won't have a corp action in the recent past and we don't
   *  want to push the more important sections (NAV / positions) below
   *  the fold for nothing. */
  defaultOpen?: boolean;
}

const COPY = {
  "zh-CN": {
    title: "分红 · 拆股 · 配股记录",
    subtitle: "最近发生在持仓上的公司行动事件，影响成本价、份额和可用现金",
    expand: "展开",
    collapse: "收起",
    loading: "加载中…",
    empty: "近期无公司行动事件",
    error: "加载失败",
    retry: "重试",
    columns: {
      exDate: "除权日",
      instrument: "标的",
      type: "类型",
      shares: "份额变化",
      cost: "成本变化",
      cash: "现金到账",
    },
    types: {
      split: "拆股 / 送股",
      cash_dividend: "现金分红",
      stock_dividend: "送股转增",
      combined: "派股 + 派现",
    },
    units: {
      shares: "股",
      yuan: "元",
    },
  },
  "en-US": {
    title: "Dividends · Splits · Rights Issues",
    subtitle: "Recent corporate actions applied to this fund's holdings (cost basis, shares, cash)",
    expand: "Show",
    collapse: "Hide",
    loading: "Loading…",
    empty: "No recent corporate actions",
    error: "Failed to load",
    retry: "Retry",
    columns: {
      exDate: "Ex-date",
      instrument: "Instrument",
      type: "Type",
      shares: "Δ Shares",
      cost: "Δ Cost",
      cash: "Cash credit",
    },
    types: {
      split: "Split / Stock div.",
      cash_dividend: "Cash dividend",
      stock_dividend: "Stock dividend",
      combined: "Stock + cash",
    },
    units: {
      shares: " sh",
      yuan: "",
    },
  },
};

const TYPE_BADGE_CLASS: Record<CorpActionApplication["actionType"], string> = {
  split: "bg-blue-50 text-blue-700 ring-1 ring-blue-100",
  cash_dividend: "bg-emerald-50 text-emerald-700 ring-1 ring-emerald-100",
  stock_dividend: "bg-violet-50 text-violet-700 ring-1 ring-violet-100",
  combined: "bg-amber-50 text-amber-700 ring-1 ring-amber-100",
};

function formatNum(n: number, fractionDigits = 2): string {
  if (!Number.isFinite(n)) return "—";
  return n.toLocaleString(undefined, {
    minimumFractionDigits: fractionDigits,
    maximumFractionDigits: fractionDigits,
  });
}

function formatDate(iso: string): string {
  // Strip the time portion and TZ — corp action ex_date is a calendar
  // date, not a moment in time. Showing "2026-05-29 00:00:00 UTC"
  // confuses operators in CST.
  return iso.slice(0, 10);
}

/**
 * CorpActionTimeline renders a collapsible card that lists the most
 * recent corp-action receipts the applier has persisted for the
 * fund. Data flows from `GET /api/funds/:fundId/corp-actions`.
 *
 * Design choices worth knowing if you maintain this:
 *   - Defaults to collapsed. Most days have no event and we don't
 *     want to push the NAV chart below the fold.
 *   - Lazy fetch: we don't hit the API until the user expands.
 *   - Single-column ROW for shares / cost / cash because the absolute
 *     numbers matter less than the DELTA the operator wants to audit.
 */
export const CorpActionTimeline: React.FC<CorpActionTimelineProps> = ({
  fundId,
  language = "zh-CN",
  limit = 50,
  defaultOpen = false,
}) => {
  const copy = useMemo(() => COPY[language], [language]);
  const [open, setOpen] = useState(defaultOpen);
  const [items, setItems] = useState<CorpActionApplication[] | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;
    if (!open || items !== null || loading) return;
    setLoading(true);
    setError(null);
    fetchFundCorpActions(fundId, limit)
      .then((resp) => {
        if (!cancelled) setItems(resp.items);
      })
      .catch((err) => {
        if (!cancelled) setError(formatApiError(err, copy.error));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [open, fundId, limit, items, loading, copy.error]);

  const retry = () => {
    setItems(null);
    setError(null);
  };

  return (
    <section className="rounded-2xl border border-gray-100 bg-white shadow-sm">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full items-start justify-between gap-4 px-6 py-4 text-left"
        aria-expanded={open}
      >
        <div>
          <h3 className="text-base font-semibold text-gray-900">{copy.title}</h3>
          <p className="mt-1 text-xs text-gray-500">{copy.subtitle}</p>
        </div>
        <span className="shrink-0 rounded-full border border-gray-200 px-3 py-1 text-xs font-medium text-gray-600">
          {open ? copy.collapse : copy.expand}
        </span>
      </button>

      {open ? (
        <div className="border-t border-gray-100 px-6 py-4">
          {loading ? (
            <p className="text-sm text-gray-500">{copy.loading}</p>
          ) : error ? (
            <div className="flex items-center gap-3 text-sm">
              <span className="text-red-600">{copy.error}: {error}</span>
              <button
                type="button"
                onClick={retry}
                className="rounded-md border border-red-200 bg-red-50 px-2 py-1 text-xs font-medium text-red-700 hover:bg-red-100"
              >
                {copy.retry}
              </button>
            </div>
          ) : !items || items.length === 0 ? (
            <p className="text-sm text-gray-500">{copy.empty}</p>
          ) : (
            <table className="w-full text-sm">
              <thead className="text-left text-xs text-gray-500">
                <tr className="border-b border-gray-100">
                  <th className="pb-2 pr-4">{copy.columns.exDate}</th>
                  <th className="pb-2 pr-4">{copy.columns.instrument}</th>
                  <th className="pb-2 pr-4">{copy.columns.type}</th>
                  <th className="pb-2 pr-4 text-right">{copy.columns.shares}</th>
                  <th className="pb-2 pr-4 text-right">{copy.columns.cost}</th>
                  <th className="pb-2 text-right">{copy.columns.cash}</th>
                </tr>
              </thead>
              <tbody>
                {items.map((item, idx) => {
                  const sharesDelta = item.postQuantity - item.preQuantity;
                  const costDelta = item.postCostPrice - item.preCostPrice;
                  return (
                    <tr
                      key={`${item.instrumentKey}-${item.exDate}-${idx}`}
                      className="border-b border-gray-50 transition-colors last:border-0 hover:bg-gray-50/60"
                    >
                      <td className="py-2.5 pr-4 font-mono text-xs text-gray-700">
                        {formatDate(item.exDate)}
                      </td>
                      <td className="py-2.5 pr-4 text-gray-900">{item.instrumentKey}</td>
                      <td className="py-2.5 pr-4">
                        <span
                          className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-xs font-medium ${TYPE_BADGE_CLASS[item.actionType]}`}
                        >
                          {copy.types[item.actionType] ?? item.actionType}
                        </span>
                      </td>
                      <td
                        className={`py-2.5 pr-4 text-right tabular-nums ${
                          sharesDelta > 0
                            ? "text-emerald-700"
                            : sharesDelta < 0
                              ? "text-red-700"
                              : "text-gray-500"
                        }`}
                      >
                        {sharesDelta > 0 ? "+" : ""}
                        {formatNum(sharesDelta, 2)}
                        {copy.units.shares}
                      </td>
                      <td
                        className={`py-2.5 pr-4 text-right tabular-nums ${
                          costDelta < 0
                            ? "text-emerald-700"
                            : costDelta > 0
                              ? "text-red-700"
                              : "text-gray-500"
                        }`}
                      >
                        {costDelta > 0 ? "+" : ""}
                        {formatNum(costDelta, 4)}
                      </td>
                      <td
                        className={`py-2.5 text-right tabular-nums ${
                          item.cashCredit > 0 ? "text-emerald-700" : "text-gray-500"
                        }`}
                      >
                        {item.cashCredit > 0
                          ? `+${formatNum(item.cashCredit, 2)}`
                          : "—"}
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          )}
        </div>
      ) : null}
    </section>
  );
};

export default CorpActionTimeline;
