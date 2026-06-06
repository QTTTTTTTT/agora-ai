import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useTranslation } from "react-i18next";
import i18n from "../i18n";
import dashboardEnFallback from "../i18n/locales/en-US/dashboard";
import {
  CartesianGrid,
  Legend,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts";
import {
  apiGet,
  apiPost,
  buildPortfolioQuotesStreamUrl,
  fetchFundLLMUsage,
  fetchFundMarketNewsDigest,
  fetchFundMarketQuotes,
  fetchFundPnLAttribution,
  fetchFundTodayPnL,
  type FundTodayPnL,
  formatApiError,
  type LLMUsageVisibility,
  type MarketNewsDigest,
  type MarketQuote,
  type PnLAttribution,
  type PortfolioQuote,
  type PortfolioQuotesFrame,
} from "../lib/api";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatDateValueForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";
import { CorpActionTimeline } from "../components/CorpActionTimeline";
import { BenchmarkChart } from "../components/BenchmarkChart";
import { HoldingsTrendsGrid } from "../components/HoldingsTrendsGrid";
import { SkeletonCard } from "../components/Skeleton";
import { useSWRFetch } from "../lib/useSWRFetch";

interface FundUniverse {
  mode: string;
  symbols: string[];
  sectors?: string[];
  themes?: string[];
  customFilters?: string[];
}

interface Fund {
  id: string;
  companyId: string;
  name: string;
  description?: string;
  tradingMode: string;
  initialCapital: number;
  currentCapital: number;
  totalAssets: number;
  nav: number;
  status: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  baseCurrency?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  universe?: FundUniverse;
}

interface NavPoint {
  date: string;
  nav: number;
  totalAssets?: number;
  availableCash?: number;
}

interface Position {
  instrumentKey?: string;
  symbol: string;
  name?: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  instrumentType?: string;
  positionSide?: string;
  quoteCurrency?: string;
  settlementCurrency?: string;
  marginMode?: string;
  quantity: number;
  availableQty?: number;
  costPrice: number;
  currentPrice: number;
  marketValue?: number;
  weight?: number;
  leverage?: number;
  contractMultiplier?: number;
  expiryDate?: string;
  unrealizedPnl?: number;
  marginUsed?: number;
  // Live-overlay metadata added by PR-2. priceAsOf is an RFC3339 string
  // (UTC) sampled by the backend; priceSource names the upstream provider
  // ("yahoo", "eastmoney", ...); isStale signals the value is older than
  // the backend's StaleQuoteAfter threshold (default 15 minutes).
  priceAsOf?: string;
  priceSource?: string;
  isStale?: boolean;
}

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
  side: string;
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
  status: string;
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

interface WorkflowStatus {
  fundId: string;
  tradingDate?: string;
  state: string;
  step: string;
  startedAt?: string;
  completedAt?: string;
  runningForMs?: number;
  progressPercent?: number;
  completedSteps?: number;
  failedSteps?: number;
  totalSteps?: number;
  steps?: WorkflowStepStatus[];
}

interface WorkflowStepStatus {
  step?: string;
  label?: string;
  order?: number;
  status?: string;
  startedAt?: string;
  endedAt?: string;
  updatedAt?: string;
  durationMs?: number;
  error?: string;
}

interface FundSummary {
  name: string;
  tradingMode: string;
  totalAssets: number;
  availableCash: number;
  todayPnL: number;
  totalReturn: number;
  baseCurrency: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  universeSummary?: string;
  universeThemesSummary?: string;
  universeSectorsSummary?: string;
}

interface PositionView extends Position {
  name: string;
  costPrice: number;
  currentPrice: number;
  pnl: number;
  pnlPercent: number;
  weight: number;
  marketValue: number;
  displayCurrency: string;
  priceCurrency: string;
}

interface TradeView {
  id: string;
  time: string;
  symbol: string;
  instrumentKey?: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  positionSide?: string;
  openClose?: string;
  leverage?: number;
  expiryDate?: string;
  action: "BUY" | "SELL";
  quantity: number;
  price: number;
  amount: number;
  status: "filled" | "pending" | "rejected";
  priceCurrency: string;
  amountCurrency: string;
}

interface DashboardResponse {
  fund: Fund;
  navHistory: NavPoint[];
  positions: Position[];
  trades: Trade[];
  workflow: WorkflowStatus;
}

interface DashboardData {
  fund: FundSummary;
  navHistory: NavPoint[];
  positions: PositionView[];
  recentTrades: TradeView[];
  workflow: WorkflowStatus;
  latestQuotes: MarketQuote[];
  newsDigest: MarketNewsDigest | null;
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

function formatDurationMs(value?: number): string {
  if (!value || !Number.isFinite(value) || value <= 0) {
    return "-";
  }
  const totalSeconds = Math.round(value / 1000);
  const minutes = Math.floor(totalSeconds / 60);
  const seconds = totalSeconds % 60;
  if (minutes <= 0) {
    return `${seconds}s`;
  }
  return `${minutes}m ${seconds}s`;
}

function workflowStepStatusClass(status?: string): string {
  switch ((status ?? "").toLowerCase()) {
    case "success":
      return "border-emerald-200 bg-emerald-50 text-emerald-700";
    case "running":
      return "border-blue-200 bg-blue-50 text-blue-700";
    case "failed":
    case "error":
      return "border-red-200 bg-red-50 text-red-700";
    case "skipped":
      return "border-gray-200 bg-gray-50 text-gray-500";
    default:
      return "border-gray-200 bg-white text-gray-500";
  }
}

function pickLocalizedText(language: string, value?: string, valueZh?: string, valueEn?: string): string {
  const base = value?.trim() ?? "";
  const zh = valueZh?.trim() ?? "";
  const en = valueEn?.trim() ?? "";
  return language === "zh-CN" ? zh || base || en : en || base || zh;
}

function instrumentMeta(...values: Array<string | undefined>): string[] {
  return values.filter((value): value is string => Boolean(value?.trim()));
}

function positionMultiplier(position: Position): number {
  return position.contractMultiplier && position.contractMultiplier > 0 ? position.contractMultiplier : 1;
}

// PriceFreshnessTranslator is the minimal `t` surface the freshness
// helpers depend on. We accept a structural type instead of importing
// react-i18next's TFunction directly so unit-test callers can pass a
// hand-rolled stub without dragging in i18next typings.
type PriceFreshnessTranslator = (
  key: string,
  vars?: Record<string, unknown>,
) => string;

// formatPriceAge produces a human-readable "5s ago / 12m ago / 3h ago"
// string from an ISO timestamp. Returns undefined when the input is
// missing or unparseable so callers can render a neutral "—".
//
// W7-2 — translations now live in the dashboard i18n namespace as
// interpolation templates (`{{n}}s ago` etc.); we accept a `t`
// function rather than an inline object so the bundle stays purely
// JSON-serializable for the missing-key parity guard.
function formatPriceAge(
  priceAsOf: string | undefined,
  now: Date,
  t: PriceFreshnessTranslator,
): string | undefined {
  if (!priceAsOf) return undefined;
  const parsed = Date.parse(priceAsOf);
  if (Number.isNaN(parsed)) return undefined;
  const diffMs = now.getTime() - parsed;
  if (diffMs < 0) return t("priceFreshness.justNow");
  const seconds = Math.floor(diffMs / 1000);
  if (seconds < 5) return t("priceFreshness.justNow");
  if (seconds < 60) return t("priceFreshness.secondsAgo", { n: seconds });
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return t("priceFreshness.minutesAgo", { n: minutes });
  const hours = Math.floor(minutes / 60);
  if (hours < 48) return t("priceFreshness.hoursAgo", { n: hours });
  return t("priceFreshness.daysAgo", { n: Math.floor(hours / 24) });
}

// PriceFreshnessBadge renders the "live / X minutes ago / stale" indicator
// next to a persisted price. It self-ticks every 15s so a long-open
// dashboard still shows accurate ages without forcing a full re-render of
// the surrounding table.
function PriceFreshnessBadge({
  priceAsOf,
  isStale,
  t,
}: {
  priceAsOf?: string;
  isStale?: boolean;
  t: PriceFreshnessTranslator;
}): JSX.Element | null {
  const [now, setNow] = useState<Date>(() => new Date());
  useEffect(() => {
    const handle = window.setInterval(() => setNow(new Date()), 15_000);
    return () => window.clearInterval(handle);
  }, []);
  if (!priceAsOf && !isStale) {
    return null;
  }
  const age = formatPriceAge(priceAsOf, now, t);
  const justNow = t("priceFreshness.justNow");
  const label = isStale ? t("priceFreshness.stale") : age ?? t("priceFreshness.live");
  const tone = isStale
    ? "bg-amber-100 text-amber-800"
    : age === undefined || age === justNow
      ? "bg-emerald-100 text-emerald-700"
      : "bg-gray-100 text-gray-600";
  return <span className={`inline-flex rounded-full px-2 py-0.5 text-[10px] font-medium uppercase tracking-wide ${tone}`}>{label}</span>;
}

function positionMarketValue(position: Position): number {
  if (typeof position.marketValue === "number" && Number.isFinite(position.marketValue)) {
    return position.marketValue;
  }
  return position.quantity * position.currentPrice * positionMultiplier(position);
}

function positionPnL(position: Position): number {
  if (typeof position.unrealizedPnl === "number" && Number.isFinite(position.unrealizedPnl)) {
    return position.unrealizedPnl;
  }
  const multiplier = positionMultiplier(position);
  const direction = (position.positionSide ?? "").toLowerCase();
  const delta = direction === "short" ? position.costPrice - position.currentPrice : position.currentPrice - position.costPrice;
  return delta * position.quantity * multiplier;
}

function normalizeTradeStatus(status: string): TradeView["status"] {
  const normalized = status.toLowerCase();
  if (normalized === "filled" || normalized === "pending") {
    return normalized;
  }
  return "rejected";
}

function buildDashboardData(
  fund: Fund,
  navHistory: NavPoint[],
  positions: Position[],
  trades: Trade[],
  workflow: WorkflowStatus,
  latestQuotes: MarketQuote[],
  newsDigest: MarketNewsDigest | null,
): DashboardData {
  // "Today" in the user's local calendar — matches how the rest of
  // the app derives a trading-date label (no TZ shenanigans because
  // nav.date is already a YYYY-MM-DD string from the server).
  const todayISO = (() => {
    const d = new Date();
    const y = d.getFullYear();
    const m = String(d.getMonth() + 1).padStart(2, "0");
    const day = String(d.getDate()).padStart(2, "0");
    return `${y}-${m}-${day}`;
  })();
  // "Yesterday's close" baseline: scan the NAV history backwards
  // and take the first row whose trading_date is strictly before
  // today. This avoids subtracting an intra-day NAV snapshot that
  // was written at PM-plan time (very common on funds with auto
  // execute enabled) and gives a stable "today vs yesterday's
  // close" delta.
  let priorClose: NavPoint | undefined;
  for (let i = navHistory.length - 1; i >= 0; i--) {
    const row = navHistory[i];
    if (row.date.slice(0, 10) < todayISO) {
      priorClose = row;
      break;
    }
  }
  const totalPositionValue = positions.reduce((sum, position) => sum + positionMarketValue(position), 0);
  // Live cash + live market value. fund.currentCapital is updated
  // synchronously by every fill (the trading engine writes it in
  // the same UoW that creates the trade_execution row), so it's a
  // safer "available cash right now" source than the NAV snapshot
  // which is only refreshed by Settle. Same logic for market value:
  // holding_positions.market_value reflects the most recent quote
  // refresh, whereas nav_snapshots.total_market_value is frozen to
  // the snapshot moment.
  const availableCash = fund.currentCapital;
  const totalAssets = availableCash + totalPositionValue;
  const previousAssets = priorClose?.totalAssets ?? fund.initialCapital;
  // Today P&L = (live cash + live market value) − previous close.
  // Equivalent to Σ ((live_price - prior_close_price) × qty) + realised
  // P&L since prior close, which is exactly the "今日盈亏" definition
  // the user expects.
  const todayPnL = totalAssets - previousAssets;
  const totalReturn = fund.initialCapital > 0 ? ((totalAssets - fund.initialCapital) / fund.initialCapital) * 100 : 0;
  const baseCurrency = fund.baseCurrency?.trim() || "USD";
  const positionViews = positions.map((position) => {
    const multiplier = positionMultiplier(position);
    const marketValue = positionMarketValue(position);
    const pnl = positionPnL(position);
    const costBasis = position.costPrice * position.quantity * multiplier;
    const pnlPercent = costBasis > 0 ? (pnl / costBasis) * 100 : 0;
    return {
      ...position,
      name: position.name || position.instrumentKey || position.symbol,
      pnl,
      pnlPercent,
      marketValue,
      weight:
        typeof position.weight === "number" && Number.isFinite(position.weight)
          ? position.weight
          : totalPositionValue > 0
            ? (marketValue / totalPositionValue) * 100
            : 0,
      displayCurrency: position.settlementCurrency || position.quoteCurrency || baseCurrency,
      priceCurrency: position.quoteCurrency || baseCurrency,
    };
  });

  const tradeViews: TradeView[] = trades.slice(0, 10).map((trade) => ({
    id: trade.id,
    time: trade.executedAt ?? trade.createdAt,
    symbol: trade.symbol,
    instrumentKey: trade.instrumentKey,
    market: trade.market,
    exchange: trade.exchange,
    assetClass: trade.assetClass,
    positionSide: trade.positionSide,
    openClose: trade.openClose,
    leverage: trade.leverage,
    expiryDate: trade.expiryDate,
    action: trade.side.toUpperCase() === "SELL" ? "SELL" : "BUY",
    quantity: trade.filledQty || trade.quantity,
    price: trade.filledPrice || trade.price,
    amount: trade.amount || (trade.filledQty || trade.quantity) * (trade.filledPrice || trade.price),
    status: normalizeTradeStatus(trade.status),
    priceCurrency: trade.quoteCurrency || baseCurrency,
    amountCurrency: trade.settlementCurrency || trade.quoteCurrency || baseCurrency,
  }));

  return {
    fund: {
      name: fund.name,
      tradingMode: fund.tradingMode,
      totalAssets,
      availableCash,
      todayPnL,
      totalReturn,
      baseCurrency,
      market: fund.market,
      exchange: fund.exchange,
      assetClass: fund.assetClass,
      benchmarkSymbol: fund.benchmarkSymbol,
      primaryDirection: fund.primaryDirection,
      universeSummary: fund.universe?.symbols?.length ? fund.universe.symbols.join(", ") : undefined,
      universeThemesSummary: fund.universe?.themes?.length ? fund.universe.themes.join(", ") : undefined,
      universeSectorsSummary: fund.universe?.sectors?.length ? fund.universe.sectors.join(", ") : undefined,
    },
    navHistory,
    positions: positionViews,
    recentTrades: tradeViews,
    workflow,
    latestQuotes,
    newsDigest,
  };
}

// dashboardCopy mirrors the previously-inline `copy` block one-for-one
// minus the function-valued priceFreshness fields (which W7-2 promoted
// to interpolation templates, called via `t()` at the consumer). The
// shape is exposed as a const-typed alias so the existing call sites
// (`copy.metrics.totalAssets`, `copy.workflowSteps[step]`, etc.) keep
// working without per-key churn.
type DashboardCopy = Omit<
  typeof import("../i18n/locales/en-US/dashboard").default,
  "priceFreshness" | "metrics"
> & {
  priceFreshness: {
    live: string;
    stale: string;
    unknown: string;
    justNow: string;
  };
  metrics: {
    totalAssets: string;
    availableCash: string;
    todayPnL: string;
    totalReturn: string;
  };
};

export default function Dashboard() {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const { t } = useTranslation("dashboard");
  const [data, setData] = useState<DashboardData | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [starting, setStarting] = useState(false);
  const [marketSymbols, setMarketSymbols] = useState<string[]>([]);
  const [quotesLoading, setQuotesLoading] = useState(false);
  const [newsLoading, setNewsLoading] = useState(false);
  const [newsError, setNewsError] = useState<string | null>(null);
  const [newsRequested, setNewsRequested] = useState(false);
  // LLM-usage tile uses SWR caching so flipping between Dashboard
  // sub-tabs / sibling pages doesn't trigger a fresh
  // /api/funds/{id}/llm/usage hit on every mount. ttl=120s; the
  // tile shows aggregated cost, which doesn't tick fast enough to
  // need real-time refresh. We gate on `data !== null` rather than
  // the later-declared `hasCoreData` const to avoid a temporal
  // dead-zone reference (this hook fires before `hasCoreData` is
  // computed in the body).
  const llmUsageSwr = useSWRFetch<LLMUsageVisibility | null>(
    fundId && data !== null ? `dashboard/llmUsage/${fundId}` : null,
    () => fetchFundLLMUsage(fundId!).catch(() => null),
    { ttlMs: 120_000 },
  );
  const llmUsage = llmUsageSwr.data ?? null;
  const llmUsageLoading = !llmUsageSwr.data && llmUsageSwr.isLoading;
  const [pnlAttribution, setPnlAttribution] = useState<PnLAttribution | null>(null);
  // Server-computed "今日盈亏" (realised today + (current unrealised −
  // prior-close unrealised)). Falls back to client-side NAV-diff
  // inside the KPI tile when this hasn't loaded yet — that keeps
  // first-paint snappy without showing a wrong number.
  const [todayPnL, setTodayPnL] = useState<FundTodayPnL | null>(null);
  const [pnlAttributionLoading, setPnlAttributionLoading] = useState(false);

  // W7-2 — translations now come from the `dashboard` i18n namespace.
  // We pull the entire bundle through `getResourceBundle` and reshape
  // it into the legacy `copy.X` accessor pattern so the existing JSX
  // doesn't have to migrate every reference to a `t(...)` call. Any
  // future page using this pattern only has to flip the namespace name.
  const copy = useMemo<DashboardCopy>(() => {
    const bundle = i18n.getResourceBundle(language, "dashboard") as
      | typeof dashboardEnFallback
      | undefined;
    // Fallback to the statically-imported en-US bundle: should never
    // hit at runtime because i18n bootstrap pre-loads both locales,
    // but it preserves the legacy "default to English on lookup miss"
    // behaviour the old code path had.
    const source = bundle ?? dashboardEnFallback;
    return {
      ...source,
      // Strip the interpolation-template fields so the consumer can't
      // accidentally render `"{{n}}s ago"` literally — the compiler
      // pins the cleaned-up shape via DashboardCopy.
      priceFreshness: {
        live: source.priceFreshness.live,
        stale: source.priceFreshness.stale,
        unknown: source.priceFreshness.unknown,
        justNow: source.priceFreshness.justNow,
      },
      metrics: {
        totalAssets: source.metrics.totalAssets,
        availableCash: source.metrics.availableCash,
        todayPnL: source.metrics.todayPnL,
        totalReturn: source.metrics.totalReturn,
      },
    };
  }, [language]);


  const tradingModeLabel = useCallback(
    (value: string) => copy.tradingModes[value.toLowerCase() as keyof typeof copy.tradingModes] ?? humanizeValue(value, copy.unset),
    [copy],
  );
  const directionLabel = useCallback(
    (value?: string) => copy.directions[(value ?? "").toLowerCase() as keyof typeof copy.directions] ?? humanizeValue(value, copy.unset),
    [copy],
  );
  const positionSideLabel = useCallback(
    (value?: string) => {
      const k = (value ?? "").toLowerCase();
      const bundle = i18n.getResourceBundle(language, "dashboard") as
        | typeof dashboardEnFallback
        | undefined;
      const map = (bundle ?? dashboardEnFallback).positionSide as Record<string, string>;
      return map[k] ?? humanizeValue(value, copy.spotLong);
    },
    [copy.spotLong, language],
  );
  const openCloseLabel = useCallback(
    (value?: string) => {
      const k = (value ?? "").toLowerCase();
      const bundle = i18n.getResourceBundle(language, "dashboard") as
        | typeof dashboardEnFallback
        | undefined;
      const map = (bundle ?? dashboardEnFallback).openClose as Record<string, string>;
      return map[k] ?? humanizeValue(value, copy.unset);
    },
    [copy.unset, language],
  );
  const workflowStateLabel = useCallback(
    (value?: string) => copy.workflowStates[(value ?? "idle").toLowerCase() as keyof typeof copy.workflowStates] ?? copy.workflowStates.idle,
    [copy],
  );
  const workflowStepLabel = useCallback(
    (value?: string) => copy.workflowSteps[(value ?? "not_started").toLowerCase() as keyof typeof copy.workflowSteps] ?? humanizeValue(value, copy.workflowSteps.not_started),
    [copy],
  );
  const tradeStatusLabel = useCallback(
    (value: TradeView["status"]) => copy.tradeStatuses[value],
    [copy.tradeStatuses],
  );
  const tradeActionLabel = useCallback(
    (value: TradeView["action"]) => copy.tradeActions[value],
    [copy.tradeActions],
  );
  const formatPercent = useCallback(
    (value: number, digits = 2) => `${value >= 0 ? "+" : ""}${formatNumberForLanguage(value, language, { minimumFractionDigits: digits, maximumFractionDigits: digits })}%`,
    [language],
  );
  const formatQuantity = useCallback(
    (value: number) => formatNumberForLanguage(value, language, { maximumFractionDigits: 4 }),
    [language],
  );
  const chartDateLabel = useCallback(
    (value: string) =>
      formatDateValueForLanguage(value, language, language === "en-US" ? { month: "short", day: "numeric" } : { month: "2-digit", day: "2-digit" }),
    [language],
  );

  const hasCoreData = data !== null;

  const fetchData = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setLoading(true);
    setError(null);
    setNewsError(null);
    setNewsRequested(false);
    // llmUsage is now SWR-owned; the cache invalidates itself
    // through TTL so we don't need to clear it here on a manual
    // dashboard refresh.
    setPnlAttribution(null);
    setTodayPnL(null);

    try {
      // Fire dashboard + today-pnl in parallel so the KPI tile and
      // the equity curve render together without an extra round-trip
      // latency.
      const [response, todayPnLResp] = await Promise.all([
        apiGet<DashboardResponse>(`/api/funds/${fundId}/dashboard`),
        fetchFundTodayPnL(fundId).catch((err) => {
          // today-pnl is non-critical: a 503 or transient error here
          // shouldn't blank the entire dashboard. Log and continue.
          console.warn("today-pnl fetch failed", err);
          return null;
        }),
      ]);
      if (todayPnLResp) {
        setTodayPnL(todayPnLResp);
      }
      const nextMarketSymbols = Array.from(
        new Set(
          [
            ...(response.positions ?? []).map((position) => position.symbol),
            response.fund.benchmarkSymbol ?? "",
            ...((response.fund.universe?.symbols ?? []).slice(0, 5)),
          ]
            .map((value) => value.trim())
            .filter(Boolean),
        ),
      );
      setMarketSymbols(nextMarketSymbols);
      setData(
        buildDashboardData(
          response.fund,
          response.navHistory ?? [],
          response.positions ?? [],
          response.trades ?? [],
          response.workflow,
          [],
          null,
        ),
      );
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, copy.missingFundId, fundId]);

  useEffect(() => {
    void fetchData();
  }, [fetchData]);

  // PR-4: subscribe to the SSE quote stream so the holdings table
  // refreshes prices ~every 2s without polling /portfolio. EventSource
  // auth relies on the `fundai_session` cookie set on login, so we set
  // withCredentials=true. The browser auto-reconnects on transient
  // network blips; we only intervene when fundId / data identity changes.
  useEffect(() => {
    if (!fundId || !hasCoreData) {
      return;
    }
    const url = buildPortfolioQuotesStreamUrl(fundId);
    const source = new EventSource(url, { withCredentials: true });
    const handleQuotes = (event: MessageEvent) => {
      let frame: PortfolioQuotesFrame;
      try {
        frame = JSON.parse(event.data) as PortfolioQuotesFrame;
      } catch {
        return;
      }
      if (!frame || !Array.isArray(frame.quotes) || frame.quotes.length === 0) {
        return;
      }
      setData((current) => {
        if (!current) return current;
        const byKey = new Map<string, PortfolioQuote>();
        for (const quote of frame.quotes) {
          if (quote.instrumentKey) byKey.set(quote.instrumentKey, quote);
        }
        const nextPositions = current.positions.map((position) => {
          const update = byKey.get(position.instrumentKey ?? "");
          if (!update) return position;
          const nextCurrentPrice = typeof update.currentPrice === "number" ? update.currentPrice : position.currentPrice;
          const multiplier = positionMultiplier(position);
          const nextMarketValue = typeof update.marketValue === "number" && Number.isFinite(update.marketValue)
            ? update.marketValue
            : position.quantity * nextCurrentPrice * multiplier;
          const direction = (position.positionSide ?? "").toLowerCase();
          const delta = direction === "short"
            ? position.costPrice - nextCurrentPrice
            : nextCurrentPrice - position.costPrice;
          const nextPnl = delta * position.quantity * multiplier;
          const costBasis = position.costPrice * position.quantity * multiplier;
          const nextPnlPercent = costBasis > 0 ? (nextPnl / costBasis) * 100 : 0;
          return {
            ...position,
            currentPrice: nextCurrentPrice,
            marketValue: nextMarketValue,
            pnl: nextPnl,
            pnlPercent: nextPnlPercent,
            priceAsOf: update.priceAsOf ?? position.priceAsOf,
            priceSource: update.priceSource ?? position.priceSource,
            isStale: typeof update.isStale === "boolean" ? update.isStale : position.isStale,
          };
        });
        return { ...current, positions: nextPositions };
      });
    };
    source.addEventListener("quotes", handleQuotes as EventListener);
    return () => {
      source.removeEventListener("quotes", handleQuotes as EventListener);
      source.close();
    };
  }, [fundId, hasCoreData]);

  useEffect(() => {
    if (!fundId || !hasCoreData) {
      return;
    }
    if (!marketSymbols.length) {
      setQuotesLoading(false);
      return;
    }
    let cancelled = false;
    setQuotesLoading(true);
    void fetchFundMarketQuotes(fundId, marketSymbols)
      .then((quotesResponse) => {
        if (cancelled) {
          return;
        }
        setData((current) =>
          current
            ? {
                ...current,
                latestQuotes: quotesResponse.quotes ?? [],
              }
            : current,
        );
      })
      .catch(() => undefined)
      .finally(() => {
        if (!cancelled) {
          setQuotesLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [hasCoreData, fundId, marketSymbols]);

  // LLM usage fetch is now driven by useSWRFetch above; this used
  // to be a manual useEffect that re-fetched on every mount. The
  // SWR cache replaces it transparently — see llmUsageSwr.

  useEffect(() => {
    if (!fundId || !hasCoreData) {
      return;
    }
    let cancelled = false;
    setPnlAttributionLoading(true);
    void fetchFundPnLAttribution(fundId)
      .then((attribution) => {
        if (!cancelled) {
          setPnlAttribution(attribution);
        }
      })
      .catch(() => {
        if (!cancelled) {
          setPnlAttribution(null);
        }
      })
      .finally(() => {
        if (!cancelled) {
          setPnlAttributionLoading(false);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [hasCoreData, fundId]);

  const loadNewsDigest = useCallback(async () => {
    if (!fundId || !data || !marketSymbols.length) {
      return;
    }
    setNewsRequested(true);
    setNewsLoading(true);
    setNewsError(null);
    try {
      const newsDigest = await fetchFundMarketNewsDigest(fundId, marketSymbols.slice(0, 4), 6);
      setData((current) =>
        current
          ? {
              ...current,
              newsDigest,
            }
          : current,
      );
    } catch (err) {
      setNewsError(formatApiError(err, copy.loadError));
    } finally {
      setNewsLoading(false);
    }
  }, [copy.loadError, data, fundId, marketSymbols]);

  const handleStartWorkflow = useCallback(async () => {
    if (!fundId) {
      return;
    }
    setStarting(true);
    try {
      await apiPost(`/api/funds/${fundId}/workflow/start`);
      await fetchData();
    } catch (err) {
      setError(formatApiError(err, copy.startWorkflowError));
    } finally {
      setStarting(false);
    }
  }, [copy.startWorkflowError, fetchData, fundId]);

  const tradeStatusBadge = useCallback((status: TradeView["status"]): string => {
    switch (status) {
      case "filled":
        return "text-emerald-700 bg-emerald-50";
      case "pending":
        return "text-amber-700 bg-amber-50";
      case "rejected":
        return "text-red-700 bg-red-50";
    }
  }, []);

  if (loading) {
    // Component-level skeletons reserve the cards' shape so the
    // dashboard doesn't visually jump when data arrives. The
    // SR-only `copy.loading` text below preserves the spoken
    // affordance for screen readers — the visual SkeletonCards
    // are aria-hidden so the announcement isn't repeated thrice.
    return (
      <div className="space-y-6">
        <p role="status" aria-live="polite" className="sr-only">
          {copy.loading}
        </p>
        <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
          <SkeletonCard rows={2} metrics />
          <SkeletonCard rows={2} metrics />
          <SkeletonCard rows={2} metrics />
        </div>
        <SkeletonCard rows={4} />
        <div className="grid grid-cols-1 gap-4 lg:grid-cols-2">
          <SkeletonCard rows={5} />
          <SkeletonCard rows={5} />
        </div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-center">
        <p className="text-sm font-medium text-red-800">{copy.errorTitle}</p>
        <p className="mt-1 text-xs text-red-600">{error}</p>
        <button onClick={() => void fetchData()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  if (!fundId) {
    return (
      <div className="rounded-xl border border-gray-200 bg-white p-6 text-center text-sm text-gray-600">
        {copy.noFundSelected}
      </div>
    );
  }
  if (!data) {
    // We're past the loading + error early returns above but still have
    // no data — this is the rare race where fetch resolves with an
    // unexpected shape. Surface an explicit empty/retry instead of a
    // blank page so the user has a way out.
    return (
      <div className="rounded-xl border border-amber-200 bg-amber-50 p-6 text-center text-sm text-amber-800">
        <p>{copy.dashboardUnavailable}</p>
        <button
          onClick={() => void fetchData()}
          className="mt-4 rounded-lg bg-amber-600 px-4 py-2 text-sm font-medium text-white transition hover:bg-amber-700"
        >
          {copy.retry}
        </button>
      </div>
    );
  }

  const workflowState = (data.workflow.state ?? "idle").toLowerCase();
  const workflowBlocked = workflowState === "running" || workflowState === "paused";

  return (
    <div className="mx-auto max-w-7xl px-4 py-6 sm:px-6 lg:px-8">
      <div className="col-span-full rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-4 sm:flex-row sm:items-center sm:justify-between">
          <div>
            <h1 className="text-xl font-bold text-gray-900">{data.fund.name}</h1>
            <div className="mt-2 flex flex-wrap items-center gap-2">
              <span
                className={`inline-block rounded-full px-2.5 py-0.5 text-xs font-medium ${
                  data.fund.tradingMode === "live" ? "bg-emerald-100 text-emerald-800" : "bg-blue-100 text-blue-800"
                }`}
              >
                {tradingModeLabel(data.fund.tradingMode)}
              </span>
              {instrumentMeta(data.fund.market, data.fund.exchange, data.fund.assetClass, data.fund.baseCurrency).map((item) => (
                <span key={item} className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">
                  {humanizeValue(item)}
                </span>
              ))}
              {data.fund.primaryDirection ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{directionLabel(data.fund.primaryDirection)}</span> : null}
            </div>
            {(data.fund.benchmarkSymbol || data.fund.universeSummary || data.fund.universeThemesSummary || data.fund.universeSectorsSummary) ? (
              <div className="mt-3 space-y-1 text-sm text-gray-500">
                <p>
                  {copy.benchmark} {data.fund.benchmarkSymbol || copy.unset}
                  {data.fund.universeSummary ? ` · ${copy.universe} ${data.fund.universeSummary}` : ""}
                </p>
                {data.fund.universeThemesSummary ? <p>{copy.themes} {data.fund.universeThemesSummary}</p> : null}
                {data.fund.universeSectorsSummary ? <p>{copy.sectors} {data.fund.universeSectorsSummary}</p> : null}
              </div>
            ) : null}
          </div>
        </div>

        <div className="mt-6 grid grid-cols-2 gap-4 sm:grid-cols-4">
          <div className="rounded-lg bg-gray-50 px-4 py-3">
            <p className="text-xs font-medium text-gray-500">{copy.metrics.totalAssets}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">
              {formatMoneyForDisplay(data.fund.totalAssets, data.fund.baseCurrency, displayCurrency, language)}
            </p>
          </div>
          <div className="rounded-lg bg-gray-50 px-4 py-3">
            <p className="text-xs font-medium text-gray-500">{copy.metrics.availableCash}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">
              {formatMoneyForDisplay(data.fund.availableCash, data.fund.baseCurrency, displayCurrency, language)}
            </p>
          </div>
          <div className="rounded-lg bg-gray-50 px-4 py-3">
            <p className="text-xs font-medium text-gray-500">{copy.metrics.todayPnL}</p>
            {(() => {
              // Prefer the server-computed today P&L when present:
              // it correctly separates realised + unrealised delta
              // and handles missing yesterday-snapshot gracefully.
              // Fall back to the client-side NAV-diff (legacy) until
              // the today-pnl request resolves.
              const value = todayPnL ? todayPnL.todayPnl : data.fund.todayPnL;
              const tone = value > 0 ? "text-emerald-600" : value < 0 ? "text-red-600" : "text-gray-900";
              const sign = value >= 0 ? "+" : "";
              return (
                <>
                  <p className={`mt-1 text-lg font-semibold ${tone}`}>
                    {sign + formatMoneyForDisplay(value, data.fund.baseCurrency, displayCurrency, language)}
                  </p>
                  {todayPnL ? (
                    <p className="mt-1 text-[10px] leading-tight text-gray-500">
                      {t("metrics.realisedTodayPrefix")}
                      <span className={todayPnL.realisedPnl >= 0 ? "text-emerald-600" : "text-red-600"}>
                        {(todayPnL.realisedPnl >= 0 ? "+" : "") + formatMoneyForDisplay(todayPnL.realisedPnl, data.fund.baseCurrency, displayCurrency, language)}
                      </span>
                      {!todayPnL.baselineFresh && todayPnL.priorCloseDate ? (
                        <span className="ml-1 text-amber-700" title={t("metrics.baselineHint")}>
                          {t("metrics.baselineLabel", { date: todayPnL.priorCloseDate })}
                        </span>
                      ) : null}
                    </p>
                  ) : null}
                </>
              );
            })()}
          </div>
          <div className="rounded-lg bg-gray-50 px-4 py-3">
            <p className="text-xs font-medium text-gray-500">{copy.metrics.totalReturn}</p>
            <p className={`mt-1 text-lg font-semibold ${data.fund.totalReturn > 0 ? "text-emerald-600" : data.fund.totalReturn < 0 ? "text-red-600" : "text-gray-900"}`}>
              {formatPercent(data.fund.totalReturn)}
            </p>
          </div>
        </div>
      </div>

      <div className="mt-6 grid grid-cols-1 gap-6 lg:grid-cols-3">
        <div className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm lg:col-span-2">
          <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.nav}</h2>
          {data.navHistory.length === 0 ? (
            <div className="rounded-lg border border-dashed border-gray-200 bg-gray-50 px-4 py-6 text-sm text-gray-500">
              <p className="font-medium text-gray-700">{copy.emptyStates.noNav.title}</p>
              <p className="mt-1">{copy.emptyStates.noNav.description}</p>
              <button
                onClick={() => void handleStartWorkflow()}
                disabled={starting}
                className="mt-4 inline-flex rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {starting ? copy.startingWorkflow : workflowBlocked ? copy.workflowRunning : copy.emptyStates.noNav.actionLabel}
              </button>
            </div>
          ) : (
            <div className="h-72">
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={data.navHistory} margin={{ top: 5, right: 20, left: 0, bottom: 5 }}>
                  <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                  <XAxis dataKey="date" tick={{ fontSize: 11 }} tickFormatter={chartDateLabel} stroke="#9ca3af" />
                  <YAxis
                    domain={["auto", "auto"]}
                    tick={{ fontSize: 11 }}
                    stroke="#9ca3af"
                    tickFormatter={(value: number) => formatNumberForLanguage(value, language, { maximumFractionDigits: 2 })}
                  />
                  <Tooltip
                    contentStyle={{ fontSize: 12, borderRadius: 8, border: "1px solid #e5e7eb" }}
                    labelFormatter={(value) => chartDateLabel(String(value))}
                    formatter={(value: number) => formatNumberForLanguage(value, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })}
                  />
                  <Legend verticalAlign="top" height={28} iconType="plainline" wrapperStyle={{ fontSize: 12 }} />
                  <Line type="monotone" dataKey="nav" name={t("chart.fundNav")} stroke="#3b82f6" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                </LineChart>
              </ResponsiveContainer>
            </div>
          )}
        </div>

        <div className="flex flex-col gap-6">
          <div className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.workflow}</h2>
            <div className="flex items-center gap-3">
              <div className="flex h-10 w-10 items-center justify-center rounded-full bg-blue-100 text-sm font-bold text-blue-700">
                {workflowState === "running" ? "ON" : workflowState === "paused" ? "PA" : workflowState === "rejected" ? "RJ" : "--"}
              </div>
              <div>
                <p className="text-sm font-semibold text-gray-900">{workflowStepLabel(data.workflow.step)}</p>
                <p className="text-xs text-gray-500">{workflowStateLabel(data.workflow.state)}</p>
              </div>
            </div>

            <div className="mt-4 rounded-lg bg-gray-50 px-3 py-2">
              <p className="text-xs text-gray-500">
                {copy.currentState}: <span className="font-medium text-gray-700">{workflowStateLabel(data.workflow.state)}</span>
              </p>
              {data.workflow.tradingDate ? <p className="text-[11px] text-gray-400">{copy.tradingDate}: {formatDateValueForLanguage(data.workflow.tradingDate, language)}</p> : null}
              {data.workflow.startedAt ? <p className="text-[11px] text-gray-400">{copy.latestStart}: {formatDateTimeForLanguage(data.workflow.startedAt, language)}</p> : null}
            </div>

            <div className="mt-4 space-y-3">
              <div>
                <div className="mb-1 flex items-center justify-between text-xs text-gray-500">
                  <span>{copy.progress}</span>
                  <span>
                    {data.workflow.completedSteps ?? 0}/{data.workflow.totalSteps ?? 0} {copy.completedSteps}
                    {(data.workflow.failedSteps ?? 0) > 0 ? ` · ${data.workflow.failedSteps} ${copy.failedSteps}` : ""}
                  </span>
                </div>
                <div className="h-2 overflow-hidden rounded-full bg-gray-100">
                  <div className="h-full rounded-full bg-blue-600" style={{ width: `${Math.max(0, Math.min(100, data.workflow.progressPercent ?? 0))}%` }} />
                </div>
              </div>
              <p className="text-xs text-gray-500">
                {copy.duration}: <span className="font-medium text-gray-700">{formatDurationMs(data.workflow.runningForMs)}</span>
              </p>
              {(data.workflow.steps ?? []).length > 0 ? (
                <details className="rounded-lg border border-gray-100 bg-white p-3">
                  <summary className="cursor-pointer text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.timeline}</summary>
                  <div className="mt-3 space-y-2">
                    {(data.workflow.steps ?? []).map((step) => (
                      <div key={step.step} className={`rounded-lg border px-3 py-2 text-xs ${workflowStepStatusClass(step.status)}`}>
                        <div className="flex items-center justify-between gap-2">
                          <span className="font-semibold">{workflowStepLabel(step.step || step.label || "")}</span>
                          <span className="uppercase tracking-wide">{step.status || "pending"}</span>
                        </div>
                        <div className="mt-1 flex flex-wrap gap-x-3 gap-y-1 opacity-80">
                          {step.startedAt ? <span>{formatDateTimeForLanguage(step.startedAt, language)}</span> : null}
                          {step.durationMs ? <span>{formatDurationMs(step.durationMs)}</span> : null}
                          {step.error ? <span>{step.error}</span> : null}
                        </div>
                      </div>
                    ))}
                  </div>
                </details>
              ) : null}
            </div>

            <button
              onClick={() => void handleStartWorkflow()}
              disabled={starting || workflowBlocked}
              className="mt-4 inline-flex items-center justify-center rounded-lg bg-blue-600 px-4 py-2.5 text-sm font-medium text-white transition-colors hover:bg-blue-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {starting ? copy.startingWorkflow : workflowBlocked ? copy.workflowRunning : copy.startWorkflow}
            </button>
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
            <div className="mb-4 flex items-center justify-between gap-3">
              <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.llmUsage}</h2>
              <Link to="../usage" className="text-xs font-medium text-blue-600 hover:text-blue-700">
                {copy.quickActions.usage}
              </Link>
            </div>
            {llmUsageLoading && !llmUsage ? (
              <p className="text-sm text-gray-500">{copy.llm.loading}</p>
            ) : llmUsage && llmUsage.totalCalls > 0 ? (
              <div className="space-y-4">
                <div className="grid grid-cols-3 gap-2">
                  <div className="rounded-lg bg-blue-50 p-3">
                    <p className="text-[11px] font-medium uppercase tracking-wide text-blue-500">{copy.llm.calls}</p>
                    <p className="mt-1 text-lg font-semibold text-blue-900">{formatNumberForLanguage(llmUsage.totalCalls, language)}</p>
                  </div>
                  <div className="rounded-lg bg-violet-50 p-3">
                    <p className="text-[11px] font-medium uppercase tracking-wide text-violet-500">{copy.llm.tokens}</p>
                    <p className="mt-1 text-lg font-semibold text-violet-900">{formatNumberForLanguage(llmUsage.totalTokens, language)}</p>
                  </div>
                  <div className="rounded-lg bg-emerald-50 p-3">
                    <p className="text-[11px] font-medium uppercase tracking-wide text-emerald-500">{copy.llm.cost}</p>
                    <p className="mt-1 text-lg font-semibold text-emerald-900">{formatMoneyForDisplay(llmUsage.priceCents / 100, "CNY", displayCurrency, language)}</p>
                  </div>
                </div>

                <div>
                  <div className="mb-2 flex items-center justify-between text-xs">
                    <span className="font-semibold text-gray-700">{copy.llm.byAgent}</span>
                    <span className="text-gray-400">{formatNumberForLanguage(llmUsage.customKeyCalls, language)} {copy.llm.customKey}</span>
                  </div>
                  <div className="space-y-2">
                    {llmUsage.byAgent.slice(0, 4).map((item) => {
                      const width = llmUsage.priceCents > 0 ? Math.max(6, Math.min(100, (item.priceCents / llmUsage.priceCents) * 100)) : 0;
                      return (
                        <div key={item.key}>
                          <div className="mb-1 flex items-center justify-between gap-2 text-xs">
                            <span className="truncate font-medium text-gray-700">{item.label || item.key}</span>
                            <span className="shrink-0 text-gray-500">{formatMoneyForDisplay(item.priceCents / 100, "CNY", displayCurrency, language)}</span>
                          </div>
                          <div className="h-1.5 rounded-full bg-gray-100">
                            <div className="h-1.5 rounded-full bg-blue-500" style={{ width: `${width}%` }} />
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </div>

                <div>
                  <p className="mb-2 text-xs font-semibold text-gray-700">{copy.llm.byStep}</p>
                  <div className="flex flex-wrap gap-2">
                    {llmUsage.byStep.slice(0, 5).map((item) => (
                      <span key={item.key} className="rounded-full bg-gray-100 px-2.5 py-1 text-[11px] font-medium text-gray-600">
                        {item.label || workflowStepLabel(item.key)} · {formatNumberForLanguage(item.totalCalls, language)}
                      </span>
                    ))}
                  </div>
                </div>

                {llmUsage.recentCalls.length > 0 ? (
                  <div>
                    <p className="mb-2 text-xs font-semibold text-gray-700">{copy.llm.recent}</p>
                    <div className="space-y-2">
                      {llmUsage.recentCalls.slice(0, 3).map((call) => (
                        <div key={call.id} className="rounded-lg border border-gray-100 px-3 py-2 text-xs">
                          <div className="flex items-center justify-between gap-2">
                            <span className="truncate font-medium text-gray-700">{call.agentName || workflowStepLabel(call.stepName)}</span>
                            <span className="text-gray-400">{formatDateTimeForLanguage(call.createdAt, language)}</span>
                          </div>
                          <p className="mt-1 text-gray-500">
                            {call.modelProvider}/{call.modelName} · {formatNumberForLanguage(call.totalTokens, language)} tokens · {formatMoneyForDisplay(call.priceCents / 100, "CNY", displayCurrency, language)}
                          </p>
                        </div>
                      ))}
                    </div>
                  </div>
                ) : null}
              </div>
            ) : (
              <p className="text-sm text-gray-500">{copy.llm.empty}</p>
            )}
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.quotes}</h2>
            {quotesLoading && data.latestQuotes.length === 0 ? (
              <div className="space-y-3">
                {[0, 1, 2].map((index) => (
                  <div key={index} className="animate-pulse rounded-lg bg-gray-50 px-3 py-3">
                    <div className="flex items-start justify-between gap-3">
                      <div className="space-y-2">
                        <div className="h-4 w-24 rounded bg-gray-200" />
                        <div className="h-3 w-32 rounded bg-gray-100" />
                      </div>
                      <div className="space-y-2 text-right">
                        <div className="h-4 w-20 rounded bg-gray-200" />
                        <div className="h-3 w-24 rounded bg-gray-100" />
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            ) : data.latestQuotes.length === 0 ? (
              <p className="text-sm text-gray-500">{copy.noQuotes}</p>
            ) : (
              <div className="space-y-3">
                {data.latestQuotes.slice(0, 5).map((quote) => (
                  <div key={`${quote.symbol}-${quote.source}-${quote.asOf}`} className="rounded-lg bg-gray-50 px-3 py-3">
                    <div className="flex items-start justify-between gap-3">
                      <div>
                        <p className="text-sm font-semibold text-gray-900">{quote.symbol}</p>
                        <div className="mt-1 flex flex-wrap gap-1.5">
                          {instrumentMeta(quote.market, quote.exchange, quote.assetClass).map((item) => (
                            <span key={`${quote.symbol}-${item}`} className="rounded-full bg-white px-2 py-1 text-[11px] text-gray-600 ring-1 ring-gray-200">
                              {humanizeValue(item)}
                            </span>
                          ))}
                        </div>
                      </div>
                      <div className="text-right">
                        <p className="text-sm font-semibold text-gray-900">
                          {formatMoneyForDisplay(quote.price, quote.quoteCurrency || data.fund.baseCurrency, displayCurrency, language)}
                        </p>
                        <p className="mt-1 text-[11px] text-gray-500">{copy.quoteSource}: {quote.source}</p>
                        <p className="text-[11px] text-gray-400">{copy.quoteAsOf}: {formatDateTimeForLanguage(quote.asOf, language)}</p>
                      </div>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>

          <div className="rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
            <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.quickActions}</h2>
            <div className="flex flex-col gap-2.5 text-sm text-gray-600">
              <Link to={`/funds/${fundId}/decisions`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.decisions}
              </Link>
              <Link to={`/funds/${fundId}/trades`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.trades}
              </Link>
              <Link to={`/funds/${fundId}/subscription`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.subscription}
              </Link>
              <Link to={`/funds/${fundId}/models`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.models}
              </Link>
              <Link to={`/funds/${fundId}/usage`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.usage}
              </Link>
              <Link to={`/funds/${fundId}/settings`} className="rounded-lg border border-gray-200 bg-white px-4 py-2.5 font-medium hover:bg-gray-50">
                {copy.quickActions.settings}
              </Link>
            </div>
          </div>
        </div>
      </div>

      <div className="mt-6 rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.news}</h2>
        <div className="space-y-3">
          {!newsRequested && !data.newsDigest ? (
            <div className="rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-5 text-sm text-gray-600">
              <p className="font-medium text-gray-900">{copy.newsIdleTitle}</p>
              <p className="mt-1">{copy.newsIdleDescription}</p>
              <button onClick={() => void loadNewsDigest()} className="mt-3 rounded-lg bg-indigo-600 px-3 py-2 text-sm font-medium text-white hover:bg-indigo-700">
                {copy.loadNews}
              </button>
            </div>
          ) : newsLoading && !data.newsDigest ? (
            <div className="space-y-3">
              <div className="rounded-xl border border-blue-100 bg-blue-50/60 px-4 py-4 text-sm text-blue-800">{copy.newsLoading}</div>
              {[0, 1, 2].map((index) => (
                <article key={index} className="animate-pulse rounded-xl border border-gray-200 bg-gray-50/80 px-4 py-4">
                  <div className="space-y-3">
                    <div className="flex gap-2">
                      <div className="h-5 w-16 rounded-full bg-gray-200" />
                      <div className="h-5 w-24 rounded-full bg-gray-100" />
                    </div>
                    <div className="h-4 w-4/5 rounded bg-gray-200" />
                    <div className="h-3 w-full rounded bg-gray-100" />
                    <div className="h-3 w-2/3 rounded bg-gray-100" />
                  </div>
                </article>
              ))}
            </div>
          ) : newsError && !data.newsDigest ? (
            <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-4 text-sm text-amber-900">
              <p className="font-medium">{copy.newsErrorTitle}</p>
              <p className="mt-1 text-amber-800">{newsError}</p>
              <button onClick={() => void loadNewsDigest()} className="mt-3 rounded-lg bg-amber-600 px-3 py-2 text-sm font-medium text-white hover:bg-amber-700">
                {copy.retryNews}
              </button>
            </div>
          ) : data.newsDigest?.items?.length ? (
            data.newsDigest.items.slice(0, 6).map((item, index) => {
              const localizedTitle = pickLocalizedText(language, item.title, item.titleZh, item.titleEn);
              const localizedSummary = pickLocalizedText(language, item.summary, item.summaryZh, item.summaryEn);
              const languageTag =
                item.language === "zh" ? copy.newsLanguageZh : item.language === "en" ? copy.newsLanguageEn : "";
              return (
                <article key={`${localizedTitle || item.title}-${index}`} className="rounded-xl border border-gray-200 bg-gray-50/80 px-4 py-4">
                  <div className="flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
                    <div className="min-w-0 flex-1">
                      <div className="flex flex-wrap gap-2">
                        {languageTag ? (
                          <span className="rounded-full bg-indigo-50 px-2.5 py-1 text-[11px] font-semibold uppercase tracking-wider text-indigo-700 ring-1 ring-indigo-100">
                            {languageTag}
                          </span>
                        ) : null}
                        {item.source ? <span className="rounded-full bg-white px-2.5 py-1 text-[11px] font-medium text-gray-600 ring-1 ring-gray-200">{item.source}</span> : null}
                        {item.publishedAt ? (
                          <span className="rounded-full bg-white px-2.5 py-1 text-[11px] text-gray-500 ring-1 ring-gray-200">
                            {copy.newsPublishedAt}: {formatDateTimeForLanguage(item.publishedAt, language)}
                          </span>
                        ) : null}
                        {item.symbols?.map((symbol) => (
                          <span key={`${localizedTitle || item.title}-${symbol}`} className="rounded-full bg-blue-50 px-2.5 py-1 text-[11px] font-medium text-blue-700 ring-1 ring-blue-100">
                            {symbol}
                          </span>
                        ))}
                      </div>
                      <p className="mt-3 text-sm font-semibold leading-6 text-gray-900">{localizedTitle}</p>
                      {localizedSummary ? <p className="mt-2 line-clamp-3 text-sm leading-6 text-gray-600">{localizedSummary}</p> : null}
                    </div>
                    {item.url ? (
                      <a
                        href={item.url}
                        target="_blank"
                        rel="noreferrer"
                        className="inline-flex shrink-0 items-center rounded-lg bg-white px-3 py-2 text-sm font-medium text-blue-600 ring-1 ring-gray-200 transition hover:text-blue-700"
                      >
                        {copy.openArticle}
                      </a>
                    ) : null}
                  </div>
                </article>
              );
            })
          ) : (
            <p className="text-sm text-gray-500">{copy.noNews}</p>
          )}
          {data.newsDigest?.providerNotes?.length ? (
            <div className="rounded-lg border border-amber-200 bg-amber-50 px-4 py-3">
              <p className="text-sm font-medium text-amber-900">{copy.newsProviderWarnings}</p>
              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-amber-800">
                {data.newsDigest.providerNotes.slice(0, 6).map((note, index) => (
                  <li key={`${note}-${index}`}>{note}</li>
                ))}
              </ul>
            </div>
          ) : null}
        </div>
      </div>

      <div className="mt-6 rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="mb-4 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.attribution}</h2>
        {pnlAttributionLoading && !pnlAttribution ? (
          <p className="text-sm text-gray-500">{copy.attribution.loading}</p>
        ) : pnlAttribution ? (
          <div className="space-y-5">
            <div className="grid gap-3 sm:grid-cols-4">
              <div className="rounded-lg bg-blue-50 p-3">
                <p className="text-[11px] font-medium uppercase tracking-wide text-blue-500">{copy.attribution.realized}</p>
                <p className="mt-1 text-lg font-semibold text-blue-900">{formatMoneyForDisplay(pnlAttribution.realizedPnl, data.fund.baseCurrency, displayCurrency, language)}</p>
              </div>
              <div className="rounded-lg bg-emerald-50 p-3">
                <p className="text-[11px] font-medium uppercase tracking-wide text-emerald-500">{copy.attribution.unrealized}</p>
                <p className="mt-1 text-lg font-semibold text-emerald-900">{formatMoneyForDisplay(pnlAttribution.unrealizedPnl, data.fund.baseCurrency, displayCurrency, language)}</p>
              </div>
              <div className="rounded-lg bg-amber-50 p-3">
                <p className="text-[11px] font-medium uppercase tracking-wide text-amber-500">{copy.attribution.fees}</p>
                <p className="mt-1 text-lg font-semibold text-amber-900">{formatMoneyForDisplay(pnlAttribution.feeDrag, data.fund.baseCurrency, displayCurrency, language)}</p>
              </div>
              <div className="rounded-lg bg-violet-50 p-3">
                <p className="text-[11px] font-medium uppercase tracking-wide text-violet-500">{copy.attribution.returnPct}</p>
                <p className="mt-1 text-lg font-semibold text-violet-900">{formatPercent(pnlAttribution.returnPct)}</p>
              </div>
            </div>
            {pnlAttribution.bySymbol.length > 0 ? (
              <div>
                <p className="mb-2 text-xs font-semibold text-gray-700">{copy.attribution.bySymbol}</p>
                <div className="space-y-2">
                  {pnlAttribution.bySymbol.slice(0, 6).map((item) => {
                    const maxAbs = Math.max(...pnlAttribution.bySymbol.map((bucket) => Math.abs(bucket.totalPnl)), 1);
                    const width = Math.max(6, Math.min(100, (Math.abs(item.totalPnl) / maxAbs) * 100));
                    const positive = item.totalPnl >= 0;
                    return (
                      <div key={item.key}>
                        <div className="mb-1 flex items-center justify-between gap-3 text-xs">
                          <span className="truncate font-medium text-gray-700">{item.label || item.key}</span>
                          <span className={positive ? "text-emerald-600" : "text-red-600"}>{formatMoneyForDisplay(item.totalPnl, data.fund.baseCurrency, displayCurrency, language)}</span>
                        </div>
                        <div className="h-1.5 rounded-full bg-gray-100">
                          <div className={`h-1.5 rounded-full ${positive ? "bg-emerald-500" : "bg-red-500"}`} style={{ width: `${width}%` }} />
                        </div>
                      </div>
                    );
                  })}
                </div>
              </div>
            ) : (
              <p className="text-sm text-gray-500">{copy.attribution.empty}</p>
            )}
          </div>
        ) : (
          <p className="text-sm text-gray-500">{copy.attribution.empty}</p>
        )}
      </div>

      <div className="mt-6 overflow-x-auto rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.positions}</h2>
        {data.positions.length === 0 ? (
          <div className="rounded-lg border border-dashed border-gray-200 bg-gray-50 px-4 py-6 text-sm text-gray-500">
            <p className="font-medium text-gray-700">{copy.emptyStates.noPositions.title}</p>
            <p className="mt-1">{copy.emptyStates.noPositions.description}</p>
            <Link to="decisions" className="mt-4 inline-flex rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
              {copy.emptyStates.noPositions.actionLabel}
            </Link>
          </div>
        ) : (
          <table className="w-full min-w-[960px] text-sm">
            <thead>
              <tr className="border-b border-gray-100 text-left text-xs font-medium uppercase tracking-wider text-gray-400">
                <th className="pb-2 pr-4">{copy.columns.instrument}</th>
                <th className="pb-2 pr-4">{copy.columns.profile}</th>
                <th className="pb-2 pr-4">{copy.columns.direction}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.quantity}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.costPrice}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.currentPrice}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.unrealizedPnL}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.margin}</th>
                <th className="pb-2 text-right">{copy.columns.weight}</th>
              </tr>
            </thead>
            <tbody>
              {data.positions.map((position) => (
                <tr key={position.instrumentKey || position.symbol} className="border-b border-gray-50 transition-colors last:border-0 hover:bg-gray-50/60">
                  <td className="py-2.5 pr-4">
                    <p className="font-medium text-gray-900">{position.symbol}</p>
                    <p className="mt-1 text-xs text-gray-500">{position.instrumentKey || position.name}</p>
                  </td>
                  <td className="py-2.5 pr-4">
                    <div className="flex flex-wrap gap-1.5">
                      {instrumentMeta(position.market, position.exchange, position.assetClass).map((item) => (
                        <span key={item} className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">
                          {humanizeValue(item)}
                        </span>
                      ))}
                      {position.expiryDate ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{formatDateForLanguage(position.expiryDate, language)}</span> : null}
                    </div>
                  </td>
                  <td className="py-2.5 pr-4 text-gray-600">
                    <div className="flex flex-col gap-1">
                      <span>{positionSideLabel(position.positionSide)}</span>
                      {typeof position.leverage === "number" ? <span className="text-xs text-gray-500">{formatNumberForLanguage(position.leverage, language)}x</span> : null}
                    </div>
                  </td>
                  <td className="py-2.5 pr-4 text-right text-gray-800">{formatQuantity(position.quantity)}</td>
                  <td className="py-2.5 pr-4 text-right text-gray-600">{formatMoneyForDisplay(position.costPrice, position.priceCurrency, displayCurrency, language)}</td>
                  <td className="py-2.5 pr-4 text-right text-gray-800">
                    <div className="flex flex-col items-end gap-0.5">
                      <span>{formatMoneyForDisplay(position.currentPrice, position.priceCurrency, displayCurrency, language)}</span>
                      <PriceFreshnessBadge priceAsOf={position.priceAsOf} isStale={position.isStale} t={t} />
                    </div>
                  </td>
                  <td className={`py-2.5 pr-4 text-right font-medium ${position.pnl > 0 ? "text-emerald-600" : position.pnl < 0 ? "text-red-600" : "text-gray-700"}`}>
                    <div>{(position.pnl >= 0 ? "+" : "") + formatMoneyForDisplay(position.pnl, position.displayCurrency, displayCurrency, language)}</div>
                    <div className="mt-1 text-xs text-gray-500">{formatPercent(position.pnlPercent)}</div>
                  </td>
                  <td className="py-2.5 pr-4 text-right text-gray-600">
                    {typeof position.marginUsed === "number" ? formatMoneyForDisplay(position.marginUsed, position.displayCurrency, displayCurrency, language) : "—"}
                  </td>
                  <td className="py-2.5 text-right text-gray-600">{formatNumberForLanguage(position.weight, language, { minimumFractionDigits: 1, maximumFractionDigits: 1 })}%</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      <div className="mt-6">
        {fundId ? <BenchmarkChart fundId={fundId} language={language} /> : null}
      </div>

      <div className="mt-6">
        {fundId ? <HoldingsTrendsGrid fundId={fundId} language={language} /> : null}
      </div>

      <div className="mt-6">
        {fundId ? <CorpActionTimeline fundId={fundId} language={language} /> : null}
      </div>

      <div className="mt-6 overflow-x-auto rounded-xl border border-gray-200 bg-white p-6 shadow-sm">
        <h2 className="mb-3 text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.titles.trades}</h2>
        {data.recentTrades.length === 0 ? (
          <div className="rounded-lg border border-dashed border-gray-200 bg-gray-50 px-4 py-6 text-sm text-gray-500">
            <p className="font-medium text-gray-700">{copy.emptyStates.noTrades.title}</p>
            <p className="mt-1">{copy.emptyStates.noTrades.description}</p>
            <Link to="decisions" className="mt-4 inline-flex rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
              {copy.emptyStates.noTrades.actionLabel}
            </Link>
          </div>
        ) : (
          <table className="w-full min-w-[980px] text-sm">
            <thead>
              <tr className="border-b border-gray-100 text-left text-xs font-medium uppercase tracking-wider text-gray-400">
                <th className="pb-2 pr-4">{copy.columns.time}</th>
                <th className="pb-2 pr-4">{copy.columns.instrument}</th>
                <th className="pb-2 pr-4">{copy.columns.side}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.quantity}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.price}</th>
                <th className="pb-2 pr-4 text-right">{copy.columns.amount}</th>
                <th className="pb-2">{copy.columns.status}</th>
              </tr>
            </thead>
            <tbody>
              {data.recentTrades.map((trade) => (
                <tr key={trade.id} className="border-b border-gray-50 transition-colors last:border-0 hover:bg-gray-50/60">
                  <td className="whitespace-nowrap py-2 pr-4 text-xs text-gray-500">{formatDateTimeForLanguage(trade.time, language)}</td>
                  <td className="py-2 pr-4">
                    <p className="font-medium text-gray-900">{trade.symbol}</p>
                    <p className="mt-1 text-xs text-gray-500">{trade.instrumentKey || trade.symbol}</p>
                    <div className="mt-2 flex flex-wrap gap-1.5">
                      {instrumentMeta(trade.market, trade.exchange, trade.assetClass).map((item) => (
                        <span key={item} className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">
                          {humanizeValue(item)}
                        </span>
                      ))}
                      {trade.expiryDate ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{formatDateForLanguage(trade.expiryDate, language)}</span> : null}
                    </div>
                  </td>
                  <td className="py-2 pr-4">
                    <div className="flex flex-col gap-1">
                      <span className={`text-xs font-semibold ${trade.action === "BUY" ? "text-emerald-600" : "text-red-600"}`}>
                        {tradeActionLabel(trade.action)}
                      </span>
                      <div className="flex flex-wrap gap-1.5 text-xs text-gray-500">
                        {trade.positionSide ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{positionSideLabel(trade.positionSide)}</span> : null}
                        {trade.openClose ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{openCloseLabel(trade.openClose)}</span> : null}
                        {typeof trade.leverage === "number" ? <span className="rounded-full bg-gray-100 px-2.5 py-1 text-xs text-gray-600">{formatNumberForLanguage(trade.leverage, language)}x</span> : null}
                      </div>
                    </div>
                  </td>
                  <td className="py-2 pr-4 text-right text-gray-800">{formatQuantity(trade.quantity)}</td>
                  <td className="py-2 pr-4 text-right text-gray-600">{formatMoneyForDisplay(trade.price, trade.priceCurrency, displayCurrency, language)}</td>
                  <td className="py-2 pr-4 text-right text-gray-800">{formatMoneyForDisplay(trade.amount, trade.amountCurrency, displayCurrency, language)}</td>
                  <td className="py-2">
                    <span className={`inline-block rounded-full px-2 py-0.5 text-xs font-medium ${tradeStatusBadge(trade.status)}`}>
                      {tradeStatusLabel(trade.status)}
                    </span>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </div>
  );
}
