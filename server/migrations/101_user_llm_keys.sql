-- Migration: 101_user_llm_keys
-- Description:
--   Phase B-1 — user-supplied LLM API keys for /advisor mode (BYOK).
--
--   The platform already stores fund-scoped overrides
--   (fund_llm_overrides, migration 084) and user-scoped fund-mode
--   overrides (model_configs, migration 002). Neither covers what
--   the /advisor surface needs:
--
--     * /advisor is fund-less; model_configs requires a model_tier
--       routing path that is fund-mode shaped (it solves "which
--       model when this fund's PM agent runs").
--     * The advisor BYOK story is "user pays for compute through
--       their own OpenAI/Anthropic account, we charge a service
--       fee per consult"; that is a different security model
--       (key never leaves the user's tenant, must be revocable
--       from the SPA, must track monthly $ cap) from the existing
--       model_configs row (which is per-user "I want to swap the
--       platform's OpenAI key for mine on every call site").
--
--   Schema is deliberately narrow:
--     * provider is one of openai / anthropic / deepseek / kimi.
--       Adding a new provider is a one-line code change + an
--       ALTER CONSTRAINT.
--     * api_key_encrypted reuses subscription.EncryptAPIKey
--       (AES-GCM with MODEL_CONFIG_API_KEY_SECRET) — no new
--       crypto, no new secret to rotate.
--     * monthly_budget_cents_usd is the soft cap the user opts
--       into. UserOverrideHook reads it; when the rolling 30-day
--       spend (tracked off-line by the LLM router's existing
--       usage_entries pipeline) exceeds the cap, the BYOK lane
--       disables for the rest of the month and the consult
--       silently falls back to the platform key (or to 402 if
--       the user is out of platform credits too).

CREATE TABLE IF NOT EXISTS user_llm_keys (
    id                         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id                    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider                   VARCHAR(32) NOT NULL
        CHECK (provider IN ('openai', 'anthropic', 'deepseek', 'kimi', 'doubao', 'qwen')),
    label                      VARCHAR(64) NOT NULL DEFAULT '',
    api_key_encrypted          TEXT NOT NULL,
    api_key_fingerprint        VARCHAR(64) NOT NULL,
    base_url                   TEXT NOT NULL DEFAULT '',
    model_name                 VARCHAR(128) NOT NULL DEFAULT '',
    monthly_budget_cents_usd   INTEGER NOT NULL DEFAULT 0
        CHECK (monthly_budget_cents_usd >= 0),
    is_active                  BOOLEAN NOT NULL DEFAULT TRUE,
    last_used_at               TIMESTAMPTZ,
    last_verified_at           TIMESTAMPTZ,
    revoked_at                 TIMESTAMPTZ,
    revoked_reason             TEXT NOT NULL DEFAULT '',
    created_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at                 TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- A user gets at most one active key per provider. The fingerprint
-- is the SHA-256 of the plaintext key prefix (first 8 chars +
-- last 4 chars), so the user can recognise their key in the UI
-- without us ever decrypting on read — and so we can dedup if the
-- user re-submits the same key.
CREATE UNIQUE INDEX IF NOT EXISTS uniq_user_llm_keys_active_provider
    ON user_llm_keys (user_id, provider) WHERE revoked_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_user_llm_keys_user_active
    ON user_llm_keys (user_id, is_active, revoked_at);

CREATE INDEX IF NOT EXISTS idx_user_llm_keys_fingerprint
    ON user_llm_keys (api_key_fingerprint);

CREATE OR REPLACE FUNCTION touch_user_llm_keys_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_user_llm_keys_updated_at ON user_llm_keys;
CREATE TRIGGER trg_user_llm_keys_updated_at
    BEFORE UPDATE ON user_llm_keys
    FOR EACH ROW EXECUTE FUNCTION touch_user_llm_keys_updated_at();

COMMENT ON TABLE user_llm_keys IS
    'Phase B-1 BYOK store. AES-GCM encrypted with MODEL_CONFIG_API_KEY_SECRET. UserOverrideHook reads the active row per user+provider at LLM-call time and routes through the user''s key instead of the platform pool.';
COMMENT ON COLUMN user_llm_keys.api_key_fingerprint IS
    'SHA-256(plaintext_prefix||plaintext_suffix) — read on the SPA to render "sk-...K8s2" without server-side decrypt.';
COMMENT ON COLUMN user_llm_keys.monthly_budget_cents_usd IS
    'User opt-in soft cap. 0 = no cap. BYOK disables when rolling 30d spend exceeds the cap.';

-- Phase B-4 — `advisor_byok` master feature flag.
--
-- A second gate ON TOP OF advisor_mode (seeded by migration 098).
-- Lets ops disable BYOK platform-wide (e.g. during an incident
-- where the encryption secret rotation breaks) without dropping
-- migrations or de-wiring the LLM router hook. When enforce_server_gate
-- = TRUE the BYOK handler returns 403 with "byok_disabled" so the
-- SPA falls back to the platform-pool path.
INSERT INTO feature_flags
    (flag_key, label, description, enabled, affects_routes, enforce_server_gate)
VALUES
    (
        'advisor_byok',
        '大师团队 BYOK',
        '允许付费用户在 advisor 模式自带 OpenAI/Anthropic/DeepSeek 等 API key。关闭后所有 advisor consult 回落到平台 LLM 池，且 /api/advisor/byok/keys 端点 403。',
        TRUE,
        ARRAY['/advisor/byok', '/settings/byok'],
        TRUE
    )
ON CONFLICT (flag_key) DO NOTHING;
