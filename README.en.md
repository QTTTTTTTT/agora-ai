# AI Fund Platform — current repository implementation notes

[简体中文](./README.md) · **English**

> A fund-company / fund-management prototype repository. The backend
> capabilities that are actually wired today are: subscription, model
> config, usage / billing, the minimal company / fund CRUD, and the
> minimal plan approval flow. The team, trading, workflow, memory and
> A/B testing surfaces still contain placeholders and partially-wired
> code paths — they need additional resilience, observability and
> test coverage before they can be considered production-grade.

## Implementation overview

```
┌─────────────────────────────────────────────────────────┐
│                     Web / Miniapp                       │
│  React SPA routes │ WeChat miniapp mock pages          │
├─────────────────────────────────────────────────────────┤
│                    Go REST API                          │
│  Health/Version │ Subscription │ Models │ Usage         │
│  Company/Fund CRUD │ SPA Fallback │ CORS                │
├─────────────────────────────────────────────────────────┤
│               PostgreSQL 16 + migrations                │
├─────────────────────────────────────────────────────────┤
│         Optional MCP containers via docker compose      │
│  china-stock / akshare always on; others via profile    │
└─────────────────────────────────────────────────────────┘
```

## What actually works today

| Module | Current state |
|------|-----------|
| **Subscription plans** | Wired — list plans, subscribe, cancel, query current subscription state |
| **Model configuration** | Wired — list platform models, save/delete user model configs, test connections |
| **Usage & billing** | Wired — today's usage, monthly aggregate, history, monthly bill, cost estimate |
| **Fund company / fund** | Wired — minimal CRUD for both company and fund |
| **Plan approval** | Minimal wiring — list plans, fetch detail, execute approve / reject state transitions |
| **React frontend** | Page scaffolding and routes exist for companies, fund dashboard, team, decisions, compare, memory, trades, settings, subscription, models, usage |
| **WeChat miniapp** | Page shells and mock data structures exist, but the README does NOT count it as a complete business surface |
| **fund team / trade / workflow / memory / abtest** | Routes partially exist but still contain placeholder implementations, partial wiring, or demo-level behaviour — do not treat them as production-complete capabilities |

## Tech stack

| Layer | Tech |
|------|------|
| Frontend | React 18 + TypeScript + Vite + TailwindCSS + Recharts |
| Miniapp | Native WeChat miniapp project (`miniapp/`) |
| Backend | Go 1.22 + `net/http` method-aware routing |
| Database | PostgreSQL 16 + SQL migrations |
| Deployment | Docker + Docker Compose + multi-stage build |
| Model integration | OpenAI-compatible config + platform models / user-defined model configs |

## Quick start

### Prerequisites

