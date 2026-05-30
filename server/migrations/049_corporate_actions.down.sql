-- 049_corporate_actions.down.sql
DROP INDEX IF EXISTS corp_action_applications_fund_idx;
DROP TABLE IF EXISTS corp_action_applications;

DROP INDEX IF EXISTS corporate_actions_instrument_date_idx;
DROP INDEX IF EXISTS corporate_actions_dedup;
DROP TABLE IF EXISTS corporate_actions;
