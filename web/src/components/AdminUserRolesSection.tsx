// AdminUserRolesSection — admin console panel for promoting users
// to admin (and, for super admins, granting / revoking
// super_admin). Includes a search box because production user
// tables get long; results are limited to 200 rows server-side.
//
// Endpoints used:
//   GET  /api/admin/user-roles?q=<search>
//   PUT  /api/admin/user-roles/{userId}    body: { role: 'user' | 'admin' | 'super_admin' }
//
// Server enforces the authorisation matrix (see admin_user_roles.go);
// this component just surfaces the response. We block self-edits and
// flag the must-keep-one-super-admin invariant by surfacing the
// server's error message verbatim.

import React, { useCallback, useEffect, useMemo, useState } from "react";
import { apiGet, apiPut, formatApiError, getStoredSession } from "../lib/api";
import { useAppPreferences, type AppLanguage } from "../lib/preferences";

interface UserRoleWire {
  id: string;
  email: string;
  displayName: string;
  role: string;
  createdAt: string;
  lastLoginAt?: string;
}

interface UserRolesResponse {
  users?: UserRoleWire[];
}

const ROLE_OPTIONS = ["user", "admin", "super_admin"] as const;

type RoleOption = typeof ROLE_OPTIONS[number];

function copyForLanguage(language: AppLanguage) {
  if (language === "en-US") {
    return {
      title: "User roles",
      subtitle: "Promote a user to admin (or, with super admin authority, grant super admin). The system always keeps at least one super admin.",
      searchPlaceholder: "Search by email or name…",
      retry: "Retry",
      loadFailed: "Could not load user roles",
      empty: "No matching users.",
      roleLabels: { user: "User", admin: "Admin", super_admin: "Super admin" } as Record<string, string>,
      saving: "Saving…",
      apply: "Apply",
      youLabel: "You",
      noLastLogin: "Never logged in",
      lastLogin: "Last login",
    };
  }
  return {
    title: "用户角色管理",
    subtitle: "授权某位用户成为管理员；超级管理员可授予或撤销超级管理员权限。系统始终至少保留一位超级管理员。",
    searchPlaceholder: "按邮箱或昵称搜索…",
    retry: "重试",
    loadFailed: "加载用户角色失败",
    empty: "未找到匹配的用户。",
    roleLabels: { user: "普通用户", admin: "管理员", super_admin: "超级管理员" } as Record<string, string>,
    saving: "保存中…",
    apply: "保存",
    youLabel: "本人",
    noLastLogin: "从未登录",
    lastLogin: "最近登录",
  };
}

interface AdminUserRolesSectionProps {
  language: AppLanguage;
}

