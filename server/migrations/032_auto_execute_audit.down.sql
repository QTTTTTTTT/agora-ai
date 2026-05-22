-- 032_auto_execute_audit.down.sql
DROP INDEX IF EXISTS idx_plan_actions_auto_executed_at;
ALTER TABLE plan_actions DROP COLUMN IF EXISTS auto_executed_at;
