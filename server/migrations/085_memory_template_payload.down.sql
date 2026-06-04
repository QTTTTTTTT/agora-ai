-- Reverse of 085_memory_template_payload.sql. Drops the two columns
-- and the partial index. Run by an operator via psql when rolling back
-- the i18n pipeline; the forward boot-time migration runner never
-- applies *.down.sql.

DROP INDEX IF EXISTS idx_memories_template_key;
ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_template_key_shape;
ALTER TABLE memories DROP COLUMN IF EXISTS payload;
ALTER TABLE memories DROP COLUMN IF EXISTS template_key;