const AdminUserRolesSection: React.FC<AdminUserRolesSectionProps> = ({ language }) => {
  const { language: lang } = useAppPreferences();
  const copy = copyForLanguage(language ?? lang);
  const [users, setUsers] = useState<UserRoleWire[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [search, setSearch] = useState("");
  // pendingChanges holds the operator's draft role per user — they
  // commit one row at a time via the per-row "Apply" button so the
  // page can render multiple inline edits without an explicit save
  // bar.
  const [pendingChanges, setPendingChanges] = useState<Record<string, RoleOption>>({});
  const [savingId, setSavingId] = useState<string | null>(null);

  const session = getStoredSession();
  const selfId = session?.userId ?? "";

  const load = useCallback(
    async (q: string) => {
      setLoading(true);
      setError(null);
      try {
        const qs = q.trim() ? `?q=${encodeURIComponent(q.trim())}` : "";
        const resp = await apiGet<UserRolesResponse>(`/api/admin/user-roles${qs}`);
        setUsers(resp?.users ?? []);
        setPendingChanges({});
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setLoading(false);
      }
    },
    [copy.loadFailed],
  );

  useEffect(() => {
    void load("");
  }, [load]);

  const submitDraft = useCallback(
    async (id: string) => {
      const draft = pendingChanges[id];
      if (!draft) return;
      setSavingId(id);
      setError(null);
      try {
        await apiPut(`/api/admin/user-roles/${id}`, { role: draft });
        setUsers((prev) => prev.map((u) => (u.id === id ? { ...u, role: draft } : u)));
        setPendingChanges((prev) => {
          const next = { ...prev };
          delete next[id];
          return next;
        });
      } catch (err) {
        setError(formatApiError(err, copy.loadFailed));
      } finally {
        setSavingId(null);
      }
    },
    [copy.loadFailed, pendingChanges],
  );

  const formattedRows = useMemo(() => users, [users]);

  return (
    <section className="rounded-2xl border border-ink-100 bg-white px-5 py-5 shadow-envelope">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-ink-900">{copy.title}</h2>
          <p className="mt-1 max-w-2xl text-sm text-ink-500">{copy.subtitle}</p>
        </div>
        <div className="flex items-center gap-2">
          <input
            type="search"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === "Enter") {
                void load(search);
              }
            }}
            placeholder={copy.searchPlaceholder}
            className="w-56 rounded-full border border-ink-200 bg-white px-3 py-1.5 text-xs text-ink-900 focus:border-ink-400 focus:outline-none"
          />
          <button
            type="button"
            onClick={() => void load(search)}
            className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100"
          >
            {copy.retry}
          </button>
        </div>
      </div>
      {error ? <p className="mt-3 text-sm text-rose-600">{error}</p> : null}
      <div className="mt-4 overflow-hidden rounded-xl ring-1 ring-ink-100">
        <table className="w-full text-sm">
          <thead className="bg-cream-100 text-left text-xs uppercase tracking-wider text-ink-500">
            <tr>
              <th className="px-4 py-3">{language === "en-US" ? "User" : "用户"}</th>
              <th className="px-4 py-3">{language === "en-US" ? "Current role" : "当前角色"}</th>
              <th className="px-4 py-3">{language === "en-US" ? "New role" : "更改为"}</th>
              <th className="px-4 py-3">{copy.lastLogin}</th>
              <th className="px-4 py-3 text-right">{language === "en-US" ? "Action" : "操作"}</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-ink-100 bg-white">
            {loading ? (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-400">
                  {language === "en-US" ? "Loading…" : "加载中…"}
                </td>
              </tr>
            ) : formattedRows.length === 0 ? (
              <tr>
                <td colSpan={5} className="px-4 py-6 text-center text-sm text-ink-400">
                  {copy.empty}
                </td>
              </tr>
            ) : (
              formattedRows.map((u) => {
                const isSelf = u.id === selfId;
                const draft = pendingChanges[u.id];
                const dirty = draft && draft !== u.role;
                return (
                  <tr key={u.id}>
                    <td className="px-4 py-3 align-top">
                      <p className="font-medium text-ink-900">
                        {u.displayName || u.email}
                        {isSelf ? (
                          <span className="ml-2 rounded-full bg-ink-900 px-2 py-0.5 text-[10px] font-semibold text-white">
                            {copy.youLabel}
                          </span>
                        ) : null}
                      </p>
                      <p className="text-xs text-ink-500">{u.email}</p>
                    </td>
                    <td className="px-4 py-3 align-top">
                      <span
                        className={`inline-flex items-center rounded-full px-2.5 py-0.5 text-[11px] font-medium ${
                          u.role === "super_admin"
                            ? "bg-indigo-100 text-indigo-700"
                            : u.role === "admin"
                              ? "bg-emerald-100 text-emerald-700"
                              : "bg-ink-100 text-ink-700"
                        }`}
                      >
                        {copy.roleLabels[u.role] ?? u.role}
                      </span>
                    </td>
                    <td className="px-4 py-3 align-top">
                      <select
                        value={draft ?? u.role}
                        onChange={(e) =>
                          setPendingChanges((prev) => ({
                            ...prev,
                            [u.id]: e.target.value as RoleOption,
                          }))
                        }
                        disabled={isSelf}
                        className="rounded-full border border-ink-200 bg-white px-2.5 py-1 text-xs text-ink-900 focus:border-ink-400 focus:outline-none disabled:cursor-not-allowed disabled:opacity-60"
                      >
                        {ROLE_OPTIONS.map((r) => (
                          <option key={r} value={r}>
                            {copy.roleLabels[r] ?? r}
                          </option>
                        ))}
                      </select>
                    </td>
                    <td className="px-4 py-3 align-top text-xs text-ink-500">
                      {u.lastLoginAt
                        ? new Date(u.lastLoginAt).toLocaleString(language === "en-US" ? "en-US" : "zh-CN")
                        : copy.noLastLogin}
                    </td>
                    <td className="px-4 py-3 align-top text-right">
                      <button
                        type="button"
                        onClick={() => void submitDraft(u.id)}
                        disabled={!dirty || savingId === u.id || isSelf}
                        className="rounded-full bg-ink-900 px-3 py-1 text-xs font-semibold text-white transition hover:bg-ink-700 disabled:cursor-not-allowed disabled:opacity-40"
                      >
                        {savingId === u.id ? copy.saving : copy.apply}
                      </button>
                    </td>
                  </tr>
                );
              })
            )}
          </tbody>
        </table>
      </div>
    </section>
  );
};

export default AdminUserRolesSection;
