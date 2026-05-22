#!/usr/bin/env bash
# F30: PostgreSQL backup helper.
#
# Writes a custom-format pg_dump to $BACKUP_DIR/<db>-<ISO timestamp>.dump
# plus a SHA-256 sidecar so a corrupted file is visible without parsing.
# Custom format is chosen over plain SQL because:
#   - pg_restore can parallelize (-j) and selectively restore tables
#   - file is much smaller (compressed by default)
#   - the format is the input the drill script expects
#
# Required env:
#   DATABASE_URL or PGHOST/PGUSER/PGDATABASE (libpq env vars)
# Optional env:
#   BACKUP_DIR     destination directory (default ./backups)
#   RETENTION_DAYS rolling deletion window (default 7; 0 disables cleanup)
#   PG_DUMP        path to pg_dump (default: from $PATH)
#
# Exit codes:
#   0  success
#   2  prerequisite missing (pg_dump not found, env unset)
#   3  dump failed
#   4  checksum write failed
#
# Designed for cron use: idempotent, single-output, stable filenames.

set -euo pipefail

BACKUP_DIR="${BACKUP_DIR:-./backups}"
RETENTION_DAYS="${RETENTION_DAYS:-7}"
PG_DUMP="${PG_DUMP:-pg_dump}"

if ! command -v "$PG_DUMP" >/dev/null 2>&1; then
    echo "db_backup: $PG_DUMP not found in PATH" >&2
    exit 2
fi

if [[ -z "${DATABASE_URL:-}" && -z "${PGDATABASE:-}" ]]; then
    echo "db_backup: set DATABASE_URL or PGDATABASE before invoking" >&2
    exit 2
fi

mkdir -p "$BACKUP_DIR"

# Database label for the filename. Prefer the db name parsed out of
# DATABASE_URL since DATABASE_URL is the source of truth in containers.
db_label="${PGDATABASE:-}"
if [[ -z "$db_label" && -n "${DATABASE_URL:-}" ]]; then
    # postgresql://user:pass@host:port/dbname?query → dbname
    db_label="$(printf '%s\n' "$DATABASE_URL" | awk -F'[/?]' '{print $4}')"
fi
db_label="${db_label:-fundai}"

timestamp="$(date -u +%Y%m%dT%H%M%SZ)"
dump_path="$BACKUP_DIR/${db_label}-${timestamp}.dump"
sha_path="${dump_path}.sha256"

echo "db_backup: writing $dump_path"
if ! "$PG_DUMP" \
    --format=custom \
    --no-owner --no-acl \
    --compress=6 \
    --file="$dump_path" \
    ${DATABASE_URL:+"$DATABASE_URL"}; then
    echo "db_backup: pg_dump failed" >&2
    rm -f "$dump_path"
    exit 3
fi

# SHA-256 sidecar. On macOS shasum is preinstalled; on Linux containers
# sha256sum is the default. Try both.
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$dump_path" > "$sha_path"
elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$dump_path" > "$sha_path"
else
    echo "db_backup: neither sha256sum nor shasum available" >&2
    exit 4
fi
echo "db_backup: checksum -> $sha_path"

if [[ "$RETENTION_DAYS" -gt 0 ]]; then
    echo "db_backup: pruning dumps older than ${RETENTION_DAYS} days"
    # -mindepth 1 prevents accidentally rm'ing $BACKUP_DIR itself
    find "$BACKUP_DIR" -mindepth 1 -maxdepth 1 -type f \
        \( -name "${db_label}-*.dump" -o -name "${db_label}-*.dump.sha256" \) \
        -mtime "+${RETENTION_DAYS}" -print -delete
fi

echo "db_backup: done"
