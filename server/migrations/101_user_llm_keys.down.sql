DELETE FROM feature_flags WHERE flag_key = 'advisor_byok';
DROP TRIGGER IF EXISTS trg_user_llm_keys_updated_at ON user_llm_keys;
DROP FUNCTION IF EXISTS touch_user_llm_keys_updated_at();
DROP INDEX IF EXISTS idx_user_llm_keys_fingerprint;
DROP INDEX IF EXISTS idx_user_llm_keys_user_active;
DROP INDEX IF EXISTS uniq_user_llm_keys_active_provider;
DROP TABLE IF EXISTS user_llm_keys;
