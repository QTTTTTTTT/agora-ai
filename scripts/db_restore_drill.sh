#!/usr/bin/env bash
# F30: backup restore drill — PROVES a backup is restorable end-to-end.
#
# What it does:
#   1. Locates the newest backup in $BACKUP_DIR matching the db label.
#   2. Verifies the SHA-256 sidecar matches (corruption guard).
#   3. Creates a throwaway database "<original>_restore_drill_<ts>".
#   4. Runs pg_restore against the throwaway DB with --exit-on-error.
#   5. Spot-checks that key tables exist and have non-negative row counts.
#   6. Drops the throwaway DB (unless --keep is passed).
#
# Why this script exists:
#   "We have backups" is meaningless until "we can restore them" is proven.
#   This drill is meant to run weekly in CI/cron. A failure here means the
#   backup is bad / the restore process drifted / a schema dependency broke
#   — exactly the surprises you do NOT want to discover during an outage.
#
# Required env (same as db_backup.sh):
#   DATABASE_URL or PGHOST/PGUSER/PGPASSWORD/PGDATABASE
# Optional env:
#   BACKUP_DIR         where to find dumps (default ./backups)
#   PG_RESTORE         path to pg_restore (default: from PATH)
#   PSQL               path to psql       (default: from PATH)
#   EXPECTED_TABLES    space-separated table names that MUST exist post-restore
#                      (default: "users funds workflow_runs wallet_accounts")
# Flags:
#   --keep             skip cleanup; leave the drill DB in place
#                      (useful for manual inspection after a failure)
#
# Exit codes:
#   0 success: restore + spot-checks all pass
#   2 prerequisite missing
#   3 no backups found
#   4 checksum mismatch
#   5 pg_restore failed
#   6 spot-check failed (expected table missing or row scan errored)

set -euo pipefail

KEEP_DRILL_DB=0
for arg in "$@"; do
    case "$arg" in
        --keep) KEEP_DRILL_DB=1 ;;
        *) echo "db_restore_drill: unknown flag $arg" >&2; exit 2 ;;
    esac
done

BACKUP_DIR="${BACKUP_DIR:-./backups}"
PG_RESTORE="${PG_RESTORE:-pg_restore}"
PSQL="${PSQL:-psql}"
EXPECTED_TABLES="${EXPECTED_TABLES:-users funds workflow_runs wallet_accounts}"

for tool in "$PG_RESTORE" "$PSQL"; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        echo "db_restore_drill: $tool not found in PATH" >&2
        exit 2
    fi
done

if [[ -z "${DATABASE_URL:-}" && -z "${PGDATABASE:-}" ]]; then
    echo "db_restore_drill: set DATABASE_URL or PGDATABASE before invoking" >&2
    exit 2
fi

# Resolve canonical db label (same logic as db_backup.sh).
db_label="${PGDATABASE:-}"
if [[ -z "$db_label" && -n "${DATABASE_URL:-}" ]]; then
    db_label="$(printf '%s\n' "$DATABASE_URL" | awk -F'[/?]' '{print $4}')"
fi
db_label="${db_label:-fundai}"

# Newest dump for this db label. We sort lexicographically because the
# filename embeds an ISO-8601 timestamp, which sorts identically to
# chronological order.
latest_dump="$(ls -1 "$BACKUP_DIR" 2>/dev/null | grep -E "^${db_label}-[0-9TZ]+\.dump$" | sort | tail -n 1 || true)"
if [[ -z "$latest_dump" ]]; then
    echo "db_restore_drill: no backups matching ${db_label}-*.dump found in $BACKUP_DIR" >&2
    exit 3
fi
dump_path="$BACKUP_DIR/$latest_dump"
sha_path="${dump_path}.sha256"

echo "db_restore_drill: candidate dump $dump_path"

if [[ -f "$sha_path" ]]; then
    echo "db_restore_drill: verifying checksum"
    if command -v sha256sum >/dev/null 2>&1; then
        (cd "$BACKUP_DIR" && sha256sum -c "$(basename "$sha_path")") || { echo "db_restore_drill: checksum mismatch" >&2; exit 4; }
    elif command -v shasum >/dev/null 2>&1; then
        (cd "$BACKUP_DIR" && shasum -a 256 -c "$(basename "$sha_path")") || { echo "db_restore_drill: checksum mismatch" >&2; exit 4; }
    else
        echo "db_restore_drill: no sha256 tool found; SKIPPING checksum verification" >&2
    fi
else
    echo "db_restore_drill: no .sha256 sidecar present; skipping checksum verification" >&2
fi

drill_ts="$(date -u +%Y%m%dt%H%M%SZ)"
drill_db="${db_label}_restore_drill_${drill_ts}"

# Build a copy of DATABASE_URL pointing at the postgres maintenance DB
# for createdb / dropdb statements. We can't connect to the drill DB
# itself before creating it, and we can't connect to the prod DB without
# risking leaking pg_restore writes there.
if [[ -n "${DATABASE_URL:-}" ]]; then
    base_url="${DATABASE_URL%/${db_label}*}"
    rest="${DATABASE_URL#*/${db_label}}"  # captures the optional ?query
    admin_url="${base_url}/postgres${rest}"
    drill_url="${base_url}/${drill_db}${rest}"
else
    admin_url=""
    drill_url=""
fi

run_psql() {
    local target="$1"; shift
    if [[ -n "$target" ]]; then
        PGOPTIONS='--client-min-messages=warning' "$PSQL" --quiet --no-align --tuples-only --command "$*" "$target"
    else
        PGOPTIONS='--client-min-messages=warning' "$PSQL" --quiet --no-align --tuples-only --command "$*"
    fi
}

echo "db_restore_drill: creating drill database $drill_db"
run_psql "$admin_url" "CREATE DATABASE \"${drill_db}\";"

cleanup_drill_db() {
    if [[ "$KEEP_DRILL_DB" -eq 1 ]]; then
        echo "db_restore_drill: --keep passed, leaving $drill_db in place for inspection"
        return
    fi
    echo "db_restore_drill: dropping drill database $drill_db"
    # Use IF EXISTS so a failure mid-restore doesn't leave a stale db
    # AND a noisy error from the cleanup. Force-disconnect any sessions
    # that pg_restore may have left around.
    run_psql "$admin_url" "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname='${drill_db}' AND pid <> pg_backend_pid();" || true
    run_psql "$admin_url" "DROP DATABASE IF EXISTS \"${drill_db}\";" || true
}
trap cleanup_drill_db EXIT

echo "db_restore_drill: running pg_restore"
if ! "$PG_RESTORE" \
    --dbname="$drill_url" \
    --exit-on-error \
    --no-owner --no-acl \
    --jobs=2 \
    "$dump_path"; then
    echo "db_restore_drill: pg_restore failed" >&2
    exit 5
fi

echo "db_restore_drill: spot-checking expected tables: $EXPECTED_TABLES"
for table in $EXPECTED_TABLES; do
    if ! count="$(run_psql "$drill_url" "SELECT count(*) FROM \"${table}\";")"; then
        echo "db_restore_drill: spot-check failed querying $table" >&2
        exit 6
    fi
    # Strip whitespace; psql --tuples-only emits "    123\n"
    count="$(echo "$count" | tr -d '[:space:]')"
    if [[ -z "$count" || "$count" -lt 0 ]]; then
        echo "db_restore_drill: $table returned invalid count '$count'" >&2
        exit 6
    fi
    echo "  $table: $count rows"
done

echo "db_restore_drill: success — backup $latest_dump is restorable"
