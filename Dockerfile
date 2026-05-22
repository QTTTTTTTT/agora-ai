# =============================================================================
# Dockerfile — AI Fund Company Simulator
# Multi-stage build: React frontend + Go backend → minimal production image
# =============================================================================
#
# .dockerignore should contain:
#   .git/
#   .github/
#   .vscode/
#   .idea/
#   *.md
#   docker-compose*.yml
#   .env*
#   server/tmp/
#   web/node_modules/
#   web/dist/
#   **/*_test.go
#   **/.*

# ---------------------------------------------------------------------------
# Stage 1: Build React frontend
# ---------------------------------------------------------------------------
FROM node:22-alpine AS frontend-builder

WORKDIR /app/web
COPY web/package*.json ./
RUN npm ci --ignore-scripts && npm cache clean --force
COPY web/ ./
RUN npm run build

# ---------------------------------------------------------------------------
# Stage 2: Build Go backend
# ---------------------------------------------------------------------------
FROM golang:1.25.10-alpine AS backend-builder

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown

WORKDIR /app
COPY server/go.mod server/go.sum ./server/

WORKDIR /app/server
RUN go mod download && go mod verify

COPY server/ ./
RUN BUILD_TIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)" && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${BUILD_VERSION} -X main.buildTime=${BUILD_TIME} -X main.buildCommit=${BUILD_COMMIT}" \
    -trimpath \
    -o /fundai-server \
    ./cmd/server

# ---------------------------------------------------------------------------
# Stage 3: Production image
# ---------------------------------------------------------------------------
FROM alpine:3.19

ARG BUILD_VERSION=dev
ARG BUILD_COMMIT=unknown
ARG BUILD_TIME=unknown

LABEL maintainer="FundAI Team <team@fundai.dev>"
LABEL version="${BUILD_VERSION}"
LABEL description="AI Fund Company Simulator — intelligent fund management platform with AI-driven trading strategies"
LABEL org.opencontainers.image.source="https://github.com/fundai/simulator"
LABEL org.opencontainers.image.licenses="MIT"
LABEL org.opencontainers.image.version="${BUILD_VERSION}"
LABEL org.opencontainers.image.revision="${BUILD_COMMIT}"
LABEL org.opencontainers.image.created="${BUILD_TIME}"

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    && update-ca-certificates

# Create non-root user and group
RUN addgroup -S fundai && adduser -S fundai -G fundai

WORKDIR /app

# Copy built artifacts
COPY --from=backend-builder /fundai-server ./server
COPY --from=frontend-builder /app/web/dist ./web/dist
COPY server/migrations ./migrations

# Create data directory for any local state
RUN mkdir -p /app/data && chown -R fundai:fundai /app

# Switch to non-root user
USER fundai

# Runtime configuration
ENV TZ=Asia/Shanghai
ENV APP_ENV=production
ENV APP_PORT=8080
ENV LOG_LEVEL=info
ENV MIGRATIONS_PATH=/app/migrations
ENV STATIC_FILES_PATH=/app/web/dist

EXPOSE 8080

# Health check — hit the health endpoint every 30s
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://localhost:8080/api/health || exit 1

ENTRYPOINT ["./server"]
