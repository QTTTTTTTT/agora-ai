# Future Work Inventory

A single-page index of every "## Future work" item recorded across
the project's ADRs (`docs/*.md`). The point is to make the
following question one `grep`-free read:

> **"What follow-ups did we sign up for in past sprints, what's
> their state, and which file owns the deeper context?"**

Without this index, a contributor scanning the repo finds 6 ADRs
each with their own "later" lists, no cross-doc state, and no easy
way to tell whether item #2 in `MARKETSTATUS_GATE.md` is the same
as item #5 in `DRAWDOWN_SOFT_CIRCUIT_BREAKER.md` (it isn't, but
they're related). The inventory below collapses those into one
table grouped by domain so prioritisation can happen at the system
level, not the document level.

The source ADR remains the authoritative description for any
single item — this file's `Source` column links to the relevant
section so the deeper rationale is one click away.

## Conventions

- **Status legend**:
  - `active` — eligible to pick up, no current owner
  - `claimed` — someone has it on their plate (write `claimed by
    @owner` in the row)
  - `blocked` — waiting on a product / infra prerequisite called
    out in the ADR
  - `done` — completed; kept here for one release cycle then
    pruned
  - `dropped` — explicitly decided not to do; reason in source ADR
- **Effort** is a t-shirt size, sourced from the ADR author's
  estimate at write time. Re-estimate before claiming if the
  underlying systems have changed.
- **Adding a new entry**: When you write `## Future work` in an
  ADR, mirror each bullet here in the appropriate section. Keep
  the row pointing back to the ADR with a relative link plus the
  bullet number in the source.
- **Closing an entry**: Flip `Status` to `done` and add a short
  pointer to the closing commit / PR. Leave it in the table for
  one release cycle so anyone tracking against the previous
  inventory sees the resolution; prune at the next major release.
- **Removing an entry**: Only when both the source ADR's bullet
  has been deleted (or struck through with a closing reference)
  AND a release has shipped that includes the work. Prefer
  marking `done` over outright deletion so the audit trail
  stays.

---

## Agent learning & memory

