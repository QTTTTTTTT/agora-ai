import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { apiDelete, apiGet, apiPost, apiPut, formatApiError } from "../lib/api";
import {
  formatDateForLanguage,
  formatDateTimeForLanguage,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";
import TeamActivityPanel from "../components/TeamActivityPanel";

type AgentRole = "pm" | "researcher" | "trader" | "risk";
type ResearchFocus = "stock" | "fundamental" | "macro";
type CoverageDirection = "stocks" | "funds" | "crypto" | "futures" | "multi_asset";
type CoverageMarket = "us_equity" | "a_share" | "crypto" | "futures";
type CoverageAssetClass = "equity" | "fund" | "crypto" | "futures";

interface AgentCoverage {
  markets?: string[];
  assetClasses?: string[];
  directions?: string[];
}

interface AgentSpecialization {
  markets?: string[];
  assetClasses?: string[];
  themes?: string[];
  instruments?: string[];
  styleHints?: string[];
  patterns?: string[];
}

interface DomainConfigShape {
  coverage?: AgentCoverage;
  specialization?: AgentSpecialization;
  [key: string]: unknown;
}

interface TeamAgent {
  id: string;
  agentId?: string;
  name?: string;
  role: AgentRole;
  focus?: string;
  llmModel?: string;
  modelProvider?: string;
  modelName?: string;
  modelBaseURL?: string;
  hasCustomModelConfig?: boolean;
  systemPrompt?: string;
  skillConfig?: unknown;
  domainConfig?: unknown;
  evolutionConfig?: unknown;
  latestLearningSummary?: string;
  latestLearningAt?: string;
  latestLearningReturn?: number;
  latestLearningTags?: string[];
  status?: string;
  joinedAt?: string;
  fundId?: string;
  bindStatus?: string;
}

interface AgentEditorState {
  role: AgentRole;
  focus: string;
  coverageMarkets: CoverageMarket[];
  coverageAssetClasses: CoverageAssetClass[];
  coverageDirections: CoverageDirection[];
  specializationMarkets: string;
  specializationAssetClasses: string;
  specializationThemes: string;
  specializationInstruments: string;
  specializationStyleHints: string;
  specializationPatterns: string;
  systemPrompt: string;
  skillConfig: string;
  domainConfig: string;
  evolutionConfig: string;
  modelProvider: string;
  modelName: string;
  modelBaseURL: string;
  apiKey: string;
  modelConfigDirty: boolean;
}

interface ConnectionTestResult {
  success: boolean;
  latency_ms: number;
  message: string;
}

const providerOptions = [
  { value: "openai", defaultLabel: "OpenAI" },
  { value: "claude", defaultLabel: "Anthropic" },
  { value: "deepseek", defaultLabel: "DeepSeek" },
  { value: "qwen", defaultLabel: "Qwen" },
  { value: "custom", defaultLabel: "Custom" },
] as const;

function parseDomainConfig(value: unknown): DomainConfigShape {
  if (!value || typeof value !== "object" || Array.isArray(value)) {
    return {};
  }
  return value as DomainConfigShape;
}

function extractCoverage(value: unknown): AgentCoverage {
  return parseDomainConfig(value).coverage ?? {};
}

function extractSpecialization(value: unknown): AgentSpecialization {
  return parseDomainConfig(value).specialization ?? {};
}

function normalizeListInput(value: string): string[] {
  return value
    .split(/[\n\r,，、;；]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

function stringifyJSON(value: unknown): string {
  if (value === null || value === undefined || value === "") {
    return "{}";
  }
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (!trimmed) {
      return "{}";
    }
    try {
      return JSON.stringify(JSON.parse(trimmed), null, 2);
    } catch {
      return trimmed;
    }
  }
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return "{}";
  }
}

function parseJSONInput(label: string, value: string): unknown {
  const trimmed = value.trim();
  if (!trimmed) {
    return {};
  }
  try {
    return JSON.parse(trimmed);
  } catch {
    throw new Error(label);
  }
}

function buildEditorState(agent: TeamAgent | null): AgentEditorState {
  const coverage = extractCoverage(agent?.domainConfig);
  const specialization = extractSpecialization(agent?.domainConfig);
  return {
    role: agent?.role ?? "researcher",
    focus: agent?.focus ?? "",
    coverageMarkets: (coverage.markets ?? []) as CoverageMarket[],
    coverageAssetClasses: (coverage.assetClasses ?? []) as CoverageAssetClass[],
    coverageDirections: (coverage.directions ?? []) as CoverageDirection[],
    specializationMarkets: (specialization.markets ?? []).join(", "),
    specializationAssetClasses: (specialization.assetClasses ?? []).join(", "),
    specializationThemes: (specialization.themes ?? []).join(", "),
    specializationInstruments: (specialization.instruments ?? []).join(", "),
    specializationStyleHints: (specialization.styleHints ?? []).join(", "),
    specializationPatterns: (specialization.patterns ?? []).join(", "),
    systemPrompt: agent?.systemPrompt ?? "",
    skillConfig: stringifyJSON(agent?.skillConfig),
    domainConfig: stringifyJSON(agent?.domainConfig),
    evolutionConfig: stringifyJSON(agent?.evolutionConfig),
    modelProvider: agent?.modelProvider ?? "openai",
    modelName: agent?.modelName ?? agent?.llmModel ?? "",
    modelBaseURL: agent?.modelBaseURL ?? "",
    apiKey: "",
    modelConfigDirty: false,
  };
}

const TeamManagement: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language } = useAppPreferences();
  const [agents, setAgents] = useState<TeamAgent[]>([]);
  const [ownedInventory, setOwnedInventory] = useState<TeamAgent[]>([]);
  const [loading, setLoading] = useState(true);
  const [loadingInventory, setLoadingInventory] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedAgentId, setSelectedAgentId] = useState<string | null>(null);
  const [filterRole, setFilterRole] = useState<AgentRole | "all">("all");
  const [showHireModal, setShowHireModal] = useState(false);
  const [saving, setSaving] = useState(false);
  const [bindingAgentId, setBindingAgentId] = useState<string | null>(null);
  const [saveError, setSaveError] = useState<string | null>(null);
  const [saveSuccess, setSaveSuccess] = useState<string | null>(null);
  const [fireTarget, setFireTarget] = useState<TeamAgent | null>(null);
  const [firing, setFiring] = useState(false);
  const [testingConnection, setTestingConnection] = useState(false);
  const [testResult, setTestResult] = useState<string | null>(null);
  const [testSucceeded, setTestSucceeded] = useState<boolean | null>(null);
  const [editor, setEditor] = useState<AgentEditorState>(buildEditorState(null));
  const previousSelectedAgentIdRef = useRef<string | null>(null);
  const [hireRole, setHireRole] = useState<AgentRole>("researcher");
  const [hireFocus, setHireFocus] = useState<ResearchFocus>("stock");

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Team management",
            subtitle:
              "Configure portfolio manager, research, trading, and risk roles for the fund, and tune model routing, prompts, and evolution settings per member.",
            loading: "Loading team settings...",
            missingFundId: "Missing fundId",
            loadFailed: "Failed to load team",
            retry: "Retry",
            addMember: "Add member",
            allRoles: "All roles",
            teamCount: "Bound team",
            inventoryCount: "Owned inventory",
            inventoryTitle: "Owned inventory",
            inventorySubtitle: "Members you own but have not yet bound to this fund can be added here at any time.",
            inventoryLoading: "Loading owned inventory...",
            inventoryEmpty: "No unbound members are available right now.",
            inventoryBind: "Bind to this fund",
            inventoryBinding: "Binding...",
            inventoryLoadFailed: "Failed to load owned inventory",
            inventoryBindFailed: "Failed to bind member",
            inventoryBindSuccess: "Member bound to this fund.",
            emptyTitle: "No team members yet",
            emptyDescription:
              "Create your first team member to unlock workflow collaboration, planning, and memory accumulation around the team setup.",
            addNow: "Add now",
            model: "Model",
            independentModel: "Independent model config",
            inheritedModel: "Inherit default routing",
            prompt: "Prompt",
            joinedAt: "Joined",
            latestLearning: "Latest self-learning",
            noLearningYet: "Not generated yet",
            waitingDailyReview: "Waiting for daily review",
            selected: "Selected",
            memberId: "Member ID",
            role: "Role",
            researchFocus: "Research focus",
            focusHint: "Only researcher roles can set a focus.",
            modelConfigTitle: "Dedicated model routing",
            modelConfigEnabled: "Enabled",
            modelConfigDisabled: "Disabled",
            modelConfigHintEnabled:
              "This member already uses a dedicated model route. Leaving API key blank will keep the saved key; leaving Base URL blank uses the provider default address.",
            modelConfigHintDisabled:
              "This member currently inherits the default route. Saving these fields creates a dedicated model config; if API key stays blank, requests fall back to the system key when the server has one.",
            provider: "Provider",
            modelName: "Model name",
            modelNamePlaceholder: "For example gpt-4o / claude-sonnet-4-6",
            baseUrl: "Base URL",
            baseUrlPlaceholder: "Leave empty to use the provider default URL",
            apiKey: "API key",
            apiKeyPlaceholderEnabled: "Leave empty to keep the saved key",
            apiKeyPlaceholderDisabled: "Optional: leave empty to use the system key",
            testConnection: "Test connection",
            testing: "Testing...",
            testSuccess: "Connection succeeded, latency",
            testFailurePrefix: "Connection failed:",
            testFailed: "Failed to test connection",
            providerAndModelRequired: "Please fill in both provider and model name first.",
            systemPrompt: "System prompt",
            systemPromptPlaceholder: "Describe the member's trading style, responsibility boundaries, and preferences.",
            skillConfig: "Skill config",
            skillConfigHint:
              "Configure enabled, skills, match.roles, match.focuses, match.workflowSteps, match.scenarioKeywords, priority, and related fields per member.",
            coverageTitle: "Coverage",
            coverageHint:
              "Set the preferred markets, asset classes, and directions for this member. Runtime will prioritize this coverage configuration.",
            markets: "Markets",
            assetClasses: "Asset classes",
            directions: "Directions",
            specializationTitle: "Member specialization",
            specializationHint:
              "Capture the member's stronger markets, themes, instruments, styles, and recurring patterns. Runtime context and member routing use these signals as affinity boosts.",
            specializationMarkets: "Specialization markets",
            specializationAssetClasses: "Specialization asset classes",
            specializationThemes: "Specialization themes",
            specializationInstruments: "Specialization instruments",
            specializationStyleHints: "Specialization style hints",
            specializationPatterns: "Specialization patterns",
            specializationPlaceholder: "CPO, NVDA, growth, mean reversion, supply chain mapping",
            domainConfig: "Domain config",
            evolutionConfig: "Evolution config",
            evolutionHint:
              "Use fields like dailyLearningEnabled, autoApplyAdjustments, and maxLessonsPerDay to tune learning behavior.",
            learningTitle: "Self-learning / daily review",
            learningSubtitle:
              "Shows the latest review generated by the end-of-day routine and feeds the results back into evolution settings.",
            generated: "Generated",
            pending: "Pending",
            latestReviewTime: "Latest review time",
            linkedReturn: "Linked daily return",
            learningTags: "Learning tags",
            learningSummary: "Learning summary",
            noSummary:
              "No daily self-learning record is available for this member yet. After one end-of-day cycle, the latest review summary will appear here.",
            currentModel: "Current model",
            currentStatus: "Current status",
            removeMember: "Unbind member",
            saveConfig: "Save config",
            saving: "Saving...",
            selectMember: "Select a team member to view details.",
            hireTitle: "Add member",
            hireSubtitle:
              "Choose a role and direction first. The system will create a team member with a default model that you can refine afterwards.",
            cancel: "Cancel",
            createMember: "Create member",
            creating: "Creating...",
            confirmRemoveTitle: "Unbind",
            confirmRemoveText: "This will unbind the member from the current fund team and return it to owned inventory.",
            confirmRemove: "Confirm unbind",
            removing: "Removing...",
            saveFailed: "Failed to update member config",
            saveSuccess: "Member configuration saved.",
            createFailed: "Failed to create member",
            createSuccess: "Team member created.",
            removeFailed: "Failed to unbind member",
            removeSuccess: "Member moved back to inventory.",
            invalidJson: {
              domain: "Domain config must be valid JSON.",
              skill: "Skill config must be valid JSON.",
              evolution: "Evolution config must be valid JSON.",
            },
            roleLabels: {
              pm: "Portfolio manager",
              researcher: "Researcher",
              trader: "Trader",
              risk: "Risk",
            } as Record<AgentRole, string>,
            roleIcons: { pm: "PM", researcher: "RS", trader: "TR", risk: "RK" } as Record<AgentRole, string>,
            focusLabels: {
              stock: "Single-stock",
              fundamental: "Fundamental",
              macro: "Macro",
            } as Record<ResearchFocus, string>,
            statusLabels: {
              active: "Active",
              idle: "Idle",
              inactive: "Inactive",
              error: "Error",
            } as Record<string, string>,
            providerLabels: {
              openai: "OpenAI",
              claude: "Anthropic",
              deepseek: "DeepSeek",
              qwen: "Qwen",
              custom: "Custom",
            } as Record<string, string>,
            marketLabels: {
              us_equity: "US equity",
              a_share: "A-shares",
              crypto: "Crypto",
              futures: "Futures",
            } as Record<CoverageMarket, string>,
            assetClassLabels: {
              equity: "Equity",
              fund: "Fund",
              crypto: "Crypto",
              futures: "Futures",
            } as Record<CoverageAssetClass, string>,
            directionLabels: {
              stocks: "Stocks",
              funds: "Funds",
              crypto: "Crypto",
              futures: "Futures",
              multi_asset: "Multi-asset",
            } as Record<CoverageDirection, string>,
            notSet: "Not set",
            unknown: "Unknown",
            none: "None",
            roleSummarySeparator: " · ",
          }
        : {
            title: "团队管理",
            subtitle: "为基金配置组合经理、研究、交易与风控成员，并为单个成员补充模型、提示词与演化配置。",
            loading: "正在加载团队配置...",
            missingFundId: "缺少 fundId",
            loadFailed: "加载团队失败",
            retry: "重试",
            addMember: "新增成员",
            allRoles: "全部角色",
            teamCount: "已绑定团队",
            inventoryCount: "未绑定库存",
            inventoryTitle: "未绑定库存",
            inventorySubtitle: "这里展示你已拥有但尚未绑定到当前基金的成员，可随时加入本基金团队。",
            inventoryLoading: "正在加载未绑定成员...",
            inventoryEmpty: "当前没有可绑定到本基金的未绑定成员。",
            inventoryBind: "绑定到本基金",
            inventoryBinding: "绑定中...",
            inventoryLoadFailed: "加载未绑定成员失败",
            inventoryBindFailed: "绑定成员失败",
            inventoryBindSuccess: "成员已绑定到当前基金。",
            emptyTitle: "当前还没有配置团队成员",
            emptyDescription: "先新增一个团队成员，后续策略流程、计划讨论和记忆沉淀都会围绕团队配置展开。",
            addNow: "立即新增",
            model: "模型",
            independentModel: "独立模型配置",
            inheritedModel: "继承默认路由",
            prompt: "提示词",
            joinedAt: "加入时间",
            latestLearning: "最新自主学习",
            noLearningYet: "暂未生成",
            waitingDailyReview: "等待每日复盘",
            selected: "当前选中",
            memberId: "成员标识",
            role: "角色",
            researchFocus: "研究方向",
            focusHint: "仅研究员角色支持配置研究方向。",
            modelConfigTitle: "独立模型配置",
            modelConfigEnabled: "已启用",
            modelConfigDisabled: "未启用",
            modelConfigHintEnabled: "该成员已启用独立模型路由；Base URL 留空时使用供应商默认地址，不填写 API Key 时会保留已保存的 Key。",
            modelConfigHintDisabled: "当前继承默认路由；修改下列模型字段并保存后，会为该成员创建独立模型配置。若 API Key 留空，则会在服务端已配置时回退到系统 Key。",
            provider: "供应商",
            modelName: "模型名称",
            modelNamePlaceholder: "例如 gpt-4o / claude-sonnet-4-6",
            baseUrl: "Base URL",
            baseUrlPlaceholder: "留空则使用供应商默认地址",
            apiKey: "API Key",
            apiKeyPlaceholderEnabled: "留空则保留已保存的 Key",
            apiKeyPlaceholderDisabled: "可选：留空则使用系统 Key",
            testConnection: "测试连接",
            testing: "测试中...",
            testSuccess: "连接成功，延迟",
            testFailurePrefix: "连接失败：",
            testFailed: "测试连接失败",
            providerAndModelRequired: "请先填写模型供应商和模型名称。",
            systemPrompt: "系统提示词",
            systemPromptPlaceholder: "描述该成员的交易风格、职责边界和偏好。",
            skillConfig: "技能配置",
            skillConfigHint: "可为单个成员配置 enabled、skills、match.roles、match.focuses、match.workflowSteps、match.scenarioKeywords、priority 等字段。",
            coverageTitle: "覆盖方向",
            coverageHint: "为成员配置优先覆盖的市场、资产类别和方向；runtime 会优先使用这里的 coverage。",
            markets: "市场",
            assetClasses: "资产类别",
            directions: "方向",
            specializationTitle: "成员专精",
            specializationHint: "描述该成员更强的市场、主题、标的、风格与常用分析模式。runtime 上下文和选人排序会把这些信号作为 affinity 加分项。",
            specializationMarkets: "专精市场",
            specializationAssetClasses: "专精资产类别",
            specializationThemes: "专精主题",
            specializationInstruments: "专精标的",
            specializationStyleHints: "专精风格提示",
            specializationPatterns: "专精模式",
            specializationPlaceholder: "CPO、NVDA、growth、mean reversion、supply chain mapping",
            domainConfig: "领域配置",
            evolutionConfig: "进化配置",
            evolutionHint: "建议在这里配置 dailyLearningEnabled、autoApplyAdjustments、maxLessonsPerDay 等学习策略字段。",
            learningTitle: "自主学习 / 每日复盘",
            learningSubtitle: "展示该成员最近一次由日终流程沉淀的学习结果，并将自动回写到进化配置。",
            generated: "已生成",
            pending: "待生成",
            latestReviewTime: "最新复盘时间",
            linkedReturn: "关联日收益",
            learningTags: "学习标签",
            learningSummary: "学习摘要",
            noSummary: "当前还没有该成员的每日自主学习记录。完成一次日终流程后，这里会显示最新复盘摘要。",
            currentModel: "当前模型",
            currentStatus: "当前状态",
            removeMember: "解绑该成员",
            saveConfig: "保存配置",
            saving: "保存中...",
            selectMember: "请选择一个团队成员查看详情。",
            hireTitle: "新增成员",
            hireSubtitle: "先选择角色与方向，系统会创建一个可继续配置的团队成员并分配默认模型。",
            cancel: "取消",
            createMember: "确认新增",
            creating: "创建中...",
            confirmRemoveTitle: "解绑",
            confirmRemoveText: "该操作会把该成员从当前基金团队解绑，并返回到未绑定库存。",
            confirmRemove: "确认解绑",
            removing: "移除中...",
            saveFailed: "更新成员配置失败",
            saveSuccess: "成员配置已保存。",
            createFailed: "新增成员失败",
            createSuccess: "团队成员已创建。",
            removeFailed: "解绑成员失败",
            removeSuccess: "成员已回到未绑定库存。",
            invalidJson: {
              domain: "领域配置不是合法 JSON。",
              skill: "技能配置不是合法 JSON。",
              evolution: "进化配置不是合法 JSON。",
            },
            roleLabels: {
              pm: "组合经理",
              researcher: "研究员",
              trader: "交易员",
              risk: "风控",
            } as Record<AgentRole, string>,
            roleIcons: { pm: "PM", researcher: "RS", trader: "TR", risk: "RK" } as Record<AgentRole, string>,
            focusLabels: {
              stock: "个股",
              fundamental: "基本面",
              macro: "宏观",
            } as Record<ResearchFocus, string>,
            statusLabels: {
              active: "启用中",
              idle: "待命中",
              inactive: "已停用",
              error: "异常",
            } as Record<string, string>,
            providerLabels: {
              openai: "OpenAI",
              claude: "Anthropic",
              deepseek: "DeepSeek",
              qwen: "Qwen",
              custom: "自定义",
            } as Record<string, string>,
            marketLabels: {
              us_equity: "美股",
              a_share: "A股",
              crypto: "Crypto",
              futures: "期货",
            } as Record<CoverageMarket, string>,
            assetClassLabels: {
              equity: "股票",
              fund: "基金",
              crypto: "Crypto",
              futures: "期货",
            } as Record<CoverageAssetClass, string>,
            directionLabels: {
              stocks: "股票",
              funds: "基金",
              crypto: "Crypto",
              futures: "期货",
              multi_asset: "多资产",
            } as Record<CoverageDirection, string>,
            notSet: "未设置",
            unknown: "未知",
            none: "暂无",
            roleSummarySeparator: " · ",
          },
    [language],
  );

  const normalizeAgentName = useCallback(
    (agent: TeamAgent) => agent.name?.trim() || `${copy.roleLabels[agent.role]}${language === "en-US" ? " member" : "成员"}`,
    [copy.roleLabels, language],
  );

  const formatProvider = useCallback(
    (provider?: string) => copy.providerLabels[provider ?? ""] ?? provider ?? copy.notSet,
    [copy.notSet, copy.providerLabels],
  );

  const modelSummary = useCallback(
    (agent: TeamAgent) => {
      if (agent.modelProvider && agent.modelName) {
        return `${formatProvider(agent.modelProvider)} / ${agent.modelName}`;
      }
      return agent.llmModel || copy.notSet;
    },
    [copy.notSet, formatProvider],
  );

  const formatPercent = useCallback(
    (value?: number) => {
      if (value === undefined || Number.isNaN(value)) {
        return copy.none;
      }
      return `${formatNumberForLanguage(value * 100, language, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}%`;
    },
    [copy.none, language],
  );

  const statusMeta = useCallback(
    (status?: string) => {
      const normalized = status ?? "inactive";
      const badgeMap: Record<string, string> = {
        active: "bg-emerald-50 text-emerald-700 border-emerald-200",
        idle: "bg-amber-50 text-amber-700 border-amber-200",
        inactive: "bg-gray-50 text-gray-600 border-gray-200",
        error: "bg-red-50 text-red-700 border-red-200",
      };
      return {
        label: copy.statusLabels[normalized] ?? status ?? copy.unknown,
        badge: badgeMap[normalized] ?? badgeMap.inactive,
      };
    },
    [copy.statusLabels, copy.unknown],
  );

  const roleSummary = useCallback(
    (role: AgentRole, focus?: string, coverage?: AgentCoverage) => {
      if (coverage?.directions?.length) {
        return `${copy.roleLabels[role]}${copy.roleSummarySeparator}${coverage.directions
          .map((item) => copy.directionLabels[item as CoverageDirection] ?? item)
          .join(" / ")}`;
      }
      if (role === "researcher" && focus) {
        return `${copy.roleLabels[role]}${copy.roleSummarySeparator}${copy.focusLabels[focus as ResearchFocus] ?? focus}`;
      }
      return copy.roleLabels[role];
    },
    [copy.directionLabels, copy.focusLabels, copy.roleLabels, copy.roleSummarySeparator],
  );

  const loadAgents = useCallback(async () => {
    if (!fundId) {
      setError(copy.missingFundId);
      setLoading(false);
      return;
    }

    setLoading(true);
    setError(null);
    try {
      const response = await apiGet<TeamAgent[]>(`/api/funds/${fundId}/team`);
      const nextAgents = response ?? [];
      setAgents(nextAgents);
      setSelectedAgentId((current) => {
        const visibleAgents = filterRole === "all" ? nextAgents : nextAgents.filter((agent) => agent.role === filterRole);
        if (current && visibleAgents.some((agent) => agent.id === current)) {
          return current;
        }
        return visibleAgents[0]?.id ?? nextAgents[0]?.id ?? null;
      });
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed, copy.missingFundId, filterRole, fundId]);

  const loadOwnedInventory = useCallback(async () => {
    setLoadingInventory(true);
    try {
      const response = await apiGet<TeamAgent[]>("/api/agents?bindStatus=unbound");
      setOwnedInventory(response ?? []);
    } catch (err) {
      setSaveError(formatApiError(err, copy.inventoryLoadFailed));
    } finally {
      setLoadingInventory(false);
    }
  }, [copy.inventoryLoadFailed]);

  useEffect(() => {
    void loadAgents();
  }, [loadAgents]);

  useEffect(() => {
    void loadOwnedInventory();
  }, [loadOwnedInventory]);

  const filteredAgents = useMemo(
    () => (filterRole === "all" ? agents : agents.filter((agent) => agent.role === filterRole)),
    [agents, filterRole],
  );

  const selectedAgent = useMemo(
    () => filteredAgents.find((agent) => agent.id === selectedAgentId) ?? filteredAgents[0] ?? null,
    [filteredAgents, selectedAgentId],
  );

  useEffect(() => {
    if (filteredAgents.length === 0) {
      setSelectedAgentId(null);
      return;
    }
    if (!selectedAgentId || !filteredAgents.some((agent) => agent.id === selectedAgentId)) {
      setSelectedAgentId(filteredAgents[0].id);
    }
  }, [filteredAgents, selectedAgentId]);

  useEffect(() => {
    const selectedId = selectedAgent?.id ?? null;
    setEditor(buildEditorState(selectedAgent));
    if (previousSelectedAgentIdRef.current !== selectedId) {
      setSaveError(null);
      setSaveSuccess(null);
      setTestResult(null);
      setTestSucceeded(null);
    }
    previousSelectedAgentIdRef.current = selectedId;
  }, [selectedAgent]);

  const resetHireForm = () => {
    setHireRole("researcher");
    setHireFocus("stock");
    setSaveError(null);
  };

  const handleHire = useCallback(async () => {
    if (!fundId) {
      return;
    }

    setSaving(true);
    setSaveError(null);
    setSaveSuccess(null);
    try {
      const created = await apiPost<TeamAgent>(`/api/funds/${fundId}/team`, {
        role: hireRole,
        focus: hireRole === "researcher" ? hireFocus : "",
      });
      setAgents((prev) => [...prev, created]);
      setSelectedAgentId(created.id);
      setFilterRole("all");
      setShowHireModal(false);
      resetHireForm();
      setSaveSuccess(copy.createSuccess);
    } catch (err) {
      setSaveError(formatApiError(err, copy.createFailed));
    } finally {
      setSaving(false);
    }
  }, [copy.createFailed, copy.createSuccess, fundId, hireFocus, hireRole]);

  const handleBindInventory = useCallback(async (agentId: string) => {
    if (!fundId) {
      return;
    }
    setBindingAgentId(agentId);
    setSaveError(null);
    setSaveSuccess(null);
    try {
      const boundAgent = await apiPost<TeamAgent>(`/api/funds/${fundId}/team/bind`, { agentId });
      setOwnedInventory((current) => current.filter((agent) => agent.id !== agentId));
      setAgents((current) => {
        const existing = current.find((agent) => agent.id === boundAgent.id);
        if (existing) {
          return current.map((agent) => (agent.id === boundAgent.id ? boundAgent : agent));
        }
        return [...current, boundAgent];
      });
      setFilterRole("all");
      setSelectedAgentId(boundAgent.id);
      setSaveSuccess(copy.inventoryBindSuccess);
    } catch (err) {
      setSaveError(formatApiError(err, copy.inventoryBindFailed));
    } finally {
      setBindingAgentId(null);
    }
  }, [copy.inventoryBindFailed, copy.inventoryBindSuccess, fundId]);

  const handleEditorChange = useCallback(<K extends keyof AgentEditorState>(key: K, value: AgentEditorState[K]) => {
    setEditor((current) => ({ ...current, [key]: value }));
  }, []);

  const handleModelFieldChange = useCallback((key: "modelProvider" | "modelName" | "modelBaseURL" | "apiKey", value: string) => {
    setEditor((current) => ({ ...current, [key]: value, modelConfigDirty: true }));
  }, []);

  const handleTestConnection = useCallback(async () => {
    if (!editor.modelProvider.trim() || !editor.modelName.trim()) {
      setSaveError(copy.providerAndModelRequired);
      return;
    }
    setTestingConnection(true);
    setSaveError(null);
    setTestResult(null);
    setTestSucceeded(null);
    try {
      const payload: Record<string, string> = {
        provider: editor.modelProvider.trim(),
        model_name: editor.modelName.trim(),
      };
      if (editor.modelBaseURL.trim()) {
        payload.base_url = editor.modelBaseURL.trim();
      }
      if (editor.apiKey.trim()) {
        payload.api_key = editor.apiKey.trim();
      }
      const result = await apiPost<ConnectionTestResult>("/api/models/test", payload);
      setTestSucceeded(result.success);
      setTestResult(
        result.success
          ? `${copy.testSuccess} ${formatNumberForLanguage(result.latency_ms, language)}ms.`
          : `${copy.testFailurePrefix}${result.message}`,
      );
    } catch (err) {
      setTestSucceeded(false);
      setSaveError(formatApiError(err, copy.testFailed));
    } finally {
      setTestingConnection(false);
    }
  }, [copy.providerAndModelRequired, copy.testFailed, copy.testFailurePrefix, copy.testSuccess, editor, language]);

  const handleSave = useCallback(async () => {
    if (!fundId || !selectedAgent) {
      return;
    }

    setSaving(true);
    setSaveError(null);
    setSaveSuccess(null);
    try {
      const domainConfig = parseJSONInput(copy.invalidJson.domain, editor.domainConfig) as DomainConfigShape;
      const payload: Record<string, unknown> = {
        role: editor.role,
        focus: editor.role === "researcher" ? editor.focus.trim() : "",
        systemPrompt: editor.systemPrompt,
        skillConfig: parseJSONInput(copy.invalidJson.skill, editor.skillConfig),
        domainConfig: {
          ...domainConfig,
          coverage: {
            markets: editor.coverageMarkets,
            assetClasses: editor.coverageAssetClasses,
            directions: editor.coverageDirections,
          },
          specialization: {
            markets: normalizeListInput(editor.specializationMarkets),
            assetClasses: normalizeListInput(editor.specializationAssetClasses),
            themes: normalizeListInput(editor.specializationThemes),
            instruments: normalizeListInput(editor.specializationInstruments),
            styleHints: normalizeListInput(editor.specializationStyleHints),
            patterns: normalizeListInput(editor.specializationPatterns),
          },
        },
        evolutionConfig: parseJSONInput(copy.invalidJson.evolution, editor.evolutionConfig),
      };

      if (selectedAgent.hasCustomModelConfig || editor.modelConfigDirty || editor.apiKey.trim()) {
        if (!editor.modelProvider.trim() || !editor.modelName.trim()) {
          throw new Error(copy.providerAndModelRequired);
        }
        payload.modelConfig = {
          provider: editor.modelProvider.trim(),
          modelName: editor.modelName.trim(),
          ...(editor.modelBaseURL.trim() ? { baseUrl: editor.modelBaseURL.trim() } : {}),
          ...(editor.apiKey.trim() ? { apiKey: editor.apiKey.trim() } : {}),
        };
      }

      const updated = await apiPut<TeamAgent>(`/api/funds/${fundId}/team/${selectedAgent.id}`, payload);
      setAgents((prev) => prev.map((agent) => (agent.id === updated.id ? updated : agent)));
      setSelectedAgentId(updated.id);
      if (filterRole !== "all" && filterRole !== updated.role) {
        setFilterRole("all");
      }
      setSaveSuccess(copy.saveSuccess);
    } catch (err) {
      setSaveError(err instanceof Error ? err.message : formatApiError(err, copy.saveFailed));
    } finally {
      setSaving(false);
    }
  }, [copy.invalidJson.domain, copy.invalidJson.evolution, copy.invalidJson.skill, copy.providerAndModelRequired, copy.saveFailed, copy.saveSuccess, editor, filterRole, fundId, selectedAgent]);

  const handleFire = useCallback(async () => {
    if (!fundId || !fireTarget) {
      return;
    }

    setFiring(true);
    setSaveError(null);
    setSaveSuccess(null);
    try {
      await apiDelete(`/api/funds/${fundId}/team/${fireTarget.id}`);
      setAgents((prev) => prev.filter((agent) => agent.id !== fireTarget.id));
      setSelectedAgentId((current) => (current === fireTarget.id ? null : current));
      setFilterRole("all");
      setFireTarget(null);
      await loadOwnedInventory();
      setSaveSuccess(copy.removeSuccess);
    } catch (err) {
      setSaveError(formatApiError(err, copy.removeFailed));
    } finally {
      setFiring(false);
    }
  }, [copy.removeFailed, copy.removeSuccess, fireTarget, fundId, loadOwnedInventory]);

  if (loading) {
    return <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadAgents()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {copy.retry}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm md:flex-row md:items-start md:justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <button
          onClick={() => {
            resetHireForm();
            setShowHireModal(true);
          }}
          className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700"
        >
          {copy.addMember}
        </button>
      </div>

      {saveError ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{saveError}</div> : null}
      {saveSuccess ? <div className="rounded-xl border border-emerald-200 bg-emerald-50 px-4 py-3 text-sm text-emerald-700">{saveSuccess}</div> : null}

      <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
        <div className="rounded-xl border border-gray-200 bg-white px-4 py-3 shadow-sm">
          <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.teamCount}</p>
          <p className="mt-1 text-sm text-gray-500">{formatNumberForLanguage(agents.length, language)}</p>
        </div>
        <div className="rounded-xl border border-gray-200 bg-white px-4 py-3 shadow-sm">
          <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.inventoryCount}</p>
          <p className="mt-1 text-sm text-gray-500">{formatNumberForLanguage(ownedInventory.length, language)}</p>
        </div>
      </div>

      {fundId ? <TeamActivityPanel fundId={fundId} /> : null}

      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
        <div className="flex flex-col gap-2 md:flex-row md:items-start md:justify-between">
          <div>
            <h2 className="text-lg font-semibold text-gray-900">{copy.inventoryTitle}</h2>
            <p className="mt-1 text-sm text-gray-500">{copy.inventorySubtitle}</p>
          </div>
        </div>
        {loadingInventory ? (
          <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-6 text-sm text-gray-500">{copy.inventoryLoading}</div>
        ) : ownedInventory.length === 0 ? (
          <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 px-4 py-6 text-sm text-gray-500">{copy.inventoryEmpty}</div>
        ) : (
          <div className="mt-4 grid grid-cols-1 gap-3 xl:grid-cols-2">
            {ownedInventory.map((agent) => (
              <div key={agent.id} className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-4">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <p className="truncate text-sm font-semibold text-gray-900">{normalizeAgentName(agent)}</p>
                      <span className="rounded-full bg-indigo-50 px-2 py-0.5 text-[10px] font-medium text-indigo-700">{copy.roleLabels[agent.role]}</span>
                    </div>
                    <p className="mt-1 text-xs text-gray-500">{roleSummary(agent.role, agent.focus, extractCoverage(agent.domainConfig))}</p>
                    <p className="mt-2 text-xs text-gray-500">{copy.currentModel}: {modelSummary(agent)}</p>
                    <p className="mt-2 line-clamp-2 text-sm text-gray-700">{agent.latestLearningSummary?.trim() || copy.noLearningYet}</p>
                  </div>
                  <button
                    onClick={() => void handleBindInventory(agent.id)}
                    disabled={bindingAgentId === agent.id || saving}
                    className="shrink-0 rounded-lg bg-indigo-600 px-3 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    {bindingAgentId === agent.id ? copy.inventoryBinding : copy.inventoryBind}
                  </button>
                </div>
              </div>
            ))}
          </div>
        )}
      </div>

      <div className="flex flex-wrap gap-2">
        {(["all", "pm", "researcher", "trader", "risk"] as const).map((role) => (
          <button
            key={role}
            onClick={() => setFilterRole(role)}
            className={`rounded-full px-4 py-1.5 text-sm font-medium transition-colors ${
              filterRole === role ? "bg-indigo-600 text-white" : "bg-white text-gray-600 ring-1 ring-gray-200 hover:bg-gray-50"
            }`}
          >
            {role === "all" ? copy.allRoles : copy.roleLabels[role]}
          </button>
        ))}
      </div>

      {agents.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
          <p className="font-medium text-gray-700">{copy.emptyTitle}</p>
          <p className="mt-2">{copy.emptyDescription}</p>
          <button
            onClick={() => {
              resetHireForm();
              setShowHireModal(true);
            }}
            className="mt-4 rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700"
          >
            {copy.addNow}
          </button>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-6 xl:grid-cols-[340px_minmax(0,1fr)]">
          <aside className="space-y-3">
            <div className="rounded-xl border border-gray-200 bg-white px-4 py-3 shadow-sm">
              <p className="text-xs font-semibold uppercase tracking-wider text-gray-500">{copy.allRoles}</p>
              <p className="mt-1 text-sm text-gray-500">{formatNumberForLanguage(filteredAgents.length, language)} / {formatNumberForLanguage(agents.length, language)}</p>
            </div>
            <div className="max-h-[72vh] overflow-y-auto rounded-2xl border border-gray-200 bg-white shadow-sm">
              {filteredAgents.length === 0 ? (
                <div className="px-4 py-8 text-center text-sm text-gray-500">{copy.emptyDescription}</div>
              ) : (
                <div className="divide-y divide-gray-100">
                  {filteredAgents.map((agent) => {
                    const isSelected = selectedAgent?.id === agent.id;
                    const meta = statusMeta(agent.status);
                    const timestamp = agent.latestLearningAt
                      ? `${copy.latestLearning} · ${formatDateTimeForLanguage(agent.latestLearningAt, language)}`
                      : agent.joinedAt
                        ? `${copy.joinedAt} · ${formatDateForLanguage(agent.joinedAt, language)}`
                        : copy.waitingDailyReview;
                    return (
                      <button
                        key={agent.id}
                        onClick={() => setSelectedAgentId(agent.id)}
                        className={`w-full border-l-4 px-4 py-3 text-left transition hover:bg-gray-50 ${
                          isSelected ? "border-indigo-500 bg-indigo-50/70" : "border-transparent"
                        }`}
                      >
                        <div className="flex items-start justify-between gap-3">
                          <div className="min-w-0 flex items-start gap-3">
                            <span className="mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-full bg-indigo-50 text-xs font-semibold text-indigo-700">
                              {copy.roleIcons[agent.role]}
                            </span>
                            <div className="min-w-0">
                              <div className="flex flex-wrap items-center gap-2">
                                <p className="truncate text-sm font-semibold text-gray-900">{normalizeAgentName(agent)}</p>
                                <span className="rounded-full bg-gray-100 px-2 py-0.5 text-[10px] font-medium text-gray-600">
                                  {copy.roleLabels[agent.role]}
                                </span>
                              </div>
                              <p className="mt-1 truncate text-xs text-gray-500">{roleSummary(agent.role, agent.focus, extractCoverage(agent.domainConfig))}</p>
                              <p className="mt-2 truncate text-[11px] text-gray-400">{timestamp}</p>
                            </div>
                          </div>
                          <span className={`shrink-0 rounded-full border px-2 py-0.5 text-[10px] font-medium ${meta.badge}`}>
                            {meta.label}
                          </span>
                        </div>
                      </button>
                    );
                  })}
                </div>
              )}
            </div>
          </aside>

          <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
            {selectedAgent ? (
              <div className="space-y-6">
                <div className="flex items-start justify-between gap-3">
                  <div>
                    <p className="text-sm text-gray-500">{copy.selected}</p>
                    <h2 className="mt-1 text-xl font-bold text-gray-900">{normalizeAgentName(selectedAgent)}</h2>
                    <p className="mt-1 text-sm text-gray-500">
                      {copy.memberId}: {selectedAgent.agentId ?? selectedAgent.id}
                    </p>
                  </div>
                  <span className="rounded-full bg-indigo-50 px-3 py-1 text-xs font-medium text-indigo-700">
                    {copy.roleLabels[selectedAgent.role]}
                  </span>
                </div>

                <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
                  <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                    <p className="text-xs text-gray-500">{copy.currentModel}</p>
                    <p className="mt-1 text-sm font-medium text-gray-900">{modelSummary(selectedAgent)}</p>
                    <p className="mt-1 text-xs text-gray-500">
                      {selectedAgent.hasCustomModelConfig ? copy.independentModel : copy.inheritedModel}
                    </p>
                  </div>
                  <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3">
                    <p className="text-xs text-gray-500">{copy.joinedAt}</p>
                    <p className="mt-1 text-sm font-medium text-gray-900">{selectedAgent.joinedAt ? formatDateForLanguage(selectedAgent.joinedAt, language) : copy.unknown}</p>
                    <p className="mt-1 text-xs text-gray-500">{copy.currentStatus}: {statusMeta(selectedAgent.status).label}</p>
                  </div>
                  <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 lg:col-span-2">
                    <p className="text-xs text-gray-500">{copy.prompt}</p>
                    <p className="mt-1 line-clamp-2 text-sm text-gray-800">{selectedAgent.systemPrompt?.trim() || copy.notSet}</p>
                  </div>
                  <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 lg:col-span-2">
                    <div className="flex items-center justify-between gap-3">
                      <p className="text-xs text-gray-500">{copy.latestLearning}</p>
                      <span className="text-[11px] text-gray-400">
                        {selectedAgent.latestLearningAt ? formatDateTimeForLanguage(selectedAgent.latestLearningAt, language) : copy.waitingDailyReview}
                      </span>
                    </div>
                    <p className="mt-1 line-clamp-2 text-sm text-gray-800">{selectedAgent.latestLearningSummary?.trim() || copy.noLearningYet}</p>
                  </div>
                </div>

                <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
                  <div>
                    <label htmlFor="agent-role" className="mb-1 block text-sm font-medium text-gray-700">{copy.role}</label>
                    <select
                      id="agent-role"
                      value={editor.role}
                      disabled={saving}
                      onChange={(e) => handleEditorChange("role", e.target.value as AgentRole)}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                    >
                      {(["pm", "researcher", "trader", "risk"] as AgentRole[]).map((role) => (
                        <option key={role} value={role}>
                          {copy.roleLabels[role]}
                        </option>
                      ))}
                    </select>
                  </div>

                  <div>
                    <label htmlFor="agent-focus" className="mb-1 block text-sm font-medium text-gray-700">{copy.researchFocus}</label>
                    <select
                      id="agent-focus"
                      value={editor.focus}
                      disabled={saving || editor.role !== "researcher"}
                      onChange={(e) => handleEditorChange("focus", e.target.value)}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none disabled:cursor-not-allowed disabled:bg-gray-50"
                    >
                      <option value="">{copy.notSet}</option>
                      {(["stock", "fundamental", "macro"] as ResearchFocus[]).map((focus) => (
                        <option key={focus} value={focus}>
                          {copy.focusLabels[focus]}
                        </option>
                      ))}
                    </select>
                    <p className="mt-1 text-xs text-gray-500">{copy.focusHint}</p>
                  </div>
                </div>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-sm font-semibold text-gray-900">
                    <span>{copy.modelConfigTitle}</span>
                    <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${selectedAgent.hasCustomModelConfig ? "bg-emerald-50 text-emerald-700" : "bg-gray-100 text-gray-600"}`}>
                      {selectedAgent.hasCustomModelConfig ? copy.modelConfigEnabled : copy.modelConfigDisabled}
                    </span>
                  </summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <div className="space-y-2 text-xs text-gray-500">
                      <p>{selectedAgent.hasCustomModelConfig ? copy.modelConfigHintEnabled : copy.modelConfigHintDisabled}</p>
                      <ul className="list-disc space-y-1 pl-5">
                        <li>{language === "en-US" ? "Leaving Base URL blank uses the provider default address." : "Base URL 留空时，使用该 provider 的默认地址。"}</li>
                        <li>{language === "en-US" ? "Leaving API key blank keeps the saved key when this member already has a dedicated config." : "API Key 留空时，如果该成员已保存独立 key，则继续沿用已保存 key。"}</li>
                        <li>{language === "en-US" ? "If this member has no dedicated key, requests fall back to the system key when the server has one configured for that provider." : "如果该成员没有独立 key，则回退到系统级 key（前提是服务端已配置该 provider 的系统 key）。"}</li>
                        <li>{language === "en-US" ? "If no system key exists either, runtime requests will fail." : "如果系统级 key 也未配置，请求会在运行时失败。"}</li>
                      </ul>
                    </div>
                    <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                      <div>
                        <label htmlFor="agent-model-provider" className="mb-1 block text-sm font-medium text-gray-700">{copy.provider}</label>
                        <select
                          id="agent-model-provider"
                          value={editor.modelProvider}
                          disabled={saving}
                          onChange={(e) => handleModelFieldChange("modelProvider", e.target.value)}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        >
                          {providerOptions.map((option) => (
                            <option key={option.value} value={option.value}>
                              {copy.providerLabels[option.value] ?? option.defaultLabel}
                            </option>
                          ))}
                        </select>
                      </div>
                      <div>
                        <label htmlFor="agent-model-name" className="mb-1 block text-sm font-medium text-gray-700">{copy.modelName}</label>
                        <input
                          id="agent-model-name"
                          value={editor.modelName}
                          disabled={saving}
                          onChange={(e) => handleModelFieldChange("modelName", e.target.value)}
                          placeholder={copy.modelNamePlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </div>
                      <div className="md:col-span-2">
                        <label htmlFor="agent-model-base-url" className="mb-1 block text-sm font-medium text-gray-700">{copy.baseUrl}</label>
                        <input
                          id="agent-model-base-url"
                          value={editor.modelBaseURL}
                          disabled={saving}
                          onChange={(e) => handleModelFieldChange("modelBaseURL", e.target.value)}
                          placeholder={copy.baseUrlPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </div>
                      <div className="md:col-span-2">
                        <label htmlFor="agent-model-api-key" className="mb-1 block text-sm font-medium text-gray-700">{copy.apiKey}</label>
                        <input
                          id="agent-model-api-key"
                          type="password"
                          value={editor.apiKey}
                          disabled={saving}
                          onChange={(e) => handleModelFieldChange("apiKey", e.target.value)}
                          placeholder={selectedAgent.hasCustomModelConfig ? copy.apiKeyPlaceholderEnabled : copy.apiKeyPlaceholderDisabled}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </div>
                    </div>

                    <div className="mt-4 flex flex-wrap items-center gap-3">
                      <button
                        onClick={() => void handleTestConnection()}
                        disabled={saving || testingConnection}
                        className="rounded-lg border border-indigo-200 bg-indigo-50 px-4 py-2 text-sm font-medium text-indigo-700 hover:bg-indigo-100 disabled:cursor-not-allowed disabled:opacity-60"
                      >
                        {testingConnection ? copy.testing : copy.testConnection}
                      </button>
                      {testResult ? (
                        <span className={`text-sm ${testSucceeded ? "text-emerald-700" : "text-red-700"}`}>{testResult}</span>
                      ) : null}
                    </div>
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.systemPrompt}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <textarea
                      id="agent-system-prompt"
                      aria-label={copy.systemPrompt}
                      rows={5}
                      value={editor.systemPrompt}
                      disabled={saving}
                      onChange={(e) => handleEditorChange("systemPrompt", e.target.value)}
                      placeholder={copy.systemPromptPlaceholder}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                    />
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.skillConfig}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <textarea
                      id="agent-skill-config"
                      aria-label={copy.skillConfig}
                      rows={7}
                      value={editor.skillConfig}
                      disabled={saving}
                      onChange={(e) => handleEditorChange("skillConfig", e.target.value)}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 font-mono text-xs focus:border-indigo-500 focus:outline-none"
                    />
                    <p className="mt-1 text-xs text-gray-500">{copy.skillConfigHint}</p>
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.coverageTitle}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <p className="text-xs text-gray-500">{copy.coverageHint}</p>
                    <div className="mt-4 grid grid-cols-1 gap-4 lg:grid-cols-3">
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.markets}</span>
                        <select
                          multiple
                          value={editor.coverageMarkets}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("coverageMarkets", Array.from(e.target.selectedOptions, (option) => option.value as CoverageMarket))}
                          className="h-24 w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        >
                          {(["us_equity", "a_share", "crypto", "futures"] as CoverageMarket[]).map((market) => (
                            <option key={market} value={market}>
                              {copy.marketLabels[market]}
                            </option>
                          ))}
                        </select>
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.assetClasses}</span>
                        <select
                          multiple
                          value={editor.coverageAssetClasses}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("coverageAssetClasses", Array.from(e.target.selectedOptions, (option) => option.value as CoverageAssetClass))}
                          className="h-24 w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        >
                          {(["equity", "fund", "crypto", "futures"] as CoverageAssetClass[]).map((assetClass) => (
                            <option key={assetClass} value={assetClass}>
                              {copy.assetClassLabels[assetClass]}
                            </option>
                          ))}
                        </select>
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.directions}</span>
                        <select
                          multiple
                          value={editor.coverageDirections}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("coverageDirections", Array.from(e.target.selectedOptions, (option) => option.value as CoverageDirection))}
                          className="h-24 w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        >
                          {(["stocks", "funds", "crypto", "futures", "multi_asset"] as CoverageDirection[]).map((direction) => (
                            <option key={direction} value={direction}>
                              {copy.directionLabels[direction]}
                            </option>
                          ))}
                        </select>
                      </label>
                    </div>
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.specializationTitle}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <p className="text-xs text-gray-500">{copy.specializationHint}</p>
                    <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2">
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationMarkets}</span>
                        <textarea
                          aria-label={copy.specializationMarkets}
                          rows={2}
                          value={editor.specializationMarkets}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationMarkets", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationAssetClasses}</span>
                        <textarea
                          aria-label={copy.specializationAssetClasses}
                          rows={2}
                          value={editor.specializationAssetClasses}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationAssetClasses", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationThemes}</span>
                        <textarea
                          aria-label={copy.specializationThemes}
                          rows={2}
                          value={editor.specializationThemes}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationThemes", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationInstruments}</span>
                        <textarea
                          aria-label={copy.specializationInstruments}
                          rows={2}
                          value={editor.specializationInstruments}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationInstruments", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationStyleHints}</span>
                        <textarea
                          aria-label={copy.specializationStyleHints}
                          rows={2}
                          value={editor.specializationStyleHints}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationStyleHints", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                      <label>
                        <span className="mb-1 block text-sm font-medium text-gray-700">{copy.specializationPatterns}</span>
                        <textarea
                          aria-label={copy.specializationPatterns}
                          rows={2}
                          value={editor.specializationPatterns}
                          disabled={saving}
                          onChange={(e) => handleEditorChange("specializationPatterns", e.target.value)}
                          placeholder={copy.specializationPlaceholder}
                          className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                        />
                      </label>
                    </div>
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.domainConfig}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <textarea
                      id="agent-domain-config"
                      aria-label={copy.domainConfig}
                      rows={7}
                      value={editor.domainConfig}
                      disabled={saving}
                      onChange={(e) => handleEditorChange("domainConfig", e.target.value)}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 font-mono text-xs focus:border-indigo-500 focus:outline-none"
                    />
                  </div>
                </details>

                <details open className="rounded-xl border border-gray-200 bg-white">
                  <summary className="cursor-pointer list-none px-4 py-3 text-sm font-semibold text-gray-900">{copy.evolutionConfig}</summary>
                  <div className="border-t border-gray-100 px-4 py-4">
                    <textarea
                      id="agent-evolution-config"
                      aria-label={copy.evolutionConfig}
                      rows={7}
                      value={editor.evolutionConfig}
                      disabled={saving}
                      onChange={(e) => handleEditorChange("evolutionConfig", e.target.value)}
                      className="w-full rounded-lg border border-gray-300 px-3 py-2 font-mono text-xs focus:border-indigo-500 focus:outline-none"
                    />
                    <p className="mt-1 text-xs text-gray-500">{copy.evolutionHint}</p>
                  </div>
                </details>

                <details open className="rounded-xl border border-indigo-200 bg-indigo-50">
                  <summary className="flex cursor-pointer list-none items-center justify-between gap-3 px-4 py-3 text-sm font-semibold text-indigo-900">
                    <span>{copy.learningTitle}</span>
                    <span className="rounded-full bg-white px-2.5 py-1 text-xs font-medium text-indigo-700 ring-1 ring-indigo-200">
                      {selectedAgent.latestLearningAt ? copy.generated : copy.pending}
                    </span>
                  </summary>
                  <div className="border-t border-indigo-100 px-4 py-4">
                    <p className="text-xs text-indigo-700">{copy.learningSubtitle}</p>
                    <div className="mt-4 grid grid-cols-1 gap-3 md:grid-cols-3">
                      <div className="rounded-lg bg-white px-3 py-2">
                        <p className="text-xs text-gray-500">{copy.latestReviewTime}</p>
                        <p className="mt-1 text-sm font-medium text-gray-900">
                          {selectedAgent.latestLearningAt ? formatDateTimeForLanguage(selectedAgent.latestLearningAt, language) : copy.none}
                        </p>
                      </div>
                      <div className="rounded-lg bg-white px-3 py-2">
                        <p className="text-xs text-gray-500">{copy.linkedReturn}</p>
                        <p className="mt-1 text-sm font-medium text-gray-900">{formatPercent(selectedAgent.latestLearningReturn)}</p>
                      </div>
                      <div className="rounded-lg bg-white px-3 py-2">
                        <p className="text-xs text-gray-500">{copy.learningTags}</p>
                        <p className="mt-1 text-sm font-medium text-gray-900">{selectedAgent.latestLearningTags?.join(" / ") || copy.none}</p>
                      </div>
                    </div>

                    <div className="mt-4 rounded-lg bg-white px-3 py-3">
                      <p className="text-xs text-gray-500">{copy.learningSummary}</p>
                      <p className="mt-2 text-sm leading-6 text-gray-800">{selectedAgent.latestLearningSummary?.trim() || copy.noSummary}</p>
                    </div>
                  </div>
                </details>

                <div className="flex gap-3">
                  <button
                    onClick={() => setFireTarget(selectedAgent)}
                    className="flex-1 rounded-lg border border-red-200 bg-red-50 px-4 py-2.5 text-sm font-medium text-red-700 hover:bg-red-100"
                  >
                    {copy.removeMember}
                  </button>
                  <button
                    onClick={() => void handleSave()}
                    disabled={saving}
                    className="flex-1 rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    {saving ? copy.saving : copy.saveConfig}
                  </button>
                </div>
              </div>
            ) : (
              <div className="flex h-full min-h-64 items-center justify-center text-center text-sm text-gray-500">
                {copy.selectMember}
              </div>
            )}
          </div>
        </div>
      )}

      {showHireModal ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
          <div className="w-full max-w-md rounded-2xl bg-white p-6 shadow-xl">
            <div className="flex items-start justify-between gap-4">
              <div>
                <h2 className="text-xl font-bold text-gray-900">{copy.hireTitle}</h2>
                <p className="mt-1 text-sm text-gray-500">{copy.hireSubtitle}</p>
              </div>
              <button onClick={() => setShowHireModal(false)} className="text-xl text-gray-400 hover:text-gray-600">
                ×
              </button>
            </div>

            {saveError ? <div className="mt-4 rounded-lg border border-red-200 bg-red-50 px-3 py-2 text-sm text-red-700">{saveError}</div> : null}

            <div className="mt-6 space-y-4">
              <div>
                <label htmlFor="hire-role" className="mb-1 block text-sm font-medium text-gray-700">{copy.role}</label>
                <select
                  id="hire-role"
                  value={hireRole}
                  onChange={(e) => setHireRole(e.target.value as AgentRole)}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none"
                >
                  {(["pm", "researcher", "trader", "risk"] as AgentRole[]).map((role) => (
                    <option key={role} value={role}>
                      {copy.roleLabels[role]}
                    </option>
                  ))}
                </select>
              </div>

              <div>
                <label htmlFor="hire-focus" className="mb-1 block text-sm font-medium text-gray-700">{copy.researchFocus}</label>
                <select
                  id="hire-focus"
                  value={hireFocus}
                  disabled={hireRole !== "researcher"}
                  onChange={(e) => setHireFocus(e.target.value as ResearchFocus)}
                  className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:outline-none disabled:cursor-not-allowed disabled:bg-gray-50"
                >
                  {(["stock", "fundamental", "macro"] as ResearchFocus[]).map((focus) => (
                    <option key={focus} value={focus}>
                      {copy.focusLabels[focus]}
                    </option>
                  ))}
                </select>
              </div>
            </div>

            <div className="mt-6 flex gap-3">
              <button
                onClick={() => setShowHireModal(false)}
                className="flex-1 rounded-lg border border-gray-300 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
              >
                {copy.cancel}
              </button>
              <button
                onClick={() => void handleHire()}
                disabled={saving}
                className="flex-1 rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {saving ? copy.creating : copy.createMember}
              </button>
            </div>
          </div>
        </div>
      ) : null}

      {fireTarget ? (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
          <div className="w-full max-w-sm rounded-2xl bg-white p-6 shadow-xl">
            <h2 className="text-xl font-bold text-gray-900">
              {copy.confirmRemoveTitle} {normalizeAgentName(fireTarget)}?
            </h2>
            <p className="mt-2 text-sm text-gray-500">{copy.confirmRemoveText}</p>
            <div className="mt-6 flex gap-3">
              <button
                onClick={() => setFireTarget(null)}
                className="flex-1 rounded-lg border border-gray-300 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50"
              >
                {copy.cancel}
              </button>
              <button
                onClick={() => void handleFire()}
                disabled={firing}
                className="flex-1 rounded-lg bg-red-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-60"
              >
                {firing ? copy.removing : copy.confirmRemove}
              </button>
            </div>
          </div>
        </div>
      ) : null}
    </div>
  );
};

export default TeamManagement;
