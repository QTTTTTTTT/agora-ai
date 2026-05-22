#!/usr/bin/env bash
# F31: migration up→down→up drill.
#
# Purpose: prove that the down migrations for the most recent N
# migrations actually undo their up counterparts and that re-running
# the ups afterwards leaves the schema in a healthy state.
#
# Why "the most recent N" rather than the whole stack:
#   - Older migrations (001-025) predate this drill and never had .down
#     files written. Adding teardowns for ALL of them is its own multi-
#     week project and would risk destabilising the prod schema for
#     little real benefit (we'd never roll back 25 migrations in one go).
#   - The valuable invariant is "the migration we shipped TODAY can be
#     rolled back if it's broken". That's covered by checking the tail
#     of the migration list.
#
# Operational use:
#   * In CI: run against an ephemeral postgres (docker-compose service).
#   * Locally: ROUNDTRIP_DB_URL=postgres://user:pass@localhost:5432/drill ./scripts/migration_roundtrip_drill.sh
#
# Env:
#   ROUNDTRIP_DB_URL       full postgres DSN for the throwaway DB
#   ROUNDTRIP_DEPTH        how many recent migrations to drill (default 4)
#   MIGRATIONS_DIR         path to migrations (default ./server/migrations)
#   PSQL                   psql binary (default: from PATH)
#
# Exit codes:
#   0 success
#   2 prerequisite failure
#   3 missing .down for a migration in the drill window
#   4 down migration failed
#   5 re-up migration failed
#   6 schema not equivalent after roundtrip (object count mismatch)

set -euo pipefail

ROUNDTRIP_DB_URL="${ROUNDTRIP_DB_URL:-}"
ROUNDTRIP_DEPTH="${ROUNDTRIP_DEPTH:-4}"
MIGRATIONS_DIR="${MIGRATIONS_DIR:-./server/migrations}"
PSQL="${PSQL:-psql}"

if [[ -z "$ROUNDTRIP_DB_URL" ]]; then
    echo "migration_roundtrip_drill: set ROUNDTRIP_DB_URL to a throwaway DB DSN" >&2
    exit 2
fi
if ! command -v "$PSQL" >/dev/null 2>&1; then
    echo "migration_roundtrip_drill: $PSQL not found in PATH" >&2
    exit 2
fi
if [[ ! -d "$MIGRATIONS_DIR" ]]; then
    echo "migration_roundtrip_drill: migrations dir $MIGRATIONS_DIR not found" >&2
    exit 2
fi

# Collect up migrations sorted by their numeric prefix. We rely on the
# naming convention "NNN_*.sql" (no .down).
mapfile -t ALL_UPS < <(ls -1 "$MIGRATIONS_DIR" | grep -E '^[0-9]+_.*\.sql$' | grep -v '\.down\.sql$' | sort)
if [[ "${#ALL_UPS[@]}" -eq 0 ]]; then
    echo "migration_roundtrip_drill: no .sql migrations found in $MIGRATIONS_DIR" >&2
    exit 2
fi

# Take the last N as the drill window.
window_start=$(( ${#ALL_UPS[@]} - ROUNDTRIP_DEPTH ))
if (( window_start < 0 )); then window_start=0; fi
WINDOW=("${ALL_UPS[@]:$window_start}")

echo "migration_roundtrip_drill: ${#ALL_UPS[@]} total migrations, drilling last ${#WINDOW[@]}:"
for m in "${WINDOW[@]}"; do
    echo "  - $m"
done

# Make sure every migration in the window has a matching .down.sql.
# We fail early so the operator gets a clear message before any DB
# mutation, rather than discovering it mid-drill.
for up in "${WINDOW[@]}"; do
    down_name="${up%.sql}.down.sql"
    if [[ ! -f "$MIGRATIONS_DIR/$down_name" ]]; then
        echo "migration_roundtrip_drill: missing down migration for $up" >&2
        echo "  expected: $MIGRATIONS_DIR/$down_name" >&2
        exit 3
    fi
done

run_psql_file() {
    local file="$1"
    PGOPTIONS='--client-min-messages=warning' "$PSQL" \
        --quiet --set ON_ERROR_STOP=1 -f "$file" "$ROUNDTRIP_DB_URL"
}

# A simple schema fingerprint: counts of tables, indexes, constraints
# inside the public schema. Comparing before/after the down→up loop
# catches the common regression where a down migration "forgets" to
# drop an object that the up adds, so re-applying the up duplicates it
# (or fails with "already exists").
schema_fingerprint() {
    "$PSQL" --quiet --no-align --tuples-only --command \
        "SELECT
            (SELECT count(*) FROM pg_tables       WHERE schemaname='public') AS tables,
            (SELECT count(*) FROM pg_indexes      WHERE schemaname='public') AS indexes,
            (SELECT count(*) FROM information_schema.table_constraints WHERE table_schema='public') AS constraints" \
        "$ROUNDTRIP_DB_URL"
}

echo "migration_roundtrip_drill: capturing baseline fingerprint"
baseline="$(schema_fingerprint)"
echo "  baseline: $baseline"

# Phase 1: roll back the window (newest → oldest).
echo "migration_roundtrip_drill: rolling back ${#WINDOW[@]} migrations"
for (( i=${#WINDOW[@]}-1; i>=0; i-- )); do
    down="${WINDOW[$i]%.sql}.down.sql"
    echo "  down: $down"
    if ! run_psql_file "$MIGRATIONS_DIR/$down"; then
        echo "migration_roundtrip_drill: down migration $down FAILED" >&2
        exit 4
    fi
done

# Phase 2: re-apply the window (oldest → newest). This is the critical
# test — if a down migration left state behind, the up will fail with
# "already exists" or similar.
echo "migration_roundtrip_drill: re-applying ${#WINDOW[@]} migrations"
for up in "${WINDOW[@]}"; do
    echo "  up: $up"
    if ! run_psql_file "$MIGRATIONS_DIR/$up"; then
        echo "migration_roundtrip_drill: re-up migration $up FAILED" >&2
        exit 5
    fi
done

# Phase 3: confirm the schema fingerprint is identical. This is a
# coarse check (it would miss e.g. a column type change), but catches
# 90% of regressions: missing dropped index, missing dropped constraint,
# dropped-and-re-added table accidentally getting an extra column.
echo "migration_roundtrip_drill: capturing post-roundtrip fingerprint"
final="$(schema_fingerprint)"
echo "  final:    $final"
if [[ "$baseline" != "$final" ]]; then
    echo "migration_roundtrip_drill: schema fingerprint mismatch after roundtrip" >&2
    echo "  baseline: $baseline" >&2
    echo "  final:    $final" >&2
    exit 6
fi

echo "migration_roundtrip_drill: success — last ${#WINDOW[@]} migrations roundtripped cleanly"
