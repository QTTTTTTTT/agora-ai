// FundingSection (P1-2)
//
// Self-service deposit/withdrawal panel for the fund owner.
// Embedded in FundSettings, default-collapsed (the workflow is
// rare for simulation funds; expanded automatically when there's
// a pending request to remind the owner).
//
// What the panel does:
//   - lists existing requests (any status) with badges
//   - lets the owner submit a new request (direction / amount /
//     method / external_reference / notes)
//   - allows the owner to cancel their own pending request
//
// What it does NOT do:
//   - approve / reject — that's admin-only and lives on the
//     Admin page (AdminFundingSection)

import { useEffect, useMemo, useState } from "react";
import {
  ApiError,
  cancelFundingRequest,
  createFundingRequest,
  formatApiError,
  listFundingRequests,
  type FundingDirection,
  type FundingMethod,
  type FundingRequestRow,
  type FundingStatus,
} from "../lib/api";
import { messages, type LocaleId } from "@fundai/api-client";

interface FundingSectionProps {
  fundId: string;
  language: LocaleId;
  /** Default expanded if the parent knows the panel matters
   *  (live mode, prior pending request, etc.). */
  defaultExpanded?: boolean;
}

const methodOptions: FundingMethod[] = [
  "wire",
  "ach",
  "sepa",
  "check",
  "internal_transfer",
  "manual",
];

const statusBadgeClass: Record<FundingStatus, string> = {
  pending: "bg-amber-100 text-amber-800",
  approved: "bg-emerald-100 text-emerald-800",
  rejected: "bg-red-100 text-red-700",
  cancelled: "bg-gray-100 text-gray-700",
  posted: "bg-indigo-100 text-indigo-800",
};

