.PHONY: help build start stop rebuild rebuild-app test test-server test-web

# Default target prints help so a fresh clone explorer learns the toolbox.
help:
	@echo "AI Fund Platform — common operations"
	@echo ""
	@echo "Local development:"
	@echo "  make start           Start postgres + app + web-search-mcp via docker compose"
	@echo "  make stop            Stop the local stack"
	@echo "  make rebuild         Alias for rebuild-app (the common case)"
	@echo "  make rebuild-app     Rebuild fundai-app image and recreate container (no postgres bounce)"
	@echo ""
	@echo "Build artifacts (no docker):"
	@echo "  make build           Build the server binary into server/bin/fundai-server"
	@echo ""
	@echo "Tests:"
	@echo "  make test            Run server + web unit tests"
	@echo "  make test-server     go test ./... in server/"
	@echo "  make test-web        npm test in web/"

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
