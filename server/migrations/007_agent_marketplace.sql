CREATE TABLE agent_market_listings (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    seller_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    source_fund_id UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    source_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    agent_name VARCHAR(255) NOT NULL,
    agent_role VARCHAR(20) NOT NULL CHECK (agent_role IN ('pm', 'researcher', 'trader', 'risk')),
    agent_focus VARCHAR(30),
    latest_learning_summary TEXT,
    ask_price_minor BIGINT NOT NULL CHECK (ask_price_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    status VARCHAR(20) NOT NULL DEFAULT 'active' CHECK (status IN ('active', 'sold', 'cancelled')),
    snapshot_payload JSONB NOT NULL DEFAULT '{}',
    sold_to_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    sold_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_market_listings_status_created_at ON agent_market_listings (status, created_at DESC);
CREATE INDEX idx_agent_market_listings_seller_user_id ON agent_market_listings (seller_user_id, created_at DESC);
CREATE UNIQUE INDEX idx_agent_market_listings_active_source ON agent_market_listings (source_fund_id, source_agent_id) WHERE status = 'active';

CREATE TABLE agent_market_bids (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    listing_id UUID NOT NULL REFERENCES agent_market_listings(id) ON DELETE CASCADE,
    bidder_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    bid_price_minor BIGINT NOT NULL CHECK (bid_price_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    status VARCHAR(20) NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'accepted', 'rejected', 'retracted')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_market_bids_listing_id ON agent_market_bids (listing_id, created_at DESC);
CREATE INDEX idx_agent_market_bids_bidder_user_id ON agent_market_bids (bidder_user_id, created_at DESC);

CREATE TABLE agent_market_orders (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    listing_id UUID NOT NULL UNIQUE REFERENCES agent_market_listings(id) ON DELETE CASCADE,
    seller_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    buyer_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    buyer_fund_id UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    source_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE RESTRICT,
    delivered_agent_id UUID NOT NULL REFERENCES agents(id) ON DELETE RESTRICT,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    status VARCHAR(20) NOT NULL DEFAULT 'completed' CHECK (status IN ('completed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_agent_market_orders_buyer_user_id ON agent_market_orders (buyer_user_id, created_at DESC);
CREATE INDEX idx_agent_market_orders_seller_user_id ON agent_market_orders (seller_user_id, created_at DESC);