export default function FundingSection({
  fundId,
  language,
  defaultExpanded = false,
}: FundingSectionProps): JSX.Element {
  const t = messages[language].funding;
  const [expanded, setExpanded] = useState(defaultExpanded);
  const [rows, setRows] = useState<FundingRequestRow[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const [cancelling, setCancelling] = useState<string | null>(null);

  const [direction, setDirection] = useState<FundingDirection>("deposit");
  const [amount, setAmount] = useState<string>("");
  const [currency, setCurrency] = useState<string>("USD");
  const [method, setMethod] = useState<FundingMethod>("wire");
  const [externalReference, setExternalReference] = useState<string>("");
  const [notes, setNotes] = useState<string>("");

  const reload = async () => {
    if (!fundId) return;
    setLoading(true);
    setError(null);
    try {
      const list = await listFundingRequests(fundId, { limit: 50 });
      setRows(list);
      // Auto-expand if there's a pending request — visibility
      // matters more than UI quietness in that case.
      if (!expanded && list.some((r) => r.status === "pending")) {
        setExpanded(true);
      }
    } catch (err) {
      if (err instanceof ApiError && (err.status === 403 || err.status === 404)) {
        setRows([]);
        return;
      }
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setLoading(false);
    }
  };

  useEffect(() => {
    void reload();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [fundId]);

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!fundId) return;
    const amt = Number(amount);
    if (!Number.isFinite(amt) || amt <= 0) {
      setError(t.errorPrefix + t.formAmount);
      return;
    }
    setSubmitting(true);
    setError(null);
    try {
      await createFundingRequest(fundId, {
        direction,
        amount: amt,
        currency: currency.trim().toUpperCase() || "USD",
        method,
        externalReference: externalReference.trim() || undefined,
        notes: notes.trim() || undefined,
      });
      setAmount("");
      setExternalReference("");
      setNotes("");
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setSubmitting(false);
    }
  };

  const handleCancel = async (id: string) => {
    if (!fundId) return;
    if (typeof window !== "undefined" && !window.confirm(t.confirmCancel)) {
      return;
    }
    setCancelling(id);
    setError(null);
    try {
      await cancelFundingRequest(fundId, id);
      await reload();
    } catch (err) {
      setError(formatApiError(err, t.errorPrefix));
    } finally {
      setCancelling(null);
    }
  };

  const sortedRows = useMemo(
    () =>
      [...rows].sort((a, b) =>
        a.createdAt < b.createdAt ? 1 : a.createdAt > b.createdAt ? -1 : 0,
      ),
    [rows],
  );

  const methodLabel = (m: FundingMethod) => {
    switch (m) {
      case "wire":
        return t.methodWire;
      case "ach":
        return t.methodACH;
      case "sepa":
        return t.methodSEPA;
      case "check":
        return t.methodCheck;
      case "internal_transfer":
        return t.methodInternal;
      case "manual":
        return t.methodManual;
      default:
        return m;
    }
  };

  const statusLabel = (s: FundingStatus) => {
    switch (s) {
      case "pending":
        return t.statusPending;
      case "approved":
        return t.statusApproved;
      case "rejected":
        return t.statusRejected;
      case "cancelled":
        return t.statusCancelled;
      case "posted":
        return t.statusPosted;
      default:
        return s;
    }
  };

  return (
    <section className="rounded-2xl border border-gray-200 bg-white shadow-sm">
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="flex w-full items-center justify-between gap-3 px-6 py-4 text-left"
        aria-expanded={expanded}
      >
        <div>
          <h2 className="text-lg font-semibold text-gray-900">{t.title}</h2>
          <p className="mt-1 text-sm text-gray-500">{t.subtitle}</p>
        </div>
        <span className="text-sm text-indigo-600">{expanded ? "−" : "+"}</span>
      </button>

      {expanded ? (
        <div className="space-y-5 border-t border-gray-100 px-6 py-5">
          {error ? (
            <div className="rounded-xl border border-red-200 bg-red-50 px-4 py-2 text-sm text-red-700">
              {error}
            </div>
          ) : null}

          <form onSubmit={handleSubmit} className="space-y-3">
            <h3 className="text-sm font-semibold text-gray-700">{t.formTitle}</h3>
            <div className="grid grid-cols-1 gap-3 md:grid-cols-3">
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{t.formDirection}</span>
                <select
                  value={direction}
                  onChange={(e) => setDirection(e.target.value as FundingDirection)}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                >
                  <option value="deposit">{t.formDirectionDeposit}</option>
                  <option value="withdrawal">{t.formDirectionWithdrawal}</option>
                </select>
              </label>
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{t.formAmount}</span>
                <input
                  type="number"
                  min="0"
                  step="0.01"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                  placeholder={t.formAmountPlaceholder}
                  required
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                />
              </label>
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{t.formCurrency}</span>
                <input
                  type="text"
                  value={currency}
                  onChange={(e) => setCurrency(e.target.value)}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                  maxLength={8}
                />
              </label>
              <label className="text-sm text-gray-600">
                <span className="mb-1 block">{t.formMethod}</span>
                <select
                  value={method}
                  onChange={(e) => setMethod(e.target.value as FundingMethod)}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                >
                  {methodOptions.map((m) => (
                    <option key={m} value={m}>
                      {methodLabel(m)}
                    </option>
                  ))}
                </select>
              </label>
              <label className="text-sm text-gray-600 md:col-span-2">
                <span className="mb-1 block">{t.formExternalReference}</span>
                <input
                  type="text"
                  value={externalReference}
                  onChange={(e) => setExternalReference(e.target.value)}
                  placeholder={t.formExternalReferencePlaceholder}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                />
              </label>
              <label className="text-sm text-gray-600 md:col-span-3">
                <span className="mb-1 block">{t.formNotes}</span>
                <textarea
                  value={notes}
                  onChange={(e) => setNotes(e.target.value)}
                  placeholder={t.formNotesPlaceholder}
                  rows={2}
                  className="w-full rounded-lg border border-gray-300 bg-white px-3 py-2 text-sm text-gray-700 outline-none focus:border-indigo-500"
                />
              </label>
            </div>
            <p className="text-xs text-gray-500">{t.formNote}</p>
            <button
              type="submit"
              disabled={submitting}
              className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {submitting ? t.formSubmitting : t.formSubmit}
            </button>
          </form>

          <div>
            <div className="mb-2 flex items-center justify-between">
              <h3 className="text-sm font-semibold text-gray-700">
                {language === "zh-CN" ? "申请记录" : "Request history"}
              </h3>
              <button
                type="button"
                onClick={() => void reload()}
                className="text-xs text-indigo-600 hover:text-indigo-800"
              >
                {t.refresh}
              </button>
            </div>
            {loading ? (
              <p className="text-sm text-gray-500">{t.loading}</p>
            ) : sortedRows.length === 0 ? (
              <p className="text-sm text-gray-500">{t.empty}</p>
            ) : (
              <ul className="divide-y divide-gray-100">
                {sortedRows.map((row) => (
                  <li key={row.id} className="py-3">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="flex items-center gap-2">
                          <span
                            className={`rounded-full px-2 py-0.5 text-xs font-medium ${statusBadgeClass[row.status] ?? "bg-gray-100 text-gray-700"}`}
                          >
                            {statusLabel(row.status)}
                          </span>
                          <span className="text-sm font-medium text-gray-900">
                            {row.direction === "deposit"
                              ? t.formDirectionDeposit
                              : t.formDirectionWithdrawal}
                          </span>
                          <span className="text-sm font-mono text-gray-700">
                            {row.amount.toLocaleString()} {row.currency}
                          </span>
                          <span className="text-xs text-gray-500">
                            {methodLabel(row.method as FundingMethod)}
                          </span>
                        </div>
                        {row.externalReference ? (
                          <p className="mt-1 text-xs text-gray-500">
                            ref: {row.externalReference}
                          </p>
                        ) : null}
                        {row.notes ? (
                          <p className="mt-1 text-xs text-gray-600">{row.notes}</p>
                        ) : null}
                        {row.status === "rejected" && row.rejectionReason ? (
                          <p className="mt-1 text-xs text-red-700">
                            {t.rejectionReasonLabel}: {row.rejectionReason}
                          </p>
                        ) : null}
                        {row.status === "pending" ? (
                          <p className="mt-1 text-xs text-amber-700">
                            {t.awaitingApproval}
                          </p>
                        ) : null}
                        <p className="mt-1 text-xs text-gray-400">
                          {new Date(row.createdAt).toLocaleString()}
                        </p>
                      </div>
                      {row.status === "pending" ? (
                        <button
                          type="button"
                          onClick={() => void handleCancel(row.id)}
                          disabled={cancelling === row.id}
                          className="shrink-0 rounded-lg border border-gray-300 bg-white px-3 py-1.5 text-xs font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
                        >
                          {cancelling === row.id ? t.cancelling : t.cancel}
                        </button>
                      ) : null}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </div>
        </div>
      ) : null}
    </section>
  );
}
