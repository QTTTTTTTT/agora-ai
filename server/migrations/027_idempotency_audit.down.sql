-- F31 down for F16 (027_idempotency_audit.sql).
--
-- This is a destructive teardown: dropping the unique constraint on
-- nav_snapshots could leave behind duplicate rows that previously
-- collapsed via UPSERT. Operators MUST treat any down-run as
-- "schema-level only, do not re-run upstream code with old assumptions"
-- — applications should be stopped before running this migration.

DROP INDEX IF EXISTS trade_executions_idem_key_uniq;
DROP INDEX IF EXISTS investment_plans_idem_key_uniq;

ALTER TABLE trade_executions  DROP COLUMN IF EXISTS client_idempotency_key;
ALTER TABLE investment_plans  DROP COLUMN IF EXISTS client_idempotency_key;

ALTER TABLE nav_snapshots DROP CONSTRAINT IF EXISTS nav_snapshots_fund_date_uniq;
