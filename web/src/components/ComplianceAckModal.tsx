// ComplianceAckModal.tsx — the affirmative click-through
// disclosure modal.
//
// Triggered the first time the user lands on any of the
// advisory surfaces (advisor, paper trading, backtest). The
// modal:
//
//   1. Shows the localised acknowledgmentText from the
//      compliance bundle.
//   2. Requires the user to TICK an explicit checkbox. The
//      "I understand" button stays disabled until the box is
//      checked. (Pre-checked tick boxes have been ruled
//      insufficient consent by SEC enforcement actions.)
//   3. POSTs the click-through to /api/compliance/acknowledgments
//      so we have a server-side audit trail with the EXACT
//      text the user saw at click time.
//   4. Persists a localStorage flag so the modal doesn't
//      re-prompt on every page load. Server-side enforcement
//      (the advisor handler's disclaimerOK gate) is still
//      the source of truth — the localStorage flag is a UX
//      optimisation, not a security measure.
//
// The component is render-prop free: it owns its own open
// state derived from the compliance context's
// isAcknowledged(surface) value. Callers just mount it and
// it auto-opens when needed.

import { useState, useEffect } from "react";

import {
  useCompliance,
  type ComplianceSurface,
} from "../lib/compliance";

export type ComplianceAckModalProps = {
  surface: ComplianceSurface;
  // forceOpen overrides the auto-open behaviour. Useful for a
  // "review disclosure" button in the user settings page.
  forceOpen?: boolean;
  onAcknowledged?: () => void;
};

export function ComplianceAckModal({
  surface,
  forceOpen,
  onAcknowledged,
}: ComplianceAckModalProps) {
  const { bundle, isAcknowledged, recordAck, ready, mode } = useCompliance();
  const [checked, setChecked] = useState(false);
  const [pending, setPending] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [open, setOpen] = useState(false);

  const b = bundle(surface);

  useEffect(() => {
    if (!ready) return;
    if (forceOpen) {
      setOpen(true);
      return;
    }
    if (mode === "ria_registered") {
      setOpen(false);
      return;
    }
    setOpen(!isAcknowledged(surface));
  }, [ready, forceOpen, mode, isAcknowledged, surface]);

  if (!open) return null;

  const labels = pickLabels(b.locale);

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="compliance-ack-title"
      className="fixed inset-0 z-[100] flex items-center justify-center bg-black/50 p-4"
    >
      <div className="max-w-lg rounded-lg bg-white p-5 shadow-xl dark:bg-zinc-900">
        <h2
          id="compliance-ack-title"
          className="text-base font-semibold text-zinc-900 dark:text-zinc-100"
        >
          {labels.title}
        </h2>
        <p className="mt-3 whitespace-pre-wrap text-sm leading-6 text-zinc-700 dark:text-zinc-300">
          {b.acknowledgmentText}
        </p>
        <label className="mt-4 flex items-start gap-2 text-sm text-zinc-700 dark:text-zinc-300">
          <input
            type="checkbox"
            checked={checked}
            onChange={(e) => setChecked(e.target.checked)}
            className="mt-0.5"
            data-testid="compliance-ack-checkbox"
          />
          <span>{labels.checkboxLabel}</span>
        </label>
        {error ? (
          <p className="mt-3 rounded bg-rose-50 px-2 py-1 text-xs text-rose-700 dark:bg-rose-950/40 dark:text-rose-200">
            {error}
          </p>
        ) : null}
        <div className="mt-5 flex items-center justify-end gap-2">
          <button
            type="button"
            disabled={!checked || pending}
            data-testid="compliance-ack-submit"
            className="rounded-md bg-zinc-900 px-3 py-1.5 text-sm font-medium text-white disabled:cursor-not-allowed disabled:opacity-50 dark:bg-zinc-100 dark:text-zinc-900"
            onClick={async () => {
              setPending(true);
              setError(null);
              try {
                await recordAck({
                  surface,
                  acknowledgedText: b.acknowledgmentText,
                });
                setOpen(false);
                onAcknowledged?.();
              } catch (e: unknown) {
                const msg =
                  e instanceof Error
                    ? e.message
                    : labels.errorFallback;
                setError(msg);
              } finally {
                setPending(false);
              }
            }}
          >
            {pending ? labels.submitting : labels.submit}
          </button>
        </div>
      </div>
    </div>
  );
}

function pickLabels(locale: string) {
  if (locale.startsWith("zh")) {
    return {
      title: "重要免责声明 / 须知",
      checkboxLabel:
        "我已阅读上述声明，并理解本服务不构成针对个人的投资建议。",
      submit: "我已理解",
      submitting: "提交中…",
      errorFallback: "保存失败，请重试。",
    };
  }
  return {
    title: "Important disclosures",
    checkboxLabel:
      "I have read the disclosure above and understand this service does not provide personalised investment advice.",
    submit: "I understand",
    submitting: "Submitting…",
    errorFallback: "Failed to record acknowledgment. Please try again.",
  };
}
