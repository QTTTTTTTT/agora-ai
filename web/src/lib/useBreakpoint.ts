// useBreakpoint.ts — current responsive breakpoint as a React hook.
//
// WHY THIS EXISTS
// ---------------
// Most of our responsive behaviour is handled by Tailwind utility
// classes (sm:, md:, lg:, xl:) which is exactly the right place
// to put it. But there are layout decisions that DON'T compress
// neatly to "show / hide via CSS":
//
//   - "render a table with 8 columns on desktop, but render the
//     same data as a stack of cards on mobile" — two completely
//     different markup trees, picked at runtime;
//   - "show the first 6 rows on mobile and a 'Show more' button
//     vs the full list on desktop";
//   - "use VirtualList overscan=10 on desktop, overscan=2 on
//     mobile because the row height is bigger";
//
// This hook gives every component access to the current
// breakpoint as a single string, computed from window.innerWidth
// using the Tailwind default breakpoints (so it stays in sync
// with the CSS we write right next to it).
//
// SSR / pre-mount semantics: returns 'lg' as a sensible default
// on the server / before the first render, then updates on mount
// to the real value. We don't try to be cute with media queries
// here because the matchMedia API would have to listen to four
// separate queries and the render order is identical.

import { useEffect, useState } from "react";

export type Breakpoint = "sm" | "md" | "lg" | "xl" | "2xl";

// Tailwind defaults — keep in sync with web/tailwind.config.js if
// the breakpoints ever get customized.
const BREAKPOINTS = {
  sm: 640,
  md: 768,
  lg: 1024,
  xl: 1280,
  "2xl": 1536,
} as const;

function resolve(width: number): Breakpoint {
  if (width >= BREAKPOINTS["2xl"]) return "2xl";
  if (width >= BREAKPOINTS.xl) return "xl";
  if (width >= BREAKPOINTS.lg) return "lg";
  if (width >= BREAKPOINTS.md) return "md";
  return "sm";
}

/**
 * Current Tailwind-aligned breakpoint. Re-renders the consuming
 * component when the user resizes across a threshold — debounced
 * to one rAF tick so a slow drag doesn't cause a render storm.
 */
export function useBreakpoint(): Breakpoint {
  const [bp, setBp] = useState<Breakpoint>(() => {
    if (typeof window === "undefined") return "lg";
    return resolve(window.innerWidth);
  });

  useEffect(() => {
    if (typeof window === "undefined") return;
    let raf = 0;
    const onResize = () => {
      cancelAnimationFrame(raf);
      raf = requestAnimationFrame(() => {
        setBp(resolve(window.innerWidth));
      });
    };
    window.addEventListener("resize", onResize, { passive: true });
    onResize();
    return () => {
      window.removeEventListener("resize", onResize);
      cancelAnimationFrame(raf);
    };
  }, []);

  return bp;
}

/**
 * Convenience: true when current breakpoint is < the given
 * Tailwind threshold. Mirrors the "below md" reading of media
 * queries.
 *
 * Example:
 *   const isMobile = useIsBelow("md");
 *   return isMobile ? <CardList /> : <DataTable />;
 */
export function useIsBelow(threshold: Breakpoint): boolean {
  const bp = useBreakpoint();
  return BREAKPOINTS[bp] < BREAKPOINTS[threshold];
}

/**
 * Convenience: true when current breakpoint is at or above the
 * given Tailwind threshold.
 */
export function useIsAtLeast(threshold: Breakpoint): boolean {
  const bp = useBreakpoint();
  return BREAKPOINTS[bp] >= BREAKPOINTS[threshold];
}
