-- Migration rollback: 099_advisor_reputation.down
--
-- Reverses migration 099 — restores the original NOT NULL on
-- fund_id, the original CHECK constraints on agent_kind /
-- direction, and drops the advisor partial indexes.
--
-- Pre-requisite: all advisor-mode rows (fund_id IS NULL) must be
-- removed first; the down migration will fail loudly if any remain.

BEGIN;

-- Refuse to roll back when advisor rows would leave the original
-- NOT NULL constraint un-satisfiable.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM agent_reputation_outcomes WHERE fund_id IS NULL) THEN
        RAISE EXCEPTION
            'Cannot roll back 099 — advisor-mode rows still present in agent_reputation_outcomes';
    END IF;
    IF EXISTS (SELECT 1 FROM agent_reputation_stats WHERE fund_id IS NULL) THEN
        RAISE EXCEPTION
            'Cannot roll back 099 — advisor-mode rows still present in agent_reputation_stats';
    END IF;
END $$;

DROP INDEX IF EXISTS uq_agent_reputation_outcomes_advisor;
DROP INDEX IF EXISTS idx_agent_rep_outcomes_advisor_kind_asof;
DROP INDEX IF EXISTS idx_agent_rep_stats_advisor;
DROP INDEX IF EXISTS uq_agent_reputation_stats_fund;
DROP INDEX IF EXISTS uq_agent_reputation_stats_advisor;

ALTER TABLE agent_reputation_stats
    ADD CONSTRAINT agent_reputation_stats_pkey PRIMARY KEY (fund_id, agent_id);

ALTER TABLE agent_reputation_outcomes
    DROP CONSTRAINT IF EXISTS agent_reputation_outcomes_direction_check;
ALTER TABLE agent_reputation_outcomes
    ADD  CONSTRAINT agent_reputation_outcomes_direction_check
    CHECK (direction IN ('bullish', 'bearish', 'neutral'));

ALTER TABLE agent_reputation_outcomes
    DROP CONSTRAINT IF EXISTS agent_reputation_outcomes_agent_kind_check;
ALTER TABLE agent_reputation_outcomes
    ADD  CONSTRAINT agent_reputation_outcomes_agent_kind_check
    CHECK (agent_kind IN ('analyst', 'advocate', 'pm', 'researcher'));

ALTER TABLE agent_reputation_stats
    DROP CONSTRAINT IF EXISTS agent_reputation_stats_agent_kind_check;
ALTER TABLE agent_reputation_stats
    ADD  CONSTRAINT agent_reputation_stats_agent_kind_check
    CHECK (agent_kind IN ('analyst', 'advocate', 'pm', 'researcher'));

ALTER TABLE agent_reputation_outcomes
    ALTER COLUMN fund_id SET NOT NULL;
ALTER TABLE agent_reputation_stats
    ALTER COLUMN fund_id SET NOT NULL;

COMMIT;
