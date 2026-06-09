-- Down migration: 111_paper_portfolios_public_track
-- Drops the three opt-in publication columns + the partial index.
--
-- Safe to run even when the columns hold non-zero rows: DROP
-- COLUMN IF EXISTS is idempotent and PostgreSQL's column-drop
-- doesn't require any data backfill on the way down.

DROP INDEX IF EXISTS idx_paper_portfolios_public_track;

ALTER TABLE paper_portfolios
    DROP COLUMN IF EXISTS inception_date,
    DROP COLUMN IF EXISTS methodology,
    DROP COLUMN IF EXISTS public_track_record;
