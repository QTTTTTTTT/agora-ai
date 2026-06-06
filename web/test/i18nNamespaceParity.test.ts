/**
 * web/test/i18nNamespaceParity.test.ts — W6-3 CI guard.
 *
 * Run via:    npm run test:i18n-keys
 *
 * Why hand-rolled and not Vitest/Jest:
 *   * The web package already runs lessonRenderer.test.ts the same way
 *     (Node --experimental-strip-types). Adding a unit-test framework
 *     for one parity check would expand the dev-dep set considerably.
 *
 * What this file pins:
 *   1. KEY COMPLETENESS  — every key present in en-US must be present
 *                          in zh-CN, and vice versa. Catches drift like
 *                          "added an English key, forgot the Chinese one".
 *   2. PLACEHOLDER PARITY — each (namespace, key) must reference the
 *                           exact same set of `{placeholders}` in both
 *                           locales. Catches drift like "translation
 *                           lost the {amount} interpolation so the value
 *                           silently disappears in production".
 *   3. SHAPE PARITY       — if en-US has a string at `path.to.key`, zh-CN
 *                           must have a string there too (not a nested
 *                           object). Catches drift like "added a sub-tree
 *                           on one side, leaf on the other" which would
 *                           cause a TypeError in `t()` callers.
 *   4. NON-EMPTY GUARD    — both locales must have a non-empty value at
 *                           every key. Catches drift like "translator
 *                           cleared a string thinking they'd come back
 *                           to it" — i18next would happily render a
 *                           blank UI element in production.
 *   5. SUSPICIOUS-IDENTICAL — for non-trivial English-looking strings
 *                              (multi-word OR ≥ 12 chars, with letters,
 *                              not URLs, not pure placeholder shells),
 *                              the en-US and zh-CN values MUST differ.
 *                              Catches the very common drift of
 *                              "copy-pasted English into the Chinese
 *                              file as a placeholder, then forgot to
 *                              translate". Acronyms / brand names /
 *                              `EN`/`ZH`/`—` legitimately match across
 *                              locales — see isSuspiciousIdentical for
 *                              the precise allow-list.
 *
 * Allowlist (W13-5)
 * -----------------
 * For the rare legitimate identical-across-locales string (e.g. a
 * brand name we've decided to keep English in zh-CN), waivers
 * live in `i18nNamespaceParity.allowlist.ts`. Each entry pins
 * (namespace, path, reason). A stale waiver — pointing at a key
 * that no longer exists — fails this test by design, so the
 * allowlist can't accidentally mask future drift.
 *
 * Which namespaces are checked:
 *   The script walks `web/src/i18n/locales/en-US/*.ts` at runtime so a
 *   newly-added namespace is automatically covered — no allow-list to
 *   forget to update. The complement — every en-US namespace must have
 *   a zh-CN twin file — is asserted as the first check below.
 */

import { readdirSync } from "node:fs";
import { fileURLToPath, pathToFileURL } from "node:url";
import { dirname, join, resolve } from "node:path";

import { ALLOWLIST } from "./i18nNamespaceParity.allowlist.ts";

// Index the allowlist as a Set for O(1) lookup. Path collisions
// inside one namespace fold to a single entry — that's fine, the
// set semantics are a "is this exempt?" query, not a count.
const ALLOWLIST_INDEX: Set<string> = new Set(
  ALLOWLIST.map((e) => `${e.namespace}::${e.path}`),
);

// ---------------------------------------------------------------------------
// Tiny assert harness (mirrors lessonRenderer.test.ts)
// ---------------------------------------------------------------------------

let failures = 0;
let total = 0;

function check(name: string, fn: () => void | Promise<void>): Promise<void> {
  total += 1;
  return Promise.resolve()
    .then(() => fn())
    .then(() => {
      console.log(`  PASS  ${name}`);
    })
    .catch((e: unknown) => {
      failures += 1;
      const msg = e instanceof Error ? e.message : String(e);
      console.error(`  FAIL  ${name}\n        ${msg}`);
    });
}

