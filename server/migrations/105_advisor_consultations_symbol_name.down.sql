-- Down migration for 105_advisor_consultations_symbol_name
--
-- Drops the index first so the ALTER TABLE doesn't have to wait
-- for the index to be re-evaluated.

DROP INDEX IF EXISTS idx_advisor_consultations_symbol_name;

ALTER TABLE advisor_consultations
    DROP COLUMN IF EXISTS symbol_name;
