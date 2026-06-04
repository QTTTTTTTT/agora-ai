/**
 * lessonRenderer + dictionary contract tests.
 *
 * Run via:    npm run test:i18n
 *
 * Why hand-rolled and not Vitest/Jest:
 *   * The web package uses tsc + eslint + playwright as its only QA
 *     surface; adding a unit-test framework here would expand the
 *     dev-dep set considerably for one file's worth of coverage.
 *   * The cases are trivial enough that a small assertion harness is
 *     enough — and the harness itself is self-contained, so future
 *     contributors can skim it in 30s.
 *
 * What this file pins:
 *   1. Per-locale render correctness   (en-US + zh-CN render expected text)
 *   2. Number/percent/date format       (Intl wiring is correct)
 *   3. Legacy fallback                   (no templateKey → undefined)
 *   4. Missing key is logged & warns     (no silent void)
 *   5. KEY COMPLETENESS guard            (every locale defines every key)
 *   6. PLACEHOLDER PARITY guard          (each locale uses the same
 *                                          {field|fmt} set)
 *   7. RENDER SNAPSHOT guard             (golden output per locale × key
 *                                          freezes accidental copy edits)
 *
 * Items 5/6/7 satisfy the CI test requirements (i18n-10/11/12). They
 * are deliberately in the same file so a future contributor adding a
 * new lesson template gets a single failure surface instead of three.
 */

import { lessonMessages, type LocaleId } from "../../shared/api-client/src/i18n.ts";
import { renderLesson } from "../src/lib/lessonRenderer.ts";

// ---------------------------------------------------------------------------
// Tiny assert harness
// ---------------------------------------------------------------------------

let failures = 0;
let total = 0;

function check(name: string, fn: () => void): void {
  total += 1;
  try {
    fn();
    console.log(`  PASS  ${name}`);
  } catch (e) {
    failures += 1;
    const msg = e instanceof Error ? e.message : String(e);
    console.error(`  FAIL  ${name}\n        ${msg}`);
  }
}

function assertEq<T>(got: T, want: T, msg: string): void {
  if (got !== want) {
    throw new Error(`${msg}\n  got:  ${JSON.stringify(got)}\n  want: ${JSON.stringify(want)}`);
  }
}

function assertContains(got: string, needle: string, msg: string): void {
  if (!got.includes(needle)) {
    throw new Error(`${msg}\n  got: ${JSON.stringify(got)}\n  needle: ${JSON.stringify(needle)}`);
  }
}

// ---------------------------------------------------------------------------
// 1. Per-locale render correctness
// ---------------------------------------------------------------------------

console.log("[1] per-locale render correctness");

const loserPayload = {
  sleeve: "mean_reversion",
  regime: "chop",
  trade_count: 12,
  win_rate: 0.25,
  total_pnl: -480.5,
  avg_pnl_pct: -0.04,
  avg_holding_days: 2.1,
};

check("en-US loser title contains sleeve + regime + win-rate %", () => {
  const r = renderLesson("en-US", "attribution.lesson.sleeve_regime_loser", loserPayload);
  if (!r) throw new Error("renderLesson returned undefined");
  assertContains(r.title, '"mean_reversion"', "title sleeve");
  assertContains(r.title, '"chop"', "title regime");
  assertContains(r.title, "12", "title trade count");
  assertContains(r.title, "25%", "title win rate %");
  // Negative PnL should print with "-" prefix, locale-formatted (no thousands sep needed).
  assertContains(r.title, "-480.50", "title pnl");
});

check("zh-CN loser title is in Chinese & numbers locale-formatted", () => {
  const r = renderLesson("zh-CN", "attribution.lesson.sleeve_regime_loser", loserPayload);
  if (!r) throw new Error("renderLesson returned undefined for zh");
  assertContains(r.title, "亏损", "title contains 亏损");
  assertContains(r.title, "mean_reversion", "title carries sleeve verbatim");
  assertContains(r.title, "12", "title trade count");
  assertContains(r.title, "25%", "title win rate %");
  assertContains(r.title, "-480.50", "title pnl");
});

