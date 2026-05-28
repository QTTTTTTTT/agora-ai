#!/usr/bin/env bash
# =============================================================================
# scripts/smoke-test.sh — three-surface release smoke gate
# =============================================================================
#
# What this does. The release playbook (docs/RELEASE_QA_PLAYBOOK.md §1)
# requires 7 user journeys to pass on each of (web, miniapp, Android).
# This script automates the API-side rehearsal of those journeys so
# operators can pre-flight a release window against a live server BEFORE
# they touch any UI.
#
# Coverage (API only — UI is manual per playbook):
#   1.  POST /api/auth/login           — sign in
#   2.  POST /api/auth/forgot-password — forgot password
#   3.  GET  /api/auth/session         — session restore
#   4.  GET  /api/companies            — fund list (used by Home + fund switch)
#   5.  GET  /api/funds/:id            — fund detail (used by all UIs)
#   6.  GET  /api/funds/:id/plans      — Decisions tab
#   7.  GET  /api/funds/:id/team       — Team tab
#   8.  GET  /api/funds/:id/memory     — Memory tab (agent)
#   9.  GET  /api/funds/:id/reflections — Memory tab (reflection)
#   10. GET  /api/funds/:id/portfolio  — Home detail
#   11. POST /api/devices/register     — Android push registration
#
# Inputs (env vars):
#   API_BASE_URL    default http://localhost:8080
#   SMOKE_EMAIL     required — login email
#   SMOKE_PASSWORD  required — login password
#   SMOKE_FUND_ID   optional — short-circuit fund discovery; if unset we
#                   pick the first fund returned by /api/companies
#
# Exit codes:
#   0  all journeys passed
#   1  prerequisite missing (env / curl / jq)
#   2  one or more journeys failed (table printed)
#
# Output:
#   - colour-coded checklist per journey
#   - JSON summary on stderr (`--json` for stdout only)
# =============================================================================

set -euo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:8080}"
JSON_ONLY=0
if [[ "${1:-}" == "--json" ]]; then
  JSON_ONLY=1
fi

# ---------- helpers ----------------------------------------------------------

red()    { printf '\033[31m%s\033[0m' "$1"; }
green()  { printf '\033[32m%s\033[0m' "$1"; }
yellow() { printf '\033[33m%s\033[0m' "$1"; }
bold()   { printf '\033[1m%s\033[0m' "$1"; }

log() {
  if [[ "$JSON_ONLY" -eq 0 ]]; then
    printf '%s\n' "$*" >&2
  fi
}

fail() {
  log "$(red "FATAL"): $*"
  exit 1
}

require_cmd() {
  command -v "$1" >/dev/null 2>&1 || fail "missing required tool: $1"
}

require_cmd curl
require_cmd jq

if [[ -z "${SMOKE_EMAIL:-}" || -z "${SMOKE_PASSWORD:-}" ]]; then
  fail "set SMOKE_EMAIL + SMOKE_PASSWORD env vars (see docs/RELEASE_QA_PLAYBOOK.md §1)"
fi

# ---------- runner -----------------------------------------------------------

declare -a JOURNEYS=()
declare -a STATUSES=()
declare -a DETAILS=()

record() {
  local name="$1" status="$2" detail="${3:-}"
  JOURNEYS+=("$name")
  STATUSES+=("$status")
  DETAILS+=("$detail")
  if [[ "$JSON_ONLY" -eq 0 ]]; then
    local mark
    case "$status" in
      pass) mark="$(green '  ✓ ')" ;;
      fail) mark="$(red   '  ✗ ')" ;;
      skip) mark="$(yellow ' -  ')" ;;
    esac
    printf '%b %-30s %s\n' "$mark" "$name" "$detail" >&2
  fi
}

# HTTP helper: returns body to stdout, status to JSON-ish trailer.
# Usage: code=$(call METHOD PATH BODY HEADERS_FILE > body_file)
declare TOKEN=""
call() {
  local method="$1" path="$2" body="$3"
  local hdrs=()
  hdrs+=(-H "Content-Type: application/json")
  hdrs+=(-H "Accept: application/json")
  if [[ -n "$TOKEN" ]]; then
    hdrs+=(-H "Authorization: Bearer $TOKEN")
  fi
  local code
  if [[ "$method" == "GET" ]]; then
    code=$(curl -sS -o /tmp/smoke_body -w '%{http_code}' "${hdrs[@]}" "$API_BASE_URL$path" || echo 0)
  else
    code=$(curl -sS -o /tmp/smoke_body -w '%{http_code}' -X "$method" "${hdrs[@]}" -d "$body" "$API_BASE_URL$path" || echo 0)
  fi
  printf '%s' "$code"
}

# ---------- journey 1: login -------------------------------------------------

login_body=$(jq -n --arg email "$SMOKE_EMAIL" --arg pw "$SMOKE_PASSWORD" '{email:$email, password:$pw}')
code=$(call POST /api/auth/login "$login_body")
if [[ "$code" == "200" ]]; then
  TOKEN=$(jq -r '.token // ""' < /tmp/smoke_body)
  if [[ -n "$TOKEN" && "$TOKEN" != "null" ]]; then
    record "login" pass "token len=${#TOKEN}"
  else
    record "login" fail "200 but no token in body"
  fi
