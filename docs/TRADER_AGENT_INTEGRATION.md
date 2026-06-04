# Trader Agent Integration — Status & Roadmap

> Doc owner: trading runtime / Tracking issue: B-step2 (TBD)
> Status as of 2026-06-04: **step 1 done; step 2 equity buy + long-side sell + execution_status rollup helper + parent/child list filter API + UI hide-children rollout + summarizeTrades splitter-aware fix + plan_actions.execution_status wire + futures cash ledger v2 (margin + realized PnL) + futures long splitter gate unlock (when v2 on) + short-side lot ledger data layer landed; short-side wiring + splitter gate flip pending**

## Why this doc exists

The `agent.TraderAgent` type
([server/internal/agent/trader.go](../server/internal/agent/trader.go))
has been implemented since Sprint 1 with full strategy selection
(`immediate` / `limit` / `twap` / `vwap`), child-order splitting,
slippage aggregation, and an `ExecutePlan` method that consumes a
whole `InvestmentPlan`. However, the actual trading runtime that
lives in `runtimeTradingEngine.executePlanAction` (in
[server/cmd/server/wiring_adapters.go](../server/cmd/server/wiring_adapters.go))
has historically **bypassed** the Trader and written one
`trade_executions` row per `plan_action` directly via the
`tradeRepoCreateAndFill` helper.

This document records the architectural debt and the staged plan to
close it.

## What "PM direct-fill" means today

```
PM (LLMDecisionEngine)
  └─ produces InvestmentPlan with N PlanActions

runtimeTradingEngine.executePlanAction (PER action)
  ├─ enforceHardRiskGate              (8 hard-risk checks)
  ├─ pmPathPreTradeGateChain          (4 broker gates: market-status,
  │                                    lockup, borrow, price-collar)
  ├─ pmPathLotSizeGuard               (lot-size + tick alignment)
  └─ tradeRepoCreateAndFill           ← writes ONE trade_executions row,
                                        side-effects positions + cash
                                        ledger.

TraderAgent.ExecutePlan               ← never called.
```

In other words: **the PM-direct-fill path applies the full
pre-trade gate chain but never asks the trader desk to pick an
execution style or slice a large order across the day**. A 10 000-
share buy fills at the live quote in one shot, exactly the same way
a 100-share buy does.

## Why this is a problem

A real research pod would split execution between:

- **PM** — decides *what to buy / how much*. Sets a Plan.
- **Trader** — decides *how to execute*. Picks `immediate` /
  `limit` / `twap` / `vwap`, slices large parents into N children,
  controls slippage, batches around the close.

