-- 084_fund_llm_overrides.down.sql — rollback S14.B.
DROP TRIGGER IF EXISTS trg_fund_llm_overrides_updated_at ON fund_llm_overrides;
DROP FUNCTION IF EXISTS touch_fund_llm_overrides_updated_at();
DROP INDEX IF EXISTS idx_fund_llm_overrides_provider;
DROP INDEX IF EXISTS idx_fund_llm_overrides_fund_enabled;
DROP INDEX IF EXISTS uniq_fund_llm_overrides_scope;
DROP TABLE IF EXISTS fund_llm_overrides;
