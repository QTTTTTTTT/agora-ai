-- Migration: 102_advisor_credits
-- Description:
--   Phase C-1 — pre-paid service unit balance for /advisor mode.
--
--   The plan rolled out in this commit prices /advisor consults as:
--
--     1. plan-included monthly units (Free 5 deep + 15 quick, Pro
--        100 deep + ∞ quick, Premium 500 deep + ∞ quick, ...)
--        — see subscription.Plan.AdvisorDeepUnitsPerMonth and
--        AdvisorQuickUnitsPerMonth, Phase A.
--     2. user-purchased credit packs (this migration). A pack
--        is a one-off LemonSqueezy SKU that grants N units to
--        the user's balance. Units never expire; users burn them
--        only AFTER plan-included units are exhausted.
--     3. (future: per-consult on-demand $ charge when the user
--        is out of plan-units AND credits. Not in this migration.)
--
--   Two tables:
--     user_advisor_credits   one row per user; running deep +
--                            quick balance. Updated transactionally
--                            from advisor_credit_orders (purchases)
--                            and advisorbilling.Gate.Consume (spends).
--     advisor_credit_orders  one row per LemonSqueezy purchase, used
--                            as the idempotency ledger for the webhook
--                            and as the read-side history.
--
--   We split balance + orders so the hot read path (Gate.Check)
--   touches a 1-row table and the audit/admin read path scans an
--   append-only table. The balance row is initialised lazily on
--   first webhook so users without purchases never get a row.
--
--   ALTER advisor_consultations adds two columns the panel writer
--   stamps so we can answer "how many of this month's consults
--   were paid by credits vs plan?".

