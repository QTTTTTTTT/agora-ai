-- 094_plan_outcome.down.sql

DROP INDEX IF EXISTS idx_investment_plans_outcome_window_kind;
DROP INDEX IF EXISTS idx_investment_plans_pending_outcome;

ALTER TABLE investment_plans
    DROP COLUMN IF EXISTS plan_outcome;