- [Docker](https://docs.docker.com/get-docker/) 20.10+
- [Docker Compose](https://docs.docker.com/compose/install/) v2+

### One-shot startup (local dev / acceptance only)

```bash
# 1. Clone
git clone <your-repo-url> ai-fund-platform-v3-full
cd ai-fund-platform-v3-full

# 2. One-shot local startup (auto-copies .env.example → .env if missing)
chmod +x scripts/start.sh
./scripts/start.sh
```

`scripts/start.sh` is for local dev / acceptance only: it starts PostgreSQL,
then `web-search-mcp` + `app`, and waits for `GET /api/health` to succeed.
The script will refuse to run when `APP_ENV=production`. Default URLs:

- **Web app / SPA**: http://localhost:8080
- **Health check**: http://localhost:8080/api/health
- **Version info**: http://localhost:8080/api/version
- **Local web-search MCP**: http://localhost:3004/health

### Manual startup (Docker, local dev / acceptance)

```bash
# 1. Create env config
cp .env.example .env
# Only 4 things are mandatory (see ".env.example configuration guide" below):
#   DATABASE_URL / JWT_SECRET / MODEL_CONFIG_API_KEY_SECRET
#   + LLM_PROVIDER + LLM_MODEL + LLM_API_KEY (or any provider alias)
# Strongly recommended: at least one market-data source + one news source
# When promoting to production, also replace CORS_ORIGINS / APP_PUBLIC_URL /
# APP_ENV / APP_DATABASE_SSLMODE.

# 2. Start PostgreSQL first so the DB is reproducible
docker compose up -d postgres

# 3. Bring up the local acceptance stack (app + postgres + bundled web-search MCP)
docker compose up -d --build app web-search-mcp

# 4. Optionally add more market-data MCPs via profile
docker compose --profile market-data up -d

# 5. Tail logs
docker compose logs -f postgres app

# 6. Stop everything
docker compose down
```

Notes:
- `docker compose up -d postgres` is the minimum-reproducible DB startup, useful when "backend runs locally but the host has no PostgreSQL"
- `docker compose up -d --build app web-search-mcp` brings up `app`, `postgres`, and the bundled `web-search-mcp` — this is the current minimum-reproducible container acceptance path
- `docker compose --profile market-data up -d` additionally starts `china-stock-mcp` and `akshare-mcp`
- `ta-lib-mcp` is still gated behind `--profile professional`
- PostgreSQL on first boot auto-runs `./scripts/init-db.sql`

### Local development (backend without Docker, DB in Docker)

```bash
# 1. Start the local PostgreSQL container
cp .env.example .env
docker compose up -d postgres

# 2. Frontend dev server
cd web
npm install
npm run dev

# 3. Backend dev server (reusing the compose PostgreSQL)
cd ../server
export DATABASE_URL="postgres://fundai:local_dev_only_change_me@localhost:5432/fundai?sslmode=disable"
go run ./cmd/server
```

Common URLs in local dev:
- Frontend Vite: http://localhost:5173
- Backend API: http://localhost:8080
- Local PostgreSQL: localhost:5432
- SPA home page (when statically served by the backend): http://localhost:8080/

## Recommended configuration path

The default flow is env-first: copy `.env.example` to `.env`, fill in the
database, one default LLM, and at least one usable market-data / news source,
and you can start. If a team agent has no per-agent model configured in the
database, it inherits in this order:

### Native Gemini provider configuration

The repository now supports `LLM_PROVIDER=gemini`, which uses Gemini's native
`generateContent` protocol — not OpenAI's `/chat/completions`.

Recommended form:

```env
LLM_PROVIDER=gemini
LLM_MODEL=
LLM_BASE_URL=
LLM_API_KEY=

GEMINI_MODEL=gemini-3.1-pro-preview
GEMINI_BASE_URL=https://generativelanguage.googleapis.com/v1beta
GEMINI_API_KEY=your_gemini_key
```

Or set it directly on the global `LLM_*` keys:

```env
LLM_PROVIDER=gemini
LLM_MODEL=gemini-3.1-pro-preview
LLM_BASE_URL=https://generativelanguage.googleapis.com/v1beta
LLM_API_KEY=your_gemini_key
```

Notes:
- Both `gemini` and `google` normalise to the Gemini provider
- When `LLM_PROVIDER=gemini`, the base URL must end in `.../v1beta`
- Final request path is `/v1beta/models/{model}:generateContent`
- `custom` still means "OpenAI-compatible custom endpoint" — do NOT
  use it for the native Gemini URL

1. Model explicitly specified by the request
2. Per-agent model configuration
3. User tier override
4. Tier default in `.env`; if that tier isn't set, falls back to global `LLM_*`
5. Hardcoded in-code default

The TeamManagement / ModelConfig pages remain available for advanced overrides
but are no longer required for a basic startup.

## Pre-production checklist

The repository's `docker-compose.yml`, `.env.example` and `scripts/start.sh`
should all be treated as local dev / acceptance entrypoints — NOT as a
production deployment manifest. A production environment must at minimum:

- Set `APP_ENV=production`
- Explicitly provide `DATABASE_URL`; do not rely on the legacy `DB_*` fallback
- `DATABASE_URL` must not point to `localhost` / `127.0.0.1`
- `DATABASE_URL` must not use `sslmode=disable`
- `DATABASE_URL` must not continue to use demo / placeholder credentials
- `JWT_SECRET` must be a strong random value of at least 32 characters
- `MODEL_CONFIG_API_KEY_SECRET` must be an independent strong random value,
  different from `JWT_SECRET`
- `CORS_ORIGINS` must be replaced with real HTTPS origins — no `*`, no
  `localhost`, no `127.0.0.1`
- Do not use `scripts/start.sh` for production deployment
- Do not commit real `.env`, cloud secrets, or database credentials to the repo

We recommend running a minimal static-validation pass before release:

```bash
APP_ENV=production \
JWT_SECRET='<32+ chars random secret>' \
MODEL_CONFIG_API_KEY_SECRET='<another 32+ chars random secret>' \
DATABASE_URL='postgres://user:pass@db.example.com:5432/fundai?sslmode=require' \
CORS_ORIGINS='https://app.example.com' \
go run ./server/cmd/server
```

If the configuration itself is safe, the service will not exit at the
config-validation stage; later, if the database is unreachable, that will
surface as a connection error rather than a config error.

## Release checklist

- Backend tests pass: `go test ./...`
- Compose still resolves: `docker compose config`
- Frontend builds: `npm --prefix web run build`
- After startup, `GET /api/health`, `GET /api/version`, `GET /api/metrics`
  are reachable
- Check logs and startup output to confirm no database connection strings,
  passwords, or third-party model API keys are exposed
- If `MODEL_CONFIG_API_KEY_SECRET` is rotated, ops must re-enter previously
  stored user model API keys

## Project structure

```text
ai-fund-platform-v3-full/
├── Dockerfile                    # Multi-stage build (web + server)
├── docker-compose.yml            # app / postgres / MCP orchestration
├── .env.example                  # Environment-variable template
├── scripts/
│   ├── init-db.sql               # Initialisation SQL (existing file)
│   ├── start.sh                  # One-shot startup script
│   └── stop.sh                   # Stop compose services
├── server/                       # Go backend
│   ├── cmd/server/main.go        # Entry, health check, version, SPA fallback
│   ├── cmd/server/wiring_adapters.go
│   ├── migrations/
│   │   ├── 001_init.sql
│   │   └── 002_subscription_and_models.sql
│   └── internal/
│       ├── api/
│       │   ├── fund_handler.go           # company/fund + placeholder routes
│       │   └── subscription_handler.go   # subscription/models/usage API
│       ├── repository/
│       ├── subscription/
│       ├── llm/
│       ├── workflow/
│       ├── agent/
│       └── abtest/
├── web/                          # React frontend
│   ├── package.json
│   └── src/
│       ├── App.tsx               # companies and funds/* routes
│       ├── components/
│       └── pages/
│           ├── Dashboard.tsx
│           ├── TeamManagement.tsx
│           ├── DecisionCenter.tsx
│           ├── ABTestCompare.tsx
│           ├── MemoryCenter.tsx
│           ├── Subscription.tsx
│           ├── ModelConfig.tsx
│           └── Usage.tsx
└── miniapp/                      # WeChat miniapp scaffolding
```

## Docker architecture

```text
┌───────────────────────────────────────┐
│          docker compose               │
├───────────────┬───────────────────────┤
│   postgres    │        app            │
│  PostgreSQL   │  Go API + React SPA   │
│  Port: 5432   │  Port: 8080           │
├───────────────┴───────────────────────┤
│              app-net                  │
├───────────────────────────────────────┤
│              mcp-net                  │
├─────────────┬─────────────┬───────────┤
│ china-stock │ akshare-mcp │ optional  │
│ always on   │ always on   │ P1 MCPs   │
└─────────────┴─────────────┴───────────┘
```

**Matching the actual files:**
- The `app` image comes from the root `Dockerfile` and serves the Go API
  + React static files at runtime
- `postgres` is PostgreSQL 16 Alpine
- `web-search-mcp` is built locally from `Dockerfile.web-search-mcp` and
  defaults to exposing `localhost:3004`
- `china-stock-mcp` and `akshare-mcp` require `docker compose --profile market-data up -d`
- `ta-lib-mcp` requires `docker compose --profile professional up -d`

## API endpoints

### 1) Wired and ready

#### Basic / meta

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/health` | Health check — `status / time / version` |
| GET | `/api/version` | Version, build time, Go version |

#### Subscription / Models / Usage

These endpoints rely on the `X-User-ID` request header.

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/plans` | List subscription plans |
| GET | `/api/subscription` | Get current user's subscription + active plan |
| POST | `/api/subscription` | Create / change subscription |
| DELETE | `/api/subscription` | Cancel subscription |
| GET | `/api/models` | List platform + user-defined models |
| GET | `/api/models/config` | Get user model config |
| POST | `/api/models/config` | Save model config |
| DELETE | `/api/models/config/{configId}` | Delete model config |
| POST | `/api/models/test` | Test model connection |
| GET | `/api/usage/today` | Today's usage summary |
| GET | `/api/usage/monthly?month=YYYY-MM` | Monthly usage |
| GET | `/api/usage/history?offset=0&limit=20` | Paginated usage history |
| GET | `/api/usage/bill?month=YYYY-MM` | Monthly bill |
| GET | `/api/usage/estimate` | Current month cost estimate |

#### Company / Fund minimal CRUD

`POST /api/companies` and `GET /api/companies` also rely on `X-User-ID`.

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/companies` | Create fund company |
| GET | `/api/companies` | List the current user's fund companies |
| POST | `/api/companies/{companyId}/funds` | Create fund |
| GET | `/api/companies/{companyId}/funds` | List funds under a company |
| GET | `/api/funds/{fundId}` | Fund detail |
| PUT | `/api/funds/{fundId}` | Update fund basics |
| DELETE | `/api/funds/{fundId}` | Delete fund |

> Body schemas are strict (`DisallowUnknownFields`). **`POST /api/companies` only accepts** `{"name": string, "description"?: string}`; extra fields (e.g. `headquarters`, `region`) return 400. **`POST /api/companies/{companyId}/funds` accepts** `{"name", "description"?, "tradingMode" (live/simulation/paper), "initialCapital", "market"?, "exchange"?, "assetClass"?, "baseCurrency"?, "benchmarkSymbol"?, "primaryDirection"?, "calendarCode"?, "timeZone"?, "universe"?, "teamIntervals"?, "specialization"?, "hardRisk"?}`. When any of `market` / `exchange` / `assetClass` is crypto / BINANCE / COINBASE, the calendar automatically routes to `CRYPTO-24X7` + UTC (see F11.1). When `benchmarkSymbol` is omitted, the default per market is `BTC-USD` (crypto) / `SPY` (us_equity) / `000300.SS` (a_share) / `ES=F` (futures); the fund name is no longer truncated into a ticker (F11.2).

#### Plan minimal wiring

These plan endpoints are wired through to the real repository — list a fund's
plans, fetch a single plan, and execute minimal state transitions
(`pending -> approved/rejected`):

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/funds/{fundId}/plans` | List a fund's plans |
| GET | `/api/plans/{planId}` | Plan detail |
| POST | `/api/plans/{planId}/approve` | Approve plan |
| POST | `/api/plans/{planId}/reject` | Reject plan |

### 2) Routes reserved but not yet production-grade

The following paths are defined in `fund_handler.go` but are still in
placeholder / partial-wiring / not-yet-production-hardened state; some
explicitly return `501 not implemented`, others have a frontend page or a
minimal data path but should not be treated as complete capabilities:

| Method | Path |
|--------|------|
| POST | `/api/funds/{fundId}/team` |
| GET | `/api/funds/{fundId}/team` |
| PUT | `/api/funds/{fundId}/team/{agentId}` |
| DELETE | `/api/funds/{fundId}/team/{agentId}` |
| GET | `/api/funds/{fundId}/trades` |
| GET | `/api/funds/{fundId}/portfolio` |
| GET | `/api/funds/{fundId}/nav` |
| POST | `/api/funds/{fundId}/workflow/start` |
| POST | `/api/funds/{fundId}/workflow/step` |
| GET | `/api/funds/{fundId}/workflow/status` |
| GET | `/api/funds/{fundId}/memory` |
| GET | `/api/funds/{fundId}/memory/search` |
| GET | `/api/funds/{fundId}/reflections` |
| GET | `/api/agents/{agentId}/skills` |
| POST | `/api/agents/{agentId}/skills/{skillKey}/approve` |
| DELETE | `/api/agents/{agentId}/skills/{skillKey}` |
| POST | `/api/abtests` |
| GET | `/api/abtests/{testId}` |
| POST | `/api/abtests/{testId}/start` |
| POST | `/api/abtests/{testId}/stop` |
| POST | `/api/abtests/{testId}/analyze` |

Note: common older README snippets like `/api/funds/:id/plans/:planId/approve`
do not match the current code; the path actually registered is
`/api/plans/{planId}/approve`.

## Frontend routes

### React SPA

The backend falls non-API paths back to `web/dist/index.html`, so in production
the Go service serves the frontend. The main routes visible in
`web/src/App.tsx`:

| Path | Description |
|------|-------------|
| `/` | Redirects to `/companies` |
| `/companies` | Company list page |
| `/funds/:fundId` | Fund dashboard home |
| `/funds/:fundId/team` | Team page (frontend exists, backend not fully wired) |
| `/funds/:fundId/decisions` | Decisions page |
| `/funds/:fundId/compare` | A/B compare page |
| `/funds/:fundId/memory` | Memory page |
| `/funds/:fundId/trades` | Trades page |
| `/funds/:fundId/settings` | Fund settings page |
| `/funds/:fundId/subscription` | Subscription page |
| `/funds/:fundId/models` | Model config page |
| `/funds/:fundId/usage` | Usage page |

### WeChat miniapp

The `miniapp/` directory contains the home, team, decisions, memory, and
"more" pages, plus subscription / model-config / usage sub-package scaffolding;
this README only acknowledges its existence — it is not described as a fully
backend-integrated delivery surface.

## .env.example configuration guide

`.env.example` is the single source of truth — every environment variable the
backend actually reads via `os.Getenv()` is grouped in that file, with a
short English/Chinese header per section explaining **when** you need it,
**why** you need it, and **how** to fill it in. **Copy to `.env` and edit
there; never commit real secrets to the repo.**

The file currently has 15 sections, matching the overview table and the
per-section walkthrough below. If you just want it to boot, fill in the
"essential four" — every other section is optional with a fail-safe
fallback (either a platform default or a soft-disable of the feature).

### The essential four (minimum runnable set)

| Variable | Purpose | What happens if missing |
|------|------|------------|
| `DATABASE_URL` | PostgreSQL connection string (containers use `APP_DATABASE_URL`) | Service exits |
| `JWT_SECRET` | Signs login tokens; ≥ 32-char random string | Startup validation fails; production refuses to start |
| `MODEL_CONFIG_API_KEY_SECRET` | Encrypts third-party model keys users store in the DB; must differ from `JWT_SECRET` | Saving a user model API key returns 500 |
| `LLM_PROVIDER` / `LLM_MODEL` / `LLM_API_KEY` | Default LLM entry (or set one provider alias such as `OPENAI_*`) | Every agent call falls back to the hardcoded default and output quality collapses |

> You can still boot without `MARKETDATA_*`: the market-data chain has public
> fallbacks built in; only "self-hosted upstreams" like the A-share MCP or
> QuantDinger need an explicit URL.

### Section overview

| # | Section | Key variables | Required? |
|---|------|----------|--------|
| 1  | Application runtime | `APP_ENV`, `APP_PORT`, `LOG_LEVEL`, `MIGRATIONS_PATH`, `STATIC_FILES_PATH`, `SESSION_TTL`, `ALLOW_INTERNAL_COMPOSE_DB` | ✗ (but `APP_ENV` decides production validation) |
| 2  | PostgreSQL | `DATABASE_URL` (+ container `APP_DATABASE_URL`, `APP_DATABASE_SSLMODE`), legacy `DB_*` fallbacks, pool `DB_MAX_*` | ✓ `DATABASE_URL` |
| 3  | Security / secrets | `JWT_SECRET`, `JWT_SECRETS_JSON` (multi-key rotation), `MODEL_CONFIG_API_KEY_SECRET`, `CORS_ORIGINS`, `API_KEY_ENCRYPTION_SECRET` (legacy alias) | ✓ JWT + Model secret |
| 4  | Public site URL / branding | `APP_PUBLIC_URL`, `BRAND_NAME` | ✗ but production must pin the correct domain (affects reset/verify email links) |
| 5  | SMTP / transactional email | `SMTP_HOST/PORT/USERNAME/PASSWORD/FROM/FROM_NAME`, `SMTP_USE_TLS/STARTTLS/TIMEOUT` | ✗ (empty `SMTP_HOST` falls back to in-memory recorder; bodies echo into the JSON response) |
| 6  | WeChat miniapp login | `WECHAT_MINIAPP_APPID/SECRET`, `WECHAT_JSCODE_SESSION_URL`, `WECHAT_LOGIN_TIMEOUT` | ✗ (unconfigured → `/api/auth/wechat-login` returns 503 and the miniapp falls back to email login) |
| 7  | Default LLM | `LLM_PROVIDER/MODEL/BASE_URL/API_KEY` + tier overrides `LLM_CRITICAL_*` / `LLM_STANDARD_*` / `LLM_SIMPLE_*` | ✓ (or use the provider aliases from section 8) |
| 8  | Provider native aliases | `OPENAI_*`, `CLAUDE_*` / `ANTHROPIC_*`, `DEEPSEEK_*`, `QWEN_*`, `GEMINI_*` / `GOOGLE_*` | ✗ (only fallback when `LLM_*` is missing pieces) |
| 9  | L3 memory embedding | `RECALL_OPENAI_API_KEY/BASE_URL`, `RECALL_EMBED_MODEL` | ✗ empty → backfill worker doesn't start (feature soft-off) |
| 10 | Market-data chain | `MARKETDATA_QUOTE_PROVIDERS` + per-market `*_CNSTOCK/USSTOCK/HKSTOCK/FUTURES/CRYPTO`, `QUANTDINGER_URL`, `MCP_*_URL`, `BINANCE_OHLC_URL`, `OHLC_/FUNDAMENTAL_/SECTORFLOW_CACHE_TTL`, `YAHOO_EARNINGS_*`, `MCP_WEB_SEARCH_URL`, `MARKETDATA_COINGECKO_BASE_URL` | ✗ (public chain covers basics, but A-share / futures strongly benefit from running an MCP) |
| 11 | Crypto WebSocket | `MARKETDATA_CRYPTO_WS_ENABLED`, `MARKETDATA_BINANCE_WS_*`, `MARKETDATA_COINBASE_WS_*`, `MARKETDATA_CRYPTO_WS_STALE_AFTER` | ✗ (when off, falls back to CoinGecko/Yahoo polling — the 30 req/min quota becomes the bottleneck) |
| 12 | News providers | `MARKETDATA_NEWS_PROVIDERS`, `MARKETDATA_NEWS_HYBRID`, `EASTMONEY/SINA_NEWS_BASE_URL`, `SERPAPI_KEYS`, `TAVILY_API_KEYS` (+ `*_BASE_URL`) | ✗ (A-share auto-gets eastmoney + sina; other markets really should add SerpAPI / Tavily) |
| 13 | News translation (optional) | `MARKETDATA_TRANSLATOR_PROVIDER/BASE_URL/API_KEY/MODEL/TIMEOUT` | ✗ defaults to `none` |
| 14 | Market-data resilience / cache / risk | `MARKETDATA_STALE_AFTER`, `MARKETDATA_CIRCUIT_FAILURES/COOLDOWN`, `MARKETDATA_THROTTLE_COOLDOWN`, `MARKETDATA_ADAPTIVE_TTL`, `MARKETDATA_ADAPTIVE_QUOTE_TTL`, `MARKETDATA_QUOTE_TTL[_INSESSION/_OFFSESSION]`, `MARKETDATA_NEWS_TTL`, `MARKETDATA_PROVIDER_TIMEOUT`, `MARKETDATA_QUOTE_RATE_LIMITS` | ✗ (defaults are the production-recommended baseline) |
| 15 | Feature flags / debug | `FUND_DEBATE_ROUNDTABLE`, `BACKTEST_DISABLED` | ✗ (toggles a feature off — only needed in special scenarios) |

### Per-section walkthrough

> Each section here only explains "why it's designed this way" and "the
> pitfalls". The full variable descriptions, default values, and legal
> formats live under the same section heading in [`.env.example`](.env.example).

#### ① Application runtime

- `APP_ENV=production` triggers extra startup validation: DB must be remote
  + SSL, JWT / Model secrets must be strong-random and distinct,
  `CORS_ORIGINS` must not contain `*` / localhost. Dev mode is permissive.
- `MIGRATIONS_PATH` / `STATIC_FILES_PATH` also accept the legacy names
  `MIGRATIONS_DIR` / `STATIC_DIR` for back-compat with older deploy scripts.
- `ALLOW_INTERNAL_COMPOSE_DB=1` together with `RUNNING_IN_CONTAINER=1`
  allows `DATABASE_URL` to point at the compose-internal host name
  `postgres`. Never enable this in real production.

#### ② Database

- Priority: `DATABASE_URL` > container `APP_DATABASE_URL` > legacy `DB_*`
  concatenation.
- Production hard rules are enforced by static validation in code: no
  localhost, no `sslmode=disable`, no demo / placeholder credentials.
- Connection pool: `DB_MAX_OPEN_CONNS=25` / `DB_MAX_IDLE_CONNS=10` /
  `DB_CONN_MAX_LIFETIME=5m` — compatible with RDS / Cloud SQL default quotas.

#### ③ Security / secrets

- `JWT_SECRET` and `MODEL_CONFIG_API_KEY_SECRET` **must be different** and
  both ≥ 32 characters. In production mode, `change_me_*`, `dev-secret-*`,
  or anything too short is rejected outright by `isInsecureJWTSecret`.
- Multi-key rotation: write
  `JWT_SECRETS_JSON=[{"kid":"k2","secret":"...","active":true},
  {"kid":"k1","secret":"..."}]`. The `active=true` key signs new tokens;
  the others only validate not-yet-expired old tokens — zero-downtime
  rotation.
- After rotating `MODEL_CONFIG_API_KEY_SECRET`, third-party model API keys
  already stored in the DB must be re-entered by users (the old ciphertext
  is encrypted under the old secret and cannot be decrypted).
- `CORS_ORIGINS` is comma-separated; production must replace this with real
  HTTPS sites.

#### ④ Public site URL / branding

- `APP_PUBLIC_URL` is used to build email links (e.g. reset:
  `${APP_PUBLIC_URL}/reset-password?token=...`). The Android deep link
  `fundai://reset` and the miniapp scheme do NOT depend on this, but the
  web side must be matched.
- `BRAND_NAME` is the product-name placeholder used by email templates.

#### ⑤ SMTP / transactional email

- Leaving `SMTP_HOST` empty switches to the **in-memory recorder**: email
  bodies (including the 6-digit verify code or reset link) are echoed back
  in the JSON response. Local onboarding / e2e tests can finish without a
  real mailbox.
- For mainland China we recommend Aliyun DM / Tencent SES (highest
  inbox-delivery rate to QQ / 163 / Outlook); globally, SendGrid / Postmark
  / Amazon SES; for dev, MailHog
  (`docker run -p 1025:1025 -p 8025:8025 mailhog/mailhog`).

#### ⑥ WeChat miniapp login

- Source: mp.weixin.qq.com → Settings → Developer settings; full setup is
  in [docs/MINIAPP_DEPLOYMENT.md](docs/MINIAPP_DEPLOYMENT.md).
- When empty, the miniapp auto-falls-back to email / password login (the
  user is not blocked).

#### ⑦ + ⑧ LLM configuration

- Resolution order:
  1. Model explicitly specified by the request
  2. Per-agent model config (ModelConfig in DB)
  3. User tier override
  4. Tier defaults in `.env` (`LLM_CRITICAL_*` / `LLM_STANDARD_*` / `LLM_SIMPLE_*`)
  5. Global `LLM_*`
  6. Provider native aliases (`OPENAI_*` / `CLAUDE_*` etc.)
  7. Hardcoded in-code default
- Gemini uses the native `generateContent` protocol (**not** OpenAI
  compatible). Form:
  ```env
  LLM_PROVIDER=gemini
  LLM_MODEL=
  LLM_BASE_URL=
  LLM_API_KEY=
  GEMINI_MODEL=gemini-3.1-pro-preview
  GEMINI_BASE_URL=https://generativelanguage.googleapis.com/v1beta
  GEMINI_API_KEY=your_gemini_key
  ```
  You can also write it on the global `LLM_*` keys. `custom` still means
  "OpenAI-compatible custom endpoint" — **do not** point it at the native
  Gemini URL.

#### ⑨ L3 memory embedding

- `RECALL_OPENAI_API_KEY` is the kill-switch for the L3 long-term-memory
  pgvector backfill worker.
- Empty → loop doesn't start (soft-off, no error); set → uses the same
  `OPENAI_*` endpoint to embed the `memories` table content into the
  pgvector column.
- Default model `text-embedding-3-small`, OpenAI-compatible; DeepSeek /
  Qwen embedding endpoints work too (any API-compatible service).

#### ⑩ Market-data chain

- Global fallback chain:
  `MARKETDATA_QUOTE_PROVIDERS=quantdinger,china-stock,akshare`.
- Per-market overrides (F1.5): `MARKETDATA_QUOTE_PROVIDERS_{MARKET}`; the
  global / default chains still get dedup-appended at the tail so the
  fallback always covers you. The recommended baseline is in the section
  10 comments in `.env.example`.
- For A-shares we recommend starting `china-stock-mcp` + `akshare-mcp`:
  `docker compose --profile market-data up -d`.
- CoinGecko uses the free key-less v3 endpoint by default (30 req/min
  free tier). With a Pro key, point `MARKETDATA_COINGECKO_BASE_URL` at a
  self-hosted reverse proxy (which injects the Authorization header); do
  not expose the key in the client.
- Yahoo earnings, OHLC / fundamentals / sector-flow cache TTLs all live in
  this section.

#### ⑪ Crypto WebSocket real-time streaming (F8)

- `marketdata.Service.StartCryptoStreams` spins up two key-less public
  WebSocket goroutines (Binance + Coinbase) at startup; the in-process
  `cryptoTickerCache` answers in microseconds.
- Default crypto chain: `binance → coinbase → coingecko → yahoo`. Only on
  WS cache miss / stale (`MARKETDATA_CRYPTO_WS_STALE_AFTER`, default 30s)
  does it fall back to REST.
- **Network reachability**: Binance + Coinbase market-data endpoints are
  silently dropped on the mainland-China public internet (TLS handshake
  succeeds but no tickers flow). Two options:
  1. Deploy in an unrestricted region (HK / SG / Tokyo / EU / US)
  2. `MARKETDATA_CRYPTO_WS_ENABLED=false` and accept CoinGecko 30 req/min polling
- Reconnect: each stream has its own exponential backoff (1s → 30s),
  auto-recovers on drop, and records the result in the `provider_health`
  table (super-admin `GET /api/admin/marketdata/health` shows
  `binance-ws` / `coinbase-ws` cumulative ok / fail counts and last error).

#### ⑫ News providers

- A-share tickers automatically get `eastmoney` + `sina` prepended (key-less,
  native Chinese); other markets still use SerpAPI / Tavily / the
  `web-search-mcp` RSS chain.
- `MARKETDATA_NEWS_HYBRID=true` (default): the ZH and EN chains run
  concurrently then merge + dedup, so the user sees local Chinese
  reporting and English macro / analyst perspectives at the same time.
  Set to `false` to fall back to the legacy single-chain fallback (only
  do this when debugging cost spikes).
- Multiple SerpAPI / Tavily keys can be comma-separated; the server
  round-robins quota across them.

#### ⑬ News translation (optional)

- Three providers:
  - `none` — no translation (default)
  - `libretranslate` — call any LibreTranslate-compatible service (open
    source; self-hostable)
  - `openai-compat` — call any OpenAI-compatible `/chat/completions`
    (DeepSeek / OpenAI / Qwen-compat / local LLM); also requires
    `MARKETDATA_TRANSLATOR_MODEL`
- Once configured, the translator fills in any missing
  `titleZh / titleEn / summaryZh / summaryEn` so the frontend can show
  the language the user prefers.

#### ⑭ Market-data resilience / cache / risk (Phase 3a / 3c)

- `MARKETDATA_STALE_AFTER` (default 15m): once `QuoteSnapshot.AsOf`
  passes this threshold, `isStale=true`. The hard-risk
  **StaleQuoteGuard** therefore rejects "buy / add / open short" but
  still allows "sell / close / reduce" so funds can always de-risk even
  when quotes are unhealthy.
- Circuit breaker: a single provider failing `CIRCUIT_FAILURES` (default
  3) consecutive times trips the breaker for `CIRCUIT_COOLDOWN`
  (default 30s); a single success closes it again. HTTP 429 goes through
  the separate `THROTTLE_COOLDOWN` (default 5m) so the upstream quota
  isn't burnt down.
- Adaptive TTL: `MARKETDATA_ADAPTIVE_TTL` and `MARKETDATA_ADAPTIVE_QUOTE_TTL`
  share one master-switch idea — when the primary market is open, use
  `_INSESSION` (5s); when closed, use `_OFFSESSION` (60s); saves upstream
  quota.
- `MARKETDATA_QUOTE_RATE_LIMITS=coingecko=0.5,yahoo=4` formats per-upstream
  QPS limits.
- Per-fund override: each fund can set
  `hardRisk.maxQuoteAgeSeconds` to 60s (HFT) or 1h (long-only) in
  "Fund settings → hard-risk overrides"; if unset, it inherits the
  platform default.

#### ⑮ Feature flags / debug

- `FUND_DEBATE_ROUNDTABLE=on` switches to round-table debate mode
  (multi-agent turn-based talk); default off.
- `BACKTEST_DISABLED=1` disables the backtest engine (used in CI / demo
  environments).

### Team activity / learning loop / auction market (runtime capabilities — no env config required)

The following are enabled by default in code and need no extra env, but
they're "things people expect to find in the README", so they're listed
alongside the 15 env sections above for easy cross-reference:

- **Team real-time activity (F2.4)**: `GET /api/funds/{fundId}/team/activity`
  (REST backfill) and `GET .../team/activity/stream` (SSE, relies on the
  `fundai_session` cookie). The frontend TeamManagement page auto-wires
  `TeamActivityPanel`. Slow-client events are dropped without blocking
  workflow; on reconnect the client uses `?sinceSeq=N` to backfill the
  gap.
- **Self-learning long-term reflection (F3)**:
  `GET /api/funds/{fundId}/reflections?limit=N`. At the end of every
  `DailyReview`, `memory.Reflect()` distils the last 30 days of daily /
  agent learnings into 1–2 long-term lesson sentences (with a 7-day
  cooldown per fund to keep LLM spend in check). When no LLM runtime is
  configured, the whole step is skipped without erroring. **A/B
  isolation**: all reads and writes filter by `fund_id` at the SQL layer
  — a treatment fund's reflections never leak into the control /
  production fund.
- **Skill candidate library (F4)**: `GET /api/agents/{agentId}/skills` +
  `POST .../approve` + `DELETE ...`. Once a reflection is persisted, a
  `status=proposed, enabled=false` skill candidate is auto-proposed.
  **Two gates**: `skillEntryIsActive` always treats `proposed` as
  inactive, so unapproved candidates never pollute an agent prompt. The
  frontend `/agent-learning` page approves / rejects with one click.
- **Auction market + wallet hold (F5)**: `POST /api/marketplace/auctions`
  / `POST .../bids` / `POST .../settle`. `WalletRepo`'s three-step hold
  / release / capture keeps English-auction money safe;
  `auctionSettlementLoop` scans every 5s; anti-snipe automatically
  extends `ends_at`. The frontend `/auctions` page provides the minimum
  usable loop.
- **A/B experience promotion + rollback (F6)**:
  `POST /api/abtests/{testId}/promote-learning` +
  `POST .../{promotionId}/rollback`. Promotion = a three-way single
  transaction (evolution_config / long_term reflection / skill
  candidate); rollback is the exact reverse. Skill landing is still
  gated by the F4 manual approval — winning an A/B test only earns
  "evidence", it does not bypass human review.
- **Workflow scheduler observability + manual trigger (F7)**:
  `GET /api/admin/workflow/scheduler` +
  `POST /api/admin/workflow/scheduler/trigger/{fundId}` (super-admin).
  The frontend `/admin` page's "Daily workflow scheduler" card refreshes
  a snapshot every 20s, sorted by next-trigger time, with an
  immediate-trigger button.
- **Prometheus observability**: `GET /api/metrics` exposes 5
  `fundai_marketdata_provider_*` metrics (calls_total / failures_total /
  consecutive_failures / circuit_open / latency_ms_ema) — drop straight
  into Prom + Grafana. The Admin "market-data provider health" card uses
  the same data, auto-refreshing every 15s.

### Offline probe: `marketdata-probe`

`server/cmd/marketdata-probe` is a standalone CLI that bypasses the
HTTP / Auth / DB layers and calls `marketdata.Service` directly, so you
can run end-to-end smoke tests against real upstreams (Tencent Finance,
Yahoo Chart v8, Eastmoney CMS, Sina Roll, web-search MCP). Common usages:

```bash
# A-share ticker: Tencent quotes + ZH/EN hybrid news
go run ./cmd/marketdata-probe -symbol 600519 -market cnstock

# Adding the EN chain (start web-search-mcp first with docker compose up -d web-search-mcp)
MCP_WEB_SEARCH_URL=http://localhost:3004 \
  go run ./cmd/marketdata-probe -symbol 600519 -market cnstock -limit 6

# US equity: Yahoo Chart v8 quote
go run ./cmd/marketdata-probe -symbol AAPL -market us_equity -skip-news

# Futures: akshare front-month contract (start akshare-mcp first with docker compose --profile market-data up -d)
go run ./cmd/marketdata-probe -symbol cu2503 -market futures \
  -akshare-url http://localhost:3002 -skip-news

# Crypto: public CoinGecko v3 (no key required)
go run ./cmd/marketdata-probe -symbol BTCUSDT -market crypto -skip-news

# Override provider chain: use only yahoo for us_equity quotes
go run ./cmd/marketdata-probe -symbol AAPL -market us_equity \
  -quote-providers yahoo -skip-news

# Validate the translator chain (libretranslate-compatible)
go run ./cmd/marketdata-probe -symbol 600519 -market cnstock \
  -translator libretranslate -translator-base-url http://127.0.0.1:5500
```

A non-zero exit code means the quote or news fetch failed; the output
includes each news item's `source/language` and the
`titleZh/titleEn` translation fill state, so it doubles as a fast
upstream-availability diagnostic.

Example minimum-runnable combo:

```env
DATABASE_URL=postgres://fundai:local_dev_only_change_me@localhost:5432/fundai?sslmode=disable
LLM_PROVIDER=openai
LLM_MODEL=gpt-4o-mini
LLM_API_KEY=your_api_key
MARKETDATA_QUOTE_PROVIDERS=quantdinger,china-stock
QUANTDINGER_URL=https://your-quantdinger-host
MARKETDATA_NEWS_PROVIDERS=eastmoney,sina,local-search,web-search
SERPAPI_KEYS=your_serpapi_key
TAVILY_API_KEYS=your_tavily_key
WEB_SEARCH_FEED_URL=https://news.google.com/rss/search
```

## Troubleshooting

### Decision Center shows "Current quote is unavailable; please refresh price before executing"

The risk / execution layer reads the real-time quote snapshot returned by
the `marketdata` provider. When that snapshot is missing or stale, the
frontend shows "current quote is unavailable; please refresh price before
executing". Investigate in this order:

1. **Is the market open?** A-shares, HK shares, etc. only return quotes
   during trading hours; outside the session most providers return no
   latest price. Try a 24×7-supported instrument (e.g. crypto) or wait
   for the next session.
2. **Does `MARKETDATA_QUOTE_PROVIDERS` include a usable provider?** The
   default is `quantdinger,china-stock,akshare`; `.env` must keep at
   least one provider that you can reach. Per-market built-in defaults:
   - cnstock: `tencent → china-stock → akshare` (the first two are key-less)
   - usstock: `yahoo` Chart v8 endpoint (key-less)
   - futures: `akshare → china-stock → yahoo` (akshare covers the four
     Chinese front-month contracts SHFE/DCE/CZCE/INE; yahoo backstops
     `GC=F` and friends)
   - crypto: `binance → coinbase → coingecko → yahoo` (Binance / Coinbase
     stream key-less WebSocket tickers; cache hits return in
     microseconds; misses fall back to CoinGecko REST 30 req/min; yahoo
     backstops mainstream coins like BTC-USD)
   - Any market can be overridden with `MARKETDATA_QUOTE_PROVIDERS_{MARKET}` (see the table above)
3. **Are provider URLs and credentials complete?**
   - `QUANTDINGER_URL` empty → the quantdinger provider is skipped
   - Empty `MCP_CHINA_STOCK_URL` / `MCP_AKSHARE_URL` → the corresponding
     MCPs are not started; run `docker compose --profile market-data up -d`
     to bring up `china-stock-mcp` + `akshare-mcp`, then fill in the
     URLs (defaults `http://china-stock-mcp:3001` /
     `http://akshare-mcp:3002`; host-direct change to
     `http://localhost:<port>`)
   - CoinGecko uses the key-less public endpoint by default; for the
     Pro tier, set `MARKETDATA_COINGECKO_BASE_URL` to point at a
     self-hosted reverse proxy (proxy injects the `Authorization`
     header); never expose the API key to the client
4. **Timeout or cache?** `MARKETDATA_PROVIDER_TIMEOUT` (default 5s) too
   short → provider errors; `MARKETDATA_QUOTE_TTL` (default 10s) too
   long → stale snapshots stick around. Tune them down temporarily when
   debugging.
5. **Inspect backend logs**: the `app` container / process emits
   `marketdata: quote provider <name> failed` etc.; the message tells
   you whether it's network, signing, or provider-side.
6. **Re-refresh**: once the configuration is in place, go back to
   Decision Center and click "Refresh price"; the system re-fetches the
   quote and clears the warning.

If the above still cannot fetch quotes, you can temporarily move the plan
back to "pending approval" and execute once the market data recovers.

## License

MIT
