#!/usr/bin/env bash
# rotate-dev-secrets.sh — replace dev-default secrets in .env with strong randoms.
#
# What this is: a hygiene script for local / dev environments. It scans
# .env for the well-known dev placeholders shipped in .env.example
# (the `local_dev_only_change_me` strings, the `change_me_to_a_random_*`
# JWT placeholders) and replaces each with a fresh openssl-generated
# random value. A timestamped backup is written to .env.bak.<ts> first
# so the previous file is never lost.
#
# What this is NOT: a production secret rotator. Production secrets
# live in a secrets manager (Vault / AWS Secrets Manager / SOPS), get
# injected at compose-up time via env, and rotate via that manager's
# audited workflow. This script intentionally refuses to run when
# APP_ENV=production is detected, on the assumption that .env in a
# prod box is already populated by the manager and shouldn't be
# rewritten by a local-developer convenience.
#
# Usage:
#   make secret-rotate-dev          # preferred — Makefile wraps this
#   bash scripts/rotate-dev-secrets.sh
#
# Behaviour:
#   - If .env doesn't exist, copies .env.example -> .env first (so a
#     fresh clone has a usable baseline).
#   - Generates fresh values for: JWT_SECRET, MODEL_CONFIG_API_KEY_SECRET,
#     POSTGRES_PASSWORD, DB_PASSWORD, TOTP_ENCRYPTION_KEY (if currently
#     placeholder).
#   - Updates DATABASE_URL, APP_DATABASE_URL, DB_PASSWORD wherever they
#     embed the rotated POSTGRES_PASSWORD inline.
#   - Skips lines that already hold a non-placeholder value (so running
#     it twice is idempotent — the second run is a no-op).

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ENV_FILE="$ROOT_DIR/.env"
EXAMPLE_FILE="$ROOT_DIR/.env.example"

# 1. Refuse to rotate against a production .env. The hardest signal we
#    have without inspecting hostnames is APP_ENV — if it's production,
#    bail with a clear message instead of clobbering a real config.
if [ -f "$ENV_FILE" ] && grep -qE '^[[:space:]]*APP_ENV=production[[:space:]]*$' "$ENV_FILE"; then
  echo "[rotate-dev-secrets] refusing: APP_ENV=production in .env" >&2
  echo "[rotate-dev-secrets] production secrets must be rotated via your secrets manager" >&2
  exit 2
fi

# 2. Bootstrap from .env.example if there's no .env yet.
if [ ! -f "$ENV_FILE" ]; then
  if [ ! -f "$EXAMPLE_FILE" ]; then
    echo "[rotate-dev-secrets] missing both .env and .env.example" >&2
    exit 1
  fi
  echo "[rotate-dev-secrets] .env not found, bootstrapping from .env.example"
  cp "$EXAMPLE_FILE" "$ENV_FILE"
fi

# 3. Backup. We keep these forever in the working tree (the file is
#    .env.bak.* so .gitignore catches it the same way as .env).
ts="$(date +%Y%m%d-%H%M%S)"
backup="$ENV_FILE.bak.$ts"
cp "$ENV_FILE" "$backup"
echo "[rotate-dev-secrets] backed up current .env to $backup"

# 4. Generators. We pick lengths that match what each var documents:
#   - JWT / model-config: hex32 = 64 chars (= 256 bits)
#   - postgres password: hex24 = 48 chars; long enough to be unguessable
#     without overflowing typical password fields some clients impose
#   - TOTP encryption: hex32 = 64-char hex (32-byte AES-256-GCM key,
#     matches the format documented in .env.example line 99-100)
JWT_SECRET_NEW="$(openssl rand -hex 32)"
MODEL_SECRET_NEW="$(openssl rand -hex 32)"
POSTGRES_PW_NEW="$(openssl rand -hex 24)"
TOTP_KEY_NEW="$(openssl rand -hex 32)"

# 5. Replacement helper. `set_if_placeholder KEY NEW VALUE_PATTERN` updates
#    KEY=<anything matching VALUE_PATTERN> to KEY=NEW. We deliberately
#    match only known placeholder shapes so a developer who's already
#    customised their .env (set a real LLM key, etc) doesn't get
#    clobbered. Idempotent: a second run sees the new strong value,
#    no longer matches the placeholder, and skips.
#
#    sed semantics: BSD sed (macOS) needs the -i argument with an empty
#    string suffix; GNU sed accepts -i alone. We pick -i.tmp + remove
#    the tmp afterwards to be portable across both.
set_if_placeholder() {
  local key="$1" new="$2" pattern="$3"
  # Tolerant of leading whitespace so we work against .env files that
  # come from `cp .env.example .env` (no indent) AND files that some
  # editors auto-indent. Capturing group $1 preserves whatever
  # whitespace the operator had so we don't reformat the file.
  if grep -qE "^[[:space:]]*${key}=${pattern}\$" "$ENV_FILE"; then
    sed -i.tmp -E "s|^([[:space:]]*)${key}=${pattern}\$|\1${key}=${new}|" "$ENV_FILE"
    rm -f "$ENV_FILE.tmp"
    echo "  rotated: $key"
  else
    echo "  skipped (already non-placeholder): $key"
  fi
}

echo "[rotate-dev-secrets] applying new secrets:"
set_if_placeholder "JWT_SECRET" "$JWT_SECRET_NEW" \
  "change_me_to_a_random_64_char_string_min_32_chars"
set_if_placeholder "MODEL_CONFIG_API_KEY_SECRET" "$MODEL_SECRET_NEW" \
  "change_me_to_a_random_64_char_string_min_32_chars_model_cfg"
set_if_placeholder "POSTGRES_PASSWORD" "$POSTGRES_PW_NEW" \
  "local_dev_only_change_me"
set_if_placeholder "DB_PASSWORD" "$POSTGRES_PW_NEW" \
  "local_dev_only_change_me"
# TOTP_ENCRYPTION_KEY is COMMENTED in .env.example — only rotate if it's
# the bare placeholder (uncommented) form. If the operator has already
# uncommented it with a real value, skip.
set_if_placeholder "TOTP_ENCRYPTION_KEY" "$TOTP_KEY_NEW" \
  "00112233445566778899aabbccddeeff00112233445566778899aabbccddeeff"

# 6. The DATABASE_URL / APP_DATABASE_URL strings embed the postgres
#    password inline. Update those too — but only when we just rotated
#    the password (don't touch a URL the operator points at a real DB).
if [ -n "${POSTGRES_PW_NEW}" ] && grep -qE 'local_dev_only_change_me' "$ENV_FILE"; then
  sed -i.tmp -E "s|local_dev_only_change_me|${POSTGRES_PW_NEW}|g" "$ENV_FILE"
  rm -f "$ENV_FILE.tmp"
  echo "  rotated: DATABASE_URL / APP_DATABASE_URL inline password"
fi

echo "[rotate-dev-secrets] done. Old .env preserved at $backup."
echo "[rotate-dev-secrets] If postgres is already running with the OLD password, run:"
echo "  docker compose down -v   # drops the volume so postgres re-inits with the new pw"
echo "  make start"
