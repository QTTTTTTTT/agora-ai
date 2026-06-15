// AdminUsersList — searchable, filterable, paginated table for the
// admin user console. v1 is read-only; clicking a row hands the
// userId up to the parent which opens AdminUserDetailDrawer.
//
// Search + tier filter both submit on Enter (or via the explicit
// "应用" button) — we don't debounce because the seed dataset is
// small and continuous network thrash from per-keystroke fetches is
// worse than one extra Enter press.

import React, { useCallback, useEffect, useMemo, useState } from "react";
import {
  fetchAdminUsersList,
  formatApiError,
  type AdminUsersListItem,
  type AdminUsersListParams,
  type AdminUsersListResponse,
} from "../lib/api";

interface AdminUsersListProps {
  selectedUserId: string | null;
  onSelectUser: (userId: string) => void;
  // refreshKey lets the parent force a reload (e.g. after a write
  // that v2 will add — currently unused but cheap to keep so the
  // component contract is stable).
  refreshKey?: number;
}

const tierOptions = [
  { value: "", label: "全部套餐" },
  { value: "free", label: "免费" },
  { value: "pro", label: "Pro" },
  { value: "premium", label: "Premium" },
  { value: "enterprise", label: "Enterprise" },
];

const roleColors: Record<string, string> = {
  super_admin: "bg-indigo-100 text-indigo-700",
  admin: "bg-emerald-100 text-emerald-700",
  user: "bg-ink-100 text-ink-700",
};

const tierColors: Record<string, string> = {
  enterprise: "bg-violet-100 text-violet-700",
  premium: "bg-amber-100 text-amber-700",
  pro: "bg-sky-100 text-sky-700",
  free: "bg-ink-100 text-ink-600",
};

const statusColors: Record<string, string> = {
  active: "bg-emerald-100 text-emerald-700",
  suspended: "bg-amber-100 text-amber-700",
  deleted: "bg-rose-100 text-rose-700",
};

function formatCents(cents: number): string {
  if (!Number.isFinite(cents) || cents === 0) return "$0.00";
  return `$${(cents / 100).toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`;
}

function formatDateTime(iso?: string): string {
  if (!iso) return "—";
  try {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
  } catch {
    return iso;
  }
}

