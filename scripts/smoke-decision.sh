#!/usr/bin/env bash
# =============================================================================
# scripts/smoke-decision.sh — 24-block PM decision-input smoke check
# =============================================================================
#
# What this does. After `docker compose up -d --build` finishes the
# Sprint A → E PM pipeline has 21 prompt-facing signal blocks. This
# script answers the Monday-morning question "did the last PM
# decision get every block I expected, and if not which ones are
# missing?" without forcing you to grep slog by hand.
#
# Pipeline:
#   1. Verify container health (postgres + app critical;
#      web-search-mcp + akshare-mcp + china-stock-mcp optional).
#   2. Pull the latest decision_input_fingerprint slog line from
#      `docker logs fundai-app`, parse the present/absent flags.
#   3. Pull the latest PM plan's Reasoning blob from postgres to
#      cross-check whether the PM actually CITED the new blocks.
#   4. Render a colour-coded 21-row table; exit non-zero when a
#      critical block (instrumentHints / quantSnapshots) is
#      missing — exactly the failure-mode Sprint C #3 was
#      designed to catch.
#
# Usage:
#   ./scripts/smoke-decision.sh                  # last decision
#   ./scripts/smoke-decision.sh fund-id-xxx      # specific fund
#   ./scripts/smoke-decision.sh --json           # machine-readable
#   ./scripts/smoke-decision.sh --tail 2000      # broader log window
#
# Exit codes:
#   0   = healthy, every critical block present
#   1   = at least one critical block missing
#   2   = no decision found in logs (PM hasn't run yet, or
#         container is not producing slog lines)
#   3   = container / docker not available
#
# Bash 3.2 compatible (the macOS default ships /bin/bash 3.2).
# No associative arrays — block-name → attr lookups go through
# case statements so we never depend on `declare -A`.
# =============================================================================

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# --- styling ------------------------------------------------------------------
if [[ -t 1 ]]; then
  RED='\033[0;31m'
  GREEN='\033[0;32m'
  YELLOW='\033[1;33m'
  CYAN='\033[0;36m'
  DIM='\033[2m'
  BOLD='\033[1m'
  NC='\033[0m'
else
  RED=''; GREEN=''; YELLOW=''; CYAN=''; DIM=''; BOLD=''; NC=''
fi

ok()    { printf "%b\n" "${GREEN}✓${NC} $*"; }
warn()  { printf "%b\n" "${YELLOW}!${NC} $*"; }
bad()   { printf "%b\n" "${RED}✗${NC} $*"; }
info()  { printf "%b\n" "${CYAN}→${NC} $*"; }
sep()   { printf "%b\n" "${DIM}── $* ──${NC}"; }

# --- args ---------------------------------------------------------------------
FUND_FILTER=""
JSON_MODE=0
# Default 5000: PM ticks are infrequent and the app produces a lot
# of OHLC / quote chatter between them. 500 is rarely enough on a
# live container; 5000 is a few hours of headroom with negligible
# parse cost. Override with --tail for older decisions.
TAIL_LINES=5000
APP_CONTAINER="${APP_CONTAINER:-fundai-app}"
PG_CONTAINER="${PG_CONTAINER:-fundai-postgres}"
PG_USER="${POSTGRES_USER:-fundai}"
PG_DB="${POSTGRES_DB:-fundai}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --json) JSON_MODE=1; shift ;;
    --tail) TAIL_LINES="$2"; shift 2 ;;
    --help|-h)
      sed -n '3,30p' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) FUND_FILTER="$1"; shift ;;
  esac
done

# --- prereqs ------------------------------------------------------------------
if ! command -v docker >/dev/null 2>&1; then
  bad "docker not installed; cannot inspect containers"
  exit 3
fi

# --- container health ---------------------------------------------------------
[[ $JSON_MODE -eq 0 ]] && sep "container health"

container_state() {
  # Prints one of: running / exited / missing
  local name="$1"
  local state
  state=$(docker inspect -f '{{.State.Status}}' "$name" 2>/dev/null || true)
  if [[ -z "$state" ]]; then
    echo "missing"
  else
    echo "$state"
  fi
}

CRITICAL_CONTAINERS="$PG_CONTAINER $APP_CONTAINER"
OPTIONAL_CONTAINERS="fundai-web-search-mcp fundai-akshare-mcp fundai-china-stock-mcp"

critical_down=0
container_status_lines=""
for c in $CRITICAL_CONTAINERS; do
  s=$(container_state "$c")
  case "$s" in
    running)
      [[ $JSON_MODE -eq 0 ]] && ok "$c: running"
      ;;
    missing)
      [[ $JSON_MODE -eq 0 ]] && bad "$c: not created (run docker compose up first)"
      critical_down=1
      ;;
    *)
      [[ $JSON_MODE -eq 0 ]] && bad "$c: $s"
      critical_down=1
      ;;
  esac
  container_status_lines="${container_status_lines}${c}=${s} "
