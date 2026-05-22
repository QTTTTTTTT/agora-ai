import React, { useCallback, useState } from "react";
import { formatApiError } from "../../lib/api";

export interface ApprovalActionsLabels {
  approvalActions: string;
  approvePlan: string;
  rejectPlan: string;
  cancel: string;
  confirmReject: string;
  rejectReasonLabel: string;
  rejectReasonPlaceholder: string;
  refreshQuotePlan: string;
  approveError: string;
  rejectError: string;
  refreshQuoteError: string;
}

interface ApprovalActionsProps {
  labels: ApprovalActionsLabels;
  onApprove: () => Promise<void>;
  onReject: (reason: string) => Promise<void>;
  onRefreshQuote: () => Promise<void>;
  onError: (message: string) => void;
}

// ApprovalActions owns the transient "approval modal" state (reject reason,
// reject box visibility, in-flight submission) locally so typing in the
// reject reason textarea never re-renders the entire DecisionCenter tree.
//
// Before this extraction every keystroke triggered:
//   - setRejectReason(...) at the top of DecisionCenter
//   - DecisionCenter function body re-runs (~1500 lines)
//   - Even though memoised children skip their JSX, their props identity
//     still gets recomputed and React re-checks the entire tree.
//
// After:
//   - setRejectReason(...) only re-renders this 80-line component
//   - DecisionCenter sees stable props (onApprove/onReject/onRefreshQuote
//     are useCallback'd by the parent) and the React.memo wrapper skips
//     reconciliation entirely.
//
// State reset on plan switch is handled by the parent passing `key={planId}`
// so React fully unmounts and remounts the component, which is simpler than
// threading another callback through.
function ApprovalActionsInner({
  labels,
  onApprove,
  onReject,
  onRefreshQuote,
  onError,
}: ApprovalActionsProps) {
  const [showRejectBox, setShowRejectBox] = useState(false);
  const [rejectReason, setRejectReason] = useState("");
  const [submitting, setSubmitting] = useState(false);

  const handleApprove = useCallback(async () => {
    setSubmitting(true);
    try {
      await onApprove();
      setShowRejectBox(false);
      setRejectReason("");
    } catch (err) {
      onError(formatApiError(err, labels.approveError));
    } finally {
      setSubmitting(false);
    }
  }, [labels.approveError, onApprove, onError]);

  const handleReject = useCallback(async () => {
    const trimmed = rejectReason.trim();
    if (!trimmed) {
      return;
    }
    setSubmitting(true);
    try {
      await onReject(trimmed);
      setShowRejectBox(false);
      setRejectReason("");
    } catch (err) {
      onError(formatApiError(err, labels.rejectError));
    } finally {
      setSubmitting(false);
    }
  }, [labels.rejectError, onError, onReject, rejectReason]);

  const handleRefreshQuote = useCallback(async () => {
    setSubmitting(true);
    try {
      await onRefreshQuote();
    } catch (err) {
      onError(formatApiError(err, labels.refreshQuoteError));
    } finally {
      setSubmitting(false);
    }
  }, [labels.refreshQuoteError, onError, onRefreshQuote]);

  return (
    <div className="rounded-2xl border border-gray-200 bg-white p-6 shadow-sm">
      <h3 className="text-sm font-semibold uppercase tracking-wider text-gray-500">{labels.approvalActions}</h3>
      {showRejectBox ? (
        <div className="mt-4 space-y-4">
          <div>
            <label className="mb-2 block text-sm font-medium text-gray-700">{labels.rejectReasonLabel}</label>
            <textarea
              value={rejectReason}
              onChange={(event) => setRejectReason(event.target.value)}
              rows={4}
              placeholder={labels.rejectReasonPlaceholder}
              className="w-full rounded-xl border border-gray-300 px-4 py-3 text-sm text-gray-700 outline-none transition focus:border-red-500"
            />
          </div>
          <div className="flex flex-col gap-3 sm:flex-row">
            <button
              onClick={() => {
                setShowRejectBox(false);
                setRejectReason("");
              }}
              disabled={submitting}
              className="rounded-lg border border-gray-300 px-4 py-2.5 text-sm font-medium text-gray-700 hover:bg-gray-50 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {labels.cancel}
            </button>
            <button
              onClick={() => void handleReject()}
              disabled={submitting || !rejectReason.trim()}
              className="rounded-lg bg-red-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-60"
            >
              {labels.confirmReject}
            </button>
          </div>
        </div>
      ) : (
        <div className="mt-4 flex flex-col gap-3 sm:flex-row">
          <button
            onClick={() => void handleApprove()}
            disabled={submitting}
            className="rounded-lg bg-emerald-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-emerald-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {labels.approvePlan}
          </button>
          <button
            onClick={() => setShowRejectBox(true)}
            disabled={submitting}
            className="rounded-lg bg-red-600 px-4 py-2.5 text-sm font-medium text-white hover:bg-red-700 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {labels.rejectPlan}
          </button>
          {/*
            * The refresh button is always visible while a plan is
            * pending_user. It used to gate on `hasQuoteUnavailableAction`
            * (only shown when the plan-generation quote had failed) but
            * the SlippageGuard rollout turned refresh into a first-class
            * pre-approval action: the user can re-pull a fresh quote at
            * any time to see whether prices have drifted enough to need
            * re-confirmation. DecisionCenter decides whether to open the
            * PriceRefreshDialog based on the actual drift; the button
            * itself just triggers the API call.
            */}
          <button
            onClick={() => void handleRefreshQuote()}
            disabled={submitting}
            className="rounded-lg border border-amber-500 bg-amber-50 px-4 py-2.5 text-sm font-medium text-amber-700 hover:bg-amber-100 disabled:cursor-not-allowed disabled:opacity-60"
          >
            {labels.refreshQuotePlan}
          </button>
        </div>
      )}
    </div>
  );
}

export const ApprovalActions = React.memo(ApprovalActionsInner);