// ---------------------------------------------------------------------------
// Locale discovery
// ---------------------------------------------------------------------------

const __filename = fileURLToPath(import.meta.url);
const __dirname = dirname(__filename);
const LOCALES_DIR = resolve(__dirname, "../src/i18n/locales");
const EN = "en-US";
const ZH = "zh-CN";

function listNamespaceFiles(localeDir: string): string[] {
  return readdirSync(localeDir)
    .filter((f) => f.endsWith(".ts"))
    .map((f) => f.replace(/\.ts$/, ""))
    .sort();
}

// ---------------------------------------------------------------------------
// Bundle loading + flattening
// ---------------------------------------------------------------------------

type Bundle = Record<string, unknown>;

async function loadNamespace(locale: string, ns: string): Promise<Bundle> {
  const filePath = join(LOCALES_DIR, locale, `${ns}.ts`);
  // pathToFileURL handles macOS / Windows path separators uniformly.
  const mod = (await import(pathToFileURL(filePath).href)) as {
    default: Bundle;
  };
  if (!mod.default || typeof mod.default !== "object") {
    throw new Error(`namespace ${locale}/${ns} has no default export object`);
  }
  return mod.default;
}

interface FlatEntry {
  path: string;
  value: string;
}

/**
 * flatten walks the bundle tree and returns an array of leaf entries
 * (string values) keyed by dotted path. Arrays are flattened with
 * numeric indices so `actions: ["a","b"]` becomes `actions.0` /
 * `actions.1` — that lets us catch list-length drift between locales
 * the same way we catch missing object keys.
 */
function flatten(b: Bundle, prefix = ""): FlatEntry[] {
  const out: FlatEntry[] = [];
  for (const [k, v] of Object.entries(b)) {
    const path = prefix ? `${prefix}.${k}` : k;
    if (v == null) {
      // null / undefined isn't expected in our locale files; treat
      // as a leaf so the parity check flags it.
      out.push({ path, value: "" });
      continue;
    }
    if (typeof v === "string") {
      out.push({ path, value: v });
      continue;
    }
    if (typeof v === "number" || typeof v === "boolean") {
      out.push({ path, value: String(v) });
      continue;
    }
    if (Array.isArray(v)) {
      v.forEach((item, idx) => {
        if (typeof item === "string") {
          out.push({ path: `${path}.${idx}`, value: item });
        } else if (item && typeof item === "object") {
          out.push(...flatten(item as Bundle, `${path}.${idx}`));
        }
      });
      continue;
    }
    if (typeof v === "object") {
      out.push(...flatten(v as Bundle, path));
    }
  }
  return out;
}

const PLACEHOLDER_RE = /\{\{?\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}?\}/g;
function placeholderSet(s: string): string {
  // We only compare the set of *names*, not formatters. i18next allows
  // both `{{name}}` and `{name}` style — we accept either so a future
  // shift between the two doesn't false-positive this guard.
  const names = new Set<string>();
  // Local regex so the global state isn't shared across calls (would
  // intermittently skip the first match otherwise).
  const re = new RegExp(PLACEHOLDER_RE.source, "g");
  let m: RegExpExecArray | null;
  while ((m = re.exec(s)) !== null) {
    names.add(m[1]);
  }
  return Array.from(names).sort().join("|");
}

// isEmptyValue catches missing translations that are still "present"
// from a key-set perspective — empty string, all whitespace, or a
// stray Unicode space. We do NOT trim before storing in the bundle
// (we trust files), so this is purely the leaf-level guard.
function isEmptyValue(s: string): boolean {
  return s.trim().length === 0;
}

