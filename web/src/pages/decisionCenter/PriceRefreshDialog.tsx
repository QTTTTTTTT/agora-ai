import React from "react";

export interface PriceRefreshLabels {
  // Header copy ("Prices changed since you opened this plan", etc.).
  title: string;
  // Subhead explaining the threshold ("These actions moved more than 0.3% — please review before approving.").
  subtitle: string;
  // Column headers and decorations.
  columnSymbol: string;
  columnOldPrice: string;
  columnNewPrice: string;
  columnDrift: string;
  // CTA: dismiss the dialog. After this the user can choose to approve
  // or refresh again.
  acknowledge: string;
  // Caption shown when only minor changes (< threshold) occurred. The
  // dialog is suppressed in that case but parents may surface this in
  // a toast.
  noMaterialChange?: string;
}

export interface PriceRefreshRow {
  // Stable identifier (the planAction.id when available, else the symbol).
  key: string;
  symbol: string;
  // Old/new prices captured before/after the refresh. Either may be
  // undefined if the action arrived without a price (e.g. quote was
  // unavailable at plan-generation time); in that case render '—'.
  oldPrice?: number;
  newPrice?: number;
  // Signed fractional drift, e.g. 0.0123 = +1.23%. The parent computes
  // this once; the dialog doesn't recompute it so a future change to
  // the drift formula is a one-place edit upstream.
  drift?: number;
}

interface Props {
  open: boolean;
  labels: PriceRefreshLabels;
  rows: PriceRefreshRow[];
  onClose: () => void;
}

// PriceRefreshDialog is a passive, presentation-only modal. The
// decision of *whether* to open it lives in DecisionCenter (which
// snapshots prices around the refresh-quote API call); this component
// just renders what it's told.
//
// Rationale: keeping the threshold logic out of the dialog means the
// same component can be reused for "all changes" displays (e.g. a
// future "what changed since I last looked at this plan?" tooltip)
// without duplicating drift-calculation code.
function PriceRefreshDialogInner({ open, labels, rows, onClose }: Props) {
  if (!open) {
    return null;
  }
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="price-refresh-dialog-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/50 px-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-2xl rounded-2xl bg-white p-6 shadow-2xl"
        // Clicks inside the panel should not bubble up to the overlay
        // and dismiss the dialog. This is the standard "click backdrop
        // to close" pattern used elsewhere in the app.
        onClick={(event) => event.stopPropagation()}
      >
        <div className="mb-4">
          <h2 id="price-refresh-dialog-title" className="text-lg font-semibold text-gray-900">
            {labels.title}
          </h2>
          <p className="mt-1 text-sm text-gray-600">{labels.subtitle}</p>
        </div>

        <div className="overflow-hidden rounded-xl border border-gray-200">
          <table className="w-full text-sm">
            <thead className="bg-gray-50 text-xs uppercase tracking-wider text-gray-500">
              <tr>
                <th className="px-4 py-2 text-left">{labels.columnSymbol}</th>
                <th className="px-4 py-2 text-right">{labels.columnOldPrice}</th>
                <th className="px-4 py-2 text-right">{labels.columnNewPrice}</th>
                <th className="px-4 py-2 text-right">{labels.columnDrift}</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-gray-100 bg-white">
              {rows.map((row) => (
                <tr key={row.key}>
                  <td className="px-4 py-2 font-medium text-gray-900">{row.symbol}</td>
                  <td className="px-4 py-2 text-right text-gray-700">{formatPrice(row.oldPrice)}</td>
                  <td className="px-4 py-2 text-right text-gray-900">{formatPrice(row.newPrice)}</td>
                  <td
                    className={`px-4 py-2 text-right font-medium ${driftTone(row.drift)}`}
                    aria-label={driftLabel(row.drift)}
                  >
                    {formatDrift(row.drift)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>

        <div className="mt-6 flex justify-end">
          <button
            onClick={onClose}
            className="rounded-lg bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-700"
          >
            {labels.acknowledge}
          </button>
        </div>
      </div>
    </div>
  );
}

// formatPrice renders 4 decimal places to match the precision the
// backend stores on plan_actions.price (NUMERIC(16, 4)). Lower precision
// in the UI than the DB would mask sub-cent drift that's still material
// for low-priced (<$1) instruments.
function formatPrice(value?: number): string {
  if (typeof value !== "number" || Number.isNaN(value)) {
    return "—";
  }
  return value.toLocaleString(undefined, { minimumFractionDigits: 2, maximumFractionDigits: 4 });
}

function formatDrift(value?: number): string {
  if (typeof value !== "number" || Number.isNaN(value)) {
    return "—";
  }
  const sign = value > 0 ? "+" : "";
  return `${sign}${(value * 100).toFixed(2)}%`;
}

function driftTone(value?: number): string {
  if (typeof value !== "number" || Number.isNaN(value)) {
    return "text-gray-500";
  }
  if (value > 0) return "text-emerald-600";
  if (value < 0) return "text-red-600";
  return "text-gray-700";
}

function driftLabel(value?: number): string {
  if (typeof value !== "number" || Number.isNaN(value)) {
    return "drift unavailable";
  }
  return `drift ${(value * 100).toFixed(2)} percent`;
}

export const PriceRefreshDialog = React.memo(PriceRefreshDialogInner);
