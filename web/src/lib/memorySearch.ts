// memorySearch.ts — token-based relevance ranking for the
// in-memory subset of memory entries available to the
// MemoryCenter UI.
//
// WHY THIS EXISTS
// ---------------
// MemoryCenter today does a single substring match over
// title+content of the CURRENT layer's loaded entries. That has
// three failure modes:
//
//   1. "Multi-term queries" — searching "moat AAPL" only
//      surfaces entries that contain that literal sequence; the
//      user actually wants entries that mention BOTH terms.
//   2. "Single layer" — search ignores the other 4 memory
//      layers (daily, weekly, monthly, evolution). A user
//      searching for "drawdown" should see hits across all of
//      them, then drill in.
//   3. "No relevance order" — substring matches are presented
//      in load order, not by how well they match. A long
//      content with one passing mention outranks a tightly
//      relevant short note.
//
// True embeddings-based semantic search needs a server-side
// vector store + index, which is a multi-week change. Until
// then we can ship a clear UX win with classic IR techniques:
//
//   - tokenize the query into terms,
//   - score each entry with a BM25-lite formula over its
//     title / content / tags, with a small title-weight bonus,
//   - threshold at "any term matches" so we don't drown out
//     short content with a partial match,
//   - highlight every matched term in the rendered snippet,
//   - sort descending by score.
//
// "Semantic" is the EXPECTED progression — the API contract
// here (rankMemoryEntries → ScoredMemoryEntry[]) is identical
// to what an embeddings backend would return, so swapping out
// the implementation later doesn't disturb the caller.
//
// IMPLEMENTATION NOTES
// --------------------
//   - Tokenization is naive: split on whitespace + punctuation,
//     lowercase, drop tokens of length 1, drop a tiny English
//     + Chinese stopword set. Good enough for a finance product
//     where the meaningful queries are tickers, themes, and
//     short phrases. Doesn't try to do CJK word segmentation —
//     the BM25 character-level logic for Chinese is a known
//     follow-up (consider https://github.com/yanyiwu/nodejieba
//     or just let server-side handle it once we ship a real
//     index).
//   - BM25 parameters use the Lucene defaults (k1 = 1.2, b =
//     0.75) which Wikipedia documents as a sensible baseline.
//   - We compute IDF over the in-memory candidate set rather
//     than the global corpus — this is "fine but slightly
//     biased toward terms that ARE rare in what's loaded".
//     Acceptable since the alternative requires the server.
//   - tagBoost = 1.5 — tags are short, deliberate annotations,
//     so a tag match should outrank a content keyword match.
//   - titleBoost = 1.3 — titles are also short and deliberate.
//   - We return the original entry plus a `score`, the matched
//     tokens, and a snippet centered on the highest-scoring
//     match within the content.

const EN_STOPWORDS = new Set([
  "the",
  "a",
  "an",
  "and",
  "or",
  "of",
  "to",
  "in",
  "on",
  "for",
  "is",
  "with",
  "by",
]);

const ZH_STOPWORDS = new Set(["的", "是", "了", "在", "和", "与", "及", "或"]);

