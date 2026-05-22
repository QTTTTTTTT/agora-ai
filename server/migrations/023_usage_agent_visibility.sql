-- Migration 023: agent-level LLM usage visibility.

ALTER TABLE usage_entries
    ADD COLUMN IF NOT EXISTS agent_id UUID REFERENCES agents(id) ON DELETE SET NULL;

CREATE INDEX IF NOT EXISTS idx_usage_entries_agent ON usage_entries(agent_id, created_at);
