-- Down migration: 110_feature_flag_agent_team_mode
-- Removes the `agent_team_mode` feature flag.
--
-- Safe to run repeatedly. After this the SPA falls back to the
-- "unknown flag → defaults to caller-supplied value" path in
-- useFeatureFlag, which the gate component invokes with
-- defaultValue=false → ordinary users keep being redirected to
-- /masters. If you also want to expose /companies again you must
-- additionally remove the SPA-side AgentTeamGate wrapper
-- introduced in the same change.

DELETE FROM feature_flags WHERE flag_key = 'agent_team_mode';
