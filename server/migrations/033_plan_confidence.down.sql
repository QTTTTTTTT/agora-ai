-- 033_plan_confidence.down.sql
DROP INDEX IF EXISTS idx_investment_plans_confidence;
ALTER TABLE investment_plans DROP COLUMN IF EXISTS confidence;
