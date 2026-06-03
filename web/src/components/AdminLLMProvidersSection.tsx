// AdminLLMProvidersSection — S13 CRUD UI for platform_llm_providers.
//
// Replaces the env-only LLM provider configuration with a managed
// list (provider, label, base_url, api_key, tier, status, pricing)
// that hot-reloads into the router on save. API keys never
// round-trip: the row shows fingerprint + masked preview only; an
// empty input on edit means "keep current".

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteAdminLLMProvider,
  formatApiError,
  listAdminLLMProviders,
  setAdminLLMProviderDefault,
  testAdminLLMProvider,
  upsertAdminLLMProvider,
  type AdminLLMProvider,
  type TestAdminLLMProviderResponse,
  type UpsertAdminLLMProviderRequest,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  language: Language;
}

type FormState = Partial<UpsertAdminLLMProviderRequest> & {
  api_key?: string;
};

const PROVIDERS = ["openai", "claude", "deepseek", "qwen", "gemini", "custom"] as const;
const TIERS = ["", "critical", "standard", "simple"] as const;

const defaultBaseURL: Record<string, string> = {
  openai: "https://api.openai.com/v1",
  claude: "https://api.anthropic.com/v1",
  deepseek: "https://api.deepseek.com/v1",
  qwen: "https://dashscope.aliyuncs.com/compatible-mode/v1",
  gemini: "https://generativelanguage.googleapis.com/v1beta",
  custom: "",
};

const defaultModel: Record<string, string> = {
  openai: "gpt-4o",
  claude: "claude-3-5-sonnet-20241022",
  deepseek: "deepseek-chat",
  qwen: "qwen-max",
  gemini: "gemini-1.5-pro",
  custom: "",
};

interface Copy {
  title: string;
  subtitle: string;
  refresh: string;
  loading: string;
  empty: string;
  errorPrefix: string;
  newButton: string;
  saveButton: string;
  cancelButton: string;
  deleteButton: string;
  defaultButton: string;
  testButton: string;
  testing: string;
  saving: string;
  routerKeysLabel: string;
  reloadGenerationLabel: string;
  colProvider: string;
  colLabel: string;
  colTier: string;
  colModel: string;
  colBaseURL: string;
  colKeyFingerprint: string;
  colStatus: string;
  colDefault: string;
  colHealth: string;
  colActions: string;
  formProvider: string;
  formLabel: string;
  formTier: string;
  formModel: string;
  formBaseURL: string;
  formAPIKey: string;
  formAPIKeyHintEdit: string;
  formAPIKeyHintCreate: string;
  formMaxTokens: string;
  formTemperature: string;
  formInputPrice: string;
  formOutputPrice: string;
  formStatus: string;
  formStatusActive: string;
  formStatusDisabled: string;
  formStatusDraft: string;
  formCreateTitle: string;
  formEditTitle: string;
  deleteConfirm: (label: string) => string;
  setDefaultConfirm: (label: string) => string;
  testOK: (latency: number, echoed: string) => string;
  testFail: (status: number | undefined, message: string) => string;
}

