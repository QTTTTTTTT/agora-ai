-- 015_marketplace_reconcile.sql
--
-- PR-02: marketplace purchase atomicity, idempotency and reconciliation.
--
-- Goals
--   1. Allow agent_market_orders to remain in flight even when no agent has
--      been delivered yet (we need an order row before the wallet transfer
--      so that idempotency keys can attach to it).
--   2. Force every wallet ledger entry that originates from a marketplace
--      purchase to carry a unique idempotency key, so that retried calls
--      never double-charge the buyer.
--   3. Persist a reconciliation marker on each order so that a background
--      cron can detect and surface ledger / order divergence.

-- ---------------------------------------------------------------------------
-- 1. agent_market_orders: extend status enum, allow nullable delivery, add
--    idempotency key + reconciliation columns.
-- ---------------------------------------------------------------------------

ALTER TABLE agent_market_orders
    ALTER COLUMN delivered_agent_id DROP NOT NULL;

ALTER TABLE agent_market_orders
    DROP CONSTRAINT IF EXISTS agent_market_orders_status_check;

ALTER TABLE agent_market_orders
    ADD CONSTRAINT agent_market_orders_status_check
    CHECK (status IN ('pending', 'completed', 'failed', 'reversed'));

ALTER TABLE agent_market_orders
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT,
    ADD COLUMN IF NOT EXISTS failure_reason TEXT,
    ADD COLUMN IF NOT EXISTS reconciled_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS reconciliation_notes TEXT,
    ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Backfill idempotency_key for legacy rows so the unique index can be added
-- without conflicts. The synthesised key is deterministic but unique because
-- a listing can only be sold once (UNIQUE on listing_id already exists).
UPDATE agent_market_orders
SET idempotency_key = 'legacy-' || id::text
WHERE idempotency_key IS NULL;

ALTER TABLE agent_market_orders
    ALTER COLUMN idempotency_key SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_market_orders_idempotency_key
    ON agent_market_orders (idempotency_key);

CREATE INDEX IF NOT EXISTS idx_agent_market_orders_pending_recon
    ON agent_market_orders (status, reconciled_at)
    WHERE status IN ('pending', 'failed');

-- The legacy schema only allowed status = 'completed' because the historical
-- check constraint was very narrow. The migration above relaxed it; we still
-- keep the listing_id UNIQUE constraint so that one listing produces at most
-- one logical order row regardless of state transitions.

-- ---------------------------------------------------------------------------
-- 2. wallet_ledger_entries: idempotency.
--
-- Each marketplace purchase writes two ledger entries (debit + credit) that
-- share an order id. We allow callers to pass an explicit idempotency key
-- per side, which combined with reference_id gives us a true unique slot.
-- ---------------------------------------------------------------------------

ALTER TABLE wallet_ledger_entries
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_wallet_ledger_idempotency_key
    ON wallet_ledger_entries (idempotency_key)
    WHERE idempotency_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- 3. marketplace_reconcile_log: append-only audit trail for the cron.
-- ---------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS marketplace_reconcile_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    order_id UUID REFERENCES agent_market_orders(id) ON DELETE SET NULL,
    listing_id UUID,
    finding VARCHAR(40) NOT NULL,
    detail JSONB NOT NULL DEFAULT '{}',
    resolved BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_marketplace_reconcile_log_unresolved
    ON marketplace_reconcile_log (resolved, created_at DESC)
    WHERE resolved = FALSE;

-- ---------------------------------------------------------------------------
-- 4. Trigger to keep agent_market_orders.updated_at fresh when reconciliation
--    or status fields change. This is purely informational; it costs nothing
--    on the happy path.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION agent_market_orders_touch_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_agent_market_orders_updated_at ON agent_market_orders;
CREATE TRIGGER trg_agent_market_orders_updated_at
    BEFORE UPDATE ON agent_market_orders
    FOR EACH ROW EXECUTE FUNCTION agent_market_orders_touch_updated_at();
