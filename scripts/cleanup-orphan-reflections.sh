#!/usr/bin/env bash
# scripts/cleanup-orphan-reflections.sh
#
# One-shot cleanup for the agent-learning regression where the
# reflection distiller emitted sub-25-rune "platitude" lessons (e.g.
# "To maximize the utility of research") that then propagated into
# every agent's skill_config as a "needs approval" proposed skill.
# The runtime fix is in server/internal/memory/reflexion.go
# (isLowQualityReflection) plus daily_learning_test.go; this script
# scrubs the rows that the old code wrote BEFORE the gate landed.
#
# Idempotent: re-running after a clean DB is a no-op. Two-phase
# transaction so a partial failure rolls back cleanly.
#
# Detection criteria — must mirror isLowQualityReflection in
# server/internal/memory/reflexion.go exactly. The Go gate has four
# stages; the SQL predicate covers all four:
#
#   1. memory.layer = 'long_term' AND memory.title LIKE 'reflection:%'
#   2. char_length(trim(content)) < 25   (length floor)
#   3. content ~* '^...(to maximize|为了实现|...)'   (platitude blacklist)
#   4. NEW (2026-05-28): trim(content) does not end in a sentence
#      terminator   (catches `finish_reason=length` truncations)
#   5. NEW (2026-05-28): trim(content) matches a known truncated
#      list-header / subordinate-clause pattern, e.g. "Lessons:*
#      Researchers must" or "When encountering zero daily returns
#      or missing data" — both observed in production for the OCS
#      Selection fund on 2026-05-26.
#
# Keep this predicate in lockstep with isLowQualityReflection +
# reflectionTruncatedPatterns. The Go-side tests in
# internal/memory/memory_test.go (TestReflect_DropsLowQualityLessons)
# enumerate every shape this script must catch.
#
# Usage:
#   scripts/cleanup-orphan-reflections.sh                  # dry-run preview
#   scripts/cleanup-orphan-reflections.sh --apply          # commit changes
#
# Designed to be safe to commit and re-run as long as the criteria
# above stay in sync with the Go-side gate.

set -euo pipefail

CONTAINER=${CONTAINER:-fundai-postgres}
DB_USER=${POSTGRES_USER:-fundai}
DB_NAME=${POSTGRES_DB:-fundai}

if ! command -v docker >/dev/null 2>&1; then
  echo "docker not on PATH" >&2
  exit 1
fi

APPLY=0
if [[ "${1-}" == "--apply" ]]; then
  APPLY=1
fi

ORPHAN_PREDICATE=$(cat <<'SQL'
SELECT id::text
FROM memories
WHERE layer = 'long_term'
  AND title LIKE 'reflection:%'
  AND (
    -- (1) length floor: too short to be a usable lesson
    char_length(trim(content)) < 25

    -- (2) platitude blacklist: motivational fluff with no operational content
    OR trim(content) ~* '^(to\s+maximize|to\s+improve|to\s+ensure|ensure\s+that|always\s+focus|always\s+remember|it\s+is\s+important|the\s+key\s+is|in\s+order\s+to|为了\s*(让|实现|最大|提升|提高|确保)|要确保|要保证|要持续|应当持续|应当确保)'

    -- (3) missing terminal punctuation: every polished lesson must end
    --     in . ! ? 。 ！ ？ (optionally followed by closing quote);
    --     missing punctuation is almost always a finish_reason=length cut
    OR right(trim(content), 1) !~ '[.!?。！？")』）]'

    -- (4a) list-header + half sentence: "Lessons:* Researchers must",
    --      "Action items: Reduce", "Insights:" + body without terminator
    OR trim(content) ~* '^(lessons?|insights?|adjustments?|hits?|misses?|recommendations?|key\s+takeaways?|takeaways?|action\s+items?|todo|notes?)\s*:\s*\*?\s*[^.!?。！？]{0,80}$'

    -- (4b) leading subordinate clause with no main clause and no period:
    --      "When encountering zero daily returns or missing data"
    OR trim(content) ~* '^(when|if|while|whenever|once|after|before|since|because|although|though|unless)\s+[^.!?。！？]{8,}[^.!?。！？[:space:]]$'

    -- (4c) bare markdown header alone, no body
    OR trim(content) ~* '^(lessons?|insights?|adjustments?|hits?|misses?|recommendations?|key\s+takeaways?|takeaways?|action\s+items?|notes?)\s*:\s*$'
  )
SQL
)

echo "==> Identifying orphan reflection memories"
# macOS ships bash 3.2 which has no `mapfile`; the portable
# substitute is to collect into a tmpfile and read line-by-line.
tmp=$(mktemp)
trap 'rm -f "$tmp"' EXIT
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -c "$ORPHAN_PREDICATE" > "$tmp"

orphan_count=0
in_list=""
strip_source_clause=""
while IFS= read -r id; do
  [[ -z "$id" ]] && continue
  printf '      - %s\n' "$id"
  if [[ -n "$in_list" ]]; then in_list+=", "; fi
  in_list+="'${id}'"
  if [[ -n "$strip_source_clause" ]]; then strip_source_clause+=", "; fi
  strip_source_clause+="'reflection:${id}'"
  orphan_count=$((orphan_count + 1))
done < "$tmp"

if [[ $orphan_count -eq 0 ]]; then
  echo "    no orphan reflections found; nothing to do"
  exit 0
fi

printf '    found %d orphan reflection memories above.\n' "$orphan_count"

if [[ $APPLY -eq 0 ]]; then
  echo ""
  echo "==> DRY-RUN — would execute:"
  echo "    DELETE FROM memories WHERE id IN ($in_list);"
  echo "    UPDATE agents SET skill_config = jsonb_set(...) WHERE skill_config -> 'skills' ?| (...)"
  echo ""
  echo "    Re-run with --apply to commit."
  exit 0
fi

echo ""
echo "==> Applying cleanup in a single transaction"
docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" <<SQL
BEGIN;

-- 1. Strip the dangling skill entries from every agent. We use
--    jsonb_agg over a filtered jsonb_array_elements to rebuild the
--    array without the orphan rows, then jsonb_set the result back.
UPDATE agents
SET skill_config = jsonb_set(
  skill_config,
  '{skills}',
  COALESCE(
    (
      SELECT jsonb_agg(s)
      FROM jsonb_array_elements(skill_config -> 'skills') AS s
      WHERE s ->> 'source' NOT IN ($strip_source_clause)
    ),
    '[]'::jsonb
  )
)
WHERE skill_config -> 'skills' IS NOT NULL
  AND EXISTS (
    SELECT 1 FROM jsonb_array_elements(skill_config -> 'skills') AS s
    WHERE s ->> 'source' IN ($strip_source_clause)
  );

-- 2. Delete the low-quality reflection memories themselves so they
--    don't get re-promoted on the next propose pass.
DELETE FROM memories WHERE id IN ($in_list);

COMMIT;
SQL

echo "==> Cleanup applied; verifying"
remaining=$(docker exec -i "$CONTAINER" psql -U "$DB_USER" -d "$DB_NAME" -At -c "$ORPHAN_PREDICATE" | wc -l | tr -d ' ')
if [[ "$remaining" != "0" ]]; then
  echo "    WARNING: $remaining orphan(s) still present" >&2
  exit 1
fi
echo "    OK — zero orphan reflection memories remain"
