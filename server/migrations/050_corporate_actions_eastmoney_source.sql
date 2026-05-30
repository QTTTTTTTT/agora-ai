-- 050_corporate_actions_eastmoney_source.sql
--
-- Sprint 4 follow-up: extend `corporate_actions.source` to accept
-- 'eastmoney'. The original 049 migration enumerated only the
-- US/HK feeds we shipped first (yahoo) and the placeholder Chinese
-- feeds we hadn't built yet (tushare/sina/tencent). With Card A
-- the actual A-share provider lives at East Money's DataCenter
-- HTTP API, so the source label needs to match what the provider
-- writes.
--
-- We also use this migration to refresh the cash_credit comment
-- on corp_action_applications to reflect the F-mini change: cash
-- dividends are now posted to funds.current_capital inside the
-- applier transaction, so cash_credit on this table is no longer
-- "informational only" — it's the audit twin of the actual fund
-- balance mutation.

ALTER TABLE corporate_actions
    DROP CONSTRAINT IF EXISTS corporate_actions_source_check;

ALTER TABLE corporate_actions
    ADD CONSTRAINT corporate_actions_source_check
        CHECK (source IN ('manual', 'yahoo', 'tushare', 'sina', 'tencent', 'eastmoney'));

COMMENT ON COLUMN corp_action_applications.cash_credit IS 'Total cash credited to the fund (= pre_quantity * cash_dividend, gross). Posted live to funds.current_capital inside the applier transaction, so this column is the audit twin: a non-zero value here implies an equal increment on funds.current_capital at applied_at.';
