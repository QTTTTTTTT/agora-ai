DROP INDEX IF EXISTS idx_memories_fund_alpha;
DROP INDEX IF EXISTS idx_memories_fund_agent_tag;

ALTER TABLE memories_archive
    DROP COLUMN IF EXISTS source_outcome_id,
    DROP COLUMN IF EXISTS alpha_vs_benchmark,
    DROP COLUMN IF EXISTS agent_tag;

ALTER TABLE memories
    DROP COLUMN IF EXISTS source_outcome_id,
    DROP COLUMN IF EXISTS alpha_vs_benchmark,
    DROP COLUMN IF EXISTS agent_tag;
