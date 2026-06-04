# Trader Agent Integration — Status & Roadmap

> Doc owner: trading runtime / Tracking issue: B-step2 (TBD)
> Status as of 2026-06-04: **step 1 done; step 2 equity buy + equity long-side sell landed; futures + short-side pending**

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

**Deferred to follow-up PRs (still TODO):**

- [ ] Wire **futures long open / close** through the splitter
      (today the gate excludes asset_class=futures across the
      board). The blocker is `recordCashLedgerForFill`: it
      writes one SellNotional cash row for any sell-side fill
      regardless of asset_class. For equity that's correct
      (proceeds hit cash); for a futures close it's a
      simplification — the real entries should be per-child
      margin release + a single realized PnL booking. The
      legacy single-row path has the same simplification, but
      the splitter must not amplify it 5× across children until
      `recordCashLedgerForFill` learns the asset_class=futures
      branch. Touches the cash ledger, not the splitter itself.
- [ ] Wire **futures short / equity short** (the symmetric
      blocker on the position_side axis) — needs the parallel
      short-lot ledger to land before the splitter can fan out
      a short open/close without the lot writes turning into
      silent no-ops.
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
- [ ] `plan_action.execution_status` aggregation: when a parent
      partially fills (3 of 5 TWAP slices land), roll the children
      into one parent-level status string so the planner UI shows
      "partial: 60% filled" rather than 5 individual rows.
- [ ] Daily-review LLM context builder: surface "TWAP intent on
      N shares, M% filled by close" as a Trader-role learning
      signal (the structured rows are already on disk now;
      `buildAgentLearning` just needs to query them).
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
