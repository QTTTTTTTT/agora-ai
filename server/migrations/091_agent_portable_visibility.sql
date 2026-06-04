-- Migration 091: agent-portable lesson visibility.
--
-- Why this exists.
-- ----------------
-- A researcher agent (memories.agent_id) accumulates lessons
-- specific to its instrument coverage — e.g. "NVDA tends to gap
-- up on earnings beat". Pre-091 every such lesson is hard-bound
-- to memories.fund_id: when the same agent joins a SECOND fund's
-- team (the platform already supports this via fund_team_members
-- being many-to-many between funds and agents), the new fund's
-- prompt-builder cannot see those lessons because the retrieval
-- query filters strictly on fund_id.
--
-- That breaks the "agent is a portable specialist" model: a
-- human equity researcher who moves from Fund A to Fund B brings
-- their NVDA notes with them. The platform should match that.
--
-- What changes.
-- -------------
-- Adds a fourth value to memories.visibility:
--
--   private          (visibility=private)         already existed
--   fund             (visibility=fund)            already existed
--   marketplace      (visibility=marketplace)     already existed
--   agent_portable   (visibility=agent_portable)  NEW IN THIS MIGRATION
--
-- Semantics of agent_portable:
--   * The row is owned by an agent_id (memories.agent_id), not
--     by fund_id alone. fund_id stays populated for provenance
--     ("learned at fund X") but is no longer a retrieval filter.
--   * At retrieval time, every fund whose fund_team_members
--     contains memories.agent_id can see the row, subject to
--     fund.config.allow_agent_portable_imports (default true).
--   * sensitivity='secret' still overrides — secret rows stay
--     fund-private even if their visibility is agent_portable
--     (see AP7: writer enforces, reader double-checks).
--
-- Per-fund opt-out (fund.config.allow_agent_portable_imports).
-- ------------------------------------------------------------
-- This migration ONLY touches schema. The opt-out flag lives in
-- fund.config (JSONB) which needs no schema change. The flag is
-- read in the alphalesson read path (AP3). Default semantics:
--
--   {} or omitted         → opt-in (we apply the new default)
--   { allow_agent_portable_imports: true  }  → opt-in (explicit)
--   { allow_agent_portable_imports: false }  → OPT-OUT — the
--       fund's prompt-builder only sees lessons that physically
--       carry its own fund_id. This is the escape hatch for
--       multi-LP / regulated funds whose lessons can never leak
--       across organisational boundaries.
--
-- Index strategy.
-- ---------------
-- The hot read pattern is:
--
--   SELECT ... FROM memories
--    WHERE visibility = 'agent_portable'
--      AND agent_id   = ANY($team_agent_ids)
--    ORDER BY created_at DESC LIMIT $K
--
-- A partial index keyed on (agent_id, created_at DESC) WHERE
-- visibility='agent_portable' is the right shape:
--   * Partial because agent_portable rows will be a minority of
--     total memories (most platform rows are visibility='fund').
--   * Leading agent_id for the equality join against the team's
--     agent set; trailing created_at DESC so the ORDER BY can
--     be answered from the index without a sort.
--
-- Backfill of memories.agent_id.
-- ------------------------------
-- alphalesson.WriteAlphaLessons currently writes the agent's UUID
-- into memories.agent_tag (text) without populating the
-- memories.agent_id FK column. To enable the new agent_id-based
-- retrieval path on EXISTING rows, this migration ALSO backfills
-- memories.agent_id from memories.agent_tag where:
--   * agent_id IS NULL (never set)
--   * agent_tag IS NOT NULL AND matches UUID format
--   * The agent_tag value points to a real row in agents(id)
-- We tolerate (silently skip) rows where the cast fails — those
-- are tag-style rows ('pm_role' / 'sleeve_momentum') that have
-- always been non-FK.
--
-- The backfill is a sibling concern (AP6 will do the
-- visibility='fund' → 'agent_portable' relabel for historical
-- researcher lessons). This migration deliberately keeps the two
-- backfills separate so a fund operator can roll AP6 back
-- without losing the agent_id FK plumbing.

BEGIN;

-- 1. Replace the visibility CHECK constraint with the 4-value
--    version. Postgres auto-named the original constraint
--    memories_visibility_check at ADD COLUMN time (migration 020).
ALTER TABLE memories
    DROP CONSTRAINT IF EXISTS memories_visibility_check;

ALTER TABLE memories
    ADD CONSTRAINT memories_visibility_check
    CHECK (visibility IN ('private', 'fund', 'marketplace', 'agent_portable'));

-- 2. Partial index for the agent-portable read path. This is
--    the index that turns the OR-merge query into two cheap
--    index scans instead of a full memories scan.
CREATE INDEX IF NOT EXISTS idx_memories_agent_portable
    ON memories (agent_id, created_at DESC)
    WHERE visibility = 'agent_portable';

-- 3. Backfill memories.agent_id from agent_tag where the tag
--    parses as a UUID matching an existing agents.id. The
--    DO block tolerates cast failures (tag-style non-UUID tags)
--    so we don't have to filter agent_tag with a regex up front.
DO $$
DECLARE
    updated_count INTEGER;
BEGIN
    WITH parseable AS (
        SELECT m.id, m.agent_tag::uuid AS parsed_id
          FROM memories m
         WHERE m.agent_id IS NULL
           AND m.agent_tag IS NOT NULL
           AND m.agent_tag ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
    ),
    matched AS (
        SELECT p.id, p.parsed_id
          FROM parseable p
          JOIN agents a ON a.id = p.parsed_id
    )
    UPDATE memories m
       SET agent_id = matched.parsed_id
      FROM matched
     WHERE m.id = matched.id;

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RAISE NOTICE 'migration 091: backfilled agent_id on % rows', updated_count;
END $$;

COMMENT ON CONSTRAINT memories_visibility_check ON memories IS
    'Visibility ACL. agent_portable means the row follows the agent across funds — '
    'every fund whose fund_team_members contains memories.agent_id can read it, '
    'subject to fund.config.allow_agent_portable_imports (default true) and '
    'sensitivity != ''secret'' which overrides cross-fund propagation. '
    'See migration 091 + ADR docs/AGENT_PORTABLE_LEARNING.md.';

COMMIT;
