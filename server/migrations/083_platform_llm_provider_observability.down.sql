-- 083_platform_llm_provider_observability.down.sql — rollback S14.A.
DROP INDEX IF EXISTS idx_provider_daily_rollups_provider_day;
DROP INDEX IF EXISTS idx_provider_daily_rollups_day;
DROP TABLE IF EXISTS platform_llm_provider_daily_rollups;
DROP INDEX IF EXISTS idx_provider_health_history_checked_at;
DROP INDEX IF EXISTS idx_provider_health_history_provider_time;
DROP TABLE IF EXISTS platform_llm_provider_health_history;
