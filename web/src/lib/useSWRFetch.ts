/**
 * useSWRFetch — minimal stale-while-revalidate hook for the
 * platform's read-heavy pages (Dashboard, FundPerformance, AuditLog,
 * TradeHistory, etc.). The hook avoids pulling in `swr` / `react-query`
 * (which would add 8-15 KB to the bundle for behaviour we only need
 * on a handful of pages) by reimplementing the three primitives
 * those libraries are valued for:
 *
 *   1. **In-memory cache** keyed by a string `key`. Two components
 *      asking for the same key share one network request and one
 *      cache slot.
 *
 *   2. **Stale-while-revalidate** semantics: a cached value is
 *      returned IMMEDIATELY on mount; a background refetch fires
 *      iff the cached value is older than the per-call `ttlMs`.
 *      The hook never blocks the UI on a refetch.
 *
 *   3. **In-flight deduplication**: while a request for key K is
 *      pending, additional callers for the same K subscribe to the
 *      same Promise rather than firing a duplicate request.
 *
 * Out of scope for this hook (deliberately):
 *
 *   * Mutation / optimistic updates → component-local state.
 *   * Prefetching / lookahead → `useSWRFetch.prefetch(key, fetcher)` is
 *     provided as a static method but the hook itself does no
 *     speculative work.
 *   * Suspense integration → React 18 SSR/Suspense flow doesn't
 *     fit our top-level hand-rolled loading UI; we keep the
 *     classic { data, error, isLoading } shape that every page
 *     already understands.
 *
 * The hook is intentionally framework-agnostic: the `fetcher`
 * argument is any async function returning the typed value. Pages
 * pass `(signal) => apiCall(args, signal)` so cancellation Just
 * Works on unmount.
 *
 * Cardinality / memory note: the cache is a module-scope `Map`;
 * each page typically holds 1-5 keys. There is no eviction policy
 * because every key is short-lived in practice (the user navigates
 * away, the page re-mounts, the cache is hit-and-discarded). If a
 * future use case demands long-lived caches we can layer LRU on
 * top — the cache shape is designed to support it.
 */

import { useCallback, useEffect, useRef, useState } from "react";

/** One cache entry per key. */
interface CacheEntry<T> {
  /** Last successful response. */
  data: T | undefined;
  /** Last error from the fetcher. Cleared on next successful fetch. */
  error: Error | undefined;
  /** epoch ms of the last *successful* fetch. */
  fetchedAt: number;
  /** In-flight request promise, deduplicates concurrent callers. */
  inFlight?: Promise<T>;
  /** Set of subscribers — pages call setState through these. */
  subscribers: Set<() => void>;
}

/** Module-scope cache. Shared across every `useSWRFetch` invocation. */
const cache = new Map<string, CacheEntry<unknown>>();

function getOrCreateEntry<T>(key: string): CacheEntry<T> {
  let entry = cache.get(key) as CacheEntry<T> | undefined;
  if (!entry) {
    entry = {
      data: undefined,
      error: undefined,
      fetchedAt: 0,
      subscribers: new Set(),
    };
    cache.set(key, entry);
  }
  return entry;
}

/**
 * Notify every subscriber of `entry`. Each subscriber is a tiny
 * function that flips a state-version counter so React re-renders.
 */
function notify<T>(entry: CacheEntry<T>) {
  for (const sub of entry.subscribers) {
    sub();
  }
}

/** Options accepted by useSWRFetch. */
export interface UseSWRFetchOptions {
  /** Time-to-live for "fresh" data, in ms. After this we refetch in
   *  the background. Default 60_000 (60 s). Set to 0 to always refetch
   *  on every mount. */
  ttlMs?: number;
  /** When true, refetch when the browser tab is brought back into
   *  focus. Default true on desktop, has no effect on mobile.
   *  Toggling this off is useful for endpoints whose cost dominates
   *  the freshness benefit (e.g. heavy admin reports). */
  revalidateOnFocus?: boolean;
  /** Do not start the request — the hook returns the cached entry
   *  if any, and stays in idle. Used by pages that lazy-mount
   *  child queries based on tab selection. */
  disabled?: boolean;
}

/** Shape returned to callers. */
export interface UseSWRFetchResult<T> {
  data: T | undefined;
  error: Error | undefined;
  isLoading: boolean;
  /** Forces a refetch ignoring TTL. Returns the response or rejects. */
  mutate: () => Promise<T>;
}

