#!/usr/bin/env bash
# chaos-incident-replay.sh — regression-test runner for the four
# production incidents archived under server/migrations/historical/.
#
# What this is: a one-command "are the defensive code paths that
# closed each historical incident still working?" gate. Each
# scenario maps a real incident (with its manual SQL cleanup
# script preserved in server/migrations/historical/) to the
# specific Go regression tests that exercise the fix. If any test
# fails, the script exits non-zero and prints a clear pointer to
# the incident write-up + the closing commit, so a regression is
# triaged with full context — not as a generic "test broke".
#
# What this is NOT: a chaos-engineering harness that mutates a
# live system. It does not write to the database, kill containers,
# or simulate network faults. The "chaos" framing reflects the
# spirit (replay an incident) rather than the technique (we use
# Go test fixtures because that's where the post-incident defences
# already live).
#
# Why this script exists: the four manual_*.sql scripts in
# server/migrations/historical/ are the cleanup record but say
# nothing about whether the defences that landed alongside the
# cleanup are still wired. A reader of those files only learns
# "this happened in prod once" — they have to grep the codebase
# to find the fix and re-grep the test suite to find the
# regression. This script is the missing third corner: incident
# → defence (commit ref) → regression test.
#
# Usage:
#   bash scripts/chaos-incident-replay.sh                    # runs all four
#   bash scripts/chaos-incident-replay.sh A                  # one scenario
#   bash scripts/chaos-incident-replay.sh A B C              # several
#
# Scenarios:
#   A — PM quote-unavailable fallback stamping notional into Price
#       (manual_reversal_erroneous_fills_20260603.sql)
#   B — Broker simulator lot-size gate (manual_s12_lotsize_cleanup_20260603.sql)
#   C — Cash-ledger atomicity / reconciliation invariants
#       (manual_full_dirty_data_sweep_20260603.sql)
#   D — PM-direct-fill bypass of broker.LotSizeGate
#       (manual_s12_pmpath_lotsize_cleanup_20260604.sql)

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER_DIR="$ROOT_DIR/server"

# Each scenario is a 4-tuple, encoded as parallel arrays so we
# stay POSIX-friendly (no associative arrays — bash 3.2 on stock
# macOS doesn't have them):
#
#   ID            short letter the operator picks on the CLI
#   TITLE         human label for the log header
#   INCIDENT_SQL  the historical cleanup file (relative to repo)
#   GO_TEST_RUN   value passed to `go test -run` (regex of test names)
#
# Order matters: matching ALL_IDS to the SCENARIO_* arrays.

ALL_IDS=("A" "B" "C" "D")

declare -a SCENARIO_TITLES=(
  "PM quote-unavailable fallback stamps notional into Price (Quantity=1)"
  "Broker LotSizeGate rejects fractional / non-lot orders"
  "Cash-ledger atomicity + reconciliation invariants"
  "PM-direct-fill bypass of broker.LotSizeGate (pmPathLotSizeGuard)"
)

declare -a SCENARIO_INCIDENT_FILES=(
  "server/migrations/historical/manual_reversal_erroneous_fills_20260603.sql"
  "server/migrations/historical/manual_s12_lotsize_cleanup_20260603.sql"
  "server/migrations/historical/manual_full_dirty_data_sweep_20260603.sql"
  "server/migrations/historical/manual_s12_pmpath_lotsize_cleanup_20260604.sql"
)

# Each entry is the regex argument to `go test -run`. We pin to
# specific test names rather than packages so an unrelated test
# failure in the same package doesn't masquerade as an incident
# regression.
declare -a SCENARIO_GO_TEST_RUNS=(
  "^TestTranslateBuyAction_QuoteUnavailable_DowngradesToWatch$"
  "^TestSimulator_LotSizeGate_(Rejects_301308_1Share_Regression|AllowsValidOrder|RejectWithSuggestion_IncludesSuggestionInError|RunsAfterPriceCollar)$"
  "^TestCashLedger_(Append_RejectsZeroAmount|Append_IdempotentConflictReturnsExisting|BalanceByFund|SubtotalByEntryType)$"
  "^TestPMPathLotSizeGuard_(RejectsSTARBuyBelowMinLot|RejectsChinextBuyBelow100|RejectsSTARSellLeavingOddLotResidual|RejectsSellWhenNoPosition|AllowsValidSTARBuy|AllowsFullPositionSellEvenIfOddLot)$"
)

