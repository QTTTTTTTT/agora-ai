-- Migration 055: broker_links table (P0-9 / P1-6).
--
-- Why this table
--
-- The platform separates "simulation funds" from "live funds" via
-- funds.trading_mode. Until now the live mode existed only as a
-- column value — there was no place to record WHICH external
-- broker account a live fund routes to, who authorised the link,
-- or whether the link is currently active. P0-9's live-trading
-- hard gate refuses to dispatch a trade into 'live' mode unless
-- a row in this table exists, so we need the schema before the
-- gate.
--
-- Scope of this migration
--
-- Just the data model + a few invariant CHECKs. The 4-eye
-- approval workflow that mutates these rows is P1-6's
-- responsibility; today we expose the table and let an admin
-- seed it manually (or via a future broker-link CRUD endpoint).
-- The credentials_encrypted column is a placeholder for when
-- API key storage actually gets wired — left BYTEA NULL today
-- so we don't ship an empty-string contract.
--
-- Why per-fund (not per-user)
--
-- A user MAY have multiple funds (one per strategy bucket); each
-- fund is the routing target. Linking at the fund level keeps the
-- gate's logic local — no need to walk a (user, fund, broker)
-- triplet. A single user CAN end up with multiple rows here, one
-- per fund × broker combination.

CREATE TABLE IF NOT EXISTS broker_links (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    -- Fund this link routes for. Cascades on fund delete because
    -- a deleted fund cannot have a meaningful broker route.
    fund_id               UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    -- The user who owns / authorised this link. We resolve it
    -- separately from fund.created_by so a fund can be owned by
    -- one user but routed via another user's broker login (rare
    -- but supported for institutional setups).
    user_id               UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    -- Broker identifier. Free text today; a future migration can
    -- pin this to an enum once we know the supported set.
    --   "ibkr"     — Interactive Brokers
    --   "futu"     — Futu
    --   "alpaca"   — Alpaca
    --   "binance"  — Binance (crypto)
    -- "mock" is reserved for development / smoke tests.
    broker_id             VARCHAR(64) NOT NULL,
    -- Broker-side account number / sub-account ID. Stored
    -- VARCHAR not BYTEA because account IDs are NOT secrets —
    -- they're shown on every broker statement.
    account_id            VARCHAR(128) NOT NULL,
    -- Status lifecycle:
    --   pending      — created, awaiting 4-eye approval (P1-6)
    --   active       — approved, gate accepts trades on this fund
    --   suspended    — temporarily disabled (incident / off-hours)
    --   revoked      — terminal; a fresh link must be created to
    --                  resume live trading
    status                VARCHAR(20) NOT NULL DEFAULT 'pending'
                              CHECK (status IN ('pending', 'active', 'suspended', 'revoked')),
    -- 4-eye approval. Populated once P1-6 lands; nullable for
    -- now to keep the migration backward-compatible if an admin
    -- wants to seed an active row without going through the
    -- approval workflow (dev path only — production should
    -- always require approval).
    approved_by           UUID REFERENCES users(id) ON DELETE SET NULL,
    approved_at           TIMESTAMPTZ,
    -- Encrypted credentials placeholder. Today we don't store API
    -- keys; once a real broker integration ships the keys go in
    -- encrypted under TOTP_ENCRYPTION_KEY (or a sibling key) using
    -- the same AES-256-GCM scheme as user_totp_secrets.
    credentials_encrypted BYTEA,
    -- Free-form metadata for broker-specific routing hints
    -- (e.g. account class, market data subscriptions). JSONB so
    -- the schema can evolve without a migration per broker.
    metadata              JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- A fund can have at most one ACTIVE link at a time — we
    -- enforce this with a partial unique index further down so
    -- multiple "revoked" rows can coexist as historical records.
    CONSTRAINT broker_links_approval_consistency
        CHECK (
            (approved_by IS NULL AND approved_at IS NULL)
            OR (approved_by IS NOT NULL AND approved_at IS NOT NULL)
        )
);

-- Lookup index for the gate's hot path: "is there an ACTIVE
-- broker link for this fund?". We don't include status in the key
-- so all-status lookups for the management UI also benefit.
CREATE INDEX IF NOT EXISTS broker_links_fund_id_idx
    ON broker_links (fund_id);

-- One ACTIVE link per fund — see the comment on the status column.
-- Partial UNIQUE means a fund can churn through multiple
-- pending/revoked rows without colliding, but only one ACTIVE
-- can exist at any moment.
CREATE UNIQUE INDEX IF NOT EXISTS broker_links_one_active_per_fund_idx
    ON broker_links (fund_id)
    WHERE status = 'active';

-- Index for "show me all links a user authored" — drives a future
-- account-security page section.
CREATE INDEX IF NOT EXISTS broker_links_user_id_idx
    ON broker_links (user_id);

-- updated_at trigger — same shape as user_totp_secrets'.
CREATE OR REPLACE FUNCTION broker_links_touch_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS broker_links_touch_updated_at ON broker_links;
CREATE TRIGGER broker_links_touch_updated_at
    BEFORE UPDATE ON broker_links
    FOR EACH ROW
    EXECUTE FUNCTION broker_links_touch_updated_at();
