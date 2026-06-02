-- Down migration for 056_cash_ledger.

DROP TRIGGER IF EXISTS cash_ledger_touch_updated_at ON cash_ledger;
DROP FUNCTION IF EXISTS cash_ledger_touch_updated_at();
DROP TABLE IF EXISTS cash_ledger;
