// AdminUserDetailDrawer — right-side slide-in panel that loads
// /api/admin/users/{userId} and renders profile + subscription
// history + LLM consumption breakdown. Read-only in v1.
//
// Layout: fixed right column, full viewport height, scrollable
// inner content. Click on the dim overlay or the close button to
// dismiss. The drawer remounts whenever userId changes (controlled
// via React's `key`) so we don't need to manually clear stale data.

import React, { useCallback, useEffect, useState } from "react";
import { Bar, BarChart, ResponsiveContainer, Tooltip, XAxis, YAxis } from "recharts";
import {
  fetchAdminUserDetail,
  formatApiError,
  type AdminUsageBreakdown,
  type AdminUserDetailResponse,
} from "../lib/api";

interface AdminUserDetailDrawerProps {
  userId: string;
  onClose: () => void;
}

function formatCents(cents: number): string {
  if (!Number.isFinite(cents)) return "—";
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

function formatDate(iso?: string): string {
  if (!iso) return "—";
  try {
    const d = new Date(iso);
    if (Number.isNaN(d.getTime())) return iso;
    const pad = (n: number) => String(n).padStart(2, "0");
    return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
  } catch {
    return iso;
  }
}

export default function AdminUserDetailDrawer(props: AdminUserDetailDrawerProps) {
  const { userId, onClose } = props;
  const [data, setData] = useState<AdminUserDetailResponse | null>(null);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);

  const load = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await fetchAdminUserDetail(userId);
      setData(resp);
    } catch (err) {
      setError(formatApiError(err, "加载用户详情失败"));
    } finally {
      setLoading(false);
    }
  }, [userId]);

  useEffect(() => {
    void load();
  }, [load]);

  // Esc key closes the drawer — keyboard-friendly is cheap when the
  // drawer is the only modal-like surface in this page.
  useEffect(() => {
    function onKey(e: KeyboardEvent) {
      if (e.key === "Escape") onClose();
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <div className="fixed inset-0 z-50 flex">
      <div className="grow bg-black/30" onClick={onClose} aria-label="关闭" />
      <aside className="flex h-full w-full max-w-3xl flex-col bg-white shadow-2xl">
        <header className="flex items-start justify-between gap-3 border-b border-ink-100 px-6 py-4">
          <div className="min-w-0">
            <h2 className="truncate text-lg font-semibold text-ink-900">
              {data?.profile.displayName || data?.profile.username || data?.profile.email || userId}
            </h2>
            <p className="truncate text-xs text-ink-500">
              {data?.profile.email || userId}
            </p>
          </div>
          <button
            type="button"
            onClick={onClose}
            className="rounded-full border border-ink-200 bg-white px-3 py-1 text-xs font-medium text-ink-700 hover:bg-cream-100"
          >
            关闭 (Esc)
          </button>
        </header>

        <div className="flex-1 overflow-y-auto px-6 py-5 space-y-6">
          {loading ? <p className="text-sm text-ink-400">加载中…</p> : null}
          {error ? (
            <div className="rounded-xl border border-rose-200 bg-rose-50 p-4 text-sm text-rose-700">
              {error}
              <button
                type="button"
                onClick={load}
                className="ml-2 rounded-full border border-rose-300 bg-white px-2.5 py-0.5 text-xs font-medium text-rose-700 hover:bg-rose-100"
              >
                重试
              </button>
            </div>
          ) : null}

          {data ? (
            <>
              <ProfileSection data={data} />
              <SubscriptionsSection data={data} />
              <UsageSummarySection data={data} />
              <WalletSection data={data} />
            </>
          ) : null}
        </div>
      </aside>
    </div>
  );
}

function ProfileSection({ data }: { data: AdminUserDetailResponse }) {
  const p = data.profile;
  const rows: Array<{ label: string; value: React.ReactNode }> = [
    { label: "用户 ID", value: <code className="text-xs text-ink-700">{p.id}</code> },
    { label: "用户名", value: p.username || "—" },
    { label: "显示名", value: p.displayName || "—" },
    { label: "邮箱", value: p.email || "—" },
    { label: "电话", value: p.phone || "—" },
    { label: "角色", value: p.role },
    { label: "账号状态", value: p.status },
    { label: "KYC", value: `${p.kycStatus} / ${p.kycLevel}` },
    { label: "邮箱已验证", value: p.emailVerified ? "是" : "否" },
    { label: "注册时间", value: formatDateTime(p.createdAt) },
    { label: "最近登录", value: formatDateTime(p.lastLoginAt) },
  ];
  return (
    <section>
      <h3 className="text-sm font-semibold text-ink-700">资料</h3>
      <dl className="mt-2 grid grid-cols-1 gap-x-4 gap-y-2 rounded-xl bg-cream-50 p-4 text-sm sm:grid-cols-2">
        {rows.map((row) => (
          <div key={row.label} className="flex items-baseline justify-between gap-3">
            <dt className="text-xs text-ink-500">{row.label}</dt>
            <dd className="truncate text-right text-ink-800">{row.value}</dd>
          </div>
        ))}
      </dl>
    </section>
  );
}

function SubscriptionsSection({ data }: { data: AdminUserDetailResponse }) {
  return (
    <section>
      <h3 className="text-sm font-semibold text-ink-700">订阅历史</h3>
      {data.subscriptions.length === 0 ? (
        <p className="mt-2 rounded-xl border border-dashed border-ink-200 px-4 py-6 text-center text-sm text-ink-400">
          暂无订阅记录
        </p>
      ) : (
        <div className="mt-2 overflow-hidden rounded-xl ring-1 ring-ink-100">
          <table className="w-full text-sm">
            <thead className="bg-cream-100 text-left text-xs uppercase tracking-wider text-ink-500">
              <tr>
                <th className="px-3 py-2">套餐</th>
                <th className="px-3 py-2">状态</th>
                <th className="px-3 py-2">开始</th>
                <th className="px-3 py-2">结束</th>
                <th className="px-3 py-2">支付方式</th>
                <th className="px-3 py-2">自动续费</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-ink-100 bg-white">
              {data.subscriptions.map((s, i) => (
                <tr key={i}>
                  <td className="px-3 py-2 font-medium text-ink-900">{s.planTier}</td>
                  <td className="px-3 py-2 text-xs text-ink-700">{s.status}</td>
                  <td className="px-3 py-2 text-xs text-ink-500">{formatDate(s.startDate)}</td>
                  <td className="px-3 py-2 text-xs text-ink-500">{formatDate(s.endDate)}</td>
                  <td className="px-3 py-2 text-xs text-ink-500">{s.paymentMethod || "—"}</td>
                  <td className="px-3 py-2 text-xs text-ink-500">{s.autoRenew ? "是" : "否"}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </section>
  );
}

function UsageSummarySection({ data }: { data: AdminUserDetailResponse }) {
  const u = data.usageSummary;
  const totalCalls = u.lifetimeCalls;
  const totalCost = u.lifetimeCostCents;
  return (
    <section>
      <h3 className="text-sm font-semibold text-ink-700">LLM 消费</h3>
      <div className="mt-2 grid grid-cols-1 gap-3 sm:grid-cols-2">
        <div className="rounded-xl bg-cream-50 px-4 py-3">
          <p className="text-xs text-ink-500">累计调用</p>
          <p className="mt-1 text-2xl font-semibold text-ink-900">{totalCalls.toLocaleString()}</p>
        </div>
        <div className="rounded-xl bg-cream-50 px-4 py-3">
          <p className="text-xs text-ink-500">累计成本</p>
          <p className="mt-1 text-2xl font-semibold text-ink-900">{formatCents(totalCost)}</p>
        </div>
      </div>

      {u.last30d.length > 0 ? (
        <div className="mt-3 rounded-xl border border-ink-100 bg-white px-4 py-3">
          <p className="text-xs text-ink-500">近 30 天日成本（USD）</p>
          <div className="mt-2 h-32">
            <ResponsiveContainer width="100%" height="100%">
              <BarChart data={u.last30d}>
                <XAxis
                  dataKey="date"
                  tick={{ fontSize: 10, fill: "#6b7280" }}
                  tickFormatter={(s: string) => s.slice(5)}
                  interval={Math.max(0, Math.floor(u.last30d.length / 8) - 1)}
                />
                <YAxis
                  tick={{ fontSize: 10, fill: "#6b7280" }}
                  tickFormatter={(v: number) => `$${(v / 100).toFixed(0)}`}
                />
                <Tooltip
                  contentStyle={{
                    fontSize: 11,
                    padding: "4px 8px",
                    border: "1px solid #e5e7eb",
                    borderRadius: 6,
                  }}
                  formatter={(value: number, name: string) => {
                    if (name === "costCents") return [formatCents(value), "成本"];
                    return [value, name];
                  }}
                />
                <Bar dataKey="costCents" fill="#6366f1" radius={[2, 2, 0, 0]} isAnimationActive={false} />
              </BarChart>
            </ResponsiveContainer>
          </div>
        </div>
      ) : null}

      <div className="mt-3 grid grid-cols-1 gap-3 sm:grid-cols-2">
        <BreakdownTable title="按 step 分解" rows={u.byStep} />
        <BreakdownTable title="按 provider 分解" rows={u.byProvider} />
      </div>
    </section>
  );
}

function BreakdownTable({ title, rows }: { title: string; rows: AdminUsageBreakdown[] }) {
  return (
    <div className="overflow-hidden rounded-xl ring-1 ring-ink-100">
      <div className="border-b border-ink-100 bg-cream-100 px-3 py-2 text-xs font-medium uppercase tracking-wider text-ink-500">
        {title}
      </div>
      {rows.length === 0 ? (
        <p className="px-3 py-4 text-center text-xs text-ink-400">无数据</p>
      ) : (
        <table className="w-full text-sm">
          <tbody className="divide-y divide-ink-100 bg-white">
            {rows.map((r) => (
              <tr key={r.key}>
                <td className="px-3 py-2 text-xs text-ink-700">{r.key}</td>
                <td className="px-3 py-2 text-right font-mono text-xs text-ink-900">
                  {formatCents(r.costCents)}
                </td>
                <td className="px-3 py-2 text-right text-[10px] text-ink-400">
                  {r.calls.toLocaleString()}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function WalletSection({ data }: { data: AdminUserDetailResponse }) {
  return (
    <section>
      <h3 className="text-sm font-semibold text-ink-700">钱包</h3>
      <div className="mt-2 rounded-xl bg-cream-50 px-4 py-3">
        <p className="text-xs text-ink-500">余额</p>
        <p className="mt-1 text-2xl font-semibold text-ink-900">
          {formatCents(data.walletBalanceCents)}
        </p>
        <p className="mt-1 text-[10px] text-ink-400">
          钱包流水视图将在 v2 接入（wallet_ledger_entries 当前为空）
        </p>
      </div>
    </section>
  );
}
