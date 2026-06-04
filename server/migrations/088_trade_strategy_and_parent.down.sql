-- Down migration for 088. Drops the index, the CHECK constraint,
-- and the two columns. Pre-088 rows are unaffected (they already
-- had strategy=NULL / strategy_parent_trade_id=NULL).

DROP INDEX IF EXISTS idx_trade_executions_strategy_parent;

ALTER TABLE trade_executions
    DROP CONSTRAINT IF EXISTS trade_executions_strategy_check;

ALTER TABLE trade_executions
    DROP COLUMN IF EXISTS strategy_parent_trade_id,
    DROP COLUMN IF EXISTS strategy;
