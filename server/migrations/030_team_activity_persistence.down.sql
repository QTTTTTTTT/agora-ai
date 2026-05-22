-- Down migration for 030_team_activity_persistence.sql. Used by the
-- integration test harness and by operators rolling back a bad deploy.
DROP INDEX IF EXISTS idx_waevents_fund_seq_uniq;
DROP INDEX IF EXISTS idx_waevents_fund_event_at;
DROP TABLE IF EXISTS workflow_activity_events;
