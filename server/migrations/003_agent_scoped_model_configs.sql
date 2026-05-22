ALTER TABLE user_model_configs
    ADD COLUMN IF NOT EXISTS agent_id UUID REFERENCES agents(id) ON DELETE CASCADE;

ALTER TABLE user_model_configs
    DROP CONSTRAINT IF EXISTS user_model_configs_config_type_check;

ALTER TABLE user_model_configs
    ADD CONSTRAINT user_model_configs_config_type_check
    CHECK (config_type IN ('tier_override', 'custom_endpoint', 'agent_default'));

CREATE INDEX IF NOT EXISTS idx_user_model_configs_agent
    ON user_model_configs(agent_id, is_active)
    WHERE agent_id IS NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_user_model_configs_agent_default
    ON user_model_configs(user_id, agent_id, config_type)
    WHERE is_active = true AND config_type = 'agent_default';