// isSuspiciousIdentical flags the most common copy-paste drift:
// translator dropped the English string into the Chinese file
// as a placeholder and forgot to translate. We have to be careful
// here — many legitimate values *are* identical across locales
// (acronyms, symbols, single English brand names, single-letter
// labels, `—`, `EN` / `ZH`). The allow rules below mean a value
// gets flagged ONLY when it looks like real English copy.
function isSuspiciousIdentical(en: string, zh: string): boolean {
  if (en !== zh) return false;
  if (en.length < 8) return false;
  // URLs and email-shaped strings stay identical across locales
  // by design.
  if (/^https?:\/\//i.test(en)) return false;
  if (/^[\w.+-]+@[\w.-]+\.[a-z]{2,}$/i.test(en)) return false;
  // If the string is ENTIRELY placeholder + whitespace + punctuation,
  // it can't be translated meaningfully. Example: "{{n}}%" — same in
  // both locales is correct.
  const withoutPlaceholders = en.replace(
    new RegExp(PLACEHOLDER_RE.source, "g"),
    "",
  );
  const lettersOnly = withoutPlaceholders.replace(/[\s\W_0-9]/g, "");
  if (lettersOnly.length < 4) return false;
  // Must contain at least 2 consecutive ASCII letters somewhere
  // outside any placeholder — symbol-only strings ("—", "P&L") miss
  // this check.
  if (!/[a-zA-Z]{2,}/.test(withoutPlaceholders)) return false;
  // Final hurdle: must look like a *phrase*, not just one word /
  // brand name. Either contains a space, or is long enough that a
  // single word is unusual (≥ 12 chars).
  if (!/ /.test(en) && en.length < 12) return false;
  return true;
}

// ---------------------------------------------------------------------------
// Run the checks
// ---------------------------------------------------------------------------

async function main(): Promise<void> {
  console.log("[1] namespace file parity (en-US ↔ zh-CN)");
  const enFiles = listNamespaceFiles(join(LOCALES_DIR, EN));
  const zhFiles = listNamespaceFiles(join(LOCALES_DIR, ZH));
  await check("each en-US namespace file has a zh-CN twin (and vice versa)", () => {
    const enSet = new Set(enFiles);
    const zhSet = new Set(zhFiles);
    const missingInZh = enFiles.filter((n) => !zhSet.has(n));
    const missingInEn = zhFiles.filter((n) => !enSet.has(n));
    const drift: string[] = [];
    if (missingInZh.length) drift.push(`zh-CN missing: ${missingInZh.join(", ")}`);
    if (missingInEn.length) drift.push(`en-US missing: ${missingInEn.join(", ")}`);
    if (drift.length) {
      throw new Error(drift.join("\n  - "));
    }
  });

  // Use intersection so a drifted file doesn't double-fail the rest.
  const namespaces = enFiles.filter((n) => zhFiles.includes(n));
  console.log(`[2] key + placeholder parity across ${namespaces.length} namespaces`);

  for (const ns of namespaces) {
    const en = await loadNamespace(EN, ns);
    const zh = await loadNamespace(ZH, ns);
    const enFlat = flatten(en);
    const zhFlat = flatten(zh);
    const enMap = new Map(enFlat.map((e) => [e.path, e.value]));
    const zhMap = new Map(zhFlat.map((e) => [e.path, e.value]));

    await check(`${ns}: every en-US key exists in zh-CN (and vice versa)`, () => {
      const onlyInEn = enFlat.map((e) => e.path).filter((p) => !zhMap.has(p));
      const onlyInZh = zhFlat.map((e) => e.path).filter((p) => !enMap.has(p));
      const drift: string[] = [];
      if (onlyInEn.length) drift.push(`only in en-US:\n    - ${onlyInEn.join("\n    - ")}`);
      if (onlyInZh.length) drift.push(`only in zh-CN:\n    - ${onlyInZh.join("\n    - ")}`);
      if (drift.length) {
        throw new Error(drift.join("\n  "));
      }
    });

    await check(`${ns}: placeholder field sets match per key`, () => {
      const drifted: string[] = [];
      for (const [path, enValue] of enMap) {
        const zhValue = zhMap.get(path);
        if (zhValue == null) continue; // already reported above
        const enSig = placeholderSet(enValue);
        const zhSig = placeholderSet(zhValue);
        if (enSig !== zhSig) {
          drifted.push(
            `${path}: en=[${enSig || "∅"}] zh=[${zhSig || "∅"}]`,
          );
        }
      }
      if (drifted.length > 0) {
        throw new Error(`placeholder drift:\n    - ${drifted.join("\n    - ")}`);
      }
    });

    await check(`${ns}: no empty / whitespace-only translations`, () => {
      const offenders: string[] = [];
      for (const { path, value } of enFlat) {
        if (isEmptyValue(value)) offenders.push(`en-US ${path}`);
      }
      for (const { path, value } of zhFlat) {
        if (isEmptyValue(value)) offenders.push(`zh-CN ${path}`);
      }
      if (offenders.length > 0) {
        throw new Error(
          `empty translations would render a blank UI:\n    - ${offenders.join("\n    - ")}`,
        );
      }
    });

    await check(`${ns}: zh-CN values not copy-pasted from en-US`, () => {
      const offenders: string[] = [];
      for (const [path, enValue] of enMap) {
        const zhValue = zhMap.get(path);
        if (zhValue == null) continue;
        if (!isSuspiciousIdentical(enValue, zhValue)) continue;
        // Allowlist bypass — each waiver requires a (namespace,
        // path) match plus a reason in the allowlist file. The
        // reason itself isn't checked here; the file's review
        // gate is what enforces "every waiver was justified".
        if (ALLOWLIST_INDEX.has(`${ns}::${path}`)) continue;
        // Trim long values for legibility in the failure message
        // — the path is enough to find the offending file.
        const preview =
          enValue.length > 60 ? `${enValue.slice(0, 57)}…` : enValue;
        offenders.push(`${path} = "${preview}"`);
      }
      if (offenders.length > 0) {
        throw new Error(
          `looks like English copy was pasted into the zh-CN file:\n    - ${offenders.join(
            "\n    - ",
          )}\n  (if intentional, add a waiver to test/i18nNamespaceParity.allowlist.ts with a reason)`,
        );
      }
    });
  }

  // ---------------------------------------------------------------------------
  // [3] Allowlist hygiene — every waiver in the file must point at
  // a key that actually exists. Stale entries silently mask future
  // drift, defeating the whole point of this guard.
  // ---------------------------------------------------------------------------
  if (ALLOWLIST.length > 0) {
    console.log(`[3] allowlist hygiene (${ALLOWLIST.length} entries)`);
    await check("every allowlist entry points at an existing key", async () => {
      const stale: string[] = [];
      for (const entry of ALLOWLIST) {
        if (!entry.reason.trim()) {
          stale.push(`${entry.namespace}::${entry.path} — empty reason`);
          continue;
        }
        try {
          const en = await loadNamespace(EN, entry.namespace);
          const zh = await loadNamespace(ZH, entry.namespace);
          const enHas = flatten(en).some((e) => e.path === entry.path);
          const zhHas = flatten(zh).some((e) => e.path === entry.path);
          if (!enHas || !zhHas) {
            stale.push(
              `${entry.namespace}::${entry.path} — path not present in both locales (en=${enHas} zh=${zhHas})`,
            );
          }
        } catch (e: unknown) {
          stale.push(
            `${entry.namespace}::${entry.path} — namespace load failed: ${
              e instanceof Error ? e.message : String(e)
            }`,
          );
        }
      }
      if (stale.length > 0) {
        throw new Error(
          `stale allowlist entries (drop or fix):\n    - ${stale.join("\n    - ")}`,
        );
      }
    });
  }

  // ---------------------------------------------------------------------------
  // Summary
  // ---------------------------------------------------------------------------
  if (failures > 0) {
    console.error(`\n${failures} of ${total} cases FAILED`);
    process.exit(1);
  }
  console.log(`\n${total} cases passed`);
}

main().catch((err: unknown) => {
  const msg = err instanceof Error ? err.stack ?? err.message : String(err);
  console.error(`unhandled error:\n${msg}`);
  process.exit(1);
});
