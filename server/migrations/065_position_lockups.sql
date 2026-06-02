-- Migration 065 — IPO lock-up store (S6.3).
--
-- What this stores
--
-- Per-fund × per-instrument lock-up records. Each row says "of
-- the fund's total position in this instrument, this many
-- shares cannot be sold until locked_until". A position can
-- have multiple overlapping records (e.g. two separate IPO
-- allocations on different dates) — the engine sums all active
-- ones to get total locked qty.
--
-- Lifecycle
--
--   * created   — admin records a new lock-up after IPO
--                 allocation, pre-IPO placement, RSU vest, etc.
--   * active    — released_at IS NULL AND locked_until > now()
--   * expired   — released_at IS NULL AND locked_until <= now()
--                 (automatically inert; no admin action needed)
--   * released  — released_at IS NOT NULL
--                 (admin early-released for an explicit reason;
--                  audit-logged)
--
-- Why per-row instead of "locked_qty on the holding"
--
--   * Different lots can unlock on different dates. A row per
--     event is the only way to model "100 IPO shares unlock
--     2026-12-01, 50 RSU shares unlock 2027-03-15".
--   * History matters for compliance — you must be able to
--     answer "why was this position untradeable on date X" two
--     years later.
--   * Operator can early-release one record without losing the
--     audit trail of the others.
--
-- Why source_lot_id
--
-- Optional FK to lot_ledger so the operator can navigate
-- "this lock-up was placed on the IPO lot booked on 2026-08-15".
-- Nullable because back-filled records (legacy positions
-- imported from a broker) may not have a lot_id.

CREATE TABLE IF NOT EXISTS position_lockups (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    fund_id         UUID NOT NULL REFERENCES funds(id) ON DELETE CASCADE,
    instrument_key  VARCHAR(64) NOT NULL,
    symbol          VARCHAR(64) NOT NULL,
    locked_qty      NUMERIC(20, 4) NOT NULL CHECK (locked_qty > 0),
    locked_until    TIMESTAMPTZ NOT NULL,
    -- Closed enum so the analytics layer can pivot by reason.
    lockup_reason   VARCHAR(32) NOT NULL DEFAULT 'ipo'
                      CHECK (lockup_reason IN (
                          'ipo',
                          'private_placement',
                          'rsu',
                          'restricted',
                          'employee_grant',
                          'block_sale',
                          'other'
                      )),
    source_lot_id   UUID,
    note            TEXT,
    -- released_at + released_reason capture an early-release.
    -- Required together; either both NULL or both set.
    released_at     TIMESTAMPTZ,
    released_reason VARCHAR(255),
    released_by     UUID,
    created_by      UUID,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CHECK ((released_at IS NULL) = (released_reason IS NULL))
);

-- Lookup index: the gate's hot query is "active lockups for
-- (fund_id, instrument_key) at a given time". A composite
-- index on (fund_id, instrument_key, locked_until) covers it
-- and stays small even with millions of expired rows because
-- we always scan with locked_until > as_of.
CREATE INDEX IF NOT EXISTS position_lockups_lookup_idx
    ON position_lockups (fund_id, instrument_key, locked_until)
    WHERE released_at IS NULL;

-- Partial index for the admin-side "active" filter.
CREATE INDEX IF NOT EXISTS position_lockups_active_idx
    ON position_lockups (locked_until)
    WHERE released_at IS NULL;
