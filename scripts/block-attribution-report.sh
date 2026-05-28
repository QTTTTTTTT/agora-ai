#!/usr/bin/env bash
# =============================================================================
# scripts/block-attribution-report.sh — per-block citation rates
# =============================================================================
#
# G1 #2c. Reads the `block_contributions` JSONB column from
# investment_plans and surfaces:
#
#   - how often each signal block was PRESENT in the DecisionInput
#   - how often the PM CITED each block in its Reasoning
#   - the CITATION RATE = cited / present (the "is the PM
#     actually using what we feed it?" metric)
#
# The script complements `smoke-decision.sh` — that one tells you
# whether the LATEST decision had every block; this one tells you
# whether the PM USED them over a window.
#
# Usage:
#   ./scripts/block-attribution-report.sh                       # last 7 days
#   ./scripts/block-attribution-report.sh --days 30             # last 30d
#   ./scripts/block-attribution-report.sh --days 14 --fund X    # one fund
#   ./scripts/block-attribution-report.sh --json | jq .         # machine-r
#
# Exit codes:
#   0 = success (table rendered)
#   1 = no plans with attribution found in window (writer not deployed
#       yet, or fund_id filter eliminated everything)
#   2 = postgres container not reachable
#
# Companion docs: docs/MONDAY_TRIAGE_PLAYBOOK.md section 7.
# =============================================================================
set -euo pipefail

DAYS=7
FUND=""
JSON_MODE=0
PG_CONTAINER="${PG_CONTAINER:-fundai-postgres}"
PG_USER="${POSTGRES_USER:-fundai}"
PG_DB="${POSTGRES_DB:-fundai}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --days) DAYS="$2"; shift 2 ;;
    --fund) FUND="$2"; shift 2 ;;
    --json) JSON_MODE=1; shift ;;
    --help|-h)
      sed -n '2,30p' "$0"
      exit 0
      ;;
    *) echo "unknown flag $1"; exit 2 ;;
  esac
done

# ----------------------------------------------------------------------------
# Colour helpers (ANSI; auto-disabled when not a TTY or in --json mode).
# ----------------------------------------------------------------------------
if [[ $JSON_MODE -eq 0 && -t 1 ]]; then
  C_RED=$'\033[31m'; C_YEL=$'\033[33m'; C_GRN=$'\033[32m'
  C_DIM=$'\033[2m'; C_RST=$'\033[0m'
else
  C_RED=""; C_YEL=""; C_GRN=""; C_DIM=""; C_RST=""
fi

# Probe the container before we shell into psql.
if ! docker ps --format '{{.Names}}' | grep -q "^${PG_CONTAINER}$"; then
  echo "${C_RED}error:${C_RST} container ${PG_CONTAINER} not running — start the stack first" >&2
  exit 2
fi

# ----------------------------------------------------------------------------
# Build the WHERE clause. We always filter on created_at >= now - $DAYS d
# AND block_contributions <> '{}' so legacy rows (writer not deployed yet)
# don't dilute the citation rate.
# ----------------------------------------------------------------------------
where_clauses="created_at >= NOW() - INTERVAL '${DAYS} days' AND block_contributions <> '{}'"
if [[ -n "$FUND" ]]; then
  where_clauses="${where_clauses} AND fund_id = '${FUND}'"
fi

# Total plans in the window (denominator for "rate" columns).
total_plans=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -At -c "
  SELECT COUNT(*) FROM investment_plans WHERE ${where_clauses};
" 2>/dev/null | tr -d '[:space:]')

if [[ -z "$total_plans" || "$total_plans" == "0" ]]; then
  echo "${C_YEL}no plans with attribution in last ${DAYS}d (fund=${FUND:-all}).${C_RST}" >&2
  echo "${C_DIM}hint: did you run 040_plan_block_contributions and deploy the writer?${C_RST}" >&2
  exit 1
fi

