#!/usr/bin/env bash
# scripts/cleanup-duplicate-daily-memories.sh
#
# One-shot cleanup for the agent-learning regression where the
# workflow scheduler's intraday cadence (e.g. OCS-style 30-min tick)
# caused StepDailyReview to write a fresh "daily" / "agent" memory
# row on every tick — leaving 7-8 near-identical rows per fund per
# day in the agent learning UI.
#
# The runtime fix is in server/cmd/server/wiring_adapters.go
# (ConsolidateDaily now early-continues when a row already exists for
# the (fund, agent, layer, trading_date) tuple). This script scrubs
# the duplicates the OLD code wrote before the dedupe gate landed:
# we keep the EARLIEST created_at per (fund, agent, layer, trading_date)
# group and delete the rest.
#
# Usage:
#   scripts/cleanup-duplicate-daily-memories.sh           # dry-run preview
#   scripts/cleanup-duplicate-daily-memories.sh --apply   # commit
#
# Idempotent: re-running on a deduped DB is a no-op.

set -euo pipefail

CONTAINER=${CONTAINER:-fundai-postgres}
DB_USER=${POSTGRES_USER:-fundai}
DB_NAME=${POSTGRES_DB:-fundai}

APPLY=0
if [[ "${1-}" == "--apply" ]]; then
  APPLY=1
fi

# Inline CTE: ROW_NUMBER() per group, partitioned by all four dedupe
# keys (fund + agent + layer + trading_date), ordered by created_at
# ASC so rn=1 is the keeper.
DUPE_SELECT=$(cat <<'SQL'
WITH ranked AS (
  SELECT
    id,
    fund_id,
    agent_id,
    layer,
    trading_date,
    created_at,
    ROW_NUMBER() OVER (
      PARTITION BY fund_id, COALESCE(agent_id::text, '__fund__'), layer, trading_date
      ORDER BY created_at ASC
    ) AS rn
  FROM memories
  WHERE layer IN ('daily', 'agent')
    AND trading_date IS NOT NULL
)
SELECT id::text FROM ranked WHERE rn > 1
SQL
)

echo "==> Counting duplicate daily/agent memory rows"
dup_count=$(docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -c "SELECT COUNT(*) FROM ($DUPE_SELECT) AS t" 2>&1 | tail -1)
if [[ "$dup_count" == "0" ]]; then
  echo "    no duplicate rows found; nothing to do"
  exit 0
fi
echo "    found $dup_count duplicate row(s) to remove"

if [[ $APPLY -eq 0 ]]; then
  echo ""
  echo "==> DRY-RUN — would delete the $dup_count rows whose ROW_NUMBER() > 1"
  echo "    per (fund_id, agent_id, layer, trading_date) group, keeping"
  echo "    the earliest created_at per group."
  echo ""
  echo "    Re-run with --apply to commit."
  exit 0
fi

echo ""
echo "==> Applying cleanup in a single transaction"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" <<SQL
BEGIN;

DELETE FROM memories
WHERE id::text IN (
  $DUPE_SELECT
);

COMMIT;
SQL

remaining=$(docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -c "SELECT COUNT(*) FROM ($DUPE_SELECT) AS t")
if [[ "$remaining" != "0" ]]; then
  echo "    WARNING: $remaining duplicate(s) still present" >&2
  exit 1
fi
echo "==> OK — zero duplicate daily/agent memories remain"
