-- 082_platform_llm_providers.down.sql — rollback for S13.
--
-- Drops the table and its touch trigger / function. The router
-- automatically falls back to env-only mode when the table is
-- absent (wiring_adapters.go::loadPlatformProviders), so the
-- downgrade path is non-destructive at runtime.

DROP TRIGGER IF EXISTS trg_platform_llm_providers_updated_at ON platform_llm_providers;
DROP FUNCTION IF EXISTS touch_platform_llm_providers_updated_at();
DROP INDEX IF EXISTS idx_platform_llm_providers_tier_active;
DROP INDEX IF EXISTS idx_platform_llm_providers_status_provider;
DROP INDEX IF EXISTS uq_platform_llm_providers_single_default;
DROP INDEX IF EXISTS uq_platform_llm_providers_provider_label;
DROP TABLE IF EXISTS platform_llm_providers;
