#!/usr/bin/env bash
# =============================================================================
# scripts/ha-failover-smoke.sh — local HA failover smoke test
# =============================================================================
#
# What this verifies (and what it does NOT)
# -----------------------------------------
# This is a **smoke** test, not a real chaos / HA validation. We
# measure two scenarios that are cheap, deterministic, and that
# real production incidents have actually exercised:
#
#   Scenario A  "app pod crash"
#     `docker kill fundai-app`. Expect docker's `restart:
#     unless-stopped` policy to bring the container back, and
#     /api/health to return 200 again. We measure wall-clock
#     recovery time and flag if it's > 60 s.
#
#   Scenario B  "DB outage"
#     `docker compose stop postgres`, wait 5 s with the app pinned
#     into a degraded state, then start postgres again. Expect the
#     app's health to flip from 503/error → 200 once Postgres is
#     reachable, with no manual app restart needed (the connection
#     pool retries via the standard sql.Open behaviour).
#
# What we explicitly do NOT cover here:
#
#   - Multi-replica failover (single-replica compose stack).
#   - Network partitions / split brain.
#   - Cross-region failover, rolling deploy with zero downtime.
#   - LLM provider failover (covered by the LLM router's own tests).
#
# Those belong in the full chaos pipeline ("M2 — HA failover" in the
# production-readiness review). This smoke is the floor: if THIS
# fails, the fancier tests don't matter yet.
#
# Output
# ------
# Human-readable timeline on stdout. Exit 0 on both scenarios pass,
# 1 on either fails. CI-friendly. Doesn't write files (so it doesn't
# pollute git status during local runs).
#
# Inputs
# ------
#   API_BASE_URL            default http://localhost:8080
#   APP_CONTAINER           default fundai-app
#   POSTGRES_CONTAINER      default fundai-postgres
#   RECOVERY_BUDGET_S       default 60   (per scenario)
#   CHECK_INTERVAL_S        default 1
# =============================================================================

set -uo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:8080}"
APP_CONTAINER="${APP_CONTAINER:-fundai-app}"
POSTGRES_CONTAINER="${POSTGRES_CONTAINER:-fundai-postgres}"
RECOVERY_BUDGET_S="${RECOVERY_BUDGET_S:-60}"
CHECK_INTERVAL_S="${CHECK_INTERVAL_S:-1}"

color_red()   { printf '\033[31m%s\033[0m' "$*"; }
color_green() { printf '\033[32m%s\033[0m' "$*"; }
color_dim()   { printf '\033[2m%s\033[0m' "$*"; }

log()  { printf '[%s] %s\n' "$(date +%H:%M:%S)" "$*"; }
fail() { printf '[%s] %s %s\n' "$(date +%H:%M:%S)" "$(color_red FAIL)"  "$*" >&2; }
pass() { printf '[%s] %s %s\n' "$(date +%H:%M:%S)" "$(color_green PASS)" "$*"; }

require_command() {
  local cmd="$1"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    fail "missing dependency: $cmd"
    exit 2
  fi
}

require_command docker
require_command curl

# ---- Pre-flight ------------------------------------------------------------

log "preflight: checking that the stack is up …"
if ! docker ps --format '{{.Names}}' | grep -qx "$APP_CONTAINER"; then
  fail "container '$APP_CONTAINER' not running. Run \`scripts/start.sh\` first."
  exit 2
fi
if ! docker ps --format '{{.Names}}' | grep -qx "$POSTGRES_CONTAINER"; then
  fail "container '$POSTGRES_CONTAINER' not running. Run \`scripts/start.sh\` first."
  exit 2
fi
if ! curl -fsS "$API_BASE_URL/api/health" >/dev/null 2>&1; then
  fail "$API_BASE_URL/api/health did not respond 200 before chaos started. Aborting."
  exit 2
fi
log "$(color_green ok) — stack is up and /api/health returns 200"

# Returns 0 if /api/health is 200, non-zero otherwise.
health_ok() { curl -fsS -o /dev/null --max-time 3 "$API_BASE_URL/api/health"; }

