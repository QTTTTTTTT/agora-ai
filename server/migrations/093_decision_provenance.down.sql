-- 093_decision_provenance.down.sql

DROP INDEX IF EXISTS idx_investment_plans_provenance_skills;
DROP INDEX IF EXISTS idx_investment_plans_provenance_lessons;

ALTER TABLE investment_plans
    DROP COLUMN IF EXISTS decision_provenance;
