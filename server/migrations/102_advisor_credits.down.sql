ALTER TABLE advisor_consultations DROP COLUMN IF EXISTS service_unit_cost;
ALTER TABLE advisor_consultations DROP COLUMN IF EXISTS service_unit_source;

DROP TRIGGER IF EXISTS trg_advisor_credit_orders_updated_at ON advisor_credit_orders;
DROP FUNCTION IF EXISTS touch_advisor_credit_orders_updated_at();
DROP INDEX IF EXISTS uniq_advisor_credit_orders_lsevent;
DROP INDEX IF EXISTS uniq_advisor_credit_orders_lsorder;
DROP INDEX IF EXISTS idx_advisor_credit_orders_status;
DROP INDEX IF EXISTS idx_advisor_credit_orders_user_created;
DROP TABLE IF EXISTS advisor_credit_orders;

DROP TRIGGER IF EXISTS trg_user_advisor_credits_updated_at ON user_advisor_credits;
DROP FUNCTION IF EXISTS touch_user_advisor_credits_updated_at();
DROP INDEX IF EXISTS idx_user_advisor_credits_has_balance;
DROP TABLE IF EXISTS user_advisor_credits;