# Waits up to budget seconds for `health_ok` to return 0. Echoes elapsed seconds.
wait_for_recovery() {
  local budget="$1"
  local started
  started=$(date +%s)
  local now
  while true; do
    now=$(date +%s)
    local elapsed=$(( now - started ))
    if (( elapsed > budget )); then
      echo "$elapsed"
      return 1
    fi
    if health_ok; then
      echo "$elapsed"
      return 0
    fi
    sleep "$CHECK_INTERVAL_S"
  done
}

scenario_a_passed=0
scenario_b_passed=0

# ============================================================================
# Scenario A — app crash
# ============================================================================

log ""
log "----- Scenario A: kill $APP_CONTAINER, expect restart -----"
old_id=$(docker inspect -f '{{.Id}}' "$APP_CONTAINER" 2>/dev/null || true)
log "old container id: ${old_id:0:12}"

if ! docker kill "$APP_CONTAINER" >/dev/null; then
  fail "docker kill $APP_CONTAINER returned non-zero"
else
  log "killed; waiting for /api/health to recover (budget ${RECOVERY_BUDGET_S}s) …"
  if elapsed=$(wait_for_recovery "$RECOVERY_BUDGET_S"); then
    new_id=$(docker inspect -f '{{.Id}}' "$APP_CONTAINER" 2>/dev/null || true)
    if [[ "$new_id" == "$old_id" ]]; then
      # Same container id means docker just restarted it (compose
      # default behaviour). That's fine — the contract is "service
      # comes back", not "fresh container".
      log "$(color_dim 'note: same container id — docker restarted in-place rather than recreating')"
    fi
    pass "Scenario A — recovered after ${elapsed}s"
    scenario_a_passed=1
  else
    fail "Scenario A — /api/health did not return 200 within ${RECOVERY_BUDGET_S}s"
    log "  current docker ps:"
    docker ps --filter "name=$APP_CONTAINER" --format '  {{.Names}} {{.Status}}'
    log "  last 20 log lines:"
    docker logs --tail 20 "$APP_CONTAINER" 2>&1 | sed 's/^/    /'
  fi
fi

# Allow the app to fully settle before stressing the next dependency.
sleep 3

# ============================================================================
# Scenario B — postgres outage
# ============================================================================

log ""
log "----- Scenario B: stop $POSTGRES_CONTAINER for 5s, then start, expect app reconnect -----"
if ! docker stop "$POSTGRES_CONTAINER" >/dev/null; then
  fail "docker stop $POSTGRES_CONTAINER returned non-zero"
else
  log "postgres stopped; sleeping 5s with DB down so the app's health-check observes it"
  sleep 5

  # During this window, /api/health may be returning 503 / 500 / 200
  # depending on whether the handler actually probes the DB. We don't
  # assert here — we only need to confirm the app DOES come back once
  # postgres returns. Some health endpoints are pure-process pings
  # and stay 200 throughout, which is fine.

  log "starting postgres back up"
  if ! docker start "$POSTGRES_CONTAINER" >/dev/null; then
    fail "docker start $POSTGRES_CONTAINER returned non-zero"
  else
    log "waiting for /api/health after DB came back (budget ${RECOVERY_BUDGET_S}s) …"
    if elapsed=$(wait_for_recovery "$RECOVERY_BUDGET_S"); then
      pass "Scenario B — recovered after ${elapsed}s"
      scenario_b_passed=1
    else
      fail "Scenario B — /api/health did not return 200 within ${RECOVERY_BUDGET_S}s after DB recovery"
      log "  app logs (tail 30):"
      docker logs --tail 30 "$APP_CONTAINER" 2>&1 | sed 's/^/    /'
      log "  postgres logs (tail 10):"
      docker logs --tail 10 "$POSTGRES_CONTAINER" 2>&1 | sed 's/^/    /'
    fi
  fi
fi

# ============================================================================
# Summary
# ============================================================================

log ""
log "===== Summary ====="
[[ "$scenario_a_passed" == 1 ]] && pass "Scenario A — app crash recovery"   || fail "Scenario A — app crash recovery"
[[ "$scenario_b_passed" == 1 ]] && pass "Scenario B — postgres outage"      || fail "Scenario B — postgres outage"

if [[ "$scenario_a_passed" == 1 && "$scenario_b_passed" == 1 ]]; then
  log "$(color_green 'all scenarios passed')"
  exit 0
fi
log "$(color_red 'one or more scenarios failed')"
exit 1
