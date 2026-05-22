#!/usr/bin/env bash
# Rebuild the fundai-app container against the current source tree without
# touching postgres / web-search-mcp / volumes. Use after pulling a new
# branch or applying server changes so the running container picks up the
# latest code (which the F8 smoke test discovered was a real footgun: the
# old image kept serving stale endpoints for hours after merge).
#
# Idempotent and safe to run on a fresh checkout (will create the container
# on first run, recreate it on every subsequent run).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

if ! command -v docker >/dev/null 2>&1; then
  printf "%b\n" "${RED}Error: docker is not installed.${NC}"
  exit 1
fi

if docker compose version >/dev/null 2>&1; then
  COMPOSE_CMD=(docker compose)
elif command -v docker-compose >/dev/null 2>&1; then
  COMPOSE_CMD=(docker-compose)
else
  printf "%b\n" "${RED}Error: docker compose is not installed.${NC}"
  exit 1
fi

SERVICE="${1:-app}"
APP_PORT="${APP_PORT:-8080}"
HEALTH_URL="http://localhost:${APP_PORT}/api/health"

if [ -f "$ROOT_DIR/.env" ]; then
  set -a
  # shellcheck disable=SC1091
  source "$ROOT_DIR/.env"
  set +a
fi

printf "%b\n" "${CYAN}Stopping and removing the ${SERVICE} container (postgres and other services are left untouched)...${NC}"
"${COMPOSE_CMD[@]}" rm -sf "$SERVICE" >/dev/null 2>&1 || true

printf "%b\n" "${CYAN}Rebuilding ${SERVICE} image with --no-cache to guarantee fresh source...${NC}"
"${COMPOSE_CMD[@]}" build --no-cache "$SERVICE"

printf "%b\n" "${CYAN}Starting ${SERVICE} container...${NC}"
"${COMPOSE_CMD[@]}" up -d "$SERVICE"

if [ "$SERVICE" != "app" ]; then
  printf "%b\n" "${GREEN}Rebuild of ${SERVICE} complete. Skipping health probe (only wired for app).${NC}"
  exit 0
fi

printf "%b\n" "${CYAN}Waiting for health endpoint: ${HEALTH_URL}${NC}"
for _ in $(seq 1 45); do
  if curl -fsS "$HEALTH_URL" >/dev/null 2>&1; then
    printf "%b\n" "${GREEN}App is healthy and serving the freshly built image.${NC}"
    image_id=$(docker inspect --format='{{.Image}}' fundai-app 2>/dev/null || echo 'unknown')
    printf "Image ID: %s\n" "$image_id"
    printf "Health: %s\n" "$HEALTH_URL"
    exit 0
  fi
  sleep 2
done

printf "%b\n" "${YELLOW}App did not become healthy within ~90s. Inspect logs with: ${COMPOSE_CMD[*]} logs -f ${SERVICE}${NC}"
exit 1
