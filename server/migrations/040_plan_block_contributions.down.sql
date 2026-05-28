DROP INDEX IF EXISTS idx_investment_plans_block_contributions_gin;
ALTER TABLE investment_plans
    DROP COLUMN IF EXISTS block_contributions;