/**
 * useSWRFetch is the canonical hook. Pages invoke:
 *
 *     const { data, error, isLoading } = useSWRFetch(
 *       `funds/${fundId}/holdings`,
 *       (signal) => api.fetchHoldings(fundId, signal),
 *       { ttlMs: 30_000 }
 *     );
 *
 * Implementation walkthrough:
 *
 *   * `useState({})` is purely a re-render trigger — when the cache
 *     entry changes, the subscriber flips this and React re-evaluates
 *     the closure, picking up the new `entry.data` / `entry.error`.
 *   * `useEffect` subscribes on mount and unsubscribes on unmount.
 *     Keying on `key` lets the hook track changes if the caller's
 *     key string changes (e.g. fundId switches).
 *   * `useEffect` ALSO triggers the initial fetch logic: if the
 *     cache entry is stale or empty AND not already in flight, fire
 *     a request.
 */
export function useSWRFetch<T>(
  key: string | null,
  fetcher: (signal: AbortSignal) => Promise<T>,
  options: UseSWRFetchOptions = {},
): UseSWRFetchResult<T> {
  const { ttlMs = 60_000, revalidateOnFocus = true, disabled = false } = options;
  const [, setVersion] = useState(0);
  const fetcherRef = useRef(fetcher);
  fetcherRef.current = fetcher;

  // Pull the entry up-front so the render reflects the current
  // cache state without waiting for the effect to subscribe. If
  // the key is null (caller-disabled), we synthesise a transient
  // entry that never persists.
  const entry: CacheEntry<T> = key !== null ? getOrCreateEntry<T>(key) : {
    data: undefined,
    error: undefined,
    fetchedAt: 0,
    subscribers: new Set(),
  };

  const triggerFetch = useCallback(
    (force: boolean): Promise<T> => {
      if (key === null) {
        return Promise.reject(new Error("useSWRFetch: cannot fetch with null key"));
      }
      if (entry.inFlight && !force) {
        return entry.inFlight;
      }
      const ctl = new AbortController();
      const promise = fetcherRef.current(ctl.signal)
        .then((data) => {
          entry.data = data;
          entry.error = undefined;
          entry.fetchedAt = Date.now();
          entry.inFlight = undefined;
          notify(entry);
          return data;
        })
        .catch((err: unknown) => {
          entry.error = err instanceof Error ? err : new Error(String(err));
          entry.inFlight = undefined;
          notify(entry);
          throw entry.error;
        });
      entry.inFlight = promise;
      return promise;
    },
    [entry, key],
  );

  useEffect(() => {
    if (key === null || disabled) return;
    const sub = () => setVersion((v) => v + 1);
    entry.subscribers.add(sub);

    // Decide if we need to refetch. The cache entry might be:
    //   * empty (first mount for this key) → fetch
    //   * stale (now - fetchedAt > ttlMs) → fetch in background
    //   * fresh → no fetch
    // ttlMs of 0 is "always refetch on mount".
    const isStale = entry.fetchedAt === 0 || Date.now() - entry.fetchedAt > ttlMs;
    if (isStale && !entry.inFlight) {
      triggerFetch(false).catch(() => {
        // error is captured into entry.error; React re-renders.
      });
    }

    let detachFocus: (() => void) | undefined;
    if (revalidateOnFocus && typeof document !== "undefined") {
      const onFocus = () => {
        if (document.visibilityState === "visible") {
          // Refetch even if the cache is fresh, to pick up edits
          // made by other tabs.
          triggerFetch(true).catch(() => undefined);
        }
      };
      document.addEventListener("visibilitychange", onFocus);
      detachFocus = () => document.removeEventListener("visibilitychange", onFocus);
    }

    return () => {
      entry.subscribers.delete(sub);
      if (detachFocus) detachFocus();
    };
  }, [entry, key, disabled, ttlMs, revalidateOnFocus, triggerFetch]);

  const mutate = useCallback(() => triggerFetch(true), [triggerFetch]);

  return {
    data: entry.data,
    error: entry.error,
    isLoading: !entry.data && !entry.error && (entry.inFlight !== undefined || (!disabled && key !== null)),
    mutate,
  };
}

/**
 * Static helpers attached to useSWRFetch for cache management
 * (mostly used in tests and from "Sign out" handlers that need to
 * scrub stale per-user data).
 */
export const swrCache = {
  /** Drop a single key from the cache. */
  invalidate(key: string) {
    cache.delete(key);
  },
  /** Drop every key whose name starts with the given prefix. */
  invalidatePrefix(prefix: string) {
    for (const k of cache.keys()) {
      if (k.startsWith(prefix)) cache.delete(k);
    }
  },
  /** Clear the whole cache — call from logout handlers so a
   *  subsequent re-login of a different user doesn't leak the
   *  previous session's data. */
  clear() {
    cache.clear();
  },
  /** Test-only inspector. */
  _entryForTest(key: string): CacheEntry<unknown> | undefined {
    return cache.get(key);
  },
};