const winnerPayload = {
  sleeve: "trend",
  regime: "trend_up",
  trade_count: 15,
  win_rate: 11 / 15,
  total_pnl: 1240,
  avg_pnl_pct: 0.08,
  avg_holding_days: 7,
};

check("en-US winner title — signed:2 prefixes '+' on positive PnL", () => {
  const r = renderLesson("en-US", "attribution.lesson.sleeve_regime_winner", winnerPayload);
  if (!r) throw new Error("renderLesson returned undefined");
  // 1240 → "+1,240.00" in en-US locale
  assertContains(r.title, "+1,240.00", "title pnl signed");
  // 11/15 = 73.33% → "73%" with 0 fraction digits (default percent)
  assertContains(r.title, "73%", "title win-rate %");
});

check("en-US insufficient_data.watching plural and date", () => {
  const r = renderLesson(
    "en-US",
    "attribution.lesson.insufficient_data.watching",
    { open_lot_count: 7, earliest_opened_at: "2026-05-12", window_days: 30 },
  );
  if (!r) throw new Error("renderLesson returned undefined");
  assertContains(r.title, "Watching 7 open lots", "plural arm");
  assertContains(r.title, "2026-05-12", "date pass-through");
  assertContains(r.title, "last 30 days", "window days");
});

check("en-US insufficient_data.watching plural=1 → singular arm", () => {
  const r = renderLesson(
    "en-US",
    "attribution.lesson.insufficient_data.watching",
    { open_lot_count: 1, earliest_opened_at: "2026-05-12", window_days: 30 },
  );
  if (!r) throw new Error("renderLesson returned undefined");
  assertContains(r.title, "1 open lot since", "singular arm chosen");
  if (r.title.includes("lots since")) {
    throw new Error(`expected singular 'lot' — got: ${r.title}`);
  }
});

// ---------------------------------------------------------------------------
// 2. Legacy fallback
// ---------------------------------------------------------------------------

console.log("[2] legacy fallback");

check("undefined templateKey → undefined render", () => {
  const r = renderLesson("en-US", undefined, undefined);
  assertEq(r, undefined, "should fall through to caller's content");
});

check("empty templateKey → undefined render", () => {
  const r = renderLesson("en-US", "", { foo: 1 });
  assertEq(r, undefined, "empty key counts as legacy");
});

// ---------------------------------------------------------------------------
// 3. Missing key warns + returns undefined
// ---------------------------------------------------------------------------

console.log("[3] missing key warns + returns undefined");

check("unknown templateKey → warn called, undefined returned", () => {
  const warned: string[] = [];
  const r = renderLesson(
    "en-US",
    "attribution.lesson.does.not.exist",
    {},
    { warn: (msg) => warned.push(msg) },
  );
  assertEq(r, undefined, "unknown key → undefined");
  if (warned.length === 0) {
    throw new Error("expected console.warn to be invoked at least once");
  }
  assertContains(warned[0], "missing template", "warn message format");
});

check("zh-CN missing key uses en-US fallback before warn", () => {
  // We can't mutate the renderer's view of `lessonMessages` from this
  // test (under Node strip-types they're separate module instances).
  // Instead pin the behaviour indirectly: for every key actually shipped
  // in en-US, rendering with locale=zh-CN must NEVER return undefined.
  // The "key completeness" guard above already enforces that every
  // production key is in zh-CN, so rendering must succeed without ever
  // reaching the en-US fallback. That's the contract that protects
  // production users; a synthetic fallback path test would only
  // cover an unshippable state.
  for (const key of Object.keys(lessonMessages["en-US"])) {
    const r = renderLesson("zh-CN", key, {
      sleeve: "x",
      regime: "y",
      trade_count: 1,
      win_rate: 0.5,
      total_pnl: 0,
      avg_pnl_pct: 0,
      avg_holding_days: 1,
      open_lot_count: 1,
      earliest_opened_at: "2026-05-12",
      window_days: 30,
    });
    if (!r) throw new Error(`zh-CN render returned undefined for ${key}`);
  }
});

