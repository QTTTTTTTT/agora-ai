-- P0-8: append-only audit hash chain for tamper detection.
--
-- Why this migration exists
-- -------------------------
-- The platform records two audit streams today:
--
--   * data_access_log   — read events (who looked at what, when)
--   * admin_change_log  — mutation events (before/after JSON snapshots)
--
-- Both tables already enforce write-only patterns at the application
-- layer (no UPDATE / DELETE statements anywhere in audit/audit.go), but
-- a determined attacker with direct database access can rewrite history
-- silently. For a live-trading platform that is unacceptable: SOX-grade
-- audit requires that any post-hoc tamper be DETECTABLE even if it
-- cannot be PREVENTED at the storage layer.
--
-- Design
-- ------
-- We hash-chain the rows. Each new row carries:
--
--   prev_hash : the row_hash of the immediately-preceding row in the
--               chain (NULL for the genesis row of each table).
--   row_hash  : sha256(prev_hash_or_zero || canonical_encoding(this_row))
--
-- The canonical encoding includes (id, actor, action, target,
-- created_at_ns, encoding_version, details_hash). details_hash is a
-- separate sha256 of the canonical JSON of details / before / after /
-- metadata payloads — see audit/chain.go for the deterministic encoder.
--
-- Storing details_hash alongside the raw JSONB lets us detect tamper
-- of the JSON body too: a verifier recomputes details_hash from the
-- current JSONB and compares with the stored value.
--
-- Verification
-- ------------
-- audit.VerifyChain(ctx, db) walks the rows in (created_at, id) order
-- and re-derives row_hash for each row. A mismatch at row N tells the
-- operator that EITHER row N was modified OR rows N..M were inserted
-- between two legitimate rows. The endpoint
-- GET /api/audit/chain/verify exposes this for ops.
--
-- Backwards compatibility
-- -----------------------
-- Both new columns are NULL-able. Rows written before this migration
-- carry NULL for both fields and are treated by the verifier as
-- "pre-chain". The verifier locates the first hashed row and reports:
--
--   * the chain segment length (number of rows under the chain)
--   * any tamper detected within that segment
--
-- Operators can backfill legacy rows post-deployment using the
-- separate `audit-backfill` command (TBD); doing it inside the SQL
-- migration would require pgcrypto + pgcrypto isn't installed in
-- production today.

BEGIN;

-- ---------------------------------------------------------------------------
-- 1. data_access_log — read-event chain
-- ---------------------------------------------------------------------------

ALTER TABLE data_access_log
    ADD COLUMN IF NOT EXISTS prev_hash    BYTEA,
    ADD COLUMN IF NOT EXISTS row_hash     BYTEA,
    ADD COLUMN IF NOT EXISTS details_hash BYTEA;

CREATE INDEX IF NOT EXISTS idx_data_access_log_chain
    ON data_access_log (created_at, id)
    WHERE row_hash IS NOT NULL;

COMMENT ON COLUMN data_access_log.prev_hash IS
    'SHA-256 of the previous chained row in (created_at, id) order. NULL for the genesis row of the chain. See audit/chain.go.';
COMMENT ON COLUMN data_access_log.row_hash IS
    'SHA-256 of the canonical encoding of (prev_hash_or_zero || metadata fields || details_hash). Used by audit.VerifyChain.';
COMMENT ON COLUMN data_access_log.details_hash IS
    'SHA-256 of the canonical-JSON encoding of the details payload. Lets the verifier detect tamper of the JSONB body alongside metadata tamper.';

-- ---------------------------------------------------------------------------
-- 2. admin_change_log — mutation-event chain
-- ---------------------------------------------------------------------------

ALTER TABLE admin_change_log
    ADD COLUMN IF NOT EXISTS prev_hash       BYTEA,
    ADD COLUMN IF NOT EXISTS row_hash        BYTEA,
    ADD COLUMN IF NOT EXISTS before_hash     BYTEA,
    ADD COLUMN IF NOT EXISTS after_hash      BYTEA,
    ADD COLUMN IF NOT EXISTS metadata_hash   BYTEA;

CREATE INDEX IF NOT EXISTS idx_admin_change_log_chain
    ON admin_change_log (created_at, id)
    WHERE row_hash IS NOT NULL;

COMMENT ON COLUMN admin_change_log.prev_hash IS
    'SHA-256 of the previous chained row in (created_at, id) order. NULL for the genesis row.';
COMMENT ON COLUMN admin_change_log.row_hash IS
    'SHA-256 of the canonical encoding of (prev_hash_or_zero || metadata fields || before_hash || after_hash || metadata_hash).';
COMMENT ON COLUMN admin_change_log.before_hash IS
    'SHA-256 of the canonical-JSON encoding of before_snapshot. NULL when before_snapshot is NULL.';
COMMENT ON COLUMN admin_change_log.after_hash IS
    'SHA-256 of the canonical-JSON encoding of after_snapshot. NULL when after_snapshot is NULL.';
COMMENT ON COLUMN admin_change_log.metadata_hash IS
    'SHA-256 of the canonical-JSON encoding of metadata. NULL when metadata is NULL or {}.';

COMMIT;
