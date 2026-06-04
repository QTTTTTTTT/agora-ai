# Trader Agent Integration — Status & Roadmap

> Doc owner: trading runtime / Tracking issue: B-step2 (TBD)
> Status as of 2026-06-04: **step 1 of 2 done**

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

### B-step 2 — real child-order splitting (**TODO**)

When this lands, the picture becomes:

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

Step 2 work items:

- [ ] Add `trade_executions.strategy TEXT NULL` column (migration
      087).
- [ ] Add `TradeExecution.Strategy sql.NullString` field +
      `TradeRepo.Create` INSERT.
- [ ] New helper `splitParentIntoChildren(action, strategy)` that
      returns `[]childOrder{qty, slice_time_offset}` for `twap` /
      `vwap`; identity for `immediate` / `limit`.
- [ ] Wrap each child in the existing `pmPathLotSizeGuard` (child
      qty) and pass `parent_trade_id` to `tradeRepoCreateAndFill`.
- [ ] Aggregate slippage across children for the parent row
      (weighted by filled qty).
- [ ] `cash_ledger` + `position_lots` updates need to be per-child,
      not per-parent — current helpers can be reused unchanged.
- [ ] Add a feature flag so existing funds default to "no
      splitting" (immediate child = parent) until ops sign off.
- [ ] Integration test: a 4000-share buy with `twap` strategy
      produces 4 children with `parent_trade_id` chained.
- [ ] Daily-review LLM context builder: surface "TWAP intent on N
      shares, M% filled by close" as a Trader-role learning signal.

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
