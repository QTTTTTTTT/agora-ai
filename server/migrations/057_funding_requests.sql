-- Migration 057 — funding_requests (P1-2).
--
-- Why this table exists
--
-- Funds need a way to receive cash (deposit) and return cash to
-- their owners (withdrawal). Until P1-1 the only paths into
-- funds.current_capital were:
--   - the trading engine (debits / credits net of fees)
--   - corp-action applier (dividend credits)
--
-- Real funds also need:
--   - LP capital calls / subscriptions
--   - LP redemptions
--   - Manager fee sweeps
--   - Tax payments
--
-- This table is the queue + audit ledger for those movements.
-- Every row is a request that goes through:
--
--   pending     → submitted by a fund owner / admin
--                 4-eye approval still required
--   approved    → second admin signed off (approver != requester)
--                 → cash_ledger row + funds.current_capital UPDATE
--                 happen in the same DB transaction
--   rejected    → terminal; second admin declined; reason captured
--   cancelled   → terminal; requester withdrew their own request
--                 before approval
--   posted      → not used yet (deposit/withdrawal that needs an
--                 external broker confirmation before the cash
--                 actually moves; reserved for live-broker phase)
--
-- The 4-eye check is enforced server-side, not by the schema
-- (Postgres can't see "the user" without a SECURITY DEFINER
-- function). The migration just records WHO requested and WHO
-- approved; the handler refuses approved_by = requested_by with
-- a 403 four_eye_violation.
--
-- Linkage: when a request is approved, the resulting cash_ledger
-- row carries this request's id in metadata.funding_request_id,
-- and the request row carries the cash_ledger row id in
-- cash_ledger_entry_id. Either side reconstructs forensics.

CREATE TABLE IF NOT EXISTS funding_requests (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id             UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    direction           VARCHAR(16) NOT NULL,
    -- Stored as positive; the sign is implied by direction.
    -- Withdrawals and deposits use the same column to keep
    -- aggregate queries (SUM by status) simple.
    amount              NUMERIC(20,4) NOT NULL,
    currency            VARCHAR(8) NOT NULL DEFAULT 'USD',
    -- method classifies HOW the cash arrives. We keep the
    -- vocabulary closed so a typo doesn't produce orphan rows
    -- the operator can't filter on. 'internal_transfer' covers
    -- inter-fund movements within the same fund company.
    method              VARCHAR(32) NOT NULL,
    -- Free-form reference field for an external slip number
    -- (wire ref, ACH trace id, manual ticket).
    external_reference  VARCHAR(128),
    -- Status state machine documented above.
    status              VARCHAR(16) NOT NULL DEFAULT 'pending',
    requested_by        UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,
    approved_by         UUID REFERENCES users(id) ON DELETE SET NULL,
    approved_at         TIMESTAMPTZ,
    rejected_by         UUID REFERENCES users(id) ON DELETE SET NULL,
    rejected_at         TIMESTAMPTZ,
    rejection_reason    TEXT,
    cancelled_at        TIMESTAMPTZ,
    -- Once approved + posted to cash_ledger, link back so the
    -- /api/funds/:id/cash-ledger view can hyperlink to the
    -- funding request that produced an entry.
    cash_ledger_entry_id UUID,
    -- Free-form notes captured at submission. Visible to
    -- approvers and the audit log.
    notes               TEXT,
    metadata            JSONB DEFAULT '{}'::jsonb,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT funding_requests_direction_chk
        CHECK (direction IN ('deposit', 'withdrawal')),
    CONSTRAINT funding_requests_status_chk
        CHECK (status IN ('pending', 'approved', 'rejected', 'cancelled', 'posted')),
    CONSTRAINT funding_requests_method_chk
        CHECK (method IN (
            'wire',
            'ach',
            'sepa',
            'check',
            'internal_transfer',
            'manual'
        )),
    CONSTRAINT funding_requests_amount_pos_chk CHECK (amount > 0),
    -- 4-eye invariant the schema CAN enforce: when approved, the
    -- approver must differ from the requester. Handler catches
    -- this earlier (better error UX) but the constraint is the
    -- last line of defence against a programmer bug.
    CONSTRAINT funding_requests_four_eye_chk
        CHECK (approved_by IS NULL OR approved_by <> requested_by)
);

CREATE INDEX IF NOT EXISTS funding_requests_fund_status_idx
    ON funding_requests (fund_id, status, created_at DESC);
CREATE INDEX IF NOT EXISTS funding_requests_status_created_idx
    ON funding_requests (status, created_at DESC)
    WHERE status = 'pending';
CREATE INDEX IF NOT EXISTS funding_requests_requested_by_idx
    ON funding_requests (requested_by);

CREATE OR REPLACE FUNCTION funding_requests_touch_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS funding_requests_touch_updated_at ON funding_requests;
CREATE TRIGGER funding_requests_touch_updated_at
    BEFORE UPDATE ON funding_requests
    FOR EACH ROW EXECUTE FUNCTION funding_requests_touch_updated_at();
