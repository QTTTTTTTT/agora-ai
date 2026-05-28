-- Sprint 3 / M4: memory archive.
--
-- The runtime PM/Risk/debate prompts are not interested in lessons
-- older than 30-60 days, but we cannot DELETE them outright — they
-- still matter for audit, retrospective scoring (M1), and replay. The
-- archive table is structurally identical to memories so a nightly job
-- can move (not copy) rows in batches without surgery on the live
-- working set.
--
-- Schema parity: we mirror EVERY column that exists on memories as of
-- migration 042. If the live table evolves later we'll add the new
-- columns here in a follow-up migration; the archive job already does
-- an explicit column list so a divergent shape would surface as a
-- compile-time error, not a silent data loss.
--
-- The archive table intentionally drops:
--  * foreign keys on fund_id/agent_id/source_listing_id: the parent
--    rows might be ON DELETE CASCADE'd in the future and we must not
--    lose archived audit history. We keep the columns as plain UUIDs.
--  * the visibility/sensitivity CHECK constraints: archived rows are
--    not surfaced via API, so the constraint is dead weight.
--  * the layer CHECK constraint: archived rows are categorically
--    "archived" and never re-routed.

CREATE TABLE IF NOT EXISTS memories_archive (
    id UUID PRIMARY KEY,
    fund_id UUID NOT NULL,
    agent_id UUID NULL,
    layer VARCHAR(20) NOT NULL,
    title VARCHAR(512) NULL,
    content TEXT NOT NULL,
    trading_date DATE NULL,
    tags TEXT[] NULL,
    owner_user_id UUID NULL,
    visibility VARCHAR(20) NOT NULL DEFAULT 'private',
    sensitivity VARCHAR(20) NOT NULL DEFAULT 'internal',
    origin_kind VARCHAR(30) NOT NULL DEFAULT 'native',
    source_listing_id UUID NULL,
    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,
    archived_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_memories_archive_fund
    ON memories_archive(fund_id);
CREATE INDEX IF NOT EXISTS idx_memories_archive_layer
    ON memories_archive(layer);
CREATE INDEX IF NOT EXISTS idx_memories_archive_created
    ON memories_archive(created_at);
CREATE INDEX IF NOT EXISTS idx_memories_archive_trading_date
    ON memories_archive(trading_date);
