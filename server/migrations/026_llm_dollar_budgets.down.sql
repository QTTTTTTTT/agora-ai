-- F31 down for F14 (026_llm_dollar_budgets.sql).
-- DROP order is the reverse of CREATE: indexes implicitly disappear
-- with the table, but we list them explicitly to keep the down
-- migration self-documenting (an operator reading this should be able
-- to reason about the schema delta without cross-referencing the up).

DROP INDEX IF EXISTS llm_budgets_user_wide_uniq;
DROP INDEX IF EXISTS llm_budgets_user_fund_uniq;
DROP TABLE IF EXISTS llm_budgets;
