import React, { useEffect, useMemo } from "react";
import type { AppLanguage } from "../lib/preferences";

// Pagination renders prev/next + page-of-N indicator + optional jump
// box. We keep this dumb (state lives in the parent) so the same
// control can drive a static array slice (recent trades, history
// plans) and a buffered SSE stream (team activity panel) without
// each call site reinventing keyboard shortcuts and disabled-state
// logic. The component is purposely visually neutral so it inherits
// the surrounding card's tone — no card chrome of its own.
interface PaginationProps {
  page: number;
  pageCount: number;
  pageSize: number;
  totalItems: number;
  language: AppLanguage;
  onPageChange: (next: number) => void;
  // pageSizeOptions is opt-in; when omitted the size selector is
  // hidden so the control collapses to just prev/next + counter on
  // dense layouts (sidebars).
  pageSizeOptions?: number[];
  onPageSizeChange?: (next: number) => void;
  // align lets sidebar consumers center the control inside narrow
  // columns while wide tables keep it right-aligned next to the
  // total counter.
  align?: "start" | "center" | "end" | "between";
  // size accepts "sm" for sidebar usage where vertical real estate
  // is tight; "md" is the default for tables.
  size?: "sm" | "md";
}

const copyByLanguage: Record<AppLanguage, {
  prev: string;
  next: string;
  pageOf: (page: number, total: number) => string;
  totalLabel: (n: number) => string;
  jumpLabel: string;
  goLabel: string;
  perPageLabel: string;
}> = {
  "zh-CN": {
    prev: "上一页",
    next: "下一页",
    pageOf: (page, total) => `第 ${page} / ${Math.max(total, 1)} 页`,
    totalLabel: (n) => `共 ${n} 条`,
    jumpLabel: "跳转",
    goLabel: "前往",
    perPageLabel: "每页",
  },
  "en-US": {
    prev: "Prev",
    next: "Next",
    pageOf: (page, total) => `Page ${page} of ${Math.max(total, 1)}`,
    totalLabel: (n) => `${n} items`,
    jumpLabel: "Go to",
    goLabel: "Go",
    perPageLabel: "Per page",
  },
};

function clampPage(page: number, pageCount: number): number {
  if (pageCount <= 0) return 0;
  if (page < 0) return 0;
  if (page >= pageCount) return pageCount - 1;
  return page;
}

const Pagination: React.FC<PaginationProps> = ({
  page,
  pageCount,
  pageSize,
  totalItems,
  language,
  onPageChange,
  pageSizeOptions,
  onPageSizeChange,
  align = "between",
  size = "md",
}) => {
  const copy = copyByLanguage[language] ?? copyByLanguage["en-US"];
  const safePage = clampPage(page, pageCount);
  const isFirst = safePage <= 0;
  const isLast = safePage >= pageCount - 1 || pageCount === 0;
  const buttonClasses =
    size === "sm"
      ? "rounded-full border border-ink-200 bg-white px-2.5 py-1 text-[11px] font-medium text-ink-700 transition hover:border-ink-400 hover:bg-cream-100 disabled:cursor-not-allowed disabled:border-ink-100 disabled:text-ink-300 disabled:hover:bg-white"
      : "rounded-full border border-ink-200 bg-white px-3 py-1.5 text-xs font-medium text-ink-700 transition hover:border-ink-400 hover:bg-cream-100 disabled:cursor-not-allowed disabled:border-ink-100 disabled:text-ink-300 disabled:hover:bg-white";
  const counterClasses = size === "sm" ? "text-[11px] text-ink-500" : "text-xs text-ink-500";

  const justifyClass =
    align === "start"
      ? "justify-start"
      : align === "center"
        ? "justify-center"
        : align === "end"
          ? "justify-end"
          : "justify-between";

  return (
    <div className={`flex flex-wrap items-center gap-2 ${justifyClass}`}>
      <div className={`flex items-center gap-2 ${counterClasses}`}>
        <span>{copy.pageOf(pageCount === 0 ? 0 : safePage + 1, pageCount)}</span>
        <span className="text-ink-300">·</span>
        <span>{copy.totalLabel(totalItems)}</span>
      </div>
      <div className="flex items-center gap-1.5">
        {pageSizeOptions && onPageSizeChange ? (
          <label className={`flex items-center gap-1 ${counterClasses}`}>
            <span>{copy.perPageLabel}</span>
            <select
              value={pageSize}
              onChange={(e) => onPageSizeChange(Number(e.target.value))}
              className="rounded-full border border-ink-200 bg-white px-2 py-1 text-[11px] text-ink-700 focus:border-ink-400 focus:outline-none"
            >
              {pageSizeOptions.map((opt) => (
                <option key={opt} value={opt}>
                  {opt}
                </option>
              ))}
            </select>
          </label>
        ) : null}
        <button
          type="button"
          onClick={() => onPageChange(Math.max(0, safePage - 1))}
          disabled={isFirst}
          className={buttonClasses}
        >
          {copy.prev}
        </button>
        <button
          type="button"
          onClick={() => onPageChange(Math.min(pageCount - 1, safePage + 1))}
          disabled={isLast}
          className={buttonClasses}
        >
          {copy.next}
        </button>
      </div>
    </div>
  );
};

export default Pagination;

// usePaginatedSlice slices a stable array against a `page` + `pageSize`
// pair and returns helpers the parent can render directly. The hook
// auto-clamps `page` whenever the data shrinks below the current
// offset (e.g. after a filter change wipes most of the rows out)
// without forcing every caller to wire its own useEffect.
export interface PaginatedSlice<T> {
  page: number;
  pageCount: number;
  pageSize: number;
  setPage: (p: number) => void;
  setPageSize: (s: number) => void;
  slice: T[];
  totalItems: number;
}

export function usePaginatedSlice<T>(
  items: T[],
  initialPageSize = 10,
  initialPage = 0,
): PaginatedSlice<T> {
  const [page, setPageRaw] = React.useState(initialPage);
  const [pageSize, setPageSize] = React.useState(initialPageSize);
  const totalItems = items.length;
  const pageCount = totalItems === 0 ? 0 : Math.ceil(totalItems / pageSize);

  const setPage = React.useCallback(
    (next: number) => {
      if (pageCount === 0) {
        setPageRaw(0);
        return;
      }
      const bounded = next < 0 ? 0 : next >= pageCount ? pageCount - 1 : next;
      setPageRaw(bounded);
    },
    [pageCount],
  );

  // Clamp the current page when items shrink below the previous offset.
  // E.g. user is on page 5 of recent trades, then a filter wipes the
  // list down to 12 items — without this guard we'd render an empty
  // page with a "Next" button that does nothing.
  useEffect(() => {
    if (pageCount === 0 && page !== 0) {
      setPageRaw(0);
      return;
    }
    if (page >= pageCount && pageCount > 0) {
      setPageRaw(pageCount - 1);
    }
  }, [pageCount, page]);

  const slice = useMemo(() => {
    if (totalItems === 0) return [] as T[];
    const start = page * pageSize;
    return items.slice(start, start + pageSize);
  }, [items, page, pageSize, totalItems]);

  return { page, pageCount, pageSize, setPage, setPageSize, slice, totalItems };
}
