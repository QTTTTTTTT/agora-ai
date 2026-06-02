-- 074_alpha_aware_memory.sql — S9.1 alpha-tagged memory.
--
-- Extends the memories table so a long-term lesson can be
-- traceably tied to one agent's realised alpha. This is the
-- data layer that lets the PM (S9.4) prefix every analyst
-- report / debate argument with "[hit_rate=X%, avg_α=Y%]" and
-- the long_term memory loop can answer "what did this agent
-- get wrong on AAPL last month?".
--
-- Columns added:
--   * agent_tag        — TEXT, the agentreputation agent_id this
--                        memory refers to. Loose-coupled (no FK)
--                        because agent_ids live in the
--                        agentreputation tables and the analyst /
--                        advocate metadata, not in the agents
--                        table.
--   * alpha_vs_benchmark — DOUBLE PRECISION, the realised alpha
--                          (realised_return - benchmark_return)
--                          this memory summarises. Nullable —
--                          older memory rows don't have it.
--   * source_outcome_id — UUID nullable, pointer back into
--                         agent_reputation_outcomes so the
--                         operator can drill down.
--
-- Index: per-(fund, agent_tag) lookup so the PM context builder
-- can pull "the last K alpha-tagged lessons for fund_id F" in
-- one query.

ALTER TABLE memories
    ADD COLUMN IF NOT EXISTS agent_tag TEXT,
    ADD COLUMN IF NOT EXISTS alpha_vs_benchmark DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS source_outcome_id UUID;

ALTER TABLE memories_archive
    ADD COLUMN IF NOT EXISTS agent_tag TEXT,
    ADD COLUMN IF NOT EXISTS alpha_vs_benchmark DOUBLE PRECISION,
    ADD COLUMN IF NOT EXISTS source_outcome_id UUID;

CREATE INDEX IF NOT EXISTS idx_memories_fund_agent_tag
    ON memories (fund_id, agent_tag)
    WHERE agent_tag IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_memories_fund_alpha
    ON memories (fund_id, alpha_vs_benchmark)
    WHERE alpha_vs_benchmark IS NOT NULL;
