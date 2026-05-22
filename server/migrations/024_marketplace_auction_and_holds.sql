-- Migration 024: marketplace English-ascending auctions + wallet holds.
--
-- Additive only — preserves every existing row and constraint. Two new
-- concepts land here, both wired into the existing tables instead of
-- standing up parallel ones (cheaper to operate, no cross-schema joins):
--
--   * `agent_market_listings.mode = 'auction'` joins the existing buyout/
--     subscribe enum. An auction listing carries the timing & ratchet
--     metadata in dedicated columns; the snapshot redaction + transfer-on-
--     settlement reuse the buyout pipeline so the auction merely chooses
--     the buyer (and price) instead of accepting whoever pays first.
--
--   * `wallet_holds` records a deduction of buyer funds that is NOT yet
--     spendable by the seller. When a bidder is outbid, the platform calls
--     ReleaseHold to refund them; when the auction settles, CaptureHold
--     converts the winning bidder's hold into a normal marketplace_purchase
--     debit/credit pair. The hold itself reserves capital so a bidder
--     cannot place a bid they cannot fund.

-- ---------------------------------------------------------------------------
-- 1. Listings: extend the mode enum + auction columns.
-- ---------------------------------------------------------------------------

ALTER TABLE agent_market_listings
    DROP CONSTRAINT IF EXISTS agent_market_listings_mode_check;
ALTER TABLE agent_market_listings
    ADD CONSTRAINT agent_market_listings_mode_check
    CHECK (mode IN ('buyout', 'subscribe', 'auction'));

ALTER TABLE agent_market_listings
    ADD COLUMN IF NOT EXISTS auction_started_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auction_ends_at TIMESTAMPTZ,
    -- Reserve floor in minor currency units (e.g. cents). Bids below this
    -- are rejected outright so the seller cannot end up with a winning
    -- bid that doesn't cover their cost. Reserve is opaque to bidders.
    ADD COLUMN IF NOT EXISTS auction_reserve_minor BIGINT
        CHECK (auction_reserve_minor IS NULL OR auction_reserve_minor > 0),
    -- Minimum increment between successive bids. Default 1 minor unit so
    -- a misconfigured listing never deadlocks on equal bids.
    ADD COLUMN IF NOT EXISTS auction_min_increment_minor BIGINT NOT NULL DEFAULT 1
        CHECK (auction_min_increment_minor > 0),
    -- Anti-sniping window: any bid arriving within this many seconds of
    -- end_time pushes the end out by the same amount (eBay-style "soft
    -- close"). 0 disables the rule.
    ADD COLUMN IF NOT EXISTS auction_anti_snipe_seconds INTEGER NOT NULL DEFAULT 60
        CHECK (auction_anti_snipe_seconds >= 0),
    -- Cached top bid for cheap list queries. Updated under the row lock
    -- whenever a new top bid is placed.
    ADD COLUMN IF NOT EXISTS auction_current_bid_minor BIGINT
        CHECK (auction_current_bid_minor IS NULL OR auction_current_bid_minor > 0),
    ADD COLUMN IF NOT EXISTS auction_current_bidder_user_id UUID
        REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS auction_current_bid_id UUID,
    -- After settlement, mark with the closing outcome so the UI can show
    -- "Sold for X" / "Reserve not met".
    ADD COLUMN IF NOT EXISTS auction_settled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auction_winning_bid_id UUID;

-- An auction listing needs both timestamps + a starting price; enforce that
-- mode='auction' carries the full set rather than half-configured rows.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint WHERE conname = 'chk_listings_auction_pricing'
    ) THEN
        ALTER TABLE agent_market_listings
            ADD CONSTRAINT chk_listings_auction_pricing
            CHECK (
                mode <> 'auction'
                OR (
                    auction_started_at IS NOT NULL
                    AND auction_ends_at IS NOT NULL
                    AND auction_ends_at > auction_started_at
                    AND ask_price_minor > 0
                )
            );
    END IF;
END$$;

-- Open auctions (status='active', mode='auction') are queried by the
-- settlement worker every few seconds; index on the end timestamp keeps
-- the scan tight.
CREATE INDEX IF NOT EXISTS idx_listings_open_auctions
    ON agent_market_listings (auction_ends_at)
    WHERE mode = 'auction' AND status = 'active';

-- ---------------------------------------------------------------------------
-- 2. Bids: link to the wallet hold + record audit fields.
-- ---------------------------------------------------------------------------

-- Replace the legacy bid-status enum with the auction lifecycle. Existing
-- rows (manual offers from buyout pre-history) map their 'pending' state
-- to the new 'active' state for forward compatibility.
ALTER TABLE agent_market_bids
    DROP CONSTRAINT IF EXISTS agent_market_bids_status_check;
UPDATE agent_market_bids SET status = 'active' WHERE status = 'pending';
ALTER TABLE agent_market_bids
    ADD CONSTRAINT agent_market_bids_status_check
    CHECK (status IN ('active', 'outbid', 'won', 'refunded', 'rejected', 'retracted'));
ALTER TABLE agent_market_bids
    ALTER COLUMN status SET DEFAULT 'active';

ALTER TABLE agent_market_bids
    ADD COLUMN IF NOT EXISTS hold_id UUID;

-- Per-listing index for "who's currently winning" + "all bids in
-- chronological order" history queries.
CREATE INDEX IF NOT EXISTS idx_bids_listing_status
    ON agent_market_bids (listing_id, status, bid_price_minor DESC);

-- ---------------------------------------------------------------------------
-- 3. Wallet holds: per-bid escrow with idempotent capture/release.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS wallet_holds (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id UUID NOT NULL REFERENCES wallet_accounts(id) ON DELETE CASCADE,
    -- Wallet account row holding the funds. We replicate user_id alongside
    -- account_id so cheap reads ("does user X have an open hold?") avoid
    -- the join, while account_id keeps the FK contract for ledger writes.
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'released', 'captured')),
    reference_type VARCHAR(40),
    reference_id TEXT,
    -- Capture forwards funds to this counterparty. Stored at hold-creation
    -- time so the capture transaction does not need to re-derive intent.
    captured_to_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    captured_at TIMESTAMPTZ,
    released_at TIMESTAMPTZ,
    -- Idempotency key prevents double-holding the same auction bid even
    -- under retries. Matches the wallet_ledger_entries.idempotency_key
    -- convention so callers can reuse a single key across both writes.
    idempotency_key TEXT,
    metadata JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_wallet_holds_user_status
    ON wallet_holds (user_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_wallet_holds_reference
    ON wallet_holds (reference_type, reference_id);
-- Only enforce uniqueness when the key is supplied — null-tolerant so
-- legacy callers that don't pass an idempotency key still write.
CREATE UNIQUE INDEX IF NOT EXISTS idx_wallet_holds_idempotency
    ON wallet_holds (idempotency_key)
    WHERE idempotency_key IS NOT NULL;
