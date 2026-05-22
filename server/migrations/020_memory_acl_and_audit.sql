-- Migration 020: memory ACL and data access audit.
--
-- This migration introduces explicit access control fields to memories,
-- solving the issue where marketplace snapshots leaked IP by relying on
-- user-controllable tags like 'self_learning'.
-- It also introduces a general data access audit log.

CREATE TABLE IF NOT EXISTS data_access_log (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    action VARCHAR(50) NOT NULL,
    resource_type VARCHAR(50) NOT NULL,
    resource_id UUID NOT NULL,
    details JSONB NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_data_access_log_actor ON data_access_log (actor_user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_data_access_log_resource ON data_access_log (resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_data_access_log_resource_created ON data_access_log (resource_type, resource_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_data_access_log_action_created ON data_access_log (action, created_at DESC);

ALTER TABLE memories
    ADD COLUMN IF NOT EXISTS owner_user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS visibility VARCHAR(20) NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'fund', 'marketplace')),
    ADD COLUMN IF NOT EXISTS sensitivity VARCHAR(20) NOT NULL DEFAULT 'internal' CHECK (sensitivity IN ('public', 'internal', 'secret')),
    ADD COLUMN IF NOT EXISTS origin_kind VARCHAR(30) NOT NULL DEFAULT 'native' CHECK (origin_kind IN ('native', 'imported_from_marketplace')),
    ADD COLUMN IF NOT EXISTS source_listing_id UUID REFERENCES agent_market_listings(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_memories_owner ON memories (owner_user_id);
CREATE INDEX IF NOT EXISTS idx_memories_visibility ON memories (visibility);
