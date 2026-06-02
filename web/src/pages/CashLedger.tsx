// CashLedger page (P1-1).
//
// Per-fund cash journal with three panes:
//   1. summary cards — total cash + per-entry-type subtotals
//   2. filter bar — date range + entry-type multiselect
//   3. table — paginated entries with cursor-based "load more"
//
// We deliberately don't model SUM rolling-up by trading-date —
// the daily snapshot lives in the existing NavSnapshot screen.
// This page is for forensic drill-down: "where did $123.45 come
// from on 2026-04-12?"

import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import {
  ApiError,
  formatApiError,
  listCashLedger,
  type CashLedgerEntry,
  type CashLedgerListResponse,
} from "../lib/api";
import { useAppPreferences, formatNumberForLanguage } from "../lib/preferences";

type Language = "zh-CN" | "en-US";

// Display group: collapses related leaves (notional + 3 fee types)
// into one display label so the dropdown isn't 16 entries.
type DisplayGroup =
  | "all"
  | "trades"
  | "fees"
  | "dividend"
  | "funding"
  | "adjustment";

// Map display group → list of underlying entry_types.
const groupTypes: Record<DisplayGroup, string[]> = {
  all: [],
  trades: [
    "trade_buy_notional",
    "trade_buy_commission",
    "trade_buy_transfer_fee",
    "trade_buy_stamp_tax",
    "trade_sell_notional",
    "trade_sell_commission",
    "trade_sell_transfer_fee",
    "trade_sell_stamp_tax",
  ],
  fees: ["fee_management", "fee_performance", "fee_platform"],
  dividend: ["dividend_cash"],
  funding: ["funding_deposit", "funding_withdrawal"],
  adjustment: ["adjustment", "reversal"],
};

const messages: Record<Language, {
  title: string;
  subtitle: string;
  filtersTitle: string;
  filterFrom: string;
  filterTo: string;
  filterGroup: string;
  filterApply: string;
  filterReset: string;
  groupLabels: Record<DisplayGroup, string>;
  summaryBalance: string;
  summaryNotional: string;
  summaryCommissions: string;
  summaryDividend: string;
  summaryFees: string;
  empty: string;
  loadMore: string;
  loading: string;
  loadingMore: string;
  errorPrefix: string;
  columns: {
    posted: string;
    type: string;
    amount: string;
    description: string;
    links: string;
  };
  typeLabels: Record<string, string>;
}> = {
  "zh-CN": {
    title: "资金流水",
    subtitle:
      "按时间倒序展示该基金的全部现金变动：成交本金 / 手续费 / 分红 / 出入金 / 调整。汇总值与基金账户余额应一致；如有偏差请联系运营校对。",
    filtersTitle: "筛选",
    filterFrom: "起始时间",
    filterTo: "截止时间",
    filterGroup: "类别",
    filterApply: "应用",
    filterReset: "重置",
    groupLabels: {
      all: "全部",
      trades: "交易",
      fees: "费用",
      dividend: "分红",
      funding: "出入金",
      adjustment: "调整",
    },
    summaryBalance: "区间净流入",
    summaryNotional: "成交本金净额",
    summaryCommissions: "手续费合计",
    summaryDividend: "分红入账",
    summaryFees: "管理/平台费",
    empty: "区间内没有现金记录",
    loadMore: "加载更多",
    loading: "加载中…",
    loadingMore: "加载中…",
    errorPrefix: "加载失败：",
    columns: {
      posted: "时间",
      type: "类别",
      amount: "金额",
      description: "说明",
      links: "关联",
    },
    typeLabels: {
      trade_buy_notional: "买入本金",
      trade_buy_commission: "买入佣金",
      trade_buy_transfer_fee: "买入过户费",
      trade_buy_stamp_tax: "买入印花税",
      trade_sell_notional: "卖出本金",
      trade_sell_commission: "卖出佣金",
      trade_sell_transfer_fee: "卖出过户费",
      trade_sell_stamp_tax: "卖出印花税",
      dividend_cash: "现金分红",
      fee_management: "管理费",
      fee_performance: "业绩费",
      fee_platform: "平台费",
      funding_deposit: "入金",
      funding_withdrawal: "出金",
      adjustment: "手工调整",
      reversal: "冲销",
    },
  },
  "en-US": {
    title: "Cash movements",
    subtitle:
      "Newest-first journal of every cash movement on this fund: trade legs, commissions, dividends, fees, deposits/withdrawals, and adjustments. Subtotals should reconcile with the fund balance; flag drift to ops.",
    filtersTitle: "Filters",
    filterFrom: "From",
    filterTo: "To",
    filterGroup: "Category",
    filterApply: "Apply",
    filterReset: "Reset",
    groupLabels: {
      all: "All",
      trades: "Trades",
      fees: "Fees",
      dividend: "Dividends",
      funding: "Deposits / withdrawals",
      adjustment: "Adjustments",
    },
    summaryBalance: "Net flow in window",
    summaryNotional: "Net trade notional",
    summaryCommissions: "Commissions paid",
    summaryDividend: "Dividends received",
    summaryFees: "Management / platform fees",
    empty: "No cash movements in this window",
    loadMore: "Load more",
    loading: "Loading…",
    loadingMore: "Loading more…",
    errorPrefix: "Load failed: ",
    columns: {
      posted: "Posted",
      type: "Category",
      amount: "Amount",
      description: "Description",
      links: "Links",
    },
    typeLabels: {
      trade_buy_notional: "Buy notional",
      trade_buy_commission: "Buy commission",
      trade_buy_transfer_fee: "Buy transfer fee",
      trade_buy_stamp_tax: "Buy stamp tax",
      trade_sell_notional: "Sell notional",
      trade_sell_commission: "Sell commission",
      trade_sell_transfer_fee: "Sell transfer fee",
      trade_sell_stamp_tax: "Sell stamp tax",
      dividend_cash: "Cash dividend",
      fee_management: "Management fee",
      fee_performance: "Performance fee",
      fee_platform: "Platform fee",
      funding_deposit: "Deposit",
      funding_withdrawal: "Withdrawal",
      adjustment: "Adjustment",
      reversal: "Reversal",
    },
  },
};

