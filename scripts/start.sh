#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

check_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf "%b\n" "${RED}Error: $1 is not installed.${NC}"
    exit 1
  fi
}

printf "%b\n" "${CYAN}Starting AI Fund Platform local bootstrap...${NC}"

check_cmd docker
check_cmd curl

if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
else
  printf "%b\n" "${RED}Error: docker compose is not installed.${NC}"
  exit 1
fi

CLI_APP_ENV="${APP_ENV:-}"

if [ ! -f "$ROOT_DIR/.env" ]; then
  printf "%b\n" "${YELLOW}No .env found. Copying the local development template from .env.example.${NC}"
  cp "$ROOT_DIR/.env.example" "$ROOT_DIR/.env"
  printf "%b\n" "${YELLOW}.env.example is for local development only. Do not use it as a production secret file.${NC}"
fi

set -a
source "$ROOT_DIR/.env"
set +a

if [ -n "$CLI_APP_ENV" ]; then
  APP_ENV="$CLI_APP_ENV"
fi
APP_ENV="${APP_ENV:-development}"
APP_ENV_NORMALIZED="$(printf '%s' "$APP_ENV" | tr '[:upper:]' '[:lower:]')"
if [ "$APP_ENV_NORMALIZED" = "production" ] || [ "$APP_ENV_NORMALIZED" = "prod" ]; then
  printf "%b\n" "${RED}Error: scripts/start.sh is only for local development and acceptance. Use an explicit production deployment flow instead.${NC}"
  exit 1
fi

APP_PORT="${APP_PORT:-8080}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
POSTGRES_DB="${POSTGRES_DB:-fundai}"
POSTGRES_USER="${POSTGRES_USER:-fundai}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-local_dev_only_change_me}"
APP_DATABASE_SSLMODE="${APP_DATABASE_SSLMODE:-disable}"
HEALTH_URL="http://localhost:${APP_PORT}/api/health"
CONTAINER_DATABASE_URL="${APP_DATABASE_URL:-postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=${APP_DATABASE_SSLMODE}}"

if [ -z "${JWT_SECRET:-}" ] || [ "${JWT_SECRET:-}" = "change_me_to_a_random_64_char_string_min_32_chars" ]; then
  printf "%b\n" "${YELLOW}Warning: JWT_SECRET is still using a template value. That is acceptable only for local development.${NC}"
fi
if [ -z "${MODEL_CONFIG_API_KEY_SECRET:-}" ] || [ "${MODEL_CONFIG_API_KEY_SECRET:-}" = "change_me_to_a_random_64_char_string_min_32_chars_model_cfg" ]; then
  printf "%b\n" "${YELLOW}Warning: MODEL_CONFIG_API_KEY_SECRET is still using a template value. That is acceptable only for local development.${NC}"
fi

printf "%b\n" "${CYAN}Starting PostgreSQL for local development...${NC}"
"${COMPOSE_CMD[@]}" up -d postgres

printf "%b\n" "${CYAN}Starting local web-search MCP and application containers against the compose PostgreSQL service...${NC}"
printf "%b\n" "${CYAN}Using local database target: postgres:5432/${POSTGRES_DB}${NC}"
APP_DATABASE_URL="$CONTAINER_DATABASE_URL" "${COMPOSE_CMD[@]}" up -d --build web-search-mcp app

printf "%b\n" "${CYAN}Waiting for health endpoint: ${HEALTH_URL}${NC}"
for _ in $(seq 1 45); do
  if curl -fsS "$HEALTH_URL" >/dev/null 2>&1; then
    printf "%b\n" "${GREEN}App is healthy and ready.${NC}"
    printf "Web App: http://localhost:%s\n" "$APP_PORT"
    printf "API Health: %s\n" "$HEALTH_URL"
    printf "PostgreSQL host: localhost:%s\n" "$POSTGRES_PORT"
    printf "PostgreSQL database: %s\n" "$POSTGRES_DB"
    exit 0
  fi
  sleep 2
done

printf "%b\n" "${YELLOW}App is still starting or unhealthy. Check logs with: ${COMPOSE_CMD[*]} logs -f postgres app${NC}"
exit 1
