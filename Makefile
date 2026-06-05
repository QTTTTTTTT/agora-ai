.PHONY: help build start stop rebuild rebuild-app test test-server test-web secret-rotate-dev

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
