// Translations for MultiFundOverview.tsx — Chinese (zh-CN).
//
// Mirrors web/src/i18n/locales/en-US/multiFundOverview.ts. The
// W6-3 CI guard (web/test/i18nNamespaceParity.test.ts) enforces
// that every key + placeholder set lines up across the two locales.
const multiFundOverview = {
  title: "多基金总览",
  subtitle: "聚合查看你能看到的所有基金与公司的运行状态。",
  loading: "正在加载组合总览……",
  loadError: "加载组合总览失败",
  retry: "重试",
  kFunds: "基金数",
  kCompanies: "公司数",
  kLive: "实盘",
  kAum: "总资产",
  kAvgNav: "平均净值",
  searchPh: "搜索基金或公司名称……",
  modeAll: "全部模式",
  modeLive: "仅实盘",
  modeSim: "仅模拟",
  emptyTitle: "暂无基金",
  emptyDescription: "请先在公司页面创建一只基金。",
  col: {
    fund: "基金",
    company: "公司",
    mode: "模式",
    status: "状态",
    workflow: "今日工作流",
    totalAssets: "总资产",
    nav: "净值",
    baseCurrency: "本币",
  },
  tradingModes: {
    live: "实盘",
    simulation: "模拟",
  },
  workflow: {
    idle: "未开始",
    forbidden: "无权限",
    live: "实时",
    stale: "重连中",
    stateRunning: "运行中",
    stateCompleted: "已完成",
    stateFailed: "失败",
    stateQueued: "排队中",
  },
} as const;

export default multiFundOverview;
