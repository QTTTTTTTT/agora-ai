-- Migration: 105_advisor_consultations_symbol_name
-- Description:
--   Adds advisor_consultations.symbol_name — the issuer's short
--   Chinese / English name (e.g. "德科立", "Apple Inc.") resolved
--   at consult time by the fundamentals loader (akshare sidecar
--   for A-shares via sina hqjs, Yahoo quoteSummary for US).
--
--   Why a column and not a join: the issuer's name at the moment
--   the consultation was generated is a historical fact. If
--   "德科立" gets delisted and the ticker is re-used by a new
--   company, the old advisor history must still read "德科立" to
--   make sense. Joining against a current snapshot would silently
--   rewrite history.
--
--   NULL is allowed because:
--     * legacy rows persisted before this migration won't have it
--     * mock loaders (in-memory tests) don't resolve a name
--     * upstream data providers can be transiently unreachable —
--       a successful consult shouldn't be blocked on name lookup
--   Consumers must tolerate NULL (the UI falls back to bare symbol).
--
--   A short btree index on symbol_name lets us later support the
--   "search advisor history by name" UX without a full table scan.
--   Filtered to NOT NULL keeps the index small until coverage
--   catches up.

ALTER TABLE advisor_consultations
    ADD COLUMN IF NOT EXISTS symbol_name TEXT;

COMMENT ON COLUMN advisor_consultations.symbol_name IS
    'Issuer short name at consult time (e.g. "德科立"). NULL for legacy rows and rows where the upstream provider did not resolve a name. Historical — not refreshed when the issuer renames or the ticker is re-used.';

CREATE INDEX IF NOT EXISTS idx_advisor_consultations_symbol_name
    ON advisor_consultations (symbol_name)
    WHERE symbol_name IS NOT NULL;
