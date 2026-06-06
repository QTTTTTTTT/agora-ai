// Translations for TradeHistory.tsx — English (en-US).
// Migrated from the inline `copy` block as part of W5-1 (deep
// follow-up to W4-26). The structure mirrors the original
// hand-rolled object so per-key consumer sites only need to
// swap `copy.foo.bar` for `t("foo.bar")`.
const tradeHistory = {
  missingFundId: "Missing fundId",
  loadError: "Failed to load trade history",
  loading: "Loading trade history...",
  retry: "Retry",
  title: "Trade history",
  subtitle:
    "Review filled, pending, and failed orders, and quickly verify notional, fees, trading mode, and execution time.",
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
  emptyDescription:
    "Complete plan approval and move into execution to accumulate fills and execution logs here.",
  goToDecisionCenter: "Go to decision center",
  filteredEmptyTitle: "No records match the current filters",
  filteredEmptyDescription:
    "Adjust the date range or status filter to review the execution details you need.",
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
    actions: "Actions",
  },
  actions: {
    cancel: "Cancel",
    replace: "Modify",
    cancelling: "Cancelling…",
    replacing: "Saving…",
    cancelTitle: "Cancel order",
    replaceTitle: "Modify order",
    cancelConfirm:
      "Cancel this order? This action records to the audit trail and cannot be undone.",
    replaceQuantity: "New quantity",
    replaceLimit: "New limit price",
    replaceStop: "New stop trigger",
    replaceTrailAmount: "New trail amount",
    replaceTrailPercent: "New trail percent (0-1)",
    replaceDisplayQty: "New display quantity (iceberg)",
    replaceNote: "Reason (optional)",
    replaceLeaveBlankHelp: "Leave blank to keep the current value.",
    save: "Save changes",
    cancelButton: "Cancel",
    dismiss: "Close",
    error: "Action failed",
    cancelSuccess: "Order cancelled.",
    replaceSuccess: "Order updated.",
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
  // statusLabels / sideLabels / tradingModes / orderTypes /
  // positionSides / openClose are dynamic dictionaries — i18next
  // resolves them via dotted keys (e.g. `t("statusLabels.filled")`).
  // Adding a new status code to the catalog is then a one-line
  // change without touching the page.
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
  splitter: {
    expand: "Show slices",
    collapse: "Hide slices",
    loading: "Loading slices…",
    empty: "No child slices for this order.",
    error: "Failed to load slices",
    strategyLabel: "Strategy",
    sliceIndex: "Slice",
    parentBadge: "TWAP parent",
  },
} as const;

export default tradeHistory;
