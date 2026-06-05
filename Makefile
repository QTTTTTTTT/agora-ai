.PHONY: help build start stop rebuild rebuild-app test test-server test-web secret-rotate-dev perf-load-baseline ha-failover-smoke test-visual-baseline test-visual-baseline-update bundle-budget

# Default target prints help so a fresh clone explorer learns the toolbox.
help:
	@echo "AI Fund Platform — common operations"
	@echo ""
	@echo "Local development:"
	@echo "  make start              Start postgres + app + web-search-mcp via docker compose"
	@echo "  make stop               Stop the local stack"
	@echo "  make rebuild            Alias for rebuild-app (the common case)"
	@echo "  make rebuild-app        Rebuild fundai-app image and recreate container (no postgres bounce)"
	@echo "  make secret-rotate-dev  Replace dev placeholder secrets in .env with strong randoms"
	@echo ""
	@echo "Build artifacts (no docker):"
	@echo "  make build              Build the server binary into server/bin/fundai-server"
	@echo ""
	@echo "Tests:"
	@echo "  make test               Run server + web unit tests"
	@echo "  make test-server        go test ./... in server/"
	@echo "  make test-web           npm test in web/"
	@echo ""
	@echo "Performance:"
	@echo "  make perf-load-baseline Run 30s load against /api/health and capture p50/p95/p99 + 5xx rate"
	@echo "  make bundle-budget      Enforce frontend bundle size caps against web/bundle-budget.json"
	@echo ""
	@echo "Reliability:"
	@echo "  make ha-failover-smoke  Kill app + bounce postgres, verify recovery within 60s"
	@echo ""
	@echo "Visual regression (Playwright):"
	@echo "  make test-visual-baseline         Compare auth-front routes against committed screenshots"
	@echo "  make test-visual-baseline-update  Refresh baselines after an intended visual change"

start:
	scripts/start.sh

stop:
	scripts/stop.sh

rebuild: rebuild-app

rebuild-app:
	scripts/rebuild-app.sh app

build:
	cd server && go build -o bin/fundai-server ./cmd/server

test: test-server test-web

test-server:
	cd server && go test ./...

test-web:
	cd web && npm test --silent --if-present

# Replaces the well-known dev placeholders in .env with fresh
# openssl-generated values (JWT_SECRET, MODEL_CONFIG_API_KEY_SECRET,
# POSTGRES_PASSWORD, TOTP_ENCRYPTION_KEY, plus inline DATABASE_URL
# updates). Idempotent — running it twice is a no-op because the
# placeholders no longer match. Refuses to run when APP_ENV=production
# is detected; production secrets must rotate via the secrets manager,
# not this convenience script. Backs up the previous .env to
# .env.bak.<timestamp> before mutating.
secret-rotate-dev:
	bash scripts/rotate-dev-secrets.sh

# Companion to scripts/perf-baseline.sh — *active* load baseline.
# perf-baseline.sh samples /api/metrics passively, which only sees
# numbers if the server is being driven by traffic right then. This
# target runs a 30s burst against /api/health (cheapest endpoint, so
# we measure the HTTP stack, not business logic) and records both
# client- and server-side latency / 5xx so a regression > 2x shows
# up release-over-release in docs/perf-load-history.csv.
perf-load-baseline:
	bash scripts/perf-load-baseline.sh

# Smoke version of the M2 HA-failover validation. Crashes the app
# container (expects `restart: unless-stopped` to recover it) and
# bounces postgres (expects the app's connection pool to retry).
# Requires the local stack already running via `make start`. This
# is the floor — a real chaos pipeline still needs to add network
# partitions, multi-replica failover, etc. (see scripts/ha-failover-smoke.sh
# for the full caveat list).
ha-failover-smoke:
	bash scripts/ha-failover-smoke.sh

# U8 visual regression. Compares pixel screenshots of the
# unauthenticated auth/landing routes (login, register tab,
# forgot-password, reset-password) against committed baselines.
# Catches stray padding regressions / accidental Tailwind purges
# / dropped marketing copy that unit tests miss. First run on a
# new machine creates the baselines automatically — review the
# generated PNGs and `git add` them to commit. After an
# intended visual change, run `make test-visual-baseline-update`
# to refresh.
test-visual-baseline:
	cd web && npm run test:e2e:visual

test-visual-baseline-update:
	cd web && npm run test:e2e:visual:update

# Enforce frontend bundle size budgets. Expects web/dist to exist
# (run `cd web && npm run build` first). CI invokes the same script.
bundle-budget:
	bash scripts/bundle-budget.sh
