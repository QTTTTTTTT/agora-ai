#!/usr/bin/env bash
# =============================================================================
# scripts/bundle-budget.sh — enforce frontend bundle size budgets
# =============================================================================
#
# Prevents bundle-size regressions from sneaking in. Reads
# web/bundle-budget.json, scans web/dist/assets/*.js, computes the
# gzipped size of each, and fails (exit 1) if any chunk exceeds its
# named cap or if the total exceeds totalGzippedKB.
#
# Used by CI (.github/workflows/ci.yml) and locally via
# `make bundle-budget`. Both invocations expect `web/dist` to
# already exist — run `npm run build` first.
#
# OUTPUT
# ------
# Per-chunk table (size / budget / ok|over) plus a Total line. Exit
# code 0 on all-green, 1 on any over-budget chunk or total. The
# table goes to stdout, errors to stderr — easy to redirect into a
# CI annotation.
#
# DESIGN NOTES
# ------------
# - Chunk names match the prefix in the file basename. Vite outputs
#   files like `Dashboard-DTy6CtjP.js`; we strip the hash suffix
#   and look up `Dashboard` in chunkBudgetsGzippedKB. Anything
#   without a named entry uses perChunkDefaultGzippedKB.
# - Gzip is via the system `gzip -c | wc -c`. We pick that over a
#   Go / Node tool to keep the script self-contained — no extra
#   build step in CI before the bundle check itself.
# - Source maps (*.map) are ignored.

set -uo pipefail

DIST="${DIST:-web/dist}"
BUDGET_FILE="${BUDGET_FILE:-web/bundle-budget.json}"

if [[ ! -d "$DIST/assets" ]]; then
  echo "ERROR: $DIST/assets not found. Run \`cd web && npm run build\` first." >&2
  exit 2
fi
if [[ ! -f "$BUDGET_FILE" ]]; then
  echo "ERROR: $BUDGET_FILE not found." >&2
  exit 2
fi

# Read budgets via Python so we don't hard-depend on jq (CI's image may differ).
read_budget() {
  python3 - "$BUDGET_FILE" "$1" <<'PY'
import json, sys
budget_path, key = sys.argv[1], sys.argv[2]
with open(budget_path) as fh:
    cfg = json.load(fh)
if key.startswith("chunk:"):
    name = key.split(":", 1)[1]
    chunks = cfg.get("chunkBudgetsGzippedKB", {})
    print(chunks.get(name, cfg.get("perChunkDefaultGzippedKB", 50)))
else:
    print(cfg.get(key, 0))
PY
}

total_kb=0
total_budget_kb=$(read_budget totalGzippedKB)
over_count=0

printf '%-45s %12s %12s %s\n' "chunk" "size(KB gz)" "budget(KB)" "status"
printf '%-45s %12s %12s %s\n' "---------------------------------------------" "-----------" "----------" "------"

while IFS= read -r f; do
  base=$(basename "$f")
  # Strip the hash suffix Vite appends, e.g. "Dashboard-DTy6CtjP.js" -> "Dashboard"
  chunk_name=$(printf '%s' "$base" | sed -E 's/-[A-Za-z0-9_-]+\.js$//')
  bytes=$(gzip -c "$f" | wc -c | tr -d ' ')
  size_kb=$(awk -v b="$bytes" 'BEGIN { printf "%.1f", b/1024 }')
  total_kb=$(awk -v a="$total_kb" -v b="$size_kb" 'BEGIN { printf "%.1f", a+b }')

  budget_kb=$(read_budget "chunk:$chunk_name")
  if awk -v s="$size_kb" -v b="$budget_kb" 'BEGIN { exit !(s > b) }'; then
    status="OVER"
    over_count=$((over_count + 1))
  else
    status="ok"
  fi
  printf '%-45s %12s %12s %s\n' "$chunk_name" "$size_kb" "$budget_kb" "$status"
done < <(find "$DIST/assets" -name '*.js' -not -name '*.map' | sort)

printf '%-45s %12s %12s %s\n' "---------------------------------------------" "-----------" "----------" "------"
total_status="ok"
if awk -v s="$total_kb" -v b="$total_budget_kb" 'BEGIN { exit !(s > b) }'; then
  total_status="OVER"
  over_count=$((over_count + 1))
fi
printf '%-45s %12s %12s %s\n' "TOTAL" "$total_kb" "$total_budget_kb" "$total_status"

if [[ "$over_count" -gt 0 ]]; then
  echo "" >&2
  echo "BUNDLE BUDGET EXCEEDED: $over_count item(s) over their cap." >&2
  echo "If the regression is intentional:" >&2
  echo "  1) explain why in the PR description," >&2
  echo "  2) bump the relevant cap in $BUDGET_FILE," >&2
  echo "  3) reviewers ack the increase as part of code review." >&2
  exit 1
fi
echo ""
echo "Bundle budget green ($total_kb KB / $total_budget_kb KB total, all chunks within cap)."
