import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { apiGet, fetchFundMarketQuotes, formatApiError, type MarketQuote } from "../lib/api";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

type TradeStatus = "pending" | "filled" | "partial" | "cancelled" | "rejected" | string;
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

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            missingFundId: "Missing fundId",
            loadError: "Failed to load trade history",
            loading: "Loading trade history...",
            retry: "Retry",
            title: "Trade history",
            subtitle: "Review filled, pending, and failed orders, and quickly verify notional, fees, trading mode, and execution time.",
            refresh: "Refresh",
            ranges: {
              "7d": "Last 7 days",
              "30d": "Last 30 days",
              all: "All",
            },
            fromDate: "Start date",
            toDate: "End date",
            statusFilter: "Status",
            allStatuses: "All statuses",
            summary: {
              total: "Records",
              executed: "Executed",
              pending: "Pending",
              notional: "Notional",
              fees: "Fees",
            },
            emptyTitle: "No trade history yet",
            emptyDescription: "Complete plan approval and move into execution to accumulate fills and execution logs here.",
            goToDecisionCenter: "Go to decision center",
            filteredEmptyTitle: "No records match the current filters",
            filteredEmptyDescription: "Adjust the date range or status filter to review the execution details you need.",
            resetFilters: "Reset filters",
            details: "Execution details",
            accumulatedFees: "Accumulated fees",
            columns: {
              time: "Time",
              instrument: "Instrument",
              side: "Side",
              quantity: "Quantity",
              price: "Price",
              amount: "Amount",
              fee: "Fee",
              mode: "Mode",
              status: "Status",
            },
            orderType: "Order type",
            reduceOnly: "Reduce only",
            liveQuote: "Live quote",
            quoteSource: "Quote source",
            quoteAsOf: "Quote as of",
            quoteLag: "Freshness",
            quoteMissing: "No market snapshot",
            quoteStale: "Stale",
            quotePriceGap: "Vs fill",
            notExecuted: "Not executed",
            notRecorded: "Not recorded",
            statusLabels: {
              filled: "Filled",
              partial: "Partially filled",
              pending: "Pending",
              cancelled: "Cancelled",
              canceled: "Cancelled",
              submitted: "Submitted",
              rejected: "Rejected",
              failed: "Rejected",
            },
            sideLabels: {
              buy: "Buy",
              sell: "Sell",
            },
            tradingModes: {
              live: "Live",
              paper: "Paper",
              simulation: "Simulation",
            },
            orderTypes: {
              market: "Market",
              limit: "Limit",
              stop: "Stop",
              stop_limit: "Stop limit",
            },
            positionSides: {
              long: "Long",
              short: "Short",
            },
            openClose: {
              open: "Open",
              close: "Close",
              close_today: "Close today",
              roll: "Roll",
            },
            unknownStatus: "Unknown status",
            spotLong: "Spot / long",
            unset: "Unset",
          }
        : {
            missingFundId: "缺少 fundId",
            loadError: "加载交易记录失败",
            loading: "正在加载交易记录...",
            retry: "重试",
            title: "交易记录",
            subtitle: "查看成交、待执行与失败订单，快速核对成交金额、费用、模式与执行时间。",
            refresh: "刷新",
            ranges: {
              "7d": "近 7 天",
              "30d": "近 30 天",
              all: "全部",
            },
            fromDate: "开始日期",
            toDate: "结束日期",
            statusFilter: "状态筛选",
            allStatuses: "全部状态",
            summary: {
              total: "记录数",
              executed: "已执行",
              pending: "待执行",
              notional: "成交金额",
              fees: "累计费用",
            },
            emptyTitle: "当前还没有交易记录",
            emptyDescription: "先完成计划审批并进入执行阶段，成交与执行日志会自动沉淀到这里。",
            goToDecisionCenter: "前往决策中心",
            filteredEmptyTitle: "当前筛选条件下没有匹配记录",
            filteredEmptyDescription: "请调整时间范围或状态筛选，重新查看对应执行明细。",
            resetFilters: "恢复默认筛选",
            details: "执行明细",
            accumulatedFees: "累计费用",
            columns: {
              time: "时间",
              instrument: "标的",
              side: "方向",
              quantity: "数量",
              price: "成交价",
              amount: "金额",
              fee: "费用",
              mode: "模式",
              status: "状态",
            },
            orderType: "订单类型",
            reduceOnly: "仅减仓",
            liveQuote: "实时行情",
            quoteSource: "行情来源",
            quoteAsOf: "行情时间",
            quoteLag: "新鲜度",
            quoteMissing: "暂无行情快照",
            quoteStale: "已过期",
            quotePriceGap: "相对成交价",
            notExecuted: "未执行",
            notRecorded: "未记录",
            statusLabels: {
              filled: "已成交",
              partial: "部分成交",
              pending: "待执行",
              cancelled: "已取消",
              canceled: "已取消",
              submitted: "已提交",
              rejected: "已拒绝",
              failed: "已拒绝",
            },
            sideLabels: {
              buy: "买入",
              sell: "卖出",
            },
            tradingModes: {
              live: "实盘",
              paper: "纸面",
              simulation: "模拟",
            },
            orderTypes: {
              market: "市价单",
              limit: "限价单",
              stop: "止损单",
              stop_limit: "止损限价单",
            },
            positionSides: {
              long: "多头",
              short: "空头",
            },
            openClose: {
              open: "开仓",
              close: "平仓",
              close_today: "平今",
              roll: "移仓",
            },
            unknownStatus: "未知状态",
            spotLong: "现货 / 多头",
            unset: "未设置",
          },
    [language],
  );

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
      const sorted = (response ?? []).slice().sort((a, b) => {
        const left = a.executedAt ?? a.createdAt;
        const right = b.executedAt ?? b.createdAt;
        return right.localeCompare(left);
      });
      setTrades(sorted);

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
                  return (
                    <tr key={trade.id} className="border-t border-gray-100 align-top">
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
                        <span className={`inline-flex rounded-full border px-2.5 py-1 text-xs font-medium ${status.badge}`}>{status.label}</span>
                      </td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
          </div>
        </div>
      )}
    </div>
  );
};

export default TradeHistory;
