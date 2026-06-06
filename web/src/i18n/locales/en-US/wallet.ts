// Translations for Wallet.tsx — English (en-US).
const wallet = {
  title: "My wallet",
  loading: "Loading wallet balance and ledger...",
  loadError: "Failed to load wallet",
  retry: "Retry",
  subtitle:
    "View your current balance and recent recharge and settlement entries. The underlying ledger still settles in USD.",
  refresh: "Refresh",
  balance: "Current balance",
  ledgerCurrency: "Ledger currency",
  displayCurrencyLabel: "Display currency",
  entries: "Ledger entries",
  ledgerTitle: "Ledger details",
  emptyLedger: "There are no wallet entries yet.",
  time: "Time",
  type: "Type",
  amount: "Amount",
  balanceAfter: "Balance",
  reference: "Reference",
  // Mustache-style braces — i18next escapeValue is off in our
  // bootstrap so the placeholders interpolate cleanly.
  convertedHint:
    "Shown in {{currency}}; ledger settlement remains in {{baseCurrency}}.",
} as const;

export default wallet;
