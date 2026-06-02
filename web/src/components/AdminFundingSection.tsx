// AdminFundingSection — P1-2 web admin component.
//
// Privileged surface that pairs with the user-side FundingSection.
// Lets a super_admin:
//
//   - filter by status (pending / approved / rejected / all)
//   - approve a pending request — the server tx-co-commits a
//     cash_ledger row + funds.current_capital UPDATE + flip to
//     'approved'. 4-eye is enforced server-side; the UI surfaces
//     a clear "ask another admin" message on 403 four_eye_violation
//   - reject a pending request with a reason
//   - see "insufficient cash" rejections for over-withdrawal
//
// Self-contained: owns its own data fetch + state.

import { useCallback, useEffect, useState } from "react";
import {
  ApiError,
  approveFundingRequest,
  formatApiError,
  listAdminFundingRequests,
  rejectFundingRequest,
  type FundingRequestRow,
} from "../lib/api";

type Language = "zh-CN" | "en-US";

type StatusFilter = "pending" | "approved" | "all";

const messages: Record<
  Language,
  {
    title: string;
    hint: string;
    empty: string;
    loading: string;
    refresh: string;
    refreshing: string;
    filterPending: string;
    filterApproved: string;
    filterAll: string;
    approve: string;
    approving: string;
    reject: string;
    rejecting: string;
    reasonPlaceholder: string;
    reasonRequired: string;
    fourEyeBlocked: string;
    insufficientCash: string;
    fundLabel: string;
    requesterLabel: string;
    methodLabel: string;
    referenceLabel: string;
    createdLabel: string;
    notesLabel: string;
    rejectionReasonLabel: string;
    noteLabel: string;
    notePlaceholder: string;
    errorPrefix: string;
    statusBadges: Record<string, string>;
  }
> = {
  "zh-CN": {
    title: "出入金 4-eye 审批",
    hint: "审批用户的出入金请求。批准会同步把现金记入 cash_ledger 并更新 funds.current_capital；拒绝必须填写理由。审批人必须不同于申请人。",
    empty: "暂无符合条件的记录",
    loading: "加载中…",
    refresh: "刷新",
    refreshing: "刷新中…",
    filterPending: "待审批",
    filterApproved: "已通过",
    filterAll: "全部",
    approve: "批准",
    approving: "处理中…",
    reject: "拒绝",
    rejecting: "处理中…",
    reasonPlaceholder: "拒绝理由（必填）",
    reasonRequired: "请填写拒绝理由",
    fourEyeBlocked: "审批人不能等于申请人（4-eye）",
    insufficientCash: "余额不足，出金已被拒绝",
    fundLabel: "基金",
    requesterLabel: "申请人",
    methodLabel: "渠道",
    referenceLabel: "外部凭证",
    createdLabel: "提交时间",
    notesLabel: "申请备注",
    rejectionReasonLabel: "拒绝原因",
    noteLabel: "审批备注（可选）",
    notePlaceholder: "工单号 / 凭证号 / 备忘…",
    errorPrefix: "操作失败：",
    statusBadges: {
      pending: "待审批",
      approved: "已通过",
      rejected: "已拒绝",
      cancelled: "已撤回",
      posted: "已落账",
    },
  },
  "en-US": {
    title: "Funding 4-eye approvals",
    hint:
      "Review fund owners' deposit/withdrawal requests. Approving co-commits a cash_ledger row + funds.current_capital update; rejection requires a reason. The approver must differ from the requester.",
    empty: "No matching rows",
    loading: "Loading…",
    refresh: "Refresh",
    refreshing: "Refreshing…",
    filterPending: "Pending",
    filterApproved: "Approved",
    filterAll: "All",
    approve: "Approve",
    approving: "Approving…",
    reject: "Reject",
    rejecting: "Rejecting…",
    reasonPlaceholder: "Rejection reason (required)",
    reasonRequired: "Please enter a rejection reason",
    fourEyeBlocked: "Approver must differ from the requester (4-eye)",
    insufficientCash: "Insufficient cash — withdrawal rejected",
    fundLabel: "Fund",
    requesterLabel: "Requester",
    methodLabel: "Method",
    referenceLabel: "External ref",
    createdLabel: "Submitted",
    notesLabel: "Request notes",
    rejectionReasonLabel: "Reason",
    noteLabel: "Approver note (optional)",
    notePlaceholder: "Ticket / wire ref / memo…",
    errorPrefix: "Action failed: ",
    statusBadges: {
      pending: "Pending",
      approved: "Approved",
      rejected: "Rejected",
      cancelled: "Cancelled",
      posted: "Posted",
    },
  },
};

