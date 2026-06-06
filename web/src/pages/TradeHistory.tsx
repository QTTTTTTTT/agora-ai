import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  apiGet,
  cancelOrder,
  fetchFundMarketQuotes,
  formatApiError,
  replaceOrder,
  type MarketQuote,
  type OrderActionResponse,
  type ReplaceOrderPayload,
} from "../lib/api";
import LiveReadinessBanner from "../components/LiveReadinessBanner";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";
import { useIsBelow } from "../lib/useBreakpoint";
// W5-1 — deep i18n migration. The full copy surface lives in
// web/src/i18n/locales/{en-US,zh-CN}/tradeHistory.ts. We import
// the en-US bundle as a *type-only* anchor so existing
// `copy.statusLabels[code as keyof typeof copy.statusLabels]`
// indexing patterns keep their narrowed types — i18next then
// returns the matching language at runtime via `getResourceBundle`.
import type tradeHistoryEn from "../i18n/locales/en-US/tradeHistory";
type TradeHistoryCopy = typeof tradeHistoryEn;

type TradeStatus = "pending" | "working" | "triggered" | "partial" | "filled" | "cancelled" | "rejected" | "expired" | string;

// isOpenOrderStatus reports whether the order is still modifiable.
// Mirrors the backend rule in trade_repo.CancelOrder/ReplaceOrderFields:
// only pending / working / triggered / partial trades may be touched.
function isOpenOrderStatus(s: string): boolean {
  return s === "pending" || s === "working" || s === "triggered" || s === "partial";
}
type TradeSide = "buy" | "sell" | string;

type RangeKey = "7d" | "30d" | "all";

interface Trade {
  id: string;
  fundId: string;
  planId?: string;
  planActionId?: string;
  instrumentKey?: string;
  symbol: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  instrumentType?: string;
  side: TradeSide;
  positionSide?: string;
  openClose?: string;
  orderType: string;
  quantity: number;
  price: number;
  amount: number;
  filledQty: number;
  filledPrice: number;
  feeCommission: number;
  feeStampTax: number;
  feeTransfer: number;
  tradingMode: string;
  status: TradeStatus;
  executedAt?: string;
  quoteCurrency?: string;
  settlementCurrency?: string;
  marginMode?: string;
  leverage?: number;
  contractMultiplier?: number;
  expiryDate?: string;
  reduceOnly?: boolean;
  // Execution strategy ("twap" / "vwap" / "limit" / ...) on rows
  // that went through the PM-direct-fill splitter (T1+ commits).
  // Empty/undefined on legacy rows. We surface it on the parent
  // row as a chip so operators see the intent at a glance.
  strategy?: string;
  // When set, this row is a child slice of a TWAP / VWAP / iceberg
  // parent; backend ?exclude_child_slices=true filters these out
  // of the list response, so any row we render here in the main
  // table is either a parent (children expandable via drilldown)
  // or a legacy non-split row. We still defensively check this
  // field so a stale cache or future regression can't sneak a
  // child into the main table.
  strategyParentTradeId?: string;
  createdAt: string;
}

function humanizeValue(value?: string, emptyLabel = "-"): string {
  if (!value) {
    return emptyLabel;
  }
  return value
    .replace(/[_-]+/g, " ")
    .trim()
    .replace(/\b\w/g, (char) => char.toUpperCase());
}

function isoStartOfDay(value: string): string {
  return new Date(`${value}T00:00:00`).toISOString();
}

function isoEndOfDay(value: string): string {
  return new Date(`${value}T23:59:59.999`).toISOString();
}

function dateInputValue(daysAgo: number): string {
  const date = new Date();
  date.setDate(date.getDate() - daysAgo);
  return date.toISOString().slice(0, 10);
}

function metaItems(...values: Array<string | undefined>): string[] {
  return values.filter((value): value is string => Boolean(value?.trim()));
}

// isMultiSliceStrategy reports whether the trade row's execution
// strategy is one that the PM-direct-fill splitter may have
// expanded into multiple child slices. We use this to decide
// whether to show the "show slices" drilldown chip — single-row
// strategies (immediate / limit) never have children so adding
// a button there would be noise. Defensive: an unknown / empty
// strategy returns false so legacy rows render the legacy way.
function isMultiSliceStrategy(strategy?: string): boolean {
  const normalized = (strategy ?? "").trim().toLowerCase();
  return normalized === "twap" || normalized === "vwap" || normalized === "iceberg" || normalized === "pov";
}

function quoteLookupKey(value?: string): string {
  return value?.trim().toUpperCase() ?? "";
}

function formatQuoteLag(asOf: string | undefined, language: string): string {
  if (!asOf) {
    return "—";
  }
  const diff = Date.now() - new Date(asOf).getTime();
  if (!Number.isFinite(diff) || diff < 0) {
    return "—";
  }
  const seconds = Math.round(diff / 1000);
  if (seconds < 60) {
    return language === "en-US" ? `${seconds}s ago` : `${seconds} 秒前`;
  }
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) {
    return language === "en-US" ? `${minutes}m ago` : `${minutes} 分钟前`;
  }
  const hours = Math.round(minutes / 60);
  if (hours < 24) {
    return language === "en-US" ? `${hours}h ago` : `${hours} 小时前`;
  }
  const days = Math.round(hours / 24);
  return language === "en-US" ? `${days}d ago` : `${days} 天前`;
}

function MetaBadge({ children }: { children: React.ReactNode }) {
  return <span className="inline-flex rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{children}</span>;
}