done
for c in $OPTIONAL_CONTAINERS; do
  s=$(container_state "$c")
  case "$s" in
    running)
      [[ $JSON_MODE -eq 0 ]] && ok "$c: running"
      ;;
    missing)
      [[ $JSON_MODE -eq 0 ]] && info "$c: not running (optional profile)"
      ;;
    *)
      [[ $JSON_MODE -eq 0 ]] && warn "$c: $s"
      ;;
  esac
done

if [[ $critical_down -eq 1 ]]; then
  exit 3
fi

# --- pull fingerprint from logs ----------------------------------------------
[[ $JSON_MODE -eq 0 ]] && sep "decision_input_fingerprint"

# 24 canonical blocks. Order matches decision.PresentBlocks /
# AbsentBlocks. Mapping helpers below convert block name → slog
# attr name; the CRITICAL set is the subset whose absence is a
# hard failure (the PM literally cannot do its job without them).
BLOCKS="roundtableStance bullCase bearCase quantCase symbolVerdicts \
fundamentalSummary sectorRotation newsSentiment sleeveScorecard \
lessonReplay instrumentHints quantSnapshots universeRanking \
qualityScores valueScores lowBetaScores pead \
cooldowns riskBudget newsCatalysts earningsCalendar \
exposure correlations pairSpreads"

# Case-statement lookups instead of associative arrays so the
# script runs on the bash 3.2 that ships with macOS.
present_attr_for() {
  case "$1" in
    roundtableStance)   echo "p_roundtable_stance" ;;
    bullCase)           echo "p_bull_case" ;;
    bearCase)           echo "p_bear_case" ;;
    quantCase)          echo "p_quant_case" ;;
    symbolVerdicts)     echo "p_symbol_verdicts" ;;
    fundamentalSummary) echo "p_fundamental_summary" ;;
    sectorRotation)     echo "p_sector_rotation" ;;
    newsSentiment)      echo "p_news_sentiment" ;;
    sleeveScorecard)    echo "p_sleeve_scorecard" ;;
    lessonReplay)       echo "p_lesson_replay" ;;
    instrumentHints)    echo "p_instrument_hints" ;;
    quantSnapshots)     echo "p_quant_snapshots" ;;
    universeRanking)    echo "p_universe_ranking" ;;
    qualityScores)      echo "p_quality_scores" ;;
    valueScores)        echo "p_value_scores" ;;
    lowBetaScores)      echo "p_low_beta_scores" ;;
    pead)               echo "p_pead" ;;
    cooldowns)          echo "p_cooldowns" ;;
    riskBudget)         echo "p_risk_budget" ;;
    newsCatalysts)      echo "p_news_catalysts" ;;
    earningsCalendar)   echo "p_earnings_calendar" ;;
    exposure)           echo "p_exposure" ;;
    correlations)       echo "p_correlations" ;;
    pairSpreads)        echo "p_pair_spreads" ;;
  esac
}

count_attr_for() {
  case "$1" in
    instrumentHints)  echo "count_instrument_hints" ;;
    quantSnapshots)   echo "count_quant_snapshots" ;;
    universeRanking)  echo "count_universe_ranking" ;;
    qualityScores)    echo "count_quality_scores" ;;
    valueScores)      echo "count_value_scores" ;;
    lowBetaScores)    echo "count_low_beta_scores" ;;
    pead)             echo "count_pead_signals" ;;
    cooldowns)        echo "count_cooldowns" ;;
    newsCatalysts)    echo "count_news_catalysts" ;;
    earningsCalendar) echo "count_earnings_calendar" ;;
    correlations)     echo "count_correlations_high" ;;
    pairSpreads)      echo "count_pair_spreads" ;;
    exposure)         echo "count_exposure_breaches" ;;
    *)                echo "" ;;
  esac
}

is_critical() {
  case "$1" in
    instrumentHints|quantSnapshots) return 0 ;;
    *)                              return 1 ;;
  esac
}

# Pull the latest fingerprint line. The fund filter is optional
# (default = last decision across the whole pipeline).
fingerprint_line=""
# The fund-id field is emitted as `fund_id=<uuid>` in text-mode
# slog and as `"fund_id":"<uuid>"` in JSON-mode slog (production
# default). Match both so the smoke runs cleanly regardless of
# the active log handler.
if [[ -n "$FUND_FILTER" ]]; then
  fingerprint_line=$(docker logs --tail "$TAIL_LINES" "$APP_CONTAINER" 2>&1 | \
    grep -E "decision_input_fingerprint" | \
    grep -E "(fund_id=$FUND_FILTER|\"fund_id\":\"$FUND_FILTER\")" | \
    tail -n 1 || true)
