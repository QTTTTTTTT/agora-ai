// AdminBrokerLinksSection — P1-6 web admin component.
//
// Privileged surface that pairs with the user-side
// BrokerLinksSection. Lets a super_admin:
//
//   - filter by status (pending / active / all)
//   - approve a pending row (4-eye check enforced server-side —
//     the API responds 403 with error="four_eye_violation" if
//     the approver matches the requester)
//   - reject a pending row with a reason
//
// Self-contained: owns its own data fetch + state. Slotted into
// the existing Admin page with a single import + render.

import React, { useCallback, useEffect, useMemo, useState } from "react";
import { ApiError, apiGet, apiPost, formatApiError } from "../lib/api";

type Language = "zh-CN" | "en-US";

interface AdminBrokerLinkRow {
  id: string;
  fundId: string;
  userId: string;
  brokerId: string;
  accountId: string;
  status: "pending" | "active" | "suspended" | "revoked";
  approvedBy?: string;
  approvedAt?: string;
  createdAt: string;
  updatedAt: string;
}

interface ListResponse {
  links: AdminBrokerLinkRow[];
  status: string;
  row_count: number;
}

type StatusFilter = "pending" | "active" | "all";

const messages: Record<Language, {
  title: string;
  hint: string;
  empty: string;
  loading: string;
  refresh: string;
  refreshing: string;
  filterPending: string;
  filterActive: string;
  filterAll: string;
  approve: string;
  approving: string;
  reject: string;
  rejecting: string;
  reasonPlaceholder: string;
  reasonRequired: string;
  fourEyeBlocked: string;
  fundLabel: string;
  requesterLabel: string;
  brokerLabel: string;
  accountLabel: string;
  createdLabel: string;
  noteLabel: string;
  notePlaceholder: string;
  errorPrefix: string;
}> = {
  "zh-CN": {
    title: "券商绑定 4-eye 审批",
    hint: "审批用户提交的券商绑定申请。请确认 KYC 已通过、文件齐备后再批准；批准人不能与申请人相同。",
    empty: "暂无符合条件的记录",
    loading: "加载中…",
    refresh: "刷新",
    refreshing: "刷新中…",
    filterPending: "待审批",
    filterActive: "已生效",
    filterAll: "全部",
    approve: "批准",
    approving: "处理中…",
    reject: "拒绝",
    rejecting: "处理中…",
    reasonPlaceholder: "拒绝理由（必填）",
    reasonRequired: "请填写拒绝理由",
    fourEyeBlocked: "审批人不能等于申请人（4-eye）",
    fundLabel: "基金",
    requesterLabel: "申请人",
    brokerLabel: "券商",
    accountLabel: "账号",
    createdLabel: "提交时间",
    noteLabel: "审批备注（可选）",
    notePlaceholder: "工单号 / 通话记录…",
    errorPrefix: "操作失败：",
  },
  "en-US": {
    title: "Broker-link 4-eye approvals",
    hint:
      "Review broker-link requests submitted by fund owners. Verify KYC has been completed and the documents are on file before approving. Approver MUST differ from the requester.",
    empty: "No matching rows",
    loading: "Loading…",
    refresh: "Refresh",
    refreshing: "Refreshing…",
    filterPending: "Pending",
    filterActive: "Active",
    filterAll: "All",
    approve: "Approve",
    approving: "Working…",
    reject: "Reject",
    rejecting: "Working…",
    reasonPlaceholder: "Rejection reason (required)",
    reasonRequired: "Please supply a rejection reason",
    fourEyeBlocked: "Approver must differ from requester (4-eye)",
    fundLabel: "Fund",
    requesterLabel: "Requester",
    brokerLabel: "Broker",
    accountLabel: "Account",
    createdLabel: "Submitted",
    noteLabel: "Approval note (optional)",
    notePlaceholder: "Ticket # / call notes…",
    errorPrefix: "Action failed: ",
  },
};

interface Props {
  language: Language;
}