declare -a SCENARIO_PACKAGES=(
  "./cmd/server/..."
  "./internal/broker/..."
  "./internal/repository/..."
  "./cmd/server/..."
)

# Pretty colour codes when stdout is a TTY; plain text in CI logs.
if [ -t 1 ]; then
  C_BOLD="$(tput bold)"; C_RED="$(tput setaf 1)"; C_GREEN="$(tput setaf 2)"
  C_YELLOW="$(tput setaf 3)"; C_RESET="$(tput sgr0)"
else
  C_BOLD=""; C_RED=""; C_GREEN=""; C_YELLOW=""; C_RESET=""
fi

print_scenario_list() {
  printf "${C_BOLD}Available scenarios:${C_RESET}\n"
  for i in "${!ALL_IDS[@]}"; do
    printf "  ${C_BOLD}%s${C_RESET}  %s\n      incident: %s\n" \
      "${ALL_IDS[$i]}" "${SCENARIO_TITLES[$i]}" "${SCENARIO_INCIDENT_FILES[$i]}"
  done
}

# Look up the index of an ID in ALL_IDS, or "-1" if not found.
index_of() {
  local needle="$1"
  for i in "${!ALL_IDS[@]}"; do
    if [ "${ALL_IDS[$i]}" = "$needle" ]; then
      echo "$i"
      return
    fi
  done
  echo "-1"
}

# Resolve the requested scenarios from CLI args — empty = all.
if [ "$#" -eq 0 ]; then
  REQUESTED=("${ALL_IDS[@]}")
else
  REQUESTED=("$@")
fi

# Validate before running anything so the operator gets all
# typos at once, not one-by-one across long test runs.
for id in "${REQUESTED[@]}"; do
  if [ "$(index_of "$id")" = "-1" ]; then
    printf "${C_RED}Unknown scenario: %s${C_RESET}\n\n" "$id" >&2
    print_scenario_list >&2
    exit 1
  fi
done

# Header.
printf "${C_BOLD}== chaos-incident-replay ==${C_RESET}\n"
printf "Repo:    %s\n" "$ROOT_DIR"
printf "Replay:  %s\n\n" "${REQUESTED[*]}"

failed=()

for id in "${REQUESTED[@]}"; do
  idx="$(index_of "$id")"
  title="${SCENARIO_TITLES[$idx]}"
  incident="${SCENARIO_INCIDENT_FILES[$idx]}"
  pkg="${SCENARIO_PACKAGES[$idx]}"
  run="${SCENARIO_GO_TEST_RUNS[$idx]}"

  printf "%s\n" "${C_BOLD}--- Scenario ${id} — ${title} ---${C_RESET}"
  printf "Incident record: %s\n" "$incident"
  printf "Regression tests: go test %s -run '%s'\n" "$pkg" "$run"

  if (cd "$SERVER_DIR" && go test "$pkg" -count=1 -run "$run"); then
    printf "${C_GREEN}PASS${C_RESET} — defences for incident %s remain wired\n\n" "$id"
  else
    printf "${C_RED}FAIL${C_RESET} — incident %s defences regressed\n" "$id"
    printf "  Read the incident record at: %s/%s\n" "$ROOT_DIR" "$incident"
    printf "  Failing tests are part of the post-incident hardening — fix the\n"
    printf "  test, not the test name; if you must rename, update this script.\n\n"
    failed+=("$id")
  fi
done

if [ "${#failed[@]}" -gt 0 ]; then
  printf "${C_RED}${C_BOLD}REPLAY FAILED${C_RESET} — scenarios with regressed defences: %s\n" "${failed[*]}" >&2
  exit 1
fi

printf "${C_GREEN}${C_BOLD}REPLAY OK${C_RESET} — all requested incident defences still in place.\n"
