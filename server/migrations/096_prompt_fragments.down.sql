-- 096_prompt_fragments.down.sql

DROP INDEX IF EXISTS uq_prompt_fragments_active_per_slot;
DROP INDEX IF EXISTS idx_prompt_fragments_slot_status;

DROP TABLE IF EXISTS prompt_fragment_uses;
DROP TABLE IF EXISTS prompt_fragments;
