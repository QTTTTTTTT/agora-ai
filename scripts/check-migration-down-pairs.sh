#!/usr/bin/env bash
# check-migration-down-pairs.sh — refuse a merge if any migration
# under server/migrations/ ships without a matching .down.sql
# (with a small allow-list for the grandfathered historical files).
#
# Why a separate script and not just a one-liner in CI:
#
#   * We want this to also run locally before the PR is pushed —
#     `make check-migrations` is the friendly handle, and the CI
#     job invokes the same script so dev / CI parity is exact.
#
#   * The allow-list lives in source (server/migrations/.no-down-allow-list)
#     so adding to it is a deliberate, reviewable change rather
#     than a CI-only knob a developer can flip without leaving a
#     paper trail. Anything in that file must justify itself in
#     the PR description ("foundational schema bootstrap, no
#     mechanical rollback possible — see ADR-X").
#
# Failure modes the gate catches:
#
#   * Forgot the .down.sql entirely when adding a migration — a
#     deploy can roll forward but not back, and any prod hotfix
#     that needs to undo the change becomes an emergency.
#   * Off-by-one name mismatch (e.g. 113_xxx.sql but the down is
#     113_xxx.dn.sql or 113_yyy.down.sql). Strict pair-by-stem
#     comparison catches these too.
#
# Failure mode the gate does NOT catch (out of scope, by design):
#
#   * Whether the .down.sql actually inverts the .sql. That's a
#     reviewer concern. We only prove a file exists with the
#     right name; semantic correctness is for code review and
#     the rollback rehearsal in scripts/db-rollback-smoke.sh.

set -euo pipefail

MIGRATIONS_DIR="${MIGRATIONS_DIR:-server/migrations}"
ALLOW_LIST_FILE="${ALLOW_LIST_FILE:-${MIGRATIONS_DIR}/.no-down-allow-list}"

if [[ ! -d "$MIGRATIONS_DIR" ]]; then
    echo "ERROR: migrations dir not found: $MIGRATIONS_DIR" >&2
    exit 2
fi

# Collect the set of stems (e.g. "111_paper_portfolios_public_track")
# for every up-migration that exists. Skip .down.sql, .no-down-allow-list,
# and any dotfiles. -printf is GNU-only; we use a portable basename
# stream so this works on macOS dev boxes as well as Ubuntu CI.
ups=$(
    find "$MIGRATIONS_DIR" -maxdepth 1 -type f -name '*.sql' \
        ! -name '*.down.sql' \
    | while read -r path; do basename "$path" .sql; done \
    | sort -u
)

downs=$(
    find "$MIGRATIONS_DIR" -maxdepth 1 -type f -name '*.down.sql' \
    | while read -r path; do basename "$path" .down.sql; done \
    | sort -u
)

# Load allow-list (one stem per line, # comments and blank lines ignored).
allow=""
if [[ -f "$ALLOW_LIST_FILE" ]]; then
    allow=$(grep -Ev '^\s*(#|$)' "$ALLOW_LIST_FILE" | sort -u || true)
fi

# Compute (ups - downs) - allow.
missing=$(comm -23 <(echo "$ups") <(echo "$downs"))
unexcused=""
if [[ -n "$allow" ]]; then
    unexcused=$(comm -23 <(echo "$missing") <(echo "$allow"))
else
    unexcused="$missing"
fi

# Sanity-check the allow-list: entries that no longer match an
# actual up-migration are removed-but-someone-forgot-to-delete-the-
# allow-list-entry stale rows. We complain loudly so the file
# doesn't grow unbounded.
stale_allow=""
if [[ -n "$allow" ]]; then
    stale_allow=$(comm -23 <(echo "$allow") <(echo "$ups"))
fi

# Also flag down files that have no matching up (renamed up but
# kept the old down hanging around). Cheap to catch here, and a
# real source of "ran rollback, nothing happened" confusion.
orphan_downs=$(comm -13 <(echo "$ups") <(echo "$downs"))

fail=0

if [[ -n "${unexcused//[[:space:]]/}" ]]; then
    fail=1
    echo "ERROR: the following migrations are missing a .down.sql pair" >&2
    echo "       and are NOT on the allow-list at $ALLOW_LIST_FILE:" >&2
    echo "" >&2
    while IFS= read -r stem; do
        [[ -z "$stem" ]] && continue
        echo "  - ${stem}.sql  (expected ${stem}.down.sql)" >&2
    done <<<"$unexcused"
    echo "" >&2
    echo "Fix options:" >&2
    echo "  1. Write the .down.sql (preferred — most migrations are mechanically reversible)." >&2
    echo "  2. Add the stem to $ALLOW_LIST_FILE with a one-line comment justifying" >&2
    echo "     why mechanical rollback is impossible (foundational schema, large data" >&2
    echo "     restructure, etc.). Reviewers gate on the justification, not the file." >&2
fi

if [[ -n "${stale_allow//[[:space:]]/}" ]]; then
    fail=1
    echo "ERROR: $ALLOW_LIST_FILE has entries with no matching .sql file:" >&2
    while IFS= read -r stem; do
        [[ -z "$stem" ]] && continue
        echo "  - $stem  (no $stem.sql in $MIGRATIONS_DIR)" >&2
    done <<<"$stale_allow"
    echo "Remove these stale entries — the file should track current state only." >&2
fi

if [[ -n "${orphan_downs//[[:space:]]/}" ]]; then
    fail=1
    echo "ERROR: the following .down.sql files have no matching .sql:" >&2
    while IFS= read -r stem; do
        [[ -z "$stem" ]] && continue
        echo "  - ${stem}.down.sql  (no ${stem}.sql)" >&2
    done <<<"$orphan_downs"
fi

if [[ $fail -ne 0 ]]; then
    exit 1
fi

up_total=$(printf '%s\n' "$ups" | grep -c . || true)
down_total=$(printf '%s\n' "$downs" | grep -c . || true)
allow_total=$(printf '%s\n' "$allow" | grep -c . || true)
echo "OK: $up_total up-migrations, $down_total downs, $allow_total grandfathered (no orphans, no unexcused gaps)."