else
  record "login" fail "http $code"
fi

# ---------- journey 2: forgot password (best-effort) ------------------------

fp_body=$(jq -n --arg email "$SMOKE_EMAIL" '{email:$email}')
code=$(call POST /api/auth/forgot-password "$fp_body")
if [[ "$code" == "200" ]]; then
  record "forgot-password" pass ""
else
  record "forgot-password" fail "http $code"
fi

# ---------- journey 3: session -----------------------------------------------

if [[ -n "$TOKEN" ]]; then
  code=$(call GET /api/auth/session "")
  if [[ "$code" == "200" ]]; then
    uid=$(jq -r '.user_id // ""' < /tmp/smoke_body)
    record "session" pass "user_id=$uid"
  else
    record "session" fail "http $code"
  fi
else
  record "session" skip "no token"
fi

# ---------- journey 4: companies + fund discovery ---------------------------

if [[ -n "$TOKEN" ]]; then
  code=$(call GET /api/companies "")
  if [[ "$code" == "200" ]]; then
    count=$(jq '[.companies[].funds[]] | length' < /tmp/smoke_body 2>/dev/null || echo 0)
    record "companies" pass "funds=$count"
    if [[ -z "${SMOKE_FUND_ID:-}" ]]; then
      SMOKE_FUND_ID=$(jq -r '.companies[0].funds[0].id // empty' < /tmp/smoke_body)
    fi
  else
    record "companies" fail "http $code"
  fi
else
  record "companies" skip "no token"
fi

if [[ -z "${SMOKE_FUND_ID:-}" ]]; then
  log "$(yellow 'WARN'): no fund id discovered; remaining fund-scoped journeys will SKIP"
fi

# ---------- journeys 5-10: fund-scoped reads --------------------------------

declare -A SCOPED=(
  [fund-detail]="/api/funds/SMOKE_FUND_ID"
  [plans]="/api/funds/SMOKE_FUND_ID/plans"
  [team]="/api/funds/SMOKE_FUND_ID/team"
  [memory-agent]="/api/funds/SMOKE_FUND_ID/memory?layer=agent"
  [reflections]="/api/funds/SMOKE_FUND_ID/reflections?limit=5"
  [portfolio]="/api/funds/SMOKE_FUND_ID/portfolio"
)

for name in fund-detail plans team memory-agent reflections portfolio; do
  if [[ -z "$TOKEN" || -z "${SMOKE_FUND_ID:-}" ]]; then
    record "$name" skip "no fund context"
    continue
  fi
  path=${SCOPED[$name]/SMOKE_FUND_ID/$SMOKE_FUND_ID}
  code=$(call GET "$path" "")
  if [[ "$code" == "200" ]]; then
    record "$name" pass ""
  else
    record "$name" fail "http $code"
  fi
done

# ---------- journey 11: device register --------------------------------------

if [[ -n "$TOKEN" ]]; then
  dev_body=$(jq -n '{token:"smoke-test-fcm-token", platform:"android", app_version:"0.0.0-smoke"}')
  code=$(call POST /api/devices/register "$dev_body")
  if [[ "$code" == "200" ]]; then
    record "device-register" pass ""
    # Best-effort cleanup
    cleanup_body=$(jq -n '{token:"smoke-test-fcm-token"}')
    _=$(call POST /api/devices/unregister "$cleanup_body")
  else
    record "device-register" fail "http $code"
  fi
else
  record "device-register" skip "no token"
fi

# ---------- summary ----------------------------------------------------------

pass_count=0
fail_count=0
skip_count=0
for s in "${STATUSES[@]}"; do
  case "$s" in
    pass) ((pass_count++)) ;;
    fail) ((fail_count++)) ;;
    skip) ((skip_count++)) ;;
  esac
done

if [[ "$JSON_ONLY" -eq 1 ]]; then
  jq -n \
    --argjson pass "$pass_count" --argjson fail "$fail_count" --argjson skip "$skip_count" \
    --argjson rows "$(jq -nc \
      --argjson names "$(printf '%s\n' "${JOURNEYS[@]}" | jq -R . | jq -sc .)" \
      --argjson statuses "$(printf '%s\n' "${STATUSES[@]}" | jq -R . | jq -sc .)" \
      --argjson details "$(printf '%s\n' "${DETAILS[@]}" | jq -R . | jq -sc .)" \
      '[range($names|length) | {name:$names[.], status:$statuses[.], detail:$details[.]}]')" \
    '{summary:{pass:$pass, fail:$fail, skip:$skip}, journeys:$rows}'
fi

log ""
log "$(bold "Summary"): $(green "$pass_count pass") · $(red "$fail_count fail") · $(yellow "$skip_count skip")"

if [[ "$fail_count" -gt 0 ]]; then
  exit 2
fi
exit 0
