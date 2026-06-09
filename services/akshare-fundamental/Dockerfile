# Akshare fundamentals sidecar for the AI Fund Platform.
#
# Talks to upstream Chinese data sources (eastmoney / sina / 同花顺 / 等)
# via the `akshare` Python library and exposes a thin HTTP API matching
# the contract in server/internal/fundamental/provider_akshare.go. Keep
# the image lean — a couple of pure-Python and one C-extension dep
# (lxml / pandas), no model weights, no native compilers needed at run
# time. Build context is just this directory so cache invalidation is
# scoped to the sidecar.

FROM python:3.11-slim AS base

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1

# lxml needs libxml2 + libxslt at runtime; everything else (akshare,
# pandas, requests, beautifulsoup4) is pure Python or ships wheels.
# curl is only here so the compose healthcheck doesn't need wget.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
        libxml2 \
        libxslt1.1 \
        curl \
 && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt ./
RUN pip install -r requirements.txt

COPY server.py ./
# Ship the unit tests inside the image so we can run
#   docker compose exec akshare-fundamental python -m unittest test_name_lookup
# after every deploy without having to mount in source. Test files
# are tiny so the image overhead is negligible.
COPY test_*.py ./

# Drop privileges. We deliberately don't run as PID 1 with python
# directly because gunicorn handles SIGTERM correctly and avoids
# zombie children when akshare's HTTP clients leave threads behind.
RUN useradd --create-home --uid 10001 sidecar
USER sidecar

EXPOSE 8001

HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8001/health || exit 1

# 2 workers × 4 threads handles the typical fan-out (one consult ->
# up to ~10 master agents -> some share the same symbol so the Go
# cache absorbs most of the load anyway). Bump WEB_CONCURRENCY in
# compose if you observe queue pile-ups.
ENV WEB_CONCURRENCY=2 \
    PORT=8001

CMD ["sh", "-c", "exec gunicorn --bind 0.0.0.0:${PORT} --workers ${WEB_CONCURRENCY} --threads 4 --timeout 30 server:app"]
