DROP INDEX IF EXISTS idx_memories_embedding_cosine;

ALTER TABLE memories
    DROP COLUMN IF EXISTS embedding,
    DROP COLUMN IF EXISTS embedding_model,
    DROP COLUMN IF EXISTS embedded_at;

ALTER TABLE memories_archive
    DROP COLUMN IF EXISTS embedding,
    DROP COLUMN IF EXISTS embedding_model,
    DROP COLUMN IF EXISTS embedded_at;

-- 不 DROP EXTENSION vector — 它可能被其他 schema 引用。
