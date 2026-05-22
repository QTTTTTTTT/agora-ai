-- Migration 018: forward-test track record + listing eligibility.
--
-- The marketplace today lets a fund be listed at any age, with no
-- minimum forward-test window and no objective performance numbers shown
-- to buyers. This migration adds:
--
--   1. funds.live_since: explicit "started forward-test on this date"
--      timestamp, distinct from created_at (row creation). Set the first
--      time the fund is flipped to live trading; immutable thereafter
--      (enforced in app layer).
--
--   2. agent_market_listings.min_forward_test_days_at_creation: snapshot
--      of the platform-wide minimum at the moment the listing was
--      created, so listings remain valid even if policy is later relaxed.
--
--   3. agent_market_listings.live_days_at_creation: how many forward-test
--      days the fund had when it was listed. Pre-computed at create time
--      to keep list views cheap.

ALTER TABLE funds
    ADD COLUMN IF NOT EXISTS live_since TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_funds_live_since
    ON funds (live_since)
    WHERE live_since IS NOT NULL;

ALTER TABLE agent_market_listings
    ADD COLUMN IF NOT EXISTS min_forward_test_days_at_creation INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN IF NOT EXISTS live_days_at_creation INTEGER NOT NULL DEFAULT 0;
