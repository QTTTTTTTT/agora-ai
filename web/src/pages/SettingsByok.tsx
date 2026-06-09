import React, { useCallback, useEffect, useMemo, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import {
  createAdvisorByokKey,
  deleteAdvisorByokKey,
  fetchAdvisorBillingSummary,
  formatApiError,
  listAdvisorByokKeys,
  setAdvisorByokActive,
  updateAdvisorByokBudget,
  type AdvisorBillingSummary,
  type AdvisorByokCreateRequest,
  type AdvisorByokKey,
} from "../lib/api";
import { useAppPreferences } from "../lib/preferences";
import ByokCallLogPanel from "../components/ByokCallLogPanel";
import ByokEncryptionAnimation from "../components/ByokEncryptionAnimation";
import ByokSecurityGuide from "../components/ByokSecurityGuide";

// SettingsByok — Phase D-1 BYOK control plane for the /advisor mode.
//
// Three vertical sections:
//   1. Trust panel       — why we ask for keys + AES-GCM disclosure
//   2. Existing key list — masked previews + pause / revoke
//   3. Add new key wizard — provider picker + key paste +
//                           encryption animation
//
// This page deliberately doesn't show keys in plaintext anywhere
// after submission; the SPA only has the user's plaintext for the
// ~5 seconds between paste and encrypt-confirm. The fingerprint
// + sk-…last4 mask is what the user sees forever afterward.

interface ProviderOption {
  value: string;
  labelZh: string;
  labelEn: string;
  keyPrefixHint: string;
  helpLink: string;
}

const PROVIDER_OPTIONS: ProviderOption[] = [
  {
    value: "openai",
    labelZh: "OpenAI",
    labelEn: "OpenAI",
    keyPrefixHint: "sk-…",
    helpLink: "https://platform.openai.com/api-keys",
  },
  {
    value: "anthropic",
    labelZh: "Anthropic (Claude)",
    labelEn: "Anthropic (Claude)",
    keyPrefixHint: "sk-ant-…",
    helpLink: "https://console.anthropic.com/settings/keys",
  },
  {
    value: "deepseek",
    labelZh: "DeepSeek",
    labelEn: "DeepSeek",
    keyPrefixHint: "sk-…",
    helpLink: "https://platform.deepseek.com/api_keys",
  },
  {
    value: "kimi",
    labelZh: "Kimi (Moonshot)",
    labelEn: "Kimi (Moonshot)",
    keyPrefixHint: "sk-…",
    helpLink: "https://platform.moonshot.cn/console/api-keys",
  },
  {
    value: "doubao",
    labelZh: "豆包 (火山引擎)",
    labelEn: "Doubao (Volcengine)",
    keyPrefixHint: "ak-…",
    helpLink: "https://www.volcengine.com/docs/82379",
  },
  {
    value: "qwen",
    labelZh: "通义千问 Qwen",
    labelEn: "Tongyi Qwen",
    keyPrefixHint: "sk-…",
    helpLink: "https://help.aliyun.com/zh/dashscope/developer-reference/api-key-management",
  },
];

const COPY = {
  zh: {
    heading: "BYOK · 自带 LLM Key",
    sub: "在 /advisor 模式中使用你自己的 OpenAI / Anthropic / DeepSeek 等 API key — 平台只收服务费，token 消耗走你自己的账户。",
    backLink: "返回 advisor",
    trustTitle: "为什么我们要存你的 key？",
    trustBullets: [
      "/advisor 大师团队咨询每次会调用 10+ 次 LLM。如果走平台池，按 GPT-4 实价每月成本可达 ¥200+。",
      "BYOK 让你直接对接 OpenAI 计费，平台只收 ¥1/次 服务费，重度用户可以省 70% 以上。",
      "Key 以 AES-256-GCM 加密存储，加密 key 来自服务器 env（不入数据库 / 不入日志）。任何时候点 \"撤销\" 即可永久删除。",
    ],
    listTitle: "我的 key 列表",
    listEmpty: "还没有添加任何 key — 在下方选择服务商开始。",
    addTitle: "添加新 key",
    submit: "加密并保存",
    submitInProgress: "加密中…",
    providerLabel: "服务商",
    apiKeyLabel: "API Key (plaintext)",
    apiKeyHint: "直接粘贴你从服务商控制台复制的 key — 提交后立刻加密，不会留存明文。",
    labelLabel: "标签 (可选)",
    labelHint: "用来区分多张同服务商 key — 比如 \"主账号\" / \"测试\"。",
    budgetLabel: "月度预算 (USD)",
    budgetHint: "可选软上限：当 30 天内累计 token 消费超过此值时，我们会自动暂停这张 key 并切回平台池。0 = 无上限。",
    baseUrlLabel: "Base URL (可选)",
    baseUrlHint: "OpenAI 兼容端点可填自定义地址，比如 Azure OpenAI proxy。留空 = 用服务商默认。",
    modelLabel: "默认模型 (可选)",
    modelHint: "比如 \"gpt-4o\" / \"claude-3-5-sonnet-20241022\"。留空 = 跟随平台 tier 默认。",
    deleteAction: "撤销",
    pauseAction: "暂停",
    resumeAction: "恢复",
    deleteConfirm: "确定撤销这张 key？撤销后无法恢复，需要重新添加。",
    deleteReasonHint: "撤销原因 (可选)",
    keyStatusActive: "active",
    keyStatusPaused: "已暂停",
    keyStatusRevoked: "已撤销",
    lastUsedAt: "最近使用",
    lastVerifiedAt: "最近验证",
    revokedReason: "撤销原因",
    monthlyBudgetCap: "月度预算",
    budgetUnlimited: "无上限",
    quotaTitle: "本月 advisor 配额",
    quotaDeep: "深度咨询",
    quotaQuick: "快速咨询",
    quotaUnlimited: "无限",
    creditBalance: "credit 余额",
    nextReset: "下次重置",
    upgradeHint: "如果配额不够用，可以考虑升级到 ",
    upgradeLink: "更高套餐",
    planNotAllowed:
      "当前套餐不支持 BYOK。升级到 Pro 及以上套餐即可启用自带 key —— 重度用户每月最多可省 70% LLM 成本。",
    planNotAllowedAction: "查看套餐",
  },
  en: {
    heading: "BYOK · Bring Your Own LLM Key",
    sub: "Use your own OpenAI / Anthropic / DeepSeek API key in /advisor mode — the platform only charges a service fee, token spend goes to your own account.",
    backLink: "Back to advisor",
    trustTitle: "Why do we ask for your key?",
    trustBullets: [
      "/advisor consultations call the LLM 10+ times per consult. Through the platform pool, that costs around $30 / month at GPT-4 rates.",
      "BYOK routes spend through your own OpenAI billing — the platform charges only $0.15 per consult, heavy users save 70%+.",
      "Keys are stored AES-256-GCM-encrypted; the secret lives only in server env (never in DB or logs). Click \"revoke\" any time to delete permanently.",
    ],
    listTitle: "My keys",
    listEmpty: "No keys added yet — pick a provider below to start.",
    addTitle: "Add a new key",
    submit: "Encrypt & save",
    submitInProgress: "Encrypting…",
    providerLabel: "Provider",
    apiKeyLabel: "API Key (plaintext)",
    apiKeyHint:
      "Paste the key directly from your provider's console — we encrypt on submit and never persist the plaintext.",
    labelLabel: "Label (optional)",
    labelHint: "Differentiate multiple keys for the same provider — e.g. \"primary\" / \"sandbox\".",
    budgetLabel: "Monthly budget (USD)",
    budgetHint:
      "Optional soft cap: when your rolling 30-day token spend exceeds this, BYOK auto-pauses and we fall back to the platform pool. 0 = no cap.",
    baseUrlLabel: "Base URL (optional)",
    baseUrlHint:
      "For OpenAI-compatible endpoints (e.g. Azure OpenAI proxy). Leave blank to use the provider default.",
    modelLabel: "Default model (optional)",
    modelHint: "e.g. \"gpt-4o\" / \"claude-3-5-sonnet-20241022\". Blank = follow platform tier default.",
    deleteAction: "Revoke",
    pauseAction: "Pause",
    resumeAction: "Resume",
    deleteConfirm: "Revoke this key? Cannot be undone — you'll need to re-add it.",
    deleteReasonHint: "Reason for revoking (optional)",
    keyStatusActive: "active",
    keyStatusPaused: "paused",
    keyStatusRevoked: "revoked",
    lastUsedAt: "Last used",
    lastVerifiedAt: "Last verified",
    revokedReason: "Revoke reason",
    monthlyBudgetCap: "Monthly cap",
    budgetUnlimited: "no cap",
    quotaTitle: "This month's advisor quota",
    quotaDeep: "Deep consults",
    quotaQuick: "Quick consults",
    quotaUnlimited: "unlimited",
    creditBalance: "credit balance",
    nextReset: "Resets",
    upgradeHint: "Out of quota? Consider upgrading to ",
    upgradeLink: "a higher tier",
    planNotAllowed:
      "BYOK requires a paid plan. Upgrading to Pro or higher unlocks it — heavy users can save up to 70% on LLM costs.",
    planNotAllowedAction: "View plans",
  },
};

const formatProviderLabel = (provider: string, lang: "zh" | "en") => {
  const found = PROVIDER_OPTIONS.find((p) => p.value === provider);
  if (!found) return provider.toUpperCase();
  return lang === "zh" ? found.labelZh : found.labelEn;
};

const SettingsByok: React.FC = () => {
  const { language } = useAppPreferences();
  const lang = language === "zh-CN" ? "zh" : "en";
  const copy = COPY[lang];
  const navigate = useNavigate();

  const [keys, setKeys] = useState<AdvisorByokKey[]>([]);
  const [summary, setSummary] = useState<AdvisorBillingSummary | null>(null);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [showAnimation, setShowAnimation] = useState(false);
  const [form, setForm] = useState<AdvisorByokCreateRequest>({
    provider: PROVIDER_OPTIONS[0].value,
    label: "",
    api_key: "",
    base_url: "",
    model_name: "",
    monthly_budget_cents_usd: 0,
  });

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const [keyList, sum] = await Promise.all([
        listAdvisorByokKeys(),
        fetchAdvisorBillingSummary().catch(() => null),
      ]);
      setKeys(keyList.keys);
      setSummary(sum);
    } catch (err) {
      setError(formatApiError(err, lang === "zh" ? "加载失败" : "Load failed"));
    } finally {
      setLoading(false);
    }
  }, [lang]);

  useEffect(() => {
    void refresh();
  }, [refresh]);

  const planNotAllowed = useMemo(
    () => Boolean(summary && summary.allow_advisor_byok === false),
    [summary],
  );

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!form.api_key.trim() || !form.provider) return;
    setShowAnimation(true);
    setSubmitting(true);
    setError(null);
    try {
      // Run the encryption animation for a few seconds so the
      // user has a moment to mentally connect "I pasted plaintext"
      // → "it just got encrypted before storage". We don't gate
      // on this for retries — the actual API call is awaited
      // afterwards.
      await new Promise((resolve) => setTimeout(resolve, 1500));
      await createAdvisorByokKey({
        ...form,
        monthly_budget_cents_usd: Math.max(0, Math.floor(form.monthly_budget_cents_usd || 0)),
      });
      // Reset form. Keep the encryption animation visible for a
      // beat so it doesn't look glitchy.
      setForm({
        provider: PROVIDER_OPTIONS[0].value,
        label: "",
        api_key: "",
        base_url: "",
        model_name: "",
        monthly_budget_cents_usd: 0,
      });
      await refresh();
    } catch (err) {
      setError(formatApiError(err, lang === "zh" ? "保存失败" : "Save failed"));
    } finally {
      setSubmitting(false);
      // Leave the animation visible briefly so the user sees the
      // "complete" frame.
      setTimeout(() => setShowAnimation(false), 600);
    }
  };

  const handleDelete = async (key: AdvisorByokKey) => {
    if (!confirm(copy.deleteConfirm)) return;
    try {
      await deleteAdvisorByokKey(key.id, "user_revoked");
      await refresh();
    } catch (err) {
      setError(formatApiError(err, lang === "zh" ? "撤销失败" : "Revoke failed"));
    }
  };

  const handlePauseToggle = async (key: AdvisorByokKey) => {
    try {
      await setAdvisorByokActive(key.id, !key.is_active);
      await refresh();
    } catch (err) {
      setError(formatApiError(err, lang === "zh" ? "操作失败" : "Update failed"));
    }
  };

  const handleBudgetChange = async (key: AdvisorByokKey, cents: number) => {
    try {
      await updateAdvisorByokBudget(key.id, Math.max(0, Math.floor(cents)));
      await refresh();
    } catch (err) {
      setError(formatApiError(err, lang === "zh" ? "更新预算失败" : "Update budget failed"));
    }
  };

  return (
    <div className="min-h-screen bg-slate-50 px-6 py-8">
      <div className="mx-auto max-w-5xl space-y-6">
        <header className="flex items-start justify-between gap-4">
          <div>
            <h1 className="text-2xl font-semibold tracking-tight text-slate-900">{copy.heading}</h1>
            <p className="mt-1 text-sm text-slate-500">{copy.sub}</p>
          </div>
          <Link
            to="/advisor"
            className="rounded-full border border-slate-200 bg-white px-4 py-1.5 text-xs font-medium text-slate-600 hover:bg-slate-50"
          >
            {copy.backLink}
          </Link>
        </header>

        {error ? (
          <div className="rounded-lg border border-rose-200 bg-rose-50 p-3 text-sm text-rose-700">
            {error}
          </div>
        ) : null}

        {planNotAllowed ? (
          <div className="rounded-xl border border-amber-200 bg-amber-50 p-4">
            <p className="text-sm text-amber-800">{copy.planNotAllowed}</p>
            <button
              type="button"
              onClick={() => navigate("/subscription")}
              className="mt-3 rounded-md bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-700"
            >
              {copy.planNotAllowedAction}
            </button>
          </div>
        ) : null}

        {/* Quota / credits header */}
        {summary ? (
          <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
            <h2 className="text-sm font-semibold text-slate-800">{copy.quotaTitle}</h2>
            <div className="mt-3 grid grid-cols-1 gap-4 sm:grid-cols-3">
              <div>
                <div className="text-xs text-slate-500">{copy.quotaDeep}</div>
                <div className="mt-0.5 text-lg font-semibold text-slate-900">
                  {summary.deep_remaining === -1
                    ? copy.quotaUnlimited
                    : `${summary.deep_remaining} / ${summary.deep_limit}`}
                </div>
                {summary.credit_deep_balance ? (
                  <div className="mt-0.5 text-[11px] text-emerald-700">
                    +{summary.credit_deep_balance} {copy.creditBalance}
                  </div>
                ) : null}
              </div>
              <div>
                <div className="text-xs text-slate-500">{copy.quotaQuick}</div>
                <div className="mt-0.5 text-lg font-semibold text-slate-900">
                  {summary.quick_remaining === -1
                    ? copy.quotaUnlimited
                    : `${summary.quick_remaining} / ${summary.quick_limit}`}
                </div>
                {summary.credit_quick_balance ? (
                  <div className="mt-0.5 text-[11px] text-emerald-700">
                    +{summary.credit_quick_balance} {copy.creditBalance}
                  </div>
                ) : null}
              </div>
              <div>
                <div className="text-xs text-slate-500">{copy.nextReset}</div>
                <div className="mt-0.5 text-sm text-slate-700">
                  {new Date(summary.next_reset_at).toLocaleDateString(lang === "zh" ? "zh-CN" : "en-US")}
                </div>
                {summary.upgrade_suggested ? (
                  <div className="mt-0.5 text-[11px] text-slate-500">
                    {copy.upgradeHint}
                    <Link to="/subscription" className="text-blue-600 hover:underline">
                      {copy.upgradeLink}
                    </Link>
                  </div>
                ) : null}
              </div>
            </div>
          </section>
        ) : null}

        {/* Trust panel */}
        <section className="rounded-xl border border-blue-200 bg-blue-50/40 p-5">
          <h2 className="text-sm font-semibold text-blue-900">{copy.trustTitle}</h2>
          <ul className="mt-3 space-y-2 text-sm text-blue-900/80">
            {copy.trustBullets.map((line, i) => (
              <li key={i} className="flex gap-2">
                <span className="mt-0.5 text-blue-600">•</span>
                <span>{line}</span>
              </li>
            ))}
          </ul>
        </section>

        {/* Existing keys list */}
        <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
          <h2 className="text-sm font-semibold text-slate-800">{copy.listTitle}</h2>
          {loading ? (
            <p className="mt-3 text-xs text-slate-500">{lang === "zh" ? "加载中…" : "Loading…"}</p>
          ) : keys.length === 0 ? (
            <p className="mt-3 text-sm text-slate-500">{copy.listEmpty}</p>
          ) : (
            <div className="mt-3 space-y-3">
              {keys.map((key) => {
                const isRevoked = Boolean(key.revoked_at);
                const status = isRevoked
                  ? copy.keyStatusRevoked
                  : key.is_active
                  ? copy.keyStatusActive
                  : copy.keyStatusPaused;
                return (
                  <div
                    key={key.id}
                    className={`flex flex-col gap-3 rounded-lg border ${
                      isRevoked
                        ? "border-slate-200 bg-slate-50 opacity-60"
                        : key.is_active
                        ? "border-emerald-200 bg-emerald-50/30"
                        : "border-amber-200 bg-amber-50/30"
                    } p-4 sm:flex-row sm:items-center sm:justify-between`}
                  >
                    <div className="space-y-1">
                      <div className="flex items-center gap-2 text-sm font-medium text-slate-900">
                        <span>{formatProviderLabel(key.provider, lang)}</span>
                        {key.label ? (
                          <span className="rounded-full bg-slate-100 px-2 py-0.5 text-[10px] font-normal text-slate-600">
                            {key.label}
                          </span>
                        ) : null}
                        <span
                          className={`rounded-full px-2 py-0.5 text-[10px] font-medium ${
                            isRevoked
                              ? "bg-slate-200 text-slate-700"
                              : key.is_active
                              ? "bg-emerald-100 text-emerald-800"
                              : "bg-amber-100 text-amber-800"
                          }`}
                        >
                          {status}
                        </span>
                      </div>
                      <div className="font-mono text-xs text-slate-500">
                        {key.api_key_preview}
                        <span className="ml-2 text-slate-400">
                          ({key.api_key_fingerprint.slice(0, 8)}…)
                        </span>
                      </div>
                      {key.model_name ? (
                        <div className="text-xs text-slate-500">model: {key.model_name}</div>
                      ) : null}
                      <div className="text-xs text-slate-500">
                        {copy.monthlyBudgetCap}:{" "}
                        <input
                          type="number"
                          defaultValue={(key.monthly_budget_cents_usd / 100).toFixed(0)}
                          className="ml-1 w-20 rounded border border-slate-200 bg-white px-1 text-right text-xs"
                          min={0}
                          step={5}
                          onBlur={(e) => {
                            const dollars = Number(e.target.value);
                            const cents = Math.round((isFinite(dollars) ? dollars : 0) * 100);
                            if (cents !== key.monthly_budget_cents_usd) {
                              void handleBudgetChange(key, cents);
                            }
                          }}
                          disabled={isRevoked}
                        />{" "}
                        USD{" "}
                        {key.monthly_budget_cents_usd === 0 ? (
                          <span className="text-slate-400">({copy.budgetUnlimited})</span>
                        ) : null}
                      </div>
                      {key.last_used_at ? (
                        <div className="text-[11px] text-slate-400">
                          {copy.lastUsedAt}: {new Date(key.last_used_at).toLocaleString()}
                        </div>
                      ) : null}
                      {key.revoked_reason ? (
                        <div className="text-[11px] text-slate-500">
                          {copy.revokedReason}: {key.revoked_reason}
                        </div>
                      ) : null}
                    </div>
                    {!isRevoked ? (
                      <div className="flex flex-wrap gap-2">
                        <button
                          type="button"
                          onClick={() => handlePauseToggle(key)}
                          className="rounded-md border border-slate-200 bg-white px-3 py-1 text-xs font-medium text-slate-700 hover:bg-slate-50"
                        >
                          {key.is_active ? copy.pauseAction : copy.resumeAction}
                        </button>
                        <button
                          type="button"
                          onClick={() => handleDelete(key)}
                          className="rounded-md border border-rose-200 bg-rose-50 px-3 py-1 text-xs font-medium text-rose-700 hover:bg-rose-100"
                        >
                          {copy.deleteAction}
                        </button>
                      </div>
                    ) : null}
                  </div>
                );
              })}
            </div>
          )}
        </section>

        {/* Add key wizard */}
        {!planNotAllowed ? (
          <section className="rounded-xl border border-slate-200 bg-white p-5 shadow-sm">
            <h2 className="text-sm font-semibold text-slate-800">{copy.addTitle}</h2>
            <form onSubmit={handleSubmit} className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
              <label className="text-xs font-medium text-slate-700">
                {copy.providerLabel}
                <select
                  value={form.provider}
                  onChange={(e) => setForm((f) => ({ ...f, provider: e.target.value }))}
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 text-sm"
                >
                  {PROVIDER_OPTIONS.map((p) => (
                    <option key={p.value} value={p.value}>
                      {lang === "zh" ? p.labelZh : p.labelEn}
                    </option>
                  ))}
                </select>
              </label>
              <label className="text-xs font-medium text-slate-700">
                {copy.labelLabel}
                <input
                  type="text"
                  value={form.label}
                  onChange={(e) => setForm((f) => ({ ...f, label: e.target.value }))}
                  placeholder="primary / sandbox / team"
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 text-sm"
                />
                <span className="block mt-0.5 text-[11px] text-slate-500">{copy.labelHint}</span>
              </label>
              <label className="text-xs font-medium text-slate-700 sm:col-span-2">
                {copy.apiKeyLabel}
                <input
                  type="password"
                  value={form.api_key}
                  onChange={(e) => setForm((f) => ({ ...f, api_key: e.target.value }))}
                  placeholder={
                    PROVIDER_OPTIONS.find((p) => p.value === form.provider)?.keyPrefixHint ?? "sk-…"
                  }
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 font-mono text-sm"
                  autoComplete="off"
                  spellCheck={false}
                />
                <span className="block mt-0.5 text-[11px] text-slate-500">{copy.apiKeyHint}</span>
              </label>
              <label className="text-xs font-medium text-slate-700">
                {copy.budgetLabel}
                <input
                  type="number"
                  min={0}
                  step={5}
                  value={(form.monthly_budget_cents_usd ?? 0) / 100}
                  onChange={(e) => {
                    const dollars = Number(e.target.value) || 0;
                    setForm((f) => ({ ...f, monthly_budget_cents_usd: Math.round(dollars * 100) }));
                  }}
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 text-sm"
                />
                <span className="block mt-0.5 text-[11px] text-slate-500">{copy.budgetHint}</span>
              </label>
              <label className="text-xs font-medium text-slate-700">
                {copy.modelLabel}
                <input
                  type="text"
                  value={form.model_name}
                  onChange={(e) => setForm((f) => ({ ...f, model_name: e.target.value }))}
                  placeholder="gpt-4o / claude-3-5-sonnet-20241022"
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 font-mono text-sm"
                />
                <span className="block mt-0.5 text-[11px] text-slate-500">{copy.modelHint}</span>
              </label>
              <label className="text-xs font-medium text-slate-700 sm:col-span-2">
                {copy.baseUrlLabel}
                <input
                  type="text"
                  value={form.base_url}
                  onChange={(e) => setForm((f) => ({ ...f, base_url: e.target.value }))}
                  placeholder="https://api.openai.com/v1"
                  className="mt-1 w-full rounded border border-slate-200 bg-white px-2 py-1.5 font-mono text-sm"
                />
                <span className="block mt-0.5 text-[11px] text-slate-500">{copy.baseUrlHint}</span>
              </label>
              <div className="sm:col-span-2 flex items-center justify-end">
                <button
                  type="submit"
                  disabled={submitting || !form.api_key.trim()}
                  className="rounded-md bg-blue-600 px-4 py-2 text-sm font-medium text-white shadow-sm hover:bg-blue-700 disabled:cursor-not-allowed disabled:opacity-50"
                >
                  {submitting ? copy.submitInProgress : copy.submit}
                </button>
              </div>
            </form>
          </section>
        ) : null}

        {/* Encryption animation overlay — visible only during submit */}
        {showAnimation ? <ByokEncryptionAnimation lang={lang} /> : null}

        {/* Call log + security guide */}
        <ByokCallLogPanel lang={lang} />
        <ByokSecurityGuide lang={lang} />
      </div>
    </div>
  );
};

export default SettingsByok;
