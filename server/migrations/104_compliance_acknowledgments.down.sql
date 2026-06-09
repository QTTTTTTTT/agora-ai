-- Down migration for 104_compliance_acknowledgments.
--
-- Removes the disclosure-ack and phrase-violation tables and
-- clears the BOOKS-AND-RECORDS comments on the paper trading
-- tables.
--
-- Safe to run; nothing downstream depends on the tables
-- structurally — they are pure compliance scratch space.

DROP INDEX IF EXISTS idx_compliance_violations_rule;
DROP INDEX IF EXISTS idx_compliance_violations_recent;
DROP TABLE IF EXISTS compliance_phrase_violations;

DROP INDEX IF EXISTS idx_compliance_ack_user;
DROP INDEX IF EXISTS uniq_compliance_ack_user_surface_mode_version;
DROP TABLE IF EXISTS compliance_acknowledgments;

COMMENT ON TABLE paper_orders IS NULL;
COMMENT ON TABLE paper_portfolios IS NULL;