Without the Trader, every fill is "immediate at the quote". On
small orders this is fine; on large orders it (a) leaks impact into
the market, (b) defeats the slippage guard's whole point, and (c)
gives the daily-review LLM no execution micro-structure to reason
about ("the trader logged a TWAP intent but only got 60% of the
parent filled before close" — that's the kind of journal entry
that should exist and currently can't).

## Staged integration plan

### B-step 1 — strategy *recording* (this PR, **DONE**)

Land the *decision* without changing the runtime shape: every
PM-direct-fill `tradeRepoCreateAndFill` call now passes a strategy
string picked by `selectPMPathExecutionStrategy`, using the same
rule as `agent.TraderAgent.selectStrategy`:

| Quantity | Plan price | Picked strategy |
|---|---|---|
| > `pmPathSplitThreshold` (1000) | any | `twap` |
| ≤ threshold | > 0 | `limit` |
| ≤ threshold | 0 / NULL | `immediate` |

The strategy is **not** persisted on a `trade_executions.strategy`
column today (we still write one row per action, so a column would
have at most one distinct value per parent). Instead it lands as a
structured log line:

```
slog.Info("pm-path execute trade",
  fund_id, plan_id, action_id, symbol, side, quantity,
  strategy, plan_price, status)
```

The daily-review LLM context builder can grep this log line in a
later iteration; analytics queries that need the strategy label can
use the same source. We will add the column in step 2 when child
orders actually produce distinct strategy values per row.

**Step 1 deliverables landed:**

- `server/cmd/server/pm_path_execution_strategy.go` — pure selector +
  normaliser, mirrors `agent.DefaultTraderConfig` thresholds.
- `tradeRepoCreateAndFill` accepts a `strategy string`, logs it.
- 4 call sites in `wiring_adapters.go` (equity buy/sell, futures
  open/close) pass the selected strategy.
- `server/cmd/server/pm_path_execution_strategy_test.go` — unit
  tests cover the quantity-gate dominance, plan-price branch, and
  normaliser edge cases (case, whitespace, unknown values).

### B-step 2 — real child-order splitting (**IN-PROGRESS: equity buy + equity long-side sell landed; futures / short-side pending**)

When this fully lands, the picture becomes:

```
runtimeTradingEngine.executePlanAction (PER action)
  ├─ enforceHardRiskGate            (unchanged)
  ├─ pmPathPreTradeGateChain        (unchanged — parent level)
  ├─ selectPMPathExecutionStrategy  (now used by splitter)
  ├─ tradingDeskSplitter.split(parent, strategy)
  │   └─ for each child:
  │      ├─ pmPathLotSizeGuard      (child qty)
  │      ├─ tickAlignment           (parent price)
  │      └─ tradeRepoCreateAndFillChild(parent_trade_id, child_qty,
  │                                     child_fill_price, strategy)
  └─ aggregatePlanActionStatus      (rolls N children into one
                                     plan_action.execution_status)
```

#### Step 2 work items — current status

**Landed in this PR (buy path only):**

- [x] Migration `088_trade_strategy_and_parent.sql` adds
      `trade_executions.strategy VARCHAR(16)` (CHECK in the
      immediate / limit / twap / vwap / iceberg / pov vocabulary)
      and `trade_executions.strategy_parent_trade_id UUID` with a
      partial index on the non-NULL slice. The column is
      **distinct from the pre-existing `parent_trade_id`** that
      migration 051 wired for OCO / bracket parents; the two
      relationships are orthogonal (see column COMMENTs for the
      disambiguation).
      _Note_: this PR uses migration **088** because migration 087
      was consumed by the `fund_team_member_specialization` table
      (T3 work, see `agent_self_learning_prompts.go` notes).
- [x] `repository.TradeExecution.Strategy` +
      `StrategyParentTradeID` fields. `TradeRepo.Create` writes
      both; `tradeExecutionColumns` SELECTs them so every reader
      sees them.
- [x] `splitParentIntoChildren(qty, strategy) []int` —
      `server/cmd/server/pm_path_child_split.go`. Returns
      `[800,800,800,800,800]` for 4000 TWAP, `[800,800,800,800,801]`
      for 4001 TWAP (last child absorbs the remainder), `[qty]` for
      `immediate` / `limit` / unknown so the splitting loop is
      uniform on every code path.
- [x] `pmPathChildSplittingEnabled(fund.Config)` — feature flag
      resolver in `pm_path_feature_flag.go`. Reads
      `pm_path_child_splitting` bool from the JSON config blob,
      defaults to **false** on missing key / malformed JSON / type
      mismatch (fail-safe to legacy single-row path).
- [x] `tradeRepoCreateAndFillSplit` implements the parent + N
      children fan-out **for the buy side only**. Parent row
      carries aggregated qty + summed fees + `strategy` +
      `strategy_parent_trade_id = NULL`. Each child row carries
      slice qty + pro-rata fees (`proRataFeeSplit`, last child
      absorbs the rounding remainder so SUM = parent) +
      `strategy_parent_trade_id = parent.ID`. **Parent row writes
      nothing to `cash_ledger` / `position_lots`**; only children
      do, so FIFO cost basis stays accurate per slice.
- [x] Idempotency keys: parent uses the legacy
      `trade:{action}:{side}:{totalQty}` key; children use
      `trade:{action}:{side}:{totalQty}:child:{idx}` so a retry of
      the same parent fans out into the same 1+N rows without
      double-booking.
- [x] Per-action test coverage: `pm_path_child_split_test.go`
      (splitter matrix + zero-quantity-child invariant),
      `pm_path_feature_flag_test.go` (flag resolver branches),
      `pm_path_split_executor_test.go` (a 4000-share TWAP buy
      produces 6 INSERTs + 6 UPDATEs in the right order, with the
      flag off the legacy single-row path is preserved, with the
      flag on but a short position the legacy single-row path is
      preserved, and `proRataFeeSplit` sums exactly to the parent
      total).
- [x] **Long-side sell splitter (T2 follow-up commit).**
      Investigation surfaced that `lotledger.recordSell` already
      FIFO-consumes `min(lot.QuantityRemaining, remaining)`
      across multiple open lots until the fill quantity is
      exhausted (see
      `server/internal/lotledger/lotledger.go:262`). N sequential
      child sells therefore produce the same FIFO close ordering
      as one big sell — the closed_lots rows differ in
      `trade_execution_id` but the per-lot consumed quantity and
      pro-rata fee allocation stay correct. No new
      `splitSellAgainstLots` helper was needed. The gate is now
      `splitterEnabledForSide(side, action)`
      (see `pm_path_splitter_gate.go`); short positions AND
      futures stay on the legacy single-row path.
- [x] Splitter-gate (side, position_side, asset_class) matrix
      (`pm_path_splitter_gate_test.go`) — 17-cell exhaustive
      table: buy/sell × long/short × equity/futures with case +
      whitespace normalisation. Safety-critical cells (any
      "short" → false, any "futures" → false) covered
      explicitly so a regression there is caught at unit-test
      time.
- [x] **execution_status rollup helper (T3 follow-up commit).**
      `aggregateChildrenStatus(children, parentQty)` in
      `pm_path_children_status.go` turns N per-child
      (status, filled_qty) pairs into a single parent-level
      label: "filled" / "partial:NN" (with NN floored to 99 so
      "filled" stays reserved for the all-filled terminal
      happy path) / "pending" / "rejected" / "" (empty cue
      to fall back to caller-decided status). Today the
      splitter logs this label as `rolled_status` on the
      `pm-path execute trade rollup` slog line — the database
      column `plan_actions.execution_status` is still driven
      by `executePlanAction` (caller-decided status) because
      `broker.Simulator` fills every child synchronously and
      the rollup would always read "filled". The helper is
      in place so the next live-broker integration
      (Alpaca / IBKR async partial fills) just has to switch
      `executePlanAction` to feed the rollup result through
      `syncPlanActionStatuses`. 13-case unit-test matrix
      pins each precedence rule.
- [x] **Parent + child list API surfaces (T3 follow-up
      commit).** `repository.TradeRepo` gained three
      additions:
      - `TradeListOpts.ExcludeChildSlices` flag and matching
        `ListByFundPageOpts` / `ListByPlanOpts` wrappers around
        the legacy `ListByFundPage` / `ListByPlan` methods.
        The new opts methods inject
        `AND ($N = false OR strategy_parent_trade_id IS NULL)`
        into the WHERE clause; the legacy methods just forward
        the zero-value opts so existing callers keep their
        full-rowset behaviour and nothing in production rolled
        over.
      - `ListChildrenByStrategyParent(parentTradeID)` for the
        drilldown query (show me the slices of TWAP parent X).
        Empty parent ID returns the empty slice rather than
        a query error so a stale UI parameter is safe.
      - `api.Trade` JSON model now carries `strategy` +
        `strategyParentTradeId` so the frontend can identify
        which rows are children. `web/src/lib/api.ts` mirrors
        the new fields. Tsc clean; existing UIs are unaffected
        (additive, both omitempty).

- [x] **UI hide-children rollout (T4 commit).** The
      frontend `TradeHistory.tsx` page now appends
      `?exclude_child_slices=true` to its list call and
      defensively filters any child rows that slip through
      (stale cache / future regression) before rendering. A
      parent whose `strategy` is one of `twap`, `vwap`,
      `iceberg`, `pov` gets a "show slices / hide slices"
      button next to its status chip; clicking it lazily
      fetches `GET /api/funds/{fundId}/trades/{id}/children`,
      caches the result per-parent so toggling twice is free,
      and surfaces per-row errors so one failed drilldown
      doesn't break the surrounding table. The dashboard
      "10 most-recent trades" preview also flipped to the
      `excludeChildSlices=true` variant so it stays at one
      row per plan_action. Three new helpers — service
      `ListTradeChildren`, handler
      `GET /api/funds/{fundId}/trades/{tradeId}/children`,
      `convertTrade`'s cross-fund guard — round out the
      backend side. Six handler tests pin the contract:
      query-param forwarding, default-false back-compat,
      malformed-value safety, happy-path JSON shape, nil-slice
      → `[]` rendering, and unauthenticated rejection.

**Deferred to follow-up PRs (still TODO):**

- [x] **Futures cash ledger v2 (T7 commit).** The blocker
      called out below has been resolved: migration 089 adds
      three new entry_type values (`futures_margin_post`,
      `futures_margin_release`, `futures_realized_pnl`), and
      `recordCashLedgerForFill` dispatches to a new
      `recordCashLedgerFuturesForFill` helper when the fund
      has opted into `futures_cash_ledger_v2`. The v2 path
      writes margin_post on an open (debit = -initialMargin)
      and margin_release + signed realized_pnl on a close,
      with commission / transfer fees reusing the existing
      `trade_{buy,sell}_commission` entry types. realizedPnL
      is now propagated through `tradeRepoCreateAndFill` as
      a new `sql.NullFloat64` parameter; the futures-close
      branch in `executePlanAction` computes it upfront and
      passes the signed value, equity paths pass the zero
      value. Per-fund flag (default false) keeps every
      production fund on the legacy `trade_buy_notional` /
      `trade_sell_notional` path until operator explicitly
      opts in — flipping the flag changes cash math on every
      futures fill so this is a deliberate per-fund migration,
      not a global rollout. 3-case sqlmock test pins
      open-writes-margin_post, close-writes-margin_release-
      plus-pnl, and flag-off-keeps-legacy.

      Follow-up T8a: the **splitter gate** for futures long
      positions has also been unlocked when the v2 cash flow
      is on. The legacy `splitterEnabledForSide` (no config)
      still returns false for futures, but the new
      `splitterEnabledForSideWithConfig` returns true for
      futures long when `futures_cash_ledger_v2=true`. The
      dispatcher (`tradeRepoCreateAndFill`) was switched to
      call the with-config variant, so opted-in funds get
      futures-long splitting end-to-end: the splitter fans
      out per-child trade_executions rows, the per-child cash
      ledger writes margin_post / margin_release / realized_pnl
      with PnL pro-rated by `childQty/totalQty`, and the lot
      ledger keeps its long-only behaviour (futures longs are
      classified as buy/sell by `ClassifyFuturesSide` so the
      existing FIFO long-lot path handles them already). The
      short axis stays blocked regardless of the v2 flag — the
      short-lot ledger is the remaining prerequisite there, not
      cash flow. 10-case matrix test pins the futures unlock
      cells; a 1-case defense-in-depth test asserts the legacy
      `splitterEnabledForSide` (no config) is byte-identical to
      its pre-T8a behaviour.
- [x] **Short-side lot ledger data layer (T8 commit).** The
      symmetric blocker on the position_side axis has its data
      layer in place: migration 090 adds a `side VARCHAR(8)`
      column to position_lots + closed_lots (default 'long'
      backfills historical rows), the FIFO index now keys on
      `(fund_id, instrument_key, side, opened_at)`, and the
      LotRepo gains `ListOpenByInstrumentSideTx(...)` so callers
      can read long OR short lots through the same hot path
      shape. The lotledger service grows two new helpers:

        * `recordShortOpen` — handles a sell-to-open FillEvent
          with PositionSide="short", writes a side='short' row.
        * `recordShortClose` — handles a buy-to-cover FillEvent,
          FIFO-walks open short lots, emits closed_lots rows
          with side='short' and the inverse PnL formula
          (`(entry - exit) * qty` so a short profits when exit
          drops below entry). MFE / MAE are sign-flipped vs the
          long side so the column meaning stays "positive =
          favorable, negative = adverse".

      Routing in `Service.Record` switches on the new
      `FillEvent.PositionSide` field: short → short helpers,
      otherwise → legacy long path. Pre-T8 callers that leave
      PositionSide empty route exactly as before.

      6-case short-side unit test matrix covers: open creates
      side='short', profitable cover signs PnL positive, squeeze
      cover signs PnL negative, FIFO across multiple lots, partial
      cover leaves remainder, orphan cover is a soft error. A
      7th test (long+short coexistence) pins the isolation
      invariant: opening BOTH a long and short lot on the same
      (fund, instrument) keeps them separate — the long sell
      consumes long lots only, the short cover consumes short
      lots only. 4 existing repo / lotledger sqlmock tests
      updated to the new column shape.

      What's STILL pending: the **engine wiring** (recordLotFill
      in wiring_adapters.go currently early-returns when
      position_side="short"; it needs to route to the short
      helpers instead) and the **splitter gate flip** for short
      positions. With the data layer in place these are now
      straightforward follow-ups rather than blocked work.
