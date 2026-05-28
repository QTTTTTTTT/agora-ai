-- 040_plan_block_contributions.sql
--
-- G1 #2: per-plan decision-block attribution skeleton.
--
-- For every plan the PM writes to investment_plans we want a
-- structured record of:
--
--   1) which signal blocks were PRESENT in the DecisionInput
--      (i.e. the prompt actually carried that block)
--   2) which signal blocks the PM REFERENCED by name in its
--      Reasoning text
--   3) extra audit fields (block count, fingerprint signature)
--
-- These three together give us the "what did the PM have access
-- to" vs "what did the PM actually use" split. Down the line we
-- correlate the cited blocks against realised plan PnL (via the
-- lot ledger) so we can produce a per-block Sharpe / hit-rate
-- decomposition without instrumenting the LLM itself.
--
-- The column is JSONB so the writer can evolve the schema (add
-- per-cluster blocks, per-symbol attribution, etc.) without
-- another migration. The default '{}' keeps the column
-- backward-compatible with rows written before the writer is
-- deployed (those rows simply have no attribution record).
--
-- Index choice:
--   - GIN on the JSONB column so we can run queries like
--     `WHERE block_contributions @> '{"cited":["valueScores"]}'`
--     in O(log n) — used by the per-window block-citation
--     reports the dashboard surfaces.
--   - btree on (fund_id, created_at DESC) is already present
--     from migration 011; reusing it for the per-fund
--     attribution query joins.

ALTER TABLE investment_plans
    ADD COLUMN IF NOT EXISTS block_contributions JSONB NOT NULL DEFAULT '{}';

CREATE INDEX IF NOT EXISTS idx_investment_plans_block_contributions_gin
    ON investment_plans USING GIN (block_contributions);

COMMENT ON COLUMN investment_plans.block_contributions IS
    'G1 #2: per-plan decision-block attribution snapshot. JSON shape: {present:[block,..], absent:[block,..], cited:[block,..], counts:{block:int}, signature:string}. Empty default keeps legacy rows backward-compatible.';
