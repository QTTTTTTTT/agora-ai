import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  fetchAdminUsageAnalytics,
  formatApiError,
  type AdminUsageAnalyticsResponse,
  type AdminUsageUserAggregate,
} from "../lib/api";
import { formatDateTimeForLanguage, useAppPreferences } from "../lib/preferences";

function featureLabel(key: string): string {
  return key.replace(/[_:.]+/g, " ").replace(/\s+/g, " ").trim() || key;
}

function roleLabel(role: string): string {
  if (role === "admin") return "管理员";
  if (role === "super_admin") return "超级管理员";
  return "普通用户";
}

export default function AdminUsageAnalyticsSection() {
  const { language } = useAppPreferences();
  const [data, setData] = useState<AdminUsageAnalyticsResponse>({ since: "", users: [] });
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const copy = useMemo(
    () =>
      language === "en-US"
        ? {
            title: "大师团与埋点统计 / Master team & usage analytics",
            subtitle: "Normal-user page entries and feature clicks in the last 30 days. Admin and super-admin users are excluded.",
            refresh: "Refresh usage",
            refreshing: "Refreshing…",
            empty: "No normal-user usage yet. Open /masters, /advisor or /daily-picks as a normal user, then refresh.",
            loadFailed: "Failed to load usage analytics",
            since: "Since",
            users: "users",
            total: "Total",
            pages: "Pages",
            actions: "Actions",
            activeDays: "Active days",
            lastSeen: "Last seen",
            topFeatures: "Top features",
          }
        : {
            title: "大师团与埋点统计",
            subtitle: "统计最近 30 天普通用户进入页面和使用功能的次数；已排除 admin / super_admin。",
            refresh: "刷新埋点统计",
            refreshing: "刷新中…",
            empty: "暂时还没有普通用户使用事件。请先用普通用户进入 /masters、/advisor 或 /daily-picks，再刷新。",
            loadFailed: "加载埋点统计失败",
            since: "统计起点",
            users: "个用户",
            total: "总次数",
            pages: "进入页面",
            actions: "功能使用",
            activeDays: "活跃天数",
            lastSeen: "最近使用",
            topFeatures: "高频功能",
          },
    [language],
  );

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const since = new Date(Date.now() - 30 * 24 * 60 * 60 * 1000).toISOString();
      const resp = await fetchAdminUsageAnalytics({ since, limit: 50 });
      setData(resp);
    } catch (err) {
      setError(formatApiError(err, copy.loadFailed));
    } finally {
      setLoading(false);
    }
  }, [copy.loadFailed]);

  useEffect(() => {
    void load();
  }, [load]);

  const rows = data.users.slice(0, 12);

  return (
    <section id="admin-usage-analytics" className="scroll-mt-6 rounded-3xl border border-indigo-200 bg-gradient-to-br from-indigo-50 via-white to-emerald-50 px-5 py-5 shadow-sm">
      <div className="flex flex-col gap-3 lg:flex-row lg:items-start lg:justify-between">
        <div>
          <p className="text-xs font-semibold uppercase tracking-[0.24em] text-indigo-500">Usage Analytics</p>
          <h2 className="mt-2 text-xl font-bold text-gray-950">{copy.title}</h2>
          <p className="mt-2 max-w-3xl text-sm text-gray-600">{copy.subtitle}</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          {data.since ? (
            <span className="rounded-full bg-white/80 px-3 py-1.5 text-xs font-medium text-indigo-700 ring-1 ring-indigo-100">
              {copy.since}: {formatDateTimeForLanguage(data.since, language)}
            </span>
          ) : null}
          <button
            type="button"
            onClick={() => void load()}
            disabled={loading}
            className="rounded-full bg-indigo-600 px-3 py-1.5 text-xs font-semibold text-white shadow-sm hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {loading ? copy.refreshing : copy.refresh}
          </button>
        </div>
      </div>

      <div className="mt-4 rounded-2xl border border-white/80 bg-white/90 p-4 shadow-sm">
        <div className="mb-3 flex flex-wrap items-center justify-between gap-2">
          <h3 className="text-base font-semibold text-gray-900">普通用户功能使用统计</h3>
          <span className="rounded-full bg-indigo-50 px-3 py-1 text-xs font-semibold text-indigo-700">
            {data.users.length} {copy.users}
          </span>
        </div>

        {error ? (
          <div className="rounded-xl border border-red-100 bg-red-50 p-4 text-sm text-red-700">{error}</div>
        ) : rows.length === 0 ? (
          <div className="rounded-xl border border-dashed border-gray-200 bg-white p-4 text-sm text-gray-500">{copy.empty}</div>
        ) : (
          <div className="overflow-hidden rounded-2xl border border-gray-100 bg-white">
            <div className="divide-y divide-gray-100">
              {rows.map((row: AdminUsageUserAggregate) => (
                <article key={row.user_id} className="grid gap-4 px-4 py-4 xl:grid-cols-[minmax(0,1.1fr)_minmax(0,1.6fr)] xl:items-center">
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <h4 className="truncate text-sm font-semibold text-gray-900">{row.display_name || row.email || row.user_id}</h4>
                      <span className="rounded-full bg-gray-100 px-2.5 py-1 text-[11px] font-medium text-gray-700">{roleLabel(row.role)}</span>
                    </div>
                    <p className="mt-1 truncate text-xs text-gray-500">{row.email || row.user_id}</p>
                    <p className="mt-1 font-mono text-[11px] text-gray-400">{row.user_id}</p>
                  </div>

                  <div className="space-y-3">
                    <div className="grid grid-cols-2 gap-2 sm:grid-cols-5">
                      <Metric label={copy.total} value={row.total_events} />
                      <Metric label={copy.pages} value={row.page_views} />
                      <Metric label={copy.actions} value={row.feature_uses} />
                      <Metric label={copy.activeDays} value={row.active_days} />
                      <Metric label={copy.lastSeen} value={formatDateTimeForLanguage(row.last_seen_at, language)} compact />
                    </div>
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="text-[11px] font-medium text-gray-500">{copy.topFeatures}</span>
                      {row.top_features.length === 0 ? (
                        <span className="text-xs text-gray-400">—</span>
                      ) : (
                        row.top_features.map((feature) => (
                          <span key={`${row.user_id}-${feature.event_name}-${feature.feature_key}`} className="rounded-full bg-indigo-50 px-2.5 py-1 text-[11px] font-medium text-indigo-700 ring-1 ring-indigo-100">
                            {featureLabel(feature.feature_key)} · {feature.count}
                          </span>
                        ))
                      )}
                    </div>
                  </div>
                </article>
              ))}
            </div>
          </div>
        )}
      </div>
    </section>
  );
}

function Metric({ label, value, compact = false }: { label: string; value: React.ReactNode; compact?: boolean }) {
  return (
    <div className="rounded-xl bg-gray-50 px-3 py-2">
      <p className="text-[11px] text-gray-500">{label}</p>
      <p className={`mt-1 font-semibold text-gray-900 ${compact ? "truncate text-xs" : "text-sm"}`}>{value}</p>
    </div>
  );
}
