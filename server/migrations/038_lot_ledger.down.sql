-- Reverse of 038_lot_ledger.sql

DROP INDEX IF EXISTS idx_closed_lots_exit_reason;
DROP INDEX IF EXISTS idx_closed_lots_fund_window;
DROP INDEX IF EXISTS idx_closed_lots_symbol_window;
DROP INDEX IF EXISTS idx_closed_lots_regime_window;
DROP INDEX IF EXISTS idx_closed_lots_sleeve_window;
DROP TABLE IF EXISTS closed_lots;

DROP INDEX IF EXISTS idx_position_lots_fund;
DROP INDEX IF EXISTS idx_position_lots_open_fifo;
DROP TABLE IF EXISTS position_lots;

ALTER TABLE plan_actions DROP COLUMN IF EXISTS exit_reason;
ALTER TABLE plan_actions DROP COLUMN IF EXISTS signal_source;
ALTER TABLE plan_actions DROP COLUMN IF EXISTS regime_tag;
ALTER TABLE plan_actions DROP COLUMN IF EXISTS sleeve;
