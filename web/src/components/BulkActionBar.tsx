// BulkActionBar.tsx — multi-select selection toolbar primitive.
//
// WHY THIS EXISTS
// ---------------
// Several pages list items where the user wants to perform the
// same action on many of them: SkillInbox (approve/reject N
// proposed skills), DecisionCenter (approve/reject pending
// decisions), AuditLog (mark N events reviewed), Promotions
// (publish a batch). Today every action is one-by-one — open
// each row, click approve, wait, repeat. For a daily reviewer
// with 10-50 items in queue that's a measurable ergonomic tax.
//
// This commit lands the PRIMITIVE — a generic
// <BulkActionBar> + a `useBulkSelection<T>()` hook — plus a
// concrete reference integration on AuditLog (the simplest
// list page in the codebase, the safest place to validate the
// pattern). Per-action wiring on SkillInbox / DecisionCenter
// follows in separate commits where each has a different
// "after-action refresh" contract that warrants its own
// review.
//
// SHAPES
// ------
//
//   const sel = useBulkSelection(items, (i) => i.id)
//   sel.selectedCount, sel.allSelected, sel.toggle(id), sel.toggleAll(), sel.clear()
//
//   <BulkActionBar
//     selectedCount={sel.selectedCount}
//     totalCount={items.length}
//     onClear={sel.clear}
//     actions={[
//       { id: "approve", label: "Approve", onRun: async () => approveMany(sel.selectedIds) },
//       { id: "reject",  label: "Reject",  variant: "danger", onRun: async () => rejectMany(sel.selectedIds) },
//     ]}
//   />
//
// The bar floats at the bottom-center when count > 0 and slides
// out when count = 0. It includes a "Clear selection" affordance
// and per-action progress (busy state during onRun). Each
// `onRun` is awaited; if it throws, the bar surfaces the error
// label briefly then clears the busy state — the parent page is
// responsible for refreshing data on success.
//
// SAFETY
// ------
// - Destructive actions (variant: 'danger') get a JS confirm()
//   prompt by default. Pages that have their own confirm modal
//   can pass `confirmHandled: true` and skip ours.
// - Concurrent triggers are blocked: while one action is
//   running, all action buttons are disabled.

import React, { useCallback, useMemo, useState } from "react";

export interface BulkAction {
  id: string;
  label: string;
  variant?: "primary" | "default" | "danger";
  /** Returns a promise — the bar awaits and disables itself while in flight. */
  onRun: () => Promise<unknown> | unknown;
  /** When true, the bar will NOT prompt window.confirm() for danger variant. */
  confirmHandled?: boolean;
  /** Override the auto-generated confirm message. */
  confirmMessage?: string;
}

export interface BulkActionBarProps {
  selectedCount: number;
  totalCount: number;
  onClear: () => void;
  actions: BulkAction[];
  /** Localised "X selected" template. {{count}} is interpolated. Default: English. */
  selectedLabel?: string;
  /** Localised "Clear" label. */
  clearLabel?: string;
}

const variantClasses: Record<NonNullable<BulkAction["variant"]>, string> = {
  primary: "bg-indigo-600 text-white hover:bg-indigo-500 disabled:bg-indigo-300",
  default: "bg-white text-gray-700 hover:bg-gray-50 disabled:text-gray-400 dark:bg-slate-800 dark:text-slate-100 dark:hover:bg-slate-700",
  danger: "bg-red-600 text-white hover:bg-red-500 disabled:bg-red-300",
};