const TradeHistory: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const [trades, setTrades] = useState<Trade[]>([]);
  const [marketQuotes, setMarketQuotes] = useState<Record<string, MarketQuote>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [range, setRange] = useState<RangeKey>("30d");
  const [fromDate, setFromDate] = useState<string>(dateInputValue(30));
  const [toDate, setToDate] = useState<string>(dateInputValue(0));
  const [statusFilter, setStatusFilter] = useState<TradeStatus | "all">("all");
  const [busyOrderId, setBusyOrderId] = useState<string | null>(null);
  const [actionMessage, setActionMessage] = useState<{ kind: "success" | "error"; text: string } | null>(null);
  const [replaceTarget, setReplaceTarget] = useState<Trade | null>(null);
  // Drilldown state for the PM-path child-splitting feature.
  // expandedParents tracks which parent rows the operator has
  // toggled open; childrenByParent caches the per-parent slice
  // response so toggling the same row twice doesn't refetch.
  // childrenLoadingByParent / childrenErrorByParent are per-row
  // statuses so a single failed drilldown doesn't break the
  // surrounding table.
  const [expandedParents, setExpandedParents] = useState<Record<string, boolean>>({});
  const [childrenByParent, setChildrenByParent] = useState<Record<string, Trade[]>>({});
  const [childrenLoadingByParent, setChildrenLoadingByParent] = useState<Record<string, boolean>>({});
  const [childrenErrorByParent, setChildrenErrorByParent] = useState<Record<string, string | null>>({});

  // W5-1 — i18n migration. We pull the merged resource bundle
  // from i18next once per language flip and keep the existing
  // `copy.X` shape verbatim so the 100+ downstream call sites
  // in this file (statusMeta, sideMeta, modify-order modal,
  // mobile-card branch, slice splitter, etc.) need no changes.
  // The `language` dep on the memo isn't redundant: i18next's
  // `getResourceBundle` returns the bundle for whatever language
  // is currently set, and `useAppPreferences -> i18n.changeLanguage`
  // (see lib/preferences.tsx) flips that synchronously, so the
  // memo recomputes when the user toggles language.
  const { i18n } = useTranslation("tradeHistory");
  const copy = useMemo(() => {
    return i18n.getResourceBundle(i18n.language, "tradeHistory") as TradeHistoryCopy;
  }, [i18n, language]);

  const statusMeta = useCallback(
    (status: string) => {
      const badgeMap: Record<string, string> = {
        filled: "bg-emerald-50 text-emerald-700 border-emerald-200",
        partial: "bg-blue-50 text-blue-700 border-blue-200",
        pending: "bg-amber-50 text-amber-700 border-amber-200",
        cancelled: "bg-gray-50 text-gray-600 border-gray-200",
        canceled: "bg-gray-50 text-gray-600 border-gray-200",
        submitted: "bg-sky-50 text-sky-700 border-sky-200",
        rejected: "bg-red-50 text-red-700 border-red-200",
        failed: "bg-red-50 text-red-700 border-red-200",
      };
      return {
        label: copy.statusLabels[status.toLowerCase() as keyof typeof copy.statusLabels] ?? humanizeValue(status, copy.unknownStatus),
        badge: badgeMap[status.toLowerCase()] ?? "bg-gray-50 text-gray-600 border-gray-200",
      };
    },
    [copy],
  );

  const sideMeta = useCallback(
    (side: string) => {
      const normalized = side.toLowerCase();
      return normalized === "sell"
        ? { label: copy.sideLabels.sell, color: "text-red-600", bg: "bg-red-50" }
        : { label: copy.sideLabels.buy, color: "text-emerald-600", bg: "bg-emerald-50" };
    },
    [copy],
  );

  const tradingModeLabel = useCallback(
    (mode: string) => copy.tradingModes[mode.toLowerCase() as keyof typeof copy.tradingModes] ?? humanizeValue(mode, copy.unset),
    [copy],
  );

  const orderTypeLabel = useCallback(
    (value?: string) => copy.orderTypes[(value ?? "").toLowerCase() as keyof typeof copy.orderTypes] ?? humanizeValue(value, copy.unset),
    [copy],
  );

  const positionSideLabel = useCallback(
    (value?: string) => copy.positionSides[(value ?? "").toLowerCase() as keyof typeof copy.positionSides] ?? humanizeValue(value, copy.spotLong),
    [copy],
  );

  const openCloseLabel = useCallback(
    (value?: string) => copy.openClose[(value ?? "").toLowerCase() as keyof typeof copy.openClose] ?? humanizeValue(value, copy.unset),
    [copy],
  );

  const formatQuantity = useCallback(
    (value?: number) =>
      typeof value === "number" && !Number.isNaN(value)
        ? formatNumberForLanguage(value, language, { maximumFractionDigits: 4 })
        : "—",
    [language],
  );

  const loadTrades = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const params = new URLSearchParams();
      params.set("limit", "200");
      params.set("offset", "0");
      // PM-path child-splitting (T1+ commits) generates 1 parent
      // + N child trade rows per plan_action. The list view stays
      // at "one row per plan_action" by asking the backend to
      // filter out child slices; the operator drills into per-
      // slice detail via the expandable "+N slices" button on the
      // parent row.
      params.set("exclude_child_slices", "true");
      if (range !== "all") {
        if (fromDate) {
          params.set("from", isoStartOfDay(fromDate));
        }
        if (toDate) {
          params.set("to", isoEndOfDay(toDate));
        }
      }
      const path = `/api/funds/${fundId}/trades?${params.toString()}`;
      const response = await apiGet<Trade[]>(path);
      // Defensive filter: even though we asked the backend to
      // exclude children, drop anything that DID slip through
      // (stale caches, future regression) so the main table is
      // 100% guaranteed parent-or-standalone only.
      const sorted = (response ?? [])
        .filter((trade) => !trade.strategyParentTradeId)
        .slice()
        .sort((a, b) => {
          const left = a.executedAt ?? a.createdAt;
          const right = b.executedAt ?? b.createdAt;
          return right.localeCompare(left);
        });
      setTrades(sorted);
      // Reset drilldown state on every reload — the cached
      // child rows would otherwise drift out of sync with the
      // freshly-loaded parents.
      setExpandedParents({});
      setChildrenByParent({});
      setChildrenLoadingByParent({});
      setChildrenErrorByParent({});

      const quoteSymbols = Array.from(new Set(sorted.map((trade) => trade.symbol.trim()).filter(Boolean)));
      if (quoteSymbols.length === 0) {
        setMarketQuotes({});
        return;
      }
      const quotesResponse = await fetchFundMarketQuotes(fundId, quoteSymbols);
      const nextQuotes = (quotesResponse.quotes ?? []).reduce(
        (acc, quote) => {
          const key = quoteLookupKey(quote.symbol || quote.instrumentKey);
          if (key) {
            acc[key] = quote;
          }
          return acc;
        },
        {} as Record<string, MarketQuote>,
      );
      setMarketQuotes(nextQuotes);
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, copy.missingFundId, fromDate, fundId, range, toDate]);

  useEffect(() => {
    void loadTrades();
  }, [loadTrades]);

  // toggleParentExpand opens / closes the drilldown panel on a
  // parent trade row. First open triggers a lazy fetch of the
  // per-slice children; subsequent toggles reuse the cached
  // response. Errors are kept per-row so a single failed
  // drilldown can't poison the surrounding table.
  const toggleParentExpand = useCallback(
    async (parent: Trade) => {
      const parentId = parent.id;
      const isOpen = expandedParents[parentId] ?? false;
      // Always flip the open state first so the user gets
      // immediate feedback even if the fetch is slow.
      setExpandedParents((prev) => ({ ...prev, [parentId]: !isOpen }));
      // Only fetch on first-open — if we already have data
      // (or a sticky error) just toggle visibility.
      if (isOpen || childrenByParent[parentId] !== undefined || childrenLoadingByParent[parentId]) {
        return;
      }
      if (!fundId) return;
      setChildrenLoadingByParent((prev) => ({ ...prev, [parentId]: true }));
      setChildrenErrorByParent((prev) => ({ ...prev, [parentId]: null }));
      try {
        const path = `/api/funds/${fundId}/trades/${encodeURIComponent(parentId)}/children`;
        const rows = await apiGet<Trade[]>(path);
        setChildrenByParent((prev) => ({ ...prev, [parentId]: rows ?? [] }));
      } catch (err) {
        setChildrenErrorByParent((prev) => ({ ...prev, [parentId]: formatApiError(err, copy.loadError) }));
      } finally {
        setChildrenLoadingByParent((prev) => ({ ...prev, [parentId]: false }));
      }
    },
    [childrenByParent, childrenLoadingByParent, copy.loadError, expandedParents, fundId],
  );

  // applyOrderUpdate patches a single trade in local state with the
  // post-action snapshot returned by the server. Avoids a full
  // reload (and the marketQuotes refetch) when only one row moved.
  const applyOrderUpdate = useCallback((updated: OrderActionResponse) => {
    setTrades((prev) =>
      prev.map((t) =>
        t.id === updated.id
          ? {
              ...t,
              status: updated.status as TradeStatus,
              quantity: updated.quantity,
              filledQty: updated.filledQty,
              price: updated.limitPrice ?? t.price,
            }
          : t,
      ),
    );
  }, []);

  const handleCancel = useCallback(
    async (trade: Trade) => {
      if (!fundId) return;
      const confirmed = window.confirm(copy.actions.cancelConfirm);
      if (!confirmed) return;
      setBusyOrderId(trade.id);
      setActionMessage(null);
      try {
        const updated = await cancelOrder(fundId, trade.id, { reason: "user_requested" });
        applyOrderUpdate(updated);
        setActionMessage({ kind: "success", text: copy.actions.cancelSuccess });
      } catch (err) {
        setActionMessage({ kind: "error", text: formatApiError(err, copy.actions.error) });
      } finally {
        setBusyOrderId(null);
      }
    },
    [applyOrderUpdate, copy.actions, fundId],
  );

  const handleReplaceSubmit = useCallback(
    async (trade: Trade, payload: ReplaceOrderPayload) => {
      if (!fundId) return;
      setBusyOrderId(trade.id);
      setActionMessage(null);
      try {
        const updated = await replaceOrder(fundId, trade.id, payload);
        applyOrderUpdate(updated);
        setReplaceTarget(null);
        setActionMessage({ kind: "success", text: copy.actions.replaceSuccess });
      } catch (err) {
        setActionMessage({ kind: "error", text: formatApiError(err, copy.actions.error) });
      } finally {
        setBusyOrderId(null);
      }
    },
    [applyOrderUpdate, copy.actions, fundId],
  );

  const visibleTrades = useMemo(() => {
    return statusFilter === "all" ? trades : trades.filter((trade) => trade.status === statusFilter);
  }, [statusFilter, trades]);

  const summary = useMemo(() => {
    return visibleTrades.reduce(
      (acc, trade) => {
        acc.total += 1;
        if (trade.status === "filled" || trade.status === "partial") {
          acc.executed += 1;
        }
        if (trade.status === "pending") {
          acc.pending += 1;
        }
        acc.notional += trade.amount || (trade.filledQty || trade.quantity) * (trade.filledPrice || trade.price || 0);
        acc.fees += (trade.feeCommission ?? 0) + (trade.feeStampTax ?? 0) + (trade.feeTransfer ?? 0);
        return acc;
      },
      { total: 0, executed: 0, pending: 0, notional: 0, fees: 0 },
    );
  }, [visibleTrades]);

  const statusOptions = useMemo(() => Array.from(new Set(trades.map((trade) => trade.status))), [trades]);

  // W4-24 ResponsiveTable wiring: below the md breakpoint we
  // render a stripped-down card view per trade. The desktop
  // table is preserved verbatim — it carries too much
  // operationally-essential structure (live-quote chip, child-
  // slice expander, modify/cancel actions) to be safely
  // recomposed for mobile, so the card view is intentionally
  // a "read-only summary" surface and we direct operators
  // to the desktop view for actions.
  const isMobile = useIsBelow("md");

  if (loading) {
    return <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadTrades()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      {fundId ? (
        <LiveReadinessBanner fundId={fundId} language={language} />
      ) : null}
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <button onClick={() => void loadTrades()} className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50">
          {copy.refresh}
        </button>
      </div>

      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-4 xl:flex-row xl:items-end xl:justify-between">
          <div className="flex flex-wrap gap-2">
            {(["7d", "30d", "all"] as const).map((key) => (
              <button
                key={key}
                onClick={() => {
                  setRange(key);
                  if (key === "7d") {
                    setFromDate(dateInputValue(7));
                    setToDate(dateInputValue(0));
                  }
                  if (key === "30d") {
                    setFromDate(dateInputValue(30));
                    setToDate(dateInputValue(0));
                  }
                }}
                className={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors ${
                  range === key ? "bg-indigo-600 text-white" : "bg-gray-100 text-gray-600 hover:bg-gray-200"
                }`}
              >
                {copy.ranges[key]}
              </button>
            ))}
          </div>

          <div className="grid grid-cols-1 gap-3 sm:grid-cols-3">
            <label className="text-sm text-gray-600">
              <span className="mb-1 block">{copy.fromDate}</span>
              <input
                type="date"
                value={fromDate}
                onChange={(event) => {
                  setRange("all");
                  setFromDate(event.target.value);
                }}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm text-gray-700"
              />
            </label>
            <label className="text-sm text-gray-600">
              <span className="mb-1 block">{copy.toDate}</span>
              <input
                type="date"
                value={toDate}
                onChange={(event) => {
                  setRange("all");
                  setToDate(event.target.value);
                }}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm text-gray-700"
              />
            </label>
            <label className="text-sm text-gray-600">
              <span className="mb-1 block">{copy.statusFilter}</span>
              <select
                value={statusFilter}
                onChange={(event) => setStatusFilter(event.target.value)}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm text-gray-700"
              >
                <option value="all">{copy.allStatuses}</option>
                {statusOptions.map((status) => (
                  <option key={status} value={status}>
                    {statusMeta(status).label}
                  </option>
                ))}
              </select>
            </label>
          </div>
        </div>

        <div className="mt-6 grid grid-cols-2 gap-4 lg:grid-cols-5">
          <div className="rounded-xl bg-gray-50 px-4 py-4">
            <p className="text-xs text-gray-500">{copy.summary.total}</p>
            <p className="mt-1 text-2xl font-semibold text-gray-900">{formatNumberForLanguage(summary.total, language)}</p>
          </div>
          <div className="rounded-xl bg-gray-50 px-4 py-4">
            <p className="text-xs text-gray-500">{copy.summary.executed}</p>
            <p className="mt-1 text-2xl font-semibold text-emerald-600">{formatNumberForLanguage(summary.executed, language)}</p>
          </div>
          <div className="rounded-xl bg-gray-50 px-4 py-4">
            <p className="text-xs text-gray-500">{copy.summary.pending}</p>
            <p className="mt-1 text-2xl font-semibold text-amber-600">{formatNumberForLanguage(summary.pending, language)}</p>
          </div>
          <div className="rounded-xl bg-gray-50 px-4 py-4">
            <p className="text-xs text-gray-500">{copy.summary.notional}</p>
            <p className="mt-1 text-2xl font-semibold text-gray-900">{formatMoneyForDisplay(summary.notional, "USD", displayCurrency, language)}</p>
          </div>
          <div className="rounded-xl bg-gray-50 px-4 py-4">
            <p className="text-xs text-gray-500">{copy.summary.fees}</p>
            <p className="mt-1 text-2xl font-semibold text-gray-900">{formatMoneyForDisplay(summary.fees, "USD", displayCurrency, language)}</p>
          </div>
        </div>
      </div>

      {trades.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
          <p className="font-medium text-gray-700">{copy.emptyTitle}</p>
          <p className="mt-2">{copy.emptyDescription}</p>
          <Link to="../decisions" className="mt-4 inline-flex rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
            {copy.goToDecisionCenter}
          </Link>
        </div>
      ) : visibleTrades.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
          <p className="font-medium text-gray-700">{copy.filteredEmptyTitle}</p>
          <p className="mt-2">{copy.filteredEmptyDescription}</p>
          <button
            onClick={() => {
              setRange("30d");
              setFromDate(dateInputValue(30));
              setToDate(dateInputValue(0));
              setStatusFilter("all");
            }}
            className="mt-4 rounded-lg border border-gray-300 px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50"
          >
            {copy.resetFilters}
          </button>
        </div>
      ) : isMobile ? (
        <div className="space-y-2">
          <div className="rounded-xl border border-gray-200 bg-white px-4 py-3 text-xs text-gray-500 shadow-sm">
            {copy.accumulatedFees} {formatMoneyForDisplay(summary.fees, "USD", displayCurrency, language)}
          </div>
          <ul className="space-y-2">
            {visibleTrades.map((trade) => {
              const side = sideMeta(trade.side);
              const status = statusMeta(trade.status);
              const effectiveQuantity = trade.filledQty || trade.quantity;
              const effectivePrice = trade.filledPrice || trade.price;
              const effectiveAmount =
                trade.amount || effectiveQuantity * effectivePrice;
              const priceCurrency = trade.quoteCurrency || "USD";
              const amountCurrency =
                trade.settlementCurrency || trade.quoteCurrency || "USD";
              return (
                <li
                  key={trade.id}
                  className="rounded-xl border border-gray-200 bg-white p-3 shadow-sm"
                >
                  <div className="flex items-start justify-between gap-2">
                    <div>
                      <p className="text-sm font-semibold text-gray-900">{trade.symbol}</p>
                      <p className="text-[11px] text-gray-400">
                        {formatDateTimeForLanguage(trade.executedAt ?? trade.createdAt, language)}
                      </p>
                    </div>
                    <span className={`rounded-full px-2 py-0.5 text-[11px] font-medium ${side.bg} ${side.color}`}>
                      {side.label}
                    </span>
                  </div>
                  <dl className="mt-3 grid grid-cols-2 gap-x-3 gap-y-1.5 text-xs">
                    <dt className="text-gray-500">{copy.columns.quantity}</dt>
                    <dd className="text-right text-gray-800">
                      {formatNumberForLanguage(effectiveQuantity, language)}
                    </dd>
                    <dt className="text-gray-500">{copy.columns.price}</dt>
                    <dd className="text-right text-gray-800">
                      {formatMoneyForDisplay(effectivePrice, priceCurrency, displayCurrency, language)}
                    </dd>
                    <dt className="text-gray-500">{copy.columns.amount}</dt>
                    <dd className="text-right text-gray-800">
                      {formatMoneyForDisplay(effectiveAmount, amountCurrency, displayCurrency, language)}
                    </dd>
                    <dt className="text-gray-500">{copy.columns.status}</dt>
                    <dd className="text-right">
                      <span className={`inline-flex rounded-full border px-2 py-0.5 text-[11px] font-medium ${status.badge}`}>
                        {status.label}
                      </span>
                    </dd>
                  </dl>
                </li>
              );
            })}
          </ul>
        </div>
      ) : (
        <div className="overflow-hidden rounded-2xl border border-gray-200 bg-white shadow-sm">
          <div className="flex items-center justify-between border-b border-gray-100 px-6 py-4">
            <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.details}</h2>
            <span className="text-xs text-gray-500">
              {copy.accumulatedFees} {formatMoneyForDisplay(summary.fees, "USD", displayCurrency, language)}
            </span>
          </div>
          <div className="overflow-x-auto">
            <table className="min-w-full text-sm">
              <thead>
                <tr className="bg-gray-50 text-left text-xs text-gray-500">
                  <th className="px-4 py-3 font-medium">{copy.columns.time}</th>
                  <th className="px-4 py-3 font-medium">{copy.columns.instrument}</th>
                  <th className="px-4 py-3 font-medium">{copy.columns.side}</th>
                  <th className="px-4 py-3 text-right font-medium">{copy.columns.quantity}</th>
                  <th className="px-4 py-3 text-right font-medium">{copy.columns.price}</th>
                  <th className="px-4 py-3 text-right font-medium">{copy.columns.amount}</th>
                  <th className="px-4 py-3 text-right font-medium">{copy.columns.fee}</th>
                  <th className="px-4 py-3 font-medium">{copy.columns.mode}</th>
                  <th className="px-4 py-3 font-medium">{copy.columns.status}</th>
                  <th className="px-4 py-3 text-right font-medium">{copy.columns.actions}</th>
                </tr>
              </thead>
              <tbody>
                {visibleTrades.map((trade) => {
                  const side = sideMeta(trade.side);
                  const status = statusMeta(trade.status);
                  const feeTotal = (trade.feeCommission ?? 0) + (trade.feeStampTax ?? 0) + (trade.feeTransfer ?? 0);
                  const effectiveQuantity = trade.filledQty || trade.quantity;
                  const effectivePrice = trade.filledPrice || trade.price;
                  const effectiveAmount = trade.amount || effectiveQuantity * effectivePrice;
                  const priceCurrency = trade.quoteCurrency || "USD";
                  const amountCurrency = trade.settlementCurrency || trade.quoteCurrency || "USD";
                  const liveQuote = marketQuotes[quoteLookupKey(trade.symbol)] ?? marketQuotes[quoteLookupKey(trade.instrumentKey)] ?? null;
                  const quoteGap = liveQuote && effectivePrice > 0 ? ((liveQuote.price - effectivePrice) / effectivePrice) * 100 : null;
                  const hasSlices = isMultiSliceStrategy(trade.strategy);
                  const isExpanded = expandedParents[trade.id] ?? false;
                  const children = childrenByParent[trade.id];
                  const childrenLoading = childrenLoadingByParent[trade.id] ?? false;
                  const childrenError = childrenErrorByParent[trade.id];
                  return (
                    <React.Fragment key={trade.id}>
                    <tr className="border-t border-gray-100 align-top">
                      <td className="px-4 py-4 text-gray-600">{formatDateTimeForLanguage(trade.executedAt ?? trade.createdAt, language)}</td>
                      <td className="px-4 py-4">
                        <div>
                          <p className="font-semibold text-gray-900">{trade.symbol}</p>
                          <p className="mt-1 text-xs text-gray-400">{trade.instrumentKey || trade.symbol}</p>
                          <div className="mt-2 flex flex-wrap gap-1.5">
                            {metaItems(trade.market, trade.exchange, trade.assetClass).map((item) => (
                              <MetaBadge key={`${trade.id}-${item}`}>{humanizeValue(item)}</MetaBadge>
                            ))}
                            {trade.expiryDate ? <MetaBadge>{formatDateForLanguage(trade.expiryDate, language)}</MetaBadge> : null}
                          </div>
                          <p className="mt-2 text-xs text-gray-400">
                            {copy.orderType}: {orderTypeLabel(trade.orderType)}
                          </p>
                          <div className="mt-3 rounded-lg border border-gray-100 bg-gray-50 px-3 py-2 text-xs text-gray-500">
                            {liveQuote ? (
                              <div className="space-y-1.5">
                                <div className="flex items-center justify-between gap-3">
                                  <span>{copy.liveQuote}</span>
                                  <span className="font-medium text-gray-800">
                                    {formatMoneyForDisplay(liveQuote.price, liveQuote.quoteCurrency || priceCurrency, displayCurrency, language)}
                                  </span>
                                </div>
                                <div className="flex flex-wrap gap-x-3 gap-y-1 text-[11px] text-gray-400">
                                  <span>
                                    {copy.quoteSource}: {liveQuote.source}
                                  </span>
                                  <span>
                                    {copy.quoteAsOf}: {formatDateTimeForLanguage(liveQuote.asOf, language)}
                                  </span>
                                  <span>
                                    {copy.quoteLag}: {formatQuoteLag(liveQuote.asOf, language)}
                                  </span>
                                  {liveQuote.isStale ? <span className="font-medium text-amber-600">{copy.quoteStale}</span> : null}
                                  {typeof quoteGap === "number" ? (
                                    <span className={quoteGap >= 0 ? "text-emerald-600" : "text-red-600"}>
                                      {copy.quotePriceGap}: {quoteGap >= 0 ? "+" : ""}
                                      {formatNumberForLanguage(quoteGap, language, { maximumFractionDigits: 2 })}%
                                    </span>
                                  ) : null}
                                </div>
                              </div>
                            ) : (
                              <span>{copy.quoteMissing}</span>
                            )}
                          </div>
                        </div>
                      </td>
                      <td className="px-4 py-4">
                        <div className="space-y-2">
                          <span className={`inline-flex rounded-full px-2.5 py-1 text-xs font-medium ${side.bg} ${side.color}`}>{side.label}</span>
                          <div className="flex flex-wrap gap-1.5 text-xs text-gray-500">
                            {trade.positionSide ? <MetaBadge>{positionSideLabel(trade.positionSide)}</MetaBadge> : null}
                            {trade.openClose ? <MetaBadge>{openCloseLabel(trade.openClose)}</MetaBadge> : null}
                            {typeof trade.leverage === "number" ? <MetaBadge>{formatNumberForLanguage(trade.leverage, language)}x</MetaBadge> : null}
                            {trade.marginMode ? <MetaBadge>{humanizeValue(trade.marginMode)}</MetaBadge> : null}
                            {trade.reduceOnly ? <MetaBadge>{copy.reduceOnly}</MetaBadge> : null}
                          </div>
                        </div>
                      </td>
                      <td className="px-4 py-4 text-right text-gray-700">{formatQuantity(effectiveQuantity)}</td>
                      <td className="px-4 py-4 text-right text-gray-700">{formatMoneyForDisplay(effectivePrice, priceCurrency, displayCurrency, language)}</td>
                      <td className="px-4 py-4 text-right font-medium text-gray-900">{formatMoneyForDisplay(effectiveAmount, amountCurrency, displayCurrency, language)}</td>
                      <td className="px-4 py-4 text-right text-gray-600">{formatMoneyForDisplay(feeTotal, amountCurrency, displayCurrency, language)}</td>
                      <td className="px-4 py-4 text-gray-600">{tradingModeLabel(trade.tradingMode)}</td>
                      <td className="px-4 py-4">
                        <div className="flex flex-col items-start gap-1.5">
                          <span className={`inline-flex rounded-full border px-2.5 py-1 text-xs font-medium ${status.badge}`}>{status.label}</span>
                          {trade.strategy ? (
                            <span className="inline-flex rounded-full border border-indigo-100 bg-indigo-50 px-2 py-0.5 text-[10px] font-medium uppercase tracking-wider text-indigo-600">
                              {copy.splitter.strategyLabel}: {trade.strategy}
                            </span>
                          ) : null}
                          {hasSlices ? (
                            <button
                              type="button"
                              onClick={() => void toggleParentExpand(trade)}
                              className="rounded-md border border-indigo-200 bg-white px-2 py-1 text-[11px] font-medium text-indigo-700 hover:bg-indigo-50"
                            >
                              {isExpanded ? copy.splitter.collapse : copy.splitter.expand}
                              {children ? ` (${children.length})` : ""}
                            </button>
                          ) : null}
                        </div>
                      </td>
                      <td className="px-4 py-4 text-right">
                        {isOpenOrderStatus(trade.status) ? (
                          <div className="flex flex-col items-end gap-1">
                            <button
                              type="button"
                              onClick={() => setReplaceTarget(trade)}
                              disabled={busyOrderId === trade.id}
                              className="rounded-md border border-indigo-200 bg-indigo-50 px-2.5 py-1 text-xs font-medium text-indigo-700 hover:bg-indigo-100 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {busyOrderId === trade.id ? copy.actions.replacing : copy.actions.replace}
                            </button>
                            <button
                              type="button"
                              onClick={() => void handleCancel(trade)}
                              disabled={busyOrderId === trade.id}
                              className="rounded-md border border-red-200 bg-red-50 px-2.5 py-1 text-xs font-medium text-red-700 hover:bg-red-100 disabled:cursor-not-allowed disabled:opacity-60"
                            >
                              {busyOrderId === trade.id ? copy.actions.cancelling : copy.actions.cancel}
                            </button>
                          </div>
                        ) : (
                          <span className="text-xs text-gray-400">—</span>
                        )}
                      </td>
                    </tr>
                    {hasSlices && isExpanded ? (
                      <tr key={`${trade.id}-children`} className="border-t border-indigo-50 bg-indigo-50/30">
                        <td colSpan={10} className="px-6 py-4">
                          {childrenLoading ? (
                            <span className="text-xs text-gray-500">{copy.splitter.loading}</span>
                          ) : childrenError ? (
                            <span className="text-xs text-red-600">
                              {copy.splitter.error}: {childrenError}
                            </span>
                          ) : children && children.length === 0 ? (
                            <span className="text-xs text-gray-500">{copy.splitter.empty}</span>
                          ) : children ? (
                            <div className="overflow-x-auto">
                              <table className="min-w-full text-xs">
                                <thead>
                                  <tr className="text-left text-[11px] uppercase tracking-wider text-indigo-700">
                                    <th className="py-2 pr-4 font-medium">#</th>
                                    <th className="py-2 pr-4 font-medium">{copy.columns.time}</th>
                                    <th className="py-2 pr-4 text-right font-medium">{copy.columns.quantity}</th>
                                    <th className="py-2 pr-4 text-right font-medium">{copy.columns.price}</th>
                                    <th className="py-2 pr-4 text-right font-medium">{copy.columns.amount}</th>
                                    <th className="py-2 pr-4 text-right font-medium">{copy.columns.fee}</th>
                                    <th className="py-2 pr-4 font-medium">{copy.columns.status}</th>
                                  </tr>
                                </thead>
                                <tbody>
                                  {children.map((child, idx) => {
                                    const childStatus = statusMeta(child.status);
                                    const childQty = child.filledQty || child.quantity;
                                    const childPrice = child.filledPrice || child.price;
                                    const childAmount = child.amount || childQty * childPrice;
                                    const childFee = (child.feeCommission ?? 0) + (child.feeStampTax ?? 0) + (child.feeTransfer ?? 0);
                                    const childPriceCurrency = child.quoteCurrency || priceCurrency;
                                    const childAmountCurrency = child.settlementCurrency || child.quoteCurrency || amountCurrency;
                                    return (
                                      <tr key={child.id} className="border-t border-indigo-100/60">
                                        <td className="py-2 pr-4 text-gray-500">
                                          {copy.splitter.sliceIndex} {idx + 1}
                                        </td>
                                        <td className="py-2 pr-4 text-gray-600">{formatDateTimeForLanguage(child.executedAt ?? child.createdAt, language)}</td>
                                        <td className="py-2 pr-4 text-right text-gray-700">{formatQuantity(childQty)}</td>
                                        <td className="py-2 pr-4 text-right text-gray-700">{formatMoneyForDisplay(childPrice, childPriceCurrency, displayCurrency, language)}</td>
                                        <td className="py-2 pr-4 text-right text-gray-900">{formatMoneyForDisplay(childAmount, childAmountCurrency, displayCurrency, language)}</td>
                                        <td className="py-2 pr-4 text-right text-gray-600">{formatMoneyForDisplay(childFee, childAmountCurrency, displayCurrency, language)}</td>
                                        <td className="py-2 pr-4">
                                          <span className={`inline-flex rounded-full border px-2 py-0.5 text-[10px] font-medium ${childStatus.badge}`}>{childStatus.label}</span>
                                        </td>
                                      </tr>
                                    );
                                  })}
                                </tbody>
                              </table>
                            </div>
                          ) : null}
                        </td>
                      </tr>
                    ) : null}
                    </React.Fragment>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}

      {actionMessage ? (
        <div
          className={`rounded-xl border px-4 py-3 text-sm ${
            actionMessage.kind === "success"
              ? "border-emerald-200 bg-emerald-50 text-emerald-700"
              : "border-red-200 bg-red-50 text-red-700"
          }`}
        >
          <div className="flex items-center justify-between gap-4">
            <span>{actionMessage.text}</span>
            <button
              type="button"
              onClick={() => setActionMessage(null)}
              className="text-xs font-medium underline-offset-2 hover:underline"
            >
              {copy.actions.dismiss}
            </button>
          </div>
        </div>
      ) : null}

      {replaceTarget ? (
        <ReplaceOrderModal
          trade={replaceTarget}
          copy={copy.actions}
          submitting={busyOrderId === replaceTarget.id}
          onCancel={() => setReplaceTarget(null)}
          onSubmit={(payload) => void handleReplaceSubmit(replaceTarget, payload)}
        />
      ) : null}
    </div>
  );
};

// ---------------------------------------------------------------------------
// ReplaceOrderModal
// ---------------------------------------------------------------------------

type ReplaceCopy = {
  replaceTitle: string;
  replaceQuantity: string;
  replaceLimit: string;
  replaceStop: string;
  replaceTrailAmount: string;
  replaceTrailPercent: string;
  replaceDisplayQty: string;
  replaceNote: string;
  replaceLeaveBlankHelp: string;
  save: string;
  cancelButton: string;
  replacing: string;
};

interface ReplaceOrderModalProps {
  trade: Trade;
  copy: ReplaceCopy;
  submitting: boolean;
  onCancel: () => void;
  onSubmit: (payload: ReplaceOrderPayload) => void;
}

const ReplaceOrderModal: React.FC<ReplaceOrderModalProps> = ({ trade, copy, submitting, onCancel, onSubmit }) => {
  const [quantity, setQuantity] = useState<string>("");
  const [limitPrice, setLimitPrice] = useState<string>("");
  const [stopPrice, setStopPrice] = useState<string>("");
  const [trailAmount, setTrailAmount] = useState<string>("");
  const [trailPercent, setTrailPercent] = useState<string>("");
  const [displayQty, setDisplayQty] = useState<string>("");
  const [note, setNote] = useState<string>("");

  const supportsLimit = trade.orderType === "limit" || trade.orderType === "stop_limit" || trade.orderType === "iceberg";
  const supportsStop = trade.orderType === "stop" || trade.orderType === "stop_limit" || trade.orderType === "trailing_stop";
  const supportsTrail = trade.orderType === "trailing_stop";
  const supportsDisplayQty = trade.orderType === "iceberg";

  const submit = (event: React.FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    const payload: ReplaceOrderPayload = {};
    const parseNum = (v: string): number | undefined => {
      const trimmed = v.trim();
      if (!trimmed) return undefined;
      const num = Number(trimmed);
      return Number.isFinite(num) && num > 0 ? num : undefined;
    };
    const q = parseNum(quantity);
    if (q !== undefined) payload.quantity = q;
    if (supportsLimit) {
      const lp = parseNum(limitPrice);
      if (lp !== undefined) payload.limitPrice = lp;
    }
    if (supportsStop) {
      const sp = parseNum(stopPrice);
      if (sp !== undefined) payload.stopPrice = sp;
    }
    if (supportsTrail) {
      const ta = parseNum(trailAmount);
      if (ta !== undefined) payload.trailAmount = ta;
      const tp = parseNum(trailPercent);
      if (tp !== undefined && tp < 1) payload.trailPercent = tp;
    }
    if (supportsDisplayQty) {
      const dq = parseNum(displayQty);
      if (dq !== undefined) payload.displayQty = dq;
    }
    const n = note.trim();
    if (n) payload.note = n;
    if (Object.keys(payload).length === 0 || (Object.keys(payload).length === 1 && payload.note)) {
      // Pure note-only changes are still rejected by the backend
      // since note doesn't count as a field change. Block here so
      // the user gets immediate feedback.
      return;
    }
    onSubmit(payload);
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 px-4">
      <div className="w-full max-w-md rounded-2xl bg-white p-6 shadow-2xl">
        <div className="mb-4 flex items-start justify-between">
          <div>
            <h3 className="text-lg font-semibold text-gray-900">{copy.replaceTitle}</h3>
            <p className="mt-1 text-xs text-gray-500">
              {trade.symbol} · {trade.side.toUpperCase()} · {trade.orderType}
            </p>
          </div>
          <button type="button" onClick={onCancel} className="rounded-md p-1 text-gray-400 hover:bg-gray-100" aria-label="close">
            ×
          </button>
        </div>
        <p className="mb-4 text-xs text-gray-500">{copy.replaceLeaveBlankHelp}</p>
        <form onSubmit={submit} className="space-y-3">
          <label className="block text-sm">
            <span className="mb-1 block text-gray-700">{copy.replaceQuantity}</span>
            <input
              type="number"
              min="0"
              step="any"
              value={quantity}
              onChange={(e) => setQuantity(e.target.value)}
              placeholder={String(trade.quantity)}
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            />
          </label>
          {supportsLimit ? (
            <label className="block text-sm">
              <span className="mb-1 block text-gray-700">{copy.replaceLimit}</span>
              <input
                type="number"
                min="0"
                step="any"
                value={limitPrice}
                onChange={(e) => setLimitPrice(e.target.value)}
                placeholder={String(trade.price)}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
              />
            </label>
          ) : null}
          {supportsStop ? (
            <label className="block text-sm">
              <span className="mb-1 block text-gray-700">{copy.replaceStop}</span>
              <input
                type="number"
                min="0"
                step="any"
                value={stopPrice}
                onChange={(e) => setStopPrice(e.target.value)}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
              />
            </label>
          ) : null}
          {supportsTrail ? (
            <>
              <label className="block text-sm">
                <span className="mb-1 block text-gray-700">{copy.replaceTrailAmount}</span>
                <input
                  type="number"
                  min="0"
                  step="any"
                  value={trailAmount}
                  onChange={(e) => setTrailAmount(e.target.value)}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
                />
              </label>
              <label className="block text-sm">
                <span className="mb-1 block text-gray-700">{copy.replaceTrailPercent}</span>
                <input
                  type="number"
                  min="0"
                  max="0.999999"
                  step="any"
                  value={trailPercent}
                  onChange={(e) => setTrailPercent(e.target.value)}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
                />
              </label>
            </>
          ) : null}
          {supportsDisplayQty ? (
            <label className="block text-sm">
              <span className="mb-1 block text-gray-700">{copy.replaceDisplayQty}</span>
              <input
                type="number"
                min="0"
                step="any"
                value={displayQty}
                onChange={(e) => setDisplayQty(e.target.value)}
                className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
              />
            </label>
          ) : null}
          <label className="block text-sm">
            <span className="mb-1 block text-gray-700">{copy.replaceNote}</span>
            <input
              type="text"
              maxLength={200}
              value={note}
              onChange={(e) => setNote(e.target.value)}
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            />
          </label>
          <div className="mt-4 flex items-center justify-end gap-2">
            <button
              type="button"
              onClick={onCancel}
              className="rounded-md border border-gray-300 bg-white px-3 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50"
            >
              {copy.cancelButton}
            </button>
            <button
              type="submit"
              disabled={submitting}
              className="rounded-md bg-indigo-600 px-3 py-2 text-sm font-semibold text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {submitting ? copy.replacing : copy.save}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

export default TradeHistory;