const AdminBrokerLinksSection: React.FC<Props> = ({ language }) => {
  const m = messages[language];
  const [filter, setFilter] = useState<StatusFilter>("pending");
  const [rows, setRows] = useState<AdminBrokerLinkRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [actingId, setActingId] = useState<string | null>(null);
  const [reasons, setReasons] = useState<Record<string, string>>({});
  const [notes, setNotes] = useState<Record<string, string>>({});
  const [refreshKey, setRefreshKey] = useState(0);

  const refresh = useCallback(() => setRefreshKey((k) => k + 1), []);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);
    const params = filter === "all" ? "?status=all" : `?status=${filter}`;
    apiGet<ListResponse>(`/api/admin/broker-links${params}`)
      .then((resp) => {
        if (!cancelled) setRows(resp.links ?? []);
      })
      .catch((err: unknown) => {
        if (cancelled) return;
        setError(formatApiError(err, m.errorPrefix));
      })
      .finally(() => {
        if (!cancelled) setLoading(false);
      });
    return () => {
      cancelled = true;
    };
  }, [filter, refreshKey, m.errorPrefix]);

  const onApprove = async (row: AdminBrokerLinkRow) => {
    setActingId(row.id);
    setError(null);
    try {
      await apiPost(`/api/admin/broker-links/${encodeURIComponent(row.id)}/approve`, {
        note: (notes[row.id] ?? "").trim() || undefined,
      });
      refresh();
    } catch (err) {
      if (err instanceof ApiError && err.status === 403 && err.message.includes("four_eye_violation")) {
        // 4-eye violation gets a dedicated message — much friendlier than
        // the raw JSON from the backend.
        setError(m.fourEyeBlocked);
      } else {
        setError(formatApiError(err, m.errorPrefix));
      }
    } finally {
      setActingId(null);
    }
  };

  const onReject = async (row: AdminBrokerLinkRow) => {
    const reason = (reasons[row.id] ?? "").trim();
    if (!reason) {
      setError(m.reasonRequired);
      return;
    }
    setActingId(row.id);
    setError(null);
    try {
      await apiPost(`/api/admin/broker-links/${encodeURIComponent(row.id)}/reject`, { reason });
      refresh();
    } catch (err) {
      if (err instanceof ApiError && err.status === 403 && err.message.includes("four_eye_violation")) {
        setError(m.fourEyeBlocked);
      } else {
        setError(formatApiError(err, m.errorPrefix));
      }
    } finally {
      setActingId(null);
    }
  };

  // Sort: pending first (action-required), then by createdAt desc.
  // We don't paginate yet — backend caps at 200 rows, which is
  // ample for the realistic broker-link request volume.
  const sorted = useMemo(() => {
    return [...rows].sort((a, b) => {
      const oa = a.status === "pending" ? 0 : 1;
      const ob = b.status === "pending" ? 0 : 1;
      if (oa !== ob) return oa - ob;
      return b.createdAt.localeCompare(a.createdAt);
    });
  }, [rows]);

  return (
    <section className="rounded-2xl border border-violet-200 bg-violet-50 px-5 py-5 shadow-sm">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 className="text-lg font-semibold text-violet-900">{m.title}</h2>
          <p className="mt-2 max-w-3xl text-sm text-violet-700">{m.hint}</p>
        </div>
        <button
          type="button"
          onClick={refresh}
          disabled={loading}
          className="rounded-xl bg-violet-600 px-3 py-2 text-xs font-medium text-white hover:bg-violet-700 disabled:cursor-not-allowed disabled:opacity-60"
        >
          {loading ? m.refreshing : m.refresh}
        </button>
      </div>

      <div className="mt-4 flex flex-wrap gap-2">
        {(
          [
            ["pending", m.filterPending],
            ["active", m.filterActive],
            ["all", m.filterAll],
          ] as Array<[StatusFilter, string]>
        ).map(([s, label]) => (
          <button
            key={s}
            type="button"
            onClick={() => setFilter(s)}
            className={`rounded-full px-3 py-1.5 text-xs font-semibold ring-1 transition ${
              filter === s
                ? "bg-violet-600 text-white ring-violet-600"
                : "bg-white text-violet-700 ring-violet-200 hover:bg-violet-100"
            }`}
          >
            {label}
          </button>
        ))}
      </div>

      {error ? <p className="mt-3 text-sm text-red-600">{error}</p> : null}

      <div className="mt-4 space-y-3">
        {loading ? (
          <p className="text-sm text-violet-700">{m.loading}</p>
        ) : sorted.length === 0 ? (
          <div className="rounded-xl border border-dashed border-violet-200 bg-white/70 p-4 text-sm text-violet-700">
            {m.empty}
          </div>
        ) : (
          sorted.map((row) => {
            const isActing = actingId === row.id;
            return (
              <article key={row.id} className="rounded-2xl border border-violet-200 bg-white px-4 py-4 shadow-sm">
                <div className="flex items-start justify-between gap-3">
                  <div className="min-w-0">
                    <p className="font-mono text-xs text-gray-400">{row.id}</p>
                    <p className="mt-1 text-sm text-gray-700">
                      <span className="font-semibold">{m.brokerLabel}:</span> {row.brokerId.toUpperCase()}{" "}
                      <span className="ml-2 font-semibold">{m.accountLabel}:</span>{" "}
                      <span className="font-mono">{row.accountId}</span>
                    </p>
                    <p className="mt-1 text-xs text-gray-500">
                      <span className="font-medium">{m.fundLabel}:</span> {row.fundId} ·{" "}
                      <span className="font-medium">{m.requesterLabel}:</span> {row.userId}
                    </p>
                    <p className="mt-1 text-xs text-gray-400">
                      {m.createdLabel}: {formatTs(row.createdAt)}
                    </p>
                  </div>
                  <span className={`whitespace-nowrap rounded-full px-2.5 py-1 text-[11px] font-medium ${tone(row.status)}`}>
                    {row.status}
                  </span>
                </div>

                {row.status === "pending" ? (
                  <div className="mt-3 space-y-2">
                    <label className="block text-xs text-gray-600">
                      {m.noteLabel}
                      <input
                        value={notes[row.id] ?? ""}
                        onChange={(e) =>
                          setNotes((cur) => ({ ...cur, [row.id]: e.target.value }))
                        }
                        placeholder={m.notePlaceholder}
                        className="mt-1 w-full rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-sm text-gray-700 outline-none focus:border-violet-500"
                      />
                    </label>
                    <label className="block text-xs text-gray-600">
                      <input
                        value={reasons[row.id] ?? ""}
                        onChange={(e) =>
                          setReasons((cur) => ({ ...cur, [row.id]: e.target.value }))
                        }
                        placeholder={m.reasonPlaceholder}
                        className="w-full rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-sm text-gray-700 outline-none focus:border-rose-500"
                      />
                    </label>
                    <div className="flex flex-wrap gap-2">
                      <button
                        type="button"
                        onClick={() => void onApprove(row)}
                        disabled={isActing}
                        className="rounded-xl bg-emerald-600 px-3 py-2 text-xs font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
                      >
                        {isActing ? m.approving : m.approve}
                      </button>
                      <button
                        type="button"
                        onClick={() => void onReject(row)}
                        disabled={isActing}
                        className="rounded-xl bg-rose-600 px-3 py-2 text-xs font-medium text-white hover:bg-rose-700 disabled:cursor-not-allowed disabled:opacity-60"
                      >
                        {isActing ? m.rejecting : m.reject}
                      </button>
                    </div>
                  </div>
                ) : null}
              </article>
            );
          })
        )}
      </div>
    </section>
  );
};

function tone(s: AdminBrokerLinkRow["status"]): string {
  switch (s) {
    case "active":
      return "bg-emerald-100 text-emerald-800";
    case "pending":
      return "bg-amber-100 text-amber-800";
    case "suspended":
      return "bg-orange-100 text-orange-800";
    default:
      return "bg-gray-100 text-gray-600";
  }
}

function formatTs(iso: string): string {
  if (!iso) return "—";
  try {
    return new Date(iso).toLocaleString();
  } catch {
    return iso;
  }
}

export default AdminBrokerLinksSection;