else
  fingerprint_line=$(docker logs --tail "$TAIL_LINES" "$APP_CONTAINER" 2>&1 | \
    grep -E "decision_input_fingerprint" | \
    tail -n 1 || true)
fi

if [[ -z "$fingerprint_line" ]]; then
  if [[ $JSON_MODE -eq 1 ]]; then
    printf '{"status":"no_decision","tail_lines":%d,"fund_filter":"%s"}\n' \
      "$TAIL_LINES" "$FUND_FILTER"
  else
    bad "no decision_input_fingerprint found in last $TAIL_LINES log lines"
    info "    likely causes:"
    info "      • PM hasn't run yet (wait for next scheduled tick)"
    info "      • LOG_LEVEL too high (set to info or debug)"
    info "      • app container restarted recently (try --tail 2000)"
    info "      • the fund_id you queried hasn't run today"
  fi
  exit 2
fi

# Extract a single top-level value from the fingerprint JSON.
# Prefers jq when available (handles every edge case correctly);
# falls back to a regex that covers our flat-object shape
# (no nested objects, values are bool / int / string).
HAVE_JQ=0
if command -v jq >/dev/null 2>&1; then HAVE_JQ=1; fi

extract_attr() {
  local key="$1"
  if [[ $HAVE_JQ -eq 1 ]]; then
    echo "$fingerprint_line" | jq -r --arg k "$key" '.[$k] // empty'
    return
  fi
  # Portable regex fallback. The fingerprint payload is a flat
  # object so we just scan for `"key":` then read the next token,
  # stripping quotes if the value is a string. Returns empty when
  # the key is absent (mirrors `jq // empty`).
  echo "$fingerprint_line" | awk -v k="$key" '
    {
      pat = "\"" k "\":"
      i = index($0, pat)
      if (i == 0) { exit }
      rest = substr($0, i + length(pat))
      # value ends at the next comma at this depth or at the
      # closing brace; our payload is flat so this is safe.
      n = length(rest)
      v = ""
      depth = 0
      in_str = 0
      for (j = 1; j <= n; j++) {
        ch = substr(rest, j, 1)
        if (in_str) {
          if (ch == "\"") { in_str = 0 }
          v = v ch
          continue
        }
        if (ch == "\"") { in_str = 1; v = v ch; continue }
        if (ch == "," || ch == "}") { break }
        v = v ch
      }
      gsub(/^[ \t]+|[ \t]+$/, "", v)
      gsub(/^"|"$/, "", v)
      print v
    }
  '
}

fund_id=$(extract_attr fund_id)
trading_date=$(extract_attr trading_date)
pm_agent_id=$(extract_attr pm_agent_id)

# --- render table -------------------------------------------------------------
if [[ $JSON_MODE -eq 0 ]]; then
  sep "PM decision: ${fund_id:-?} @ ${trading_date:-?}"
  [[ -n "$pm_agent_id" ]] && info "pm_agent_id=$pm_agent_id"
  printf "\n    %-20s %-10s %-8s\n" "BLOCK" "PRESENT" "COUNT"
  printf "    %-20s %-10s %-8s\n"   "--------------------" "----------" "--------"
fi

present_count=0
absent_count=0
critical_missing=""
json_blocks=""

for block in $BLOCKS; do
  attr_present=$(present_attr_for "$block")
  attr_count=$(count_attr_for "$block")
  flag_val=$(extract_attr "$attr_present")
  count_val=""
  [[ -n "$attr_count" ]] && count_val=$(extract_attr "$attr_count")

  if [[ "$flag_val" == "true" ]]; then
    present_count=$((present_count + 1))
    if [[ $JSON_MODE -eq 0 ]]; then
      printf "  %b %-20s %-10s %-8s\n" "${GREEN}✓${NC}" "$block" "yes" "${count_val:-—}"
    fi
    if [[ -n "$json_blocks" ]]; then json_blocks="${json_blocks},"; fi
    json_blocks="${json_blocks}\"${block}\":{\"present\":true,\"count\":${count_val:-null}}"
  else
    absent_count=$((absent_count + 1))
    if is_critical "$block"; then
      critical_missing="${critical_missing}${block} "
      if [[ $JSON_MODE -eq 0 ]]; then
        printf "  %b %-20s ${RED}%-10s${NC} %-8s ${RED}CRITICAL${NC}\n" "${RED}✗${NC}" "$block" "no" "${count_val:-0}"
      fi
    else
      if [[ $JSON_MODE -eq 0 ]]; then
        printf "  %b %-20s ${DIM}%-10s${NC} %-8s\n" "${DIM}·${NC}" "$block" "no" "${count_val:-0}"
      fi
    fi
    if [[ -n "$json_blocks" ]]; then json_blocks="${json_blocks},"; fi
    json_blocks="${json_blocks}\"${block}\":{\"present\":false,\"count\":${count_val:-0}}"
  fi
