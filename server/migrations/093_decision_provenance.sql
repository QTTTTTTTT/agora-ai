-- 093_decision_provenance.sql — W1-4: capture which signals/lessons/skills
-- shaped each plan, so Wave-2 self-learning loops can close.
--
-- WHY THIS EXISTS
-- ---------------
-- We already record what the LLM produced (`risk_review`,
-- `discussion_snapshot`, `confidence`, `decision_source`,
-- `block_contributions`). What we DON'T record is what the LLM was
-- TOLD when it produced that plan:
--
--   * which signal blocks were spliced into the prompt
--     (regime / exposure / quality / value / news / earnings / …);
--   * which alpha-tagged lessons were retrieved by
--     alphalesson.BuildContext as soft priors;
--   * which agent skills (the long-term reflection-distilled
--     candidate skills under skill_config.skills) were active.
--
-- Without this provenance the Wave-2 calibration / skill-effectiveness
-- / lesson-refute trackers can't trace "this plan misfired BECAUSE
-- it leaned on lesson X" — they can only see the outcome, not the
-- cause. The Brier-score reliability diagram for agent calibration
-- (Wave 2 #7) needs the per-plan confidence + outcome already
-- recorded; the lesson-refute path (#9) and skill-effectiveness
-- tracker (#8) need this new column.
--
-- SHAPE
-- -----
-- One JSONB column rather than three. Everything that is metadata
-- about how the plan was produced lives here:
--
--   {
--     "promptBlocks":   ["regime", "exposure", "qualityScores", ...],
--     "lessonsUsed":    [
--       {"id": "<memory_uuid>", "kind": "alpha_tagged", "agentTag": "fundamentals"}
--     ],
--     "skillsUsed":     [
--       {"agentId": "<agent_uuid>", "skillKey": "wait_for_fundamental_confirmation"}
--     ],
--     "signalCount":    23,
--     "promptTokens":   12480,
--     "completionTokens": 1834,
--     "promptHash":     "sha256:abcd…"   -- canonical fingerprint for diff
--   }
--
-- Schema is intentionally flexible — JSONB on Postgres lets us add
-- fields later without an ALTER TABLE migration. The Go side owns
-- the canonical shape.

ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS decision_provenance JSONB;

-- GIN index on the keys the Wave-2 trackers will ask about. The
-- WHERE clause keeps the index narrow (only plans where we
-- actually captured provenance) so it stays cheap to maintain.
CREATE INDEX IF NOT EXISTS idx_investment_plans_provenance_lessons
    ON investment_plans USING GIN ((decision_provenance -> 'lessonsUsed'))
    WHERE decision_provenance IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_investment_plans_provenance_skills
    ON investment_plans USING GIN ((decision_provenance -> 'skillsUsed'))
    WHERE decision_provenance IS NOT NULL;

COMMENT ON COLUMN investment_plans.decision_provenance IS
    'JSONB capturing prompt provenance: promptBlocks (string[]), lessonsUsed ({id,kind,agentTag}[]), skillsUsed ({agentId,skillKey}[]), signalCount, promptTokens, completionTokens, promptHash. Soft-fail: NULL is "we did not capture provenance for this plan" (legacy / pre-W1-4 rows).';