export default function AdminUsersList(props: AdminUsersListProps) {
  const { selectedUserId, onSelectUser, refreshKey } = props;
  const [search, setSearch] = useState("");
  const [tier, setTier] = useState("");
  const [page, setPage] = useState(1);
  const [size] = useState(50);
  const [data, setData] = useState<AdminUsersListResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // We keep one "applied" copy of search/tier separate from the
  // controlled inputs above so the user can edit the search box
  // without firing a request on every keystroke. apply() syncs the
  // controlled state into the request params.
  const [applied, setApplied] = useState<AdminUsersListParams>({});

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchAdminUsersList({ ...applied, page, size });
      setData(resp);
    } catch (err) {
      setError(formatApiError(err, "加载用户列表失败"));
    } finally {
      setLoading(false);
    }
  }, [applied, page, size]);

  useEffect(() => {
    void load();
  }, [load, refreshKey]);

  const apply = useCallback(() => {
    setApplied({ q: search, tier });
    setPage(1);
  }, [search, tier]);

  const totalPages = useMemo(() => {
    if (!data || data.size <= 0) return 1;
    return Math.max(1, Math.ceil(data.total / data.size));
  }, [data]);

  return (
    <section className="rounded-2xl border border-ink-100 bg-white px-5 py-5 shadow-envelope">
      <div className="flex flex-wrap items-end gap-3">
        <div className="grow">
          <h2 className="text-lg font-semibold text-ink-900">用户列表</h2>
          <p className="mt-1 text-sm text-ink-500">
            {data ? `${data.total} 个用户` : "—"} · 点击一行查看详情
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <input
            type="search"
            placeholder="搜索邮箱 / 用户名 / 显示名"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") apply();
            }}
            className="w-64 rounded-full border border-ink-200 bg-white px-3 py-1.5 text-xs text-ink-900 focus:border-ink-400 focus:outline-none"
          />
          <select
            value={tier}
            onChange={(e) => setTier(e.target.value)}
            className="rounded-full border border-ink-200 bg-white px-3 py-1.5 text-xs text-ink-900 focus:border-ink-400 focus:outline-none"
          >
            {tierOptions.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
          <button
            type="button"
            onClick={apply}
            className="rounded-full border border-ink-300 bg-ink-900 px-3 py-1.5 text-xs font-medium text-white hover:bg-ink-700"
          >
            应用
          </button>
        </div>
      </div>

      {error ? <p className="mt-3 text-sm text-rose-600">{error}</p> : null}

      <div className="mt-4 overflow-hidden rounded-xl ring-1 ring-ink-100">
        <table className="w-full text-sm">
          <thead className="bg-cream-100 text-left text-xs uppercase tracking-wider text-ink-500">
            <tr>
              <th className="px-4 py-3">用户</th>
              <th className="px-4 py-3">角色</th>
              <th className="px-4 py-3">状态</th>
              <th className="px-4 py-3">套餐</th>
              <th className="px-4 py-3 text-right">LLM 累计消费</th>
              <th className="px-4 py-3">注册</th>
              <th className="px-4 py-3">最近登录</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-100 bg-white">
            {loading ? (
              <tr>
                <td colSpan={7} className="px-4 py-6 text-center text-sm text-ink-400">
                  加载中…
                </td>
              </tr>
            ) : !data || data.users.length === 0 ? (
              <tr>
                <td colSpan={7} className="px-4 py-6 text-center text-sm text-ink-400">
                  没有匹配的用户
                </td>
              </tr>
            ) : (
              data.users.map((u: AdminUsersListItem) => {
                const isSelected = u.id === selectedUserId;
                return (
                  <tr
                    key={u.id}
                    onClick={() => onSelectUser(u.id)}
                    className={`cursor-pointer transition-colors ${
                      isSelected ? "bg-indigo-50" : "hover:bg-cream-50"
                    }`}
                  >
                    <td className="px-4 py-3 align-top">
                      <p className="font-medium text-ink-900">
                        {u.displayName || u.username || u.email || "—"}
                      </p>
                      <p className="text-xs text-ink-500">{u.email || u.id}</p>
                    </td>
                    <td className="px-4 py-3 align-top">
                      <span
                        className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-[11px] font-medium ${
                          roleColors[u.role] ?? "bg-ink-100 text-ink-700"
                        }`}
                      >
                        {u.role}
                      </span>
                    </td>
                    <td className="px-4 py-3 align-top">
                      <span
                        className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-[11px] font-medium ${
                          statusColors[u.status] ?? "bg-ink-100 text-ink-700"
                        }`}
                      >
                        {u.status}
                      </span>
                    </td>
                    <td className="px-4 py-3 align-top">
                      <span
                        className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-[11px] font-medium ${
                          tierColors[u.currentTier] ?? "bg-ink-100 text-ink-700"
                        }`}
                      >
                        {u.currentTier}
                      </span>
                      {u.tierUntil ? (
                        <p className="mt-1 text-[10px] text-ink-400">至 {formatDateTime(u.tierUntil)}</p>
                      ) : null}
                    </td>
                    <td className="px-4 py-3 text-right align-top">
                      <p className="font-mono text-sm text-ink-900">{formatCents(u.lifetimeLLMCostCents)}</p>
                      <p className="text-[10px] text-ink-400">{u.lifetimeLLMCalls.toLocaleString()} calls</p>
                    </td>
                    <td className="px-4 py-3 text-xs text-ink-500 align-top">{formatDateTime(u.createdAt)}</td>
                    <td className="px-4 py-3 text-xs text-ink-500 align-top">{formatDateTime(u.lastLoginAt)}</td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>

      {data && data.total > size ? (
        <div className="mt-3 flex items-center justify-between text-xs text-ink-500">
          <span>
            第 {data.page} / {totalPages} 页 · 每页 {data.size}
          </span>
          <div className="flex items-center gap-2">
            <button
              type="button"
              disabled={page <= 1}
              onClick={() => setPage((p) => Math.max(1, p - 1))}
              className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100 disabled:opacity-40"
            >
              上一页
            </button>
            <button
              type="button"
              disabled={page >= totalPages}
              onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
              className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100 disabled:opacity-40"
            >
              下一页
            </button>
          </div>
        </div>
      ) : null}
    </section>
  );
}