# Pull a row per block-name with present + cited counts.
# jsonb_array_elements_text lets us count occurrences of each block
# name across both the 'present' and 'cited' arrays.
rows=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -At -F $'\t' -c "
  WITH plans AS (
    SELECT id, block_contributions
      FROM investment_plans
     WHERE ${where_clauses}
  ),
  present_blocks AS (
    SELECT id, jsonb_array_elements_text(block_contributions->'present') AS block
      FROM plans
  ),
  cited_blocks AS (
    SELECT id, jsonb_array_elements_text(block_contributions->'cited') AS block
      FROM plans
  )
  SELECT
    COALESCE(p.block, c.block) AS block,
    COUNT(DISTINCT p.id)        AS present_count,
    COUNT(DISTINCT c.id)        AS cited_count
  FROM present_blocks p
  FULL OUTER JOIN cited_blocks c
    ON p.id = c.id AND p.block = c.block
  GROUP BY COALESCE(p.block, c.block)
  ORDER BY cited_count DESC, present_count DESC, block;
" 2>/dev/null)

# ----------------------------------------------------------------------------
# Render the report. JSON mode dumps a structured payload; default mode
# is a colour-coded table.
# ----------------------------------------------------------------------------
if [[ $JSON_MODE -eq 1 ]]; then
  printf '{"days":%d,"fund":"%s","total_plans":%d,"blocks":[' \
    "$DAYS" "$FUND" "$total_plans"
  first=1
  while IFS=$'\t' read -r block present cited; do
    [[ -z "$block" ]] && continue
    if [[ $first -eq 1 ]]; then first=0; else printf ','; fi
    rate="null"
    if [[ "$present" -gt 0 ]]; then
      rate=$(awk -v c="$cited" -v p="$present" 'BEGIN{printf "%.3f", c/p}')
    fi
    printf '{"block":"%s","present":%s,"cited":%s,"present_rate":%s,"citation_rate":%s}' \
      "$block" "$present" "$cited" \
      "$(awk -v c="$present" -v t="$total_plans" 'BEGIN{printf "%.3f", c/t}')" \
      "$rate"
  done <<< "$rows"
  printf ']}\n'
  exit 0
fi

# Human-readable table.
echo
echo "${C_DIM}per-block attribution report — last ${DAYS}d, fund=${FUND:-ALL}, n=${total_plans} plans${C_RST}"
printf '%-22s %10s %10s %12s %12s\n' "BLOCK" "PRESENT" "CITED" "PRES.RATE" "CITE.RATE"
printf '%-22s %10s %10s %12s %12s\n' "----------------------" "-------" "-----" "---------" "---------"
while IFS=$'\t' read -r block present cited; do
  [[ -z "$block" ]] && continue
  pres_rate=$(awk -v c="$present" -v t="$total_plans" 'BEGIN{printf "%.0f%%", 100*c/t}')
  cite_rate="-"
  if [[ "$present" -gt 0 ]]; then
    cite_rate=$(awk -v c="$cited" -v p="$present" 'BEGIN{printf "%.0f%%", 100*c/p}')
  fi
  # Colour the cite_rate: red when block is fed but rarely cited
  # (PM ignoring the signal), green when cited > present (cited but
  # absent — PM hallucinating it, also a problem), yellow otherwise.
  colour="$C_DIM"
  if [[ "$present" -gt 0 && "$cited" -lt $((present / 5)) ]]; then
    colour="$C_RED" # cited < 20% of present → PM ignores the block
  elif [[ "$cited" -gt "$present" ]]; then
    colour="$C_YEL" # cited > present → PM cites blocks the wiring didn't carry
  elif [[ "$present" -gt 0 ]]; then
    colour="$C_GRN" # healthy
  fi
  printf "${colour}%-22s %10s %10s %12s %12s${C_RST}\n" \
    "$block" "$present" "$cited" "$pres_rate" "$cite_rate"
done <<< "$rows"
echo
echo "${C_DIM}colour key: ${C_GRN}green${C_DIM}=healthy, ${C_RED}red${C_DIM}=block fed but rarely cited (<20%), ${C_YEL}yellow${C_DIM}=cited > present (prompt drift)${C_RST}"