export const BulkActionBar: React.FC<BulkActionBarProps> = ({
  selectedCount,
  totalCount,
  onClear,
  actions,
  selectedLabel = "{{count}} selected",
  clearLabel = "Clear",
}) => {
  const [runningId, setRunningId] = useState<string | null>(null);

  const handle = useCallback(
    async (action: BulkAction) => {
      if (runningId) return;
      if (action.variant === "danger" && !action.confirmHandled) {
        const msg =
          action.confirmMessage ?? `${action.label}: ${selectedCount} item(s)?`;
        if (!window.confirm(msg)) return;
      }
      setRunningId(action.id);
      try {
        await action.onRun();
      } catch (err) {
        // Best-effort: log to console; the parent page is expected
        // to handle the error UX (toast, inline message). We don't
        // re-throw because that would crash the React event handler.
        console.error("[bulk-action]", action.id, "failed:", err);
      } finally {
        setRunningId(null);
      }
    },
    [runningId, selectedCount],
  );

  if (selectedCount === 0) return null;

  return (
    <div
      role="toolbar"
      aria-label="Bulk actions"
      className="fixed bottom-6 left-1/2 z-40 -translate-x-1/2 transform"
    >
      <div className="flex items-center gap-2 rounded-full border border-gray-200 bg-white px-4 py-2 shadow-2xl dark:border-slate-600 dark:bg-slate-800">
        <span className="text-sm font-medium text-gray-700 dark:text-slate-200">
          {selectedLabel.replace("{{count}}", selectedCount.toString())}
          <span className="ml-1 text-xs text-gray-400 dark:text-slate-500">/ {totalCount}</span>
        </span>
        <span className="mx-1 h-5 w-px bg-gray-200 dark:bg-slate-600" aria-hidden="true" />
        {actions.map((a) => {
          const variant = a.variant ?? "default";
          const busy = runningId === a.id;
          return (
            <button
              key={a.id}
              type="button"
              onClick={() => void handle(a)}
              disabled={runningId !== null}
              className={`rounded-full px-3 py-1.5 text-sm font-medium transition ${variantClasses[variant]} disabled:cursor-not-allowed`}
            >
              {busy ? `${a.label}…` : a.label}
            </button>
          );
        })}
        <button
          type="button"
          onClick={onClear}
          disabled={runningId !== null}
          aria-label={clearLabel}
          className="ml-1 rounded-full px-2 py-1 text-sm text-gray-500 transition hover:bg-gray-100 hover:text-gray-700 dark:text-slate-400 dark:hover:bg-slate-700 dark:hover:text-slate-200"
        >
          ×
        </button>
      </div>
    </div>
  );
};

// ---------------------------------------------------------------
// useBulkSelection — generic selection state hook for any list.
// ---------------------------------------------------------------

export interface UseBulkSelectionResult<T> {
  /** Set of selected ids. Stable identity unless the underlying selection changes. */
  selectedIds: Set<string>;
  selectedCount: number;
  allSelected: boolean;
  noneSelected: boolean;
  isSelected: (id: string) => boolean;
  toggle: (id: string) => void;
  toggleAll: () => void;
  /** Select N specific items at once (e.g. shift-click range select). */
  setMany: (ids: string[], checked: boolean) => void;
  clear: () => void;
  /** Concrete items currently selected (filtered by membership). */
  selectedItems: T[];
}

export function useBulkSelection<T>(
  items: T[],
  getId: (item: T) => string,
): UseBulkSelectionResult<T> {
  const [selected, setSelected] = useState<Set<string>>(() => new Set());

  const totalIds = useMemo(() => items.map(getId), [items, getId]);

  const selectedItems = useMemo(
    () => items.filter((it) => selected.has(getId(it))),
    [items, selected, getId],
  );

  const isSelected = useCallback((id: string) => selected.has(id), [selected]);

  const toggle = useCallback((id: string) => {
    setSelected((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  }, []);

  const setMany = useCallback((ids: string[], checked: boolean) => {
    setSelected((prev) => {
      const next = new Set(prev);
      for (const id of ids) {
        if (checked) next.add(id);
        else next.delete(id);
      }
      return next;
    });
  }, []);

  const toggleAll = useCallback(() => {
    setSelected((prev) => {
      // If everything's selected → clear; otherwise → select all.
      if (totalIds.every((id) => prev.has(id))) return new Set();
      return new Set(totalIds);
    });
  }, [totalIds]);

  const clear = useCallback(() => setSelected(new Set()), []);

  return {
    selectedIds: selected,
    selectedCount: selected.size,
    allSelected: totalIds.length > 0 && totalIds.every((id) => selected.has(id)),
    noneSelected: selected.size === 0,
    isSelected,
    toggle,
    toggleAll,
    setMany,
    clear,
    selectedItems,
  };
}

export default BulkActionBar;
