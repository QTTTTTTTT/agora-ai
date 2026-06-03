import React, { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { apiGet, apiPut, formatApiError } from "../lib/api";
import { formatMoneyForDisplay, formatNumberForLanguage, useAppPreferences } from "../lib/preferences";
import BrokerLinksSection from "../components/BrokerLinksSection";
import FundingSection from "../components/FundingSection";
import FundBaseCurrencySection from "../components/FundBaseCurrencySection";
import FundLLMOverridesSection from "../components/FundLLMOverridesSection";

type TradingMode = "simulation" | "live" | "paper";
type FundStatus = "active" | "paused" | "closed";

interface FundUniverse {
  mode: string;
  symbols: string[];
  sectors?: string[];
  themes?: string[];
  customFilters?: string[];
}

interface FundTeamIntervals {
  pm?: number;
  researcher?: number;
  trader?: number;
  risk?: number;
}

interface FundTeamSpecialization {
  markets?: string[];
  assetClasses?: string[];
  themes?: string[];
  instruments?: string[];
  styleHints?: string[];
}

interface FundSpecialization {
  team?: FundTeamSpecialization;
}

interface FundHardRiskConfig {
  maxQuoteAgeSeconds?: number;
}

interface Fund {
  id: string;
  companyId: string;
  name: string;
  description?: string;
  tradingMode: TradingMode;
  initialCapital: number;
  currentCapital: number;
  totalAssets: number;
  nav: number;
  status: FundStatus;
  market?: string;
  exchange?: string;
  assetClass?: string;
  baseCurrency?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  calendarCode?: string;
  timeZone?: string;
  universe?: FundUniverse;
  teamIntervals?: FundTeamIntervals;
  specialization?: FundSpecialization;
  hardRisk?: FundHardRiskConfig;
  // activityRetentionDays controls how long Team Live Activity events
  // are persisted (1..10 days, default 7). When the field is missing
  // from the API response we fall back to the same default the server
  // applies, so the dropdown still shows a sensible selection.
  activityRetentionDays?: number;
}

interface FundForm {
  name: string;
  description: string;
  tradingMode: TradingMode;
  status: FundStatus;
  initialCapital: string;
  market: string;
  exchange: string;
  assetClass: string;
  baseCurrency: string;
  benchmarkSymbol: string;
  primaryDirection: string;
  calendarCode: string;
  timeZone: string;
  universeMode: string;
  universeSymbols: string;
  universeThemes: string;
  universeSectors: string;
  universeCustomFilters: string;
  specializationMarkets: string;
  specializationAssetClasses: string;
  specializationThemes: string;
  specializationInstruments: string;
  specializationStyleHints: string;
  pmInterval: string;
  researcherInterval: string;
  traderInterval: string;
  riskInterval: string;
  hardRiskMaxQuoteAgeSeconds: string;
  activityRetentionDays: string;
}

const intervalFieldKeys = [
  { key: "pmInterval", role: "pm" },
  { key: "researcherInterval", role: "researcher" },
  { key: "traderInterval", role: "trader" },
  { key: "riskInterval", role: "risk" },
] as const;

function buildForm(fund: Fund): FundForm {
  return {
    name: fund.name,
    description: fund.description?.trim() ?? "",
    tradingMode: fund.tradingMode,
    status: fund.status,
    initialCapital: Number.isFinite(fund.initialCapital) ? String(fund.initialCapital) : "0",
    market: fund.market ?? "",
    exchange: fund.exchange ?? "",
    assetClass: fund.assetClass ?? "",
    baseCurrency: fund.baseCurrency ?? "USD",
    benchmarkSymbol: fund.benchmarkSymbol ?? "",
    primaryDirection: fund.primaryDirection ?? "",
    calendarCode: fund.calendarCode ?? "",
    timeZone: fund.timeZone ?? "",
    universeMode: fund.universe?.mode ?? "manual",
    universeSymbols: fund.universe?.symbols?.join(", ") ?? "",
    universeThemes: fund.universe?.themes?.join(", ") ?? "",
    universeSectors: fund.universe?.sectors?.join(", ") ?? "",
    universeCustomFilters: fund.universe?.customFilters?.join(", ") ?? "",
    specializationMarkets: fund.specialization?.team?.markets?.join(", ") ?? "",
    specializationAssetClasses: fund.specialization?.team?.assetClasses?.join(", ") ?? "",
    specializationThemes: fund.specialization?.team?.themes?.join(", ") ?? "",
    specializationInstruments: fund.specialization?.team?.instruments?.join(", ") ?? "",
    specializationStyleHints: fund.specialization?.team?.styleHints?.join(", ") ?? "",
    pmInterval: formatIntervalInput(fund.teamIntervals?.pm),
    researcherInterval: formatIntervalInput(fund.teamIntervals?.researcher),
    traderInterval: formatIntervalInput(fund.teamIntervals?.trader),
    riskInterval: formatIntervalInput(fund.teamIntervals?.risk),
    hardRiskMaxQuoteAgeSeconds: formatIntervalInput(fund.hardRisk?.maxQuoteAgeSeconds),
    activityRetentionDays: formatActivityRetentionDays(fund.activityRetentionDays),
  };
}

// formatActivityRetentionDays normalizes whatever the API returned into
// the dropdown's expected string form. Out-of-range values (legacy
// rows, manual SQL edits) fall back to the platform default so the
// user never sees an empty / surprising selection.
function formatActivityRetentionDays(value?: number): string {
  if (typeof value !== "number" || !Number.isFinite(value)) {
    return String(DEFAULT_ACTIVITY_RETENTION_DAYS);
  }
  if (value < MIN_ACTIVITY_RETENTION_DAYS) {
    return String(MIN_ACTIVITY_RETENTION_DAYS);
  }
  if (value > MAX_ACTIVITY_RETENTION_DAYS) {
    return String(MAX_ACTIVITY_RETENTION_DAYS);
  }
  return String(Math.floor(value));
}

const MIN_ACTIVITY_RETENTION_DAYS = 1;
const MAX_ACTIVITY_RETENTION_DAYS = 10;
const DEFAULT_ACTIVITY_RETENTION_DAYS = 7;

const ACTIVITY_RETENTION_OPTIONS = Array.from(
  { length: MAX_ACTIVITY_RETENTION_DAYS - MIN_ACTIVITY_RETENTION_DAYS + 1 },
  (_, i) => MIN_ACTIVITY_RETENTION_DAYS + i,
);

function formatIntervalInput(value?: number): string {
  return value && Number.isFinite(value) && value > 0 ? String(value) : "";
}

function normalizeListInput(value: string): string[] {
  return value
    .split(/[\n\r,，、;；]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function normalizeUniverseSymbols(value: string): string[] {
  return normalizeListInput(value);
}

function suggestCalendarProfile(market: string, exchange: string, assetClass: string): { calendarCode: string; timeZone: string } {
  const normalizedMarket = market.trim().toLowerCase();
  const normalizedExchange = exchange.trim().toUpperCase();
  const normalizedAssetClass = assetClass.trim().toLowerCase();

  if (normalizedExchange === "SSE") {
    return { calendarCode: "CN-SSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedExchange === "SZSE") {
    return { calendarCode: "CN-SZSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedExchange === "NYSE" || normalizedExchange === "XNYS") {
    return { calendarCode: "US-XNYS", timeZone: "America/New_York" };
  }
  if (normalizedExchange === "NASDAQ" || normalizedExchange === "XNAS") {
    return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
  }
  if (["CME", "CBOT", "COMEX", "NYMEX"].includes(normalizedExchange)) {
    return { calendarCode: "CME-INDEX", timeZone: "America/Chicago" };
  }
  if (["CFFEX", "SHFE", "DCE", "CZCE", "INE"].includes(normalizedExchange)) {
    return { calendarCode: normalizedExchange, timeZone: "Asia/Shanghai" };
  }
  if (normalizedMarket === "a_share") {
    return { calendarCode: "CN-SSE", timeZone: "Asia/Shanghai" };
  }
  if (normalizedMarket === "us_equity") {
    return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
  }
  if (normalizedMarket === "crypto" || normalizedAssetClass === "crypto") {
    return { calendarCode: "CRYPTO-24X7", timeZone: "UTC" };
  }
  if (normalizedMarket === "futures" || normalizedAssetClass === "futures") {
    return { calendarCode: "CME-INDEX", timeZone: "America/Chicago" };
  }
  return { calendarCode: "US-XNAS", timeZone: "America/New_York" };
}

function sameListInput(a: string, b: string): boolean {
  return JSON.stringify(normalizeListInput(a)) === JSON.stringify(normalizeListInput(b));
}

function normalizeTeamIntervalForRequest(value: string): number {
  const trimmed = value.trim();
  if (!trimmed) {
    return 0;
  }
  return Math.round(Number(trimmed));
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

function coverageTone(count: number): string {
  if (count >= 5) {
    return "text-emerald-600";
  }
  if (count >= 2) {
    return "text-amber-600";
  }
  return "text-gray-500";
}

const FundSettings: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const [fund, setFund] = useState<Fund | null>(null);
  const [form, setForm] = useState<FundForm | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveMessage, setSaveMessage] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            missingFundId: "Missing fundId",
            loadError: "Failed to load fund settings",
            saveError: "Failed to save fund settings",
            loading: "Loading fund settings...",
            empty: "No editable fund information is available.",
            title: "Fund settings",
            subtitle: "Maintain fund identity, trading mode, market profile, and operating cadence. Changes take effect immediately after saving.",
            retry: "Retry",
            nameRequired: "Fund name is required",
            invalidInitialCapital: "Initial capital must be a number greater than or equal to 0",
            intervalInvalid: "{label} interval must be a number greater than 0",
            saveSuccess: "Fund settings saved.",
            name: "Fund name",
            description: "Description",
            descriptionPlaceholder: "Add the fund objective, strategy, or intended use case.",
            tradingMode: "Trading mode",
            status: "Status",
            initialCapital: "Initial capital",
            market: "Market",
            exchange: "Exchange",
            exchangePlaceholder: "NASDAQ / SSE / SZSE / BINANCE / CME",
            assetClass: "Asset class",
            baseCurrency: "Base currency",
            baseCurrencyPlaceholder: "USD / CNY / USDT",
            benchmarkSymbol: "Benchmark symbol",
            benchmarkPlaceholder: "SPY / 000300 / BTCUSDT / ESU2026",
            primaryDirection: "Primary direction",
            calendarCode: "Calendar code",
            timeZone: "Time zone",
            universeMode: "Universe mode",
            universeSymbols: "Universe symbols",
            universeThemes: "Theme tags",
            universeSectors: "Sector tags",
            universeCustomFilters: "Custom filters",
            universePlaceholder: "AAPL, NVDA or BTCUSDT, ETHUSDT",
            universeThemesPlaceholder: "CPO, light modules, AI infra",
            universeSectorsPlaceholder: "technology, semiconductor, energy",
            universeCustomFiltersPlaceholder: "marketCap>10B, liquidity:high",
            specializationTitle: "Team specialization",
            specializationSubtitle: "Capture the fund team's long-term strengths and preferences. Runtime context and member selection use these signals as affinity boosts instead of hard constraints.",
            specializationMarkets: "Specialization markets",
            specializationAssetClasses: "Specialization asset classes",
            specializationThemes: "Specialization themes",
            specializationInstruments: "Specialization instruments",
            specializationStyleHints: "Specialization style hints",
            specializationPlaceholder: "CPO, us_equity, NVDA, growth, event-driven",
            intervalTitle: "Team operating intervals",
            intervalSubtitle: "Set analysis and check cadence per role in minutes. Leave blank to inherit the platform default.",
            intervalHint: "5-minute steps recommended",
            intervalPlaceholder: "Leave blank to inherit the platform default",
            hardRiskTitle: "Hard risk overrides",
            hardRiskSubtitle: "Per-fund overrides for the deterministic risk engine. Leave blank to inherit the platform default. Values outside the allowed range are dropped silently.",
            hardRiskMaxQuoteAge: "Max quote age (seconds)",
            hardRiskMaxQuoteAgeHint: "Stale-quote guard threshold for risk-increasing orders. Range 1-86400, platform default 900 (15 minutes).",
            hardRiskMaxQuoteAgePlaceholder: "Leave blank to inherit platform default (900s)",
            hardRiskMaxQuoteAgeInvalid: "Max quote age must be an integer between 1 and 86400 seconds",
            activityRetentionTitle: "Team Live Activity retention",
            activityRetentionSubtitle: "Controls how long the Team Live Activity timeline persists workflow events. Older events are purged daily by the retention sweep.",
            activityRetentionLabel: "Retain activity for",
            activityRetentionHint: "1 to 10 days. The default of 7 keeps roughly a trading week. Lower values save disk space on busy funds; higher values let you scroll further back through old workflow runs.",
            activityRetentionOption: (days: number) => (days === 1 ? "1 day" : `${days} days`),
            save: "Save settings",
            saving: "Saving...",
            discard: "Discard changes",
            overview: "Current overview",
            fundId: "Fund ID",
            companyId: "Company ID",
            currentCapital: "Current capital",
            totalAssets: "Total assets",
            nav: "NAV",
            marketProfile: "Market profile",
            benchmarkAndUniverse: "Benchmark / universe",
            teamIntervals: "Team intervals",
            marketCoverage: "Market data readiness",
            watchlistCoverage: "Tracked symbols",
            benchmarkCoverage: "Benchmark coverage",
            specializationCoverage: "Specialization signals",
            marketCoverageSubtitle: "These fields drive which symbols receive quote, research, and news snapshots across workflow and dashboards.",
            impactNotice: "Changing trading mode or status affects available actions and downstream workflow behavior. Validate the full flow in simulation or paper mode before using real capital.",
            notSet: "Not set",
            platformDefault: "Use platform default",
            tradingModes: {
              simulation: "Simulation",
              paper: "Paper",
              live: "Live",
            },
            statuses: {
              active: "Active",
              paused: "Paused",
              closed: "Closed",
            },
            markets: {
              us_equity: "US equity",
              a_share: "A-shares",
              crypto: "Crypto",
              futures: "Futures",
            },
            assetClasses: {
              equity: "Equity",
              crypto: "Crypto",
              futures: "Futures",
            },
            directions: {
              stocks: "Stocks",
              crypto: "Crypto",
              futures: "Futures",
              multi_asset: "Multi-asset",
            },
            universeModes: {
              manual: "Manual",
              watchlist: "Watchlist",
            },
            intervalRoles: {
              pm: { label: "Portfolio manager", description: "Controls portfolio-level decision cadence." },
              researcher: { label: "Researcher", description: "Controls research refresh and thesis cadence." },
              trader: { label: "Trader", description: "Controls trading analysis and execution checks." },
              risk: { label: "Risk", description: "Controls risk scans and constraint checks." },
            },
          }
        : {
            missingFundId: "缺少 fundId",
            loadError: "加载基金设置失败",
            saveError: "保存基金设置失败",
            loading: "正在加载基金设置...",
            empty: "当前没有可编辑的基金信息。",
            title: "基金设置",
            subtitle: "维护基金基础信息、交易模式、市场画像与运行节奏，保存后会立即同步到当前基金页面与后续策略流程。",
            retry: "重试",
            nameRequired: "基金名称不能为空",
            invalidInitialCapital: "初始资金必须是大于等于 0 的数字",
            intervalInvalid: "{label}间隔必须是大于 0 的数字",
            saveSuccess: "基金设置已保存。",
            name: "基金名称",
            description: "基金描述",
            descriptionPlaceholder: "补充这只基金的目标、策略或使用场景。",
            tradingMode: "交易模式",
            status: "运行状态",
            initialCapital: "初始资金规模",
            market: "市场",
            exchange: "交易所",
            exchangePlaceholder: "NASDAQ / SSE / SZSE / BINANCE / CME",
            assetClass: "资产类别",
            baseCurrency: "基础货币",
            baseCurrencyPlaceholder: "USD / CNY / USDT",
            benchmarkSymbol: "基准标的",
            benchmarkPlaceholder: "SPY / 000300 / BTCUSDT / ESU2026",
            primaryDirection: "主方向",
            calendarCode: "交易日历代码",
            timeZone: "时区",
            universeMode: "标的池模式",
            universeSymbols: "自选标的",
            universeThemes: "主题标签",
            universeSectors: "行业/板块",
            universeCustomFilters: "自定义筛选条件",
            universePlaceholder: "AAPL, NVDA 或 BTCUSDT, ETHUSDT",
            universeThemesPlaceholder: "CPO、光模块、AI infra",
            universeSectorsPlaceholder: "technology、semiconductor、energy",
            universeCustomFiltersPlaceholder: "marketCap>10B、liquidity:high",
            specializationTitle: "团队专精",
            specializationSubtitle: "描述这只基金团队长期积累的市场、资产、主题、标的与风格偏好。runtime 上下文与选人排序会把这些信号作为加分项，而不是硬限制。",
            specializationMarkets: "专精市场",
            specializationAssetClasses: "专精资产类别",
            specializationThemes: "专精主题",
            specializationInstruments: "专精标的",
            specializationStyleHints: "专精风格提示",
            specializationPlaceholder: "CPO、美股成长、NVDA、growth、event-driven",
            intervalTitle: "团队运作间隔",
            intervalSubtitle: "按角色设置执行分析与检查节奏，单位为分钟；留空时使用平台默认值。",
            intervalHint: "建议使用 5 分钟步长",
            intervalPlaceholder: "留空则继承平台默认值",
            hardRiskTitle: "硬风控覆盖",
            hardRiskSubtitle: "针对当前基金覆盖确定性风控引擎的阈值，留空则继承平台默认值；超出允许范围的取值会被自动丢弃。",
            hardRiskMaxQuoteAge: "最大行情过期时间（秒）",
            hardRiskMaxQuoteAgeHint: "用于 StaleQuoteGuard：超过该秒数的最新报价将拒绝任何加仓 / 开仓等增风险订单。取值范围 1~86400，平台默认 900（15 分钟）。",
            hardRiskMaxQuoteAgePlaceholder: "留空则继承平台默认值（900 秒）",
            hardRiskMaxQuoteAgeInvalid: "最大行情过期时间必须是 1~86400 秒之间的整数",
            activityRetentionTitle: "团队实时活动保留期",
            activityRetentionSubtitle: "决定团队实时活动面板能够回看多久的历史事件。超过保留期的事件会被每日清理任务删除。",
            activityRetentionLabel: "保留时长",
            activityRetentionHint: "可选 1~10 天，默认 7 天（约一个交易周）。设短一些可以减少磁盘占用；设长一些可让你下拉看到更久之前的工作流。",
            activityRetentionOption: (days: number) => (days === 1 ? "1 天" : `${days} 天`),
            save: "保存设置",
            saving: "保存中...",
            discard: "放弃修改",
            overview: "当前概览",
            fundId: "基金标识",
            companyId: "所属公司标识",
            currentCapital: "当前资金",
            totalAssets: "总资产",
            nav: "单位净值",
            marketProfile: "市场画像",
            benchmarkAndUniverse: "基准 / 标的池",
            teamIntervals: "团队运作间隔",
            marketCoverage: "市场数据就绪度",
            watchlistCoverage: "跟踪标的数",
            benchmarkCoverage: "基准覆盖",
            specializationCoverage: "专精信号",
            marketCoverageSubtitle: "这些字段会决定 workflow 和 Dashboard 中哪些标的能拿到行情、研究和资讯快照。",
            impactNotice: "更改交易模式和运行状态会影响后续操作入口与策略流程行为。涉及真实资金前，建议先在模拟或纸面模式下验证完整流程。",
            notSet: "未设置",
            platformDefault: "使用平台默认值",
            tradingModes: {
              simulation: "模拟",
              paper: "纸面",
              live: "实盘",
            },
            statuses: {
              active: "运行中",
              paused: "已暂停",
              closed: "已关闭",
            },
            markets: {
              us_equity: "美股",
              a_share: "A股",
              crypto: "Crypto",
              futures: "期货",
            },
            assetClasses: {
              equity: "股票",
              crypto: "Crypto",
              futures: "期货",
            },
            directions: {
              stocks: "股票",
              crypto: "Crypto",
              futures: "期货",
              multi_asset: "多资产",
            },
            universeModes: {
              manual: "手动",
              watchlist: "观察列表",
            },
            intervalRoles: {
              pm: { label: "组合经理", description: "控制组合层决策节奏。" },
              researcher: { label: "研究员", description: "控制研究与观点刷新的频率。" },
              trader: { label: "交易员", description: "控制交易员分析与执行检查的频率。" },
              risk: { label: "风控", description: "控制风控扫描与约束检查频率。" },
            },
          },
    [language],
  );

  const intervalFields = useMemo(
    () => intervalFieldKeys.map((field) => ({ ...field, ...copy.intervalRoles[field.role] })),
    [copy],
  );

  const tradingModeLabel = useCallback(
    (value: string) => copy.tradingModes[value as keyof typeof copy.tradingModes] ?? humanizeValue(value, copy.notSet),
    [copy],
  );
  const statusLabel = useCallback(
    (value: string) => copy.statuses[value as keyof typeof copy.statuses] ?? humanizeValue(value, copy.notSet),
    [copy],
  );
  const marketLabel = useCallback(
    (value?: string) => (value ? copy.markets[value as keyof typeof copy.markets] ?? humanizeValue(value, copy.notSet) : copy.notSet),
    [copy],
  );
  const assetClassLabel = useCallback(
    (value?: string) => (value ? copy.assetClasses[value as keyof typeof copy.assetClasses] ?? humanizeValue(value, copy.notSet) : copy.notSet),
    [copy],
  );
  const directionLabel = useCallback(
    (value?: string) => (value ? copy.directions[value as keyof typeof copy.directions] ?? humanizeValue(value, copy.notSet) : copy.notSet),
    [copy],
  );
  const universeModeLabel = useCallback(
    (value?: string) => (value ? copy.universeModes[value as keyof typeof copy.universeModes] ?? humanizeValue(value, copy.notSet) : copy.notSet),
    [copy],
  );

  const formatTeamIntervalSummary = useCallback(
    (intervals?: FundTeamIntervals): string => {
      if (!intervals) {
        return copy.platformDefault;
      }
      const items = [
        intervals.pm ? `${copy.intervalRoles.pm.label} ${formatNumberForLanguage(intervals.pm, language)} min` : "",
        intervals.researcher ? `${copy.intervalRoles.researcher.label} ${formatNumberForLanguage(intervals.researcher, language)} min` : "",
        intervals.trader ? `${copy.intervalRoles.trader.label} ${formatNumberForLanguage(intervals.trader, language)} min` : "",
        intervals.risk ? `${copy.intervalRoles.risk.label} ${formatNumberForLanguage(intervals.risk, language)} min` : "",
      ].filter(Boolean);
      if (items.length === 0) {
        return copy.platformDefault;
      }
      return language === "en-US" ? items.join(" · ") : items.join(" · ").replace(/ min/g, " 分钟");
    },
    [copy, language],
  );

  const loadFund = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const response = await apiGet<Fund>(`/api/funds/${fundId}`);
      setFund(response);
      setForm(buildForm(response));
    } catch (err) {
      setError(formatApiError(err, copy.loadError));
    } finally {
      setLoading(false);
    }
  }, [copy.loadError, copy.missingFundId, fundId]);

  useEffect(() => {
    void loadFund();
  }, [loadFund]);

  const hasChanges = useMemo(() => {
    if (!fund || !form) {
      return false;
    }
    const normalizedInitialCapital = Number(form.initialCapital || 0);
    return (
      form.name.trim() !== fund.name ||
      form.description.trim() !== (fund.description?.trim() ?? "") ||
      form.tradingMode !== fund.tradingMode ||
      form.status !== fund.status ||
      normalizedInitialCapital !== fund.initialCapital ||
      form.market.trim() !== (fund.market ?? "") ||
      form.exchange.trim() !== (fund.exchange ?? "") ||
      form.assetClass.trim() !== (fund.assetClass ?? "") ||
      form.baseCurrency.trim() !== (fund.baseCurrency ?? "USD") ||
      form.benchmarkSymbol.trim() !== (fund.benchmarkSymbol ?? "") ||
      form.primaryDirection.trim() !== (fund.primaryDirection ?? "") ||
      form.calendarCode.trim() !== (fund.calendarCode ?? "") ||
      form.timeZone.trim() !== (fund.timeZone ?? "") ||
      form.universeMode.trim() !== (fund.universe?.mode ?? "manual") ||
      !sameListInput(form.universeSymbols, (fund.universe?.symbols ?? []).join(", ")) ||
      !sameListInput(form.universeThemes, (fund.universe?.themes ?? []).join(", ")) ||
      !sameListInput(form.universeSectors, (fund.universe?.sectors ?? []).join(", ")) ||
      !sameListInput(form.universeCustomFilters, (fund.universe?.customFilters ?? []).join(", ")) ||
      !sameListInput(form.specializationMarkets, (fund.specialization?.team?.markets ?? []).join(", ")) ||
      !sameListInput(form.specializationAssetClasses, (fund.specialization?.team?.assetClasses ?? []).join(", ")) ||
      !sameListInput(form.specializationThemes, (fund.specialization?.team?.themes ?? []).join(", ")) ||
      !sameListInput(form.specializationInstruments, (fund.specialization?.team?.instruments ?? []).join(", ")) ||
      !sameListInput(form.specializationStyleHints, (fund.specialization?.team?.styleHints ?? []).join(", ")) ||
      form.pmInterval.trim() !== formatIntervalInput(fund.teamIntervals?.pm) ||
      form.researcherInterval.trim() !== formatIntervalInput(fund.teamIntervals?.researcher) ||
      form.traderInterval.trim() !== formatIntervalInput(fund.teamIntervals?.trader) ||
      form.riskInterval.trim() !== formatIntervalInput(fund.teamIntervals?.risk)
    );
  }, [form, fund]);

  const handleSave = useCallback(async () => {
    if (!fundId || !fund || !form) {
      return;
    }

    const trimmedName = form.name.trim();
    if (!trimmedName) {
      setSaveError(copy.nameRequired);
      return;
    }

    const nextInitialCapital = Number(form.initialCapital);
    if (!Number.isFinite(nextInitialCapital) || nextInitialCapital < 0) {
      setSaveError(copy.invalidInitialCapital);
      return;
    }

    for (const field of intervalFields) {
      const rawValue = form[field.key].trim();
      if (!rawValue) {
        continue;
      }
      const minutes = Number(rawValue);
      if (!Number.isFinite(minutes) || minutes <= 0) {
        setSaveError(copy.intervalInvalid.replace("{label}", field.label));
        return;
      }
    }

    const trimmedMaxQuoteAge = form.hardRiskMaxQuoteAgeSeconds.trim();
    // Clear sentinel: 0 lets the backend drop the override and fall back to the platform default.
    let nextMaxQuoteAgeSeconds = 0;
    if (trimmedMaxQuoteAge) {
      const parsed = Number(trimmedMaxQuoteAge);
      if (!Number.isFinite(parsed) || parsed <= 0 || parsed > 86400 || !Number.isInteger(parsed)) {
        setSaveError(copy.hardRiskMaxQuoteAgeInvalid);
        return;
      }
      nextMaxQuoteAgeSeconds = parsed;
    }

    // Activity retention is a strict integer in [MIN..MAX] days. The
    // dropdown only ever produces valid values, but we still validate
    // here to defend against manual DOM tampering / external callers.
    const retentionRaw = Number(form.activityRetentionDays);
    let nextActivityRetentionDays = DEFAULT_ACTIVITY_RETENTION_DAYS;
    if (Number.isFinite(retentionRaw) && Number.isInteger(retentionRaw)) {
      if (retentionRaw < MIN_ACTIVITY_RETENTION_DAYS) {
        nextActivityRetentionDays = MIN_ACTIVITY_RETENTION_DAYS;
      } else if (retentionRaw > MAX_ACTIVITY_RETENTION_DAYS) {
        nextActivityRetentionDays = MAX_ACTIVITY_RETENTION_DAYS;
      } else {
        nextActivityRetentionDays = retentionRaw;
      }
    }

    setSaving(true);
    setSaveError(null);
    setSaveMessage(null);
    try {
      const updated = await apiPut<Fund>(`/api/funds/${fundId}`, {
        name: trimmedName,
        description: form.description.trim(),
        tradingMode: form.tradingMode,
        status: form.status,
        initialCapital: nextInitialCapital,
        market: form.market.trim(),
        exchange: form.exchange.trim(),
        assetClass: form.assetClass.trim(),
        baseCurrency: form.baseCurrency.trim(),
        benchmarkSymbol: form.benchmarkSymbol.trim(),
        primaryDirection: form.primaryDirection.trim(),
        calendarCode: form.calendarCode.trim(),
        timeZone: form.timeZone.trim(),
        universe: {
          mode: form.universeMode.trim() || "manual",
          symbols: normalizeUniverseSymbols(form.universeSymbols),
          themes: normalizeListInput(form.universeThemes),
          sectors: normalizeListInput(form.universeSectors),
          customFilters: normalizeListInput(form.universeCustomFilters),
        },
        specialization: {
          team: {
            markets: normalizeListInput(form.specializationMarkets),
            assetClasses: normalizeListInput(form.specializationAssetClasses),
            themes: normalizeListInput(form.specializationThemes),
            instruments: normalizeListInput(form.specializationInstruments),
            styleHints: normalizeListInput(form.specializationStyleHints),
          },
        },
        teamIntervals: {
          pm: normalizeTeamIntervalForRequest(form.pmInterval),
          researcher: normalizeTeamIntervalForRequest(form.researcherInterval),
          trader: normalizeTeamIntervalForRequest(form.traderInterval),
          risk: normalizeTeamIntervalForRequest(form.riskInterval),
        },
        hardRisk: {
          maxQuoteAgeSeconds: nextMaxQuoteAgeSeconds,
        },
        activityRetentionDays: nextActivityRetentionDays,
      });
      setFund(updated);
      setForm(buildForm(updated));
      setSaveMessage(copy.saveSuccess);
    } catch (err) {
      setSaveError(formatApiError(err, copy.saveError));
    } finally {
      setSaving(false);
    }
  }, [copy.hardRiskMaxQuoteAgeInvalid, copy.intervalInvalid, copy.invalidInitialCapital, copy.nameRequired, copy.saveError, copy.saveSuccess, form, fund, fundId, intervalFields]);

  if (loading) {
    return <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadFund()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  if (!fund || !form) {
    return <div className="rounded-xl border border-dashed border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.empty}</div>;
  }

  const marketProfile = [
    marketLabel(fund.market),
    fund.exchange || copy.notSet,
    assetClassLabel(fund.assetClass),
    fund.calendarCode || copy.notSet,
    fund.timeZone || copy.notSet,
  ]
    .filter((value, index, array) => !(index !== 1 && value === copy.notSet && array.some((item) => item !== copy.notSet)))
    .join(" · ");
  const benchmarkSummary = `${fund.benchmarkSymbol || copy.notSet}${fund.universe?.symbols?.length ? ` · ${fund.universe.symbols.join(", ")}` : ""}`;
  const themeSummary = fund.universe?.themes?.length ? fund.universe.themes.join(", ") : copy.notSet;
  const sectorSummary = fund.universe?.sectors?.length ? fund.universe.sectors.join(", ") : copy.notSet;
  const specializationSummaryItems = [
    { label: copy.specializationMarkets, value: fund.specialization?.team?.markets?.join(", ") },
    { label: copy.specializationAssetClasses, value: fund.specialization?.team?.assetClasses?.join(", ") },
    { label: copy.specializationThemes, value: fund.specialization?.team?.themes?.join(", ") },
    { label: copy.specializationInstruments, value: fund.specialization?.team?.instruments?.join(", ") },
    { label: copy.specializationStyleHints, value: fund.specialization?.team?.styleHints?.join(", ") },
  ].filter((item) => item.value);
  const universeSymbolCount = fund.universe?.symbols?.length ?? 0;
  const benchmarkCount = fund.benchmarkSymbol?.trim() ? 1 : 0;
  const specializationSignalCount = specializationSummaryItems.length;

  return (
    <div className="space-y-6">
      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
      </div>

      {saveError ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{saveError}</div> : null}
      {saveMessage ? <div className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-700">{saveMessage}</div> : null}

      <div className="grid grid-cols-1 gap-6 xl:grid-cols-[minmax(0,1fr)_320px]">
        <section className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
          <div className="grid grid-cols-1 gap-5 md:grid-cols-2">
            <label className="md:col-span-2">
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.name}</span>
              <input
                type="text"
                value={form.name}
                onChange={(event) => setForm((current) => (current ? { ...current, name: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label className="md:col-span-2">
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.description}</span>
              <textarea
                value={form.description}
                onChange={(event) => setForm((current) => (current ? { ...current, description: event.target.value } : current))}
                rows={4}
                placeholder={copy.descriptionPlaceholder}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.tradingMode}</span>
              <select
                value={form.tradingMode}
                onChange={(event) => setForm((current) => (current ? { ...current, tradingMode: event.target.value as TradingMode } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="simulation">{copy.tradingModes.simulation}</option>
                <option value="paper">{copy.tradingModes.paper}</option>
                <option value="live">{copy.tradingModes.live}</option>
              </select>
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.status}</span>
              <select
                value={form.status}
                onChange={(event) => setForm((current) => (current ? { ...current, status: event.target.value as FundStatus } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="active">{copy.statuses.active}</option>
                <option value="paused">{copy.statuses.paused}</option>
                <option value="closed">{copy.statuses.closed}</option>
              </select>
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.initialCapital}</span>
              <input
                type="number"
                min="0"
                step="0.01"
                value={form.initialCapital}
                onChange={(event) => setForm((current) => (current ? { ...current, initialCapital: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.market}</span>
              <select
                value={form.market}
                onChange={(event) =>
                  setForm((current) => {
                    if (!current) {
                      return current;
                    }
                    const nextMarket = event.target.value;
                    const suggestion = suggestCalendarProfile(nextMarket, current.exchange, current.assetClass);
                    return { ...current, market: nextMarket, calendarCode: suggestion.calendarCode, timeZone: suggestion.timeZone };
                  })
                }
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="">{copy.notSet}</option>
                <option value="us_equity">{copy.markets.us_equity}</option>
                <option value="a_share">{copy.markets.a_share}</option>
                <option value="crypto">{copy.markets.crypto}</option>
                <option value="futures">{copy.markets.futures}</option>
              </select>
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.exchange}</span>
              <input
                type="text"
                value={form.exchange}
                onChange={(event) =>
                  setForm((current) => {
                    if (!current) {
                      return current;
                    }
                    const nextExchange = event.target.value;
                    const suggestion = suggestCalendarProfile(current.market, nextExchange, current.assetClass);
                    return { ...current, exchange: nextExchange, calendarCode: suggestion.calendarCode, timeZone: suggestion.timeZone };
                  })
                }
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                placeholder={copy.exchangePlaceholder}
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.assetClass}</span>
              <select
                value={form.assetClass}
                onChange={(event) =>
                  setForm((current) => {
                    if (!current) {
                      return current;
                    }
                    const nextAssetClass = event.target.value;
                    const suggestion = suggestCalendarProfile(current.market, current.exchange, nextAssetClass);
                    return { ...current, assetClass: nextAssetClass, calendarCode: suggestion.calendarCode, timeZone: suggestion.timeZone };
                  })
                }
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="">{copy.notSet}</option>
                <option value="equity">{copy.assetClasses.equity}</option>
                <option value="crypto">{copy.assetClasses.crypto}</option>
                <option value="futures">{copy.assetClasses.futures}</option>
              </select>
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.baseCurrency}</span>
              <input
                type="text"
                value={form.baseCurrency}
                onChange={(event) => setForm((current) => (current ? { ...current, baseCurrency: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                placeholder={copy.baseCurrencyPlaceholder}
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.benchmarkSymbol}</span>
              <input
                type="text"
                value={form.benchmarkSymbol}
                onChange={(event) => setForm((current) => (current ? { ...current, benchmarkSymbol: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                placeholder={copy.benchmarkPlaceholder}
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.primaryDirection}</span>
              <select
                value={form.primaryDirection}
                onChange={(event) => setForm((current) => (current ? { ...current, primaryDirection: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="">{copy.notSet}</option>
                <option value="stocks">{copy.directions.stocks}</option>
                <option value="crypto">{copy.directions.crypto}</option>
                <option value="futures">{copy.directions.futures}</option>
                <option value="multi_asset">{copy.directions.multi_asset}</option>
              </select>
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.calendarCode}</span>
              <input
                type="text"
                value={form.calendarCode}
                onChange={(event) => setForm((current) => (current ? { ...current, calendarCode: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                placeholder="US-XNAS / CN-SSE / CRYPTO-24X7"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.timeZone}</span>
              <input
                type="text"
                value={form.timeZone}
                onChange={(event) => setForm((current) => (current ? { ...current, timeZone: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                placeholder="America/New_York / Asia/Shanghai / UTC"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.universeMode}</span>
              <select
                value={form.universeMode}
                onChange={(event) => setForm((current) => (current ? { ...current, universeMode: event.target.value } : current))}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              >
                <option value="manual">{copy.universeModes.manual}</option>
                <option value="watchlist">{copy.universeModes.watchlist}</option>
              </select>
            </label>

            <label className="md:col-span-2">
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.universeSymbols}</span>
              <textarea
                aria-label={copy.universeSymbols}
                value={form.universeSymbols}
                onChange={(event) => setForm((current) => (current ? { ...current, universeSymbols: event.target.value } : current))}
                rows={3}
                placeholder={copy.universePlaceholder}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.universeThemes}</span>
              <textarea
                aria-label={copy.universeThemes}
                value={form.universeThemes}
                onChange={(event) => setForm((current) => (current ? { ...current, universeThemes: event.target.value } : current))}
                rows={3}
                placeholder={copy.universeThemesPlaceholder}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label>
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.universeSectors}</span>
              <textarea
                aria-label={copy.universeSectors}
                value={form.universeSectors}
                onChange={(event) => setForm((current) => (current ? { ...current, universeSectors: event.target.value } : current))}
                rows={3}
                placeholder={copy.universeSectorsPlaceholder}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <label className="md:col-span-2">
              <span className="mb-2 block text-sm font-medium text-gray-700">{copy.universeCustomFilters}</span>
              <textarea
                aria-label={copy.universeCustomFilters}
                value={form.universeCustomFilters}
                onChange={(event) => setForm((current) => (current ? { ...current, universeCustomFilters: event.target.value } : current))}
                rows={2}
                placeholder={copy.universeCustomFiltersPlaceholder}
                className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
              />
            </label>

            <div className="md:col-span-2 rounded-2xl border border-violet-100 bg-violet-50/60 p-5">
              <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                <div>
                  <h2 className="text-base font-semibold text-violet-900">{copy.specializationTitle}</h2>
                  <p className="mt-1 text-sm text-violet-700">{copy.specializationSubtitle}</p>
                </div>
              </div>
              <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationMarkets}</span>
                  <textarea
                    aria-label={copy.specializationMarkets}
                    value={form.specializationMarkets}
                    onChange={(event) => setForm((current) => (current ? { ...current, specializationMarkets: event.target.value } : current))}
                    rows={2}
                    placeholder={copy.specializationPlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                  />
                </label>
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationAssetClasses}</span>
                  <textarea
                    aria-label={copy.specializationAssetClasses}
                    value={form.specializationAssetClasses}
                    onChange={(event) => setForm((current) => (current ? { ...current, specializationAssetClasses: event.target.value } : current))}
                    rows={2}
                    placeholder={copy.specializationPlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                  />
                </label>
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationThemes}</span>
                  <textarea
                    aria-label={copy.specializationThemes}
                    value={form.specializationThemes}
                    onChange={(event) => setForm((current) => (current ? { ...current, specializationThemes: event.target.value } : current))}
                    rows={2}
                    placeholder={copy.specializationPlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                  />
                </label>
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationInstruments}</span>
                  <textarea
                    aria-label={copy.specializationInstruments}
                    value={form.specializationInstruments}
                    onChange={(event) => setForm((current) => (current ? { ...current, specializationInstruments: event.target.value } : current))}
                    rows={2}
                    placeholder={copy.specializationPlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                  />
                </label>
                <label className="md:col-span-2">
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.specializationStyleHints}</span>
                  <textarea
                    aria-label={copy.specializationStyleHints}
                    value={form.specializationStyleHints}
                    onChange={(event) => setForm((current) => (current ? { ...current, specializationStyleHints: event.target.value } : current))}
                    rows={2}
                    placeholder={copy.specializationPlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                  />
                </label>
              </div>
            </div>

            <div className="md:col-span-2 rounded-2xl border border-indigo-100 bg-indigo-50/60 p-5">
              <div className="flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
                <div>
                  <h2 className="text-base font-semibold text-indigo-900">{copy.intervalTitle}</h2>
                  <p className="mt-1 text-sm text-indigo-700">{copy.intervalSubtitle}</p>
                </div>
                <span className="rounded-full bg-white px-3 py-1 text-xs font-medium text-indigo-700">{copy.intervalHint}</span>
              </div>
              <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                {intervalFields.map((field) => (
                  <label key={field.key}>
                    <span className="mb-2 block text-sm font-medium text-gray-700">{field.label}</span>
                    <input
                      type="number"
                      min="0"
                      step="5"
                      inputMode="numeric"
                      value={form[field.key]}
                      onChange={(event) => setForm((current) => (current ? { ...current, [field.key]: event.target.value } : current))}
                      placeholder={copy.intervalPlaceholder}
                      className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-indigo-500"
                    />
                    <span className="mt-2 block text-xs leading-5 text-gray-500">{field.description}</span>
                  </label>
                ))}
              </div>
            </div>

            <div className="md:col-span-2 rounded-2xl border border-amber-200 bg-amber-50/70 p-5">
              <div>
                <h2 className="text-base font-semibold text-amber-900">{copy.hardRiskTitle}</h2>
                <p className="mt-1 text-sm text-amber-800">{copy.hardRiskSubtitle}</p>
              </div>
              <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.hardRiskMaxQuoteAge}</span>
                  <input
                    type="number"
                    min="0"
                    max="86400"
                    step="1"
                    inputMode="numeric"
                    value={form.hardRiskMaxQuoteAgeSeconds}
                    onChange={(event) =>
                      setForm((current) => (current ? { ...current, hardRiskMaxQuoteAgeSeconds: event.target.value } : current))
                    }
                    placeholder={copy.hardRiskMaxQuoteAgePlaceholder}
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-amber-500"
                  />
                  <span className="mt-2 block text-xs leading-5 text-gray-600">{copy.hardRiskMaxQuoteAgeHint}</span>
                </label>
              </div>
            </div>

            <div className="md:col-span-2 rounded-2xl border border-sky-200 bg-sky-50/70 p-5">
              <div>
                <h2 className="text-base font-semibold text-sky-900">{copy.activityRetentionTitle}</h2>
                <p className="mt-1 text-sm text-sky-800">{copy.activityRetentionSubtitle}</p>
              </div>
              <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                <label>
                  <span className="mb-2 block text-sm font-medium text-gray-700">{copy.activityRetentionLabel}</span>
                  <select
                    value={form.activityRetentionDays}
                    onChange={(event) =>
                      setForm((current) => (current ? { ...current, activityRetentionDays: event.target.value } : current))
                    }
                    className="w-full rounded-xl border border-gray-300 bg-white px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-sky-500"
                  >
                    {ACTIVITY_RETENTION_OPTIONS.map((value) => (
                      <option key={value} value={String(value)}>
                        {copy.activityRetentionOption(value)}
                      </option>
                    ))}
                  </select>
                  <span className="mt-2 block text-xs leading-5 text-gray-600">{copy.activityRetentionHint}</span>
                </label>
              </div>
            </div>
          </div>

          <div className="mt-6 flex flex-col gap-3 sm:flex-row">
            <button
              onClick={() => void handleSave()}
              disabled={saving || !hasChanges}
              className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {saving ? copy.saving : copy.save}
            </button>
            <button
              onClick={() => {
                setForm(buildForm(fund));
                setSaveError(null);
                setSaveMessage(null);
              }}
              disabled={saving || !hasChanges}
              className="rounded-lg border border-gray-300 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {copy.discard}
            </button>
          </div>
        </section>

        <aside className="space-y-4">
          <div className="rounded-2xl border border-indigo-100 bg-indigo-50/60 p-6 shadow-sm">
            <h2 className="text-sm font-semibold uppercase tracking-wider text-indigo-700">{copy.marketCoverage}</h2>
            <p className="mt-2 text-sm text-indigo-700">{copy.marketCoverageSubtitle}</p>
            <div className="mt-4 grid grid-cols-1 gap-3">
              <div className="rounded-xl bg-white px-4 py-4">
                <p className="text-xs text-gray-500">{copy.watchlistCoverage}</p>
                <p className={`mt-1 text-2xl font-semibold ${coverageTone(universeSymbolCount)}`}>{formatNumberForLanguage(universeSymbolCount, language)}</p>
              </div>
              <div className="rounded-xl bg-white px-4 py-4">
                <p className="text-xs text-gray-500">{copy.benchmarkCoverage}</p>
                <p className={`mt-1 text-2xl font-semibold ${coverageTone(benchmarkCount)}`}>{formatNumberForLanguage(benchmarkCount, language)}</p>
              </div>
              <div className="rounded-xl bg-white px-4 py-4">
                <p className="text-xs text-gray-500">{copy.specializationCoverage}</p>
                <p className={`mt-1 text-2xl font-semibold ${coverageTone(specializationSignalCount)}`}>{formatNumberForLanguage(specializationSignalCount, language)}</p>
              </div>
            </div>
          </div>

          <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
            <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.overview}</h2>
            <dl className="mt-4 space-y-4 text-sm">
              <div>
                <dt className="text-gray-500">{copy.fundId}</dt>
                <dd className="mt-1 break-all font-medium text-gray-900">{fund.id}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.companyId}</dt>
                <dd className="mt-1 break-all font-medium text-gray-900">{fund.companyId}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.currentCapital}</dt>
                <dd className="mt-1 font-medium text-gray-900">{formatMoneyForDisplay(fund.currentCapital, fund.baseCurrency, displayCurrency, language)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.totalAssets}</dt>
                <dd className="mt-1 font-medium text-gray-900">{formatMoneyForDisplay(fund.totalAssets, fund.baseCurrency, displayCurrency, language)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.nav}</dt>
                <dd className="mt-1 font-medium text-gray-900">{formatNumberForLanguage(fund.nav, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 })}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.tradingMode}</dt>
                <dd className="mt-1 font-medium text-gray-900">{tradingModeLabel(fund.tradingMode)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.status}</dt>
                <dd className="mt-1 font-medium text-gray-900">{statusLabel(fund.status)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.marketProfile}</dt>
                <dd className="mt-1 font-medium text-gray-900">{marketProfile}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.primaryDirection}</dt>
                <dd className="mt-1 font-medium text-gray-900">{directionLabel(fund.primaryDirection)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.calendarCode}</dt>
                <dd className="mt-1 font-medium text-gray-900">{fund.calendarCode || copy.notSet}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.timeZone}</dt>
                <dd className="mt-1 font-medium text-gray-900">{fund.timeZone || copy.notSet}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.universeMode}</dt>
                <dd className="mt-1 font-medium text-gray-900">{universeModeLabel(fund.universe?.mode)}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.benchmarkAndUniverse}</dt>
                <dd className="mt-1 font-medium text-gray-900">{benchmarkSummary}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.universeThemes}</dt>
                <dd className="mt-1 font-medium text-gray-900">{themeSummary}</dd>
              </div>
              <div>
                <dt className="text-gray-500">{copy.universeSectors}</dt>
                <dd className="mt-1 font-medium text-gray-900">{sectorSummary}</dd>
              </div>
              {specializationSummaryItems.length > 0 ? (
                <div>
                  <dt className="text-gray-500">{copy.specializationTitle}</dt>
                  <dd className="mt-2 space-y-2">
                    {specializationSummaryItems.map((item) => (
                      <div key={item.label}>
                        <p className="text-xs text-gray-500">{item.label}</p>
                        <p className="mt-1 font-medium text-gray-900">{item.value}</p>
                      </div>
                    ))}
                  </dd>
                </div>
              ) : null}
              <div>
                <dt className="text-gray-500">{copy.teamIntervals}</dt>
                <dd className="mt-1 font-medium text-gray-900">{formatTeamIntervalSummary(fund.teamIntervals)}</dd>
              </div>
            </dl>
          </div>

          <div className="rounded-2xl border border-dashed border-gray-200 bg-white p-6 text-sm text-gray-500 shadow-sm">
            {copy.impactNotice}
          </div>
        </aside>
      </div>

      {fundId ? (
        <BrokerLinksSection
          fundId={fundId}
          language={language}
          defaultExpanded={fund.tradingMode === "live"}
        />
      ) : null}
      {fundId ? (
        <FundBaseCurrencySection
          fundId={fundId}
          currentBaseCurrency={fund.baseCurrency || "USD"}
          language={language}
          onSaved={(next) => setFund((f) => (f ? { ...f, baseCurrency: next } : f))}
        />
      ) : null}
      {fundId ? (
        <FundingSection
          fundId={fundId}
          language={language}
          defaultExpanded={fund.tradingMode === "live"}
        />
      ) : null}
      {fundId ? (
        <FundLLMOverridesSection fundId={fundId} language={language} />
      ) : null}
    </div>
  );
};

export default FundSettings;
