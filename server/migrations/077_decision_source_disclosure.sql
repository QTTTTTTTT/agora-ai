-- 077: Sprint 11.1 — surface LLM-vs-fallback decision provenance.
--
-- Today the fallback path returns plan rows that look identical to
-- successful LLM runs (same shape, same confidence column populated by
-- the legacy heuristic at 0.55). The decision center has no way to
-- tell whether "today's PM plan" came from Claude / GPT or from a 5-line
-- deterministic rule that fired because the LLM failed.
--
-- This migration adds two columns:
--
--   decision_source  enum-style TEXT — six finite values:
--     llm_pm                — single-shot LLM PM succeeded
--     llm_three_stage       — S9.4 trader→risk→PM pipeline succeeded
--     fallback_no_llm       — no LLM client wired (legacy deploy)
--     fallback_after_llm_error
--                           — LLM was wired but Decide() returned err
--     fallback_empty_plan   — LLM returned 0 actions
--     legacy                — pre-S11 rows we never re-classify
--
--   fallback_reason  JSONB — populated only for fallback_* rows. Shape:
--     { "category": "rate_limited" | "service_unavailable" | ... ,
--       "provider": "openai" | "claude" | ...,
--       "model":    "gpt-4o" | "claude-opus-4" | ...,
--       "attempt":  N,
--       "summary":  "short technical summary (NEVER user-facing)",
--       "at":       RFC-3339 timestamp }
--
-- decision_source is intentionally a TEXT column rather than a real
-- ENUM type so future LLM pipelines (e.g. ensemble engines) can extend
-- the set without an ALTER TYPE migration. The errorclass package owns
-- the authoritative enum on the Go side.

ALTER TABLE investment_plans
  ADD COLUMN IF NOT EXISTS decision_source TEXT NOT NULL DEFAULT 'legacy',
  ADD COLUMN IF NOT EXISTS fallback_reason JSONB;

-- Partial index on fallback_* sources — the admin LLM-health dashboard
-- (S11.4) ranges on decision_source while restricting to recent rows;
-- a partial index keeps the green/fast-path rows out of the index.
CREATE INDEX IF NOT EXISTS idx_investment_plans_fallback
  ON investment_plans (created_at DESC, decision_source)
  WHERE decision_source LIKE 'fallback_%';

COMMENT ON COLUMN investment_plans.decision_source IS
  'How the plan was produced. One of llm_pm, llm_three_stage, fallback_no_llm, fallback_after_llm_error, fallback_empty_plan, legacy.';
COMMENT ON COLUMN investment_plans.fallback_reason IS
  'JSONB; populated only when decision_source LIKE ''fallback_%''. Keys: category, provider, model, attempt, summary, at. Summary is technical and must NOT be exposed to non-admin users verbatim.';
