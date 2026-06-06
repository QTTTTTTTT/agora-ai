# AI Fund Platform — 当前仓库实现说明

**简体中文** · [English](./README.en.md)

> 一个基金公司/基金管理原型仓库。当前已真正接线的后端能力以 subscription、model config、usage/billing、company/fund 最小 CRUD，以及 plan 最小审批流为主；团队、交易、workflow、记忆、A/B 测试等链路仍存在占位与渐进式接线状态，需要按生产化要求继续补齐韧性、观测与测试。

## 当前实现概览

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

## 当前真实能力

| 模块 | 当前状态 |
|------|-----------|
| **订阅套餐** | 已接线，可查询套餐、订阅、取消订阅、查询当前订阅状态 |
| **模型配置** | 已接线，可列出平台模型、保存/删除用户模型配置、测试连接 |
| **用量与账单** | 已接线，可查询今日用量、月度汇总、历史明细、账单与费用预估 |
| **基金公司 / 基金** | 已接线，支持 company 与 fund 的最小 CRUD |
| **计划审批** | 已接线最小能力，可读取计划列表/详情，并执行 approve / reject 基本流转 |
| **前端 React** | 已有页面框架与路由，包含 companies、fund dashboard、team、decisions、compare、memory、trades、settings、subscription、models、usage |
| **微信小程序** | 已有页面壳与 mock 数据结构，但 README 不将其视为完整业务端 |
| **基金 team / trade / workflow / memory / abtest** | 路由部分存在，但仍包含占位实现、渐进接线或 demo 级行为，不能直接视为生产完整能力 |

## 技术栈

| 层级 | 技术 |
|------|------|
| 前端 | React 18 + TypeScript + Vite + TailwindCSS + Recharts |
| 小程序 | 微信小程序原生工程（`miniapp/`） |
| 后端 | Go 1.22 + `net/http` method-aware routing |
| 数据库 | PostgreSQL 16 + SQL migrations |
| 部署 | Docker + Docker Compose + Multi-stage Build |
| 模型接入 | OpenAI 兼容配置 + 平台模型/用户自定义模型配置 |

## 快速开始

### 前置条件

