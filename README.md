# AI Fund Platform — 当前仓库实现说明

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
# 推荐只先补三类配置：DATABASE_URL、默认 LLM（LLM_PROVIDER/LLM_MODEL/LLM_API_KEY）、至少一个市场数据源
# 行情可配 QUANTDINGER_URL 或 MCP_CHINA_STOCK_URL / MCP_AKSHARE_URL；新闻推荐直接配置 SERPAPI_KEYS / TAVILY_API_KEYS
# 若切到生产环境，必须替换 JWT_SECRET、MODEL_CONFIG_API_KEY_SECRET、DATABASE_URL、CORS_ORIGINS

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

## 环境变量

参见 [.env.example](.env.example) 获取完整配置说明。当前建议优先关注下面几组配置：

| 分组 | 关键变量 | 说明 |
|------|----------|------|
| 运行时 | `APP_ENV`, `APP_PORT`, `MIGRATIONS_PATH`, `STATIC_FILES_PATH` | 基础服务启动参数 |
| 数据库 | `DATABASE_URL`, `POSTGRES_*`, `DB_*` | 本地开发可直接复用 compose；生产环境必须显式设置安全 `DATABASE_URL` |
| 安全 | `JWT_SECRET`, `MODEL_CONFIG_API_KEY_SECRET`, `CORS_ORIGINS` | 生产环境必须替换默认占位值 |
| 默认 LLM | `LLM_PROVIDER`, `LLM_MODEL`, `LLM_BASE_URL`, `LLM_API_KEY` | 全局默认模型入口；未单独配置的 team agent 会继承这里 |
| tier 覆盖 | `LLM_CRITICAL_*`, `LLM_STANDARD_*`, `LLM_SIMPLE_*` | 可按 critical / standard / simple 覆盖默认模型 |
| provider alias | `OPENAI_*`, `CLAUDE_*`, `ANTHROPIC_*`, `DEEPSEEK_*`, `QWEN_*` | 可直接沿用 provider 原生命名；当 `LLM_*` 未填满时会自动作为 fallback |
| 行情链路 | `MARKETDATA_QUOTE_PROVIDERS`, `QUANTDINGER_URL`, `MCP_CHINA_STOCK_URL`, `MCP_AKSHARE_URL`, `MARKETDATA_COINGECKO_BASE_URL` | 控制全局 quote provider 启用与 fallback 顺序；`QUANTDINGER_URL` 现在只影响 quote。`MARKETDATA_COINGECKO_BASE_URL` 留空走公网免 key 端点（30 req/min 限额），需要 Pro 套餐时可指向自建反代以注入 Authorization 头。**Crypto 默认链已升级为 WS 优先**：`binance → coinbase → coingecko → yahoo`，前两者来自后台 WebSocket 实时推流，详见 F8 行 |
| 团队实时活动 (F2.4) | 路由 `GET /api/funds/{fundId}/team/activity`（REST backfill）与 `GET /api/funds/{fundId}/team/activity/stream`（SSE） | 进程内 ring buffer（默认 200 events/fund）+ 按 fund 隔离的 SSE 广播。前端 TeamManagement 页面会自动接入 `TeamActivityPanel`，显示组合经理、研究员、风控、交易员逐步执行的实时时间线；SSE 断线后用 `?sinceSeq=N` 调 REST 端点做缺口补齐。SSE 依赖 `fundai_session` cookie 鉴权（EventSource 无法设置 Authorization 头）。慢 client 的事件会被 drop（每个订阅器独立计数），不会阻塞工作流 |
| 自学习长期反思 (F3) | 路由 `GET /api/funds/{fundId}/reflections?limit=N`；底层 `memory.Reflect()` 会在每次 `DailyReview` 末尾基于最近 30 天的 `daily`/`agent` 学习记录自动跑一次（按 fund 7 天冷却防止 LLM 烧钱），把同主题（chip、crude、rates…）的若干日学习蒸馏成 1–2 句长期 lesson，写入 `memories.layer=long_term`。前端 `AgentLearning` 页面新增 "长期反思" 区块从这个端点拉取并按主题渲染。**A/B 隔离**：所有读写都按 `fund_id` 在 SQL 层过滤，treatment 基金的反思绝不会泄漏到 control / production 基金，由 `TestRunReflectionCyclePersistsToCorrectFundOnly` 永久锁定。LLM Runtime 未配置时反思整段跳过、不报错 |
| 候选技能库 (F4) | 路由 `GET /api/agents/{agentId}/skills` + `POST .../approve` + `DELETE ...`；每次反思持久化后自动调用 `runtimeSkillProposer` 给基金团队的每个 agent 追加一条 `parsedSkillEntry`，`status=proposed`、`enabled=false`、`source=reflection:<id>`、`match.roles=[agent.role]`、`key=reflection:<reflection-id>`（幂等，重复反思不会写重复）。**安全闸门**：`skillEntryIsActive` 把 `status=proposed` 一律当作非活跃，即使 `enabled` 被外部置为 true 也不会进 prompt resolver，保证未审批的候选技能绝不污染 agent 行为。前端 `AgentLearning` 页面新增 "技能库" 区块，按 PROPOSED / APPROVED 分组展示，用户点 Approve 后服务端置 `status=approved`+`enabled=true`+`approvedAt`，技能立刻被 researcher/PM/trader 的 `appendSkillContext` 调用；Reject 直接从 SkillConfig 移除（下一次反思命中相同主题会重新生成候选） |
| 拍卖市场 + 钱包冻结 (F5) | 路由 `POST /api/marketplace/auctions`（发起）、`POST .../{id}/bids`（出价）、`POST .../{id}/settle`（卖方/cron 结算）。底层模式 `agent_market_listings.mode='auction'`，新增 `auction_started_at/ends_at/reserve_minor/min_increment_minor/anti_snipe_seconds/current_bid_minor/current_bidder_user_id/current_bid_id/winning_bid_id/settled_at` 列。**钱包冻结三步法**：`WalletRepo.HoldFundsWithTx` 锁定账户行 + 校验余额 + 余额-X + 写 `wallet_hold` ledger 行 + 插入 `wallet_holds`（status=active，带 idempotency_key 防重复冻结）；`ReleaseHoldWithTx` 退款 + ledger 反向条目 + 标记 released；`CaptureHoldWithTx` 先退款再走 `TransferWithTx` 转给卖方，buyer ledger 留三行（hold-, release+, settlement-）净额仍为 hold-, seller 只见 settlement+。**英式递增 + anti-sniping**：`PlaceBid` 锁住 listing 行后校验 `bid >= MinNextBidMinor(starting, current_top, min_increment)`；先冻结新出价人的钱、再插入 bid（带 hold_id）、再释放上一个 top bidder 的 hold 并把他的 bid 置 `outbid`、最后用 `ApplyAntiSnipe(ends_at, now, anti_snipe_seconds)` 计算可能延长后的 ends_at 一并写回。后台 `auctionSettlementLoop` 每 5 秒扫一次 `mode='auction' AND status='active' AND ends_at<=now`：达到保留价就 `CaptureHoldWithTx` 转账给卖方 + 克隆 agent + 写 `agent_market_orders` + 标 `status='sold'`；未达保留价或无出价则把所有活跃 hold 退回、标 `status='cancelled'`。前端 `/auctions` 页提供开拍/出价/结算的最小可用闭环 |
| A/B 经验晋升 + 回滚 (F6) | 路由复用 `POST /api/abtests/{testId}/promote-learning` 与 `POST .../{promotionId}/rollback`。Migration 025 把 `ab_test_learning_promotions` 扩 `promoted_memory_ids/promoted_skill_keys/previous_skill_config`。**晋升=三联写（同一事务）**：(1) `agents.evolution_config` 注入 `recentLessons/promotedABLearning` 元数据（沿用旧路径）；(2) `MemoryRepo.CreateWithTx` 把 shadow lessons 物化成 `layer=long_term` 反思插入控制基金 `memories` 表（每条带标签 `ab_promotion`、`ab:<testId>`、`variant:<key>`、`source:ab_test`，最多 12 条）；(3) `mergePromotedSkillsIntoConfig` 把 shadow adjustments 转成 `status=proposed`、`enabled=false`、`source=ab_promotion:<testId>:<variantKey>`、`match.roles=[agent.role]` 的候选技能追加进控制 agent 的 `skill_config`（mode=overwrite 时丢弃前次的 `ab_promotion:*` 旧条目避免堆积；重复 key 仍记入 inserted 列表，让回滚永远幂等）。**安全**：候选技能落地时已被 `skillEntryIsActive` 双闸门拦在 prompt 外，A/B 赢的只是"证据"，最终上线仍走 F4 审批闸门。**回滚**：`RollbackLearningPromotion` 在同事务里恢复 `evolution_config` snapshot、恢复 `skill_config` snapshot、`MemoryRepo.DeleteByIDsWithTx` 精确删除晋升时新增的反思行，并把 `promoted_memory_ids/skill_keys` 清零，因此重复点回滚也是 no-op。**前端**：`ABTestCompare` 在结果卡片新增"已晋升: 反思 ×N · 候选技能 ×M"和"回滚: −N 反思 · −M 候选技能"汇总徽章，把整条经验流水线从 A/B 跑分一路拉到控制基金的反思和技能管理面 |
| Crypto WebSocket 实时推流 (F8) | `MARKETDATA_CRYPTO_WS_ENABLED`（默认 `true`）、`MARKETDATA_BINANCE_WS_URL`、`MARKETDATA_BINANCE_WS_SYMBOLS`、`MARKETDATA_COINBASE_WS_URL`、`MARKETDATA_COINBASE_WS_PRODUCTS`、`MARKETDATA_CRYPTO_WS_STALE_AFTER`（默认 `30s`） | 启动时 `marketdata.Service.StartCryptoStreams` 拉起两条后台 goroutine，分别长连 Binance `wss://data-stream.binance.vision/ws`（订阅 `<symbol>@ticker`）和 Coinbase Exchange `wss://ws-feed.exchange.coinbase.com`（订阅 ticker channel）。两者都**免 key、免费**，每条连接订阅默认 25/22 个主流币对（可用 `_SYMBOLS` / `_PRODUCTS` 覆盖），ticker 事件解析后写入进程内 `cryptoTickerCache`（按 `BTCUSDT`/`BTC-USD` 归一化键存最新 `QuoteSnapshot`）。**默认 crypto 链已重排为 `binance → coinbase → coingecko → yahoo`**：`Quote()` 命中 cache 且新鲜（默认 30s 内）则微秒返回 `Source=binance-ws`/`coinbase-ws`；cache miss 或 stale 则回退到 CoinGecko/Yahoo polling，永不阻塞决策。**重连**：每条流独立的指数退避（1s→30s 上限），断线自动恢复，记入 `provider_health` 表（super-admin `GET /api/admin/marketdata/health` 可见 `binance-ws`/`coinbase-ws` 累计成功/失败次数与最近错误）。**优雅退出**：`Services.Stop` 调 `MarketDataService.Close(5s)` 触发 ctx cancel + 等待 goroutine 退出。`coder/websocket`（零 transitive deps）做底层 WS 客户端，protocol 帧自动 ping/pong，每帧带 90s read 超时兜底僵尸连接 |
| 每日工作流调度可观察 + 手工触发 (F7) | `fundWorkflowScheduler` 现在每个 tick 产出结构化 `FundSchedulerSnapshot`：`lastPollAt/nextPollAt/isLeader/totalActive/triggeredCount` 全局指标 + 每只活跃基金的 `nextTriggerAt`、`nextTradingDay`、`due`、`started`、`lastStatus`、`skipReason`（`not-yet-due`/`next-trigger-error`/`start-error`）、`error`。Snapshot 是线程安全 copy-on-read，admin 端任意频率读取都不会阻塞 leader 的 tick。**REST**：`GET /api/admin/workflow/scheduler` 返回当前 snapshot（super-admin only，调度器未接线时降级 503）；`POST /api/admin/workflow/scheduler/trigger/{fundId}`（body 可空，可带 `{"tradingDate":"YYYY-MM-DD"}` 指定交易日）走 `workflowService.AdminTriggerFund` → `startWorkflowForFundWithMode(forceImmediate=true)`，与 cron 共用 `workflow_run` 行 claim 防止双触发；操作写 audit log。**Schedule interface 解耦**：`fundWorkflowScheduler.service` 改为 `schedulerService` 接口（6 个窄方法：ListActiveFunds/GetWorkflowRun/NextWorkflowStart/SessionForDate/TradingProfileForFund/StartWorkflowForFund），生产由 `workflowServiceAdapter` 实现，测试用 `stubSchedulerService` 即可端到端验证 due/notDue × 无 run/completed/failed/running 触发矩阵。**前端**：`/admin` 页新增"每日工作流调度器"卡片，每 20s 拉一次 snapshot，按下一触发时刻排序展示每只基金的日历 / 下次开跑时间 / outcome 徽章 / "立即开跑"按钮 |
| 行情按市场分链 (F1.5) | `MARKETDATA_QUOTE_PROVIDERS_{CNSTOCK\|USSTOCK\|HKSTOCK\|FUTURES\|CRYPTO}` | 每个市场独立覆盖 provider 链。若设置则该市场用此列表覆盖全局 `MARKETDATA_QUOTE_PROVIDERS`，全局列表与默认链仍会在尾部追加（去重）以保证 fallback。推荐基线：`_CNSTOCK=tencent,akshare`、`_USSTOCK=yahoo`、`_FUTURES=akshare,yahoo`（akshare 覆盖 SHFE/DCE/CZCE/INE，yahoo 兜底 `GC=F` 等全球期货）、`_CRYPTO=binance,coinbase,coingecko,yahoo`（前两者来自后台 WS 推流即开即用，coingecko/yahoo 兜底） |
| 新闻链路 | `MARKETDATA_NEWS_PROVIDERS`, `SERPAPI_KEYS`, `TAVILY_API_KEYS`, `MCP_WEB_SEARCH_URL`, `WEB_SEARCH_FEED_URL`, `EASTMONEY_NEWS_BASE_URL`, `SINA_NEWS_BASE_URL`, `MARKETDATA_NEWS_HYBRID` | 控制 news/search provider 启用与 fallback 顺序；A 股标的会自动优先走 `eastmoney` + `sina` 两个免 key 的中文原生新闻源，其它市场仍沿用 SerpAPI / Tavily / `web-search-mcp` RSS。`MARKETDATA_NEWS_HYBRID=true`（默认）会把 ZH 与 EN 两条 provider 链路并发拉取后合并去重，让用户同时看到本地中文报道与英文宏观/分析师视角；设为 `false` 回到传统的单链路 fallback |
| 新闻翻译 | `MARKETDATA_TRANSLATOR_PROVIDER`, `MARKETDATA_TRANSLATOR_BASE_URL`, `MARKETDATA_TRANSLATOR_API_KEY`, `MARKETDATA_TRANSLATOR_MODEL`, `MARKETDATA_TRANSLATOR_TIMEOUT` | 可插拔的 news 翻译器，把 hybrid 拉到的中文/英文标题与摘要补齐为另一种语言。`none`（默认）= 不翻译；`libretranslate` = 调用 LibreTranslate 兼容服务；`openai-compat` = 调用任意 OpenAI 兼容 `/chat/completions`（DeepSeek、OpenAI、Qwen-compat、本地 LLM 等），需要同时填 `_MODEL`。翻译结果以 `titleZh/titleEn/summaryZh/summaryEn` 形式返回，前端按用户语言选择展示 |
| 行情韧性 | `MARKETDATA_STALE_AFTER`, `MARKETDATA_CIRCUIT_FAILURES`, `MARKETDATA_CIRCUIT_COOLDOWN`, `MARKETDATA_ADAPTIVE_TTL` | Phase 3a 加固：当 `QuoteSnapshot.AsOf` 超过 `STALE_AFTER`（默认 15m）时 `isStale=true`，决策中心会附带 `quote outdated (age: …)` 提示；同一 provider 连续失败到 `CIRCUIT_FAILURES`（默认 3）次后熔断 `CIRCUIT_COOLDOWN`（默认 30s）才允许重试；`ADAPTIVE_TTL=true`（默认）在主要市场休市时段把 news TTL 放大到 3×（上限 10m）以节省上游配额。每个 provider 的累计调用数、连续失败次数、上次错误、EMA 延迟可通过 super-admin 的 `GET /api/admin/marketdata/health` 查看 |
| 硬风控 / 风控阻断 | 内置 `StaleQuoteGuard`（fund 配置中的 `hardRisk.maxQuoteAgeSeconds`，默认 15m，最长 24h） | Phase 3c：执行层会把当前报价的 `AsOf` 与 `isStale` 注入 `risk.ProposedTrade`。如果是 risk-increasing 单（买/加仓/开空）但报价过期，硬风控直接 reject 并附 `hard_stale_quote_guard` 规则名，前端会在决策中心弹出"先刷新报价再下单"的提示；卖出/平仓/减仓不受此规则限制，保证报价异常时仍能 de-risk |
| 硬风控 fund 级覆盖 | 基金设置页 → "硬风控覆盖" / "Hard risk overrides" 区块，对应 API `PUT /api/funds/:fundId` 中的 `hardRisk.maxQuoteAgeSeconds`（int 秒，范围 `1..86400`，传 `0` 表示清除该 fund 的覆盖回到平台默认） | Phase 3e：每只基金可以独立设置 `StaleQuoteGuard` 的容忍时间。对长线策略可以放宽到 1h，对高频或事件驱动策略可以收紧到 60s。表单为空时自动继承平台默认；超过 24h 或非整数的取值会在 server 端被静默丢弃，避免运营误操作把硬风控关掉 |
| Prometheus 观测 | `GET /api/metrics` 现在多出 5 个 `fundai_marketdata_provider_*` 指标 | `_calls_total{provider}` / `_failures_total` / `_consecutive_failures` / `_circuit_open` / `_latency_ms_ema`。可以直接接 Prom + Grafana 报警面板。Admin 后台的"行情数据源健康"卡片也基于同一份数据，每 15s 自动刷新 |

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