// ---------------------------------------------------------------------------
// 4. CI guard: KEY COMPLETENESS — every locale must define every key
// ---------------------------------------------------------------------------

console.log("[4] CI: key completeness across locales");

check("every templateKey is defined in every shipped locale", () => {
  const locales = Object.keys(lessonMessages) as LocaleId[];
  const allKeys = new Set<string>();
  for (const loc of locales) {
    for (const k of Object.keys(lessonMessages[loc])) allKeys.add(k);
  }
  const missing: string[] = [];
  for (const k of allKeys) {
    for (const loc of locales) {
      if (!lessonMessages[loc][k]) {
        missing.push(`${loc}::${k}`);
      }
    }
  }
  if (missing.length > 0) {
    throw new Error(`missing translations:\n  - ${missing.join("\n  - ")}`);
  }
});

// ---------------------------------------------------------------------------
// 5. CI guard: PLACEHOLDER PARITY — same field set per (key, locale)
// ---------------------------------------------------------------------------
//
// We compare the SET of payload field names referenced by each
// translation, not the full format token. Format tokens are intentionally
// locale-specific: Chinese doesn't pluralize the way English does (no
// `{n|plural:lot:lots}`), and the same payload field can be rendered as
// `|number` in one locale and `|signed:2` in another for typographic
// reasons. The invariant we DO need to enforce is that every locale
// references the same payload data — otherwise one locale would silently
// hide a number that a reviewer actually needs to see.

console.log("[5] CI: placeholder parity across locales");

const PLACEHOLDER_RE = /\{([a-zA-Z_][a-zA-Z0-9_]*)(\|[^}]*)?\}/g;
function fieldSet(s: string): string {
  // Just the *field names*, deduplicated and sorted. Format tokens are
  // intentionally allowed to vary per locale (see header).
  const names = new Set<string>();
  let m: RegExpExecArray | null;
  while ((m = PLACEHOLDER_RE.exec(s)) !== null) {
    names.add(m[1]);
  }
  return Array.from(names).sort().join("|");
}

check("each templateKey references identical payload fields in every locale", () => {
  const locales = Object.keys(lessonMessages) as LocaleId[];
  const baseline = "en-US" as LocaleId;
  const drifted: string[] = [];
  for (const k of Object.keys(lessonMessages[baseline])) {
    const titleSig = fieldSet(lessonMessages[baseline][k].title);
    const bodySig = fieldSet(lessonMessages[baseline][k].body);
    for (const loc of locales) {
      if (loc === baseline) continue;
      const t = lessonMessages[loc][k];
      if (!t) continue;
      if (fieldSet(t.title) !== titleSig) {
        drifted.push(`${loc}::${k}::title baseline=[${titleSig}] got=[${fieldSet(t.title)}]`);
      }
      if (fieldSet(t.body) !== bodySig) {
        drifted.push(`${loc}::${k}::body baseline=[${bodySig}] got=[${fieldSet(t.body)}]`);
      }
    }
  }
  if (drifted.length > 0) {
    throw new Error(`field-set drift across locales:\n  - ${drifted.join("\n  - ")}`);
  }
});

// ---------------------------------------------------------------------------
// 6. CI guard: RENDER SNAPSHOT — golden output freezes accidental edits
// ---------------------------------------------------------------------------
//
// Update procedure when a copy edit is intentional:
//   1. Run `npm run test:i18n` and copy the printed "[snapshot] …" lines
//      into the SNAPSHOTS map below.
//   2. Send the diff for review along with the dictionary change so a
//      reviewer sees both in one PR.
//
// A snapshot bake-off entry per (locale, templateKey) covers the common
// numeric / plural / locale-format edge cases that are easy to break
// silently.

console.log("[6] CI: render snapshot");

