// AdminUsersStatsCards — top-of-page KPI strip for the admin user
// console. Four cards (total / active 7d / new this week / MRR) plus
// a 30-day signup sparkline. The data shape is the wire response
// from GET /api/admin/users/stats; rendering is deliberately
// self-contained (no shared "MetricCard" component yet because the
// rest of the admin console doesn't have a unified primitive — when
// it does this should fold into it).
//
// Sparkline uses recharts AreaChart sized at h-12 so it reads as
// "trend at a glance" rather than a real chart with axes.

import React, { useMemo } from "react";
import { Area, AreaChart, ResponsiveContainer, Tooltip } from "recharts";
import type { AdminUsersStatsResponse } from "../lib/api";

interface AdminUsersStatsCardsProps {
  stats: AdminUsersStatsResponse | null;
  loading: boolean;
  error: string | null;
  onRetry: () => void;
}

const tierLabels: Record<string, string> = {
  free: "免费",
  pro: "Pro",
  premium: "Premium",
  enterprise: "Enterprise",
};

function formatCents(cents: number): string {
  if (!Number.isFinite(cents)) return "—";
  return `$${(cents / 100).toLocaleString(undefined, {
    minimumFractionDigits: 2,
    maximumFractionDigits: 2,
  })}`;
}

export default function AdminUsersStatsCards(props: AdminUsersStatsCardsProps) {
  const { stats, loading, error, onRetry } = props;

  // Derive "new this week" from the last 7 entries of newUsers30d so
  // the card and the sparkline share one source of truth — the
  // server already zero-fills missing days, so the slice math is
  // straightforward.
  const newThisWeek = useMemo(() => {
    if (!stats?.newUsers30d) return 0;
    const last7 = stats.newUsers30d.slice(-7);
    return last7.reduce((acc, day) => acc + day.count, 0);
  }, [stats]);

  if (error) {
    return (
      <section className="rounded-2xl border border-rose-200 bg-rose-50 px-5 py-4">
        <p className="text-sm text-rose-700">{error}</p>
        <button
          type="button"
          onClick={onRetry}
          className="mt-2 rounded-full border border-rose-300 bg-white px-3 py-1 text-xs font-medium text-rose-700 hover:bg-rose-100"
        >
          重试
        </button>
      </section>
    );
  }

  return (
    <section className="grid grid-cols-1 gap-4 md:grid-cols-2 xl:grid-cols-4">
      <Card title="注册总数" value={loading ? "…" : String(stats?.totalUsers ?? 0)} subtitle="未注销账号" />
      <Card
        title="近 7 天活跃"
        value={loading ? "…" : String(stats?.activeUsers7d ?? 0)}
        subtitle="last_login_at < 7d"
      />
      <Card
        title="近 7 天新增"
        value={loading ? "…" : String(newThisWeek)}
        subtitle={loading ? "" : `日均 ≈ ${(newThisWeek / 7).toFixed(1)}`}
        chart={
          stats?.newUsers30d ? (
            <ResponsiveContainer width="100%" height={48}>
              <AreaChart data={stats.newUsers30d}>
                <defs>
                  <linearGradient id="adminUsersSignupFill" x1="0" y1="0" x2="0" y2="1">
                    <stop offset="0%" stopColor="#6366f1" stopOpacity={0.5} />
                    <stop offset="100%" stopColor="#6366f1" stopOpacity={0.05} />
                  </linearGradient>
                </defs>
                <Tooltip
                  cursor={false}
                  contentStyle={{
                    fontSize: 11,
                    padding: "4px 8px",
                    border: "1px solid #e5e7eb",
                    borderRadius: 6,
                  }}
                  formatter={(value: number) => [value, "新增"]}
                  labelFormatter={(label: string) => label}
                />
                <Area
                  type="monotone"
                  dataKey="count"
                  stroke="#6366f1"
                  strokeWidth={1.5}
                  fill="url(#adminUsersSignupFill)"
                  isAnimationActive={false}
                />
              </AreaChart>
            </ResponsiveContainer>
          ) : null
        }
      />
      <Card
        title="月度经常性收入"
        value={loading ? "…" : formatCents(stats?.mrrCents ?? 0)}
        subtitle={
          stats && stats.tierDistribution.length > 0
            ? stats.tierDistribution
                .map((t) => `${tierLabels[t.tier] ?? t.tier} ${t.count}`)
                .join(" · ")
            : "按当前 active 订阅 × 套餐月费"
        }
      />
    </section>
  );
}

interface CardProps {
  title: string;
  value: string;
  subtitle?: string;
  chart?: React.ReactNode;
}

function Card(props: CardProps) {
  return (
    <div className="flex flex-col rounded-2xl border border-ink-100 bg-white px-5 py-4 shadow-envelope">
      <div className="flex items-baseline justify-between gap-3">
        <span className="text-xs uppercase tracking-wider text-ink-500">{props.title}</span>
      </div>
      <div className="mt-2 text-2xl font-semibold text-ink-900">{props.value}</div>
      {props.subtitle ? <p className="mt-1 text-xs text-ink-500">{props.subtitle}</p> : null}
      {props.chart ? <div className="mt-3">{props.chart}</div> : null}
    </div>
  );
}
