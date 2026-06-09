-- Migration: 099_advisor_reputation
-- Description:
--   Phase 5 — extend the existing agent_reputation_outcomes ledger so
--   it can also hold rows produced by the /advisor surface (where there
--   is no fund_id and the agent_kind values are 'master' and 'tactic'
--   instead of 'analyst' / 'advocate' / 'pm' / 'researcher').
--
--   Why piggy-back on the existing ledger instead of adding a third
--   table: the rollup UI already knows how to render
--   (decisions_count / hits_count / avg_alpha / last_decision_at)
--   from agent_reputation_stats; adding a parallel table would mean
--   duplicating the entire admin / per-fund / public rollup surface.
--
--   Changes:
--     1. Drop the NOT NULL on fund_id so advisor decisions can use
--        NULL (we keep the FK so legacy fund-scoped rows still
--        cascade on fund deletion).
--     2. Relax the agent_kind CHECK to also accept 'master' and
--        'tactic'.
--     3. Relax the direction CHECK to also accept 'buy' / 'avoid' /
--        'skip' so MasterAgent / TacticAgent verdicts map cleanly
--        without forcing every advisor row into bullish/bearish.
--     4. Extend the UNIQUE constraint to treat NULL fund_id as
--        equivalent for advisor rows — Postgres treats NULL as
--        distinct in unique indexes so we add a partial index
--        scoped to advisor rows.
--
--   Schema impact: backward-compatible. Existing fund-scoped rows
--   keep working; new advisor rows use fund_id IS NULL.

BEGIN;

-- 1. Allow NULL fund_id.
ALTER TABLE agent_reputation_outcomes
    ALTER COLUMN fund_id DROP NOT NULL;

-- agent_reputation_stats currently has PRIMARY KEY (fund_id, agent_id);
-- a PK rejects NULL by definition, so we have to drop the PK and
-- replace it with two unique indexes — one for fund-scoped rows and
-- a partial one for advisor rows. The application upserts use these
-- as conflict targets.
ALTER TABLE agent_reputation_stats
    DROP CONSTRAINT IF EXISTS agent_reputation_stats_pkey;
ALTER TABLE agent_reputation_stats
    ALTER COLUMN fund_id DROP NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_reputation_stats_fund
    ON agent_reputation_stats (fund_id, agent_id)
    WHERE fund_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_reputation_stats_advisor
    ON agent_reputation_stats (agent_id)
    WHERE fund_id IS NULL;

-- 2. Broaden agent_kind to cover advisor personas.
ALTER TABLE agent_reputation_outcomes
    DROP CONSTRAINT IF EXISTS agent_reputation_outcomes_agent_kind_check;
ALTER TABLE agent_reputation_outcomes
    ADD  CONSTRAINT agent_reputation_outcomes_agent_kind_check
    CHECK (agent_kind IN ('analyst', 'advocate', 'pm', 'researcher', 'master', 'tactic'));

ALTER TABLE agent_reputation_stats
    DROP CONSTRAINT IF EXISTS agent_reputation_stats_agent_kind_check;
ALTER TABLE agent_reputation_stats
    ADD  CONSTRAINT agent_reputation_stats_agent_kind_check
    CHECK (agent_kind IN ('analyst', 'advocate', 'pm', 'researcher', 'master', 'tactic'));

-- 3. Broaden direction to accept advisor verdict mappings.
ALTER TABLE agent_reputation_outcomes
    DROP CONSTRAINT IF EXISTS agent_reputation_outcomes_direction_check;
ALTER TABLE agent_reputation_outcomes
    ADD  CONSTRAINT agent_reputation_outcomes_direction_check
    CHECK (direction IN (
        'bullish', 'bearish', 'neutral',
        'buy', 'avoid', 'skip', 'wait'
    ));

-- 4. Partial unique index for advisor rows. The existing UNIQUE
--    (fund_id, agent_id, symbol, asof, horizon_days) already keeps
--    NULL fund_id rows distinct by symbol+asof+horizon because
--    Postgres treats NULL as distinct, but two advisor rows for
--    the same (agent_id, symbol, asof, horizon) would slip through.
--    The partial unique fixes that.
CREATE UNIQUE INDEX IF NOT EXISTS uq_agent_reputation_outcomes_advisor
    ON agent_reputation_outcomes (agent_id, symbol, asof, horizon_days)
    WHERE fund_id IS NULL;

CREATE INDEX IF NOT EXISTS idx_agent_rep_outcomes_advisor_kind_asof
    ON agent_reputation_outcomes (agent_kind, asof DESC)
    WHERE fund_id IS NULL;

-- 5. Sibling for the stats table — every advisor stats row keys on
--    (NULL, agent_id), and PRIMARY KEY (fund_id, agent_id) already
--    accepts that combination once we allow NULL fund_id. We just
--    need an extra partial index for fast list lookups in the
--    public-facing track record handler.
CREATE INDEX IF NOT EXISTS idx_agent_rep_stats_advisor
    ON agent_reputation_stats (agent_kind, agent_id)
    WHERE fund_id IS NULL;

COMMIT;
