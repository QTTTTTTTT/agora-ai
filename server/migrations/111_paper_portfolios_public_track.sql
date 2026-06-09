-- Migration: 111_paper_portfolios_public_track
-- Description:
--   Extend paper_portfolios with the three columns needed to publish
--   a SEC-compliant Publisher's-Exclusion track record:
--
--     public_track_record BOOLEAN  -- opt-in publication flag
--     methodology         TEXT     -- mandatory disclosure body
--     inception_date      DATE     -- "performance since" anchor
--
--   The new GET /api/papertrading/public/track-record family
--   (handler in cmd/server/paper_trading_public_handler.go) reads
--   only rows where public_track_record = TRUE and renders the
--   nav curve + derived metrics + the methodology field + a
--   fixed disclosure block.
--
--   Why opt-in (default FALSE):
--     Stage-4 paper portfolios may include experimental / internal
--     strategies that aren't intended for public consumption.
--     Defaulting OFF means a brand-new portfolio is invisible to
--     /public until an operator explicitly turns it on through the
--     admin surface (or via a direct UPDATE for migration cohorts).
--
--   Partial index — only public rows are listed externally, so we
--   only need an index over those. Saves space + keeps writes to
--   the (much larger) private cohort cheap.

ALTER TABLE paper_portfolios
    ADD COLUMN IF NOT EXISTS public_track_record BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS methodology TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS inception_date DATE;

CREATE INDEX IF NOT EXISTS idx_paper_portfolios_public_track
    ON paper_portfolios (public_track_record)
    WHERE public_track_record = TRUE;
