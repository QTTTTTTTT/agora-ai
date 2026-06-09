// AdminFeatureFlagsSection — admin console panel for toggling
// platform feature flags. Calls /api/admin/feature-flags for the
// list and PUT /api/admin/feature-flags/{key} to flip a row.
//
// Drives two effects when a flag flips:
//   1. SPA hides nav entries / route content via the FeatureFlags
//      context (useFeatureFlag).
//   2. For flags marked enforce_server_gate, the server returns
//      503 from gated endpoints (see feature_flags.go's
//      featureGateMiddleware) on the very next request after the
//      cache TTL expires (~5s).

import React, { useCallback, useEffect, useState } from "react";
import { apiGet, apiPut, formatApiError } from "../lib/api";
import { useRefreshFeatureFlags } from "../lib/featureFlags";
import { formatDateTimeForLanguage, useAppPreferences, type AppLanguage } from "../lib/preferences";

interface FeatureFlagWire {
  key: string;
  label: string;
  description?: string;
  enabled: boolean;
  affectsRoutes?: string[];
  enforceServerGate?: boolean;
  updatedAt: string;
  updatedBy?: string;
}

interface FeatureFlagsResponse {
  flags?: FeatureFlagWire[];
}

function copyForLanguage(language: AppLanguage) {
  if (language === "en-US") {
    return {
      title: "Feature flags",
      subtitle: "Pause or resume product surfaces without a code release. The web app picks up changes within ~60s; gated server endpoints flip within ~5s.",
      retry: "Retry",
      loadFailed: "Could not load feature flags",
      enabled: "Enabled",
      disabled: "Disabled",
      enforceLabel: "Hard gate",
      enforceTooltip: "When OFF, the server also rejects requests to this surface with 503.",
      affectsLabel: "Affected routes",
      noAffects: "Soft hide only",
      updatedBy: "Updated by",
      saving: "Saving...",
      empty: "No feature flags registered yet.",
    };
  }
  return {
    title: "功能开关",
    subtitle: "无需发版即可暂停或恢复产品功能。Web 端 60 秒内同步；启用 server 闸的接口 5 秒内生效。",
    retry: "重试",
    loadFailed: "加载功能开关失败",
    enabled: "已开启",
    disabled: "已关闭",
    enforceLabel: "服务端闸",
    enforceTooltip: "关闭后，服务端会同时拒绝相关接口的请求并返回 503。",
    affectsLabel: "影响路径",
    noAffects: "仅前端隐藏",
    updatedBy: "最近修改",
    saving: "保存中...",
    empty: "暂未注册任何功能开关。",
  };
}

interface AdminFeatureFlagsSectionProps {
  language: AppLanguage;
}

const AdminFeatureFlagsSection: React.FC<AdminFeatureFlagsSectionProps> = ({ language }) => {
  const copy = copyForLanguage(language);
  const refreshContext = useRefreshFeatureFlags();
  const [flags, setFlags] = useState<FeatureFlagWire[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [saving, setSaving] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await apiGet<FeatureFlagsResponse>("/api/admin/feature-flags");
      setFlags(resp?.flags ?? []);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed]);

  useEffect(() => {
    void load();
  }, [load]);

  const toggle = useCallback(
    async (flag: FeatureFlagWire) => {
      setSaving(flag.key);
      try {
        const updated = await apiPut<FeatureFlagWire>(`/api/admin/feature-flags/${encodeURIComponent(flag.key)}`, {
          enabled: !flag.enabled,
        });
        setFlags((prev) => prev.map((f) => (f.key === flag.key ? { ...f, ...updated } : f)));
        // Tell the global context to re-pull immediately so the
        // sidebar nav reacts without waiting on the 60s timer.
        void refreshContext();
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setSaving(null);
      }
    },
    [copy.loadFailed, refreshContext],
  );

  return (
    <section className="rounded-2xl border border-ink-100 bg-white px-5 py-5 shadow-envelope">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-ink-900">{copy.title}</h2>
          <p className="mt-1 max-w-2xl text-sm text-ink-500">{copy.subtitle}</p>
        </div>
        <button
          type="button"
          onClick={() => void load()}
          className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100"
        >
          {copy.retry}
        </button>
      </div>
      {error ? <p className="mt-3 text-sm text-rose-600">{error}</p> : null}
      <div className="mt-4 overflow-hidden rounded-xl ring-1 ring-ink-100">
        <table className="w-full text-sm">
          <thead className="bg-cream-100 text-left text-xs uppercase tracking-wider text-ink-500">
            <tr>
              <th className="px-4 py-3">Flag</th>
              <th className="px-4 py-3">{copy.affectsLabel}</th>
              <th className="px-4 py-3">{copy.enforceLabel}</th>
              <th className="px-4 py-3">{copy.updatedBy}</th>
              <th className="px-4 py-3 text-right">{copy.enabled}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-100 bg-white">
            {loading ? (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-400">
                  {language === "en-US" ? "Loading…" : "加载中…"}
                </td>
              </tr>
            ) : flags.length === 0 ? (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-400">
                  {copy.empty}
                </td>
              </tr>
            ) : (
              flags.map((flag) => (
                <tr key={flag.key}>
                  <td className="px-4 py-3 align-top">
                    <p className="font-medium text-ink-900">{flag.label}</p>
                    <p className="mt-0.5 text-xs text-ink-500">{flag.key}</p>
                    {flag.description ? (
                      <p className="mt-1 max-w-md text-xs text-ink-500">{flag.description}</p>
                    ) : null}
                  </td>
                  <td className="px-4 py-3 align-top text-xs text-ink-500">
                    {flag.affectsRoutes && flag.affectsRoutes.length > 0 ? (
                      <ul className="space-y-0.5">
                        {flag.affectsRoutes.map((r) => (
                          <li key={r} className="font-mono">{r}</li>
                        ))}
                      </ul>
                    ) : (
                      <span>{copy.noAffects}</span>
                    )}
                  </td>
                  <td className="px-4 py-3 align-top">
                    {flag.enforceServerGate ? (
                      <span
                        title={copy.enforceTooltip}
                        className="inline-flex items-center rounded-full bg-rose-50 px-2.5 py-0.5 text-[11px] font-medium text-rose-700"
                      >
                        {copy.enforceLabel}
                      </span>
                    ) : (
                      <span className="text-xs text-ink-400">—</span>
                    )}
                  </td>
                  <td className="px-4 py-3 align-top text-xs text-ink-500">
                    <p>{formatDateTimeForLanguage(flag.updatedAt, language)}</p>
                    {flag.updatedBy ? <p className="text-ink-400">{flag.updatedBy}</p> : null}
                  </td>
                  <td className="px-4 py-3 align-top text-right">
                    <button
                      type="button"
                      onClick={() => void toggle(flag)}
                      disabled={saving === flag.key}
                      className={`inline-flex items-center gap-2 rounded-full px-3 py-1 text-xs font-semibold transition ${
                        flag.enabled
                          ? "bg-emerald-100 text-emerald-700 hover:bg-emerald-200"
                          : "bg-ink-100 text-ink-500 hover:bg-ink-200"
                      } disabled:opacity-60`}
                      aria-pressed={flag.enabled}
                    >
                      <span
                        className={`h-2.5 w-2.5 rounded-full transition ${
                          flag.enabled ? "bg-emerald-500" : "bg-ink-300"
                        }`}
                      />
                      {saving === flag.key
                        ? copy.saving
                        : flag.enabled
                          ? copy.enabled
                          : copy.disabled}
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
};

export default AdminFeatureFlagsSection;
