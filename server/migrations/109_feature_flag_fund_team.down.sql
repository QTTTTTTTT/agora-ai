-- Down migration: 109_feature_flag_fund_team
-- Removes the fund_team feature flag. Safe to re-up by re-running
-- migration 109 (the seed uses INSERT … ON CONFLICT DO NOTHING).

DELETE FROM feature_flags WHERE flag_key = 'fund_team';
