-- Down migration for 092_backfill_agent_portable_lessons.sql.
--
-- Reverts the visibility='agent_portable' relabel back to
-- 'fund' for ROWS THAT WERE TOUCHED BY 092. The up migration
-- left a sentinel 'ap6_backfilled' tag on every relabelled row
-- precisely so we can identify them here without ambiguity.
--
-- Rows that have visibility='agent_portable' but do NOT carry
-- the sentinel were written that way by the AP2 writer (post-
-- migration native rows); we leave them alone.
--
-- Pre-conditions for safe rollback:
--   * The migration ran exactly once. Re-running would not
--     re-stamp anything (idempotent) but would still leave a
--     consistent sentinel population for the down migration.
--   * Nothing outside the AP6 backfill path writes the
--     'ap6_backfilled' tag. This is an invariant — if a future
--     process needs to mark relabelled rows it MUST use a
--     different tag.

BEGIN;

DO $$
DECLARE
    reverted_count INTEGER;
BEGIN
    UPDATE memories
       SET visibility = 'fund',
           tags       = array_remove(tags, 'ap6_backfilled')
     WHERE visibility = 'agent_portable'
       AND tags && ARRAY['ap6_backfilled']::text[];

    GET DIAGNOSTICS reverted_count = ROW_COUNT;
    RAISE NOTICE 'migration 092 down: reverted % rows back to visibility=fund', reverted_count;
END $$;

COMMIT;
