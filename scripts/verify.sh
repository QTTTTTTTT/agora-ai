#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
CYAN='\033[0;36m'
NC='\033[0m'

RUN_DOCKER="${RUN_DOCKER:-0}"
RUN_E2E="${RUN_E2E:-0}"
RUN_CORE="${RUN_CORE:-1}"
RUN_RACE="${RUN_RACE:-0}"
MAX_CANDIDATE_FILE_BYTES="${MAX_CANDIDATE_FILE_BYTES:-1000000}"
HYGIENE_SCOPE="${HYGIENE_SCOPE:-candidate}"

check_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    printf "%b\n" "${RED}Error: $1 is not installed.${NC}"
    exit 1
  fi
}

step() {
  printf "\n%b\n" "${CYAN}==> $*${NC}"
}

ok() {
  printf "%b\n" "${GREEN}✓ $*${NC}"
}

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

wait_for_http_services() {
  python3 - "$@" <<'PY'
import json
import sys
import time
import urllib.request

checks = []
for raw in sys.argv[1:]:
    if "=" not in raw:
        raise SystemExit(f"Invalid health check argument {raw!r}; expected name=url")
    name, url = raw.split("=", 1)
    checks.append((name, url))

for name, url in checks:
    last_error = None
    for _ in range(60):
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                body = response.read().decode("utf-8", errors="replace")
                print(json.dumps({"service": name, "status": response.status, "body": body[:500]}))
                if response.status == 200:
                    break
        except Exception as exc:  # CI/local diagnostics
            last_error = repr(exc)
            time.sleep(2)
    else:
        raise SystemExit(f"{name} health check failed: {last_error}")
PY
}

check_cmd python3

if [ "$RUN_CORE" = "1" ]; then
  check_cmd go
  check_cmd npm
  step "Backend: tests, vet, govulncheck"
  (
    cd "$ROOT_DIR/server"
    go test ./...
    go vet ./...
    go run golang.org/x/vuln/cmd/govulncheck@latest ./...
  )
  ok "Backend gates passed"

  step "Frontend: audit, build, lint"
  (
    cd "$ROOT_DIR/web"
    npm audit
    npm run build
    npm run lint
  )
  ok "Frontend gates passed"

  step "Miniapp: static structure and syntax validation"
  (
    cd "$ROOT_DIR"
    node scripts/validate-miniapp.mjs miniapp
  )
  ok "Miniapp gates passed"

  step "API contract: frontend and miniapp route compatibility"
  (
    cd "$ROOT_DIR"
    node scripts/validate-api-contract.mjs .
  )
  ok "API contract gates passed"

  step "Observability: Prometheus alert rules"
  (
    cd "$ROOT_DIR"
    python3 scripts/validate-prometheus-alerts.py prometheus/alerts.yml
  )
  ok "Observability gates passed"

  step "Release hygiene: version metadata and git invariants"
  (
    cd "$ROOT_DIR"
    python3 scripts/validate-release-hygiene.py .
  )
  ok "Release hygiene gates passed"

  step "CI and Compose configuration"
  (
    cd "$ROOT_DIR"
    go run github.com/rhysd/actionlint/cmd/actionlint@latest .github/workflows/ci.yml
    if command -v docker >/dev/null 2>&1; then
      compose_cmd config --quiet
      POSTGRES_PASSWORD='verify-prod-db-password-change-before-release' \
        JWT_SECRET='verify-prod-jwt-secret-32-bytes-minimum' \
        MODEL_CONFIG_API_KEY_SECRET='verify-prod-model-config-secret-32-bytes' \
        CORS_ORIGINS='https://app.example.com' \
        compose_cmd -f docker-compose.yml -f docker-compose.prod.yml config --quiet
    else
      printf "%b\n" "${YELLOW}docker is not installed; skipping compose config validation.${NC}"
    fi
  )
  ok "Configuration gates passed"
else
  printf "%b\n" "${YELLOW}RUN_CORE=0; skipping backend/frontend/config gates.${NC}"
fi

if [ "$RUN_RACE" = "1" ]; then
  check_cmd go
  step "Backend: race detector tests"
  (
    cd "$ROOT_DIR/server"
    go test -race ./...
  )
  ok "Backend race detector gates passed"
fi

