-- Rollback for 098_advisor.
--
-- Order matters: child tables first (FK to advisor_consultations),
-- then the parent + the presets table. The feature flag row is
-- removed last so monitoring keeps seeing it until the surface is
-- fully torn down.

DROP TABLE IF EXISTS advisor_tactic_reports;
DROP TABLE IF EXISTS advisor_master_reports;
DROP TABLE IF EXISTS advisor_consultations;
DROP TABLE IF EXISTS advisor_persona_presets;

DELETE FROM feature_flags WHERE flag_key = 'advisor_mode';