const messages: Record<Language, Copy> = {
  "zh-CN": {
    title: "LLM 模型供应商",
    subtitle: "平台级 LLM Provider 配置（API key 加密入库，保存后立即生效，无需重启）。",
    refresh: "刷新",
    loading: "加载中…",
    empty: "尚未配置任何 provider。首次启动会从 env 自动 seed。",
    errorPrefix: "加载失败",
    newButton: "新建 provider",
    saveButton: "保存",
    cancelButton: "取消",
    deleteButton: "删除",
    defaultButton: "设为默认",
    testButton: "测试连接",
    testing: "测试中…",
    saving: "保存中…",
    routerKeysLabel: "Router 当前生效 key",
    reloadGenerationLabel: "Reload 代次",
    colProvider: "Provider",
    colLabel: "标签",
    colTier: "Tier",
    colModel: "默认模型",
    colBaseURL: "Base URL",
    colKeyFingerprint: "Key 指纹",
    colStatus: "状态",
    colDefault: "默认",
    colHealth: "最近测连",
    colActions: "操作",
    formProvider: "Provider",
    formLabel: "标签 (唯一)",
    formTier: "Tier (可空)",
    formModel: "默认模型",
    formBaseURL: "Base URL",
    formAPIKey: "API Key",
    formAPIKeyHintEdit: "留空表示保留原 key",
    formAPIKeyHintCreate: "新建时必填",
    formMaxTokens: "Max tokens",
    formTemperature: "Temperature",
    formInputPrice: "输入价 /1M",
    formOutputPrice: "输出价 /1M",
    formStatus: "状态",
    formStatusActive: "active",
    formStatusDisabled: "disabled",
    formStatusDraft: "draft",
    formCreateTitle: "新建 LLM Provider",
    formEditTitle: "编辑 LLM Provider",
    deleteConfirm: (label) => `确定删除 "${label}"？该操作不可撤销，但下次重启 env 仍会重新 seed。`,
    setDefaultConfirm: (label) => `把 "${label}" 设为平台默认？所有未指定 tier override 的请求都将走它。`,
    testOK: (latency, echoed) => `连接成功 · ${latency}ms${echoed ? ` · ${echoed}` : ""}`,
    testFail: (status, msg) => `连接失败${status ? ` · HTTP ${status}` : ""} · ${msg}`,
  },
  "en-US": {
    title: "LLM Model Providers",
    subtitle: "Platform-level LLM provider config (encrypted at rest; hot-reload, no restart needed).",
    refresh: "Refresh",
    loading: "Loading…",
    empty: "No providers configured yet. First boot auto-seeds from env.",
    errorPrefix: "Load failed",
    newButton: "New provider",
    saveButton: "Save",
    cancelButton: "Cancel",
    deleteButton: "Delete",
    defaultButton: "Set default",
    testButton: "Test connection",
    testing: "Testing…",
    saving: "Saving…",
    routerKeysLabel: "Router active keys",
    reloadGenerationLabel: "Reload generation",
    colProvider: "Provider",
    colLabel: "Label",
    colTier: "Tier",
    colModel: "Default model",
    colBaseURL: "Base URL",
    colKeyFingerprint: "Key fingerprint",
    colStatus: "Status",
    colDefault: "Default",
    colHealth: "Last test",
    colActions: "Actions",
    formProvider: "Provider",
    formLabel: "Label (unique)",
    formTier: "Tier (optional)",
    formModel: "Default model",
    formBaseURL: "Base URL",
    formAPIKey: "API Key",
    formAPIKeyHintEdit: "leave empty to keep current",
    formAPIKeyHintCreate: "required on create",
    formMaxTokens: "Max tokens",
    formTemperature: "Temperature",
    formInputPrice: "Input price /1M",
    formOutputPrice: "Output price /1M",
    formStatus: "Status",
    formStatusActive: "active",
    formStatusDisabled: "disabled",
    formStatusDraft: "draft",
    formCreateTitle: "Create LLM Provider",
    formEditTitle: "Edit LLM Provider",
    deleteConfirm: (label) => `Delete "${label}"? This is irreversible, but the env seed will repopulate on next restart.`,
    setDefaultConfirm: (label) => `Promote "${label}" to platform default? Every request without a tier override will route here.`,
    testOK: (latency, echoed) => `OK · ${latency}ms${echoed ? ` · ${echoed}` : ""}`,
    testFail: (status, msg) => `Failed${status ? ` · HTTP ${status}` : ""} · ${msg}`,
  },
};

