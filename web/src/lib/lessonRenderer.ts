/**
 * lessonRenderer.ts — frontend half of the structured-i18n contract
 * established by server migration 085.
 *
 * The server sends every attribution lesson as
 *
 *   { templateKey: "attribution.lesson.<kind>[.vN]", payload: {...} }
 *
 * The frontend looks up the matching template in
 * `lessonMessages[locale][templateKey]` (declared in
 * shared/api-client/src/i18n.ts) and interpolates the payload via the
 * placeholder grammar described in i18n.ts:
 *
 *   {field}                → raw value
 *   {field|number}         → Intl.NumberFormat(locale)
 *   {field|number:N}       → fixed digits
 *   {field|percent}        → ratio 0..1 → "20%" (locale-formatted)
 *   {field|percent:N}      → ratio 0..1 with N fraction digits
 *   {field|signed:N}       → "+1240.50" / "-1240.50"
 *   {field|signed_pct}     → "+8.0%" / "-4.0%" from a ratio
 *   {field|date}           → already-ISO yyyy-mm-dd, no transformation
 *   {field|plural:a:b}     → singular/plural toggle (uses Intl.PluralRules)
 *
 * Why this is in `web/src/lib/` and not `shared/api-client/`:
 *   * The grammar is implementation detail of the renderer; it doesn't
 *     belong in the type-safe data layer.
 *   * The Android port has its own renderer that consumes the same
 *     dictionary — keeping the two implementations in sync is easier
 *     than sharing a piece of business code that drags `Intl` polyfill
 *     concerns onto the mobile bundle.
 *
 * Fallback policy:
 *   * Missing template key for the locale → English fallback.
 *   * Missing in English too → return undefined, caller renders the
 *     server-supplied `content` field instead. Logged to console.warn
 *     in dev so we notice unmapped keys early.
 *   * Missing payload field a placeholder references → renders the raw
 *     "{field}" so the bug is visible (silent "" would be worse).
 */

import { lessonMessages, type LocaleId, type LessonTemplate } from "@fundai/api-client";

// ---------------------------------------------------------------------------
// Public surface
// ---------------------------------------------------------------------------

export interface RenderedLesson {
  title: string;
  body: string;
}

export interface RenderLessonOptions {
  /** Override for tests — defaults to the global console. */
  warn?: (msg: string, info?: unknown) => void;
}

/**
 * renderLesson takes the structured payload the server emitted and
 * returns the localised { title, body } for the caller's locale.
 * Returns undefined for legacy rows (no templateKey) — the caller
 * is expected to fall back to memories.content in that case.
 */
export function renderLesson(
  locale: LocaleId,
  templateKey: string | undefined | null,
  payload: Record<string, unknown> | undefined | null,
  opts: RenderLessonOptions = {},
): RenderedLesson | undefined {
  if (!templateKey) {
    return undefined;
  }
  const template = pickTemplate(locale, templateKey);
  if (!template) {
    (opts.warn ?? console.warn)(
      `[lessonRenderer] missing template ${templateKey} for ${locale}`,
    );
    return undefined;
  }
  const data = (payload ?? {}) as Record<string, unknown>;
  return {
    title: interpolate(locale, template.title, data),
    body: interpolate(locale, template.body, data),
  };
}

// ---------------------------------------------------------------------------
// Template lookup with locale fallback
// ---------------------------------------------------------------------------

function pickTemplate(locale: LocaleId, key: string): LessonTemplate | undefined {
  const direct = lessonMessages[locale]?.[key];
  if (direct) return direct;
  // Always fall back through en-US so an experimental zh-CN deploy
  // missing one new template doesn't render a broken UI — we'd rather
  // show English than "{field}" garbage.
  if (locale !== "en-US") {
    const englishFallback = lessonMessages["en-US"]?.[key];
    if (englishFallback) return englishFallback;
  }
  return undefined;
}

// ---------------------------------------------------------------------------
// Placeholder interpolation
// ---------------------------------------------------------------------------

const PLACEHOLDER_RE = /\{([a-zA-Z_][a-zA-Z0-9_]*)(\|([a-zA-Z_]+)(?::([^}]*))?)?\}/g;

function interpolate(
  locale: LocaleId,
  template: string,
  data: Record<string, unknown>,
): string {
  return template.replace(PLACEHOLDER_RE, (_match, field: string, _grp2, fmt: string | undefined, args: string | undefined) => {
    const raw = data[field];
    if (raw === undefined || raw === null) {
      // Placeholder references a field the server didn't send — keep
      // the literal "{field}" in the output so it's obvious in QA.
      return `{${field}}`;
    }
    if (!fmt) {
      return String(raw);
    }
    return formatValue(locale, fmt, args, raw);
  });
}

function formatValue(
  locale: LocaleId,
  fmt: string,
  args: string | undefined,
  raw: unknown,
): string {
  switch (fmt) {
    case "number": {
      const n = toNumber(raw);
      const digits = parseDigitArg(args, undefined);
      return new Intl.NumberFormat(locale, fractionOpts(digits)).format(n);
    }
    case "percent": {
      const ratio = toNumber(raw);
      const digits = parseDigitArg(args, 0);
      return new Intl.NumberFormat(locale, {
        style: "percent",
        minimumFractionDigits: digits,
        maximumFractionDigits: digits,
      }).format(ratio);
    }
    case "signed": {
      const n = toNumber(raw);
      const digits = parseDigitArg(args, 2);
      return new Intl.NumberFormat(locale, {
        signDisplay: "always",
        minimumFractionDigits: digits,
        maximumFractionDigits: digits,
      }).format(n);
    }
    case "signed_pct": {
      const ratio = toNumber(raw);
      const digits = parseDigitArg(args, 1);
      return new Intl.NumberFormat(locale, {
        style: "percent",
        signDisplay: "always",
        minimumFractionDigits: digits,
        maximumFractionDigits: digits,
      }).format(ratio);
    }
    case "date":
      // Server already shipped yyyy-mm-dd. We leave as-is so the same
      // string survives a round-trip through CI snapshot tests. A
      // future locale-specific date format can be added here without
      // touching the dictionary entries.
      return String(raw);
    case "plural": {
      // {field|plural:singular:plural} — picks one based on
      // Intl.PluralRules. We accept exactly two arms; richer rules
      // (zero/few/many) would need a different placeholder grammar.
      const parts = (args ?? "").split(":");
      if (parts.length !== 2) {
        return String(raw);
      }
      const [singular, plural] = parts;
      const n = toNumber(raw);
      const rule = new Intl.PluralRules(locale).select(n);
      return rule === "one" ? singular : plural;
    }
    default:
      // Unknown format token. Surface the literal so QA spots it.
      return `{${fmt}:${String(raw)}}`;
  }
}

// ---------------------------------------------------------------------------
// Coercion helpers
// ---------------------------------------------------------------------------

function toNumber(raw: unknown): number {
  if (typeof raw === "number") return raw;
  if (typeof raw === "string") {
    const parsed = Number(raw);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

function parseDigitArg(args: string | undefined, fallback: number | undefined): number | undefined {
  if (args === undefined || args === "") return fallback;
  const n = Number(args);
  return Number.isFinite(n) && n >= 0 ? n : fallback;
}

function fractionOpts(digits: number | undefined): Intl.NumberFormatOptions {
  if (digits === undefined) {
    return { maximumFractionDigits: 6 };
  }
  return {
    minimumFractionDigits: digits,
    maximumFractionDigits: digits,
  };
}
