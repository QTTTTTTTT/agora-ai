// FundLLMOverridesSection — S14.B per-fund LLM provider override
// editor surfaced on the FundSettings page.
//
// Visible to fund owners (server enforces the auth via
// authorizeFundAccess). Lets the operator pin "this fund's pm_agent
// uses claude" or "all critical-tier calls for this fund use
// openai-prod" without leaving the fund settings UI.
//
// The "effective_*" columns rendered alongside each row come from
// the server resolving (provider, label) against platform_llm_providers
// so the operator sees what the router would actually pick.

import { useCallback, useEffect, useMemo, useState } from "react";

import {
  deleteFundLLMOverride,
  formatApiError,
  listAdminLLMProviders,
  listFundLLMOverrides,
  upsertFundLLMOverride,
  type AdminLLMProvider,
  type FundLLMOverride,
  type UpsertFundLLMOverrideRequest,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

interface Props {
  fundId: string;
  language: Language;
}

interface Copy {
  title: string;
  subtitle: string;
  refresh: string;
  newButton: string;
  noOverrides: string;
  loading: string;
  errorPrefix: string;
  scope: string;
  scopeAgent: string;
  scopeRole: string;
  scopeTier: string;
  scopeAll: string;
  effective: string;
  enabled: string;
  disabled: string;
  edit: string;
  delete: string;
  confirmDelete: string;
  formTitle: string;
  formAgentId: string;
  formRole: string;
  formTier: string;
  formProvider: string;
  formLabel: string;
  formModelName: string;
  formEnabled: string;
  formNote: string;
  save: string;
  cancel: string;
  placeholderAgentID: string;
  placeholderAny: string;
  validationProvider: string;
  noProviders: string;
}

const COPY: Record<Language, Copy> = {
  "zh-CN": {
    title: "LLM 提供商覆盖",
    subtitle:
      "为本基金（或某个 agent / role / tier）指定使用哪个平台 LLM provider，优先级在 A/B 实验之下、用户偏好之上。",
    refresh: "刷新",
    newButton: "新建覆盖",
    noOverrides: "暂无覆盖（路由按平台默认 + 用户偏好）",
    loading: "加载中…",
    errorPrefix: "操作失败",
    scope: "作用范围",
    scopeAgent: "Agent",
    scopeRole: "Role",
    scopeTier: "Tier",
    scopeAll: "全部",
    effective: "解析为",
    enabled: "启用",
    disabled: "停用",
    edit: "编辑",
    delete: "删除",
    confirmDelete: "确认删除该覆盖？",
    formTitle: "覆盖配置",
    formAgentId: "Agent ID (留空 = 所有 agent)",
    formRole: "Role (e.g. pm, risk, trader; 留空 = 所有)",
    formTier: "Tier (留空 = 所有)",
    formProvider: "Provider *",
    formLabel: "Label (留空 = 该 provider 的当前平台默认)",
    formModelName: "Model name (留空 = provider 的 model_name)",
    formEnabled: "启用",
    formNote: "备注",
    save: "保存",
    cancel: "取消",
    placeholderAgentID: "uuid 或留空",
    placeholderAny: "任意",
    validationProvider: "Provider 必填",
    noProviders: "平台尚未配置任何 LLM provider — 请先在 Admin 页添加",
  },
  "en-US": {
    title: "LLM Provider Overrides",
    subtitle:
      "Pin which platform LLM provider this fund (or a specific agent / role / tier) uses. Higher priority than user preference, lower than A/B experiments.",
    refresh: "Refresh",
    newButton: "New override",
    noOverrides: "No overrides yet (router uses platform default + user preference)",
    loading: "Loading…",
    errorPrefix: "Failed",
    scope: "Scope",
    scopeAgent: "Agent",
    scopeRole: "Role",
    scopeTier: "Tier",
    scopeAll: "All",
    effective: "Resolves to",
    enabled: "Enabled",
    disabled: "Disabled",
    edit: "Edit",
    delete: "Delete",
    confirmDelete: "Delete this override?",
    formTitle: "Override config",
    formAgentId: "Agent ID (empty = all agents)",
    formRole: "Role (e.g. pm, risk, trader; empty = all)",
    formTier: "Tier (empty = all)",
    formProvider: "Provider *",
    formLabel: "Label (empty = current platform default for this provider)",
    formModelName: "Model name (empty = provider's model_name)",
    formEnabled: "Enabled",
    formNote: "Note",
    save: "Save",
    cancel: "Cancel",
    placeholderAgentID: "uuid or empty",
    placeholderAny: "any",
    validationProvider: "Provider is required",
    noProviders: "No LLM providers configured yet — add them on the Admin page first",
  },
};

const TIERS = ["", "critical", "standard", "simple"] as const;

type FormState = Partial<UpsertFundLLMOverrideRequest>;

export default function FundLLMOverridesSection({ fundId, language }: Props) {
  const t = COPY[language];

  const [rows, setRows] = useState<FundLLMOverride[]>([]);
  const [providers, setProviders] = useState<AdminLLMProvider[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [editingForm, setEditingForm] = useState<FormState | null>(null);

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [overrides, provs] = await Promise.all([
        listFundLLMOverrides(fundId),
        listAdminLLMProviders({ status: "active" }).catch(() => ({
          providers: [],
          router_active_keys: {},
          reload_generation: 0,
        })),
      ]);
      setRows(overrides.overrides);
      setProviders(provs.providers);
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [fundId, t.errorPrefix]);

  useEffect(() => {
    if (fundId) reload();
  }, [fundId, reload]);

  // De-duplicated provider names (the providers list can have
  // multiple labels per provider; the dropdown shows providers only,
  // and a separate label dropdown narrows further).
  const providerNames = useMemo(
    () => Array.from(new Set(providers.map((p) => p.provider))).sort(),
    [providers],
  );

  const labelOptionsFor = useCallback(
    (provider: string) =>
      providers
        .filter((p) => p.provider === provider)
        .map((p) => p.label)
        .sort(),
    [providers],
  );

  const openCreate = () => {
    setEditingForm({
      provider: providerNames[0] ?? "",
      enabled: true,
    });
  };

  const openEdit = (row: FundLLMOverride) => {
    setEditingForm({
      id: row.id,
      agent_id: row.agent_id ?? "",
      role: row.role ?? "",
      model_tier: row.model_tier ?? "",
      provider: row.provider,
      label: row.label ?? "",
      model_name: row.model_name ?? "",
      enabled: row.enabled,
      note: row.note ?? "",
    });
  };

  const submit = async () => {
    if (!editingForm) return;
    const provider = (editingForm.provider ?? "").trim();
    if (!provider) {
      setError(t.validationProvider);
      return;
    }
    setLoading(true);
    setError(null);
    try {
      await upsertFundLLMOverride(fundId, {
        id: editingForm.id,
        agent_id: editingForm.agent_id?.trim() || null,
        role: editingForm.role?.trim() || undefined,
        model_tier: editingForm.model_tier?.trim() || undefined,
        provider,
        label: editingForm.label?.trim() || undefined,
        model_name: editingForm.model_name?.trim() || undefined,
        enabled: editingForm.enabled ?? true,
        note: editingForm.note?.trim() || undefined,
      });
      setEditingForm(null);
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  };

  const remove = async (row: FundLLMOverride) => {
    if (!confirm(t.confirmDelete)) return;
    setLoading(true);
    setError(null);
    try {
      await deleteFundLLMOverride(fundId, row.id);
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  };

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
            disabled={providerNames.length === 0}
            className="rounded-md border border-emerald-500/40 bg-emerald-500/15 px-3 py-1 text-sm text-emerald-200 hover:bg-emerald-500/25 disabled:opacity-50"
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

      {providerNames.length === 0 && (
        <div className="mt-4 rounded-md border border-amber-700/40 bg-amber-900/20 p-3 text-xs text-amber-200">
          {t.noProviders}
        </div>
      )}

      <div className="mt-6 overflow-x-auto rounded-lg border border-zinc-700 bg-zinc-900/30">
        {loading && rows.length === 0 ? (
          <p className="p-4 text-sm text-zinc-400">{t.loading}</p>
        ) : rows.length === 0 ? (
          <p className="p-4 text-sm text-zinc-400">{t.noOverrides}</p>
        ) : (
          <table className="w-full text-xs text-zinc-200">
            <thead>
              <tr className="border-b border-zinc-700 text-[10px] uppercase text-zinc-400">
                <th className="px-2 py-1 text-left">{t.scope}</th>
                <th className="px-2 py-1 text-left">{t.scopeAgent}</th>
                <th className="px-2 py-1 text-left">{t.scopeRole}</th>
                <th className="px-2 py-1 text-left">{t.scopeTier}</th>
                <th className="px-2 py-1 text-left">Provider / Label / Model</th>
                <th className="px-2 py-1 text-left">{t.effective}</th>
                <th className="px-2 py-1 text-left">{t.enabled}</th>
                <th className="px-2 py-1 text-left"></th>
              </tr>
            </thead>
            <tbody>
              {rows.map((row) => (
                <tr key={row.id} className="border-b border-zinc-800/40 hover:bg-zinc-800/20">
                  <td className="px-2 py-1 font-mono text-[10px] uppercase tabular-nums">
                    {row.specificity}
                  </td>
                  <td className="px-2 py-1 truncate max-w-[180px]">
                    {row.agent_id ?? <span className="text-zinc-500">{t.scopeAll}</span>}
                  </td>
                  <td className="px-2 py-1">{row.role || <span className="text-zinc-500">{t.scopeAll}</span>}</td>
                  <td className="px-2 py-1">{row.model_tier || <span className="text-zinc-500">{t.scopeAll}</span>}</td>
                  <td className="px-2 py-1">
                    <span className="font-medium uppercase">{row.provider}</span>
                    {row.label ? <span className="text-zinc-400"> · {row.label}</span> : null}
                    {row.model_name ? <span className="text-zinc-400"> · {row.model_name}</span> : null}
                  </td>
                  <td className="px-2 py-1 text-zinc-400">
                    {row.effective_provider}
                    {row.effective_label ? ` / ${row.effective_label}` : ""}
                    {row.effective_model_name ? ` / ${row.effective_model_name}` : ""}
                  </td>
                  <td className="px-2 py-1">
                    {row.enabled ? (
                      <span className="rounded border border-emerald-500/30 bg-emerald-500/10 px-1.5 py-0.5 text-[10px] uppercase text-emerald-200">
                        {t.enabled}
                      </span>
                    ) : (
                      <span className="rounded border border-zinc-500/30 bg-zinc-500/10 px-1.5 py-0.5 text-[10px] uppercase text-zinc-300">
                        {t.disabled}
                      </span>
                    )}
                  </td>
                  <td className="px-2 py-1">
                    <div className="flex gap-1">
                      <button
                        type="button"
                        onClick={() => openEdit(row)}
                        className="rounded border border-zinc-600 bg-zinc-800 px-2 py-0.5 text-[10px] uppercase text-zinc-100 hover:bg-zinc-700"
                      >
                        {t.edit}
                      </button>
                      <button
                        type="button"
                        onClick={() => remove(row)}
                        className="rounded border border-rose-600/40 bg-rose-900/30 px-2 py-0.5 text-[10px] uppercase text-rose-200 hover:bg-rose-900/50"
                      >
                        {t.delete}
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {editingForm && (
        <div className="mt-6 rounded-lg border border-emerald-700/30 bg-zinc-900/40 p-4">
          <h3 className="mb-3 text-sm font-semibold text-emerald-200">{t.formTitle}</h3>
          <div className="grid grid-cols-1 gap-3 md:grid-cols-2">
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formAgentId}
              <input
                type="text"
                value={editingForm.agent_id ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, agent_id: e.target.value })}
                placeholder={t.placeholderAgentID}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              />
            </label>
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formRole}
              <input
                type="text"
                value={editingForm.role ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, role: e.target.value })}
                placeholder={t.placeholderAny}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              />
            </label>
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formTier}
              <select
                value={editingForm.model_tier ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, model_tier: e.target.value })}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              >
                {TIERS.map((tier) => (
                  <option key={tier} value={tier}>
                    {tier === "" ? t.placeholderAny : tier}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formProvider}
              <select
                value={editingForm.provider ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, provider: e.target.value, label: "" })}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              >
                {providerNames.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formLabel}
              <select
                value={editingForm.label ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, label: e.target.value })}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              >
                <option value="">{t.placeholderAny}</option>
                {labelOptionsFor(editingForm.provider ?? "").map((lbl) => (
                  <option key={lbl} value={lbl}>
                    {lbl}
                  </option>
                ))}
              </select>
            </label>
            <label className="flex flex-col text-xs text-zinc-300">
              {t.formModelName}
              <input
                type="text"
                value={editingForm.model_name ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, model_name: e.target.value })}
                placeholder={t.placeholderAny}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              />
            </label>
            <label className="flex items-center gap-2 text-xs text-zinc-300">
              <input
                type="checkbox"
                checked={editingForm.enabled ?? true}
                onChange={(e) => setEditingForm({ ...editingForm, enabled: e.target.checked })}
                className="rounded border-zinc-600 bg-zinc-900"
              />
              {t.formEnabled}
            </label>
            <label className="flex flex-col text-xs text-zinc-300 md:col-span-2">
              {t.formNote}
              <input
                type="text"
                value={editingForm.note ?? ""}
                onChange={(e) => setEditingForm({ ...editingForm, note: e.target.value })}
                className="mt-1 rounded border border-zinc-700 bg-zinc-900 px-2 py-1 text-sm text-zinc-100"
              />
            </label>
          </div>
          <div className="mt-3 flex justify-end gap-2">
            <button
              type="button"
              onClick={() => setEditingForm(null)}
              className="rounded-md border border-zinc-600 bg-zinc-800 px-3 py-1 text-sm text-zinc-100 hover:bg-zinc-700"
            >
              {t.cancel}
            </button>
            <button
              type="button"
              onClick={submit}
              disabled={loading}
              className="rounded-md border border-emerald-500/40 bg-emerald-500/15 px-3 py-1 text-sm text-emerald-200 hover:bg-emerald-500/25 disabled:opacity-50"
            >
              {t.save}
            </button>
          </div>
        </div>
      )}
    </section>
  );
}
