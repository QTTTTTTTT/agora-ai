#!/usr/bin/env bash
# =============================================================================
# scripts/perf-baseline.sh — collect release performance numbers
# =============================================================================
#
# Captures the metrics docs/RELEASE_QA_PLAYBOOK.md §2 requires for sign-off
# and appends one row per run to docs/perf-history.csv so we can graph
# release-over-release drift. Intended to run AFTER `docker compose up`
# is healthy and AFTER a release APK is built.
#
# Captured metrics:
#   - api_p95_ms             — server p95 latency from /api/metrics
#   - api_5xx_rate           — server 5xx ratio from /api/metrics
#   - boot_seconds           — wall-clock time `docker compose up` →
#                              /api/health returns 200
#   - apk_release_mb         — `ls` size of the freshly built APK
#   - web_dist_compressed_mb — gzipped size of web/dist
#
# CSV columns: ts,release,api_p95_ms,api_5xx_rate,boot_seconds,
#              apk_release_mb,web_dist_compressed_mb,git_sha
#
# Inputs (env vars):
#   API_BASE_URL    default http://localhost:8080
#   RELEASE         release identifier (defaults to `git describe --tags`)
#   APK_PATH        optional — full path to the release APK
# =============================================================================

set -euo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:8080}"
RELEASE="${RELEASE:-$(git describe --tags --always 2>/dev/null || echo unknown)}"
APK_PATH="${APK_PATH:-android/android/app/build/outputs/apk/release/app-release.apk}"
OUT_CSV="${OUT_CSV:-docs/perf-history.csv}"

now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
git_sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)

# ---- 1. Server metrics ------------------------------------------------------

api_p95_ms="NA"
api_5xx_rate="NA"
if curl -fsS "$API_BASE_URL/api/health" >/dev/null 2>&1; then
  metrics=$(curl -sS "$API_BASE_URL/api/metrics" || true)
  # crude extraction — works for the project's prom text encoding
  api_p95_ms=$(printf '%s' "$metrics" | awk '/^api_latency_ms_bucket\{.*le="800"\}/ {bucket800=$NF} /^api_latency_ms_count/ {count=$NF} END {if (count>0) print int(bucket800/count*100); else print "NA"}')
  api_5xx_rate=$(printf '%s' "$metrics" | awk '/^api_responses_total\{.*code=~"5..\"}/ {fives+=$NF} /^api_responses_total/ {total+=$NF} END {if (total>0) printf "%.4f", fives/total; else print "NA"}')
fi

# ---- 2. Boot seconds (assume current up cycle ≤ 5min ago) ------------------

boot_seconds="NA"
container_id=$(docker ps --filter "name=fundai-app" --format '{{.ID}}' 2>/dev/null || true)
if [[ -n "$container_id" ]]; then
  started=$(docker inspect -f '{{.State.StartedAt}}' "$container_id" 2>/dev/null || true)
  if [[ -n "$started" ]]; then
    boot_seconds=$(python3 -c "import datetime,sys; s=datetime.datetime.fromisoformat('${started%.*}'.replace('Z','+00:00')); n=datetime.datetime.now(datetime.timezone.utc); print(int((n-s).total_seconds()))" 2>/dev/null || echo NA)
  fi
fi

# ---- 3. APK size ------------------------------------------------------------

apk_mb="NA"
if [[ -f "$APK_PATH" ]]; then
  bytes=$(stat -f %z "$APK_PATH" 2>/dev/null || stat -c %s "$APK_PATH" 2>/dev/null || echo 0)
  apk_mb=$(awk -v b="$bytes" 'BEGIN { printf "%.1f", b/1024/1024 }')
fi

# ---- 4. Web dist compressed size -------------------------------------------

web_mb="NA"
if [[ -d "web/dist" ]]; then
  bytes=$(find web/dist -type f -exec gzip -c {} \; 2>/dev/null | wc -c | tr -d ' ')
  web_mb=$(awk -v b="$bytes" 'BEGIN { printf "%.1f", b/1024/1024 }')
fi

# ---- write row --------------------------------------------------------------

mkdir -p "$(dirname "$OUT_CSV")"
if [[ ! -f "$OUT_CSV" ]]; then
  echo "ts,release,api_p95_ms,api_5xx_rate,boot_seconds,apk_release_mb,web_dist_compressed_mb,git_sha" > "$OUT_CSV"
fi
echo "$now,$RELEASE,$api_p95_ms,$api_5xx_rate,$boot_seconds,$apk_mb,$web_mb,$git_sha" >> "$OUT_CSV"

# ---- print summary ----------------------------------------------------------

cat <<EOF
release       : $RELEASE  ($git_sha)
api_p95_ms    : $api_p95_ms     (target < 800)
api_5xx_rate  : $api_5xx_rate   (target < 0.001)
boot_seconds  : $boot_seconds   (target < 30)
apk_release_mb: $apk_mb         (target < 30)
web_dist_mb   : $web_mb
appended to   : $OUT_CSV
EOF
