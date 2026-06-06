// Translations for MultiFundOverview.tsx — English (en-US).
//
// W8-3 — migrated from the inline ternary `copy = isEnglish ? {...} : {...}`
// pattern. Function-valued translations are flattened to `{{key}}`
// interpolation strings (none in this page yet) so the bundle stays
// JSON-serializable for the W6-3 missing-key parity guard.
//
// `tradingModes` and `loadError` cover the small handful of strings
// that previously lived as inline ternaries inside helpers (the
// `tradingModeLabel` callback and the catch-block fallback in the
// initial-load effect). Pulling them in here removes the last
// `language ===`/`isEnglish ?` references from the page body.
const multiFundOverview = {
  title: "Multi-fund overview",
  subtitle: "Aggregate state across every fund and company you can see.",
  loading: "Loading portfolio overview…",
  loadError: "Failed to load portfolio overview",
  retry: "Retry",
  kFunds: "Funds",
  kCompanies: "Companies",
  kLive: "Live",
  kAum: "Total AUM",
  kAvgNav: "Avg NAV",
  searchPh: "Search fund or company name…",
  modeAll: "All modes",
  modeLive: "Live only",
  modeSim: "Simulation only",
  emptyTitle: "No funds yet",
  emptyDescription: "Create a fund from the Companies page to see it here.",
  col: {
    fund: "Fund",
    company: "Company",
    mode: "Mode",
    status: "Status",
    workflow: "Today's workflow",
    totalAssets: "Total assets",
    nav: "NAV",
    baseCurrency: "Base ccy",
  },
  tradingModes: {
    live: "Live",
    simulation: "Simulation",
  },
  workflow: {
    idle: "Not started",
    forbidden: "No access",
    live: "Live",
    stale: "Reconnecting",
    stateRunning: "Running",
    stateCompleted: "Completed",
    stateFailed: "Failed",
    stateQueued: "Queued",
  },
} as const;

export default multiFundOverview;
