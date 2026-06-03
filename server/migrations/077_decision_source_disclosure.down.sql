DROP INDEX IF EXISTS idx_investment_plans_fallback;
ALTER TABLE investment_plans
  DROP COLUMN IF EXISTS fallback_reason,
  DROP COLUMN IF EXISTS decision_source;
