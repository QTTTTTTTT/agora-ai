-- Sprint 3 / L3: pgvector + memory embeddings + recall.
--
-- 目的：把 memories.content 经 LLM embedding 模型 (text-embedding-3-small,
-- 1536 维) 编码后落盘，PM 决策时按"问题相似度 top-k"召回最相关的过往 lesson
-- /reflection，扩 recentLessons 的"时间窗口召回"为"语义召回"。
--
-- 设计要点：
--   1) 我们用 pgvector 的 vector(1536) 类型。如果数据库没装 pgvector 扩展，
--      下面 `CREATE EXTENSION` 会失败 — operator 必须在 Postgres 上提前
--      `CREATE EXTENSION IF NOT EXISTS vector;`（Aliyun RDS / 自建 PG14+
--      都支持 1-line 安装）。
--   2) embedding 列默认 NULL — 新写入的 memory 由 daily review 后台同步
--      调 embed cron 填充。冷启数据后台逐行回填，零侵入。
--   3) 用 cosine distance 索引（IVFFlat lists=100，召回质量 / 速度的典型
--      参数）。当 memories 行数 > 100k 时考虑升到 HNSW。
--
-- 不在本迁移做：embed worker 本体（见 cmd/server/memory_embed.go）。

CREATE EXTENSION IF NOT EXISTS vector;

ALTER TABLE memories
    ADD COLUMN IF NOT EXISTS embedding vector(1536),
    ADD COLUMN IF NOT EXISTS embedding_model TEXT,
    ADD COLUMN IF NOT EXISTS embedded_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_memories_embedding_cosine
    ON memories USING ivfflat (embedding vector_cosine_ops)
    WITH (lists = 100);

-- 给 archive 表也加上同一列形态 — 这样归档不会丢失 embedding。
ALTER TABLE memories_archive
    ADD COLUMN IF NOT EXISTS embedding vector(1536),
    ADD COLUMN IF NOT EXISTS embedding_model TEXT,
    ADD COLUMN IF NOT EXISTS embedded_at TIMESTAMPTZ;