- [ ] Per-child fill PRICE (not just qty). The current splitter
      shares the parent's `executionPrice` across every child
      because `broker.Simulator` returns a single fill; once the
      venue path produces distinct intraday prices, change
      `splitParentIntoChildren` to return
      `[]struct{Qty int; Price float64}` and aggregate the parent
      row's `filled_price` as the qty-weighted average.
- [ ] Aggregate slippage across distinct-price children for the
      parent row (today every child + parent carry the same
      slippage because the price is shared).
- [x] **`plan_action.execution_status` wiring landed (T6
      commit).** `tradeRepoCreateAndFill` now returns
      `(rolledStatus string, err error)`. The non-split path
      returns `""` (caller falls back to its own status
      decision); the split path returns the
      `aggregateChildrenStatus` result. Caller helpers
      (`executePlanAction` equity + futures branches) prefer
      `rolledStatus` when non-empty, so when the live-broker
      integration eventually emits genuinely partial / rejected
      child statuses the splitter's roll-up flows straight
      into `plan_actions.execution_status` without any further
      code changes. Two new test assertions pin the contract:
      buy-TWAP split path returns `rolledStatus="filled"`, and
      the flag-off single-row path returns `rolledStatus=""`
      so a regression that flipped the wire's polarity would
      light up.
