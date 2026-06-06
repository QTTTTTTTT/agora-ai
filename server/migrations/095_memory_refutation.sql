-- 095_memory_refutation.sql — W2-9: track lesson refutation.
--
-- WHY THIS EXISTS
-- ---------------
-- Migration 074 made memories alpha-aware (agent_tag, alpha,
-- source_outcome_id). Migration 093 (this branch) records which
-- lessons were USED to shape a plan. Migration 094 records the
-- realised outcome. The W2-9 lesson-refute path closes the
-- learning loop on the negative side: when a plan that leaned on
-- lesson L produced a bad outcome, lesson L gets a "refutation"
-- against it.
--
-- A high enough refutation_count means the lesson is actively
-- *misleading* the LLM. Today every lesson with hit_rate ≥ X
-- and N ≥ 3 stays in the alphalesson context block forever. Once
-- a lesson goes stale (regime changed, the company restructured,
-- the underlying thesis no longer applies), there is no signal
-- to remove it. This column gives the alphalesson context
-- builder a downgrade signal: skip lessons whose refutation_count
-- × refute_weight exceeds their alpha × use_count.
--
-- SHAPE
-- -----
-- Three new columns on memories (and the archive twin):
--
--   * refutation_count   INT DEFAULT 0
--       — monotonically non-decreasing. Incremented by the
--         lesson-refute pipeline when a plan that USED this
--         memory produced a bad outcome.
--   * last_refuted_at    TIMESTAMPTZ
--       — most recent refutation timestamp. Lets the context
--         builder weight recent refutations more heavily than
--         ancient ones.
--   * status             TEXT NOT NULL DEFAULT 'active'
--       — small finite enum: 'active' | 'soft_refuted' |
--         'hard_refuted' | 'archived'. The auto-mark policy
--         flips active → soft_refuted at refutation_count ≥ N
--         (default 3) and soft_refuted → hard_refuted at N (5).
--         The lessonrefute package owns the canonical thresholds.
--
-- We do NOT auto-delete or mutate the memory body — refute is a
-- soft signal. An admin can override via the SkillInbox UI.

ALTER TABLE memories
    ADD COLUMN IF NOT EXISTS refutation_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_refuted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';

ALTER TABLE memories_archive
    ADD COLUMN IF NOT EXISTS refutation_count INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS last_refuted_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';

-- Partial index for the alphalesson context builder: pull only
-- "still credible" memories. Cheap because most rows stay
-- 'active'; the index only carries the soft/hard refuted ones.
CREATE INDEX IF NOT EXISTS idx_memories_refuted_status
    ON memories (status, last_refuted_at DESC)
    WHERE status <> 'active';

COMMENT ON COLUMN memories.refutation_count IS
    'W2-9: monotonic count of refutations. Incremented when a plan that used this memory produced a bad outcome (negative alpha, missed thesis). Owned by internal/lessonrefute.';
COMMENT ON COLUMN memories.last_refuted_at IS
    'W2-9: timestamp of the most recent refutation (NULL until first refute).';
COMMENT ON COLUMN memories.status IS
    'W2-9: lesson status. One of active | soft_refuted | hard_refuted | archived. The alphalesson context builder skips hard_refuted by default and down-weights soft_refuted.';
