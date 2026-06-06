-- 095_memory_refutation.down.sql

DROP INDEX IF EXISTS idx_memories_refuted_status;

ALTER TABLE memories_archive
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS last_refuted_at,
    DROP COLUMN IF EXISTS refutation_count;

ALTER TABLE memories
    DROP COLUMN IF EXISTS status,
    DROP COLUMN IF EXISTS last_refuted_at,
    DROP COLUMN IF EXISTS refutation_count;
