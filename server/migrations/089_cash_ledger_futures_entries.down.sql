-- 089_cash_ledger_futures_entries.down.sql
--
-- Rollback for 089. Drops the three futures-specific entry_type
-- values from the CHECK. SAFE only when no fund.config has the
-- futures_cash_ledger_v2 flag turned on AND no row of the new
-- types has been written; if either is true, the rollback will
-- leave invalid rows pointing at a CHECK that no longer admits
-- them. The runtime gate (default false) is designed so this
-- precondition holds for funds that never opted in.

BEGIN;

ALTER TABLE cash_ledger DROP CONSTRAINT IF EXISTS cash_ledger_entry_type_chk;
ALTER TABLE cash_ledger ADD CONSTRAINT cash_ledger_entry_type_chk CHECK (
    entry_type IN (
        'trade_buy_notional',
        'trade_buy_commission',
        'trade_buy_transfer_fee',
        'trade_buy_stamp_tax',
        'trade_sell_notional',
        'trade_sell_commission',
        'trade_sell_transfer_fee',
        'trade_sell_stamp_tax',
        'dividend_cash',
        'fee_management',
        'fee_performance',
        'fee_platform',
        'funding_deposit',
        'funding_withdrawal',
        'adjustment',
        'reversal',
        'borrow_fee',
        'locate_fee'
    )
);

COMMIT;