| ID  | Item                                                                                  | Status   | Effort | Source                                                                                               |
| --- | ------------------------------------------------------------------------------------- | -------- | ------ | ---------------------------------------------------------------------------------------------------- |
| AL1 | Per-fund regime selection — wire `currentRegime` into agent-portable lesson retrieval | blocked  | M      | [AGENT_PORTABLE_LEARNING.md §Future work #2](AGENT_PORTABLE_LEARNING.md#future-work) (product call)  |
| AL2 | Per-row export opt-out — `cross_fund_disabled` boolean on `memories`                   | active   | M      | [AGENT_PORTABLE_LEARNING.md §Future work #3](AGENT_PORTABLE_LEARNING.md#future-work) (schema change) |
| AL3 | Marketplace ↔ agent_portable bridge — buy-this-agent's-notebook flow                   | active   | L      | [AGENT_PORTABLE_LEARNING.md §Future work #4](AGENT_PORTABLE_LEARNING.md#future-work) (separate ADR)  |
| AL4 | React LessonList surface — operator audit / curate UI page                             | active   | M      | [AGENT_PORTABLE_LEARNING.md §Future work #5](AGENT_PORTABLE_LEARNING.md#future-work)                 |
| AL5 | Reflection-path coverage gap — researcher / analyst reflections don't follow the agent | active   | M      | [AGENT_PORTABLE_LEARNING.md §Future work #6](AGENT_PORTABLE_LEARNING.md#future-work) (4 design opts) |

## Trading & market data

| ID  | Item                                                                            | Status  | Effort | Source                                                                                                |
| --- | ------------------------------------------------------------------------------- | ------- | ------ | ----------------------------------------------------------------------------------------------------- |
| TR1 | Large-order market-impact model — slippage as `notional / ADV`, not flat bps     | active  | L      | [MARKETSTATUS_GATE.md §Future work #1](MARKETSTATUS_GATE.md#future-work-subsequent-s6-rounds) (new pkg) |
| TR2 | Securities lending / locate — borrow fee + HTB / NTB classification              | active  | L      | [MARKETSTATUS_GATE.md §Future work #2](MARKETSTATUS_GATE.md#future-work-subsequent-s6-rounds)         |
| TR3 | IPO lock-up — block sells inside insider lock-up window                          | active  | M      | [MARKETSTATUS_GATE.md §Future work #3](MARKETSTATUS_GATE.md#future-work-subsequent-s6-rounds)         |
| TR4 | WebSocket realtime quote feed — replace REST polling pipeline                    | active  | XL     | [MARKETSTATUS_GATE.md §Future work #4](MARKETSTATUS_GATE.md#future-work-subsequent-s6-rounds)         |
| TR5 | `defensive_only` × MARKETSTATUS_GATE integration — block longs while breached    | active  | M      | [MARKETSTATUS_GATE.md §Future work #5](MARKETSTATUS_GATE.md#future-work-subsequent-s6-rounds)         |

## Risk & breakers

| ID  | Item                                                                                | Status   | Effort | Source                                                                                                                  |
| --- | ----------------------------------------------------------------------------------- | -------- | ------ | ----------------------------------------------------------------------------------------------------------------------- |
| RK1 | Order-pipeline integration for `auto_execute=true` drawdown trim plans               | active   | M      | [DRAWDOWN_SOFT_CIRCUIT_BREAKER.md §Future work #1](DRAWDOWN_SOFT_CIRCUIT_BREAKER.md#future-work-out-of-scope-for-v1)     |
| RK2 | `defensive_only` enforcement at order entry — reject NEW longs while tier 3 active   | active   | S      | [DRAWDOWN_SOFT_CIRCUIT_BREAKER.md §Future work #2](DRAWDOWN_SOFT_CIRCUIT_BREAKER.md#future-work-out-of-scope-for-v1) — overlaps with TR5 |
| RK3 | Per-position trim weights — v2: trim higher-beta harder, skip <X% NAV positions      | active   | M      | [DRAWDOWN_SOFT_CIRCUIT_BREAKER.md §Future work #3](DRAWDOWN_SOFT_CIRCUIT_BREAKER.md#future-work-out-of-scope-for-v1)     |
| RK4 | Tier-history audit UI — surface `policy_upsert` chain in the admin view              | active   | S      | [DRAWDOWN_SOFT_CIRCUIT_BREAKER.md §Future work #4](DRAWDOWN_SOFT_CIRCUIT_BREAKER.md#future-work-out-of-scope-for-v1)     |
| RK5 | Cross-fund correlation alert — coalesce when many funds breach tier 3 at once        | active   | M      | [DRAWDOWN_SOFT_CIRCUIT_BREAKER.md §Future work #5](DRAWDOWN_SOFT_CIRCUIT_BREAKER.md#future-work-out-of-scope-for-v1)     |

## Surveillance

| ID  | Item                                                                                       | Status | Effort | Source                                                                          |
| --- | ------------------------------------------------------------------------------------------ | ------ | ------ | ------------------------------------------------------------------------------- |
| SV1 | More rules — rapid-fire reversal, layering suspect, cross-fund self-cross variant           | active | M      | [SURVEILLANCE_FRAMEWORK.md §Future work #1](SURVEILLANCE_FRAMEWORK.md#future-work) |
| SV2 | MarketContext enrichment — wire `marketdata` to populate `AvgDailyNotional` + `RecentVWAP` | active | M      | [SURVEILLANCE_FRAMEWORK.md §Future work #2](SURVEILLANCE_FRAMEWORK.md#future-work) |
| SV3 | Per-fund / per-instrument session calendars — exchange-calendar lookup vs hard-coded 20:00 UTC | active | L  | [SURVEILLANCE_FRAMEWORK.md §Future work #3](SURVEILLANCE_FRAMEWORK.md#future-work) |
| SV4 | Real-time hot-path — bolt self-trade rule onto order-entry as synchronous block             | blocked | M      | [SURVEILLANCE_FRAMEWORK.md §Future work #4](SURVEILLANCE_FRAMEWORK.md#future-work) (needs months of shadow signal) |

## Observability / SRE infra

Tracks follow-ups recorded across the embed-pipeline observability
arc (W6 → W14): the embed-quota limiter, memory re-embed queue,
DB pool surface, and the per-fund side-car. The shipped state
lives in `docs/PROMETHEUS_QUERIES.md` (alert thresholds + admin
endpoints) and `docs/PER_FUND_EMBEDQUOTA_OBSERVABILITY.md` (ADR).

| ID  | Item                                                                                          | Status | Effort | Source                                                                                                                  |
| --- | --------------------------------------------------------------------------------------------- | ------ | ------ | ----------------------------------------------------------------------------------------------------------------------- |
| OBS1 | `memreembed.Request.FundID` — propagate fundID from consolidation enqueue path so the re-embed worker's Embed call can populate per-fund metrics | active  | S | [PER_FUND_EMBEDQUOTA_OBSERVABILITY.md §7 follow-ups](PER_FUND_EMBEDQUOTA_OBSERVABILITY.md#7-what-ships-per-wave) |
| OBS2 | Per-fund `_overflow` cardinality recording rule + alert (paged when MaxFunds budget exhausted) | active  | S      | [PER_FUND_EMBEDQUOTA_OBSERVABILITY.md §7 follow-ups](PER_FUND_EMBEDQUOTA_OBSERVABILITY.md#7-what-ships-per-wave)        |
| OBS3 | `INSTRUMENTATION_ROADMAP.md` — decide whether to switch the hand-rolled atomic histograms to `prometheus/client_golang` | active  | M      | [PER_FUND_EMBEDQUOTA_OBSERVABILITY.md §9](PER_FUND_EMBEDQUOTA_OBSERVABILITY.md#9-prior-art--why-we-didnt-reuse-prometheuss-own) |
| OBS4 | i18n parity allowlist hygiene — review `web/test/i18nNamespaceParity.allowlist.ts` quarterly so flagged identical strings don't accumulate stale exemptions | active | XS | [W13-5 commit message + `i18nNamespaceParity.test.ts` preamble] |

## Model A/B & experimentation

| ID  | Item                                                                                | Status | Effort | Source                                                                                  |
| --- | ----------------------------------------------------------------------------------- | ------ | ------ | --------------------------------------------------------------------------------------- |
| AB1 | Phase-2 auto-create — when Apply confirmed, spawn next experiment with new control   | active | M      | [MODEL_AB_AUTO_PROMOTION.md §Future work #1](MODEL_AB_AUTO_PROMOTION.md#future-work)     |
| AB2 | Per-scope criteria overrides — tighter agreement bar for scope=global vs scope=fund  | active | S      | [MODEL_AB_AUTO_PROMOTION.md §Future work #2](MODEL_AB_AUTO_PROMOTION.md#future-work)     |
| AB3 | Daily summary email to on-call distribution list                                     | active | S      | [MODEL_AB_AUTO_PROMOTION.md §Future work #3](MODEL_AB_AUTO_PROMOTION.md#future-work)     |
| AB4 | Card K — real LLM shadow runs (replaces deterministic-scaling B-side)                | active | XL     | [ABTEST_SHADOW_AGENTS.md §Future work #1](ABTEST_SHADOW_AGENTS.md#future-work)           |
| AB5 | Android parity for the shadow-agent panel                                            | active | M      | [ABTEST_SHADOW_AGENTS.md §Future work #2](ABTEST_SHADOW_AGENTS.md#future-work)           |
| AB6 | CSV export for `bySymbol` operational-attribution table                              | active | S      | [ABTEST_SHADOW_AGENTS.md §Future work #3](ABTEST_SHADOW_AGENTS.md#future-work)           |

## Cross-cutting prioritisation notes

Items that are explicitly **dependent** on each other:

- **TR5 ↔ RK2**: both want "block longs while a `defensive_only`
  drawdown is live". TR5 is the gate-pipeline angle, RK2 is the
  order-entry angle. Land them together to avoid a half-state
  where the gate rejects but the order entry doesn't (or vice
  versa).
- **TR1 (market impact) → TR4 (WebSocket feed)**: market-impact
  modelling improves materially with intraday VWAP that the
  REST poll can't supply. TR1 has a workable v1 against the
  current data, but the headline experience improves once TR4
  is in.
- **AL5 (reflection cross-fund) ↔ AL1 (regime selection)**: the
  ADR notes these are not mutually exclusive but share the
  visibility-router design space; pick the AL5 option BEFORE
  AL1 lands so the regime gate doesn't get retrofit twice.

Items that are explicitly **dropped** (recorded so future
contributors don't keep re-discovering them):

- (none today; add a row here when an ADR moves an item to
  status `dropped`)

## How to update this file

Treat the inventory as load-bearing — it's the only place where
the cross-ADR view exists, so a stale entry is more harmful than
no entry. The discipline is:

1. **At ADR write time.** When you add `## Future work` to an
   ADR, copy each bullet to the matching section above. Pick a
   short two- or three-letter prefix per domain (AL = Agent
   Learning, TR = Trading, RK = Risk, SV = Surveillance, AB =
   Model A/B); add the next free number. The ID is the stable
   handle other ADRs / commits can reference.
2. **At pickup.** Edit the row's `Status` to `claimed by
   @your-handle`. Tracking this here (vs only in a project
   board) keeps the source-of-truth in version control —
   project boards rotate, the repo doesn't.
3. **At completion.** Flip `Status` to `done` with a short
   trailing note: `done · commit abc1234 · 2026-06-12`. Leave
   the row for one release cycle then prune.
4. **At de-scope.** Flip `Status` to `dropped`, append a one-
   sentence rationale, and update the source ADR's bullet to
   match (strike-through with a brief closing note pointing
   here).

If you find a bullet in an ADR that's NOT mirrored here, that's
a backlog gap — please add the row inline as part of your next
PR. The inventory's value falls off a cliff if it lags the
ADRs.
