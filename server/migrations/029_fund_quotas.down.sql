-- F31 down for F28 (029_fund_quotas.sql).
-- Drops the per-fund quota tables. quota.Service degrades to "no
-- enforcement" once these are gone (CheckActiveAgents / CheckLLMTokens
-- become no-ops), so this is operationally safe to run on a stopped
-- application — quotas won't be RE-enforced, but nothing breaks.

DROP INDEX IF EXISTS fund_llm_token_usage_date_idx;
DROP TABLE IF EXISTS fund_llm_token_usage;

DROP INDEX IF EXISTS fund_quotas_default_uniq;
DROP INDEX IF EXISTS fund_quotas_fund_uniq;
DROP TABLE IF EXISTS fund_quotas;
