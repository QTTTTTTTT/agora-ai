// Translations for MemoryCenter.tsx — Simplified Chinese
// (zh-CN). Mirror of en-US/memoryCenter.ts; the parity guard
// at web/test/i18nNamespaceParity.test.ts enforces that every
// nested key here is also present in the English bundle.
const memoryCenter = {
  loading: "正在加载记忆中心...",
  retry: "重试",
  missingFundId: "缺少 fundId",
  loadFailed: "加载记忆中心失败",
  title: "记忆中心",
  subtitle:
    "查看基金的长期记忆、日记日志、洞察沉淀与成员协作记录，并按成员筛选内容、时间线与统计。",
  memberFilter: "成员筛选",
  allMembers: "全部成员",
  focusLabel: "查看重点",
  focusOptions: {
    all: "全部记忆",
    market: "市场研究",
  },
  marketCoverage: "市场研究覆盖",
  marketCoverageSubtitle:
    "快速查看团队记忆里有多少内容已经沉淀为行情、资讯和研究快照。",
  marketEntries: "市场条目",
  marketTags: "市场标签",
  latestMarketEntry: "最近市场条目",
  noMarketEntry: "暂无",
  researchBadge: "研究",
  newsBadge: "资讯",
  quoteBadge: "行情",
  signalBadge: "信号",
  viewModes: {
    content: "内容视图",
    search: "全文搜索",
    timeline: "时间线",
    stats: "统计",
  },
  statsEmptyTitle: "当前还没有可统计的记忆内容",
  statsEmptyDescription:
    "等策略流程、研究讨论或成员协作产生记录后，这里会自动汇总条目分布、活跃成员与高频主题。",
  totalEntries: "总条目数",
  mostActiveMember: "最活跃成员",
  layerDistribution: "分层分布",
  topTags: "高频标签",
  noTags: "暂无标签统计",
  searchPlaceholder: "按标题、内容关键词搜索记忆...",
  foundResults: "条结果",
  noSearchResults:
    "没有匹配的记忆内容，请尝试更换关键词或切到内容视图浏览各层记录。",
  timelineTitle: "日记时间线",
  timelineEmpty:
    "当前还没有日记层记忆。等日常运行和协作记录产生后，这里会按时间顺序展示关键节点。",
  contentEmpty: "该层还没有记忆条目，后续相关协作内容会自动沉淀到这里。",
  noSelectedEntry:
    "当前层还没有可展示的记忆内容，请先切换到其他层或等待新的协作记录写入。",
  memberPrefix: "成员：",
  unassignedMember: "未关联成员",
  noActiveMember: "暂无",
  layerLabels: {
    long_term: "长期记忆",
    daily: "日记日志",
    dreams: "洞察沉淀",
    agent: "协作记忆",
    analysis: "市场分析",
  },
  layerIcons: {
    long_term: "🧠",
    daily: "📅",
    dreams: "💭",
    agent: "🤖",
    analysis: "📈",
  },
} as const;

export default memoryCenter;