if command -v git >/dev/null 2>&1 && git -C "$ROOT_DIR" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  step "Commit hygiene: large files, binaries, secret patterns (${HYGIENE_SCOPE})"
  (
    cd "$ROOT_DIR"
    MAX_CANDIDATE_FILE_BYTES="$MAX_CANDIDATE_FILE_BYTES" HYGIENE_SCOPE="$HYGIENE_SCOPE" python3 - <<'PY'
import os
import re
import subprocess
from pathlib import Path

root = Path.cwd()
max_bytes = int(os.environ.get("MAX_CANDIDATE_FILE_BYTES", "1000000"))
scope = os.environ.get("HYGIENE_SCOPE", "candidate")
paths = []

if scope == "candidate":
    raw = subprocess.check_output(["git", "status", "--porcelain", "-uall", "-z"])
    for item in raw.split(b"\0"):
        if not item:
            continue
        text = item.decode("utf-8", errors="replace")
        path = text[3:]
        if " -> " in path:
            path = path.split(" -> ", 1)[1]
        paths.append(path)
elif scope == "tracked":
    raw = subprocess.check_output(["git", "ls-files", "-z"])
    paths = [item.decode("utf-8", errors="replace") for item in raw.split(b"\0") if item]
elif scope == "all":
    raw = subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"])
    paths = [item.decode("utf-8", errors="replace") for item in raw.split(b"\0") if item]
else:
    raise SystemExit(f"Unsupported HYGIENE_SCOPE={scope!r}; expected candidate, tracked, or all")

large = []
binary = []
secret_findings = []
patterns = {
    "openai_like": re.compile(r"(?<![A-Za-z0-9_-])sk-[A-Za-z0-9_-]{32,}"),
    "aws_access_key": re.compile(r"(?<![A-Z0-9])AKIA[0-9A-Z]{16}(?![A-Z0-9])"),
    "google_api_key": re.compile(r"(?<![A-Za-z0-9_-])AIza[0-9A-Za-z_-]{35}(?![A-Za-z0-9_-])"),
    "slack_token": re.compile(r"(?<![A-Za-z0-9_-])xox[baprs]-[0-9A-Za-z-]+"),
    "private_key": re.compile(r"-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----"),
}
allow_placeholder = re.compile(r"(change_me|local_dev_only|example|placeholder|your_|test_|dev-secret|dummy|mock)", re.I)

for rel in paths:
    p = root / rel
    if not p.is_file():
        continue
    try:
        data = p.read_bytes()
    except OSError:
        continue
    size = len(data)
    if size >= max_bytes:
        large.append((size, rel))
    if b"\0" in data[:4096]:
        binary.append((size, rel))
        continue
    try:
        text = data.decode("utf-8")
    except UnicodeDecodeError:
        continue
    for lineno, line in enumerate(text.splitlines(), start=1):
        if allow_placeholder.search(line):
            continue
        for name, pattern in patterns.items():
            if pattern.search(line):
                secret_findings.append((rel, lineno, name, line[:180]))

if large:
    print(f"Large candidate files >= {max_bytes} bytes:")
    for size, rel in sorted(large, reverse=True):
        print(f"  {size}\t{rel}")
if binary:
    print("Binary candidate files:")
    for size, rel in sorted(binary, reverse=True):
        print(f"  {size}\t{rel}")
if secret_findings:
    print("Potential secret findings:")
    for rel, lineno, name, line in secret_findings:
        print(f"  {rel}:{lineno}: {name}: {line}")
if large or binary or secret_findings:
    raise SystemExit(1)

print(f"hygiene_scope={scope}")
print(f"scanned_files={len(paths)}")
print("large_files=0")
print("binary_files=0")
print("potential_secret_findings=0")
PY
  )
  ok "Commit hygiene gates passed"
else
  printf "%b\n" "${YELLOW}git is not available or this is not a worktree; skipping commit hygiene scan.${NC}"
fi

if [ "$RUN_DOCKER" = "1" ]; then
  check_cmd docker
  step "Docker image builds"
  (
    cd "$ROOT_DIR"
    docker build -f Dockerfile -t fundai-simulator:verify .
    docker build -f Dockerfile.web-search-mcp -t fundai-web-search-mcp:verify .
  )
  ok "Docker image builds passed"
fi

if [ "$RUN_E2E" = "1" ]; then
  check_cmd docker
  check_cmd npm
  step "Compose smoke + Playwright E2E"
  cleanup() {
    cd "$ROOT_DIR"
    compose_cmd down -v >/dev/null 2>&1 || true
  }
  trap cleanup EXIT
  (
    cd "$ROOT_DIR"
    compose_cmd up -d --build postgres web-search-mcp app
  )
  wait_for_http_services \
    "app=http://localhost:8080/api/health" \
    "web-search-mcp=http://localhost:3004/health"
  (
    cd "$ROOT_DIR/web"
    PLAYWRIGHT_BASE_URL=http://localhost:8080 \
      PLAYWRIGHT_API_URL=http://localhost:8080 \
      PLAYWRIGHT_SKIP_WEBSERVER=1 \
      npm run test:e2e
  )
  cleanup
  trap - EXIT
  ok "Compose smoke and E2E passed"
fi

printf "\n%b\n" "${GREEN}All selected verification gates passed.${NC}"
printf "%b\n" "${CYAN}Tip: set RUN_DOCKER=1 to include Docker builds; set RUN_E2E=1 to include Compose-backed Playwright E2E.${NC}"
printf "%b\n" "${CYAN}Tip: set RUN_RACE=1 to include Go race detector tests.${NC}"
printf "%b\n" "${CYAN}Tip: set RUN_CORE=0 to run only commit hygiene unless optional Docker/E2E gates are enabled.${NC}"
printf "%b\n" "${CYAN}Tip: set HYGIENE_SCOPE=tracked in CI, or HYGIENE_SCOPE=all to scan tracked + untracked non-ignored files.${NC}"
