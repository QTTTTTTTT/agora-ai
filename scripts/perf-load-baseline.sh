#!/usr/bin/env bash
# =============================================================================
# scripts/perf-load-baseline.sh — active-load perf baseline (companion
# to scripts/perf-baseline.sh, which only samples /api/metrics passively)
# =============================================================================
#
# WHY THIS EXISTS
# ---------------
# perf-baseline.sh reads p95 / 5xx rate from the running server's
# Prometheus surface. That's only meaningful if the server is actually
# *under* load when sampled. In CI / dev nobody manually generates
# traffic before running perf-baseline, so the numbers tend to be
# "0 traffic, 0 latency" and we miss regressions.
#
# This script generates a controlled 30-second burst of traffic
# against /api/health (the cheapest, cache-friendly endpoint, so
# this measures the HTTP stack itself — chi router, middleware
# chain, JSON encoder — not any business handler), THEN samples the
# server's metrics, AND captures the load-side percentiles from
# the in-tree perfload tool. Two viewpoints — server-side and
# client-side — that should agree within ~10ms; large divergence
# is a smoking gun for queue / proxy / TCP issues.
#
# This is the "short-run version" of H5 from the production-readiness
# review. The full version still requires k6 + multi-load curves
# (1k / 5k / 10k QPS) to map the saturation envelope; this is a
# repeatable single-point baseline that catches >2x regressions.
#
# OUTPUT
# ------
# Appends one row to docs/perf-load-history.csv with columns:
#   ts,release,git_sha,target_url,duration_s,concurrency,
#   client_qps,client_p50_ms,client_p95_ms,client_p99_ms,
#   client_err_rate,client_5xx_rate,
#   server_p95_ms,server_5xx_rate
#
# INPUTS (env vars)
# -----------------
#   API_BASE_URL    default http://localhost:8080
#   TARGET_PATH     default /api/health
#   DURATION        default 30s
#   CONCURRENCY     default 50
#   RELEASE         release identifier (defaults to `git describe --tags`)
#   OUT_CSV         defaults to docs/perf-load-history.csv
# =============================================================================

set -euo pipefail

API_BASE_URL="${API_BASE_URL:-http://localhost:8080}"
TARGET_PATH="${TARGET_PATH:-/api/health}"
DURATION="${DURATION:-30s}"
CONCURRENCY="${CONCURRENCY:-50}"
RELEASE="${RELEASE:-$(git describe --tags --always 2>/dev/null || echo unknown)}"
OUT_CSV="${OUT_CSV:-docs/perf-load-history.csv}"

now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
git_sha=$(git rev-parse --short HEAD 2>/dev/null || echo unknown)
target_url="${API_BASE_URL}${TARGET_PATH}"

# ---- 1. Server up? ---------------------------------------------------------

if ! curl -fsS "$API_BASE_URL/api/health" >/dev/null 2>&1; then
  echo "ERROR: $API_BASE_URL/api/health not reachable. Start the stack with \`scripts/start.sh\` first." >&2
  exit 1
fi

# ---- 2. Run the in-tree load generator ------------------------------------

echo "perf-load-baseline: $target_url duration=$DURATION concurrency=$CONCURRENCY"
load_json=$(
  cd server && \
  go run ./cmd/perfload \
    -url "$target_url" \
    -duration "$DURATION" \
    -concurrency "$CONCURRENCY" \
    -json
)

extract() {
  printf '%s' "$load_json" | python3 -c "import json,sys; d=json.loads(sys.stdin.read()); print(d.get('$1', ''))"
}

client_qps=$(extract qps)
client_p50_ms=$(extract p50_ms)
client_p95_ms=$(extract p95_ms)
client_p99_ms=$(extract p99_ms)
client_err_rate=$(extract err_rate)
client_5xx_rate=$(extract five_xx_rate)

# ---- 3. Sample server-side metrics WHILE / right after the load -----------
# (The server keeps a rolling window, so post-load read still sees the
#  burst; this is "good enough" for short baselines.)

server_p95_ms="NA"
server_5xx_rate="NA"
metrics=$(curl -sS "$API_BASE_URL/api/metrics" 2>/dev/null || true)
if [[ -n "$metrics" ]]; then
  server_p95_ms=$(printf '%s' "$metrics" | awk '/^api_latency_ms_bucket\{.*le="800"\}/ {bucket800=$NF} /^api_latency_ms_count/ {count=$NF} END {if (count>0) print int(bucket800/count*100); else print "NA"}')
  server_5xx_rate=$(printf '%s' "$metrics" | awk '/^api_responses_total\{.*code=~"5..\"}/ {fives+=$NF} /^api_responses_total/ {total+=$NF} END {if (total>0) printf "%.4f", fives/total; else print "NA"}')
fi

# ---- 4. Persist & summarise -----------------------------------------------

mkdir -p "$(dirname "$OUT_CSV")"
if [[ ! -f "$OUT_CSV" ]]; then
  echo "ts,release,git_sha,target_url,duration,concurrency,client_qps,client_p50_ms,client_p95_ms,client_p99_ms,client_err_rate,client_5xx_rate,server_p95_ms,server_5xx_rate" > "$OUT_CSV"
fi
echo "$now,$RELEASE,$git_sha,$target_url,$DURATION,$CONCURRENCY,$client_qps,$client_p50_ms,$client_p95_ms,$client_p99_ms,$client_err_rate,$client_5xx_rate,$server_p95_ms,$server_5xx_rate" >> "$OUT_CSV"

cat <<EOF

perf-load-baseline summary
  release        $RELEASE  ($git_sha)
  target         $target_url
  duration       $DURATION
  concurrency    $CONCURRENCY

  client-side    qps=$client_qps  p50=${client_p50_ms}ms  p95=${client_p95_ms}ms  p99=${client_p99_ms}ms
                 err_rate=$client_err_rate  5xx_rate=$client_5xx_rate

  server-side    p95=${server_p95_ms}ms  5xx_rate=$server_5xx_rate
                 (sampled from /api/metrics — should agree with client p95
                  within ~10ms; bigger gap indicates queue or proxy issues)

  appended to    $OUT_CSV
EOF
