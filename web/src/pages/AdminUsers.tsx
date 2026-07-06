// AdminUsers — read-only admin user management console at
// /admin/users. Composition: stats cards, list table, detail
// drawer. The page itself is thin — each subsystem lives in its
// own component so they can be reused (or replaced) independently.
//
// Auth: assumes the route guard (App.tsx <RequireAdmin>) has
// already redirected non-admins. The handler still 403s server-side
// so a malicious client can't bypass — the client guard is only a
// UX courtesy.

import React, { useCallback, useEffect, useState } from "react";
import AdminUsersStatsCards from "../components/AdminUsersStatsCards";
import AdminUsersList from "../components/AdminUsersList";
import AdminUserDetailDrawer from "../components/AdminUserDetailDrawer";
import AdminUsageAnalyticsSection from "../components/AdminUsageAnalyticsSection";
import {
  fetchAdminUsersStats,
  formatApiError,
  type AdminUsersStatsResponse,
} from "../lib/api";

export default function AdminUsers() {
  const [stats, setStats] = useState<AdminUsersStatsResponse | null>(null);
  const [statsLoading, setStatsLoading] = useState(false);
  const [statsError, setStatsError] = useState<string | null>(null);
  const [selectedUserId, setSelectedUserId] = useState<string | null>(null);

  const loadStats = useCallback(async () => {
    setStatsLoading(true);
    setStatsError(null);
    try {
      const resp = await fetchAdminUsersStats();
      setStats(resp);
    } catch (err) {
      setStatsError(formatApiError(err, "加载统计失败"));
    } finally {
      setStatsLoading(false);
    }
  }, []);

  useEffect(() => {
    void loadStats();
  }, [loadStats]);

  return (
    <div className="mx-auto max-w-7xl px-4 py-8 space-y-6">
      <header>
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h1 className="text-2xl font-semibold text-ink-900">用户管理</h1>
            <p className="mt-1 text-sm text-ink-500">
              只读视图：注册用户、订阅分布、LLM 消费明细。修改角色 / 套餐请到原有的对应入口。
            </p>
          </div>
          <a href="#admin-usage-analytics" className="rounded-full bg-indigo-600 px-3 py-1.5 text-xs font-semibold text-white shadow-sm hover:bg-indigo-700">
            大师团与埋点统计
          </a>
        </div>
      </header>

      <AdminUsageAnalyticsSection />

      <AdminUsersStatsCards
        stats={stats}
        loading={statsLoading}
        error={statsError}
        onRetry={loadStats}
      />

      <AdminUsersList
        selectedUserId={selectedUserId}
        onSelectUser={(id) => setSelectedUserId(id)}
      />

      {selectedUserId ? (
        <AdminUserDetailDrawer
          key={selectedUserId}
          userId={selectedUserId}
          onClose={() => setSelectedUserId(null)}
        />
      ) : null}
    </div>
  );
}