const TOKEN_SPLIT = /[\s\.,;:!?\-_()[\]{}'"<>/\\\u3000-\u3002\uFF01-\uFF1F]+/u;

export interface MemorySearchableEntry {
  id: string;
  title?: string;
  content: string;
  tags?: string[];
}

export interface ScoredMemoryEntry<T extends MemorySearchableEntry> {
  entry: T;
  score: number;
  matchedTokens: string[];
  /** Highlighted snippet (raw text — caller renders <mark>). */
  snippet: string;
  /** Layer-aware grouping id — caller can supply via {layer, fundId}. */
  meta?: Record<string, string>;
}

export interface RankMemoryEntriesOptions<T extends MemorySearchableEntry> {
  /** When set, an annotation attached to each result for caller use. */
  metaFor?: (entry: T) => Record<string, string>;
  /** Override snippet length. Defaults to ~140 chars. */
  snippetLen?: number;
}

/**
 * Tokenize input — lowercase, split, drop stopwords + short
 * tokens. Exported for tests and for the highlighter.
 */
export function tokenize(text: string): string[] {
  return text
    .toLowerCase()
    .split(TOKEN_SPLIT)
    .filter((t) => t.length > 1 && !EN_STOPWORDS.has(t) && !ZH_STOPWORDS.has(t));
}

/**
 * Rank a list of memory entries against a free-text query
 * using BM25-lite with title/tag boosts.
 *
 * Returns entries sorted descending by score. Entries with
 * score === 0 (no token match) are excluded.
 */
export function rankMemoryEntries<T extends MemorySearchableEntry>(
  entries: readonly T[],
  query: string,
  opts: RankMemoryEntriesOptions<T> = {},
): ScoredMemoryEntry<T>[] {
  const queryTokens = tokenize(query);
  if (queryTokens.length === 0 || entries.length === 0) {
    return [];
  }

  // Doc frequency for IDF.
  const docFreq = new Map<string, number>();
  const docs: { entry: T; tokens: string[]; tagTokens: string[]; titleTokens: string[] }[] = [];
  let avgDocLen = 0;
  for (const entry of entries) {
    const tokens = tokenize(entry.content);
    const titleTokens = tokenize(entry.title ?? "");
    const tagTokens = tokenize((entry.tags ?? []).join(" "));
    docs.push({ entry, tokens, tagTokens, titleTokens });
    avgDocLen += tokens.length;
    const seen = new Set<string>([...tokens, ...titleTokens, ...tagTokens]);
    seen.forEach((t) => docFreq.set(t, (docFreq.get(t) ?? 0) + 1));
  }
  avgDocLen = docs.length > 0 ? avgDocLen / docs.length : 1;

  const N = docs.length;
  const k1 = 1.2;
  const b = 0.75;
  const titleBoost = 1.3;
  const tagBoost = 1.5;
  const snippetLen = opts.snippetLen ?? 140;

  const scored: ScoredMemoryEntry<T>[] = [];
  for (const doc of docs) {
    let score = 0;
    const matched = new Set<string>();
    for (const qt of queryTokens) {
      const tf = countOccurrences(doc.tokens, qt);
      const titleTf = countOccurrences(doc.titleTokens, qt);
      const tagTf = countOccurrences(doc.tagTokens, qt);
      if (tf + titleTf + tagTf === 0) continue;
      matched.add(qt);
      const df = docFreq.get(qt) ?? 0;
      const idf = Math.log(1 + (N - df + 0.5) / (df + 0.5));
      const docLenNorm = doc.tokens.length / avgDocLen;
      const bm25 = (idf * (tf * (k1 + 1))) / (tf + k1 * (1 - b + b * docLenNorm));
      score += bm25;
      if (titleTf > 0) score += titleBoost * idf;
      if (tagTf > 0) score += tagBoost * idf;
    }
    if (score === 0) continue;
    const snippet = buildSnippet(doc.entry.content, queryTokens, snippetLen);
    const result: ScoredMemoryEntry<T> = {
      entry: doc.entry,
      score,
      matchedTokens: Array.from(matched),
      snippet,
    };
    if (opts.metaFor) result.meta = opts.metaFor(doc.entry);
    scored.push(result);
  }

  scored.sort((a, b) => b.score - a.score);
  return scored;
}

/**
 * Build a snippet centered on the first matched query token,
 * with up to `len` chars of context. If no token is found in
 * content, returns the first `len` chars of content.
 */
function buildSnippet(content: string, queryTokens: readonly string[], len: number): string {
  const lowered = content.toLowerCase();
  let bestIdx = -1;
  for (const t of queryTokens) {
    const idx = lowered.indexOf(t);
    if (idx >= 0 && (bestIdx === -1 || idx < bestIdx)) {
      bestIdx = idx;
    }
  }
  if (bestIdx === -1) {
    return content.slice(0, len);
  }
  const half = Math.floor(len / 2);
  const start = Math.max(0, bestIdx - half);
  const end = Math.min(content.length, bestIdx + half);
  return (start > 0 ? "…" : "") + content.slice(start, end) + (end < content.length ? "…" : "");
}

function countOccurrences(tokens: readonly string[], target: string): number {
  let n = 0;
  for (const t of tokens) {
    if (t === target) n++;
  }
  return n;
}

/**
 * Wrap matched tokens in HTML <mark> for display. Caller is
 * responsible for setting innerHTML or using
 * dangerouslySetInnerHTML — we don't escape, so callers MUST
 * ensure the input came from trusted sources (memory entries
 * are user-generated but already rendered as text in the
 * existing UI).
 */
export function highlightTokens(text: string, tokens: readonly string[]): string {
  if (tokens.length === 0 || !text) return text;
  // Sort longest first so "drawdown" highlights before "down"
  // when both are query tokens.
  const sorted = [...tokens].sort((a, b) => b.length - a.length);
  const escaped = sorted.map((t) => t.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"));
  const re = new RegExp(`(${escaped.join("|")})`, "gi");
  return text.replace(re, "<mark>$1</mark>");
}
