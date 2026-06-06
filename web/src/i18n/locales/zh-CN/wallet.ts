// Wallet.tsx 的中文翻译（W4-26 迁移）。
const wallet = {
  title: "我的钱包",
  loading: "正在加载钱包余额与流水...",
  loadError: "加载钱包失败",
  retry: "重试",
  subtitle: "查看当前余额和最近的充值、结算流水。底层账本仍按美元结算。",
  refresh: "刷新",
  balance: "当前余额",
  ledgerCurrency: "账本币种",
  displayCurrencyLabel: "显示币种",
  entries: "流水条数",
  ledgerTitle: "流水明细",
  emptyLedger: "当前还没有钱包流水。",
  time: "时间",
  type: "类型",
  amount: "金额",
  balanceAfter: "余额",
  reference: "引用",
  convertedHint: "当前按 {{currency}} 展示，底层账本结算币种仍为 {{baseCurrency}}。",
} as const;

export default wallet;
