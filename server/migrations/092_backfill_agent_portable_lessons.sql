-- Migration 092: backfill historical alpha lessons to
-- visibility='agent_portable' for researcher / analyst rows.
--
-- Why this exists.
-- ----------------
-- AP1 (migration 091) added 'agent_portable' to the
-- memories.visibility CHECK enum.
-- AP2 made WriteAlphaLessons stamp NEW rows with
-- visibility='agent_portable' when the AgentKind is researcher
-- or analyst.
-- AP3 made ListLessons UNION the cross-fund branch into
-- retrieval.
--
-- But every alpha lesson written BEFORE AP2 went in with
-- visibility='fund'. The new retrieval path only matches
-- agent_portable rows on the cross-fund branch, so without this
-- backfill the AP3 feature has nothing to actually serve to
-- sister funds — the entire backlog of researcher learning
-- stays trapped in its origin fund.
--
-- This migration retroactively re-stamps qualifying rows to
-- 'agent_portable' so the cross-fund retrieval can see them.
--
-- Eligibility criteria.
-- ---------------------
-- A row is relabelled iff ALL of these hold:
--
--   1. visibility = 'fund'
--      We only touch fund-scoped rows. private / marketplace
--      rows have their own semantics (private = single user,
--      marketplace = explicit sale listing) and re-stamping
--      them would break those flows.
--
--   2. origin_kind = 'alpha_lesson'
--      We only touch the alpha-tagged lesson population (S9.1
--      and later). Other origin_kinds ('native' = hand-written
--      operator notes, 'imported_from_marketplace' = bought
--      from another fund) carry no portability invariant and
--      must stay fund-private.
--
--   3. agent_id IS NOT NULL
--      The cross-fund retrieval path joins on agent_id. A row
--      without a populated FK can't be propagated cross-fund
--      regardless of its visibility, so relabelling it would
--      be pure overhead. Migration 091 already backfilled
--      agent_id from agent_tag where the tag parses as a
--      valid UUID; this migration is downstream of that.
--
--   4. sensitivity != 'secret'
--      Mirrors the AP7 reader gate. A secret row never
--      propagates cross-fund — relabelling its visibility
--      would silently downgrade the operator's stated intent.
--
--   5. tags && ARRAY['researcher','analyst']::text[]
--      The original lessonTags() helper stamps the AgentKind
--      into the tags array as a bare value (no namespace
--      prefix). So a researcher lesson has 'researcher' in
--      its tags; an analyst lesson has 'analyst'. We rely on
--      this tag presence rather than joining on agents.role
--      because agentreputation.KindAnalyst doesn't map to any
--      single agents.role value (analysts can be hybrid agents
--      that don't show as role='analyst' in the schema).
--
-- Why no per-fund opt-out on the WRITE side.
-- ------------------------------------------
-- The privacy story belongs to the READ side: a fund that
-- doesn't want to receive other funds' lessons sets
-- fund.config.allow_agent_portable_imports=false, and the AP3
-- read path observes that flag at query time. If we ALSO let
-- funds opt out of having their OWN lessons re-stamped, a
-- common shape ("multi-LP fund prefers not to send out") would
-- start breaking the agent's portability invariant: an agent
-- who moves from opted-out Fund A to Fund B would suddenly
-- have no track record at Fund B, which is the exact thing
-- this whole feature is supposed to prevent.
--
-- The right per-row opt-out lever is sensitivity='secret'
-- on the specific lesson, which the eligibility filter already
-- respects.
--
-- Rollback (092.down.sql).
-- ------------------------
-- Reverts the relabelled rows back to visibility='fund'. The
-- down migration is exact-inverse-capable because we tag
-- relabelled rows with a sentinel 'ap6_backfilled' value in
-- the tags array (no other process writes this tag). The
-- down migration only touches rows that carry the sentinel.

BEGIN;

DO $$
DECLARE
    updated_count INTEGER;
BEGIN
    WITH eligible AS (
        SELECT id
          FROM memories
         WHERE visibility   = 'fund'
           AND origin_kind  = 'alpha_lesson'
           AND agent_id     IS NOT NULL
           AND sensitivity != 'secret'
           AND tags && ARRAY['researcher', 'analyst']::text[]
    )
    UPDATE memories
       SET visibility = 'agent_portable',
           tags       = array_append(tags, 'ap6_backfilled')
      FROM eligible
     WHERE memories.id = eligible.id;

    GET DIAGNOSTICS updated_count = ROW_COUNT;
    RAISE NOTICE 'migration 092: relabelled % alpha lessons to agent_portable', updated_count;
END $$;

COMMIT;
