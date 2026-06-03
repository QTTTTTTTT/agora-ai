-- 080_instrument_metadata.down.sql — roll back the instrument_metadata
-- table introduced in 080. Drops the trigger and function first to
-- keep PostgreSQL's dependency tracker happy.

DROP TRIGGER IF EXISTS trg_instrument_metadata_updated_at ON instrument_metadata;
DROP FUNCTION IF EXISTS touch_instrument_metadata_updated_at();
DROP INDEX IF EXISTS idx_instrument_metadata_supports_fractional;
DROP INDEX IF EXISTS idx_instrument_metadata_market_asset;
DROP TABLE IF EXISTS instrument_metadata;
