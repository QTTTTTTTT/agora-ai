-- F16: idempotency end-to-end audit.
--
-- Audit findings (pre-F16):
--
--   table                | idempotency status                       | risk
--   ---------------------|------------------------------------------|------------------
--   wallet_ledger        | has idempotency_key (UNIQUE)             | OK
--   wallet_holds         | has idempotency_key (UNIQUE)              | OK
--   marketplace_orders   | has idempotency_key (UNIQUE)             | OK
--   investment_plans     | NO key; duplicates possible on retry     | medium (workflow MaxAttempts=1 mitigates)
--   trade_executions     | NO key; duplicates possible on broker retry | high (real $ when live)
--   nav_snapshots        | NO unique (fund, date); duplicates corrupt P&L | high
--   memories/reflections | content-addressed ID                     | OK (de-dup via SHA-1)
--   workflow_runs        | UNIQUE (fund_id, trading_date)           | OK
--
-- This migration patches the three gaps:
--
--   1. nav_snapshots: enforce UNIQUE (fund_id, trading_date) so an
--      accidental double-INSERT (e.g. from F12 recovery or admin
--      manual trigger) becomes an UPSERT instead of a silent duplicate.
--
--   2. investment_plans + trade_executions: add optional
--      client_idempotency_key TEXT column with partial UNIQUE index.
--      Callers (workflow orchestrator, broker integration) pass
--      run_id|step|action_id and a duplicate write is collapsed to a
--      no-op. Existing rows have NULL keys and are unaffected.
--
-- The partial index pattern (WHERE col IS NOT NULL) means rows without
-- a key are NOT subject to uniqueness — gives a safe rollout path
-- where new code starts populating the key while legacy rows stay
-- valid.

-- ---------------------------------------------------------------------------
-- 1. nav_snapshots — UPSERT key on (fund_id, trading_date)
-- ---------------------------------------------------------------------------

-- First, dedupe any pre-existing duplicates (keep the latest) so the
-- UNIQUE constraint can be added without failure on dirty test DBs.
-- We use ctid to break ties when (fund_id, trading_date) appear twice.
DELETE FROM nav_snapshots a
USING nav_snapshots b
WHERE a.fund_id = b.fund_id
  AND a.trading_date = b.trading_date
  AND a.ctid < b.ctid;

ALTER TABLE nav_snapshots
    ADD CONSTRAINT nav_snapshots_fund_date_uniq UNIQUE (fund_id, trading_date);

-- ---------------------------------------------------------------------------
-- 2. investment_plans — optional client_idempotency_key
-- ---------------------------------------------------------------------------

ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS client_idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS investment_plans_idem_key_uniq
    ON investment_plans (client_idempotency_key)
    WHERE client_idempotency_key IS NOT NULL;

COMMENT ON COLUMN investment_plans.client_idempotency_key IS
    'Optional caller-supplied idempotency key. Workflow orchestrator passes <run_id>|<step> so a retried PM-plan step collapses to the existing row.';

-- ---------------------------------------------------------------------------
-- 3. trade_executions — optional client_idempotency_key
-- ---------------------------------------------------------------------------

ALTER TABLE trade_executions
    ADD COLUMN IF NOT EXISTS client_idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS trade_executions_idem_key_uniq
    ON trade_executions (client_idempotency_key)
    WHERE client_idempotency_key IS NOT NULL;

COMMENT ON COLUMN trade_executions.client_idempotency_key IS
    'Optional caller-supplied idempotency key. Broker integration passes <plan_action_id>|<order_attempt> so a network-retry of the same submission collapses to the original execution row instead of double-submitting.';
