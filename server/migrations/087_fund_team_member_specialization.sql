-- Migration 087: fund_team_member_specialization
--
-- Why this exists.
-- ----------------
-- `fund_team_members.focus` is a coarse-grained category column
-- (CHECK IN ('stock', 'fundamental', 'macro')). It was never a
-- ticker list. But the agent self-learning prompt builder
-- (server/cmd/server/agent_self_learning_prompts.go) tries to
-- pull ticker-shaped tokens out of `focus` so a "Wei Liu the
-- semiconductor researcher" only journals about their own
-- coverage instead of the fund's whole book. With focus being
-- one of three category strings, that extraction silently
-- always falls back to the global view — the isolation logic
-- exists but never fires in production.
--
-- This migration introduces the real structured backing field
-- the prompt builder needed all along: a TEXT[] of instruments
-- (tickers) the team member covers, plus matching THEME and
-- MARKET arrays for finer slicing later. The prompt builder
-- will read from here when a row exists for the member, and
-- fall back to the legacy focus-string heuristic when it
-- doesn't — so the table is OPT-IN. Funds that don't configure
-- specialization keep behaving exactly as they do today.
--
-- Schema choices.
-- ---------------
-- * PRIMARY KEY = member_id, not a synthetic UUID. One row per
--   team member, and we want UPSERT semantics from the API
--   handler — no orphan duplicates.
-- * ON DELETE CASCADE so removing a team member cleans up.
-- * NOT NULL TEXT[] DEFAULT '{}'. Empty arrays are valid and
--   semantically mean "no specialization set"; NULL would force
--   every consumer to write a tri-state branch.
-- * GIN index on `instruments` because the only query pattern
--   we expect is "does this member cover symbol X?" — that's
--   what the prompt builder will run when filtering positions /
--   actions per researcher.
-- * No index on themes / markets yet: those are surface for
--   future UI grouping ("show me all macro researchers"),
--   present low-cardinality, can be added when the query
--   pattern actually emerges.

CREATE TABLE fund_team_member_specialization (
    member_id   UUID PRIMARY KEY REFERENCES fund_team_members(id) ON DELETE CASCADE,
    instruments TEXT[] NOT NULL DEFAULT '{}',
    themes      TEXT[] NOT NULL DEFAULT '{}',
    markets     TEXT[] NOT NULL DEFAULT '{}',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

COMMENT ON TABLE fund_team_member_specialization IS
    'Structured per-researcher coverage. Replaces ad-hoc parsing of '
    'fund_team_members.focus for self_learning isolation. Optional: '
    'absence of a row means "no specialization set, use focus string '
    'heuristic". Each array is normalized to lower-case at write time '
    'so the prompt builder can match against position symbols without '
    'rewriting the comparison.';

CREATE INDEX idx_fund_team_member_specialization_instruments
    ON fund_team_member_specialization USING GIN (instruments);
