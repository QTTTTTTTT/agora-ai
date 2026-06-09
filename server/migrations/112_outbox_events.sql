-- Migration: 112_outbox_events
-- Description:
--   Add the transactional-outbox primitive. The intent is to give
--   the audit / lineage / attribution writers (and any future
--   side-effect that needs to "fan out to external systems
--   without losing data") a single ordered queue that:
--
--     - Lives in the SAME PostgreSQL transaction as the business
--       row, so if the tx rolls back the queued event vanishes
--       too. No dual-write inconsistency window.
--
--     - Is consumed by a background flusher process (see
--       internal/outbox/flusher.go) which gets exactly-once
--       semantics from a row-level FOR UPDATE SKIP LOCKED claim
--       + idempotent handler design.
--
--   Why a single events table instead of one queue per consumer
--
--     Two reasons. First, ordering: a few of our use cases want
--     causal order across event types (e.g. "lineage edge added"
--     must be visible before "agent listed" is published) and a
--     single table with monotonic created_at gives us that for
--     free. Second, operational surface area: one table to back
--     up, one set of metrics, one truncation policy.
--
--   When the queue grows past a few million rows we'll partition
--   by month — that's a future-proofing migration, not a v1
--   concern.
--
--   The handler executes inside its own tx; on success it sets
--   consumed_at. On retryable failure it increments attempts and
--   sets last_error. On a permanent failure (poison message) the
--   admin tool moves it to consumed_at + last_error="dead" so the
--   queue doesn't stall.

CREATE TABLE IF NOT EXISTS outbox_events (
    id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type   TEXT NOT NULL,
    -- aggregate_type / aggregate_id let consumers filter without
    -- decoding the payload (e.g. "give me all events about this
    -- particular agent"). Both optional — pure cross-cutting
    -- events leave them empty.
    aggregate_type TEXT NOT NULL DEFAULT '',
    aggregate_id   TEXT NOT NULL DEFAULT '',
    payload      JSONB NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- consumed_at NULL means the row is unprocessed. A non-NULL
    -- value means a flusher has handled it; we keep the row for
    -- audit replay until the periodic cleanup job removes it.
    consumed_at  TIMESTAMPTZ,
    attempts     INTEGER NOT NULL DEFAULT 0,
    last_error   TEXT NOT NULL DEFAULT ''
);

-- Pending-only partial index: the flusher's hot query is
-- "WHERE consumed_at IS NULL ORDER BY created_at LIMIT n". A
-- partial index keeps the working set tiny — we only ever index
-- the rows the worker actually scans. Once consumed_at is stamped,
-- the row drops out of the index automatically.
CREATE INDEX IF NOT EXISTS idx_outbox_events_pending
    ON outbox_events (created_at)
    WHERE consumed_at IS NULL;

-- Aggregate lookup for the "give me everything about agent X"
-- replay query. Sparse so it's only indexed when the publisher
-- actually populates the aggregate fields.
CREATE INDEX IF NOT EXISTS idx_outbox_events_aggregate
    ON outbox_events (aggregate_type, aggregate_id, created_at)
    WHERE aggregate_type <> '';