export function AdminLLMProvidersSection({ language }: Props) {
  const t = messages[language];
  const [rows, setRows] = useState<AdminLLMProvider[]>([]);
  const [routerKeys, setRouterKeys] = useState<Record<string, boolean> | null>(null);
  const [reloadGen, setReloadGen] = useState<number>(0);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [form, setForm] = useState<FormState | null>(null);
  const [saving, setSaving] = useState(false);
  const [testingId, setTestingId] = useState<string | null>(null);
  const [testResults, setTestResults] = useState<Record<string, TestAdminLLMProviderResponse>>({});

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminLLMProviders();
      setRows(resp.providers ?? []);
      setRouterKeys(resp.router_active_keys);
      setReloadGen(resp.reload_generation ?? 0);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [t.errorPrefix]);

  useEffect(() => {
    reload();
  }, [reload]);

  const openCreate = () => {
    setForm({
      provider: "openai",
      label: "",
      model_tier: "",
      model_name: defaultModel.openai,
      base_url: defaultBaseURL.openai,
      api_key: "",
      max_tokens: 4096,
      temperature: 0.7,
      status: "active",
    });
  };

  const openEdit = (row: AdminLLMProvider) => {
    setForm({
      id: row.id,
      provider: row.provider,
      label: row.label,
      model_tier: row.model_tier ?? "",
      model_name: row.model_name,
      base_url: row.base_url,
      api_key: "",
      max_tokens: row.max_tokens,
      temperature: row.temperature,
      input_price_per_1m: row.input_price_per_1m,
      output_price_per_1m: row.output_price_per_1m,
      cost_per_1m: row.cost_per_1m,
      status: row.status,
    });
  };

  const onSubmitForm = async () => {
    if (!form) return;
    if (!form.provider || !form.label || !form.model_name || !form.base_url) {
      setError("provider, label, model_name, base_url 都必填");
      return;
    }
    if (!form.id && !form.api_key) {
      setError("新建时必须输入 API Key");
      return;
    }
    setSaving(true);
    setError(null);
    try {
      await upsertAdminLLMProvider({
        id: form.id,
        provider: form.provider,
        label: form.label,
        model_tier: form.model_tier || undefined,
        model_name: form.model_name,
        base_url: form.base_url,
        api_key: form.api_key || undefined,
        max_tokens: form.max_tokens,
        temperature: form.temperature,
        input_price_per_1m: form.input_price_per_1m ?? undefined,
        output_price_per_1m: form.output_price_per_1m ?? undefined,
        cost_per_1m: form.cost_per_1m ?? undefined,
        status: form.status,
      });
      setForm(null);
      await reload();
    } catch (err) {
      setError(formatApiError(err, "保存失败"));
    } finally {
      setSaving(false);
    }
  };

  const onDelete = async (row: AdminLLMProvider) => {
    if (!confirm(t.deleteConfirm(row.label))) return;
    try {
      await deleteAdminLLMProvider(row.id);
      await reload();
    } catch (err) {
      setError(formatApiError(err, "删除失败"));
    }
  };

  const onSetDefault = async (row: AdminLLMProvider) => {
    if (!confirm(t.setDefaultConfirm(row.label))) return;
    try {
      await setAdminLLMProviderDefault(row.id);
      await reload();
    } catch (err) {
      setError(formatApiError(err, "设默认失败"));
    }
  };

  const onTest = async (row: AdminLLMProvider) => {
    setTestingId(row.id);
    try {
      const resp = await testAdminLLMProvider({
        id: row.id,
        provider: row.provider,
        model_name: row.model_name,
        base_url: row.base_url,
      });
      setTestResults((prev) => ({ ...prev, [row.id]: resp }));
    } catch (err) {
      setError(formatApiError(err, "测连失败"));
    } finally {
      setTestingId(null);
    }
  };

  const onTestForm = async () => {
    if (!form) return;
    setTestingId("form");
    try {
      const resp = await testAdminLLMProvider({
        id: form.id,
        provider: form.provider ?? "",
        model_name: form.model_name ?? "",
        base_url: form.base_url ?? "",
        api_key: form.api_key || undefined,
      });
      setTestResults((prev) => ({ ...prev, form: resp }));
    } catch (err) {
      setError(formatApiError(err, "测连失败"));
    } finally {
      setTestingId(null);
    }
  };

  const routerKeyChips = useMemo(() => {
    if (!routerKeys) return null;
    const entries = Object.entries(routerKeys).filter(([, v]) => v);
    if (entries.length === 0) return <span className="text-rose-300">none</span>;
    return entries.map(([p]) => (
      <span key={p} className="rounded border border-emerald-500/30 bg-emerald-500/10 px-1.5 py-0.5 text-[10px] uppercase text-emerald-200">
        {p}
      </span>
    ));
  }, [routerKeys]);

  return (
    <section className="rounded-2xl border border-zinc-700 bg-zinc-900/40 p-6">
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div>
          <h2 className="text-lg font-semibold text-zinc-100">{t.title}</h2>
          <p className="mt-1 text-sm text-zinc-400">{t.subtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <button
            type="button"
            onClick={reload}
            disabled={loading}
            className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 hover:bg-zinc-700 disabled:opacity-50"
          >
            {t.refresh}
          </button>
          <button
            type="button"
            onClick={openCreate}
            className="rounded-md border border-emerald-500/40 bg-emerald-500/15 px-3 py-1 text-sm text-emerald-200 hover:bg-emerald-500/25"
          >
            {t.newButton}
          </button>
        </div>
      </div>

      {error && (
        <div className="mt-4 rounded-md border border-rose-700 bg-rose-900/30 p-3 text-sm text-rose-200">
          {error}
        </div>
      )}

      <div className="mt-4 flex flex-wrap items-center gap-3 text-xs text-zinc-400">
        <span>{t.routerKeysLabel}:</span>
        <span className="flex flex-wrap items-center gap-1">{routerKeyChips}</span>
        <span className="ml-4">{t.reloadGenerationLabel}: <code className="text-zinc-200">{reloadGen}</code></span>
      </div>

      <div className="mt-6 overflow-x-auto rounded-lg border border-zinc-700 bg-zinc-900/30">
        {loading ? (
          <p className="p-4 text-sm text-zinc-400">{t.loading}</p>
        ) : rows.length === 0 ? (
          <p className="p-4 text-sm text-zinc-400">{t.empty}</p>
        ) : (
          <table className="w-full text-xs text-zinc-200">
            <thead>
              <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                <th className="px-2 py-1 text-left">{t.colProvider}</th>
                <th className="px-2 py-1 text-left">{t.colLabel}</th>
                <th className="px-2 py-1 text-left">{t.colTier}</th>
                <th className="px-2 py-1 text-left">{t.colModel}</th>
                <th className="px-2 py-1 text-left">{t.colBaseURL}</th>
                <th className="px-2 py-1 text-left">{t.colKeyFingerprint}</th>
                <th className="px-2 py-1 text-left">{t.colStatus}</th>
                <th className="px-2 py-1 text-left">{t.colDefault}</th>
                <th className="px-2 py-1 text-left">{t.colHealth}</th>
                <th className="px-2 py-1 text-left">{t.colActions}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => {
                const test = testResults[row.id];
                return (
                  <tr key={row.id} className="border-b border-zinc-800/40 hover:bg-zinc-800/20">
                    <td className="px-2 py-1 font-medium uppercase">{row.provider}</td>
                    <td className="px-2 py-1">{row.label}</td>
                    <td className="px-2 py-1 text-zinc-400">{row.model_tier || "—"}</td>
                    <td className="px-2 py-1 font-mono text-[11px]">{row.model_name}</td>
                    <td className="px-2 py-1 truncate text-zinc-400" style={{ maxWidth: 200 }} title={row.base_url}>{row.base_url}</td>
                    <td className="px-2 py-1 font-mono text-[11px]">{row.api_key_masked_preview}</td>
                    <td className="px-2 py-1">
                      <span className={`rounded px-1.5 py-0.5 text-[10px] uppercase ${
                        row.status === "active" ? "bg-emerald-500/15 text-emerald-200"
                          : row.status === "disabled" ? "bg-zinc-700/50 text-zinc-300"
                          : "bg-amber-500/15 text-amber-200"
                      }`}>
                        {row.status}
                      </span>
                    </td>
                    <td className="px-2 py-1">
                      {row.is_platform_default ? (
                        <span className="rounded border border-blue-500/40 bg-blue-500/15 px-1.5 py-0.5 text-[10px] uppercase text-blue-200">
                          default
                        </span>
                      ) : "—"}
                    </td>
                    <td className="px-2 py-1 text-zinc-400">
                      {test ? (
                        <span className={test.ok ? "text-emerald-300" : "text-rose-300"}>
                          {test.ok ? t.testOK(test.latency_ms, test.echoed_model ?? "") : t.testFail(test.http_status, test.message ?? "")}
                        </span>
                      ) : row.last_health_check_result?.ok ? (
                        <span className="text-emerald-300">
                          {t.testOK(row.last_health_check_result.latency_ms ?? 0, row.last_health_check_result.echoed_model ?? "")}
                        </span>
                      ) : row.last_health_check_result?.message ? (
                        <span className="text-rose-300">
                          {t.testFail(row.last_health_check_result.http_status, row.last_health_check_result.message)}
                        </span>
                      ) : "—"}
                    </td>
                    <td className="px-2 py-1">
                      <div className="flex flex-wrap gap-1">
                        <button
                          type="button"
                          onClick={() => openEdit(row)}
                          className="rounded border border-zinc-600 px-1.5 py-0.5 text-[10px] hover:bg-zinc-700"
                        >
                          编辑
                        </button>
                        <button
                          type="button"
                          onClick={() => onTest(row)}
                          disabled={testingId === row.id}
                          className="rounded border border-blue-500/40 bg-blue-500/10 px-1.5 py-0.5 text-[10px] text-blue-200 hover:bg-blue-500/20 disabled:opacity-50"
                        >
                          {testingId === row.id ? t.testing : t.testButton}
                        </button>
                        {!row.is_platform_default && (
                          <button
                            type="button"
                            onClick={() => onSetDefault(row)}
                            className="rounded border border-emerald-500/40 bg-emerald-500/10 px-1.5 py-0.5 text-[10px] text-emerald-200 hover:bg-emerald-500/20"
                          >
                            {t.defaultButton}
                          </button>
                        )}
                        <button
                          type="button"
                          onClick={() => onDelete(row)}
                          className="rounded border border-rose-500/40 bg-rose-500/10 px-1.5 py-0.5 text-[10px] text-rose-200 hover:bg-rose-500/20"
                        >
                          {t.deleteButton}
                        </button>
                      </div>
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
      </div>

      {form && (
        <div className="mt-6 rounded-lg border border-zinc-600 bg-zinc-900/60 p-4">
          <h3 className="text-sm font-semibold text-zinc-100">
            {form.id ? t.formEditTitle : t.formCreateTitle}
          </h3>
          <div className="mt-3 grid grid-cols-1 gap-3 md:grid-cols-2">
            <label className="text-xs text-zinc-300">
              {t.formProvider}
              <select
                value={form.provider ?? ""}
                onChange={(e) => {
                  const next = e.target.value;
                  setForm((s) => ({
                    ...(s ?? {}),
                    provider: next,
                    base_url: s?.base_url || defaultBaseURL[next] || "",
                    model_name: s?.model_name || defaultModel[next] || "",
                  }));
                }}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              >
                {PROVIDERS.map((p) => (
                  <option key={p} value={p}>{p}</option>
                ))}
              </select>
            </label>
            <label className="text-xs text-zinc-300">
              {t.formLabel}
              <input
                type="text"
                value={form.label ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), label: e.target.value }))}
                placeholder="e.g. openai-prod-main"
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              />
            </label>
            <label className="text-xs text-zinc-300">
              {t.formTier}
              <select
                value={form.model_tier ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), model_tier: e.target.value }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              >
                {TIERS.map((tier) => (
                  <option key={tier} value={tier}>{tier || "(any)"}</option>
                ))}
              </select>
            </label>
            <label className="text-xs text-zinc-300">
              {t.formModel}
              <input
                type="text"
                value={form.model_name ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), model_name: e.target.value }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100 font-mono text-[11px]"
              />
            </label>
            <label className="text-xs text-zinc-300 md:col-span-2">
              {t.formBaseURL}
              <input
                type="text"
                value={form.base_url ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), base_url: e.target.value }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100 font-mono text-[11px]"
              />
            </label>
            <label className="text-xs text-zinc-300 md:col-span-2">
              {t.formAPIKey}
              <input
                type="password"
                value={form.api_key ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), api_key: e.target.value }))}
                placeholder={form.id ? t.formAPIKeyHintEdit : t.formAPIKeyHintCreate}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100 font-mono text-[11px]"
              />
            </label>
            <label className="text-xs text-zinc-300">
              {t.formMaxTokens}
              <input
                type="number"
                value={form.max_tokens ?? 4096}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), max_tokens: Number(e.target.value) || 0 }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              />
            </label>
            <label className="text-xs text-zinc-300">
              {t.formTemperature}
              <input
                type="number"
                step="0.01"
                value={form.temperature ?? 0.7}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), temperature: Number(e.target.value) }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              />
            </label>
            <label className="text-xs text-zinc-300">
              {t.formInputPrice}
              <input
                type="number"
                step="0.0001"
                value={form.input_price_per_1m ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), input_price_per_1m: e.target.value === "" ? undefined : Number(e.target.value) }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              />
            </label>
            <label className="text-xs text-zinc-300">
              {t.formOutputPrice}
              <input
                type="number"
                step="0.0001"
                value={form.output_price_per_1m ?? ""}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), output_price_per_1m: e.target.value === "" ? undefined : Number(e.target.value) }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              />
            </label>
            <label className="text-xs text-zinc-300 md:col-span-2">
              {t.formStatus}
              <select
                value={form.status ?? "active"}
                onChange={(e) => setForm((s) => ({ ...(s ?? {}), status: e.target.value as "active" | "disabled" | "draft" }))}
                className="mt-1 block w-full rounded-md border border-zinc-700 bg-zinc-800 px-2 py-1 text-zinc-100"
              >
                <option value="active">{t.formStatusActive}</option>
                <option value="disabled">{t.formStatusDisabled}</option>
                <option value="draft">{t.formStatusDraft}</option>
              </select>
            </label>
          </div>
          {testResults.form && (
            <div className={`mt-3 rounded-md border p-2 text-xs ${
              testResults.form.ok
                ? "border-emerald-500/40 bg-emerald-500/10 text-emerald-200"
                : "border-rose-500/40 bg-rose-500/10 text-rose-200"
            }`}>
              {testResults.form.ok
                ? t.testOK(testResults.form.latency_ms, testResults.form.echoed_model ?? "")
                : t.testFail(testResults.form.http_status, testResults.form.message ?? "")}
            </div>
          )}
          <div className="mt-4 flex flex-wrap gap-2">
            <button
              type="button"
              onClick={onSubmitForm}
              disabled={saving}
              className="rounded-md border border-emerald-500/40 bg-emerald-500/15 px-3 py-1 text-sm text-emerald-200 hover:bg-emerald-500/25 disabled:opacity-50"
            >
              {saving ? t.saving : t.saveButton}
            </button>
            <button
              type="button"
              onClick={onTestForm}
              disabled={testingId === "form"}
              className="rounded-md border border-blue-500/40 bg-blue-500/15 px-3 py-1 text-sm text-blue-200 hover:bg-blue-500/25 disabled:opacity-50"
            >
              {testingId === "form" ? t.testing : t.testButton}
            </button>
            <button
              type="button"
              onClick={() => setForm(null)}
              className="rounded-md border border-zinc-600 px-3 py-1 text-sm text-zinc-300 hover:bg-zinc-800"
            >
              {t.cancelButton}
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
