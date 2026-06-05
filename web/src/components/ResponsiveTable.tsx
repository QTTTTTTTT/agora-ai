// ResponsiveTable.tsx — adaptive table → card switcher.
//
// WHY THIS EXISTS
// ---------------
// Our finance tables (TradeHistory, AuditLog, DecisionCenter,
// ABTestCompare, etc.) routinely have 8–12 columns. On a phone
// (~360 px wide) they horizontally scroll — technically correct,
// practically miserable: users have to scroll left-right to read
// a single row, the column headers disappear off-screen, and the
// "tap to drill in" action sits in the rightmost column.
//
// This component lets a page declare ONE column schema and pick
// rendering at runtime:
//
//   - At md+ breakpoints: render a regular HTML table.
//   - Below md (or wherever caller specifies via collapseBelow):
//     render the same rows as a vertical stack of cards, with
//     each row's columns becoming label/value pairs inside the
//     card. The first column is treated as the "primary" / title
//     line and rendered larger; the rest stack under it.
//
// We deliberately DON'T use display:contents or some pure-CSS
// table-to-card trick because:
//   - the column headers need to vanish on mobile, but a CSS-
//     only flip leaves them visually heavy;
//   - the "primary column → card title" promotion needs different
//     typography that's hard to express purely via classes;
//   - each card sometimes wants a tap target wrapping the whole
//     row, which a table can't do directly.
//
// CONTRACT
// --------
// The page builds a column schema:
//
//   const columns: ResponsiveColumn<Trade>[] = [
//     { key: "executedAt", header: copy.col.time, primary: true,
//       cell: (t) => formatDate(t.executedAt) },
//     { key: "symbol", header: copy.col.symbol,
//       cell: (t) => t.symbol },
//     { key: "side", header: copy.col.side,
//       cell: (t) => <SideBadge side={t.side} /> },
//     ...
//   ];
//
//   <ResponsiveTable
//     rows={trades}
//     keyOf={(t) => t.id}
//     columns={columns}
//     onRowClick={(t) => openTrade(t)}
//   />
//
// "primary: true" marks the column shown as the card title on
// mobile (defaults to the first column if none flagged).
//
// SCOPE
// -----
// Starter version: doesn't sort, paginate, or virtualize.
// Caller is expected to have already sliced rows to a sensible
// length, and to use VirtualList / pagination at the page level.

import React, { ReactNode } from "react";
import { useIsBelow } from "../lib/useBreakpoint";

export interface ResponsiveColumn<T> {
  /** Stable key — used as the React key for cells. */
  key: string;
  /** Header shown in desktop table mode. Also used as the label
      next to the value in mobile card mode. */
  header: ReactNode;
  /** Cell renderer. Receives the row, returns a ReactNode. */
  cell: (row: T) => ReactNode;
  /** Treat this column as the card title on mobile. Defaults to
      the first column when no column is flagged. */
  primary?: boolean;
  /** Optional Tailwind class for the desktop cell to e.g. align
      right or constrain width. */
  className?: string;
  /** Hide the column entirely on mobile (e.g. very low-signal
      metadata that bloats the card). */
  hideOnMobile?: boolean;
}

export interface ResponsiveTableProps<T> {
  rows: readonly T[];
  columns: readonly ResponsiveColumn<T>[];
  keyOf: (row: T) => string;
  onRowClick?: (row: T) => void;
  /** Tailwind threshold below which we collapse to cards. Default 'md'. */
  collapseBelow?: "sm" | "md" | "lg";
  /** Empty-state node when rows.length === 0. */
  empty?: ReactNode;
  /** Additional class for the wrapping container. */
  className?: string;
}

export function ResponsiveTable<T>(props: ResponsiveTableProps<T>) {
  const { rows, columns, keyOf, onRowClick, collapseBelow = "md", empty, className } = props;
  const isMobile = useIsBelow(collapseBelow);

  if (rows.length === 0) {
    if (empty) return <>{empty}</>;
    return null;
  }

  const primaryIdx = (() => {
    const flagged = columns.findIndex((c) => c.primary);
    return flagged >= 0 ? flagged : 0;
  })();

  if (isMobile) {
    return (
      <ul className={`space-y-2 ${className ?? ""}`}>
        {rows.map((row) => {
          const primary = columns[primaryIdx];
          const rest = columns
            .map((col, i) => ({ col, i }))
            .filter(({ i, col }) => i !== primaryIdx && !col.hideOnMobile);
          return (
            <li
              key={keyOf(row)}
              className={`rounded-xl border border-gray-200 bg-white p-3 shadow-sm dark:border-slate-700 dark:bg-slate-800 ${onRowClick ? "cursor-pointer transition hover:border-indigo-300 hover:bg-indigo-50/40 dark:hover:border-indigo-500 dark:hover:bg-indigo-900/20" : ""}`}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
              role={onRowClick ? "button" : undefined}
              tabIndex={onRowClick ? 0 : undefined}
              onKeyDown={
                onRowClick
                  ? (e) => {
                      if (e.key === "Enter" || e.key === " ") {
                        e.preventDefault();
                        onRowClick(row);
                      }
                    }
                  : undefined
              }
            >
              <div className="text-sm font-semibold text-gray-900 dark:text-slate-100">
                {primary.cell(row)}
              </div>
              <dl className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1.5 text-xs">
                {rest.map(({ col }) => (
                  <React.Fragment key={col.key}>
                    <dt className="truncate text-gray-500 dark:text-slate-400">{col.header}</dt>
                    <dd className="truncate text-right text-gray-800 dark:text-slate-200">{col.cell(row)}</dd>
                  </React.Fragment>
                ))}
              </dl>
            </li>
          );
        })}
      </ul>
    );
  }

  return (
    <div className={`overflow-x-auto ${className ?? ""}`}>
      <table className="min-w-full divide-y divide-gray-200 dark:divide-slate-700">
        <thead className="bg-gray-50 dark:bg-slate-800/60">
          <tr>
            {columns.map((col) => (
              <th
                key={col.key}
                className={`px-3 py-2 text-left text-xs font-medium uppercase tracking-wide text-gray-500 dark:text-slate-400 ${col.className ?? ""}`}
              >
                {col.header}
              </th>
            ))}
          </tr>
        </thead>
        <tbody className="divide-y divide-gray-100 bg-white dark:divide-slate-800 dark:bg-slate-900">
          {rows.map((row) => (
            <tr
              key={keyOf(row)}
              className={onRowClick ? "cursor-pointer transition hover:bg-indigo-50/40 dark:hover:bg-indigo-900/20" : ""}
              onClick={onRowClick ? () => onRowClick(row) : undefined}
            >
              {columns.map((col) => (
                <td
                  key={col.key}
                  className={`whitespace-nowrap px-3 py-2 text-sm text-gray-700 dark:text-slate-200 ${col.className ?? ""}`}
                >
                  {col.cell(row)}
                </td>
              ))}
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