export default function CashLedger(): JSX.Element {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const m = messages[language];

  const [from, setFrom] = useState<string>("");
  const [to, setTo] = useState<string>("");
  const [group, setGroup] = useState<DisplayGroup>("all");
  const [appliedFrom, setAppliedFrom] = useState<string>("");
  const [appliedTo, setAppliedTo] = useState<string>("");
  const [appliedGroup, setAppliedGroup] = useState<DisplayGroup>("all");

  const [entries, setEntries] = useState<CashLedgerEntry[]>([]);
  const [subtotals, setSubtotals] = useState<Record<string, number>>({});
  const [balance, setBalance] = useState<number | null>(null);
  const [cursor, setCursor] = useState<string | undefined>(undefined);
  const [loading, setLoading] = useState(false);
  const [loadingMore, setLoadingMore] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const buildOpts = useCallback(
    (override: { cursor?: string; reset: boolean }) => ({
      from: appliedFrom ? new Date(appliedFrom).toISOString() : undefined,
      to: appliedTo ? new Date(appliedTo).toISOString() : undefined,
      types: groupTypes[appliedGroup].length > 0 ? groupTypes[appliedGroup] : undefined,
      limit: 50,
      cursor: override.cursor,
      summary: override.reset, // only on first page
      balance: override.reset,
    }),
    [appliedFrom, appliedTo, appliedGroup],
  );

  const reload = useCallback(async () => {
    if (!fundId) return;
    setLoading(true);
    setError(null);
    try {
      const resp: CashLedgerListResponse = await listCashLedger(fundId, buildOpts({ reset: true }));
      setEntries(resp.entries);
      setSubtotals(resp.subtotals ?? {});
      setBalance(resp.balance ?? null);
      setCursor(resp.next_cursor);
    } catch (err) {
      if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
        setEntries([]);
        setSubtotals({});
        setBalance(null);
        setCursor(undefined);
        return;
      }
      setError(formatApiError(err, m.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [fundId, buildOpts, m.errorPrefix]);

  const loadMore = useCallback(async () => {
    if (!fundId || !cursor) return;
    setLoadingMore(true);
    setError(null);
    try {
      const resp = await listCashLedger(fundId, buildOpts({ cursor, reset: false }));
      setEntries((prev) => [...prev, ...resp.entries]);
      setCursor(resp.next_cursor);
    } catch (err) {
      setError(formatApiError(err, m.errorPrefix));
    } finally {
      setLoadingMore(false);
    }
  }, [fundId, cursor, buildOpts, m.errorPrefix]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const onApplyFilters = () => {
    setAppliedFrom(from);
    setAppliedTo(to);
    setAppliedGroup(group);
  };

  const onResetFilters = () => {
    setFrom("");
    setTo("");
    setGroup("all");
    setAppliedFrom("");
    setAppliedTo("");
    setAppliedGroup("all");
  };

  // Derived subtotals for the cards. We sum the relevant types
  // from the response's subtotals map; this avoids a second
  // round-trip per card and keeps the math explicit.
  const summary = useMemo(() => {
    const sumOf = (types: string[]) =>
      types.reduce((acc, t) => acc + (subtotals[t] ?? 0), 0);
    return {
      notional: sumOf([
        "trade_buy_notional",
        "trade_sell_notional",
      ]),
      commissions: sumOf([
        "trade_buy_commission",
        "trade_sell_commission",
        "trade_buy_transfer_fee",
        "trade_sell_transfer_fee",
        "trade_buy_stamp_tax",
        "trade_sell_stamp_tax",
      ]),
      dividend: sumOf(["dividend_cash"]),
      fees: sumOf(["fee_management", "fee_performance", "fee_platform"]),
    };
  }, [subtotals]);

  return (
    <div className="space-y-5">
      <header className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h1 className="text-2xl font-bold text-gray-900">{m.title}</h1>
        <p className="mt-2 text-sm text-gray-500">{m.subtitle}</p>
      </header>

      {/* Summary cards */}
      <section className="grid grid-cols-2 gap-4 xl:grid-cols-5">
        <SummaryCard label={m.summaryBalance} value={balance} language={language} highlight />
        <SummaryCard label={m.summaryNotional} value={summary.notional} language={language} />
        <SummaryCard label={m.summaryCommissions} value={summary.commissions} language={language} />
        <SummaryCard label={m.summaryDividend} value={summary.dividend} language={language} />
        <SummaryCard label={m.summaryFees} value={summary.fees} language={language} />
      </section>

      {/* Filters */}
      <section className="rounded-2xl border border-gray-200 bg-white p-5 shadow-sm">
        <h2 className="text-sm font-semibold text-gray-800">{m.filtersTitle}</h2>
        <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-4">
          <label className="text-sm text-gray-600">
            <span className="mb-1 block">{m.filterFrom}</span>
            <input
              type="datetime-local"
              value={from}
              onChange={(e) => setFrom(e.target.value)}
              className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
            />
          </label>
          <label className="text-sm text-gray-600">
            <span className="mb-1 block">{m.filterTo}</span>
            <input
              type="datetime-local"
              value={to}
              onChange={(e) => setTo(e.target.value)}
              className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
            />
          </label>
          <label className="text-sm text-gray-600">
            <span className="mb-1 block">{m.filterGroup}</span>
            <select
              value={group}
              onChange={(e) => setGroup(e.target.value as DisplayGroup)}
              className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
            >
              {(Object.keys(m.groupLabels) as DisplayGroup[]).map((g) => (
                <option key={g} value={g}>
                  {m.groupLabels[g]}
                </option>
              ))}
            </select>
          </label>
          <div className="flex items-end gap-2">
            <button
              type="button"
              onClick={onApplyFilters}
              className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700"
            >
              {m.filterApply}
            </button>
            <button
              type="button"
              onClick={onResetFilters}
              className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50"
            >
              {m.filterReset}
            </button>
          </div>
        </div>
      </section>

      {error ? (
        <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{error}</div>
      ) : null}

      {/* Table */}
      <section className="rounded-2xl border border-gray-200 bg-white shadow-sm">
        {loading ? (
          <div className="px-6 py-8 text-sm text-gray-500">{m.loading}</div>
        ) : entries.length === 0 ? (
          <div className="px-6 py-8 text-sm text-gray-500">{m.empty}</div>
        ) : (
          <div className="overflow-x-auto">
            <table className="min-w-full divide-y divide-gray-200">
              <thead className="bg-gray-50 text-left text-xs font-medium uppercase tracking-wide text-gray-500">
                <tr>
                  <th className="px-4 py-3">{m.columns.posted}</th>
                  <th className="px-4 py-3">{m.columns.type}</th>
                  <th className="px-4 py-3 text-right">{m.columns.amount}</th>
                  <th className="px-4 py-3">{m.columns.description}</th>
                  <th className="px-4 py-3">{m.columns.links}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-gray-100 text-sm text-gray-700">
                {entries.map((e) => (
                  <tr key={e.id}>
                    <td className="whitespace-nowrap px-4 py-2">{formatTimestamp(e.posted_at)}</td>
                    <td className="px-4 py-2">{m.typeLabels[e.entry_type] ?? e.entry_type}</td>
                    <td
                      className={`whitespace-nowrap px-4 py-2 text-right font-mono ${
                        e.amount < 0 ? "text-red-600" : "text-emerald-700"
                      }`}
                    >
                      {formatSignedAmount(e.amount, e.currency, language)}
                    </td>
                    <td className="px-4 py-2 text-gray-600">{e.description ?? ""}</td>
                    <td className="px-4 py-2 text-xs text-gray-500">
                      {[
                        e.trade_id && `trade:${shortenLinkId(e.trade_id)}`,
                        e.corp_action_id && `corp:${shortenLinkId(e.corp_action_id)}`,
                      ]
                        .filter(Boolean)
                        .join(" · ")}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
        {cursor ? (
          <div className="border-t border-gray-100 px-4 py-3 text-center">
            <button
              type="button"
              onClick={() => void loadMore()}
              disabled={loadingMore}
              className="rounded-lg border border-gray-300 bg-white px-4 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {loadingMore ? m.loadingMore : m.loadMore}
            </button>
          </div>
        ) : null}
      </section>
    </div>
  );
}

function SummaryCard({
  label,
  value,
  language,
  highlight,
}: {
  label: string;
  value: number | null;
  language: Language;
  highlight?: boolean;
}) {
  const formatted =
    value == null ? "—" : formatNumberForLanguage(value, language, { maximumFractionDigits: 2 });
  const tone =
    value == null
      ? "text-gray-700"
      : value < 0
        ? "text-red-600"
        : value > 0
          ? "text-emerald-700"
          : "text-gray-700";
  return (
    <div
      className={`rounded-2xl border bg-white px-4 py-4 shadow-sm ${
        highlight ? "border-indigo-200" : "border-gray-200"
      }`}
    >
      <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{label}</p>
      <p className={`mt-2 text-2xl font-semibold ${tone}`}>{formatted}</p>
    </div>
  );
}

function formatTimestamp(iso: string): string {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

function formatSignedAmount(value: number, currency: string, language: Language): string {
  // Always render with the leading sign so debits and credits are
  // unambiguous on a quick scan. Locale formatting still applies
  // to thousands separators and decimal point.
  const sign = value < 0 ? "-" : value > 0 ? "+" : "";
  const abs = Math.abs(value);
  const formatted = formatNumberForLanguage(abs, language, { maximumFractionDigits: 4 });
  return `${sign}${formatted} ${currency}`;
}

function shortenLinkId(id: string): string {
  if (!id || id.length < 8) return id;
  return id.slice(0, 8);
}
