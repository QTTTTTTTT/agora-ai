// components/Skeleton.tsx — component-level skeleton placeholders.
//
// WHY THIS EXISTS
// ---------------
// RouteFallback covers the route-loading-the-bundle case (chunk
// fetch). What's missing is the per-component "data is loading,
// don't blank the screen" case: open Dashboard, the route bundle
// is already cached, the chunk paints instantly, but the dashboard
// then renders an empty card while waiting for fund metrics from
// /api/funds/.../overview. Today most pages do this:
//
//   {loading ? <p>加载中…</p> : <Card data={data} />}
//
// — a one-line text fallback, no layout reservation, the card
// jumps in when data arrives causing CLS (cumulative layout
// shift). Skeleton fixes that by reserving the same shape the
// real card will occupy and animating it gently while the data
// loads.
//
// COMPONENTS
// ----------
//
//   <Skeleton variant="text"|"circular"|"rectangular" ... />
//     The atom: a single shimmering placeholder block.
//
//   <SkeletonText lines={3} />
//     N pulsing horizontal lines, mimicking a paragraph of text.
//
//   <SkeletonCard rows={3} />
//     A whole card-shaped scaffold (title + paragraph + metric row),
//     suitable for dashboard widgets while their API call resolves.
//
//   <SkeletonTableRows rows={5} columns={4} />
//     N rows by M columns — drop into a <tbody> placeholder while
//     a list endpoint loads.
//
// ANIMATION
// ---------
// All variants share the `animate-pulse` Tailwind utility (built-in
// in Tailwind v3). The pulse is light grey on white for accessibility
// — strong contrast pulses are tiring to look at, especially on
// long-loading endpoints. Future work: add a wave-style shimmer for
// users who prefer it (prefers-reduced-motion is honoured by
// `animate-pulse` because Tailwind already exposes the
// `motion-reduce:animate-none` modifier).
//
// USAGE PATTERN
// -------------
//
//   {loading ? <SkeletonCard rows={4} /> : <FundOverviewCard data={d} />}
//
// Keep the skeleton's outer dimensions (padding, border-radius,
// margin) matching the real component so the layout doesn't jump
// when the real data arrives. The skeleton card's classnames
// match the convention used by the existing Dashboard widgets
// (rounded-2xl border bg-white p-6) so swap-in is mechanical.
//
// ACCESSIBILITY
// -------------
// Each skeleton sets `role="status"` + `aria-live="polite"` and a
// hidden label so screen readers announce the load. We hide the
// pulsing visuals via `aria-hidden` on the inner blocks so the
// reader doesn't dictate "rectangle, rectangle, rectangle".

import React from "react";

type SkeletonVariant = "text" | "circular" | "rectangular";

export interface SkeletonProps {
  variant?: SkeletonVariant;
  width?: string | number;
  height?: string | number;
  className?: string;
}

const baseClasses =
  "animate-pulse motion-reduce:animate-none bg-gray-200 dark:bg-slate-700";

export const Skeleton: React.FC<SkeletonProps> = ({
  variant = "rectangular",
  width,
  height,
  className,
}) => {
  let shape = "rounded-md";
  if (variant === "circular") shape = "rounded-full";
  else if (variant === "text") shape = "rounded";

  const style: React.CSSProperties = {};
  if (width !== undefined) style.width = typeof width === "number" ? `${width}px` : width;
  if (height !== undefined) style.height = typeof height === "number" ? `${height}px` : height;

  return (
    <span
      aria-hidden="true"
      className={[baseClasses, shape, "block", className ?? ""].join(" ").trim()}
      style={style}
    />
  );
};

export interface SkeletonTextProps {
  lines?: number;
  className?: string;
  /** Last line is shorter than the rest by default — matches typical paragraph wrap. */
  shrinkLastLine?: boolean;
}

export const SkeletonText: React.FC<SkeletonTextProps> = ({
  lines = 3,
  className,
  shrinkLastLine = true,
}) => {
  const items = Array.from({ length: lines });
  return (
    <div role="status" aria-label="Loading" className={["space-y-2", className ?? ""].join(" ").trim()}>
      {items.map((_, i) => {
        const isLast = i === items.length - 1;
        const w = isLast && shrinkLastLine ? "w-2/3" : "w-full";
        return <Skeleton key={i} variant="text" height={12} className={w} />;
      })}
    </div>
  );
};

export interface SkeletonCardProps {
  /** Number of paragraph lines below the title. */
  rows?: number;
  /** If true, render an avatar circle on the left. */
  avatar?: boolean;
  /** If true, render a metric strip (e.g. 3 stat boxes) below the body. */
  metrics?: boolean;
  className?: string;
}

export const SkeletonCard: React.FC<SkeletonCardProps> = ({
  rows = 3,
  avatar = false,
  metrics = false,
  className,
}) => (
  <div
    role="status"
    aria-label="Loading"
    aria-live="polite"
    className={[
      "rounded-2xl border border-gray-200 bg-white p-6 shadow-sm",
      className ?? "",
    ]
      .join(" ")
      .trim()}
  >
    <div className="flex items-start gap-4">
      {avatar ? <Skeleton variant="circular" width={40} height={40} /> : null}
      <div className="flex-1 space-y-3">
        <Skeleton variant="text" height={18} className="w-1/3" />
        <SkeletonText lines={rows} />
      </div>
    </div>
    {metrics ? (
      <div className="mt-6 grid grid-cols-3 gap-3">
        {[0, 1, 2].map((i) => (
          <div key={i} className="rounded-xl border border-gray-100 bg-gray-50 p-3">
            <Skeleton variant="text" height={10} className="w-2/3" />
            <Skeleton variant="text" height={20} className="mt-2 w-1/2" />
          </div>
        ))}
      </div>
    ) : null}
  </div>
);

export interface SkeletonTableRowsProps {
  rows?: number;
  columns?: number;
  /** Match a parent <tr>'s alignment so callers can drop this in <tbody>. */
  className?: string;
}

export const SkeletonTableRows: React.FC<SkeletonTableRowsProps> = ({
  rows = 5,
  columns = 4,
  className,
}) => (
  <>
    {Array.from({ length: rows }).map((_, r) => (
      <tr key={r} className={className} aria-hidden="true">
        {Array.from({ length: columns }).map((__, c) => (
          <td key={c} className="px-4 py-3">
            <Skeleton variant="text" height={12} className={c === 0 ? "w-full" : c === columns - 1 ? "w-1/2" : "w-3/4"} />
          </td>
        ))}
      </tr>
    ))}
  </>
);

export default Skeleton;