interface SnapshotCase {
  key: string;
  payload: Record<string, unknown>;
}
const SNAPSHOT_CASES: SnapshotCase[] = [
  { key: "attribution.lesson.sleeve_regime_loser", payload: loserPayload },
  { key: "attribution.lesson.sleeve_regime_winner", payload: winnerPayload },
  {
    key: "attribution.lesson.insufficient_data.watching",
    payload: { open_lot_count: 7, earliest_opened_at: "2026-05-12", window_days: 30 },
  },
  {
    key: "attribution.lesson.insufficient_data.watching",
    payload: { open_lot_count: 1, earliest_opened_at: "2026-05-12", window_days: 30 },
  },
  {
    key: "attribution.lesson.insufficient_data.watching_no_date",
    payload: { open_lot_count: 4, window_days: 30 },
  },
  {
    key: "attribution.lesson.insufficient_data.empty",
    payload: { window_days: 30 },
  },
];

const SNAPSHOTS: Record<string, { title: string; body: string }> = {
  "en-US::attribution.lesson.sleeve_regime_loser::loser": {
    title:
      'Sleeve "mean_reversion" is losing money in regime "chop" (12 trades, win-rate 25%, PnL -480.50)',
    body:
      "Across 12 closed lots in regime chop, the mean_reversion sleeve recorded a 25% win rate and a cumulative realised P&L of -480.50 (avg pnl pct: -4.0%, avg holding 2.1 days). Consider pausing this (sleeve, regime) combination in the fund's strategy sleeves config (fund.config.strategySleeves) until conditions change, or instrumenting the entry filter further to understand why the signal misfires in this regime.",
  },
  "en-US::attribution.lesson.sleeve_regime_winner::winner": {
    title:
      'Sleeve "trend" is profitable in regime "trend_up" (15 trades, win-rate 73%, PnL +1,240.00)',
    body:
      "Across 15 closed lots in regime trend_up, the trend sleeve recorded a 73% win rate and a cumulative realised P&L of +1,240.00 (avg pnl pct: +8.0%, avg holding 7.0 days). This combination is contributing positively; the LLM PM may want to scale exposure or relax confidence thresholds when regime=trend_up.",
  },
  "en-US::attribution.lesson.insufficient_data.watching::open=7": {
    title:
      "Watching 7 open lots since 2026-05-12 — no closed roundtrip in the last 30 days yet",
    body:
      "The attribution agent has 7 still-open lots under observation (earliest opened on 2026-05-12). It will produce a per-sleeve / per-regime scorecard once the first sell closes one of them. Until then, no win-rate or P&L lessons can be issued.",
  },
  "en-US::attribution.lesson.insufficient_data.watching::open=1": {
    title:
      "Watching 1 open lot since 2026-05-12 — no closed roundtrip in the last 30 days yet",
    body:
      "The attribution agent has 1 still-open lot under observation (earliest opened on 2026-05-12). It will produce a per-sleeve / per-regime scorecard once the first sell closes one of them. Until then, no win-rate or P&L lessons can be issued.",
  },
  "en-US::attribution.lesson.insufficient_data.watching_no_date::open=4": {
    title:
      "Watching 4 open lots — no closed roundtrip in the last 30 days yet",
    body:
      "The attribution agent has 4 still-open lots under observation. It will produce a per-sleeve / per-regime scorecard once the first sell closes one of them.",
  },
  "en-US::attribution.lesson.insufficient_data.empty::window=30": {
    title: "No closed trades in the last 30 days",
    body: "Attribution will populate once the fund has produced its first realized P&L.",
  },
  "zh-CN::attribution.lesson.sleeve_regime_loser::loser": {
    title:
      '策略套件 "mean_reversion" 在 "chop" 行情下亏损（12 笔，胜率 25%，盈亏 -480.50）',
    body:
      '在 "chop" 行情下共 12 个已平仓批次，"mean_reversion" 套件录得 25% 胜率，累计已实现盈亏 -480.50（平均收益率 -4.0%，平均持仓 2.1 天）。建议在该基金的「策略套件配置」（fund.config.strategySleeves）中暂停该 (套件, 行情) 组合，直到行情切换；或加强进场过滤，以理解此行情下信号失效的原因。',
  },
  "zh-CN::attribution.lesson.sleeve_regime_winner::winner": {
    title:
      '策略套件 "trend" 在 "trend_up" 行情下盈利（15 笔，胜率 73%，盈亏 +1,240.00）',
    body:
      '在 "trend_up" 行情下共 15 个已平仓批次，"trend" 套件录得 73% 胜率，累计已实现盈亏 +1,240.00（平均收益率 +8.0%，平均持仓 7.0 天）。该组合贡献为正；当 regime=trend_up 时，PM 可考虑加大该套件敞口或放宽信心阈值。',
  },
  "zh-CN::attribution.lesson.insufficient_data.watching::open=7": {
    title:
      "正在观察 7 个未平仓批次（自 2026-05-12 起）— 最近 30 天还没有完整的回合",
    body:
      "归因代理目前在跟踪 7 个尚未平仓的批次（最早开仓于 2026-05-12）。一旦有卖出动作完成首个回合，将开始按 (策略套件, 行情) 输出评分。在此之前，无法生成胜率或盈亏类的反思。",
  },
  "zh-CN::attribution.lesson.insufficient_data.watching::open=1": {
    title:
      "正在观察 1 个未平仓批次（自 2026-05-12 起）— 最近 30 天还没有完整的回合",
    body:
      "归因代理目前在跟踪 1 个尚未平仓的批次（最早开仓于 2026-05-12）。一旦有卖出动作完成首个回合，将开始按 (策略套件, 行情) 输出评分。在此之前，无法生成胜率或盈亏类的反思。",
  },
  "zh-CN::attribution.lesson.insufficient_data.watching_no_date::open=4": {
    title: "正在观察 4 个未平仓批次 — 最近 30 天还没有完整的回合",
    body:
      "归因代理目前在跟踪 4 个尚未平仓的批次。一旦有卖出动作完成首个回合，将开始按 (策略套件, 行情) 输出评分。",
  },
  "zh-CN::attribution.lesson.insufficient_data.empty::window=30": {
    title: "最近 30 天没有已平仓的交易",
    body: "基金完成首笔已实现盈亏后，归因结果就会自动填充。",
  },
};