done

# --- DB cross-check (does Reasoning cite new blocks?) ------------------------
[[ $JSON_MODE -eq 0 ]] && sep "PM Reasoning cross-check"

reasoning_cites=""
# Soft-pull: if the DB isn't reachable or the schema differs, we
# silently skip — the slog half is the authoritative signal.
#
# G1 #2: PRIMARY source of truth is investment_plans.block_contributions
# (a JSONB column) which the runtime writes via the attribution layer.
# It already covers both English and Chinese citation vocabulary, so
# we lift the `cited` array directly. The legacy keyword grep on
# `reasoning` is kept as a FALLBACK for older plans (or environments
# where the writer hasn't been deployed yet).
if [[ -n "$fund_id" ]]; then
  cited_csv=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -At -c "
    SELECT COALESCE(
      (SELECT string_agg(value, ' ')
         FROM jsonb_array_elements_text(block_contributions->'cited')),
      ''
    )
    FROM investment_plans
    WHERE fund_id = '$fund_id'
      AND block_contributions IS NOT NULL
      AND block_contributions <> '{}'::jsonb
    ORDER BY created_at DESC
    LIMIT 1;
  " 2>/dev/null || true)

  if [[ -n "$cited_csv" ]]; then
    reasoning_cites="$cited_csv"
    if [[ $JSON_MODE -eq 0 ]]; then
      ok "block_contributions.cited: ${reasoning_cites}"
    fi
  else
    # Fallback path: keyword grep on the legacy `reasoning` blob.
    reasoning_blob=$(docker exec "$PG_CONTAINER" psql -U "$PG_USER" -d "$PG_DB" -At -c "
      SELECT COALESCE(reasoning, '')
      FROM investment_plans
      WHERE fund_id = '$fund_id'
      ORDER BY created_at DESC
      LIMIT 1;
    " 2>/dev/null || true)
    if [[ -n "$reasoning_blob" ]]; then
      for keyword in tsmom qualityScores valueScores lowBetaScores pead earningsCalendar pairSpreads riskBudget cooldown newsCatalyst exposure correlation universeRanking; do
        if echo "$reasoning_blob" | grep -qiE "$keyword"; then
          reasoning_cites="${reasoning_cites}${keyword} "
        fi
      done
      if [[ $JSON_MODE -eq 0 ]]; then
        if [[ -z "$reasoning_cites" ]]; then
          warn "Plan.Reasoning cites NO new block keywords"
          info "    PM may be ignoring the new signal blocks — read the latest Reasoning"
          info "    blob in full to confirm; expected references include tsmom,"
          info "    qualityScores, valueScores, lowBetaScores, pead, earningsCalendar,"
          info "    pairSpreads, riskBudget, cooldowns."
        else
          ok "Plan.Reasoning cites: ${reasoning_cites}"
        fi
      fi
    else
      [[ $JSON_MODE -eq 0 ]] && info "no plans row for fund_id=$fund_id (or db query failed)"
    fi
  fi
fi

# --- summary ------------------------------------------------------------------
status="ok"
if [[ -n "$critical_missing" ]]; then status="critical_missing"; fi

# Render JSON pieces as proper arrays.
crit_json=""
for m in $critical_missing; do
  if [[ -n "$crit_json" ]]; then crit_json="${crit_json},"; fi
  crit_json="${crit_json}\"${m}\""
done
cites_json=""
for k in $reasoning_cites; do
  if [[ -n "$cites_json" ]]; then cites_json="${cites_json},"; fi
  cites_json="${cites_json}\"${k}\""
done

if [[ $JSON_MODE -eq 1 ]]; then
  printf '{"status":"%s","fund_id":"%s","trading_date":"%s","pm_agent_id":"%s","present":%d,"absent":%d,"critical_missing":[%s],"reasoning_cites":[%s],"blocks":{%s}}\n' \
    "$status" "$fund_id" "$trading_date" "$pm_agent_id" \
    "$present_count" "$absent_count" \
    "$crit_json" "$cites_json" "$json_blocks"
else
  sep "summary"
  printf "  %b present: %d / 24    %b absent: %d / 24\n" \
    "${GREEN}●${NC}" "$present_count" \
    "${YELLOW}●${NC}" "$absent_count"
  if [[ -n "$critical_missing" ]]; then
    bad "critical blocks missing: ${critical_missing}"
    info "    decision pipeline cannot function — check researcher / fundamental wiring"
    exit 1
  else
    ok "all critical blocks present"
  fi
fi

exit 0
