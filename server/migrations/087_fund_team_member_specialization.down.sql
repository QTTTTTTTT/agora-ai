-- Down migration for 087. Drops the specialization table; consumers
-- automatically fall back to the legacy focus-string heuristic.
DROP TABLE IF EXISTS fund_team_member_specialization;
