-- Down migration: 112_outbox_events
-- Removes the transactional-outbox table and its indexes.

DROP INDEX IF EXISTS idx_outbox_events_aggregate;
DROP INDEX IF EXISTS idx_outbox_events_pending;
DROP TABLE IF EXISTS outbox_events;
