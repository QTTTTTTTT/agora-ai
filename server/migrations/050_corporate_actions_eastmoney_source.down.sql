-- Restore the 049 source enum exactly. Note the cash_credit comment
-- isn't reverted because dropping a column comment doesn't break
-- anything and keeping the more accurate text is harmless.
ALTER TABLE corporate_actions
    DROP CONSTRAINT IF EXISTS corporate_actions_source_check;

ALTER TABLE corporate_actions
    ADD CONSTRAINT corporate_actions_source_check
        CHECK (source IN ('manual', 'yahoo', 'tushare', 'sina', 'tencent'));