interface Props {
  language: Language;
}

export default function AdminFundingSection({ language }: Props): JSX.Element {
  const m = messages[language];
  const [filter, setFilter] = useState<StatusFilter>("pending");
  const [rows, setRows] = useState<FundingRequestRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [pendingAction, setPendingAction] = useState<{
    id: string;
    kind: "approve" | "reject";
  } | null>(null);
  const [rejectReason, setRejectReason] = useState<Record<string, string>>({});
  const [approveNote, setApproveNote] = useState<Record<string, string>>({});

  const reload = useCallback(async () => {
    setLoading(true);
    setError(null);
    try {
      const resp = await listAdminFundingRequests(filter);
      setRows(resp.requests);
    } catch (err) {
      setError(formatApiError(err, m.errorPrefix));
    } finally {
      setLoading(false);
    }
  }, [filter, m.errorPrefix]);

  useEffect(() => {
    void reload();
  }, [reload]);

  const handleApprove = async (id: string) => {
    setPendingAction({ id, kind: "approve" });
    setError(null);
    try {
      await approveFundingRequest(id, approveNote[id] ?? "");
      setApproveNote((prev) => ({ ...prev, [id]: "" }));
      await reload();
    } catch (err) {
      setError(extractFundingError(err, m));
    } finally {
      setPendingAction(null);
    }
  };

  const handleReject = async (id: string) => {
    const reason = (rejectReason[id] ?? "").trim();
    if (!reason) {
      setError(m.reasonRequired);
      return;
    }
    setPendingAction({ id, kind: "reject" });
    setError(null);
    try {
      await rejectFundingRequest(id, reason);
      setRejectReason((prev) => ({ ...prev, [id]: "" }));
      await reload();
    } catch (err) {
      setError(extractFundingError(err, m));
    } finally {
      setPendingAction(null);
    }
  };

  return (
    <section className="rounded-2xl border border-gray-200 bg-white shadow-sm">
      <header className="flex flex-col gap-2 border-b border-gray-100 px-6 py-4 md:flex-row md:items-center md:justify-between">
        <div>
          <h2 className="text-lg font-semibold text-gray-900">{m.title}</h2>
          <p className="mt-1 text-sm text-gray-500">{m.hint}</p>
        </div>
        <div className="flex items-center gap-2">
          <select
            value={filter}
            onChange={(e) => setFilter(e.target.value as StatusFilter)}
            className="rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
          >
            <option value="pending">{m.filterPending}</option>
            <option value="approved">{m.filterApproved}</option>
            <option value="all">{m.filterAll}</option>
          </select>
          <button
            type="button"
            onClick={() => void reload()}
            disabled={loading}
            className="rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {loading ? m.refreshing : m.refresh}
          </button>
        </div>
      </header>

      <div className="px-6 py-4">
        {error ? (
          <div className="mb-3 rounded-xl border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700">
            {error}
          </div>
        ) : null}

        {loading ? (
          <p className="text-sm text-gray-500">{m.loading}</p>
        ) : rows.length === 0 ? (
          <p className="text-sm text-gray-500">{m.empty}</p>
        ) : (
          <ul className="divide-y divide-gray-100">
            {rows.map((row) => (
              <li key={row.id} className="py-3">
                <div className="flex flex-wrap items-start justify-between gap-3">
                  <div className="min-w-0 flex-1">
                    <div className="flex flex-wrap items-center gap-2">
                      <span
                        className={`rounded-full px-2 py-0.5 text-xs font-medium ${
                          statusBadgeClass[row.status] ?? "bg-gray-100 text-gray-700"
                        }`}
                      >
                        {m.statusBadges[row.status] ?? row.status}
                      </span>
                      <span className="text-sm font-medium text-gray-900">
                        {row.direction === "deposit"
                          ? language === "zh-CN"
                            ? "入金"
                            : "Deposit"
                          : language === "zh-CN"
                            ? "出金"
                            : "Withdrawal"}
                      </span>
                      <span className="font-mono text-sm text-gray-700">
                        {row.amount.toLocaleString()} {row.currency}
                      </span>
                    </div>
                    <dl className="mt-2 grid grid-cols-1 gap-y-0.5 text-xs text-gray-600 md:grid-cols-2">
                      <div>
                        <span className="text-gray-400">{m.fundLabel}: </span>
                        <span className="font-mono">{row.fundId.slice(0, 8)}…</span>
                      </div>
                      <div>
                        <span className="text-gray-400">{m.requesterLabel}: </span>
                        <span className="font-mono">{row.requestedBy.slice(0, 8)}…</span>
                      </div>
                      <div>
                        <span className="text-gray-400">{m.methodLabel}: </span>
                        <span>{row.method}</span>
                      </div>
                      {row.externalReference ? (
                        <div>
                          <span className="text-gray-400">{m.referenceLabel}: </span>
                          <span>{row.externalReference}</span>
                        </div>
                      ) : null}
                      <div>
                        <span className="text-gray-400">{m.createdLabel}: </span>
                        <span>{new Date(row.createdAt).toLocaleString()}</span>
                      </div>
                    </dl>
                    {row.notes ? (
                      <p className="mt-1 text-xs text-gray-700">
                        <span className="text-gray-400">{m.notesLabel}: </span>
                        {row.notes}
                      </p>
                    ) : null}
                    {row.status === "rejected" && row.rejectionReason ? (
                      <p className="mt-1 text-xs text-red-700">
                        <span className="text-gray-400">{m.rejectionReasonLabel}: </span>
                        {row.rejectionReason}
                      </p>
                    ) : null}
                  </div>

                  {row.status === "pending" ? (
                    <div className="flex flex-col gap-2 lg:w-[28rem]">
                      <input
                        type="text"
                        value={approveNote[row.id] ?? ""}
                        onChange={(e) =>
                          setApproveNote((prev) => ({ ...prev, [row.id]: e.target.value }))
                        }
                        placeholder={m.notePlaceholder}
                        className="w-full rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-xs text-gray-700 outline-none focus:border-indigo-500"
                      />
                      <input
                        type="text"
                        value={rejectReason[row.id] ?? ""}
                        onChange={(e) =>
                          setRejectReason((prev) => ({ ...prev, [row.id]: e.target.value }))
                        }
                        placeholder={m.reasonPlaceholder}
                        className="w-full rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-xs text-gray-700 outline-none focus:border-indigo-500"
                      />
                      <div className="flex gap-2">
                        <button
                          type="button"
                          onClick={() => void handleApprove(row.id)}
                          disabled={pendingAction?.id === row.id}
                          className="flex-1 rounded-lg bg-emerald-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {pendingAction?.id === row.id && pendingAction.kind === "approve"
                            ? m.approving
                            : m.approve}
                        </button>
                        <button
                          type="button"
                          onClick={() => void handleReject(row.id)}
                          disabled={pendingAction?.id === row.id}
                          className="flex-1 rounded-lg border border-red-300 bg-white px-3 py-1.5 text-xs font-medium text-red-700 hover:bg-red-50 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {pendingAction?.id === row.id && pendingAction.kind === "reject"
                            ? m.rejecting
                            : m.reject}
                        </button>
                      </div>
                    </div>
                  ) : null}
                </div>
              </li>
            ))}
          </ul>
        )}
      </div>
    </section>
  );
}

const statusBadgeClass: Record<string, string> = {
  pending: "bg-amber-100 text-amber-800",
  approved: "bg-emerald-100 text-emerald-800",
  rejected: "bg-red-100 text-red-700",
  cancelled: "bg-gray-100 text-gray-700",
  posted: "bg-indigo-100 text-indigo-800",
};

// extractFundingError translates the server's error envelope
// into the operator-friendly UI string. We special-case the two
// well-known business errors (4-eye, insufficient_cash) so the
// admin gets actionable copy instead of a generic "operation
// failed".
function extractFundingError(err: unknown, m: typeof messages["zh-CN"]): string {
  if (err instanceof ApiError) {
    const code = (err.payload as { error?: string } | undefined)?.error;
    if (code === "four_eye_violation") return m.fourEyeBlocked;
    if (code === "insufficient_cash") return m.insufficientCash;
  }
  return formatApiError(err, m.errorPrefix);
}
