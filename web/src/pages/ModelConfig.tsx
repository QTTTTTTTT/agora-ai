import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiDelete, apiGet, apiPost, formatApiError } from "../lib/api";
import {
  formatDateTimeForLanguage,
  formatMoneyForDisplay,
  formatNumberForLanguage,
  useAppPreferences,
} from "../lib/preferences";

interface PlatformModel {
  provider: string;
  model_name: string;
  display_name: string;
  tier: string;
  input_price_per_1k: number;
  output_price_per_1k: number;
  available: boolean;
}

interface UserModelConfig {
  id: string;
  user_id: string;
  config_type: "tier_override" | "custom_endpoint";
  tier?: string;
  provider: string;
  model_name: string;
  base_url?: string;
  is_active: boolean;
  created_at?: string;
  updated_at?: string;
}

interface ModelsResponse {
  platform_models: PlatformModel[];
  custom_models: PlatformModel[];
}

interface ModelConfigsResponse {
  configs: UserModelConfig[];
}

interface ConnectionTestResult {
  success: boolean;
  latency_ms: number;
  message: string;
  model_id?: string;
}

const providerOptions = [
  { value: "openai", defaultLabel: "OpenAI" },
  { value: "claude", defaultLabel: "Anthropic" },
  { value: "deepseek", defaultLabel: "DeepSeek" },
  { value: "qwen", defaultLabel: "Qwen" },
  { value: "custom", defaultLabel: "Custom" },
] as const;

const tierIds = ["critical", "standard", "simple"] as const;

type TierId = (typeof tierIds)[number];

