DROP TABLE IF EXISTS short_position_borrow_ledger;
DROP TABLE IF EXISTS security_locate_events;
DROP TABLE IF EXISTS security_borrow_rates;

-- Restore the original cash_ledger entry_type CHECK.
ALTER TABLE cash_ledger_entries DROP CONSTRAINT IF EXISTS cash_ledger_entry_type_chk;
ALTER TABLE cash_ledger_entries ADD CONSTRAINT cash_ledger_entry_type_chk CHECK (
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
        'reversal'
    )
);
