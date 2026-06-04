-- 089_cash_ledger_futures_entries.sql
-- T7 follow-up to the trader-agent step-2 integration.
--
-- Adds three new entry_type values to cash_ledger so the runtime
-- can record futures cash flow correctly: a futures open posts
-- margin (cash out), a futures close releases margin (cash in)
-- AND books a separate realized PnL line. Pre-T7 the runtime
-- treated a futures buy as trade_buy_notional (-full notional)
-- and a futures sell as trade_sell_notional (+full notional),
-- which over-states cash movement by a factor of (1 - 1/leverage)
-- and never books realized PnL into the journal at all (the
-- in-memory funds.current_capital was the only place the PnL
-- showed up).
--
-- Behaviour gate: the runtime side of this is guarded by a per-
-- fund feature flag (fund.config.futures_cash_ledger_v2). Funds
-- that don't opt in keep the legacy trade_buy_notional /
-- trade_sell_notional path so this migration is a pure additive
-- vocabulary change with zero immediate effect on cash math.
-- The flag flip is the breaking change; this migration just
-- unblocks the runtime from being able to write the new rows.
--
-- The CHECK constraint is REPLACED (not extended) because
-- PostgreSQL doesn't have a CHECK-add-value primitive. Drop +
-- re-create is atomic inside the transaction so concurrent
-- writers can't slip a now-invalid row through the gap.

BEGIN;

ALTER TABLE cash_ledger DROP CONSTRAINT IF EXISTS cash_ledger_entry_type_chk;
ALTER TABLE cash_ledger ADD CONSTRAINT cash_ledger_entry_type_chk CHECK (
    entry_type IN (
        -- Equity trade legs (P1-1 / migration 056).
        'trade_buy_notional',
        'trade_buy_commission',
        'trade_buy_transfer_fee',
        'trade_buy_stamp_tax',
        'trade_sell_notional',
        'trade_sell_commission',
        'trade_sell_transfer_fee',
        'trade_sell_stamp_tax',
        -- Corporate actions.
        'dividend_cash',
        -- Recurring fund fees.
        'fee_management',
        'fee_performance',
        'fee_platform',
        -- LP cash movements.
        'funding_deposit',
        'funding_withdrawal',
        -- Reconciliation / corrections.
        'adjustment',
        'reversal',
        -- S6.4 short-borrow line items (migration 066).
        'borrow_fee',
        'locate_fee',
        -- T7 futures cash flow:
        --
        --   futures_margin_post     — debit at open; amount = -initial_margin
        --   futures_margin_release  — credit at close; amount = +initial_margin
        --   futures_realized_pnl    — signed at close;
        --                              long close: +(close - cost) * qty * mult
        --                              short close: +(cost - close) * qty * mult
        --
        -- Commission / transfer fees on a futures trade reuse the
        -- existing trade_buy_commission / trade_sell_commission etc.
        -- so reports that subtotal "commissions paid" don't need
        -- separate futures-specific buckets.
        'futures_margin_post',
        'futures_margin_release',
        'futures_realized_pnl'
    )
);

COMMIT;
