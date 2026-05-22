-- 033_plan_confidence.sql
--
-- Add a first-class plan-level confidence column so the
-- LLMDecisionEngine's plan-confidence output (Phase 2A) is queryable
-- without parsing the risk_review JSON blob. The same value is still
-- written into risk_review for human-readable audit, but the
-- auto-execute gate (autoExecuteGateCheck.MinConfidence) prefers the
-- typed column when it's NOT NULL — JSON parsing is the fallback
-- path for legacy plans.
--
-- Range: 0..1 (NUMERIC(4,3) is plenty: 0.000..1.000). NULL means
-- "engine did not provide a confidence" — typically the deterministic
-- FallbackEngine path or a legacy plan written before this column
-- existed. The gate treats NULL the same as 0, which (combined with
-- the default 0.60 floor) ensures auto-execute never picks up plans
-- that lack a confidence signal.
--
-- We deliberately don't backfill historical rows: legacy plans are
-- manually-approved by construction, and a backfill would have to
-- invent a confidence value out of thin air.

ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS confidence NUMERIC(4, 3);

COMMENT ON COLUMN investment_plans.confidence IS
    'Plan-level confidence in [0,1] produced by the LLM-driven decision engine (Phase 2A). NULL for fallback-engine / legacy plans. Used by the auto-execute gate (autoExecuteGateCheck.MinConfidence) and by analytics dashboards.';

-- Operator analytics index — "show me plans below confidence X this
-- month". Partial index keeps it cheap.
CREATE INDEX IF NOT EXISTS idx_investment_plans_confidence
    ON investment_plans (confidence)
    WHERE confidence IS NOT NULL;
