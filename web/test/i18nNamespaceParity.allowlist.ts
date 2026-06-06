/**
 * web/test/i18nNamespaceParity.allowlist.ts — W13-5
 *
 * Curated waivers for the "suspicious-identical" guard in
 * i18nNamespaceParity.test.ts.
 *
 * Why this exists at all
 * ----------------------
 * The W12-1 guard catches the most common i18n drift: a translator
 * pastes English copy into the zh-CN file as a placeholder and
 * forgets to translate. The detector is heuristic — it treats any
 * en === zh leaf that "looks like an English phrase" as a fail.
 *
 * Across 13 namespaces today, that heuristic produces ZERO false
 * positives. But it's a matter of when, not if, a legitimately
 * identical-across-locales string trips it. Examples that may
 * arrive over time:
 *
 *   - Product / brand names that *should* stay English in zh-CN
 *     (e.g. "Bloomberg Terminal", "OpenAI Embeddings").
 *   - Domain idioms that translators agreed not to localize
 *     (e.g. "P&L attribution" — "盈亏归因" was rejected as
 *     unidiomatic in PM-internal copy).
 *   - Technical labels that match an external API contract
 *     (e.g. "FIX message" tag names).
 *
 * Design choices
 * --------------
 * 1. Empty by default. We intentionally check in this file with
 *    NO entries. A waiver is a code-review event, not a routine
 *    additive change — every entry must have a `reason` and a
 *    PR-author signoff.
 *
 * 2. (namespace, path) keying. We don't waive at the *value*
 *    level because two unrelated strings might happen to share a
 *    word ("Bloomberg" appearing in different namespaces should
 *    each be reviewed separately). Pinning by path forces the
 *    reviewer to acknowledge the specific use site.
 *
 * 3. No regex / glob. Path is a literal dotted path. If the same
 *    waiver applies to many keys, list them. The redundancy is
 *    the point — every entry is a deliberate "yes, this one is
 *    fine".
 *
 * How to add an entry (process)
 * -----------------------------
 *   - Open the failing parity-test output. It will say:
 *     `path = "<value>"` for each suspicious row.
 *   - Confirm the value really should be identical across
 *     locales (talk to the translation owner if unsure).
 *   - Append an entry below with a one-line reason.
 *   - Reference the PR / discussion in the reason if relevant.
 */

export interface AllowlistEntry {
  /** Namespace file basename, e.g. "decisionCenter". */
  namespace: string;
  /** Dotted path to the leaf, e.g. "columns.amount". */
  path: string;
  /** One-line justification. Required. */
  reason: string;
}

/**
 * The actual allowlist. Add entries with care.
 *
 * Empty today. Do not "preseed" — we'd rather see a real failure
 * and a real review.
 */
export const ALLOWLIST: AllowlistEntry[] = [];
