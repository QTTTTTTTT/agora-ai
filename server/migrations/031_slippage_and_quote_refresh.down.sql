-- Down migration for 031_slippage_and_quote_refresh.sql. Used by the
-- integration test harness and by operators rolling back a bad deploy.
DROP INDEX IF EXISTS idx_trade_executions_slippage_abs;
ALTER TABLE trade_executions DROP COLUMN IF EXISTS slippage_pct;
ALTER TABLE plan_actions     DROP COLUMN IF EXISTS quote_refreshed_at;