- [Docker](https://docs.docker.com/get-docker/) 20.10+
- [Docker Compose](https://docs.docker.com/compose/install/) v2+

### 一键启动（仅限本地开发 / 验收）

```bash
# 1. 克隆项目
git clone <your-repo-url> ai-fund-platform-v3-full
cd ai-fund-platform-v3-full

# 2. 本地一键启动（若不存在 .env 会自动由 .env.example 复制）
chmod +x scripts/start.sh
./scripts/start.sh
```

`scripts/start.sh` 只用于本地开发 / 验收：它会先启动 PostgreSQL，再启动 `web-search-mcp` 与 `app` 容器，并等待 `GET /api/health` 返回成功。该脚本在 `APP_ENV=production` 时会直接拒绝执行。默认访问地址：
- **Web 应用 / SPA**: http://localhost:8080
- **健康检查**: http://localhost:8080/api/health
- **版本信息**: http://localhost:8080/api/version
- **本地 Web Search MCP**: http://localhost:3004/health

### 手动启动（Docker，本地开发 / 验收）

```bash
# 1. 创建环境配置
cp .env.example .env
# 必填只有 4 个 (见下方 ".env.example 配置说明")：
#   DATABASE_URL / JWT_SECRET / MODEL_CONFIG_API_KEY_SECRET
#   + LLM_PROVIDER + LLM_MODEL + LLM_API_KEY (或任一 provider 别名)
# 推荐再补：至少一个行情源 + 至少一个新闻源
# 切生产时同步替换 CORS_ORIGINS / APP_PUBLIC_URL / APP_ENV / APP_DATABASE_SSLMODE

# 2. 先只启动 PostgreSQL，确保本机有可复现数据库
docker compose up -d postgres

# 3. 启动本地验收栈（app + postgres + 本地 web-search MCP）
docker compose up -d --build app web-search-mcp

# 4. 如需更多市场数据 MCP，再额外启动可选 profile
docker compose --profile market-data up -d

# 5. 查看日志
docker compose logs -f postgres app

# 6. 停止服务
docker compose down
```

说明：
- `docker compose up -d postgres` 是最小可复现数据库启动路径，适合“后端本地跑但机器上没有 PostgreSQL”的场景
- `docker compose up -d --build app web-search-mcp` 会启动 `app`、`postgres` 与仓库内置的 `web-search-mcp`，这是当前可复现的最小容器验收路径
- `docker compose --profile market-data up -d` 会额外启动 `china-stock-mcp`、`akshare-mcp`
- `ta-lib-mcp` 仍然只在 `--profile professional` 下启动
- PostgreSQL 首次启动会自动执行 `/Users/bytedance/Downloads/ai-fund-platform-v3-full/scripts/init-db.sql`

### 本地开发（后端不用 Docker，数据库用 Docker）

```bash
# 1. 启动本地 PostgreSQL 容器
cp .env.example .env
docker compose up -d postgres

# 2. 前端开发服务器
cd web
npm install
npm run dev

# 3. 后端开发服务器（复用 docker compose 提供的 PostgreSQL）
cd ../server
export DATABASE_URL="postgres://fundai:local_dev_only_change_me@localhost:5432/fundai?sslmode=disable"
go run ./cmd/server
```

本地开发常用地址：
- 前端 Vite: http://localhost:5173
- 后端 API: http://localhost:8080
- 本地 PostgreSQL: localhost:5432
- SPA 首页（由后端静态托管时）: http://localhost:8080/

### 可选：安装 git 预提交钩子

仓库内置 `.githooks/pre-commit`，会在涉及 `web/src/i18n/locales/**` 的提交上跑一次 i18n key 对齐校验（CI 中的同名步骤是真正的 gate）。一次性激活：

```bash
bash scripts/install-git-hooks.sh
```

后续可用 `git config --unset core.hooksPath` 关闭。Backend-only 的贡献者可以不开，CI 仍会兜底。

## 推荐配置路径

默认推荐走 env-first：复制 `.env.example` 到 `.env`，填好数据库、一个默认 LLM、以及至少一个可用市场数据/新闻来源后即可启动。若某个 team agent 没有在数据库里单独配置模型，它会按以下顺序自动继承：

### Gemini 原生 provider 配置

现在仓库已支持 `LLM_PROVIDER=gemini`，会走 Gemini 原生 `generateContent` 协议，而不是 OpenAI 的 `/chat/completions`。

推荐写法：

```env
LLM_PROVIDER=gemini
LLM_MODEL=
LLM_BASE_URL=
LLM_API_KEY=

GEMINI_MODEL=gemini-3.1-pro-preview
GEMINI_BASE_URL=https://generativelanguage.googleapis.com/v1beta
GEMINI_API_KEY=your_gemini_key
```

也可以直接写到全局 `LLM_*`：

```env
LLM_PROVIDER=gemini
LLM_MODEL=gemini-3.1-pro-preview
LLM_BASE_URL=https://generativelanguage.googleapis.com/v1beta
LLM_API_KEY=your_gemini_key
```

说明：
- `gemini` / `google` 都会归一到 Gemini provider
- `LLM_PROVIDER=gemini` 时，请把 base URL 配到 `.../v1beta`
- 最终请求路径会是 `/v1beta/models/{model}:generateContent`
- `custom` 仍然表示 OpenAI-compatible 自定义端点，不要再把 Gemini 原生地址配置到 `custom`

1. 请求显式指定模型
2. agent 单独模型配置
3. 用户 tier 覆盖
4. `.env` 中的 tier 默认；若该 tier 未单独设置，则继承全局 `LLM_*`
5. 代码内置兜底默认

TeamManagement / ModelConfig 页面仍可用于高级覆盖，但不再是基础启动必需步骤。

## 生产部署前检查

当前仓库的 `docker-compose.yml`、`.env.example`、`scripts/start.sh` 都应视为本地开发 / 验收入口，不是生产部署清单。生产环境至少需要满足以下条件：

- 设置 `APP_ENV=production`
- 显式提供 `DATABASE_URL`，不能依赖 legacy `DB_*` fallback
- `DATABASE_URL` 不能指向 `localhost` / `127.0.0.1`
- `DATABASE_URL` 不能使用 `sslmode=disable`
- `DATABASE_URL` 不能继续使用 demo / placeholder 凭据
- `JWT_SECRET` 必须是强随机值，且长度至少 32 字符
- `MODEL_CONFIG_API_KEY_SECRET` 必须是独立强随机值，且不能与 `JWT_SECRET` 相同
- `CORS_ORIGINS` 必须替换为真实 HTTPS 源，不能包含 `*`、`localhost`、`127.0.0.1`
- 不要使用 `scripts/start.sh` 做生产部署
- 不要把真实 `.env`、云端密钥或数据库凭据提交到仓库

建议在正式发布前先做一次最小静态校验：

```bash
APP_ENV=production \
JWT_SECRET='<32+ chars random secret>' \
MODEL_CONFIG_API_KEY_SECRET='<another 32+ chars random secret>' \
DATABASE_URL='postgres://user:pass@db.example.com:5432/fundai?sslmode=require' \
CORS_ORIGINS='https://app.example.com' \
go run ./server/cmd/server
```

如果配置本身安全，服务不会因为配置校验阶段而退出；后续若数据库不可达，则会表现为连接错误，而不是配置错误。

## 发布检查清单

- 后端测试通过：`go test ./...`
- 容器编排仍可解析：`docker compose config`
- 前端构建通过：`npm --prefix web run build`
- 启动后 `GET /api/health`、`GET /api/version`、`GET /api/metrics` 可访问
- 检查日志与启动输出，确认未暴露数据库连接串、密码或第三方模型 API key
- 若更换 `MODEL_CONFIG_API_KEY_SECRET`，需要通知运营重新录入已存储的用户模型 API key

## 项目结构

```text
ai-fund-platform-v3-full/
├── Dockerfile                    # 多阶段构建（web + server）
├── docker-compose.yml            # app / postgres / MCP 编排
├── .env.example                  # 环境变量模板
├── scripts/
│   ├── init-db.sql               # 初始化 SQL（仓库现有文件）
│   ├── start.sh                  # 一键启动脚本
│   └── stop.sh                   # 停止 compose 服务
├── server/                       # Go 后端
│   ├── cmd/server/main.go        # 入口、健康检查、版本、SPA fallback
│   ├── cmd/server/wiring_adapters.go
│   ├── migrations/
│   │   ├── 001_init.sql
│   │   └── 002_subscription_and_models.sql
│   └── internal/
│       ├── api/
│       │   ├── fund_handler.go           # company/fund 及占位路由
│       │   └── subscription_handler.go   # subscription/models/usage API
│       ├── repository/
│       ├── subscription/
│       ├── llm/
│       ├── workflow/
│       ├── agent/
│       └── abtest/
├── web/                          # React 前端
│   ├── package.json
│   └── src/
│       ├── App.tsx               # companies 与 funds/* 路由
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
└── miniapp/                      # 微信小程序工程骨架
```

## Docker 架构

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

**与当前文件一致的说明：**
- `app` 镜像来自根目录 `Dockerfile`，运行时同时提供 Go API 与 React 静态文件
- `postgres` 为 PostgreSQL 16 Alpine
- `web-search-mcp` 使用仓库内置 `Dockerfile.web-search-mcp` 本地构建，并默认暴露 `localhost:3004`
- `china-stock-mcp`、`akshare-mcp` 需要 `docker compose --profile market-data up -d`
- `ta-lib-mcp` 需要 `docker compose --profile professional up -d`

## API 端点

### 1) 已接线并可用

#### 基础与元信息

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/health` | 健康检查，包含 `status/time/version` |
| GET | `/api/version` | 版本、构建时间、Go 版本 |

#### Subscription / Models / Usage

这些接口依赖 `X-User-ID` 请求头。

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/plans` | 列出订阅套餐 |
| GET | `/api/subscription` | 获取当前用户订阅与生效 plan |
| POST | `/api/subscription` | 创建/变更订阅 |
| DELETE | `/api/subscription` | 取消订阅 |
| GET | `/api/models` | 列出平台模型与用户自定义模型 |
| GET | `/api/models/config` | 获取用户模型配置 |
| POST | `/api/models/config` | 保存模型配置 |
| DELETE | `/api/models/config/{configId}` | 删除模型配置 |
| POST | `/api/models/test` | 测试模型连接 |
| GET | `/api/usage/today` | 今日用量摘要 |
| GET | `/api/usage/monthly?month=YYYY-MM` | 月度用量 |
| GET | `/api/usage/history?offset=0&limit=20` | 用量历史分页 |
| GET | `/api/usage/bill?month=YYYY-MM` | 月度账单 |
| GET | `/api/usage/estimate` | 当前月费用预估 |

#### Company / Fund 最小 CRUD

`POST /api/companies` 与 `GET /api/companies` 也依赖 `X-User-ID` 请求头。

| Method | Path | Description |
|--------|------|-------------|
| POST | `/api/companies` | 创建基金公司 |
| GET | `/api/companies` | 列出当前用户的基金公司 |
| POST | `/api/companies/{companyId}/funds` | 创建基金 |
| GET | `/api/companies/{companyId}/funds` | 列出公司下基金 |
| GET | `/api/funds/{fundId}` | 基金详情 |
| PUT | `/api/funds/{fundId}` | 更新基金基础信息 |
| DELETE | `/api/funds/{fundId}` | 删除基金 |

> Body schema 严格（`DisallowUnknownFields`）。**`POST /api/companies` 仅接受** `{"name": string, "description"?: string}`；额外字段（如 `headquarters`、`region` 等）会返回 400。**`POST /api/companies/{companyId}/funds` 接受** `{"name", "description"?, "tradingMode" (live/simulation/paper), "initialCapital", "market"?, "exchange"?, "assetClass"?, "baseCurrency"?, "benchmarkSymbol"?, "primaryDirection"?, "calendarCode"?, "timeZone"?, "universe"?, "teamIntervals"?, "specialization"?, "hardRisk"?}`。当 `market`/`exchange`/`assetClass` 任一为 crypto/BINANCE/COINBASE 时日历自动分流到 `CRYPTO-24X7` + UTC（详见 F11.1）。当未设置 `benchmarkSymbol` 时，按市场默认 `BTC-USD`（crypto）/`SPY`（us_equity）/`000300.SS`（a_share）/`ES=F`（futures），不再从 fund name 截词作为 ticker（F11.2）。

#### Plan 最小接线

以下计划接口已接到真实 repository 路径，可读取基金计划、读取单条计划，并执行最小状态流转（`pending -> approved/rejected`）：

| Method | Path | Description |
|--------|------|-------------|
| GET | `/api/funds/{fundId}/plans` | 列出基金计划 |
| GET | `/api/plans/{planId}` | 计划详情 |
| POST | `/api/plans/{planId}/approve` | 审批通过计划 |
| POST | `/api/plans/{planId}/reject` | 驳回计划 |

### 2) 路由已保留，但当前仍不应按生产能力验收

以下路径在 `fund_handler.go` 中定义，但目前仍处于占位、部分接线或未完成生产化 hardening 状态；其中一部分会直接返回 `501 not implemented`，另一部分虽然已有前端页面或最小数据链路，但仍不能视为已完整可用能力：

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

注意：README 之前常见的错误示例如 `/api/funds/:id/plans/:planId/approve` 与当前代码不一致；当前注册的真实路径是 `/api/plans/{planId}/approve`。

## 前端路由

### React SPA

后端会把非 API 路径 fallback 到 `web/dist/index.html`，因此生产态由 Go 服务统一托管前端页面。当前 `web/src/App.tsx` 中可见的主要路由为：

| Path | Description |
|------|-------------|
| `/` | 重定向到 `/companies` |
| `/companies` | 公司列表页 |
| `/funds/:fundId` | 基金仪表盘首页 |
| `/funds/:fundId/team` | 团队页（前端页面存在，后端未完整接线） |
| `/funds/:fundId/decisions` | 决策页 |
| `/funds/:fundId/compare` | A/B 对比页 |
| `/funds/:fundId/memory` | 记忆页 |
| `/funds/:fundId/trades` | 交易页 |
| `/funds/:fundId/settings` | 基金设置页 |
| `/funds/:fundId/subscription` | 订阅页 |
| `/funds/:fundId/models` | 模型配置页 |
| `/funds/:fundId/usage` | 用量页 |

### 微信小程序

`miniapp/` 目录包含首页、团队、决策、记忆、更多等页面，以及 subscription / model-config / usage 等分包页面骨架；当前 README 仅说明其存在，不把它描述为与后端完全联动的正式交付端。

## .env.example 配置说明

`.env.example` 是单一可信清单 —— 凡是后端真正 `os.Getenv()` 读到的变量，都
分组列在这个文件里，每组顶部有一段中文说明告诉你"什么时候需要、为什么需要、
怎么填"。**复制到 `.env` 后再编辑，不要把真实密钥提交到仓库。**

文件目前一共划分 15 个分区，对应下方一张总览表与本节后面的"按分区
逐条说明"。如果你只想跑起来，先填【必填四件套】即可，其它分区都允许留空、
全部带 fail-safe 退化路径（要么走平台默认、要么对应功能软关）。

### 必填四件套（最小可运行集）

| 变量 | 用途 | 不填会怎样 |
|------|------|------------|
| `DATABASE_URL` | PostgreSQL 连接串（容器内用 `APP_DATABASE_URL`） | 服务直接退出 |
| `JWT_SECRET` | 登录 token 签发；至少 32 字符随机串 | 启动校验 fail，生产模式拒绝启动 |
| `MODEL_CONFIG_API_KEY_SECRET` | 加密用户存进 DB 的第三方模型 key；必须独立于 `JWT_SECRET` | 用户保存模型 API key 时报 500 |
| `LLM_PROVIDER` / `LLM_MODEL` / `LLM_API_KEY` | 默认 LLM 入口（或填一组 provider 别名 `OPENAI_*` 等） | 所有 agent 调用都会回退到代码内置兜底，输出质量塌方 |

> 不填 `MARKETDATA_*` 也能起来：行情链路全部带公网 fallback；只有 A 股 MCP /
> QuantDinger 这种"自建上游"才需要显式给 URL。

### 分区总览

| # | 分区 | 关键变量 | 必填？ |
|---|------|----------|--------|
| 1  | 应用运行时 | `APP_ENV`, `APP_PORT`, `LOG_LEVEL`, `MIGRATIONS_PATH`, `STATIC_FILES_PATH`, `SESSION_TTL`, `ALLOW_INTERNAL_COMPOSE_DB` | ✗（除 `APP_ENV` 决定生产校验） |
| 2  | PostgreSQL | `DATABASE_URL` (+ 容器内 `APP_DATABASE_URL`, `APP_DATABASE_SSLMODE`)，老式 `DB_*` 回退，连接池 `DB_MAX_*` | ✓ `DATABASE_URL` |
| 3  | 安全 / 密钥 | `JWT_SECRET`, `JWT_SECRETS_JSON`（多 key 轮换）, `MODEL_CONFIG_API_KEY_SECRET`, `CORS_ORIGINS`, `API_KEY_ENCRYPTION_SECRET`（历史别名） | ✓ JWT + Model secret |
| 4  | 站点公开地址 / 品牌 | `APP_PUBLIC_URL`, `BRAND_NAME` | ✗ 但生产必填正确域名（影响 reset/verify 邮件链接） |
| 5  | SMTP / 事务邮件 | `SMTP_HOST/PORT/USERNAME/PASSWORD/FROM/FROM_NAME`, `SMTP_USE_TLS/STARTTLS/TIMEOUT` | ✗（`SMTP_HOST` 留空走 in-memory recorder，邮件正文回写到 JSON response） |
| 6  | 微信小程序登录 | `WECHAT_MINIAPP_APPID/SECRET`, `WECHAT_JSCODE_SESSION_URL`, `WECHAT_LOGIN_TIMEOUT` | ✗（未配 `/api/auth/wechat-login` 返回 503，小程序自动 fall back 到邮箱登录） |
| 7  | 默认 LLM | `LLM_PROVIDER/MODEL/BASE_URL/API_KEY` + tier 覆盖 `LLM_CRITICAL_*` / `LLM_STANDARD_*` / `LLM_SIMPLE_*` | ✓（或用分区 8 的 provider 别名替代） |
| 8  | provider 原生别名 | `OPENAI_*`, `CLAUDE_*` / `ANTHROPIC_*`, `DEEPSEEK_*`, `QWEN_*`, `GEMINI_*` / `GOOGLE_*` | ✗（仅 `LLM_*` 缺项时 fallback） |
| 9  | L3 记忆 embedding | `RECALL_OPENAI_API_KEY/BASE_URL`, `RECALL_EMBED_MODEL` | ✗ 留空 → backfill worker 不启动（功能软关） |
| 10 | 行情链路 | `MARKETDATA_QUOTE_PROVIDERS` + 各市场 `*_CNSTOCK/USSTOCK/HKSTOCK/FUTURES/CRYPTO`, `QUANTDINGER_URL`, `MCP_*_URL`, `BINANCE_OHLC_URL`, `OHLC_/FUNDAMENTAL_/SECTORFLOW_CACHE_TTL`, `YAHOO_EARNINGS_*`, `MCP_WEB_SEARCH_URL`, `MARKETDATA_COINGECKO_BASE_URL` | ✗（公网链兜底，但 A 股 / 期货推荐起 MCP） |
| 11 | Crypto WebSocket | `MARKETDATA_CRYPTO_WS_ENABLED`, `MARKETDATA_BINANCE_WS_*`, `MARKETDATA_COINBASE_WS_*`, `MARKETDATA_CRYPTO_WS_STALE_AFTER` | ✗（关掉后退到 CoinGecko/Yahoo 轮询，30 req/min 配额成为瓶颈） |
| 12 | 新闻 provider | `MARKETDATA_NEWS_PROVIDERS`, `MARKETDATA_NEWS_HYBRID`, `EASTMONEY/SINA_NEWS_BASE_URL`, `SERPAPI_KEYS`, `TAVILY_API_KEYS` (+ `*_BASE_URL`) | ✗（A 股自动带 eastmoney + sina，其它市场最好补 SerpAPI / Tavily） |
| 13 | 新闻翻译（可选） | `MARKETDATA_TRANSLATOR_PROVIDER/BASE_URL/API_KEY/MODEL/TIMEOUT` | ✗ 默认 `none` |
| 14 | 行情韧性 / 缓存 / 风控 | `MARKETDATA_STALE_AFTER`, `MARKETDATA_CIRCUIT_FAILURES/COOLDOWN`, `MARKETDATA_THROTTLE_COOLDOWN`, `MARKETDATA_ADAPTIVE_TTL`, `MARKETDATA_ADAPTIVE_QUOTE_TTL`, `MARKETDATA_QUOTE_TTL[_INSESSION/_OFFSESSION]`, `MARKETDATA_NEWS_TTL`, `MARKETDATA_PROVIDER_TIMEOUT`, `MARKETDATA_QUOTE_RATE_LIMITS` | ✗（默认值就是生产推荐基线） |
| 15 | Feature flags / debug | `FUND_DEBATE_ROUNDTABLE`, `BACKTEST_DISABLED` | ✗（关掉对应能力，仅特殊场景需要） |

### 按分区逐条说明

> 下面每个分区只解释"为什么这么设计"和"踩坑点"。具体每个变量的中文注释、
> 默认值和合法格式，都在 [`.env.example`](.env.example) 同名分区头部。

#### ① 应用运行时

- `APP_ENV=production` 触发额外启动校验：DB 必须远程 + SSL、JWT/Model secret
  必须强随机且互不相同、`CORS_ORIGINS` 不能包含 `*` / localhost。dev 模式
  全部宽松。
- `MIGRATIONS_PATH` / `STATIC_FILES_PATH` 同时支持旧名 `MIGRATIONS_DIR` /
  `STATIC_DIR`，方便兼容老部署脚本。
- `ALLOW_INTERNAL_COMPOSE_DB=1` 与 `RUNNING_IN_CONTAINER=1` 同时为真，才允许
  `DATABASE_URL` 指向 compose 内部主机名 `postgres`。生产部署千万别开。

#### ② 数据库

- 优先级：`DATABASE_URL` > 容器内 `APP_DATABASE_URL` > 旧 `DB_*` 拼接。
- 生产硬条件由代码做静态校验：禁用 localhost / 禁用 `sslmode=disable` /
  禁用 demo/placeholder 凭据。
- 连接池：`DB_MAX_OPEN_CONNS=25` / `DB_MAX_IDLE_CONNS=10` /
  `DB_CONN_MAX_LIFETIME=5m`，与 RDS / Cloud SQL 默认 quota 兼容。

#### ③ 安全 / 密钥

- `JWT_SECRET` 与 `MODEL_CONFIG_API_KEY_SECRET` **必须不同**且都 ≥ 32 字符。
  生产模式下 `change_me_*`、`dev-secret-*`、长度不足都会被 `isInsecureJWTSecret`
  直接拒绝启动。
- 多 key 轮换：写 `JWT_SECRETS_JSON=[{"kid":"k2","secret":"...","active":true},
  {"kid":"k1","secret":"..."}]`，`active=true` 的 key 用来签新 token，其它 key
  仅用来校验未过期的旧 token，做无停机轮换。
- `MODEL_CONFIG_API_KEY_SECRET` 改值后，已存进 DB 的第三方模型 API key 需要
  让用户重新录入（旧密钥用旧 secret 加密、无法解出）。
- `CORS_ORIGINS` 逗号分隔；生产必须替换为真实 HTTPS 站点。

#### ④ 站点公开地址 / 品牌

- `APP_PUBLIC_URL` 用于构造邮件链接（如 reset:
  `${APP_PUBLIC_URL}/reset-password?token=...`）。Android 深链 `fundai://reset`
  与小程序自带 scheme 不依赖这里，但 web 端必须配对。
- `BRAND_NAME` 是邮件模板里的产品名占位符。

#### ⑤ SMTP / 事务邮件

- 留空 `SMTP_HOST` 走 **in-memory recorder**：邮件正文（含 6 位 verify code 或
  reset 链接）会直接回写到 JSON response 里。本地 onboarding / e2e 测试不需要
  真邮箱即可跑完。
- 中国大陆推荐阿里云 DM / 腾讯云 SES（直送 QQ / 163 / Outlook 收件率最高）；
  全球推荐 SendGrid / Postmark / Amazon SES；dev 用 MailHog
  `docker run -p 1025:1025 -p 8025:8025 mailhog/mailhog`。

#### ⑥ 微信小程序登录

- 来源：mp.weixin.qq.com → 设置 → 开发设置；详细配置见
  [docs/MINIAPP_DEPLOYMENT.md](docs/MINIAPP_DEPLOYMENT.md)。
- 留空时小程序自动 fall back 到邮箱/密码登录（不会卡住用户）。

#### ⑦ + ⑧ LLM 配置

- 解析优先级：
  1. 请求显式 model
  2. agent 单独模型配置（DB 里的 ModelConfig）
  3. 用户 tier 覆盖
  4. `.env` 里的 tier 默认（`LLM_CRITICAL_*` / `LLM_STANDARD_*` / `LLM_SIMPLE_*`）
  5. 全局 `LLM_*`
  6. provider 原生别名（`OPENAI_*` / `CLAUDE_*` 等）
  7. 代码内置兜底
- Gemini 走原生 `generateContent` 协议（**不是** OpenAI 兼容）。写法：
  ```env
  LLM_PROVIDER=gemini
  LLM_MODEL=
  LLM_BASE_URL=
  LLM_API_KEY=
  GEMINI_MODEL=gemini-3.1-pro-preview
  GEMINI_BASE_URL=https://generativelanguage.googleapis.com/v1beta
  GEMINI_API_KEY=your_gemini_key
  ```
  也可以直接写到全局 `LLM_*` 上。`custom` 仍然表示 OpenAI 兼容自定义端点，
  **不要**把 Gemini 原生地址配置到 `custom`。

#### ⑨ L3 记忆 embedding

- `RECALL_OPENAI_API_KEY` 是 L3 长期记忆 pgvector backfill worker 的开关。
- 留空 → loop 不启动（功能软关，不报错）；填了就会用同一组 `OPENAI_*`
  endpoint 把 `memories` 表里的内容向量化进 pgvector 列。
- 默认模型 `text-embedding-3-small`，与 OpenAI 兼容；DeepSeek / Qwen
  自带 embedding 时也可以指过去（API 兼容即可）。

#### ⑩ 行情链路

- 全局 fallback 链：`MARKETDATA_QUOTE_PROVIDERS=quantdinger,china-stock,akshare`。
- 各市场单独覆盖（F1.5）：`MARKETDATA_QUOTE_PROVIDERS_{MARKET}`，全局链与
  默认链仍会去重追加在尾部，确保 fallback 永远兜得住。推荐基线见
  `.env.example` 第 10 区注释。
- A 股 MCP 推荐起 `china-stock-mcp` + `akshare-mcp`：
  `docker compose --profile market-data up -d`。
- CoinGecko 默认走免 key v3（30 req/min 免费）。有 Pro key 时把
  `MARKETDATA_COINGECKO_BASE_URL` 指向自建反代（反代注入 Authorization 头），
  不要把 key 直接暴露在客户端。
- Yahoo 财报、OHLC / 基本面 / 板块资金流缓存 TTL 都在这一区。

#### ⑪ Crypto WebSocket 实时推流 (F8)

- 启动时 `marketdata.Service.StartCryptoStreams` 拉起 Binance + Coinbase 两条
  免 key 公网 WS goroutine，进程内 `cryptoTickerCache` 微秒返回。
- 默认 crypto 链：`binance → coinbase → coingecko → yahoo`。WS cache miss /
  stale (`MARKETDATA_CRYPTO_WS_STALE_AFTER`, 默认 30s) 才回 REST。
- **网络可达性**：Binance + Coinbase market-data 端点在中国大陆公网会被静默
  丢包（TLS 握手成功但 ticker 不推）。两个选项：
  1. 部署到无限制区域（HK / SG / Tokyo / EU / US）
  2. `MARKETDATA_CRYPTO_WS_ENABLED=false`，接受 CoinGecko 30 req/min 轮询
- 重连：每条流独立指数退避（1s → 30s），断线自动恢复，记入 `provider_health`
  表（super-admin `GET /api/admin/marketdata/health` 可见
  `binance-ws` / `coinbase-ws` 累计 ok / fail 次数与最近错误）。

#### ⑫ 新闻 provider

- A 股标的自动在前面追加 `eastmoney` + `sina`（免 key，中文原生），其它市场
  仍沿用 SerpAPI / Tavily / `web-search-mcp` RSS。
- `MARKETDATA_NEWS_HYBRID=true`（默认）：ZH 与 EN 两条链并发拉取后合并去重，
  让用户同时看到本地中文报道与英文宏观/分析师视角。设为 `false` 回到传统
  单链路 fallback（调试成本飙升时再关）。
- SerpAPI / Tavily key 多个用逗号分隔，server 端做轮询配额分摊。

#### ⑬ 新闻翻译（可选）

- 三种 provider：
  - `none` 不翻译（默认）
  - `libretranslate` 调 LibreTranslate 兼容服务（开源；可自建）
  - `openai-compat` 调任意 OpenAI 兼容 `/chat/completions`（DeepSeek / OpenAI /
    Qwen-compat / 本地 LLM），需要同时填 `MARKETDATA_TRANSLATOR_MODEL`
- 配置后翻译器把缺失的 `titleZh / titleEn / summaryZh / summaryEn` 补齐，前端
  按用户语言选择展示。

#### ⑭ 行情韧性 / 缓存 / 风控 (Phase 3a / 3c)

- `MARKETDATA_STALE_AFTER`（默认 15m）：`QuoteSnapshot.AsOf` 超过该阈值
  → `isStale=true`。硬风控的 **StaleQuoteGuard** 会因此 reject "买入/加仓/开空"，
  但放行 "卖出/平仓/减仓"，保证报价异常时仍能 de-risk。
- 熔断：单 provider 连续失败 `CIRCUIT_FAILURES`（默认 3）次 → 熔断
  `CIRCUIT_COOLDOWN`（默认 30s）；单次成功立即关熔断。429 单独走
  `THROTTLE_COOLDOWN`（默认 5m）防止打爆配额。
- 自适应 TTL：`MARKETDATA_ADAPTIVE_TTL` 和 `MARKETDATA_ADAPTIVE_QUOTE_TTL` 共享
  一个总开关思路 —— 主要市场开盘时走 `_INSESSION`（5s），休市时走 `_OFFSESSION`
  （60s），节省上游配额。
- `MARKETDATA_QUOTE_RATE_LIMITS=coingecko=0.5,yahoo=4` 这种格式做上游 QPS 限速。
- fund 级覆盖：每只基金可以在「基金设置 → 硬风控覆盖」里把
  `hardRisk.maxQuoteAgeSeconds` 调到 60s（高频）或 1h（长线），不写则继承平台默认。

#### ⑮ Feature flags / debug

- `FUND_DEBATE_ROUNDTABLE=on` 切到圆桌讨论模式（多 agent 轮发言）；默认关。
- `BACKTEST_DISABLED=1` 关掉回测引擎（CI / 演示环境用）。

### 团队实时活动 / 学习闭环 / 拍卖市场（运行时能力，不需要 env 配置）

下列能力在代码层默认开启、无需额外 env，但属于"读 README 想看见的事"，
和上面 15 个分区一起列在这里方便交叉对照：

- **团队实时活动 (F2.4)**：`GET /api/funds/{fundId}/team/activity`（REST backfill）
  与 `GET .../team/activity/stream`（SSE，依赖 `fundai_session` cookie）。前端
  TeamManagement 页自动接入 `TeamActivityPanel`。慢 client 事件会被 drop 不阻塞
  工作流；断线后用 `?sinceSeq=N` 调 REST 缺口补齐。
- **自学习长期反思 (F3)**：`GET /api/funds/{fundId}/reflections?limit=N`。每次
  `DailyReview` 末尾 `memory.Reflect()` 把近 30 天的 daily/agent 学习蒸馏成 1–2
  句长期 lesson（每基金 7 天 cooldown 防 LLM 烧钱）。LLM runtime 没配置就整段
  跳过，不报错。**A/B 隔离**：所有读写都按 `fund_id` SQL 层过滤，treatment 基金
  反思绝不泄漏到 control / production 基金。
- **候选技能库 (F4)**：`GET /api/agents/{agentId}/skills` + `POST .../approve` +
  `DELETE ...`。反思持久化后自动 propose 一条 `status=proposed, enabled=false`
  的候选技能。**双闸门**：`skillEntryIsActive` 把 `proposed` 一律当非活跃，未审批
  的候选绝不污染 agent prompt。前端 `/agent-learning` 页可一键 Approve / Reject。
- **拍卖市场 + 钱包冻结 (F5)**：`POST /api/marketplace/auctions` /
  `POST .../bids` / `POST .../settle`。`WalletRepo` 的 hold / release / capture 三步
  保证英式拍卖资金安全；`auctionSettlementLoop` 5s/次扫描结算；anti-snipe 自动
  延长 `ends_at`。前端 `/auctions` 页提供最小可用闭环。
- **A/B 经验晋升 + 回滚 (F6)**：`POST /api/abtests/{testId}/promote-learning` +
  `POST .../{promotionId}/rollback`。晋升 = 同一事务三联写（evolution_config /
  long_term 反思 / 候选技能），回滚精确反向。候选技能落地仍卡在 F4 审批闸门，
  A/B 赢的只是"证据"，不会绕开人工审核。
- **工作流调度可观察 + 手工触发 (F7)**：`GET /api/admin/workflow/scheduler`
  +`POST /api/admin/workflow/scheduler/trigger/{fundId}`（super-admin）。前端
  `/admin` 页"每日工作流调度器"卡片每 20s 拉一次 snapshot，按下一触发时刻
  排序展示 + 立即开跑按钮。
- **Prometheus 观测**：`GET /api/metrics` 自带 5 个
  `fundai_marketdata_provider_*` 指标（calls_total / failures_total /
  consecutive_failures / circuit_open / latency_ms_ema），直接接 Prom + Grafana。
  Admin 后台「行情数据源健康」卡片用同一份数据，15s 自动刷新。

### 离线探针：`marketdata-probe`

`server/cmd/marketdata-probe` 是一个独立的 CLI，绕过 HTTP/Auth/DB 层直接调用 `marketdata.Service`，用于快速对真实上游（腾讯财经、Yahoo Chart v8、Eastmoney CMS、Sina Roll、web-search MCP）做端到端冒烟测试。常见用法：

```bash
# A 股标的：腾讯报价 + 中英 hybrid 新闻
go run ./cmd/marketdata-probe -symbol 600519 -market cnstock

# 加上 EN 链路（需先 docker compose up -d web-search-mcp）
MCP_WEB_SEARCH_URL=http://localhost:3004 \
  go run ./cmd/marketdata-probe -symbol 600519 -market cnstock -limit 6

# 美股：Yahoo Chart v8 quote
go run ./cmd/marketdata-probe -symbol AAPL -market us_equity -skip-news

# 期货：akshare 主力合约（需 docker compose --profile market-data up -d 启动 akshare-mcp）
go run ./cmd/marketdata-probe -symbol cu2503 -market futures \
  -akshare-url http://localhost:3002 -skip-news

# 加密货币：公网 CoinGecko v3（无需 key）
go run ./cmd/marketdata-probe -symbol BTCUSDT -market crypto -skip-news

# 覆盖 provider 链：只想用 yahoo 拉 us_equity 报价
go run ./cmd/marketdata-probe -symbol AAPL -market us_equity \
  -quote-providers yahoo -skip-news

# 验证翻译链路（libretranslate 兼容）
go run ./cmd/marketdata-probe -symbol 600519 -market cnstock \
  -translator libretranslate -translator-base-url http://127.0.0.1:5500
```

退出码非零意味着 quote 或 news 拉取失败；输出包含每条新闻的 `source/language` 和 `titleZh/titleEn` 翻译填充情况，可直接用作上游可用性的快速诊断。

示例最小可运行组合：

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

## 常见问题排查

### 决策中心提示「当前报价不可用，执行前需要重新刷新价格」

风控/执行环节读取的是 `marketdata` 提供方返回的实时报价快照。当快照缺失或过期时，前端会提示「当前报价不可用，执行前需要重新刷新价格」。按以下顺序排查：

1. **是否在交易时段内**：A 股、港股等会按交易日历返回报价；非开盘时段大多数 provider 不会返回最新价，建议在交易时段内或使用支持 24×7 的标的（如加密货币）复测。
2. **`MARKETDATA_QUOTE_PROVIDERS` 是否包含可用 provider**：默认值为 `quantdinger,china-stock,akshare`，`.env` 中至少要保留一个可联通的 provider。每个市场的内置默认链：
   - cnstock：`tencent → china-stock → akshare`（前两者免 key）
   - usstock：`yahoo` Chart v8 端点（免 key）
   - futures：`akshare → china-stock → yahoo`（akshare 覆盖国内四大期交所主力合约 SHFE/DCE/CZCE/INE，yahoo 兜底 `GC=F` 等全球期货）
   - crypto：`binance → coinbase → coingecko → yahoo`（Binance / Coinbase 走免 key 公网 WebSocket 实时推流，命中本地 ticker cache 时微秒返回；cache miss/stale 才走 CoinGecko REST 30 req/min；yahoo 兜底 BTC-USD 等主流币）
   - 任何市场如需自定义可设置 `MARKETDATA_QUOTE_PROVIDERS_{MARKET}` 覆盖（详见上文配置表）
3. **provider URL 与凭证是否齐全**：
   - `QUANTDINGER_URL` 留空时 quantdinger provider 会被跳过；
   - `MCP_CHINA_STOCK_URL` / `MCP_AKSHARE_URL` 为空时对应 MCP 也不会启用，可执行 `docker compose --profile market-data up -d` 启动 `china-stock-mcp` 与 `akshare-mcp` 并填入对应地址（默认 `http://china-stock-mcp:3001` / `http://akshare-mcp:3002`，host 直连改为 `http://localhost:<port>`）。
   - CoinGecko 默认走公网免 key 端点；如果需要 Pro 套餐，设置 `MARKETDATA_COINGECKO_BASE_URL` 指向自建反代（由反代注入 `Authorization` 头），不要把 API key 暴露在客户端。
4. **超时或缓存**：`MARKETDATA_PROVIDER_TIMEOUT`（默认 5s）过短会触发 provider 报错；`MARKETDATA_QUOTE_TTL`（默认 10s）过长会让旧快照被沿用，可在排查时临时调小。
5. **观测后端日志**：`app` 容器/进程会输出 `marketdata: quote provider <name> failed` 等错误，结合错误信息确认是网络、签名还是 provider 自身故障。
6. **重新刷新**：以上配置就绪后回到决策中心点击「重新刷新价格」，系统会重新拉取报价并清掉提示。

如果以上步骤仍无法获取报价，可临时将该计划放回「待审批」状态，待行情恢复后再执行。

## License

MIT
