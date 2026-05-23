-- p2_clear_auto_init_agent_models.sql
--
-- Optional one-shot script. Run ONLY if you want to retroactively undo
-- the old AddAgent auto-init that silently bound every freshly created
-- agent to a hard-coded provider/model (claude-sonnet-4-6 for pm/risk,
-- claude-opus-4-7 for researcher, gpt-4o for trader). Post-T1 (commit
-- after 2026-05-23), new agents are created with NULL model_provider /
-- model_name and naturally fall back to the .env platform default.
--
-- Existing rows still carry the old defaults — they're indistinguishable
-- from "the operator deliberately chose claude-sonnet-4-6". This script
-- nukes only the exact (role, provider, model_name) tuples that the
-- removed defaultAgentModel() function used to produce. If your
-- operators picked these models on purpose, DO NOT run this script —
-- you'll lose those preferences and re-introduce the silent-downgrade
-- to the .env default.
--
-- Usage:
--   docker exec -i fundai-postgres psql -U fundai -d fundai \
--     < scripts/p2_clear_auto_init_agent_models.sql
--
-- Then restart the app so llmRuntime.SyncAll picks up the cleared rows:
--   docker compose restart app
--
-- Verify per-user, e.g.:
--   SELECT id, role, model_provider, model_name
--     FROM agents WHERE user_id = '<USER-UUID>';

BEGIN;

-- Preview affected rows before committing.
SELECT id, user_id, role, model_provider, model_name
  FROM agents
 WHERE (role = 'pm'         AND model_provider = 'claude' AND model_name = 'claude-sonnet-4-6')
    OR (role = 'risk'       AND model_provider = 'claude' AND model_name = 'claude-sonnet-4-6')
    OR (role = 'researcher' AND model_provider = 'claude' AND model_name = 'claude-opus-4-7')
    OR (role = 'trader'     AND model_provider = 'openai' AND model_name = 'gpt-4o');

UPDATE agents
   SET model_provider = NULL,
       model_name     = NULL,
       llm_model      = NULL,
       updated_at     = NOW()
 WHERE (role = 'pm'         AND model_provider = 'claude' AND model_name = 'claude-sonnet-4-6')
    OR (role = 'risk'       AND model_provider = 'claude' AND model_name = 'claude-sonnet-4-6')
    OR (role = 'researcher' AND model_provider = 'claude' AND model_name = 'claude-opus-4-7')
    OR (role = 'trader'     AND model_provider = 'openai' AND model_name = 'gpt-4o');

COMMIT;
