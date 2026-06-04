-- Down migration for 091_agent_portable_visibility.sql.
--
-- Reverts the CHECK constraint to the 3-value version and
-- drops the partial index. The agent_id backfill is NOT
-- reverted: setting agent_id back to NULL would lose
-- information (we don't know which rows had a NULL agent_id
-- vs which we backfilled). If you need a clean rollback,
-- snapshot before applying 091 and restore from snapshot.
--
-- Pre-conditions for safe down migration:
--   * No production memories.visibility = 'agent_portable' rows
--     remain. The constraint re-add will FAIL if any such row
--     exists. Run `UPDATE memories SET visibility = 'fund' WHERE
--     visibility = 'agent_portable'` first if you need to force
--     the rollback.

BEGIN;

DROP INDEX IF EXISTS idx_memories_agent_portable;

ALTER TABLE memories
    DROP CONSTRAINT IF EXISTS memories_visibility_check;

ALTER TABLE memories
    ADD CONSTRAINT memories_visibility_check
    CHECK (visibility IN ('private', 'fund', 'marketplace'));

COMMIT;
