import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useParams } from "react-router-dom";
import { CartesianGrid, Legend, Line, LineChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import { apiGet, apiPost, formatApiError, listFundLLMCatalog, type FundLLMCatalogEntry } from "../lib/api";
import { ABShadowAgentPanel } from "../components/ABShadowAgentPanel";
import { ABOperationalAttributionTable } from "../components/ABOperationalAttributionTable";
import {
  formatDateForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

type TestStatus = "draft" | "running" | "completed" | "analyzed" | string;
type VariableType = "model_change" | "strategy_compare" | string;

interface ABTestResults {
  variantA?: Record<string, number>;
  variantB?: Record<string, number>;
  winner?: string;
  navSeries?: ABTestNAVPoint[];
  decisionDiffs?: ABTestDecisionDiff[];
  variantATrades?: ABTestVariantTrade[];
  variantBTrades?: ABTestVariantTrade[];
  confidence?: ABTestConfidenceSummary;
  scorecard?: ABTestScorecard;
}

interface ABTestNAVPoint {
  date: string;
  variantA?: number;
  variantB?: number;
  variantAReturn?: number;
  variantBReturn?: number;
  excessReturn?: number;
}

interface ABTestVariantTrade {
  date: string;
  variantKey: string;
  symbol: string;
  side: string;
  quantity: number;
  price: number;
  notional: number;
  realizedPnL: number;
  reasoning?: string;
}

interface ABTestDecisionDiff {
  date: string;
  symbol: string;
  variantAAction?: string;
  variantBAction?: string;
  returnImpact: number;
  explanation?: string;
}

interface ABTestConfidenceSummary {
  level: string;
  score: number;
  sampleDays: number;
  tradeCount: number;
  warnings?: string[];
  recommendation?: string;
}

interface ABTestScorecard {
  recommendedVariant: string;
  variantAScore: number;
  variantBScore: number;
  scoreGap: number;
  components: ABTestScoreComponent[];
  riskNotes?: string[];
  costNotes?: string[];
  verdict?: string;
}

interface ABTestScoreComponent {
  key: string;
  label: string;
  variantA: number;
  variantB: number;
  contribution: number;
  direction: string;
  explanation?: string;
}

interface ABTestPromotedAgent {
  agentId: string;
  agentName?: string;
  role?: string;
  appliedMode: string;
  lessonCount: number;
  learningEventCount: number;
  latestTradingDate?: string;
  lessons?: string[];
  adjustments?: string[];
  promotedReflectionIds?: string[];
  promotedSkillKeys?: string[];
}

interface ABTestPromotionSkip {
  agentId?: string;
  reason: string;
}

interface ABTestLearningPromotionResult {
  testId: string;
  variantKey: string;
  mode: "merge" | "overwrite" | string;
  dryRun: boolean;
  updatedAgents: ABTestPromotedAgent[];
  skippedAgents?: ABTestPromotionSkip[];
  warnings?: string[];
}

interface ABTestLearningPromotionRecord {
  id: string;
  testId: string;
  variantKey: string;
  variantName?: string;
  agentId: string;
  agentName?: string;
  mode: string;
  promotedBy?: string;
  promotedAt: string;
}

interface ABTestLearningRollbackResult {
  promotionId: string;
  testId: string;
  agentId: string;
  agentName?: string;
  rolledBack: boolean;
  rolledBackReflectionIds?: string[];
  skillKeysReverted?: string[];
}

interface ApiABTest {
  id: string;
  name: string;
  controlFundId: string;
  treatmentFundId: string;
  variableType: VariableType;
  variableConfig?: unknown;
  status: TestStatus;
  startDate?: string;
  endDate?: string;
  results?: ABTestResults;
  createdAt: string;
  updatedAt: string;
}

interface VariableConfigView {
  oldValue: string;
  newValue: string;
  durationDays: number | null;
  variantAName: string;
  variantBName: string;
  strategySummary: string;
}

interface CreateTestFormData {
  name: string;
  variantAName: string;
  variantBName: string;
  strategySummary: string;
  pmStyle: string;
  maxSinglePosition: number;
  durationDays: number;
  // The mode toggle at the top of the create modal. We intentionally
  // store the mode in the form rather than as a separate piece of
  // state so the controlled inputs keep their last value when the
  // operator flips the toggle and back ("oh wait, I did want
  // strategy compare after all"). Both modes round-trip through
  // /api/abtests with variableType='strategy_compare' — the LLM
  // fields are read by the B-side decider as additional knobs in
  // strategyConfig (see ab_shadow_bside.go::abStrategyTradeScale).
  // We deliberately do NOT submit variableType='model_change' here
  // because that path requires control_fund != treatment_fund (a
  // separately provisioned fund) and the in-modal flow is single-
  // fund; if/when a multi-fund picker ships, this becomes the place
  // to switch.
  mode: "strategy" | "model";
  // LLM hints. Empty = "use the fund's default LLM, don't override".
  // The provider field is the platform string ('openai', 'claude',
  // 'qwen', ...) — we constrain it to the catalog response so the
  // server-side router can resolve it without a free-text guard.
  variantAProvider: string;
  variantAModelName: string;
  variantBProvider: string;
  variantBModelName: string;
}

const INITIAL_FORM: CreateTestFormData = {
  name: "",
  variantAName: "当前策略",
  variantBName: "实验策略",
  strategySummary: "",
  pmStyle: "aggressive",
  maxSinglePosition: 20,
  durationDays: 30,
  mode: "strategy",
  variantAProvider: "",
  variantAModelName: "",
  variantBProvider: "",
  variantBModelName: "",
};

function addDays(date: Date, days: number): Date {
  const next = new Date(date);
  next.setDate(next.getDate() + days);
  return next;
}

function toDateString(date: Date): string {
  return date.toISOString().slice(0, 10);
}

function parseVariableConfig(raw: unknown): VariableConfigView {
  if (!raw || typeof raw !== "object") {
    return { oldValue: "", newValue: "", durationDays: null, variantAName: "", variantBName: "", strategySummary: "" };
  }

  const source = raw as Record<string, unknown>;
  const variantA = typeof source.variantA === "object" && source.variantA ? (source.variantA as Record<string, unknown>) : {};
  const variantB = typeof source.variantB === "object" && source.variantB ? (source.variantB as Record<string, unknown>) : {};
  const variantBStrategy = typeof variantB.strategyConfig === "object" && variantB.strategyConfig ? (variantB.strategyConfig as Record<string, unknown>) : {};
  const oldValueCandidates = [source.oldValue, source.controlModel, source.fromModel, source.baselineModel];
  const newValueCandidates = [source.newValue, source.treatmentModel, source.toModel, source.targetModel];
  const durationCandidates = [source.durationDays, source.duration, source.windowDays];

  const variantAName = (typeof variantA.name === "string" ? variantA.name : "") || "当前策略";
  const variantBName = (typeof variantB.name === "string" ? variantB.name : "") || "实验策略";
  const strategySummary =
    (typeof source.strategySummary === "string" ? source.strategySummary : "") ||
    (typeof variantBStrategy.summary === "string" ? variantBStrategy.summary : "") ||
    "";
  const oldValue = oldValueCandidates.find((value): value is string => typeof value === "string") ?? variantAName;
  const newValue = newValueCandidates.find((value): value is string => typeof value === "string") ?? variantBName;
  const durationValue = durationCandidates.find((value) => typeof value === "number" || typeof value === "string");
  const durationDays = typeof durationValue === "number" ? durationValue : typeof durationValue === "string" ? Number(durationValue) : null;

  return {
    oldValue,
    newValue,
    durationDays: typeof durationDays === "number" && Number.isFinite(durationDays) ? durationDays : null,
    variantAName,
    variantBName,
    strategySummary,
  };
}

const CreateTestModal: React.FC<{
  open: boolean;
  fundId: string;
  saving: boolean;
  error: string | null;
  language: "zh-CN" | "en-US";
  onClose: () => void;
  onCreate: (data: CreateTestFormData) => Promise<void>;
}> = ({ open, fundId, saving, error, language, onClose, onCreate }) => {
  const [form, setForm] = useState<CreateTestFormData>({ ...INITIAL_FORM });

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Create A/B test",
            subtitle: "Compare two strategy variants under the same fund and review return metrics after analysis.",
            controlFund: "Fund",
            name: "Test name",
            namePlaceholder: "For example: aggressive allocation vs current policy",
            variantA: "Variant A name",
            variantAPlaceholder: "Current strategy",
            variantB: "Variant B name",
            variantBPlaceholder: "Aggressive strategy",
            strategySummary: "Strategy B change summary",
            strategySummaryPlaceholder: "For example: increase max position and use aggressive PM style",
            pmStyle: "PM style",
            maxSinglePosition: "Max single position (%)",
            duration: "Duration (days)",
            cancel: "Cancel",
            create: "Create test",
            creating: "Creating...",
            modeLabel: "Compare what?",
            modeStrategy: "Strategy",
            modeStrategyHint: "Compare PM style / position limits / summary text. LLM is optional.",
            modeModel: "LLM model",
            modeModelHint: "Compare two LLM provider+model pairs against the same strategy.",
            llmSection: "LLM (optional)",
            llmSectionRequired: "LLM (required)",
            llmHint: "Empty = inherit fund default. Pick from the platform catalog.",
            providerLabel: "Provider",
            providerPlaceholder: "openai / claude / qwen / ...",
            modelLabel: "Model name",
            modelPlaceholder: "gpt-4o / claude-opus-4 / ...",
            variantAGroup: "A group LLM",
            variantBGroup: "B group LLM",
            llmCatalogLoading: "Loading providers…",
            llmCatalogEmpty: "No providers configured. Ask an admin to enable at least one.",
            llmCatalogError: "Failed to load providers; you can still type values manually.",
          }
        : {
            title: "新建 A/B 测试",
            subtitle: "在当前基金下配置 A/B 两套策略，分析后对比收益、回撤、波动和交易表现。",
            controlFund: "当前基金",
            name: "测试名称",
            namePlaceholder: "例如：激进仓位策略 vs 当前策略",
            variantA: "A 组名称",
            variantAPlaceholder: "当前策略",
            variantB: "B 组名称",
            variantBPlaceholder: "激进策略",
            strategySummary: "B 组策略变化说明",
            strategySummaryPlaceholder: "例如：提高单票上限，PM 风格改为 aggressive",
            pmStyle: "PM 风格",
            maxSinglePosition: "单票最大仓位（%）",
            duration: "测试周期（天）",
            cancel: "取消",
            create: "创建测试",
            creating: "创建中...",
            modeLabel: "对比维度",
            modeStrategy: "策略",
            modeStrategyHint: "对比 PM 风格 / 仓位上限 / 策略说明，LLM 字段可选填。",
            modeModel: "模型",
            modeModelHint: "在同一策略下，对比两个 LLM provider+model。",
            llmSection: "LLM 模型（可选）",
            llmSectionRequired: "LLM 模型（必填）",
            llmHint: "留空 = 沿用基金默认 LLM。下拉选项来自平台 LLM 目录。",
            providerLabel: "Provider",
            providerPlaceholder: "openai / claude / qwen / ...",
            modelLabel: "模型名称",
            modelPlaceholder: "gpt-4o / claude-opus-4 / ...",
            variantAGroup: "A 组使用的 LLM",
            variantBGroup: "B 组使用的 LLM",
            llmCatalogLoading: "正在加载 provider 列表…",
            llmCatalogEmpty: "尚未配置任何 provider，请联系管理员启用。",
            llmCatalogError: "provider 列表加载失败，可手动输入。",
          },
    [language],
  );

  useEffect(() => {
    if (!open) {
      setForm({ ...INITIAL_FORM });
    }
  }, [open]);

  // Load the LLM catalog on first open. Empty fundId = no fetch (the
  // dialog can't submit anyway). Errors are non-fatal: the operator
  // can still type provider/model strings by hand and the server-side
  // router will validate them at create-time. We hold the result in
  // a ref-shaped state because the dropdown listens to it but the
  // form values track separate refs.
  const [catalog, setCatalog] = useState<FundLLMCatalogEntry[] | null>(null);
  const [catalogStatus, setCatalogStatus] = useState<"idle" | "loading" | "empty" | "error">("idle");
  useEffect(() => {
    if (!open || !fundId) {
      return;
    }
    let cancelled = false;
    setCatalogStatus("loading");
    listFundLLMCatalog(fundId)
      .then((res) => {
        if (cancelled) return;
        const list = res.providers ?? [];
        setCatalog(list);
        setCatalogStatus(list.length === 0 ? "empty" : "idle");
      })
      .catch(() => {
        if (cancelled) return;
        setCatalog(null);
        setCatalogStatus("error");
      });
    return () => {
      cancelled = true;
    };
  }, [open, fundId]);

  // Distinct providers, preserving the catalog order so the platform
  // default sorts to the top (the API surfaces it via
  // is_platform_default and we keep ListAll's default ORDER BY).
  const providerOptions = useMemo(() => {
    if (!catalog) return [] as string[];
    const seen = new Set<string>();
    const ordered: string[] = [];
    for (const row of catalog) {
      if (!row.provider || seen.has(row.provider)) continue;
      seen.add(row.provider);
      ordered.push(row.provider);
    }
    return ordered;
  }, [catalog]);

  // Models scoped to the selected provider. When provider is empty we
  // surface the full union so the operator can pick a model first
  // and we'll auto-fill the provider on selection (handled by the
  // controlled <select> via a "{provider}|{model}" composite value
  // — see the model <select> below).
  const modelsForProvider = useCallback(
    (provider: string): FundLLMCatalogEntry[] => {
      if (!catalog) return [];
      const trimmed = provider.trim();
      if (!trimmed) return catalog.filter((row) => !!row.model_name);
      return catalog.filter((row) => row.provider === trimmed && !!row.model_name);
    },
    [catalog],
  );

  if (!open) {
    return null;
  }

  const update = <K extends keyof CreateTestFormData>(key: K, value: CreateTestFormData[K]) => {
    setForm((current) => ({ ...current, [key]: value }));
  };

  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 p-4">
      <div className="w-full max-w-xl rounded-2xl bg-white p-6 shadow-2xl">
        <div className="flex items-start justify-between gap-4">
          <div>
            <h2 className="text-xl font-bold text-gray-900">{copy.title}</h2>
            <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
          </div>
          <button type="button" onClick={onClose} className="text-2xl leading-none text-gray-400 hover:text-gray-700">
            ×
          </button>
        </div>

        <form
          className="mt-6 space-y-4"
          onSubmit={(event) => {
            event.preventDefault();
            void onCreate(form);
          }}
        >
          <div className="rounded-xl border border-gray-200 bg-gray-50 p-4 text-sm text-gray-600">
            <p>{copy.controlFund}</p>
            <p className="mt-1 break-all font-mono text-xs text-gray-800">{fundId}</p>
          </div>

          {/* Mode toggle: strategy vs model. We render it as two
              radio-style buttons rather than a real <input
              type="radio"> so the active state can flip on click
              without the form re-emitting an onChange storm. The
              hint text below changes per mode so the operator
              understands what each mode submits. */}
          <div>
            <label className="mb-2 block text-sm font-medium text-gray-700">{copy.modeLabel}</label>
            <div className="grid grid-cols-2 gap-2">
              <button
                type="button"
                onClick={() => update("mode", "strategy")}
                className={`rounded-xl border px-4 py-2.5 text-sm font-medium transition ${form.mode === "strategy" ? "border-indigo-500 bg-indigo-50 text-indigo-700" : "border-gray-300 bg-white text-gray-700 hover:border-gray-400"}`}
              >
                {copy.modeStrategy}
              </button>
              <button
                type="button"
                onClick={() => update("mode", "model")}
                className={`rounded-xl border px-4 py-2.5 text-sm font-medium transition ${form.mode === "model" ? "border-indigo-500 bg-indigo-50 text-indigo-700" : "border-gray-300 bg-white text-gray-700 hover:border-gray-400"}`}
              >
                {copy.modeModel}
              </button>
            </div>
            <p className="mt-1.5 text-xs text-gray-500">
              {form.mode === "strategy" ? copy.modeStrategyHint : copy.modeModelHint}
            </p>
          </div>

          <div>
            <label className="mb-2 block text-sm font-medium text-gray-700">{copy.name}</label>
            <input
              required
              value={form.name}
              onChange={(event) => update("name", event.target.value)}
              className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
              placeholder={copy.namePlaceholder}
            />
          </div>

          <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
            <div>
              <label className="mb-2 block text-sm font-medium text-gray-700">{copy.variantA}</label>
              <input
                required
                value={form.variantAName}
                onChange={(event) => update("variantAName", event.target.value)}
                className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                placeholder={copy.variantAPlaceholder}
              />
            </div>
            <div>
              <label className="mb-2 block text-sm font-medium text-gray-700">{copy.variantB}</label>
              <input
                required
                value={form.variantBName}
                onChange={(event) => update("variantBName", event.target.value)}
                className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                placeholder={copy.variantBPlaceholder}
              />
            </div>
          </div>

          <div>
            <label className="mb-2 block text-sm font-medium text-gray-700">{copy.strategySummary}</label>
            <textarea
              required
              value={form.strategySummary}
              onChange={(event) => update("strategySummary", event.target.value)}
              className="min-h-24 w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
              placeholder={copy.strategySummaryPlaceholder}
            />
          </div>

          {/* LLM picker block. In "strategy" mode it's optional and
              renders as a collapsible-feeling section; in "model"
              mode it's the headline of the experiment so we mark
              both A/B providers as required. The catalog query is
              auth-scoped to the fund so an empty list = "platform
              hasn't enabled any provider for you", which we surface
              explicitly rather than just hiding the dropdown. */}
          <div className="rounded-xl border border-gray-200 bg-gray-50 p-4">
            <div className="flex items-baseline justify-between">
              <h3 className="text-sm font-semibold text-gray-800">
                {form.mode === "model" ? copy.llmSectionRequired : copy.llmSection}
              </h3>
              <span className="text-xs text-gray-500">
                {catalogStatus === "loading"
                  ? copy.llmCatalogLoading
                  : catalogStatus === "empty"
                    ? copy.llmCatalogEmpty
                    : catalogStatus === "error"
                      ? copy.llmCatalogError
                      : copy.llmHint}
              </span>
            </div>
            <div className="mt-3 grid grid-cols-1 gap-4 md:grid-cols-2">
              {(["A", "B"] as const).map((side) => {
                const providerKey = side === "A" ? "variantAProvider" : "variantBProvider";
                const modelKey = side === "A" ? "variantAModelName" : "variantBModelName";
                const providerVal = form[providerKey];
                const modelVal = form[modelKey];
                const isModelRequired = form.mode === "model";
                const sideLabel = side === "A" ? copy.variantAGroup : copy.variantBGroup;
                const modelOptions = modelsForProvider(providerVal);
                return (
                  <div key={side}>
                    <p className="mb-2 text-xs font-medium uppercase tracking-wide text-gray-500">{sideLabel}</p>
                    <label className="mb-1 block text-xs text-gray-600">{copy.providerLabel}</label>
                    {providerOptions.length > 0 ? (
                      <select
                        required={isModelRequired}
                        value={providerVal}
                        onChange={(event) => {
                          const next = event.target.value;
                          update(providerKey, next);
                          // When the operator switches provider,
                          // wipe the model so we don't end up
                          // submitting a (claude, gpt-4o) pair.
                          if (form[modelKey]) {
                            const stillValid = modelsForProvider(next).some((row) => row.model_name === form[modelKey]);
                            if (!stillValid) update(modelKey, "");
                          }
                        }}
                        className="w-full rounded-xl border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500"
                      >
                        <option value="">{form.mode === "model" ? "—" : `(${copy.llmHint.split("。")[0]})`}</option>
                        {providerOptions.map((p) => (
                          <option key={p} value={p}>
                            {p}
                          </option>
                        ))}
                      </select>
                    ) : (
                      <input
                        required={isModelRequired}
                        value={providerVal}
                        onChange={(event) => update(providerKey, event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500"
                        placeholder={copy.providerPlaceholder}
                      />
                    )}
                    <label className="mb-1 mt-2 block text-xs text-gray-600">{copy.modelLabel}</label>
                    {modelOptions.length > 0 ? (
                      <select
                        required={isModelRequired}
                        value={modelVal}
                        onChange={(event) => {
                          const next = event.target.value;
                          update(modelKey, next);
                          // If the operator picked a model without
                          // first selecting a provider, infer the
                          // provider from the catalog row so the
                          // server-side router gets a valid pair.
                          if (next && !providerVal) {
                            const inferred = modelOptions.find((row) => row.model_name === next);
                            if (inferred) update(providerKey, inferred.provider);
                          }
                        }}
                        className="w-full rounded-xl border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500"
                      >
                        <option value="">—</option>
                        {modelOptions.map((row, idx) => (
                          <option
                            key={`${row.provider}-${row.label ?? ""}-${row.model_name}-${idx}`}
                            value={row.model_name ?? ""}
                          >
                            {row.model_name}
                            {row.label ? ` · ${row.label}` : ""}
                            {row.is_platform_default ? " ★" : ""}
                          </option>
                        ))}
                      </select>
                    ) : (
                      <input
                        required={isModelRequired}
                        value={modelVal}
                        onChange={(event) => update(modelKey, event.target.value)}
                        className="w-full rounded-xl border border-gray-300 px-3 py-2 text-sm outline-none focus:border-indigo-500"
                        placeholder={copy.modelPlaceholder}
                      />
                    )}
                  </div>
                );
              })}
            </div>
          </div>

          {form.mode === "strategy" ? (
            <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
              <div>
                <label className="mb-2 block text-sm font-medium text-gray-700">{copy.pmStyle}</label>
                <select
                  value={form.pmStyle}
                  onChange={(event) => update("pmStyle", event.target.value)}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                >
                  <option value="conservative">conservative</option>
                  <option value="balanced">balanced</option>
                  <option value="aggressive">aggressive</option>
                  <option value="value">value</option>
                  <option value="growth">growth</option>
                </select>
              </div>
              <div>
                <label className="mb-2 block text-sm font-medium text-gray-700">{copy.maxSinglePosition}</label>
                <input
                  required
                  min={1}
                  max={100}
                  type="number"
                  value={form.maxSinglePosition}
                  onChange={(event) => update("maxSinglePosition", Number(event.target.value))}
                  className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
                />
              </div>
            </div>
          ) : null}

          <div>
            <label className="mb-2 block text-sm font-medium text-gray-700">{copy.duration}</label>
            <input
              required
              min={1}
              max={365}
              type="number"
              value={form.durationDays}
              onChange={(event) => update("durationDays", Number(event.target.value))}
              className="w-full rounded-xl border border-gray-300 px-4 py-2.5 text-sm outline-none transition focus:border-indigo-500"
            />
          </div>

          {error ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{error}</div> : null}

          <div className="flex flex-col gap-3 sm:flex-row sm:justify-end">
            <button
              type="button"
              onClick={onClose}
              disabled={saving}
              className="rounded-lg border border-gray-300 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {copy.cancel}
            </button>
            <button
              type="submit"
              disabled={saving}
              className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {saving ? copy.creating : copy.create}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
};

const ABTestCompare: React.FC = () => {
  const { fundId } = useParams<{ fundId: string }>();
  const { language, displayCurrency } = useAppPreferences();
  const [tests, setTests] = useState<ApiABTest[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [modalOpen, setModalOpen] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);
  const [saving, setSaving] = useState(false);
  const [acting, setActing] = useState(false);
  const [promoting, setPromoting] = useState(false);
  const [promotionMode, setPromotionMode] = useState<"merge" | "overwrite">("merge");
  const [promotionResult, setPromotionResult] = useState<ABTestLearningPromotionResult | null>(null);
  const [promotionHistory, setPromotionHistory] = useState<ABTestLearningPromotionRecord[]>([]);
  const [rollingBack, setRollingBack] = useState<string | null>(null);
  const [rollbackResult, setRollbackResult] = useState<ABTestLearningRollbackResult | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            loading: "Loading A/B tests...",
            missingFundId: "Missing fundId",
            loadFailed: "Failed to load A/B tests",
            title: "Strategy A/B return comparison",
            subtitle: "Compare strategy variants under this fund and review return, drawdown, volatility, and trade metrics.",
            totalTests: "Total tests",
            create: "Create test",
            emptyTitle: "No A/B tests yet",
            emptyDescription: "Create a strategy comparison first, then review draft, running, and analyzed return results here.",
            createNow: "Create now",
            testList: "Tests",
            createdAt: "Created",
            notSet: "Not set",
            selectPrompt: "Select a test from the left to view details.",
            testType: "Test type",
            modelCompare: "Strategy comparison",
            strategyChange: "Strategy B change",
            timeWindow: "Time window",
            duration: "Duration",
            days: "days",
            controlFund: "Variant A fund ID",
            treatmentFund: "Variant B fund ID",
            start: "Start test",
            stop: "Stop test",
            analyze: "Generate analysis",
            statusTitle: "Current status",
            draftState: "This test is still a draft. Confirm the parameters and treatment fund before starting it.",
            runningState: "This test is still running. Once completed or manually stopped, analysis can begin.",
            completedState: "This test is complete. Generate an analysis next to compare both variants.",
            emptyState: "This test does not have any analysis data yet.",
            summaryTitle: "Result summary",
            curveTitle: "Cumulative return curve",
            confidenceTitle: "Conclusion confidence",
            scorecardTitle: "Strategy review scorecard",
            recommendedVariant: "Recommended variant",
            scoreGap: "Score gap",
            scoreBreakdown: "Score breakdown",
            riskNotes: "Risk notes",
            costNotes: "Cost notes",
            confidenceScore: "Confidence score",
            sampleDays: "Sample days",
            sampleTrades: "Sample trades",
            confidenceLow: "Low",
            confidenceMedium: "Medium",
            confidenceHigh: "High",
            decisionDiffTitle: "Decision differences",
            tradeDetailTitle: "Shadow trade details",
            date: "Date",
            symbol: "Symbol",
            actionA: "A action",
            actionB: "B action",
            impact: "Impact",
            explanation: "Explanation",
            side: "Side",
            quantity: "Qty",
            price: "Price",
            notional: "Notional",
            pnl: "P&L",
            reasoning: "Reasoning",
            noDecisionDiffs: "No decision-level differences have been recorded yet.",
            noTrades: "No shadow trade details have been recorded yet.",
            promotionTitle: "Apply winning shadow learning",
            promotionDescription: "Preview or apply the winning variant's shadow learning to the real team agents. Merge is safer; overwrite should be used only after review.",
            promotionMode: "Apply mode",
            mergeMode: "Merge learning",
            overwriteMode: "Overwrite learning",
            dryRun: "Dry-run preview",
            applyPromotion: "Apply learning",
            applying: "Applying...",
            previewing: "Previewing...",
            promotionWarning: "This updates real agent evolution config when applied. Use dry-run first and review confidence, decisions, and trades.",
            promotionResult: "Promotion result",
            affectedAgents: "Affected agents",
            skippedAgents: "Skipped agents",
            promotionHistory: "Promotion history",
            rollback: "Rollback",
            rollingBack: "Rolling back...",
            rollbackDone: "Rollback completed",
            noPromotionHistory: "No applied promotion records yet.",
            lessons: "Lessons",
            adjustments: "Adjustments",
            noPromotionResult: "Run dry-run preview before applying the shadow learning.",
            excessReturn: "Excess return",
            winnerPrefix: "Winner:",
            winnerTreatment: "Treatment",
            winnerControl: "Control",
            winnerNone: "No clear winner",
            control: "A strategy",
            treatment: "B strategy",
            actionsFailed: {
              start: "Failed to start test",
              stop: "Failed to stop test",
              analyze: "Failed to analyze test",
              create: "Failed to create test",
              promote: "Failed to promote shadow learning",
              rollback: "Failed to rollback promotion",
            },
            statusLabels: {
              draft: "Draft",
              running: "Running",
              completed: "Completed",
              analyzed: "Analyzed",
            } as Record<string, string>,
            variableTypes: {
              model_change: "Model change",
              strategy_compare: "Strategy comparison",
            } as Record<string, string>,
            metrics: {
              startNav: "Start NAV",
              endNav: "End NAV",
              latestNav: "Latest NAV",
              totalReturn: "Total return",
              annualizedReturn: "Annualized return",
              maxDrawdown: "Max drawdown",
              volatility: "Volatility",
              sharpe: "Sharpe",
              assetReturn: "Asset return",
              totalAssets: "Total assets",
              startAssets: "Start assets",
              endAssets: "End assets",
              tradeCount: "Trade count",
              totalFees: "Total fees",
              totalTurnover: "Turnover",
              realizedPnL: "Realized P&L",
              navPoints: "NAV points",
            } as Record<string, string>,
          }
        : {
            loading: "正在加载 A/B 测试...",
            missingFundId: "缺少 fundId",
            loadFailed: "加载 A/B 测试失败",
            title: "策略 A/B 收益对比",
            subtitle: "在当前基金下对比 A/B 策略，查看收益、回撤、波动与交易表现。",
            totalTests: "测试总数",
            create: "新建测试",
            emptyTitle: "当前还没有 A/B 测试",
            emptyDescription: "先创建一个策略对比实验，后续即可在这里查看草稿、运行态与收益分析结果。",
            createNow: "立即创建",
            testList: "测试列表",
            createdAt: "创建于",
            notSet: "未设置",
            selectPrompt: "请选择左侧测试查看详情。",
            testType: "测试类型",
            modelCompare: "策略对比",
            strategyChange: "B 组策略变化",
            timeWindow: "时间窗口",
            duration: "实验周期",
            days: "天",
            controlFund: "A 组基金标识",
            treatmentFund: "B 组基金标识",
            start: "启动测试",
            stop: "停止测试",
            analyze: "生成分析",
            statusTitle: "当前状态",
            draftState: "该测试当前仍是草稿，请先确认参数与实验基金后再启动。",
            runningState: "该测试正在运行中，完成或手动停止后即可进入结果分析。",
            completedState: "该测试已完成，接下来可以生成分析结果并比较两组表现。",
            emptyState: "该测试暂时还没有可展示的分析数据。",
            summaryTitle: "结果摘要",
            curveTitle: "累计收益曲线",
            confidenceTitle: "结论置信度",
            scorecardTitle: "策略评审评分卡",
            recommendedVariant: "推荐版本",
            scoreGap: "分差",
            scoreBreakdown: "评分拆解",
            riskNotes: "风险归因",
            costNotes: "成本归因",
            confidenceScore: "置信度分数",
            sampleDays: "样本天数",
            sampleTrades: "交易样本",
            confidenceLow: "低",
            confidenceMedium: "中",
            confidenceHigh: "高",
            decisionDiffTitle: "决策差异",
            tradeDetailTitle: "影子交易明细",
            date: "日期",
            symbol: "标的",
            actionA: "A 操作",
            actionB: "B 操作",
            impact: "收益影响",
            explanation: "解释",
            side: "方向",
            quantity: "数量",
            price: "价格",
            notional: "成交额",
            pnl: "盈亏",
            reasoning: "理由",
            noDecisionDiffs: "暂未记录到决策级差异。",
            noTrades: "暂未记录到影子交易明细。",
            promotionTitle: "应用胜出影子学习",
            promotionDescription: "将胜出 variant 的影子学习预览或应用到真实团队 agent。merge 更安全；overwrite 仅建议在充分复核后使用。",
            promotionMode: "应用方式",
            mergeMode: "合并学习",
            overwriteMode: "覆盖学习",
            dryRun: "Dry-run 预览",
            applyPromotion: "应用学习",
            applying: "应用中...",
            previewing: "预览中...",
            promotionWarning: "正式应用会更新真实 agent evolution_config。建议先 dry-run，并结合置信度、决策差异和交易明细复核。",
            promotionResult: "应用结果",
            affectedAgents: "影响 Agent",
            skippedAgents: "跳过 Agent",
            promotionHistory: "应用历史",
            rollback: "回滚",
            rollingBack: "回滚中...",
            rollbackDone: "回滚完成",
            noPromotionHistory: "暂无正式应用记录。",
            lessons: "学习结论",
            adjustments: "建议调整",
            noPromotionResult: "请先执行 dry-run 预览，再正式应用影子学习。",
            excessReturn: "超额收益",
            winnerPrefix: "胜出版本：",
            winnerTreatment: "实验组",
            winnerControl: "对照组",
            winnerNone: "未分出明显胜者",
            control: "A 策略",
            treatment: "B 策略",
            actionsFailed: {
              start: "启动测试失败",
              stop: "停止测试失败",
              analyze: "分析测试失败",
              create: "创建测试失败",
              promote: "应用影子学习失败",
              rollback: "回滚应用记录失败",
            },
            statusLabels: {
              draft: "草稿",
              running: "运行中",
              completed: "已完成",
              analyzed: "已分析",
            } as Record<string, string>,
            variableTypes: {
              model_change: "模型变更",
              strategy_compare: "策略对比",
            } as Record<string, string>,
            metrics: {
              startNav: "起始单位净值",
              endNav: "结束单位净值",
              latestNav: "最新单位净值",
              totalReturn: "累计收益率",
              annualizedReturn: "年化收益率",
              maxDrawdown: "最大回撤",
              volatility: "波动率",
              sharpe: "夏普比率",
              assetReturn: "资产收益率",
              totalAssets: "总资产",
              startAssets: "起始资产",
              endAssets: "结束资产",
              tradeCount: "成交笔数",
              totalFees: "总费用",
              totalTurnover: "成交额",
              realizedPnL: "已实现盈亏",
              navPoints: "净值点数",
            } as Record<string, string>,
          },
    [language],
  );

  const statusMeta = useCallback(
    (status: string) => {
      const badgeMap: Record<string, string> = {
        draft: "bg-gray-100 text-gray-700 border-gray-200",
        running: "bg-blue-50 text-blue-700 border-blue-200",
        completed: "bg-amber-50 text-amber-700 border-amber-200",
        analyzed: "bg-emerald-50 text-emerald-700 border-emerald-200",
      };
      return {
        label: (copy.statusLabels[status] ?? status) || copy.notSet,
        badge: badgeMap[status] ?? "bg-gray-100 text-gray-700 border-gray-200",
      };
    },
    [copy.notSet, copy.statusLabels],
  );

  const variableTypeLabel = useCallback(
    (value: string) => copy.variableTypes[value] ?? value.replace(/[_-]+/g, " "),
    [copy.variableTypes],
  );

  const metricLabel = useCallback(
    (key: string) => copy.metrics[key] ?? key,
    [copy.metrics],
  );

  const formatMetric = useCallback(
    (key: string, value?: number) => {
      if (typeof value !== "number" || Number.isNaN(value)) {
        return "—";
      }
      if (key === "totalAssets" || key === "startAssets" || key === "endAssets" || key === "totalFees" || key === "totalTurnover" || key === "realizedPnL") {
        return formatMoneyForDisplay(value, "USD", displayCurrency, language);
      }
      if (key === "latestNav" || key === "startNav" || key === "endNav") {
        return formatNumberForLanguage(value, language, { minimumFractionDigits: 4, maximumFractionDigits: 4 });
      }
      const lowerKey = key.toLowerCase();
      if (lowerKey.includes("rate") || lowerKey.includes("return") || lowerKey.includes("drawdown") || lowerKey.includes("volatility")) {
        return `${formatNumberForLanguage(value, language, { minimumFractionDigits: 2, maximumFractionDigits: 2 })}%`;
      }
      if (Number.isInteger(value)) {
        return formatNumberForLanguage(value, language);
      }
      return formatNumberForLanguage(value, language, { minimumFractionDigits: 2, maximumFractionDigits: 2 });
    },
    [displayCurrency, language],
  );

  // We keep the localized copy strings in refs so loadTests's identity
  // does not flip every time PreferencesProvider re-issues the `copy`
  // memo (e.g. on language switch or displayCurrency change). Without
  // this guard, useEffect would re-fire, setLoading(true) would run
  // again, and any in-flight fetch race could leave the UI stuck on
  // "Loading…" indefinitely after a transient page interaction.
  const copyRef = useRef(copy);
  useEffect(() => {
    copyRef.current = copy;
  }, [copy]);

  const loadTests = useCallback(
    async (opts?: { silent?: boolean }) => {
      if (!fundId) {
        setError(copyRef.current.missingFundId);
        setLoading(false);
        return;
      }

      if (!opts?.silent) {
        setLoading(true);
      }
      setError(null);
      try {
        const response = await apiGet<ApiABTest[]>(`/api/funds/${fundId}/abtests`);
        const nextTests = (response ?? []).slice().sort((a, b) => b.createdAt.localeCompare(a.createdAt));
        setTests(nextTests);
        setSelectedId((current) => {
          if (current && nextTests.some((test) => test.id === current)) {
            return current;
          }
          return nextTests[0]?.id ?? null;
        });
      } catch (err) {
        setError(formatApiError(err, copyRef.current.loadFailed));
      } finally {
        setLoading(false);
      }
    },
    [fundId],
  );

  useEffect(() => {
    void loadTests();
  }, [loadTests]);

  const selected = useMemo(() => tests.find((test) => test.id === selectedId) ?? null, [selectedId, tests]);
  const selectedConfig = useMemo(() => parseVariableConfig(selected?.variableConfig), [selected]);

  const resultKeys = useMemo(() => {
    if (!selected?.results) {
      return [];
    }
    const controlKeys = Object.keys(selected.results.variantA ?? {});
    const treatmentKeys = Object.keys(selected.results.variantB ?? {});
    const preferredOrder = [
      "totalReturn",
      "annualizedReturn",
      "maxDrawdown",
      "volatility",
      "sharpe",
      "startNav",
      "endNav",
      "startAssets",
      "endAssets",
      "tradeCount",
      "totalTurnover",
      "totalFees",
      "realizedPnL",
      "navPoints",
    ];
    return Array.from(new Set([...controlKeys, ...treatmentKeys])).sort((a, b) => {
      const ai = preferredOrder.indexOf(a);
      const bi = preferredOrder.indexOf(b);
      if (ai >= 0 && bi >= 0) return ai - bi;
      if (ai >= 0) return -1;
      if (bi >= 0) return 1;
      return a.localeCompare(b);
    });
  }, [selected]);

  const navSeries = useMemo(() => selected?.results?.navSeries ?? [], [selected]);
  const decisionDiffs = useMemo(() => selected?.results?.decisionDiffs ?? [], [selected]);
  const shadowTrades = useMemo(() => [...(selected?.results?.variantATrades ?? []), ...(selected?.results?.variantBTrades ?? [])], [selected]);
  const confidence = selected?.results?.confidence;
  const scorecard = selected?.results?.scorecard;
  const latestSeriesPoint = navSeries.length > 0 ? navSeries[navSeries.length - 1] : null;
  const latestExcessReturn = latestSeriesPoint?.excessReturn;

  const confidenceLabel = useCallback(
    (level?: string) => {
      if (level === "high") return copy.confidenceHigh;
      if (level === "medium") return copy.confidenceMedium;
      return copy.confidenceLow;
    },
    [copy.confidenceHigh, copy.confidenceLow, copy.confidenceMedium],
  );

  const upsertTest = useCallback((updated: ApiABTest) => {
    setTests((prev) => {
      const exists = prev.some((test) => test.id === updated.id);
      const next = exists ? prev.map((test) => (test.id === updated.id ? updated : test)) : [updated, ...prev];
      return next.slice().sort((a, b) => b.createdAt.localeCompare(a.createdAt));
    });
    setSelectedId(updated.id);
  }, []);

  const handleCreate = useCallback(
    async (data: CreateTestFormData) => {
      if (!fundId) {
        return;
      }
      setSaving(true);
      setActionError(null);
      try {
        const startDate = new Date();
        const endDate = addDays(startDate, Math.max(data.durationDays - 1, 0));
        // Trim once and reuse — the server treats whitespace-only
        // strings as "set" but the router's lookup will then 404
        // on the (provider, model) pair. Normalising here also
        // makes the conditional spread below readable.
        const variantAProvider = data.variantAProvider.trim();
        const variantAModelName = data.variantAModelName.trim();
        const variantBProvider = data.variantBProvider.trim();
        const variantBModelName = data.variantBModelName.trim();
        // Strategy-compare is the only variableType the in-modal
        // flow submits today (model_change wants two distinct
        // funds). The LLM hints ride along in strategyConfig so a
        // future B-side decider — or operator-facing post-mortem —
        // can read what model the operator believed B should run.
        const variantAStrategyConfig: Record<string, unknown> = {
          source: "current_fund",
        };
        if (variantAProvider) variantAStrategyConfig.provider = variantAProvider;
        if (variantAModelName) variantAStrategyConfig.modelName = variantAModelName;

        const variantBStrategyConfig: Record<string, unknown> = {};
        if (data.mode === "strategy") {
          variantBStrategyConfig.pmStyle = data.pmStyle;
          variantBStrategyConfig.maxSinglePosition = data.maxSinglePosition / 100;
          variantBStrategyConfig.summary = data.strategySummary.trim();
        } else {
          // Model-compare: strategy knobs are inherited from the
          // fund's current config so the only deliberate variable
          // is the LLM. We still record the operator's free-text
          // summary so the post-analysis page can render "B was
          // run on Claude Opus 4 instead of GPT-4o".
          variantBStrategyConfig.source = "current_fund";
          variantBStrategyConfig.summary = data.strategySummary.trim();
        }
        if (variantBProvider) variantBStrategyConfig.provider = variantBProvider;
        if (variantBModelName) variantBStrategyConfig.modelName = variantBModelName;

        const created = await apiPost<ApiABTest>("/api/abtests", {
          name: data.name.trim(),
          controlFundId: fundId,
          treatmentFundId: fundId,
          variableType: "strategy_compare",
          variableConfig: {
            variantA: {
              name: data.variantAName.trim(),
              strategyConfig: variantAStrategyConfig,
            },
            variantB: {
              name: data.variantBName.trim(),
              strategyConfig: variantBStrategyConfig,
            },
            strategySummary: data.strategySummary.trim(),
            durationDays: data.durationDays,
            // Mode is repeated at the top level so analytics can
            // bucket experiments without parsing variantB.strategyConfig.
            mode: data.mode,
          },
          startDate: toDateString(startDate),
          endDate: toDateString(endDate),
        });
        upsertTest(created);
        setModalOpen(false);
      } catch (err) {
        setActionError(formatApiError(err, copy.actionsFailed.create));
      } finally {
        setSaving(false);
      }
    },
    [copy.actionsFailed.create, fundId, upsertTest],
  );

  const handleStatusAction = useCallback(
    async (kind: "start" | "stop" | "analyze") => {
      if (!selected) {
        return;
      }
      setActing(true);
      setActionError(null);
      try {
        const updated = await apiPost<ApiABTest>(`/api/abtests/${selected.id}/${kind}`);
        upsertTest(updated);
      } catch (err) {
        setActionError(formatApiError(err, copy.actionsFailed[kind]));
      } finally {
        setActing(false);
      }
    },
    [copy.actionsFailed, selected, upsertTest],
  );

  useEffect(() => {
    setPromotionResult(null);
    setPromotionMode("merge");
    setRollbackResult(null);
  }, [selected?.id]);

  const handlePromoteLearning = useCallback(
    async (dryRun: boolean) => {
      if (!selected) {
        return;
      }
      setPromoting(true);
      setActionError(null);
      try {
        const result = await apiPost<ABTestLearningPromotionResult>(`/api/abtests/${selected.id}/promote-learning`, {
          mode: promotionMode,
          dryRun,
          requireAnalyzed: true,
        });
        setPromotionResult(result);
        if (!dryRun) {
          const history = await apiGet<ABTestLearningPromotionRecord[]>(`/api/abtests/${selected.id}/learning-promotions`);
          setPromotionHistory(history ?? []);
        }
      } catch (err) {
        setActionError(formatApiError(err, copy.actionsFailed.promote));
      } finally {
        setPromoting(false);
      }
    },
    [copy.actionsFailed.promote, promotionMode, selected],
  );

  useEffect(() => {
    if (!selected?.id || selected.status !== "analyzed") {
      setPromotionHistory([]);
      return;
    }
    let cancelled = false;
    void apiGet<ABTestLearningPromotionRecord[]>(`/api/abtests/${selected.id}/learning-promotions`)
      .then((history) => {
        if (!cancelled) setPromotionHistory(history ?? []);
      })
      .catch(() => {
        if (!cancelled) setPromotionHistory([]);
      });
    return () => {
      cancelled = true;
    };
  }, [selected?.id, selected?.status]);

  const handleRollbackPromotion = useCallback(
    async (promotionId: string) => {
      if (!selected) return;
      setRollingBack(promotionId);
      setActionError(null);
      try {
        const result = await apiPost<ABTestLearningRollbackResult>(`/api/abtests/${selected.id}/learning-promotions/${promotionId}/rollback`);
        setRollbackResult(result);
        const history = await apiGet<ABTestLearningPromotionRecord[]>(`/api/abtests/${selected.id}/learning-promotions`);
        setPromotionHistory(history ?? []);
      } catch (err) {
        setActionError(formatApiError(err, copy.actionsFailed.rollback));
      } finally {
        setRollingBack(null);
      }
    },
    [copy.actionsFailed.rollback, selected],
  );

  if (loading) {
    return <div className="rounded-xl border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>;
  }

  if (error) {
    return (
      <div className="rounded-xl border border-red-200 bg-red-50 p-6 text-sm text-red-700">
        <p>{error}</p>
        <button onClick={() => void loadTests()} className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700">
          {language === "en-US" ? "Retry" : "重试"}
        </button>
      </div>
    );
  }

  return (
    <div className="space-y-6">
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="flex items-center gap-3">
          <div className="rounded-xl border border-gray-200 bg-gray-50 px-4 py-3 text-center text-sm">
            <p className="text-xs text-gray-500">{copy.totalTests}</p>
            <p className="mt-1 text-lg font-semibold text-gray-900">{formatNumberForLanguage(tests.length, language)}</p>
          </div>
          <button
            onClick={() => {
              setActionError(null);
              setModalOpen(true);
            }}
            className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700"
          >
            {copy.create}
          </button>
        </div>
      </div>

      {actionError ? <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-3 text-sm text-red-700">{actionError}</div> : null}

      {tests.length === 0 ? (
        <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
          <p className="font-medium text-gray-700">{copy.emptyTitle}</p>
          <p className="mt-2">{copy.emptyDescription}</p>
          <button onClick={() => setModalOpen(true)} className="mt-4 rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700">
            {copy.createNow}
          </button>
        </div>
      ) : (
        <div className="grid grid-cols-1 gap-6 xl:grid-cols-[320px_minmax(0,1fr)]">
          <aside className="rounded-2xl border border-gray-200 bg-white shadow-sm">
            <div className="border-b border-gray-200 px-5 py-4">
              <h2 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.testList}</h2>
            </div>
            <div className="divide-y divide-gray-100">
              {tests.map((test) => {
                const meta = statusMeta(test.status);
                const config = parseVariableConfig(test.variableConfig);
                const isSelected = selected?.id === test.id;
                return (
                  <button
                    key={test.id}
                    onClick={() => setSelectedId(test.id)}
                    className={`w-full px-5 py-4 text-left transition ${isSelected ? "bg-indigo-50/60" : "hover:bg-gray-50"}`}
                  >
                    <div className="flex items-start justify-between gap-3">
                      <p className="font-medium text-gray-900">{test.name}</p>
                      <span className={`rounded-full border px-2.5 py-1 text-xs font-medium ${meta.badge}`}>{meta.label}</span>
                    </div>
                    <p className="mt-2 text-xs text-gray-500">{variableTypeLabel(test.variableType)}</p>
                    <p className="mt-1 break-all text-xs text-gray-500">
                      {config.oldValue || copy.notSet} → {config.newValue || copy.notSet}
                    </p>
                    <p className="mt-2 text-xs text-gray-400">
                      {copy.createdAt} {formatDateForLanguage(test.createdAt, language)}
                    </p>
                  </button>
                );
              })}
            </div>
          </aside>

          <section className="space-y-6">
            {!selected ? (
              <div className="rounded-xl border border-dashed border-gray-200 bg-white p-8 text-center text-sm text-gray-500">
                {copy.selectPrompt}
              </div>
            ) : (
              <>
                <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                  <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                    <div>
                      <div className="flex flex-wrap items-center gap-3">
                        <h2 className="text-2xl font-bold text-gray-900">{selected.name}</h2>
                        <span className={`rounded-full border px-3 py-1 text-sm font-medium ${statusMeta(selected.status).badge}`}>
                          {statusMeta(selected.status).label}
                        </span>
                      </div>
                      <div className="mt-3 flex flex-col gap-2 text-sm text-gray-500">
                        <p>{copy.testType}: {variableTypeLabel(selected.variableType)}</p>
                        <p>{copy.modelCompare}: {selectedConfig.oldValue || copy.notSet} → {selectedConfig.newValue || copy.notSet}</p>
                        {selectedConfig.strategySummary ? <p>{copy.strategyChange}: {selectedConfig.strategySummary}</p> : null}
                        <p>{copy.timeWindow}: {formatDateForLanguage(selected.startDate, language)} → {formatDateForLanguage(selected.endDate, language)}</p>
                        <p>
                          {copy.duration}: {selectedConfig.durationDays ? `${formatNumberForLanguage(selectedConfig.durationDays, language)}${language === "en-US" ? ` ${copy.days}` : copy.days}` : copy.notSet}
                        </p>
                      </div>
                    </div>
                    <div className="flex flex-col gap-2 sm:flex-row lg:flex-col xl:flex-row">
                      {selected.status === "draft" ? (
                        <button
                          onClick={() => void handleStatusAction("start")}
                          disabled={acting}
                          className="rounded-lg bg-emerald-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {copy.start}
                        </button>
                      ) : null}
                      {selected.status === "running" ? (
                        <button
                          onClick={() => void handleStatusAction("stop")}
                          disabled={acting}
                          className="rounded-lg bg-red-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {copy.stop}
                        </button>
                      ) : null}
                      {selected.status === "completed" ? (
                        <button
                          onClick={() => void handleStatusAction("analyze")}
                          disabled={acting}
                          className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {copy.analyze}
                        </button>
                      ) : null}
                    </div>
                  </div>

                  <div className="mt-5 grid grid-cols-1 gap-3 md:grid-cols-2">
                    <div className="rounded-xl bg-gray-50 p-4 text-sm text-gray-600">
                      <p className="text-xs text-gray-500">{copy.controlFund}</p>
                      <p className="mt-2 break-all font-mono text-xs text-gray-800">{selected.controlFundId}</p>
                    </div>
                    <div className="rounded-xl bg-gray-50 p-4 text-sm text-gray-600">
                      <p className="text-xs text-gray-500">{copy.treatmentFund}</p>
                      <p className="mt-2 break-all font-mono text-xs text-gray-800">{selected.treatmentFundId}</p>
                    </div>
                  </div>
                </div>

                {selected.results ? (
                  <>
                    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                      <div className="flex flex-wrap items-center justify-between gap-3">
                        <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.summaryTitle}</h3>
                        <div className="flex flex-wrap items-center gap-2">
                          {typeof latestExcessReturn === "number" ? (
                            <span className={`rounded-full px-3 py-1 text-sm font-medium ${latestExcessReturn >= 0 ? "bg-emerald-50 text-emerald-700" : "bg-red-50 text-red-700"}`}>
                              {copy.excessReturn}: {formatMetric("totalReturn", latestExcessReturn)}
                            </span>
                          ) : null}
                          <span className="rounded-full bg-indigo-50 px-3 py-1 text-sm font-medium text-indigo-700">
                            {copy.winnerPrefix}
                            {selected.results.winner === "treatment"
                              ? copy.winnerTreatment
                              : selected.results.winner === "control"
                                ? copy.winnerControl
                                : copy.winnerNone}
                          </span>
                        </div>
                      </div>

                      <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-3">
                        {resultKeys.map((key) => {
                          const control = selected.results?.variantA?.[key];
                          const treatment = selected.results?.variantB?.[key];
                          const winner =
                            typeof control === "number" && typeof treatment === "number"
                              ? treatment > control
                                ? "treatment"
                                : treatment < control
                                  ? "control"
                                  : "tie"
                              : null;
                          return (
                            <div key={key} className="rounded-xl border border-gray-200 bg-gray-50 p-4">
                              <p className="text-xs font-medium uppercase tracking-wide text-gray-500">{metricLabel(key)}</p>
                              <div className="mt-4 grid grid-cols-2 gap-4">
                                <div>
                                  <p className="text-[10px] font-semibold uppercase text-blue-600">{copy.control}</p>
                                  <p className="mt-1 text-lg font-bold text-gray-900">{formatMetric(key, control)}</p>
                                </div>
                                <div>
                                  <p className="text-[10px] font-semibold uppercase text-orange-600">{copy.treatment}</p>
                                  <p className={`mt-1 text-lg font-bold ${winner === "treatment" ? "text-emerald-600" : winner === "control" ? "text-red-600" : "text-gray-900"}`}>
                                    {formatMetric(key, treatment)}
                                  </p>
                                </div>
                              </div>
                            </div>
                          );
                        })}
                      </div>
                    </div>

                    {navSeries.length > 0 ? (
                      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                        <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.curveTitle}</h3>
                        <div className="mt-4 h-72">
                          <ResponsiveContainer width="100%" height="100%">
                            <LineChart data={navSeries} margin={{ top: 10, right: 24, left: 0, bottom: 5 }}>
                              <CartesianGrid strokeDasharray="3 3" stroke="#e5e7eb" />
                              <XAxis dataKey="date" tick={{ fontSize: 12 }} />
                              <YAxis tick={{ fontSize: 12 }} tickFormatter={(value) => `${formatNumberForLanguage(Number(value), language)}%`} />
                              <Tooltip
                                formatter={(value: unknown, name: unknown) => [
                                  typeof value === "number" ? formatMetric("totalReturn", value) : String(value),
                                  name === "variantAReturn" ? copy.control : name === "variantBReturn" ? copy.treatment : copy.excessReturn,
                                ]}
                              />
                              <Legend />
                              <Line type="monotone" dataKey="variantAReturn" name={copy.control} stroke="#2563eb" strokeWidth={2} dot={false} />
                              <Line type="monotone" dataKey="variantBReturn" name={copy.treatment} stroke="#f97316" strokeWidth={2} dot={false} />
                              <Line type="monotone" dataKey="excessReturn" name={copy.excessReturn} stroke="#10b981" strokeWidth={2} strokeDasharray="4 4" dot={false} />
                            </LineChart>
                          </ResponsiveContainer>
                        </div>
                      </div>
                    ) : null}

                    {confidence ? (
                      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                        <div className="flex flex-wrap items-center justify-between gap-3">
                          <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.confidenceTitle}</h3>
                          <span className={`rounded-full px-3 py-1 text-sm font-medium ${confidence.level === "high" ? "bg-emerald-50 text-emerald-700" : confidence.level === "medium" ? "bg-amber-50 text-amber-700" : "bg-red-50 text-red-700"}`}>
                            {confidenceLabel(confidence.level)} · {formatNumberForLanguage(confidence.score, language, { maximumFractionDigits: 0 })}/100
                          </span>
                        </div>
                        <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-3">
                          <div className="rounded-xl bg-gray-50 p-4">
                            <p className="text-xs text-gray-500">{copy.confidenceScore}</p>
                            <p className="mt-1 text-xl font-bold text-gray-900">{formatNumberForLanguage(confidence.score, language, { maximumFractionDigits: 0 })}</p>
                          </div>
                          <div className="rounded-xl bg-gray-50 p-4">
                            <p className="text-xs text-gray-500">{copy.sampleDays}</p>
                            <p className="mt-1 text-xl font-bold text-gray-900">{formatNumberForLanguage(confidence.sampleDays, language)}</p>
                          </div>
                          <div className="rounded-xl bg-gray-50 p-4">
                            <p className="text-xs text-gray-500">{copy.sampleTrades}</p>
                            <p className="mt-1 text-xl font-bold text-gray-900">{formatNumberForLanguage(confidence.tradeCount, language)}</p>
                          </div>
                        </div>
                        {confidence.recommendation ? <p className="mt-4 text-sm text-gray-700">{confidence.recommendation}</p> : null}
                        {confidence.warnings?.length ? (
                          <ul className="mt-3 list-disc space-y-1 pl-5 text-sm text-amber-700">
                            {confidence.warnings.map((warning) => <li key={warning}>{warning}</li>)}
                          </ul>
                        ) : null}
                      </div>
                    ) : null}

                    {scorecard ? (
                      <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                        <div className="flex flex-wrap items-center justify-between gap-3">
                          <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.scorecardTitle}</h3>
                          <span className="rounded-full bg-slate-100 px-3 py-1 text-sm font-medium text-slate-700">
                            {copy.recommendedVariant}: {scorecard.recommendedVariant === "treatment" ? copy.treatment : scorecard.recommendedVariant === "control" ? copy.control : copy.winnerNone}
                          </span>
                        </div>
                        <div className="mt-4 grid grid-cols-1 gap-4 md:grid-cols-3">
                          <div className="rounded-xl border border-blue-100 bg-blue-50 p-4">
                            <p className="text-xs font-medium uppercase text-blue-600">{copy.control}</p>
                            <p className="mt-1 text-2xl font-bold text-blue-900">{formatNumberForLanguage(scorecard.variantAScore, language, { maximumFractionDigits: 0 })}</p>
                          </div>
                          <div className="rounded-xl border border-orange-100 bg-orange-50 p-4">
                            <p className="text-xs font-medium uppercase text-orange-600">{copy.treatment}</p>
                            <p className="mt-1 text-2xl font-bold text-orange-900">{formatNumberForLanguage(scorecard.variantBScore, language, { maximumFractionDigits: 0 })}</p>
                          </div>
                          <div className="rounded-xl border border-gray-200 bg-gray-50 p-4">
                            <p className="text-xs font-medium uppercase text-gray-500">{copy.scoreGap}</p>
                            <p className="mt-1 text-2xl font-bold text-gray-900">{formatNumberForLanguage(scorecard.scoreGap, language, { maximumFractionDigits: 1 })}</p>
                          </div>
                        </div>
                        {scorecard.verdict ? <p className="mt-4 text-sm font-medium text-gray-800">{scorecard.verdict}</p> : null}
                        <div className="mt-5">
                          <p className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.scoreBreakdown}</p>
                          <div className="mt-3 grid grid-cols-1 gap-3 lg:grid-cols-2">
                            {scorecard.components.map((component) => (
                              <div key={component.key} className="rounded-xl border border-gray-200 bg-gray-50 p-4 text-sm">
                                <div className="flex items-center justify-between gap-3">
                                  <p className="font-semibold text-gray-800">{component.label}</p>
                                  <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${component.contribution >= 0 ? "bg-emerald-50 text-emerald-700" : "bg-red-50 text-red-700"}`}>
                                    {component.contribution >= 0 ? "+" : ""}{formatNumberForLanguage(component.contribution, language, { maximumFractionDigits: 1 })}
                                  </span>
                                </div>
                                <div className="mt-2 grid grid-cols-2 gap-3 text-xs text-gray-600">
                                  <span>{copy.control}: {formatNumberForLanguage(component.variantA, language, { maximumFractionDigits: 2 })}</span>
                                  <span>{copy.treatment}: {formatNumberForLanguage(component.variantB, language, { maximumFractionDigits: 2 })}</span>
                                </div>
                                {component.explanation ? <p className="mt-2 text-xs text-gray-500">{component.explanation}</p> : null}
                              </div>
                            ))}
                          </div>
                        </div>
                        {(scorecard.riskNotes?.length || scorecard.costNotes?.length) ? (
                          <div className="mt-5 grid grid-cols-1 gap-4 md:grid-cols-2">
                            <div className="rounded-xl border border-amber-100 bg-amber-50 p-4">
                              <p className="text-xs font-semibold uppercase tracking-wide text-amber-700">{copy.riskNotes}</p>
                              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-amber-800">
                                {(scorecard.riskNotes ?? []).map((note) => <li key={note}>{note}</li>)}
                              </ul>
                            </div>
                            <div className="rounded-xl border border-sky-100 bg-sky-50 p-4">
                              <p className="text-xs font-semibold uppercase tracking-wide text-sky-700">{copy.costNotes}</p>
                              <ul className="mt-2 list-disc space-y-1 pl-5 text-sm text-sky-800">
                                {(scorecard.costNotes ?? []).map((note) => <li key={note}>{note}</li>)}
                              </ul>
                            </div>
                          </div>
                        ) : null}
                      </div>
                    ) : null}

                    <div className="rounded-2xl border border-indigo-100 bg-indigo-50/40 p-6 shadow-sm">
                      <div className="flex flex-col gap-4 lg:flex-row lg:items-start lg:justify-between">
                        <div>
                          <h3 className="text-sm font-semibold uppercase tracking-wider text-indigo-700">{copy.promotionTitle}</h3>
                          <p className="mt-2 max-w-3xl text-sm text-gray-600">{copy.promotionDescription}</p>
                          <p className="mt-2 text-sm font-medium text-amber-700">{copy.promotionWarning}</p>
                        </div>
                        <div className="flex flex-col gap-2 sm:flex-row lg:flex-col xl:flex-row">
                          <button
                            onClick={() => void handlePromoteLearning(true)}
                            disabled={promoting || selected.status !== "analyzed"}
                            className="rounded-lg border border-indigo-200 bg-white px-4 py-2.5 text-sm font-medium text-indigo-700 hover:bg-indigo-50 disabled:cursor-not-allowed disabled:opacity-60"
                          >
                            {promoting ? copy.previewing : copy.dryRun}
                          </button>
                          <button
                            onClick={() => void handlePromoteLearning(false)}
                            disabled={promoting || selected.status !== "analyzed" || !promotionResult?.dryRun}
                            className="rounded-lg bg-indigo-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
                          >
                            {promoting ? copy.applying : copy.applyPromotion}
                          </button>
                        </div>
                      </div>

                      <div className="mt-5 grid grid-cols-1 gap-4 lg:grid-cols-[260px_minmax(0,1fr)]">
                        <div className="rounded-xl border border-indigo-100 bg-white p-4">
                          <p className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.promotionMode}</p>
                          <div className="mt-3 space-y-2 text-sm">
                            <label className="flex cursor-pointer items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 hover:bg-gray-50">
                              <input type="radio" checked={promotionMode === "merge"} onChange={() => { setPromotionMode("merge"); setPromotionResult(null); }} />
                              <span>{copy.mergeMode}</span>
                            </label>
                            <label className="flex cursor-pointer items-center gap-2 rounded-lg border border-gray-200 px-3 py-2 hover:bg-gray-50">
                              <input type="radio" checked={promotionMode === "overwrite"} onChange={() => { setPromotionMode("overwrite"); setPromotionResult(null); }} />
                              <span>{copy.overwriteMode}</span>
                            </label>
                          </div>
                        </div>

                        <div className="rounded-xl border border-indigo-100 bg-white p-4">
                          <div className="flex flex-wrap items-center justify-between gap-2">
                            <p className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.promotionResult}</p>
                            {promotionResult ? (
                              <span className={`rounded-full px-2.5 py-1 text-xs font-medium ${promotionResult.dryRun ? "bg-amber-50 text-amber-700" : "bg-emerald-50 text-emerald-700"}`}>
                                {promotionResult.dryRun ? copy.dryRun : copy.applyPromotion} · Variant {promotionResult.variantKey} · {promotionResult.mode}
                              </span>
                            ) : null}
                          </div>
                          {promotionResult ? (
                            <div className="mt-4 space-y-4">
                              {promotionResult.warnings?.length ? (
                                <ul className="list-disc space-y-1 pl-5 text-sm text-amber-700">
                                  {promotionResult.warnings.map((warning) => <li key={warning}>{warning}</li>)}
                                </ul>
                              ) : null}
                              <div>
                                <p className="text-sm font-semibold text-gray-800">{copy.affectedAgents}: {formatNumberForLanguage(promotionResult.updatedAgents.length, language)}</p>
                                <div className="mt-2 grid grid-cols-1 gap-3 xl:grid-cols-2">
                                  {promotionResult.updatedAgents.map((agent) => (
                                    <div key={agent.agentId} className="rounded-lg border border-gray-200 bg-gray-50 p-3 text-sm">
                                      <div className="flex flex-wrap items-center justify-between gap-2">
                                        <p className="font-medium text-gray-900">{agent.agentName || agent.agentId}</p>
                                        <span className="rounded-full bg-white px-2 py-0.5 text-xs text-gray-500">{agent.role || "agent"}</span>
                                      </div>
                                      <p className="mt-1 text-xs text-gray-500">events: {formatNumberForLanguage(agent.learningEventCount, language)} · lessons: {formatNumberForLanguage(agent.lessonCount, language)}</p>
                                      {agent.lessons?.length ? <p className="mt-2 text-xs text-gray-600">{copy.lessons}: {agent.lessons.join("；")}</p> : null}
                                      {agent.adjustments?.length ? <p className="mt-1 text-xs text-gray-600">{copy.adjustments}: {agent.adjustments.join("；")}</p> : null}
                                      {(agent.promotedReflectionIds?.length || agent.promotedSkillKeys?.length) ? (
                                        <p className="mt-2 text-xs font-medium text-indigo-700">
                                          {language === "zh-CN" ? "已晋升" : "Promoted"}: {language === "zh-CN" ? "反思" : "reflections"} ×{formatNumberForLanguage(agent.promotedReflectionIds?.length ?? 0, language)} · {language === "zh-CN" ? "候选技能" : "skill candidates"} ×{formatNumberForLanguage(agent.promotedSkillKeys?.length ?? 0, language)}
                                        </p>
                                      ) : null}
                                    </div>
                                  ))}
                                </div>
                              </div>
                              {promotionResult.skippedAgents?.length ? (
                                <div>
                                  <p className="text-sm font-semibold text-gray-800">{copy.skippedAgents}: {formatNumberForLanguage(promotionResult.skippedAgents.length, language)}</p>
                                  <div className="mt-2 flex flex-wrap gap-2">
                                    {promotionResult.skippedAgents.map((skip, index) => (
                                      <span key={`${skip.agentId ?? "skip"}-${index}`} className="rounded-full bg-gray-100 px-3 py-1 text-xs text-gray-600">
                                        {skip.agentId || "agent"}: {skip.reason}
                                      </span>
                                    ))}
                                  </div>
                                </div>
                              ) : null}
                            </div>
                          ) : (
                            <div className="mt-4 rounded-lg border border-dashed border-gray-200 bg-gray-50 p-4 text-sm text-gray-500">{copy.noPromotionResult}</div>
                          )}
                        </div>
                      </div>

                      <div className="mt-4 rounded-xl border border-indigo-100 bg-white p-4">
                        <div className="flex flex-wrap items-center justify-between gap-2">
                          <p className="text-xs font-semibold uppercase tracking-wide text-gray-500">{copy.promotionHistory}</p>
                          {rollbackResult?.rolledBack ? (
                            <span className="rounded-full bg-emerald-50 px-2.5 py-1 text-xs font-medium text-emerald-700">
                              {copy.rollbackDone}: {rollbackResult.agentName || rollbackResult.agentId}
                              {(rollbackResult.rolledBackReflectionIds?.length || rollbackResult.skillKeysReverted?.length) ? (
                                <span className="ml-1 text-emerald-600">
                                  · −{formatNumberForLanguage(rollbackResult.rolledBackReflectionIds?.length ?? 0, language)} {language === "zh-CN" ? "反思" : "reflections"} · −{formatNumberForLanguage(rollbackResult.skillKeysReverted?.length ?? 0, language)} {language === "zh-CN" ? "候选技能" : "skill candidates"}
                                </span>
                              ) : null}
                            </span>
                          ) : null}
                        </div>
                        {promotionHistory.length > 0 ? (
                          <div className="mt-3 overflow-x-auto">
                            <table className="min-w-full divide-y divide-gray-200 text-sm">
                              <thead className="bg-gray-50 text-xs uppercase tracking-wide text-gray-500">
                                <tr>
                                  <th className="px-3 py-2 text-left">Variant</th>
                                  <th className="px-3 py-2 text-left">Agent</th>
                                  <th className="px-3 py-2 text-left">{copy.promotionMode}</th>
                                  <th className="px-3 py-2 text-left">{copy.date}</th>
                                  <th className="px-3 py-2 text-right">{copy.rollback}</th>
                                </tr>
                              </thead>
                              <tbody className="divide-y divide-gray-100">
                                {promotionHistory.slice(0, 10).map((item) => (
                                  <tr key={item.id}>
                                    <td className="whitespace-nowrap px-3 py-3 font-medium text-indigo-700">{item.variantKey}{item.variantName ? ` · ${item.variantName}` : ""}</td>
                                    <td className="whitespace-nowrap px-3 py-3 text-gray-800">{item.agentName || item.agentId}</td>
                                    <td className="whitespace-nowrap px-3 py-3 text-gray-600">{item.mode}</td>
                                    <td className="whitespace-nowrap px-3 py-3 text-gray-600">{formatDateForLanguage(item.promotedAt, language)}</td>
                                    <td className="whitespace-nowrap px-3 py-3 text-right">
                                      <button
                                        onClick={() => void handleRollbackPromotion(item.id)}
                                        disabled={rollingBack === item.id}
                                        className="rounded-lg border border-red-200 px-3 py-1.5 text-xs font-medium text-red-700 hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60"
                                      >
                                        {rollingBack === item.id ? copy.rollingBack : copy.rollback}
                                      </button>
                                    </td>
                                  </tr>
                                ))}
                              </tbody>
                            </table>
                          </div>
                        ) : (
                          <div className="mt-3 rounded-lg border border-dashed border-gray-200 bg-gray-50 p-4 text-sm text-gray-500">{copy.noPromotionHistory}</div>
                        )}
                      </div>
                    </div>

                    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                      <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.decisionDiffTitle}</h3>
                      {decisionDiffs.length > 0 ? (
                        <div className="mt-4 overflow-x-auto">
                          <table className="min-w-full divide-y divide-gray-200 text-sm">
                            <thead className="bg-gray-50 text-xs uppercase tracking-wide text-gray-500">
                              <tr>
                                <th className="px-3 py-2 text-left">{copy.date}</th>
                                <th className="px-3 py-2 text-left">{copy.symbol}</th>
                                <th className="px-3 py-2 text-left">{copy.actionA}</th>
                                <th className="px-3 py-2 text-left">{copy.actionB}</th>
                                <th className="px-3 py-2 text-right">{copy.impact}</th>
                                <th className="px-3 py-2 text-left">{copy.explanation}</th>
                              </tr>
                            </thead>
                            <tbody className="divide-y divide-gray-100">
                              {decisionDiffs.slice(0, 20).map((diff, index) => (
                                <tr key={`${diff.date}-${diff.symbol}-${index}`}>
                                  <td className="whitespace-nowrap px-3 py-3 text-gray-600">{formatDateForLanguage(diff.date, language)}</td>
                                  <td className="whitespace-nowrap px-3 py-3 font-medium text-gray-900">{diff.symbol}</td>
                                  <td className="px-3 py-3 text-blue-700">{diff.variantAAction || "—"}</td>
                                  <td className="px-3 py-3 text-orange-700">{diff.variantBAction || "—"}</td>
                                  <td className={`whitespace-nowrap px-3 py-3 text-right font-medium ${diff.returnImpact >= 0 ? "text-emerald-600" : "text-red-600"}`}>{formatMetric("totalReturn", diff.returnImpact)}</td>
                                  <td className="min-w-64 px-3 py-3 text-gray-600">{diff.explanation || "—"}</td>
                                </tr>
                              ))}
                            </tbody>
                          </table>
                        </div>
                      ) : (
                        <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{copy.noDecisionDiffs}</div>
                      )}
                    </div>

                    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                      <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.tradeDetailTitle}</h3>
                      {shadowTrades.length > 0 ? (
                        <div className="mt-4 overflow-x-auto">
                          <table className="min-w-full divide-y divide-gray-200 text-sm">
                            <thead className="bg-gray-50 text-xs uppercase tracking-wide text-gray-500">
                              <tr>
                                <th className="px-3 py-2 text-left">{copy.date}</th>
                                <th className="px-3 py-2 text-left">Variant</th>
                                <th className="px-3 py-2 text-left">{copy.symbol}</th>
                                <th className="px-3 py-2 text-left">{copy.side}</th>
                                <th className="px-3 py-2 text-right">{copy.quantity}</th>
                                <th className="px-3 py-2 text-right">{copy.price}</th>
                                <th className="px-3 py-2 text-right">{copy.notional}</th>
                                <th className="px-3 py-2 text-right">{copy.pnl}</th>
                                <th className="px-3 py-2 text-left">{copy.reasoning}</th>
                              </tr>
                            </thead>
                            <tbody className="divide-y divide-gray-100">
                              {shadowTrades.slice(0, 30).map((trade, index) => (
                                <tr key={`${trade.variantKey}-${trade.date}-${trade.symbol}-${index}`}>
                                  <td className="whitespace-nowrap px-3 py-3 text-gray-600">{formatDateForLanguage(trade.date, language)}</td>
                                  <td className={`whitespace-nowrap px-3 py-3 font-semibold ${trade.variantKey === "A" ? "text-blue-700" : "text-orange-700"}`}>{trade.variantKey}</td>
                                  <td className="whitespace-nowrap px-3 py-3 font-medium text-gray-900">{trade.symbol}</td>
                                  <td className="whitespace-nowrap px-3 py-3 text-gray-700">{trade.side}</td>
                                  <td className="whitespace-nowrap px-3 py-3 text-right text-gray-700">{formatNumberForLanguage(trade.quantity, language, { maximumFractionDigits: 4 })}</td>
                                  <td className="whitespace-nowrap px-3 py-3 text-right text-gray-700">{formatNumberForLanguage(trade.price, language, { maximumFractionDigits: 4 })}</td>
                                  <td className="whitespace-nowrap px-3 py-3 text-right text-gray-700">{formatMoneyForDisplay(trade.notional, "USD", displayCurrency, language)}</td>
                                  <td className={`whitespace-nowrap px-3 py-3 text-right font-medium ${trade.realizedPnL >= 0 ? "text-emerald-600" : "text-red-600"}`}>{formatMoneyForDisplay(trade.realizedPnL, "USD", displayCurrency, language)}</td>
                                  <td className="min-w-72 px-3 py-3 text-gray-600">{trade.reasoning || "—"}</td>
                                </tr>
                              ))}
                            </tbody>
                          </table>
                        </div>
                      ) : (
                        <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-5 text-sm text-gray-500">{copy.noTrades}</div>
                      )}
                    </div>
                  </>
                ) : (
                  <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
                    <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{copy.statusTitle}</h3>
                    <div className="mt-4 rounded-xl border border-dashed border-gray-200 bg-gray-50 p-6 text-sm text-gray-500">
                      {selected.status === "draft"
                        ? copy.draftState
                        : selected.status === "running"
                          ? copy.runningState
                          : selected.status === "completed"
                            ? copy.completedState
                            : copy.emptyState}
                    </div>
                  </div>
                )}

                {/* Card D — Per-symbol attribution + shadow agent panels.
                    Render OUTSIDE the `selected.results` block so users
                    on `running` / `completed` tests can still see what
                    the shadow run already produced (learning events
                    are written even before `analyze` finalises results).
                    Each panel is collapsible + lazy; the analyzed=false
                    branch shows a friendly hint instead of a 500 spam.
                    The feature is fund-agnostic — it works for every
                    strategy_compare test regardless of which company /
                    fund created it. */}
                {selected.status !== "draft" ? (
                  <>
                    <ABOperationalAttributionTable
                      testId={selected.id}
                      analyzed={selected.status !== "draft"}
                      language={language}
                    />

                    {selected.variableType === "strategy_compare" ? (
                      <ABShadowAgentPanel
                        testId={selected.id}
                        analyzed={selected.status !== "draft"}
                        language={language}
                      />
                    ) : null}
                  </>
                ) : null}
              </>
            )}
          </section>
        </div>
      )}

      <CreateTestModal
        open={modalOpen}
        fundId={fundId ?? ""}
        saving={saving}
        error={actionError}
        language={language}
        onClose={() => setModalOpen(false)}
        onCreate={handleCreate}
      />
    </div>
  );
};

export default ABTestCompare;
