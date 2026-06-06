# Lesson i18n glossary

Reference for translators working on `attribution.lesson.*` template
strings (defined in `shared/api-client/src/i18n.ts`, rendered by
`web/src/lib/lessonRenderer.ts`).

This glossary fixes the canonical translation of every domain term
used in lesson templates. New translations MUST conform; if a term
doesn't appear here, add it before merging the new translation.

## How this glossary is enforced

A reviewer reading the i18n PR is the primary backstop. To make their
job mechanical, every term below is paired with the exact text the CI
snapshot test (`web/test/lessonRenderer.test.ts`, "[6] CI: render
snapshot") expects to see in the rendered output. If a translation
diverges from this glossary, the snapshot will fail.

## Term table

| Concept                  | en-US                          | zh-CN                       | Notes                                                                          |
| ------------------------ | ------------------------------ | --------------------------- | ------------------------------------------------------------------------------ |
| sleeve                   | sleeve                         | 策略套件                    | Independent strategy "section" of a fund. Always quoted in title for emphasis. |
| regime                   | regime                         | 行情                        | Market regime classifier output (chop, trend_up, …). Quoted in title.          |
| sleeve+regime            | (sleeve, regime) combination   | (套件, 行情) 组合           | Pair-level decision unit, used when suggesting pause / scale.                  |
| trade                    | trade                          | 笔                          | One closed roundtrip = 1 trade in attribution.                                 |
| closed lot               | closed lot                     | 已平仓批次                  | Single fill that closed a position out (sell after buy). Plural OK.            |
| open lot                 | open lot                       | 未平仓批次                  | Position still under observation (buy without sell yet).                       |
| roundtrip                | closed roundtrip               | 完整的回合                  | Buy + sell pair making a P&L observation.                                      |
| win rate                 | win rate                       | 胜率                        | profitable_trades / total_trades. Always rendered as %.                        |
| realised P&L             | realised P&L / realized P&L    | 已实现盈亏                  | "realised" (UK) is the established codebase spelling.                          |
| avg PnL %                | avg pnl pct                    | 平均收益率                  | Mean of `pnl / cost_basis` per closed lot.                                     |
| avg holding              | avg holding                    | 平均持仓                    | Mean holding period across closed lots, in days.                               |
| LLM PM                   | LLM PM                         | PM                          | Portfolio Manager agent. Untranslated initialism in en-US.                     |
| confidence threshold     | confidence threshold           | 信心阈值                    | LLM PM tunable: minimum decision confidence to act.                            |
| entry filter             | entry filter                   | 进场过滤                    | Pre-trade gating logic that decides whether to open a position.                |
| scorecard                | scorecard                      | 评分                        | Per-(sleeve, regime) summary table.                                            |
| attribution agent        | attribution agent              | 归因代理                    | The system component that emits these lessons.                                 |
| fund.config.strategySleeves | fund.config.strategySleeves | fund.config.strategySleeves | Code identifier — never translated.                                            |

## Number formatting

| Quantity      | en-US          | zh-CN         |
| ------------- | -------------- | ------------- |
| trade count   | `12 trades`    | `12 笔`       |
| win rate      | `25%` (no decimals)        | `25%`        |
| PnL           | `+1,240.00` / `-480.50`    | `+1,240.00` / `-480.50` |
| avg pnl pct   | `+8.0%` / `-4.0%` (signed) | `+8.0%` / `-4.0%` |
| holding days  | `2.1 days`     | `2.1 天`     |
| date          | `2026-05-12`   | `2026-05-12` |

All numbers are formatted client-side via `Intl.NumberFormat(locale)`.
Templates use the `{field|number}`, `{field|percent}`, `{field|signed:N}`,
`{field|signed_pct}`, `{field|date}`, `{field|plural:a:b}` tokens —
see `shared/api-client/src/i18n.ts` for the full grammar.

## Style guidelines

- **Identifiers stay literal.** Sleeve names ("mean_reversion",
  "trend"), regime names ("chop", "trend_up"), config paths
  ("fund.config.strategySleeves"), and code-shape strings render verbatim
  in every locale. Quoting is OK, translation is not.
- **Plural correctness.** English uses `{n|plural:lot:lots}` to switch
  between "lot" and "lots". Chinese has no grammatical plural — use the
  measure word "个" with no plural switch. The CI parity check enforces
  *field set* parity, not format token parity (so missing `|plural` in
  Chinese is fine).
- **Tone.** Lessons are operator-facing diagnostics, not user-facing
  marketing. Prefer concise indicative mood ("the X sleeve recorded a
  Y win rate") over imperative or apologetic phrasings.
- **Action verbs.** When a lesson suggests action ("Consider pausing"),
  Chinese uses 建议 + verb. Keep it neutral; don't escalate to 必须 / 请.

## Adding a new template

1. Add the English entry to `lessonMessages["en-US"][newKey]`.
2. Add the Chinese entry to `lessonMessages["zh-CN"][newKey]`.
3. Reference only fields present in the server-side `Payload`. Run
   `npm run test:i18n` — the key completeness + field parity guards
   will fail loudly on mismatches.
4. Add a snapshot fixture to `web/test/lessonRenderer.test.ts`
   (`SNAPSHOT_CASES` + `SNAPSHOTS`). Run the test — failures show the
   raw rendered output, copy it back into `SNAPSHOTS` if it matches
   intent.
5. If the new template introduces a new domain term, extend the table
   above before merging.

## Additional namespace-level i18n guards (W12-1 / W13-5)

`web/test/i18nNamespaceParity.test.ts` runs against every namespace
under `web/src/i18n/locales/{lang}/*.ts`, not just lesson templates.
Two checks are particularly likely to fire on a glossary-driven
translation PR:

1. **non-empty guard** — every leaf string must be non-empty after
   trim. Empty Chinese values for an English string (or vice versa)
   are flagged before review.
2. **suspicious-identical** — flags pairs where the en-US and zh-CN
   strings are byte-for-byte identical AND the value looks like an
   English phrase (multi-word, no CJK characters). The intent is
   to catch copy-paste regressions where a translator left the
   English in by mistake. Identifier-shaped strings (single tokens,
   code paths) are exempt automatically.

If a justified pair would trip the suspicious-identical check —
e.g. a brand name that's intentionally untranslated — add an entry
to `web/test/i18nNamespaceParity.allowlist.ts` with a one-line
`reason`. The allowlist file's hygiene check will then ensure the
entry stays valid (key still exists in both bundles, reason
present). See OBS4 in `FUTURE_WORK_INVENTORY.md` for the quarterly
allowlist review cadence.

## Bumping the payload schema

When the server-side `Payload` field set changes in a non-additive
way (renamed, removed, retyped), bump the template key with a `.v2`
suffix. Keep the old key (`.v1` implicit) until the 30-day lesson
replay window has aged out the old rows. The frontend always renders
the exact key the server emitted, so two versions can coexist
peacefully.
