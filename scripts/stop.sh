#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

RED='\033[0;31m'
GREEN='\033[0;32m'
CYAN='\033[0;36m'
NC='\033[0m'

REMOVE_VOLUMES="${REMOVE_VOLUMES:-0}"

compose_cmd() {
  if docker compose version >/dev/null 2>&1; then
    docker compose "$@"
  elif command -v docker-compose >/dev/null 2>&1; then
    docker-compose "$@"
  else
    printf "%b\n" "${RED}Error: docker compose is not installed.${NC}"
    exit 1
  fi
}

printf "%b\n" "${CYAN}Stopping AI Fund Platform...${NC}"

if [ "$REMOVE_VOLUMES" = "1" ]; then
  compose_cmd down -v
  printf "%b\n" "${GREEN}✓ All services stopped and data volumes removed.${NC}"
else
  compose_cmd down
  printf "%b\n" "${GREEN}✓ All services stopped.${NC}"
  printf "\nTo also remove data volumes: REMOVE_VOLUMES=1 scripts/stop.sh\n"
fi
