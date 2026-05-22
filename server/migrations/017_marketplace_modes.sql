-- Migration 017: marketplace dual-mode (buyout/subscribe) + black-box inference audit.
--
-- Additive only — preserves existing rows. Default `mode='buyout'` reproduces
-- legacy behaviour: a listing is purchased once, the agent is cloned into the
-- buyer's account, listing is marked sold.
--
-- The new `subscribe` mode does not transfer the agent. Instead, a buyer pays
-- a recurring price and may invoke the seller's agent through the inference
-- gateway (server-side execution; raw prompts/policies never leave the
-- seller's account).

-- 1. Extend listings with mode + subscription pricing.
ALTER TABLE agent_market_listings
    ADD COLUMN IF NOT EXISTS mode VARCHAR(16) NOT NULL DEFAULT 'buyout'
        CHECK (mode IN ('buyout', 'subscribe')),
    ADD COLUMN IF NOT EXISTS subscription_price_minor BIGINT
        CHECK (subscription_price_minor IS NULL OR subscription_price_minor > 0),
    ADD COLUMN IF NOT EXISTS subscription_period VARCHAR(16)
        CHECK (subscription_period IS NULL OR subscription_period IN ('daily', 'weekly', 'monthly'));

-- Mode/price coherence: buyout requires ask_price_minor (pre-existing NOT NULL);
-- subscribe requires both subscription_price_minor and subscription_period.
DO $$
BEGIN
    IF NOT EXISTS (
        SELECT 1 FROM pg_constraint
        WHERE conname = 'chk_listings_subscribe_pricing'
    ) THEN
        ALTER TABLE agent_market_listings
            ADD CONSTRAINT chk_listings_subscribe_pricing
            CHECK (
                mode = 'buyout'
                OR (subscription_price_minor IS NOT NULL AND subscription_period IS NOT NULL)
            );
    END IF;
END$$;

-- The active-source uniqueness now needs to be scoped per-mode; a fund/agent
-- pair can have at most one active listing of each mode at a time.
DROP INDEX IF EXISTS idx_agent_market_listings_active_source;
CREATE UNIQUE INDEX idx_agent_market_listings_active_source_mode
    ON agent_market_listings (source_fund_id, source_agent_id, mode)
    WHERE status = 'active';

-- 2. Subscriptions: a per-buyer recurring relationship to a listing.
CREATE TABLE IF NOT EXISTS agent_market_subscriptions (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    listing_id UUID NOT NULL REFERENCES agent_market_listings(id) ON DELETE CASCADE,
    seller_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    subscriber_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    price_minor BIGINT NOT NULL CHECK (price_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    period VARCHAR(16) NOT NULL CHECK (period IN ('daily', 'weekly', 'monthly')),
    status VARCHAR(16) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'cancelled', 'expired')),
    current_period_start TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    current_period_end TIMESTAMPTZ NOT NULL,
    cancelled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_subs_subscriber
    ON agent_market_subscriptions (subscriber_user_id, status, current_period_end);
CREATE INDEX IF NOT EXISTS idx_subs_listing
    ON agent_market_subscriptions (listing_id, status);
-- A subscriber may only hold one active subscription per listing at a time.
CREATE UNIQUE INDEX IF NOT EXISTS idx_subs_active_unique
    ON agent_market_subscriptions (listing_id, subscriber_user_id)
    WHERE status = 'active';

-- 3. Inference requests audit log — every invocation of a subscribed agent
--    is recorded so the platform can bill, rate-limit, and (most importantly)
--    prove to sellers that their IP has not been exfiltrated.
CREATE TABLE IF NOT EXISTS agent_inference_requests (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    subscription_id UUID NOT NULL REFERENCES agent_market_subscriptions(id) ON DELETE CASCADE,
    listing_id UUID NOT NULL REFERENCES agent_market_listings(id) ON DELETE CASCADE,
    subscriber_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    request_payload JSONB NOT NULL DEFAULT '{}',
    response_payload JSONB NOT NULL DEFAULT '{}',
    latency_ms INTEGER NOT NULL DEFAULT 0,
    status VARCHAR(16) NOT NULL CHECK (status IN ('ok', 'error', 'rate_limited')),
    error_message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_inference_subscriber_created
    ON agent_inference_requests (subscriber_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_inference_listing_created
    ON agent_inference_requests (listing_id, created_at DESC);
