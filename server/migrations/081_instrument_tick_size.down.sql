-- 081_instrument_tick_size.down.sql — drop the tick_size /
-- tick_rules columns added in 081.

ALTER TABLE instrument_metadata
    DROP COLUMN IF EXISTS tick_rules,
    DROP COLUMN IF EXISTS tick_size;