- [x] **Daily-review LLM TWAP-aware signals (T5 commit).**
      Pre-T5 `summarizeTrades` treated every trade row as an
      independent fill, so a single TWAP plan_action that
      split into 1 parent + 5 children DOUBLED the daily
      counters: total=6 not 1, fillRatio double-counted child
      fills against the same plan quantity. This silently
      inflated trader-role hits/misses and confused the LLM
      prompt's "how many fills did we do today?" anchor.
      Post-T5 the helper skips rows whose
      `strategy_parent_trade_id` is set when computing
      total / filled / partial / rejected, but still feeds
      the children's filled_qty into the fillRatio numerator
      (parent.filled_qty equals the sum of child fills so
      we'd double-count otherwise) and exposes new
      `twapSliceCount` / `twapParentCount` fields that the
      trader-role prompt uses to surface "N parents went
      TWAP, avg M slices each". 7-case matrix test pins
      legacy / split / partial-fill / mixed-standalone /
      rejection / empty corner cases.
- [ ] Defer-add a `FOREIGN KEY (strategy_parent_trade_id)
      REFERENCES trade_executions(id) ON DELETE SET NULL` once
      we have production evidence that the buy-path splitter
      never produces orphans. The current migration skips the FK
      on purpose (parent + children INSERT in the same tx but the
      parent's UUID is only available at RETURNING time, so a
      non-deferred FK would force a partition split that's not
      worth the lock surface during rollout).

### Why we did step 1 separately

Three reasons:

1. **Audit value lands immediately.** Today's logs already tell us
   which orders *should have* used TWAP — the OCS-Selection 2026-06
   investigation flagged a 4000-share buy that filled in one shot,
   and we want that visible in production before the splitter
   rewrites the runtime.
2. **No schema risk in step 1.** Step 2 needs a column on a
   high-traffic table; doing it as part of a multi-week splitter
   rollout would block downstream analytics. Splitting the migration
   into a separate PR lets ops review it on its own.
3. **TraderAgent.selectStrategy decision rule is reused as-is.**
   Both steps share `pmPathSplitThreshold = 1000`, so step 2's
   children will continue to be tagged with the same strategy labels
   step 1 is already writing — the historical log lines stay
   compatible.