CREATE TABLE IF NOT EXISTS user_advisor_credits (
    user_id                UUID         PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    deep_units_balance     INTEGER      NOT NULL DEFAULT 0 CHECK (deep_units_balance >= 0),
    quick_units_balance    INTEGER      NOT NULL DEFAULT 0 CHECK (quick_units_balance >= 0),
    total_purchased_cents  BIGINT       NOT NULL DEFAULT 0 CHECK (total_purchased_cents >= 0),
    last_purchase_at       TIMESTAMPTZ,
    last_consumption_at    TIMESTAMPTZ,
    created_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at             TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_advisor_credits_has_balance
    ON user_advisor_credits (user_id)
    WHERE deep_units_balance > 0 OR quick_units_balance > 0;

CREATE OR REPLACE FUNCTION touch_user_advisor_credits_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_user_advisor_credits_updated_at ON user_advisor_credits;
CREATE TRIGGER trg_user_advisor_credits_updated_at
    BEFORE UPDATE ON user_advisor_credits
    FOR EACH ROW EXECUTE FUNCTION touch_user_advisor_credits_updated_at();

COMMENT ON TABLE user_advisor_credits IS
    'Phase C-1 advisor credit-pack balance. One row per user. Units never expire.';
COMMENT ON COLUMN user_advisor_credits.total_purchased_cents IS
    'Lifetime USD cents the user has spent on credit packs. Read by admin reports + the upgrade-prompt heuristic.';

-- ---------------------------------------------------------------------------
-- advisor_credit_orders — LemonSqueezy purchase ledger
-- ---------------------------------------------------------------------------
--
-- Each row tracks one credit-pack purchase. lemonsqueezy_order_id
-- is the merchant-side idempotency key: when the same webhook
-- arrives twice we INSERT … ON CONFLICT (lemonsqueezy_order_id)
-- DO NOTHING so the balance is debited once, never twice.
--
-- status flow:
--    'pending'   created at /checkout request (before webhook arrives)
--    'paid'      webhook received with valid signature
--    'refunded'  refund webhook received later
--    'failed'    cancellation / dispute
--
-- pack_sku is the internal SKU (advisor_credits_small / _medium /
-- _large) defined in cmd/server/advisor_credit_packs.go. We store
-- it (not the variant_id) so renaming a LemonSqueezy variant
-- doesn't invalidate historical orders.

CREATE TABLE IF NOT EXISTS advisor_credit_orders (
    id                       UUID         PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                  UUID         NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    pack_sku                 VARCHAR(64)  NOT NULL,
    deep_units_granted       INTEGER      NOT NULL CHECK (deep_units_granted >= 0),
    quick_units_granted      INTEGER      NOT NULL CHECK (quick_units_granted >= 0),
    price_cents_usd          INTEGER      NOT NULL CHECK (price_cents_usd > 0),
    currency                 CHAR(3)      NOT NULL DEFAULT 'USD',
    status                   VARCHAR(16)  NOT NULL DEFAULT 'pending'
        CHECK (status IN ('pending', 'paid', 'refunded', 'failed')),
    lemonsqueezy_order_id    VARCHAR(128),
    lemonsqueezy_variant_id  VARCHAR(64),
    lemonsqueezy_event_id    VARCHAR(128),
    checkout_url             TEXT         NOT NULL DEFAULT '',
    paid_at                  TIMESTAMPTZ,
    refunded_at              TIMESTAMPTZ,
    raw_webhook_payload      JSONB        NOT NULL DEFAULT '{}'::jsonb,
    created_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_advisor_credit_orders_user_created
    ON advisor_credit_orders (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_advisor_credit_orders_status
    ON advisor_credit_orders (status, created_at DESC);
-- Idempotency index: webhook handler does INSERT ... ON CONFLICT.
-- NULL is allowed here (pending orders predate the LemonSqueezy
-- callback) but unique when set, so the second event for the same
-- order can't double-credit. Partial index handles the NULL case
-- without violating uniqueness.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_advisor_credit_orders_lsorder
    ON advisor_credit_orders (lemonsqueezy_order_id)
    WHERE lemonsqueezy_order_id IS NOT NULL;
-- Same idea for webhook event id (LS may send a different event id
-- on the same order for retries; we still want at-most-once).
CREATE UNIQUE INDEX IF NOT EXISTS uniq_advisor_credit_orders_lsevent
    ON advisor_credit_orders (lemonsqueezy_event_id)
    WHERE lemonsqueezy_event_id IS NOT NULL;

CREATE OR REPLACE FUNCTION touch_advisor_credit_orders_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_advisor_credit_orders_updated_at ON advisor_credit_orders;
CREATE TRIGGER trg_advisor_credit_orders_updated_at
    BEFORE UPDATE ON advisor_credit_orders
    FOR EACH ROW EXECUTE FUNCTION touch_advisor_credit_orders_updated_at();

COMMENT ON TABLE advisor_credit_orders IS
    'Phase C-1 credit-pack purchase ledger. Idempotent on lemonsqueezy_order_id.';

-- ---------------------------------------------------------------------------
-- ALTER advisor_consultations — service-unit accounting per consult
-- ---------------------------------------------------------------------------
--
-- service_unit_cost is the number of units the consult charged
-- (typically 1; reserved for future "deep+1" upsell where a single
-- consult burns 2 units).
--
-- service_unit_source records which bucket the unit came from
-- ('plan', 'credit', 'unmetered') so admins can build "Premium
-- users use X% credit packs" reports.

ALTER TABLE advisor_consultations
    ADD COLUMN IF NOT EXISTS service_unit_cost   INTEGER     NOT NULL DEFAULT 1
        CHECK (service_unit_cost >= 0),
    ADD COLUMN IF NOT EXISTS service_unit_source VARCHAR(16) NOT NULL DEFAULT 'plan'
        CHECK (service_unit_source IN ('plan', 'credit', 'unmetered'));

COMMENT ON COLUMN advisor_consultations.service_unit_cost IS
    'Phase C-1 service units charged for this consult. Usually 1.';
COMMENT ON COLUMN advisor_consultations.service_unit_source IS
    'Phase C-1 which quota bucket paid for the consult: plan / credit / unmetered (admin / system).';