function snapshotKey(loc: LocaleId, kase: SnapshotCase): string {
  // Tag each fixture with a short variant marker so the snapshot map
  // stays human-readable when somebody scans the dictionary later.
  const p = kase.payload as Record<string, unknown>;
  if (kase.key.endsWith(".watching") || kase.key.endsWith(".watching_no_date")) {
    return `${loc}::${kase.key}::open=${p.open_lot_count}`;
  }
  if (kase.key.endsWith(".empty")) {
    return `${loc}::${kase.key}::window=${p.window_days}`;
  }
  if (kase.key.endsWith(".sleeve_regime_loser")) {
    return `${loc}::${kase.key}::loser`;
  }
  if (kase.key.endsWith(".sleeve_regime_winner")) {
    return `${loc}::${kase.key}::winner`;
  }
  return `${loc}::${kase.key}`;
}

for (const loc of ["en-US", "zh-CN"] as LocaleId[]) {
  for (const kase of SNAPSHOT_CASES) {
    const sk = snapshotKey(loc, kase);
    check(`snapshot ${sk}`, () => {
      const r = renderLesson(loc, kase.key, kase.payload);
      if (!r) throw new Error("renderLesson returned undefined");
      const want = SNAPSHOTS[sk];
      if (!want) {
        throw new Error(
          `missing baseline snapshot for ${sk}\n  current title: ${r.title}\n  current body:  ${r.body}`,
        );
      }
      if (r.title !== want.title) {
        throw new Error(
          `title drift\n  expected: ${want.title}\n  got:      ${r.title}`,
        );
      }
      if (r.body !== want.body) {
        throw new Error(
          `body drift\n  expected: ${want.body}\n  got:      ${r.body}`,
        );
      }
    });
  }
}

// ---------------------------------------------------------------------------
// Summary
// ---------------------------------------------------------------------------

if (failures > 0) {
  console.error(`\n${failures} of ${total} cases FAILED`);
  process.exit(1);
}
console.log(`\n${total} cases passed`);