const ModelConfig: React.FC = () => {
  const { language, displayCurrency } = useAppPreferences();
  const [platformModels, setPlatformModels] = useState<PlatformModel[]>([]);
  const [configs, setConfigs] = useState<UserModelConfig[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [actionError, setActionError] = useState<string | null>(null);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);
  const [savingTier, setSavingTier] = useState<string | null>(null);
  const [deletingConfigId, setDeletingConfigId] = useState<string | null>(null);
  const [testing, setTesting] = useState(false);
  const [testResult, setTestResult] = useState<string | null>(null);
  const [testSucceeded, setTestSucceeded] = useState<boolean | null>(null);
  const [form, setForm] = useState({
    provider: "openai",
    modelName: "",
    apiUrl: "",
    apiKey: "",
  });

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "Model config",
            subtitle: "Assign models for each task tier and manage custom endpoints with connection testing and persistent settings.",
            monthlyEstimate: "Estimated monthly model cost",
            monthSuffix: "/month",
            loading: "Loading model configuration...",
            loadFailed: "Failed to load model configuration",
            retry: "Retry",
            tierLabels: {
              critical: "Critical decisions (Tier 1)",
              standard: "Daily tasks (Tier 2)",
              simple: "Simple tasks (Tier 3)",
            } as Record<TierId, string>,
            tierDescriptions: {
              critical: "Used for final investment decisions, risk reviews, and strategy generation.",
              standard: "Used for daily analysis, reporting, and team collaboration.",
              simple: "Used for formatting, classification, and low-complexity summarization.",
            } as Record<TierId, string>,
            tierHints: {
              critical: "Choose the strongest reasoning model and prioritize accuracy.",
              standard: "Balance cost and quality for frequent requests.",
              simple: "Prioritize low cost for bulk processing.",
            } as Record<TierId, string>,
            providerLabels: {
              openai: "OpenAI",
              claude: "Anthropic",
              deepseek: "DeepSeek",
              qwen: "Qwen",
              custom: "Custom",
            } as Record<string, string>,
            priceInput: "Input",
            priceOutput: "Output",
            unavailable: "Unavailable",
            available: "Available",
            selected: "Current selection",
            noModels: "No models are available yet. They will appear here once the model catalog is connected.",
            saveModelFailed: "Failed to save model config",
            saveModelSuccess: "Updated model for",
            endpointTitle: "Custom endpoints",
            endpointSubtitle: "Test connectivity for your own model service, then save it as a reusable long-term configuration.",
            provider: "Provider",
            modelName: "Model name",
            modelNamePlaceholder: "For example: gpt-4o-2024-05-13",
            apiUrl: "API URL",
            apiKey: "API key",
            testConnection: "Test connection",
            testing: "Testing...",
            saveConfig: "Save config",
            saveEndpointFailed: "Failed to save custom endpoint",
            saveEndpointSuccess: "Custom endpoint saved.",
            deleteEndpointFailed: "Failed to delete config",
            deleteEndpointSuccess: "Configuration deleted.",
            testFailed: "Failed to test connection",
            testSucceeded: "Connection succeeded, latency",
            testFailedPrefix: "Connection failed:",
            savedConfigs: "Saved custom configurations",
            updatedAt: "Updated at",
            defaultAddress: "Use default URL",
            deleting: "Deleting...",
            delete: "Delete",
            noSavedConfigs: "No custom endpoints have been saved yet. After testing one successfully, you can keep it here for reuse.",
          }
        : {
            title: "模型配置",
            subtitle: "为不同任务层级分配模型，并管理自定义端点的连通性与长期配置。",
            monthlyEstimate: "预计月度模型费用",
            monthSuffix: "/月",
            loading: "正在加载模型配置...",
            loadFailed: "加载模型配置失败",
            retry: "重试",
            tierLabels: {
              critical: "关键决策 (Tier 1)",
              standard: "日常任务 (Tier 2)",
              simple: "简单任务 (Tier 3)",
            } as Record<TierId, string>,
            tierDescriptions: {
              critical: "用于最终投资决策、风险评估、策略生成等关键步骤。",
              standard: "用于数据解读、报告生成、成员协作对话等日常工作。",
              simple: "用于数据格式化、简单分类、摘要提取等低复杂度步骤。",
            } as Record<TierId, string>,
            tierHints: {
              critical: "建议选择推理能力最强的模型，准确性优先。",
              standard: "平衡性能与成本，适合高频调用。",
              simple: "成本优先，适合批量处理。",
            } as Record<TierId, string>,
            providerLabels: {
              openai: "OpenAI",
              claude: "Anthropic",
              deepseek: "DeepSeek",
              qwen: "Qwen",
              custom: "自定义",
            } as Record<string, string>,
            priceInput: "输入",
            priceOutput: "输出",
            unavailable: "不可用",
            available: "可用",
            selected: "当前已启用",
            noModels: "当前还没有可选模型，后续接通模型目录后会自动展示在这里。",
            saveModelFailed: "保存模型配置失败",
            saveModelSuccess: "已更新",
            endpointTitle: "自定义端点配置",
            endpointSubtitle: "你可以接入自有模型服务进行连通性测试，再决定是否保存为长期配置。未保存前不会影响现有路由策略。",
            provider: "供应商",
            modelName: "模型名称",
            modelNamePlaceholder: "例如：gpt-4o-2024-05-13",
            apiUrl: "API 地址",
            apiKey: "访问密钥",
            testConnection: "测试连接",
            testing: "测试中...",
            saveConfig: "保存配置",
            saveEndpointFailed: "保存自定义端点失败",
            saveEndpointSuccess: "自定义端点已保存。",
            deleteEndpointFailed: "删除配置失败",
            deleteEndpointSuccess: "配置已删除。",
            testFailed: "测试连接失败",
            testSucceeded: "连接成功，延迟",
            testFailedPrefix: "连接失败：",
            savedConfigs: "已保存的自定义配置",
            updatedAt: "更新于",
            defaultAddress: "使用默认地址",
            deleting: "删除中...",
            delete: "删除",
            noSavedConfigs: "当前还没有保存任何自定义端点，完成测试后可在这里长期保留可复用配置。",
          },
    [language],
  );

  const formatProvider = useCallback(
    (provider: string) => copy.providerLabels[provider] ?? provider,
    [copy.providerLabels],
  );

  const formatPrice = useCallback(
    (model: PlatformModel) => {
      if (!model.input_price_per_1k && !model.output_price_per_1k) {
        return "-";
      }
      return `${copy.priceInput} ${formatMoneyForDisplay(model.input_price_per_1k, "CNY", displayCurrency, language)}/1K · ${copy.priceOutput} ${formatMoneyForDisplay(model.output_price_per_1k, "CNY", displayCurrency, language)}/1K`;
    },
    [copy.priceInput, copy.priceOutput, displayCurrency, language],
  );

  const formatTimestamp = useCallback(
    (value?: string) => formatDateTimeForLanguage(value, language),
    [language],
  );

  const loadData = useCallback(async () => {
    setLoading(true);
    setError(null);

    try {
      const [modelsRes, configsRes] = await Promise.all([
        apiGet<ModelsResponse>("/api/models"),
        apiGet<ModelConfigsResponse>("/api/models/config"),
      ]);
      setPlatformModels(modelsRes.platform_models ?? []);
      setConfigs(configsRes.configs ?? []);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed]);

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const configsByTier = useMemo(() => {
    const map: Record<string, UserModelConfig | undefined> = {};
    for (const config of configs) {
      if (config.config_type === "tier_override" && config.tier && config.is_active) {
        map[config.tier] = config;
      }
    }
    return map;
  }, [configs]);

  const customEndpoints = useMemo(
    () => configs.filter((config) => config.config_type === "custom_endpoint"),
    [configs],
  );

  const tierModels = useMemo(
    () =>
      tierIds.map((tierId) => ({
        id: tierId,
        name: copy.tierLabels[tierId],
        description: copy.tierDescriptions[tierId],
        hint: copy.tierHints[tierId],
        models: platformModels.filter((model) => model.tier === tierId),
      })),
    [copy.tierDescriptions, copy.tierHints, copy.tierLabels, platformModels],
  );

  const monthlyEstimate = useMemo(() => {
    return tierIds.reduce((sum, tierId) => {
      const config = configsByTier[tierId];
      const model = config
        ? platformModels.find((item) => item.model_name === config.model_name && item.provider === config.provider)
        : undefined;
      return sum + (model?.output_price_per_1k ?? 0) * 100;
    }, 0);
  }, [configsByTier, platformModels]);

  const handleSelectModel = useCallback(
    async (tierId: string, model: PlatformModel) => {
      setSavingTier(tierId);
      setActionError(null);
      setSuccessMessage(null);
      try {
        await apiPost("/api/models/config", {
          config_type: "tier_override",
          tier: tierId,
          provider: model.provider,
          model_name: model.model_name,
        });
        setSuccessMessage(
          language === "en-US"
            ? `${copy.saveModelSuccess} ${copy.tierLabels[tierId as TierId] ?? tierId}.`
            : `${copy.saveModelSuccess}${copy.tierLabels[tierId as TierId] ?? tierId}模型。`,
        );
        await loadData();
      } catch (err) {
        setActionError(formatApiError(err, copy.saveModelFailed));
      } finally {
        setSavingTier(null);
      }
    },
    [copy.saveModelFailed, copy.saveModelSuccess, copy.tierLabels, language, loadData],
  );

  const handleTestConnection = useCallback(async () => {
    setTesting(true);
    setTestResult(null);
    setTestSucceeded(null);
    setActionError(null);
    try {
      const payload: Record<string, string> = {
        provider: form.provider,
        model_name: form.modelName.trim(),
      };
      if (form.apiUrl.trim()) {
        payload.base_url = form.apiUrl.trim();
      }
      if (form.apiKey.trim()) {
        payload.api_key = form.apiKey.trim();
      }
      const result = await apiPost<ConnectionTestResult>("/api/models/test", payload);
      setTestSucceeded(result.success);
      setTestResult(
        result.success
          ? `${copy.testSucceeded} ${formatNumberForLanguage(result.latency_ms, language)}ms.`
          : `${copy.testFailedPrefix}${result.message}`,
      );
    } catch (err) {
      setTestSucceeded(false);
      setActionError(formatApiError(err, copy.testFailed));
    } finally {
      setTesting(false);
    }
  }, [copy.testFailed, copy.testFailedPrefix, copy.testSucceeded, form, language]);

  const handleSaveEndpoint = useCallback(async () => {
    setActionError(null);
    setSuccessMessage(null);
    setTestResult(null);
    setTestSucceeded(null);

    try {
      await apiPost("/api/models/config", {
        config_type: "custom_endpoint",
        provider: form.provider,
        model_name: form.modelName.trim(),
        base_url: form.apiUrl.trim() || undefined,
        api_key: form.apiKey.trim() || undefined,
      });
      setSuccessMessage(copy.saveEndpointSuccess);
      setForm({ provider: "openai", modelName: "", apiUrl: "", apiKey: "" });
      await loadData();
    } catch (err) {
      setActionError(formatApiError(err, copy.saveEndpointFailed));
    }
  }, [copy.saveEndpointFailed, copy.saveEndpointSuccess, form, loadData]);

  const handleDeleteEndpoint = useCallback(
    async (id: string) => {
      setDeletingConfigId(id);
      setActionError(null);
      setSuccessMessage(null);
      try {
        await apiDelete(`/api/models/config/${id}`);
        setSuccessMessage(copy.deleteEndpointSuccess);
        await loadData();
      } catch (err) {
        setActionError(formatApiError(err, copy.deleteEndpointFailed));
      } finally {
        setDeletingConfigId(null);
      }
    },
    [copy.deleteEndpointFailed, copy.deleteEndpointSuccess, loadData],
  );

  if (loading) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-gray-200 bg-white p-6 text-sm text-gray-500">{copy.loading}</div>
      </div>
    );
  }

  if (error) {
    return (
      <div className="space-y-4">
        <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
        <div className="rounded-lg border border-red-200 bg-red-50 p-6 text-sm text-red-700">
          <p>{error}</p>
          <button
            onClick={() => void loadData()}
            className="mt-4 rounded-lg bg-red-600 px-4 py-2 text-sm font-medium text-white hover:bg-red-700"
          >
            {copy.retry}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="space-y-8">
      <div className="flex flex-col gap-4 rounded-2xl border border-gray-200 bg-white p-6 shadow-sm lg:flex-row lg:items-start lg:justify-between">
        <div>
          <h1 className="text-2xl font-bold text-gray-900">{copy.title}</h1>
          <p className="mt-2 text-sm text-gray-500">{copy.subtitle}</p>
        </div>
        <div className="rounded-lg border border-emerald-200 bg-emerald-50 px-4 py-2">
          <span className="text-sm text-emerald-700">{copy.monthlyEstimate}: </span>
          <span className="text-lg font-bold text-emerald-800">
            {formatMoneyForDisplay(monthlyEstimate, "CNY", displayCurrency, language)}
          </span>
          <span className="text-sm text-emerald-600">{copy.monthSuffix}</span>
        </div>
      </div>

      {actionError ? <div className="rounded-lg border border-red-200 bg-red-50 p-4 text-sm text-red-700">{actionError}</div> : null}
      {successMessage ? <div className="rounded-lg border border-green-200 bg-green-50 p-4 text-sm text-green-700">{successMessage}</div> : null}

      {tierModels.map((tier) => (
        <div key={tier.id} className="rounded-xl border border-gray-200 bg-white p-6">
          <h2 className="mb-1 text-lg font-bold text-gray-900">{tier.name}</h2>
          <p className="mb-1 text-sm text-gray-600">{tier.description}</p>
          <p className="mb-4 text-xs text-amber-600">{tier.hint}</p>

          {tier.models.length === 0 ? (
            <div className="rounded-lg border border-dashed border-gray-200 bg-gray-50 p-4 text-sm text-gray-500">
              {copy.noModels}
            </div>
          ) : (
            <div className="space-y-3">
              {tier.models.map((model) => {
                const selectedConfig = configsByTier[tier.id];
                const selected = selectedConfig?.model_name === model.model_name && selectedConfig?.provider === model.provider;

                return (
                  <label
                    key={`${tier.id}-${model.provider}-${model.model_name}`}
                    className={`flex cursor-pointer items-start gap-3 rounded-lg border-2 p-4 transition-colors ${
                      selected ? "border-indigo-500 bg-indigo-50" : "border-gray-100 hover:border-gray-300"
                    }`}
                  >
                    <input
                      type="radio"
                      name={`tier-${tier.id}`}
                      checked={selected}
                      onChange={() => void handleSelectModel(tier.id, model)}
                      disabled={savingTier === tier.id}
                      className="mt-1 h-4 w-4 text-indigo-600"
                    />
                    <div className="flex-1">
                      <div className="flex items-center gap-2">
                        <span className="font-semibold text-gray-900">{model.display_name}</span>
                        <span className="rounded bg-gray-100 px-2 py-0.5 text-xs text-gray-500">{formatProvider(model.provider)}</span>
                        <span className="ml-auto text-sm font-medium text-indigo-600">{formatPrice(model)}</span>
                      </div>
                      <p className="mt-0.5 text-sm text-gray-500">
                        {model.available ? copy.available : copy.unavailable}
                        {selectedConfig && selected ? ` · ${copy.selected}` : ""}
                      </p>
                    </div>
                  </label>
                );
              })}
            </div>
          )}
        </div>
      ))}

      <div className="rounded-xl border border-gray-200 bg-white p-6">
        <h2 className="mb-2 text-lg font-bold text-gray-900">{copy.endpointTitle}</h2>
        <p className="mb-4 text-sm text-gray-500">{copy.endpointSubtitle}</p>

        <div className="mb-4 grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label className="mb-1 block text-sm font-medium text-gray-700">{copy.provider}</label>
            <select
              value={form.provider}
              onChange={(e) => setForm((prev) => ({ ...prev, provider: e.target.value }))}
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            >
              {providerOptions.map((option) => (
                <option key={option.value} value={option.value}>
                  {formatProvider(option.value)}
                </option>
              ))}
            </select>
          </div>
          <div>
            <label className="mb-1 block text-sm font-medium text-gray-700">{copy.modelName}</label>
            <input
              type="text"
              value={form.modelName}
              onChange={(e) => setForm((prev) => ({ ...prev, modelName: e.target.value }))}
              placeholder={copy.modelNamePlaceholder}
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            />
          </div>
          <div>
            <label className="mb-1 block text-sm font-medium text-gray-700">{copy.apiUrl}</label>
            <input
              type="text"
              value={form.apiUrl}
              onChange={(e) => setForm((prev) => ({ ...prev, apiUrl: e.target.value }))}
              placeholder="https://api.example.com/v1"
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            />
          </div>
          <div>
            <label className="mb-1 block text-sm font-medium text-gray-700">{copy.apiKey}</label>
            <input
              type="password"
              value={form.apiKey}
              onChange={(e) => setForm((prev) => ({ ...prev, apiKey: e.target.value }))}
              placeholder="sk-..."
              className="w-full rounded-lg border border-gray-300 px-3 py-2 text-sm focus:border-indigo-500 focus:ring-1 focus:ring-indigo-500"
            />
          </div>
        </div>

        <div className="mb-4 flex items-center gap-3">
          <button
            onClick={() => void handleTestConnection()}
            disabled={testing || !form.provider || !form.modelName.trim()}
            className="rounded-lg border border-indigo-300 bg-indigo-50 px-4 py-2 text-sm font-medium text-indigo-700 transition-colors hover:bg-indigo-100 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {testing ? copy.testing : copy.testConnection}
          </button>
          <button
            onClick={() => void handleSaveEndpoint()}
            disabled={!form.provider || !form.modelName.trim()}
            className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white transition-colors hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {copy.saveConfig}
          </button>
          {testResult ? (
            <span className={`text-sm font-medium ${testSucceeded ? "text-emerald-600" : "text-red-600"}`}>{testResult}</span>
          ) : null}
        </div>

        {customEndpoints.length > 0 ? (
          <div>
            <h3 className="mb-2 text-sm font-semibold text-gray-700">{copy.savedConfigs}</h3>
            <div className="space-y-2">
              {customEndpoints.map((endpoint) => (
                <div
                  key={endpoint.id}
                  className="flex items-center justify-between rounded-lg border border-gray-100 bg-gray-50 px-4 py-3"
                >
                  <div>
                    <span className="font-medium text-gray-800">{endpoint.model_name}</span>
                    <span className="ml-2 text-xs text-gray-400">{formatProvider(endpoint.provider)}</span>
                    <span className="ml-2 text-xs text-gray-400">{endpoint.base_url ?? copy.defaultAddress}</span>
                    <span className="ml-2 text-xs text-gray-300">
                      {copy.updatedAt} {formatTimestamp(endpoint.updated_at ?? endpoint.created_at)}
                    </span>
                  </div>
                  <button
                    onClick={() => void handleDeleteEndpoint(endpoint.id)}
                    disabled={deletingConfigId === endpoint.id}
                    className="rounded px-2 py-1 text-xs text-red-600 transition-colors hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60"
                  >
                    {deletingConfigId === endpoint.id ? copy.deleting : copy.delete}
                  </button>
                </div>
              ))}
            </div>
          </div>
        ) : (
          <div className="rounded-lg border border-dashed border-gray-200 bg-gray-50 p-4 text-sm text-gray-500">
            {copy.noSavedConfigs}
          </div>
        )}
      </div>
    </div>
  );
};

export default ModelConfig;
