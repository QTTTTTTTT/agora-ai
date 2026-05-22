-- 039_attribution_memory_layer.sql
--
-- Phase 3A-5: register the "attribution" memory layer.
--
-- The attribution.Service (server/internal/attribution) persists
-- the lessons it derives from the lot ledger into the existing
-- `memories` table with layer='attribution'. Before this
-- migration, that INSERT was rejected by the memories_layer_check
-- CHECK constraint (which only allowed long_term / daily / dreams
-- / agent / analysis) — meaning the daily review hook AND the
-- operator-driven /strategy-attribution/refresh endpoint both
-- silently failed to write any rows, and the dashboard's "agent
-- learning records" rail stayed empty regardless of how much
-- closed-lot data accumulated.
--
-- The fix is mechanical: drop the existing CHECK and re-create it
-- with 'attribution' added to the allow-list. We mirror
-- migration 008 (which originally added 'analysis') so the upgrade
-- path stays consistent across deployments.
--
-- No .down.sql is shipped for this migration. CHECK-constraint
-- expansions are strictly additive (every legacy row continues to
-- satisfy the new predicate), so there is nothing schematically to
-- roll back. A naive down that narrows the constraint back would
-- additionally need to delete every attribution memory row first
-- (otherwise the re-ADD fails) — which is destructive enough that
-- we want it to be an explicit ops step, not an auto-applied file
-- that the migration runner might re-execute on a redeploy.

ALTER TABLE memories DROP CONSTRAINT IF EXISTS memories_layer_check;

ALTER TABLE memories
    ADD CONSTRAINT memories_layer_check
    CHECK (layer IN ('long_term', 'daily', 'dreams', 'agent', 'analysis', 'attribution'));
