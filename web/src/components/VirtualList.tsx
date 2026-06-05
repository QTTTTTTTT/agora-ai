// VirtualList.tsx — virtualized scrollable list primitives.
//
// WHY THIS EXISTS
// ---------------
// Several pages in the codebase render long lists by mapping the
// full array to JSX:
//
//   {entries.map((e) => <tr key={e.id}>...</tr>)}
//
// AuditLog caps at 100 rows defensively; TradeHistory caps at the
// last 30 days; MemoryCenter shows whatever the API returns. With
// caps the pages render fine, but the cap is a UX leak — power
// users want to scroll back further, ops needs a 1000-event audit
// trail, and the only thing standing between a clean DOM and 5000
// table rows is "nobody asked yet".
//
// react-window virtualizes lists by rendering only the rows
// currently visible inside the scroll viewport plus a small
// overscan buffer. A 10,000-row list mounts ~30 DOM nodes total,
// not 10,000. CPU stays flat, scroll stays buttery, and "show me
// the full year of decisions" becomes feasible without paging.
//
// USAGE PATTERN (FIXED-HEIGHT ROWS — most common)
// -----------------------------------------------
//
//   <VirtualList
//     items={trades}
//     itemHeight={56}        // pixels per row, must be exact for FixedSize
//     height={480}           // visible viewport height (or 'flex-1' parent + windowSize)
//     renderRow={(trade) => (
//       <div className="grid grid-cols-6 px-4 py-3 text-sm">
//         <div>{trade.symbol}</div>
//         <div>{trade.side}</div>
//         ...
//       </div>
//     )}
//   />
//
// USAGE PATTERN (VARIABLE-HEIGHT ROWS — JSON detail blocks etc.)
// --------------------------------------------------------------
//
//   <VirtualVariableList
//     items={auditEntries}
//     estimatedItemHeight={120}
//     getItemHeight={(entry) => Math.min(360, 80 + entry.detailLines * 18)}
//     height={600}
//     renderRow={(entry) => <AuditRow entry={entry} />}
//   />
//
// Variable-height is more expensive (react-window has to query
// each item's size), so prefer FixedSize when feasible.
//
// MIGRATION RECIPE FOR EXISTING TABLES
// ------------------------------------
// HTML <table> + react-window don't compose cleanly because
// react-window needs absolute-positioned children. The clean
// migration:
//   1. Move the <thead> out of the <table> as a separate row
//      div with the same column widths (CSS Grid is convenient).
//   2. Replace <tbody> { items.map(<tr>) } with VirtualList over
//      a div-based row layout matching the header.
//   3. Wrap the whole thing in a container with explicit height
//      (h-[600px], h-screen-minus-header, or a flex-1 ResizeObserver
//      pattern).
//
// PERFORMANCE NOTES
// -----------------
// - overscanCount defaults to 8 (a few rows above + below the
//   visible area, prevents flicker on fast scroll).
// - Pass a stable `itemKey` if your items have IDs — react-window
//   uses index by default, which makes Reorder operations
//   re-mount rows unnecessarily.
// - Don't put heavy CSS animations on rendered rows; the rows
//   mount/unmount on scroll, and an entrance animation triggering
//   on every scroll feels janky.
//
// FUTURE WORK
// -----------
//   - Auto-height via ResizeObserver — the current API requires
//     callers to pass `height`. A future iteration could expose
//     `<AutoVirtualList>` that fills its parent. (react-virtuoso
//     does this elegantly; keeping our dep surface minimal here
//     until a real consumer needs it.)
//   - Sticky headers in the virtualized list (currently solved
//     by keeping the header outside the VirtualList container).

import React from "react";
import { FixedSizeList, VariableSizeList } from "react-window";

export interface VirtualListProps<T> {
  items: T[];
  /** Exact pixel height of each row (rows are absolutely positioned). */
  itemHeight: number;
  /** Pixel height of the scrollable viewport. */
  height: number;
  /** Render a single row. Wrap in a single root element with no margin. */
  renderRow: (item: T, index: number) => React.ReactNode;
  /** Optional stable key fn — defaults to index, prefer item.id when available. */
  itemKey?: (index: number, item: T) => string;
  /** Rows above/below visible area to keep mounted. Default 8. */
  overscanCount?: number;
  className?: string;
  /** Width of the list. 'auto' (100%) by default; pass a number for fixed-width. */
  width?: number | string;
}

export function VirtualList<T>({
  items,
  itemHeight,
  height,
  renderRow,
  itemKey,
  overscanCount = 8,
  className,
  width = "100%",
}: VirtualListProps<T>): React.ReactElement {
  // FixedSizeList caches row sizes by index; recomputing on every
  // render is fine because the underlying virtual scroll math is
  // O(visible-rows), not O(total).
  return (
    <FixedSizeList
      height={height}
      width={width}
      itemCount={items.length}
      itemSize={itemHeight}
      overscanCount={overscanCount}
      itemKey={itemKey ? (i) => itemKey(i, items[i]) : undefined}
      className={className}
    >
      {({ index, style }) => (
        // The `style` from react-window includes absolute
        // positioning + width — DO NOT override it on the row
        // wrapper, or scroll will visibly tear.
        <div style={style}>{renderRow(items[index], index)}</div>
      )}
    </FixedSizeList>
  );
}

export interface VirtualVariableListProps<T> {
  items: T[];
  estimatedItemHeight: number;
  /** Compute exact pixel height for the row at index. */
  getItemHeight: (item: T, index: number) => number;
  height: number;
  renderRow: (item: T, index: number) => React.ReactNode;
  itemKey?: (index: number, item: T) => string;
  overscanCount?: number;
  className?: string;
  width?: number | string;
}

export function VirtualVariableList<T>({
  items,
  estimatedItemHeight,
  getItemHeight,
  height,
  renderRow,
  itemKey,
  overscanCount = 4, // smaller default — variable-size rows pay an O(n) prefix-sum
  className,
  width = "100%",
}: VirtualVariableListProps<T>): React.ReactElement {
  // VariableSizeList memoises sizes by index; if your getItemHeight
  // depends on outside state (e.g. expanded/collapsed rows), call
  // listRef.current?.resetAfterIndex(0) when that state changes.
  return (
    <VariableSizeList
      height={height}
      width={width}
      itemCount={items.length}
      itemSize={(i) => getItemHeight(items[i], i)}
      estimatedItemSize={estimatedItemHeight}
      overscanCount={overscanCount}
      itemKey={itemKey ? (i) => itemKey(i, items[i]) : undefined}
      className={className}
    >
      {({ index, style }) => (
        <div style={style}>{renderRow(items[index], index)}</div>
      )}
    </VariableSizeList>
  );
}
