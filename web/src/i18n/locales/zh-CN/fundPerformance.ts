// Translations for FundPerformance.tsx — Simplified Chinese
// (zh-CN). Mirror of en-US/fundPerformance.ts; the shape parity
// guard (web/test/i18nNamespaceParity.test.ts) enforces that
// every key here is also present in the English bundle.
const fundPerformance = {
  pageTitle: "业绩中心",
  pageSubtitle:
    "把 NAV 走势、P&L 归因和策略 Sleeve 学习汇集在一个视图，方便你判断这只基金是不是在赚钱、为什么赚、哪些标的是真正的贡献来源。",
  windowGroupLabel: "回看窗口",
  windowLabels: {
    "7d": "近 7 天",
    "30d": "近 30 天",
    "90d": "近 90 天",
    "1y": "近 1 年",
    all: "全部",
  },
  navCurveTitle: "单位净值与总资产",
  navCurveSubtitle:
    "蓝线是基金单位净值，灰色面积是日终总资产；面积分成现金（淡色）和持仓市值（深色），方便看仓位水位。",
  kpis: {
    nav: "单位净值",
    totalReturn: "区间总收益率",
    totalPnL: "区间 P&L",
    realizedPnL: "已实现",
    unrealizedPnL: "未实现",
    feeDrag: "费用拖累",
    beginningAssets: "期初总资产",
    endingAssets: "期末总资产",
  },
  contributorsTitle: "Top 贡献",
  contributorsSubtitle: "按区间 Total P&L 排序的前 5 个标的。",
  contributorsEmpty: "区间内没有已成交的标的。",
  detractorsTitle: "Top 拖累",
  detractorsSubtitle: "按区间 Total P&L 倒序的前 5 个标的（负贡献最大）。",
  contributorsCols: {
    symbol: "标的",
    realized: "已实现",
    unrealized: "未实现",
    total: "合计",
    trades: "交易笔数",
    exposure: "敞口",
    weight: "权重",
  },
  assetClassTitle: "按资产类别归因",
  assetClassSubtitle:
    "用 Total P&L 比较不同资产类别（股票 / 加密 / 期货 …）的相对贡献。",
  assetClassEmpty: "区间内没有按资产类别可统计的数据。",
  dailyReturnsTitle: "日收益分布",
  dailyReturnsSubtitle:
    "把每个交易日的回报放进直方桶里 —— 偏左厚尾说明有大跌、偏右厚尾说明有大涨；红色桶是负日，绿色桶是正日。",
  dailyReturnsEmpty: "没有足够的日收益数据来画直方图。",
  loading: "正在加载业绩数据…",
  errorPrefix: "加载失败：",
  retry: "重试",
  exposureLabel: "敞口",
  weightLabel: "权重",
  pnlLabel: "P&L",
  navLabel: "单位净值",
  totalAssetsLabel: "总资产",
  availableCashLabel: "现金",
  marketValueLabel: "持仓市值",
  noNavData: "暂无净值历史。基金可能尚未开始记账，等第一次结算后再来看。",
} as const;

export default fundPerformance;
