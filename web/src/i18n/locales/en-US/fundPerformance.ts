// Translations for FundPerformance.tsx — English (en-US).
//
// W9-3 — migrated from the inline `COPY: Record<AppLanguage,
// PageCopy>` table. Same shape as the previous PageCopy
// interface so the consumer keeps using `copy.X` access via
// `i18n.getResourceBundle()` rather than rewriting every JSX
// attribute.
const fundPerformance = {
  pageTitle: "Performance",
  pageSubtitle:
    "NAV path, P&L attribution and strategy-sleeve learning in a single view, so you can answer 'is this fund working, and why?' without bouncing between tabs.",
  windowGroupLabel: "Lookback",
  windowLabels: {
    "7d": "Last 7 days",
    "30d": "Last 30 days",
    "90d": "Last 90 days",
    "1y": "Last year",
    all: "All time",
  },
  navCurveTitle: "Unit NAV & total assets",
  navCurveSubtitle:
    "Blue line is unit NAV. The grey stacked area is end-of-day total assets, split into cash (light) and market value (dark) so the cash sleeve is visible.",
  kpis: {
    nav: "Latest NAV",
    totalReturn: "Period return",
    totalPnL: "Period P&L",
    realizedPnL: "Realised",
    unrealizedPnL: "Unrealised",
    feeDrag: "Fee drag",
    beginningAssets: "Period start AUM",
    endingAssets: "Period end AUM",
  },
  contributorsTitle: "Top contributors",
  contributorsSubtitle: "Top 5 names by period Total P&L.",
  contributorsEmpty: "No traded names in this period.",
  detractorsTitle: "Top detractors",
  detractorsSubtitle: "Bottom 5 names by period Total P&L (biggest drags first).",
  contributorsCols: {
    symbol: "Symbol",
    realized: "Realised",
    unrealized: "Unrealised",
    total: "Total",
    trades: "Trades",
    exposure: "Exposure",
    weight: "Weight",
  },
  assetClassTitle: "Asset-class attribution",
  assetClassSubtitle:
    "Total P&L compared across asset classes (equity / crypto / futures …).",
  assetClassEmpty: "No per-asset-class data for this window yet.",
  dailyReturnsTitle: "Daily return distribution",
  dailyReturnsSubtitle:
    "Buckets each trading day's return into a histogram — a fat left tail means big drawdowns, fat right means big up days. Red bars are losing days, green winning days.",
  dailyReturnsEmpty: "Not enough daily-return rows to draw a histogram yet.",
  loading: "Loading performance data…",
  errorPrefix: "Load failed: ",
  retry: "Retry",
  exposureLabel: "Exposure",
  weightLabel: "Weight",
  pnlLabel: "P&L",
  navLabel: "Unit NAV",
  totalAssetsLabel: "Total assets",
  availableCashLabel: "Cash",
  marketValueLabel: "Market value",
  noNavData:
    "No NAV history yet. The fund probably hasn't booked a settlement — check back after the first daily review.",
} as const;

export default fundPerformance;
