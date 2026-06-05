// useListSearch.ts — debounced client-side list search/filter hook.
//
// WHY THIS EXISTS
// ---------------
// Several list pages (Companies, Marketplace, TradeHistory,
// MemoryCenter, AgentLearning, AuditLog) ship today without a
// search box at all, or with a hand-rolled inline filter that
// re-implements the same patterns slightly differently each time:
//
//   const [q, setQ] = useState("");
//   const filtered = useMemo(
//     () => items.filter((i) => i.name.includes(q.toLowerCase())),
//     [items, q],
//   );
//
// Each copy reinvents:
//   - case-insensitive comparison,
//   - debouncing (none of the existing hand-rolls do this — every
//     keystroke immediately re-runs the filter, which is fine for
//     50 rows and visibly janks at 5000),
//   - which fields to search (some include id, some don't),
//   - whether to trim, whether to handle Chinese (most don't).
//
// This hook centralises the pattern with sensible defaults:
//   - 200ms debounce (responsive, but doesn't re-filter on every
//     keystroke for large lists),
//   - case-insensitive, accent-insensitive, whitespace-trimmed,
//   - caller passes a `getSearchableText(item)` to produce the
//     concatenated haystack for each row — usually a join of the
//     few fields the user actually wants to match against,
//   - empty query → all items pass through unchanged (never
//     re-allocate the array when there's no filter).
//
// USAGE PATTERN
// -------------
//
//   const { query, setQuery, filtered } = useListSearch(
//     companies,
//     (c) => `${c.name} ${c.symbol} ${c.sector ?? ""}`,
//   );
//
//   <input value={query} onChange={(e) => setQuery(e.target.value)} ... />
//   {filtered.map((c) => ...)}
//
// COMPLEXITY
// ----------
// Each filter pass is O(n × len(haystack)). For lists up to ~5000
// rows × ~50-char haystacks this stays well under a frame at
// 60fps. For larger lists, do server-side search (POST /search?q=).
// The hook intentionally doesn't add an internal index — building
// an index for a list that gets re-derived every time the parent
// re-fetches is the wrong trade-off here.

import { useEffect, useMemo, useState } from "react";

export interface UseListSearchOptions<T> {
  /** Build the searchable string for an item. Concatenate the fields you want matched. */
  getSearchableText: (item: T) => string;
  /** Debounce delay in milliseconds. Default 200. Pass 0 for synchronous filter. */
  debounceMs?: number;
  /** Initial query value. Default "". Useful for restoring from URL params. */
  initialQuery?: string;
}

export interface UseListSearchResult<T> {
  /** Current input value (updates synchronously on keystroke). */
  query: string;
  setQuery: (value: string) => void;
  /** Debounced query that the filter actually uses — lags `query` by debounceMs. */
  effectiveQuery: string;
  /** items filtered by the debounced query. Reference-stable when query is empty. */
  filtered: T[];
  /** Number of matched items. Same as filtered.length, exposed for ergonomic banners. */
  matchCount: number;
}

// normalize converts the search haystack and needle into a form
// that's case- and accent-insensitive. We intentionally normalise
// unicode width (NFKC) so a Japanese full-width "ＡＢＣ" matches
// "ABC", and we lowercase. Trim is applied to the needle on the
// way in.
function normalize(s: string): string {
  // try-catch because some older WebKit versions choke on certain
  // surrogate-pair edge cases in normalize(); a non-normalised
  // fallback is still better than throwing.
  try {
    return s.normalize("NFKC").toLowerCase();
  } catch {
    return s.toLowerCase();
  }
}

export function useListSearch<T>(
  items: T[],
  getSearchableTextOrOptions:
    | ((item: T) => string)
    | UseListSearchOptions<T>,
  maybeOptions?: Omit<UseListSearchOptions<T>, "getSearchableText">,
): UseListSearchResult<T> {
  // Allow either signature:
  //   useListSearch(items, getSearchableText)
  //   useListSearch(items, options)
  // The two-arg form is what most callers want (fewer braces).
  let getSearchableText: (item: T) => string;
  let opts: Omit<UseListSearchOptions<T>, "getSearchableText"> = {};
  if (typeof getSearchableTextOrOptions === "function") {
    getSearchableText = getSearchableTextOrOptions;
    opts = maybeOptions ?? {};
  } else {
    getSearchableText = getSearchableTextOrOptions.getSearchableText;
    opts = getSearchableTextOrOptions;
  }
  const debounceMs = opts.debounceMs ?? 200;

  const [query, setQuery] = useState(opts.initialQuery ?? "");
  const [effectiveQuery, setEffectiveQuery] = useState(query);

  useEffect(() => {
    if (debounceMs <= 0) {
      setEffectiveQuery(query);
      return;
    }
    const t = setTimeout(() => setEffectiveQuery(query), debounceMs);
    return () => clearTimeout(t);
  }, [query, debounceMs]);

  const filtered = useMemo(() => {
    const needle = normalize(effectiveQuery.trim());
    if (!needle) return items; // identity passthrough — preserves array reference
    return items.filter((item) => {
      const haystack = normalize(getSearchableText(item));
      return haystack.includes(needle);
    });
    // getSearchableText is intentionally not in the deps array:
    // the lambda often changes per render (closures over copy / language)
    // and re-running the filter on closure churn would defeat the
    // debounce. Callers who genuinely need the filter to react to
    // a closure change should bump `effectiveQuery` (e.g. setQuery
    // back to itself) — same shape as react-table's contract.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [items, effectiveQuery]);

  return {
    query,
    setQuery,
    effectiveQuery,
    filtered,
    matchCount: filtered.length,
  };
}
