-- Migration 025: A/B learning promotion — track what was promoted so it
-- can be rolled back atomically.
--
-- Migration 022 introduced ab_test_learning_promotions to record
-- evolution_config changes. F6 layers two more promotion targets on top of
-- that: long-term reflections (memories) and approved skills
-- (parsedSkillEntry rows). Each rollback now needs to know which exact
-- rows were copied into the control fund so it can undo them.

ALTER TABLE ab_test_learning_promotions
    -- Newly-cloned memory ids (in the control fund). Rollback deletes
    -- exactly these ids — the control fund's pre-existing memories are
    -- preserved. JSONB so the array shape stays flexible (per-agent
    -- promotions may produce 0..N memory rows).
    ADD COLUMN IF NOT EXISTS promoted_memory_ids JSONB NOT NULL DEFAULT '[]',
    -- Skill keys that were inserted (or flipped from missing to proposed)
    -- in the control agent's skill_config. Rollback removes only these
    -- keys; manually-added or pre-existing skills with the same key are
    -- untouched (we snapshot previous_skill_config to verify).
    ADD COLUMN IF NOT EXISTS promoted_skill_keys JSONB NOT NULL DEFAULT '[]',
    -- Full snapshot of the control agent's skill_config BEFORE the
    -- promotion. Rollback restores from this snapshot if any skill
    -- entry was overwritten (mode='overwrite'). For mode='merge' we
    -- only delete the promoted keys, but keep the snapshot for audit.
    ADD COLUMN IF NOT EXISTS previous_skill_config JSONB NOT NULL DEFAULT '{}';

-- Index on test_id+agent_id already exists; add a partial index on
-- promotions that include memories/skills so list views can highlight
-- "full promotions" vs evolution-only ones cheaply.
CREATE INDEX IF NOT EXISTS idx_ab_test_learning_promotions_full
    ON ab_test_learning_promotions (test_id, promoted_at DESC)
    WHERE jsonb_array_length(promoted_memory_ids) > 0
       OR jsonb_array_length(promoted_skill_keys) > 0;
