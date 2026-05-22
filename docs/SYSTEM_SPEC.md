# AI Fund Platform — 系统说明文档

> 多 Agent 团队组成的跨市场交易系统：A 股、美股、期货（贵金属/能源）、虚拟货币。本文档是系统的**目标态规格**，而非当前实现状态。当前实现状态请参见 [README.md](../README.md) 的"当前真实能力"章节。

---

## 0. 文档约定

- **必备 (MUST)**：核心闭环，缺失则系统不能视为达到目标
- **应当 (SHOULD)**：显著提升体验/价值，但可以分批落地
- **可选 (MAY)**：长期方向，本期不阻塞

---

## 1. 系统全景

### 1.1 一句话定义

允许用户**注册基金公司**、为公司**创建多支基金**（每支基金对应一支 AI Agent 团队，一只基金一支团队），团队按可配节奏在 A 股 / 美股 / 期货 / 加密四大市场协作交易，并通过**记忆 + 反思 + 技能库**持续自我进化；用户可以对同一团队同时跑**正式策略**和**A/B 影子策略**进行对照实验，并可在 **Agent 市场**挂牌买卖训练好的 Agent。

### 1.2 顶层架构

```
┌──────────────────────────────────────────────────────────────────┐
│                       Web SPA + WeChat Miniapp                   │
│      Companies → Funds → Teams → Decisions                        │
│      Marketplace · AB-Compare · MemoryCenter · AgentLineage       │
├──────────────────────────────────────────────────────────────────┤
│                        Go REST API (gateway)                     │
├──────┬───────────┬──────────┬──────────┬──────────┬───────────────┤
│Auth/ │ Workflow  │ Agent    │ A/B Test │ Market   │ Marketplace   │
│KYC   │ Scheduler │ Runtime  │ Service  │ Data     │ + Auction     │
│Bill  │ (cron)    │ (PM/R/T/ │ Service  │ Service  │ Service       │
│      │           │  Risk)   │          │          │               │
├──────┴───────────┴──────────┴──────────┴──────────┴───────────────┤
│  Memory · Reflexion · Skill Library · Lineage · Risk Engine      │
├──────────────────────────────────────────────────────────────────┤
│            PostgreSQL 16  +  Redis (cache, rate-limit)            │
├──────────────────────────────────────────────────────────────────┤
│  Market data providers (per-market fallback chain)                │
│  ├─ A-share : tencent · sina · eastmoney · akshare · tushare      │
│  ├─ US      : yahoo v8 · polygon · serpapi · tavily                │
│  ├─ Futures : akshare(CN) · iTick / polygon (global)              │
│  └─ Crypto  : binance · coinbase · coingecko                       │
├──────────────────────────────────────────────────────────────────┤
│      LLM providers : OpenAI · Anthropic · Gemini · 自定义         │
└──────────────────────────────────────────────────────────────────┘
```

---

## 2. 域模型

```
User ─owns─> Company ─has─> Fund ─has─> Team
                                                  │
                                                  │ runs
                                                  ▼
                                            DailyWorkflow ─emits─> Plan
                                                  │
                                                  ├─ Trade(s) ──> NAV history
                                                  ├─ Memory ──> Reflection ──> Skill
                                                  └─ Lineage edges

Team ⤜ composed of ⤛ Agents (PM / Researcher × N / Trader / Risk)
                              │
                              │ may be listed
                              ▼
                       Marketplace Listing
                              │
                              ├─ Buyout (一口价)
                              ├─ Subscribe (订阅)
                              └─ Auction (English ascending + 反狙击) ◄── 新增
```

### 2.1 实体定义

| 实体 | 关键字段 | 备注 |
|-----|---------|------|
| `User` | id, email, kyc_status, balance | 平台用户 |
| `Company` | id, owner_user_id, name | 基金公司，1 user 可拥多 company |
| `Fund` | id, company_id, market, asset_class, trading_mode(sim/paper/live), nav, hard_risk | 基金本身；公司下可挂多支基金，每支基金对应一个独立的 Agent 团队 |
| `Team` | id, fund_id | 一个 fund 一个 team |
| `Agent` | id, team_id, role(pm/researcher/trader/risk), persona, skill_library, model_config | 团队成员 |
| `DailyWorkflow` | id, fund_id, trading_date, status, current_step | 单日运行实例 |
| `Plan` | id, fund_id, status(draft→risk→pending→approved→executing→completed), actions[] | PM 产出的待审批计划 |
| `Trade` | id, plan_id, symbol, side, qty, price, fees, slippage | 实际执行的成交 |
| `MemoryItem` | id, agent_id, fund_id, layer(raw/long_term/dream), kind(decision/outcome/reflection), tags, importance | 单条记忆 |
| `Reflection` | id, fund_id, theme, lesson_text, source_memory_ids[], generated_at | 由 Reflexion 任务从 raw 升华出的长期教训 |
| `Skill` ★新增 | id, agent_id, name, code/prompt, source_reflection_ids[], success_rate, usage_count | Voyager 风格的可累积技能 |
| `LineageEdge` | child_agent_id, parent_agent_id, via(buyout/subscribe/abtest/manual_copy) | Agent 血缘 |
| `Listing` | id, agent_id, mode(buyout/subscribe/**auction**), price, status | Marketplace 挂牌 |
| `Auction` ★新增 | listing_id, reserve_price, current_bid, end_at, anti_snipe_window, bids[] | 拍卖元数据 |
| `Bid` ★新增 | id, auction_id, bidder_id, amount, ts | 一次出价 |
| `ABTest` | id, control_fund_id, treatment_fund_id, variable, status, results | A/B 实验 |
| `LearningRun` ★新增 | id, fund_id, scope(prod/abtest_xxx), input_memories_count, generated_reflections, generated_skills, can_promote | 一次完整学习 run，明确标注 prod / shadow |

★ = 当前仓库**不存在**，本文档新增。

---

## 3. Agent 团队组成（基于行业调研）

### 3.1 业界基准（2026）

参考 Resonanz Capital "2025 Hedge Fund Talent Tape"、Millennium-style pod 结构：

- 单个 pod / team 典型规模：**1 PM + 1–3 Analyst + 1 Trader + 1 Risk overseer + 数据/工程 共享**
- 单 pod 资金量：$100M–$200M
- 2025–2026 趋势：**Risk / Data / Engineering 三类角色配比上升**

### 3.2 平台映射

| 角色 | 数量 | 平台对应 | 主要职责 |
|------|------|----------|----------|
| **Portfolio Manager (PM)** | 1（必选） | `agent.PMAgent` | 汇总研究意见、产出 `Plan`、最终决策；要求用户审批多策略时按条审批 |
| **Researcher** | 1–5（推荐 3） | `agent.ResearcherAgent` | 各自负责一组标的，按"宏观 / 基本面 / 技术面 / 量化"四个分析方向交叉，产出 `ResearchBrief` |
| **Trader** | 1（必选） | `agent.TraderAgent` | 执行 plan，按标的特性选 immediate/TWAP/VWAP/limit，控制滑点 |
| **Risk Overseer** | 1（必选） | `agent.RiskAgent` + `risk.HardRiskPolicy` | 软风控（agent 评论 + 警告）+ 硬风控（确定性规则拒单） |
| **Researcher 专精** | 1 名以上 Researcher 必须打 `specialization` 标签 | `FundSpecialization` | 例如 "CPO / 半导体 / AI infra" 之于美股、"白酒 / 新能源" 之于 A 股 |

### 3.3 配置约束

- 每个 Fund **MUST** 至少有 1 PM + 1 Trader + 1 Risk + 1 Researcher
- Researcher 上限默认 5；超过时按 PM 的"重要性打分"截断送入 PM prompt（已存在 `maxPromptConsensus = 30`）
- 一个 Agent 同时只能属于一个 Fund 的 active 团队；可以同时被多个 Listing 挂出去（subscribe / auction）
- 通过 marketplace 买入/订阅的 Agent，在加入新团队前**MUST** 经过 sandbox 跑 N 天 paper 模式（默认 5 个交易日）才能切 live

---

## 4. 交易日工作流（10 步）

复用现有 `workflow.daily.go` 的 10 step 定义。每一步都 emit `WorkflowEvent` 到 EventBus，前端可订阅实时进度。

| Step | 触发 | 主要 Agent | 输出 |
|------|------|-----------|------|
| `macro_brief` (09:00) | cron | 全部 Researcher 共读 | 当日宏观日报 → Memory |
| `research_parallel` (09:30) | cron | 每个 Researcher 跑各自的 watchlist | N 份 `ResearchBrief` |
| `quant_signals` (10:00) | cron | Quant Researcher / 工具 | 技术面信号入 Memory |
| `roundtable` (10:30) | cron | 全部 Researcher + PM | 多空辩论 → 共识表 |
| `pm_plan` (11:00) | cron | PM | `Plan` 草稿（含多条 Action） |
| `risk_review` (11:10) | cron | RiskAgent + HardRiskPolicy | 通过 / 警告 / 拒绝列表 |
| `user_approval` (11:30) | **必选交互** | — | 用户逐条审批多策略 |
| `trade_execution` | 用户批准后 | TraderAgent | 实际下单 + 成交记录 |
| `settlement` (15:00) | cron | 系统 | 当日 NAV、P&L、归因 |
| `daily_review` (15:30) | cron | PM | 当日复盘 → Memory + 触发 Reflexion |

**间隔可配**：`teamIntervals.{pm,researcher,trader,risk}` 在基金设置页。盘中可主动触发研究刷新，不必等 cron。

**多策略审批 UX**：DecisionCenter 页面对每个 Action 都给 ✓ ✗ ⏸ 三个按钮。批准后 plan 的 status 推进；任一条被拒整 plan 走 `rejected`（也可配置为"批准其余、拒绝单条"）。

---

## 5. 自我学习闭环（核心创新）

### 5.1 三层学习架构（受 Voyager + Reflexion 启发）

```
        ┌─────────────────────────────────────────────────┐
        │   Layer 1: Episodic Memory (raw events)         │
        │   每个 decision/outcome/news_read 都进 raw       │
        │   importance 评分自动计算                         │
        └────────────────┬────────────────────────────────┘
                         │ Reflexion job (每日收盘后)
                         ▼
        ┌─────────────────────────────────────────────────┐
        │   Layer 2: Long-term Memory (lessons)           │
        │   按主题（symbol / sector / event_type）聚类     │
        │   LLM distill → 一段话教训                       │
        └────────────────┬────────────────────────────────┘
                         │ Skill extraction job (每周)
                         ▼
        ┌─────────────────────────────────────────────────┐
        │   Layer 3: Skill Library (executable knowledge) │
        │   "earnings_beat_chase" / "vix_spike_hedge" ...  │
        │   每条 skill 是 prompt 片段 + 触发条件 +         │
        │   过往胜率统计，会被 PM/Researcher 在 prompt     │
        │   构造时按相关性召回                              │
        └─────────────────────────────────────────────────┘
```

### 5.2 学习触发条件

- **每日收盘**: `daily_review` step 之后立即触发 Reflexion，产出当日的 lesson
- **每周**: 周末触发 Skill extraction，把高置信度 lesson 提升为可调用 skill
- **大事件**: 单日 P&L 超过 `±2σ` 或 max drawdown 触发额外 reflection（用 `importance` 加权）

### 5.3 学习对象

每条 lesson / skill 都关联**主体范围**：

- `agent_id` — 该 agent 独有
- `team_id` — 团队共享（PM 总结的跨标的经验）
- `fund_id` — 基金级（用于切换 PM 后保留团队 know-how）
- `marketplace` — 公开可购买的知识（escape valve）

### 5.4 学习的"覆盖"语义（A/B 隔离 + 提升）

```
┌─────────────────────────┐         ┌─────────────────────────┐
│ Prod LearningRun        │         │ A/B Shadow LearningRun  │
│ scope = "prod"          │         │ scope = "abtest_xxx"    │
│ memories writable       │         │ memories writable       │
│ reflections writable    │         │ reflections writable    │
│ skills writable         │         │ skills writable         │
└─────────────────────────┘         └─────────────────────────┘
        ▲                                   │
        │      Promote / Merge (user)       │
        └───────────────────────────────────┘
```

- A/B 跑出来的 reflection/skill 默认**只在 shadow 命名空间生效**
- AB 分析页提供"Promote winning lessons"按钮，把选中的 reflection/skill 拷贝到 prod scope
- 现有 `lineage.ViaABTestClone` 用来追踪这条提升路径

---

## 6. A/B 实验：严格隔离

### 6.1 隔离边界

| 资源 | 隔离方式 |
|------|---------|
| Fund 配置 | Clone fund，treatment 修改单一变量（agent_swap / risk_rule / pm_strategy / model_change / skill_change） |
| Trade 执行 | Shadow 模式：treatment fund 不下真实单（即使主 fund 是 live），所有 trade 标 `is_shadow=true` |
| Memory | scope tag = `abtest_{id}`，prod 召回不会读 shadow，反之亦然 |
| Skill | 同 memory；shadow skill 只对 shadow agent 可见 |
| NAV/账户 | shadow fund 用虚拟资金，独立的 ledger |
| 行情/新闻 | 共享（行情是同源 fact） |
| LLM 调用 | 共享（但计 usage 时打 `abtest_run` 标签便于成本归因） |

### 6.2 用户操作

1. 选 control fund + variable → CreateTest
2. 选时间窗口 → StartTest（cron 推进或 fast-forward 跑历史日）
3. 实时看 control vs treatment 的 NAV / Sharpe / divergence
4. 结束后 AnalyzeTest → 提示 winner + 95% 置信度
5. 用户决定：
   - **Promote**：把 treatment 的变量 + lessons 应用到 prod
   - **Reject**：删除 treatment fund + shadow data
   - **Keep as reference**：保留 shadow，但不影响 prod

---

## 7. Agent Marketplace + Auction

### 7.1 上架条件（已实现：`marketplace.EligibilityPolicy`）

- forward-test 至少 N 天（默认 50 个交易日）
- 至少 N 个 NAV 数据点
- 必须有非负累计收益（可配）
- 通过 lineage 反洗钱检查（不能上架自己刚买回来的衍生品）

### 7.2 三种成交模式

| 模式 | 适用场景 | 当前状态 |
|------|---------|---------|
| **Buyout** 一口价 | 卖家想立刻变现，定价合理 | ✅ 已实现 |
| **Subscribe** 订阅 | 卖家想长期收租 | ✅ 已实现 |
| **Auction** 拍卖 ★ | 价格难评估、买家多、卖家想最大化 | ❌ 待实现 |

### 7.3 Auction 设计（English ascending + 反狙击）

参考学术结论（Milgrom-Weber linkage principle）：**English ascending** 和 **sealed-bid second-price** 在 valuations 相关时收益最高。English 对 UI/UX 更友好，故选 **English ascending + anti-sniping extension**：

```
卖家上架：
  reserve_price (保留价，必须 ≥ ListingMode.Buyout 时的最低价)
  start_at (默认立即)
  duration (默认 7 天)
  anti_snipe_window (默认 5 分钟)
  bid_increment (默认 5% 或 ¥10 取较大)

买家出价：
  1. 出价 ≥ current_bid + bid_increment
  2. 平台冻结买家钱包中的对应金额（防赖账）
  3. 之前的 highest bidder 钱包解冻
  4. 如果出价时间在 [end_at - anti_snipe_window, end_at]：
        end_at += anti_snipe_window  ← 反狙击关键
  5. 公开广播新 high_bid + 新 end_at

结束：
  1. cron 到 end_at 检查，无 unsettled 出价后：
     - 若 current_bid ≥ reserve_price → 成交，调用统一的 LineageEdge + 钱包结算逻辑
     - 否则 → 流拍（reserve not met），所有冻结金额解冻
  2. 卖家可以选 "Anytime buy now"：在拍卖期间随时点击成交（按当前最高价），跳过倒计时

费率：
  - 平台抽佣（与 buyout 相同 fee schedule）
  - 卖家可选 "VIP 拍卖"：广告位 + 更低费率（订阅产品挂钩）
```

### 7.4 防作恶

- **Shilling**（卖家自抬价）：禁止同一支付实体 / 同一 KYC 主体在自己拍卖出价
- **Bid sniping**：anti-snipe 已经解决
- **Wallet front-running**：冻结而非托管，链下账本即可
- **Failed delivery**：复用现有 `marketplace.Reconciler`，拍卖也走 pending → completed 状态机

---

## 8. 跨市场数据源策略

基于 2026 业界调研（APIScout、TickDB benchmark）：

### 8.1 推荐源（按市场）

| 市场 | 行情主源 | 行情备源 | 新闻 / 研报 |
|------|----------|---------|-----------|
| **A 股** | Tushare Pro（基本面 + 实时） | tencent / sina / akshare / china-stock-mcp | eastmoney + sina + 财联社（用 web-search MCP） |
| **港股** | iTick / TickDB | tencent / sina | 港交所披露易 + 财联社 |
| **美股** | Polygon.io（低延迟实时） | Yahoo v8 / Alpha Vantage | SerpAPI Google News + Tavily |
| **期货（CN）** | Akshare（SHFE/DCE/CZCE/INE） | 中金所 CFFEX 网站 | 期货日报 + 文华财经 |
| **期货（全球）** | TickDB / iTick | Polygon Futures | Bloomberg / Reuters via SerpAPI |
| **加密** | Binance native WebSocket | Coinbase + CoinGecko | CoinDesk + Twitter（受限） |

### 8.2 系统约束

- 每个 provider **必须**实现 `QuoteProvider` 或 `NewsProvider` interface
- 每个 provider **必须**有：超时、重试、熔断器（已有 `providerHealthTracker`）
- 多 provider 用 **per-market fallback chain** 配置 in `MARKETDATA_QUOTE_PROVIDERS_{MARKET}`，比单一全局链路更合理
- **加密**（F8 done）：默认走 WebSocket 推送（节省 API 配额）—— `marketdata.Service.StartCryptoStreams` 启动 Binance + Coinbase 两条长连，ticker 事件写入进程内 `cryptoTickerCache`；`binance` / `coinbase` quote provider 微秒级从 cache 读取，stale (>30s) 或 miss 时回退到 CoinGecko REST 30 req/min 兜底；连接断线由内置指数退避自动重连，超时帧由 90s read 超时兜底僵尸连接。**期货**仍走 polling（akshare → china-stock → yahoo），未来若有 tick 级需求可同模式拓展
- 跨市场标的查询走统一的 `InstrumentRef{Symbol, Market, AssetClass}`，已存在

### 8.3 现状 gap（F1 后更新）

| 市场 | 现有 provider | 缺失 |
|------|-------------|------|
| A 股 | tencent / sina / eastmoney / akshare-mcp / china-stock-mcp | ✅ 覆盖充分（运营选择跳过 Tushare Pro 付费 API） |
| 美股 | yahoo v8 | ✅ 覆盖充分（运营选择跳过 Polygon 付费 API） |
| 期货 | **akshare-mcp（F1.3 接入）** + yahoo（GC=F 等全球期货）| ⬜ iTick / TickDB（生产级延迟更低） |
| 加密 | **F8 Binance + Coinbase WebSocket（实时推流、免 key、零成本）→ CoinGecko v3（30 req/min 兜底）→ Yahoo（BTC-USD 等主流币兜底）** | ✅ done |

**F1.5（已完成）：per-market fallback chain。** 通过 `MARKETDATA_QUOTE_PROVIDERS_{CNSTOCK|USSTOCK|HKSTOCK|FUTURES|CRYPTO}` 可独立覆盖单个市场的 provider 链，全局 `MARKETDATA_QUOTE_PROVIDERS` 与内置默认链仍在尾部追加（去重）以保证 fallback。详见 README 的"行情按市场分链"配置说明。

---

## 9. 非功能性需求

### 9.1 韧性（已部分完成 Phase 3）

- ✅ Quote stale detection
- ✅ Provider 熔断（连续失败 N 次开熔断 M 秒）
- ✅ Adaptive news TTL
- ✅ Hard risk gate + StaleQuoteGuard
- ⬜ **MUST**: 全部 outbound HTTP 都进熔断器（不仅 marketdata）
- ⬜ **SHOULD**: trade execution 重试 + 幂等

### 9.2 观测（已部分完成 Phase 3c/3d）

- ✅ `/api/metrics` Prometheus 指标
- ✅ Admin Console provider health 面板
- ⬜ **SHOULD**: Grafana dashboard JSON + alert rules（如果上 oncall）
- ⬜ **SHOULD**: Workflow 每步耗时 + 失败计数指标
- ⬜ **MUST**: 用户操作审计日志（已有 audit 包，需要补 marketplace / abtest 关键操作）

### 9.3 安全

- ✅ KYC 框架（页面 + 路由）
- ✅ JWT + `MODEL_CONFIG_API_KEY_SECRET`
- ⬜ **MUST**: marketplace 钱包冻结 / 拍卖资金扣留必须经过 UnitOfWork（防双花）
- ⬜ **MUST**: 跨用户 fund 访问严格 RBAC（owner / 团队成员 / 订阅者只能访问自己应有的资源）
- ⬜ **SHOULD**: rate limiting（已有审计但没有限频）

### 9.4 多语言

- ✅ News i18n（titleZh / titleEn）+ 翻译器（Phase 2）
- ✅ FundSettings / Admin / DecisionCenter / Dashboard 双语 copy
- ⬜ **SHOULD**: 全部页面双语 + 站点级别 i18n 配置

---

## 10. 当前实现 vs 目标 — Gap Matrix

| 域 | 当前 | 目标 | gap 级别 |
|----|------|------|---------|
| Market data — A 股 quote | ✅ | — (Tushare 跳过付费) | done |
| Market data — US quote | ✅ Yahoo | — (Polygon 跳过付费) | done |
| Market data — Futures | ✅ F1.3 (akshare + yahoo) | iTick / TickDB | low |
| Market data — Crypto | ✅ F8 Binance + Coinbase WebSocket 实时推流（默认链 `binance → coinbase → coingecko → yahoo`） | — | done |
| Per-market fallback chain | ✅ F1.5 | — | done |
| News + 翻译 | ✅ Phase 2 | — | done |
| Quote 韧性 | ✅ Phase 3a | — | done |
| Hard risk + Stale guard | ✅ Phase 3c | — | done |
| Prom 指标 | ✅ Phase 3c | + Grafana | low |
| Admin health UI | ✅ Phase 3d | — | done |
| Fund-level hard risk | ✅ Phase 3e | + 其他字段 | low |
| **Department 层** | — | 用户决定不需要（公司 → 基金 → 团队即可） | done |
| Team CRUD（add/remove agent） | ✅ AddAgent/RemoveAgent/UpdateAgent/BindAgent/ListTeam 已实现 | — | done |
| **Team Live Activity (F2.4)** | ✅ F2.2 ActivityBus + F2.3 REST/SSE 端点 + F2.4 前端 TeamActivityPanel | — | done |
| **Researcher / PM / Trader / Risk runtime** | ✅ F7 端到端贯通：`runtimeResearcherPool`（macro brief / 多 focus 并行 research / quant signals / roundtable，含市场数据 + LLM）→ `runtimePMAgent`（GeneratePlan + 风格 + 风控规则 + LLM 推理）→ `runtimeApprovalGateway`（人审或自动）→ `runtimeRiskAgent`（ReviewPlan 含 sector concentration / drawdown / liquidity）→ `runtimeTradingEngine`（按 plan action 执行真实下单、撮合、持仓更新、NAV 快照）。所有 5 个角色都注入 LLM Runtime 并写入 `team_activity` 事件总线 | — | done |
| **DailyWorkflow scheduler** | ✅ F7 `fundWorkflowScheduler` 完成可观察化：`triggerDueFunds(now)` 返回结构化 `FundSchedulerSnapshot`（每只活跃基金的 next trigger / due / lastStatus / skipReason / error）+ `Snapshot()` API（线程安全、copy-on-read）+ leader-only 触发（lease 名 `workflow-scheduler`）+ 失败不阻塞下一只基金。运行时通过 `workflowService.scheduler.Start()` 在 `initServices` 启动 | — | done |
| **Workflow 调度可观察 + 手工触发 (F7)** | ✅ `GET /api/admin/workflow/scheduler` 返回 snapshot（super-admin 限定）；`POST /api/admin/workflow/scheduler/trigger/{fundId}` 复用 `startWorkflowForFundWithMode(forceImmediate=true)`，与 cron 走同一 `workflow_run` 行 claim 防止双触发；`adminTriggerFund` 写 audit log；Admin 前端 `/admin` 加 "每日工作流调度器" 表盘，列每只基金的下一交易日 / trigger time / leader 标识 / outcome / "立即开跑" 按钮，每 20s 自动刷 | — | done |
| **Crypto WebSocket 实时推流 (F8)** | ✅ `marketdata.Service.StartCryptoStreams(ctx)` 在 `initServices` 启动两条后台 goroutine：`binanceWSStream` 长连 `wss://data-stream.binance.vision/ws` 订阅 `<symbol>@ticker`，`coinbaseWSStream` 长连 `wss://ws-feed.exchange.coinbase.com` 订阅 ticker channel，**均免 API key**；ticker 事件解析后写进程内 `cryptoTickerCache`（键归一化为 `BTCUSDT` 形式，跨两个 source 共享）。**默认 crypto 链重排为 `binance → coinbase → coingecko → yahoo`**：cache 命中且新鲜 (<30s) 微秒返回 `Source=binance-ws`/`coinbase-ws`，miss/stale 回退到 CoinGecko REST。**重连**：每条流独立指数退避（1s→30s 上限）+ 90s read deadline 防僵尸；失败计入 `provider_health` 表（`binance-ws`/`coinbase-ws`），super-admin `GET /api/admin/marketdata/health` 可见。**优雅退出**：`Services.Stop` 调 `MarketDataService.Close(5s)` 触发 ctx cancel 等 goroutine 结束。底层用 `github.com/coder/websocket`（零 transitive deps），自动 ping/pong。可通过 `MARKETDATA_CRYPTO_WS_ENABLED=false` 关闭以纯走 polling | — | done |
| **Workflow 步进持久化 (F9.1)** | ✅ 每个 step 完成 / pause / fail 都把 orchestrator state 同步写入 `workflow_runs` 表。实现：`internal/workflow/daily.go` 给 `WorkflowEvent` 加 `Snapshot *WorkflowState` 字段，`emit()` 把当前 state 深拷贝挂到事件上；`cmd/server/persisting_eventbus.go` 包一层 `persistingEventBus`（先转发给 activity SSE bus，再按事件类型白名单 (`run_*` / `step_*` / `awaiting_user`) 调 `persistRuntimeState`）；wiring 把它取代直接的 `activityBus` 注入 orchestrator。修复 F8 smoke test 发现的 bug：原本 `runFullWorkflow` 只在 `RunFull` 整体返回后才持久化，而 `RunFull` 会阻塞在 `WaitForDecision` 上几分钟到几小时，导致 admin 看到的 workflow_runs 长期停留在 `macro_brief` 即使活动流早已走到 `user_approval` | — | done |
| **API 未匹配路径返回 JSON 404 (F9.3)** | ✅ `spaHandler` 在 fall-through 前先用 `isAPILikePath` 判断（`/api/...` / `/events/...`）—— 命中则返回 `{"error":"not_found","path":...,"method":...}` 而不是 SPA `index.html`。修掉 fetch / curl 调用者看到 200 + HTML 却无法解析 JSON 的隐性失败 | — | done |
| **fund CRUD 唤醒 workflow scheduler (F10.1) + 兜底 10 分钟 cap (F10.2)** | ✅ `fundWorkflowScheduler.Wake()` 暴露公开 API（nil-safe / pre-Start safe / idempotent），`workflowServiceAdapter.WakeScheduler()` 在 `CreateFund` / `UpdateFund` / `DeleteFund` 调用以让新基金在毫秒级被调度器看到（而不是等下一个长达数小时的 `nextPollAt`）。同时 loop sleep 用 `clampLoopDelay` 强制上限 10 分钟（`schedulerMaxIdleDelay`）作为 fail-safe：即使没人调 Wake / leader 短暂失联 / 时钟漂移，也最多 10 分钟之内必再扫一次基金集 | — | done |
| **Calendar / Benchmark 默认值 (F11.1 + F11.2)** | ✅ F11.1：`marketcalendar.NormalizeProfile` 把 timezone 推断逻辑挪到 calendarCode defaulting **之后**——原本空入参会得到 `US-XNAS + UTC` 这种半正确组合，现在会得到 `US-XNAS + America/New_York`；crypto 通路（`market=crypto` 或 `assetClass=crypto` 或 `exchange in {BINANCE, COINBASE, OKX, BYBIT}`）仍可靠地落到 `CRYPTO-24X7 + UTC`，新增 5 个回归 test 锁死。F11.2：`inferWorkflowSymbolWithCandidates` 在 `benchmark` 之后加 `defaultBenchmarkForMarketProfile`（crypto → BTC-USD / a_share → 000300.SS / us_equity → SPY / futures → ES=F），优先于 `fallbackWorkflowSymbolFromFundName`；后者新增 `fallbackNonTickerWords` 阻断 `SMOKE` / `TEST` / `ALPHA` / `MACRO` / `CRYPTO` 等 40+ 个常见非 ticker 词，避免污染 quote provider | — | done |
| **路由 alias + Company input 文档 (F11.3 + F11.5)** | ✅ F11.3：`pathAliasMiddleware` 把 `/api/ab-tests/...` 重写到规范的 `/api/abtests/...`，让两种拼写都能命中正确 handler，单元测试覆盖。F11.5：`POST /api/companies` 明确仅接受 `{name, description?}`（README 与 SYSTEM_SPEC 都已对齐），多余字段被 `DisallowUnknownFields` 拦下返回 400 + 字段名 | — | done |
| **rebuild-app 脚本 (F10.3)** | ✅ `scripts/rebuild-app.sh [SERVICE=app]` 单独 rebuild docker 镜像 + 重建容器（postgres / volumes 不动），并 wait `/api/health` 通过。`Makefile` 提供 `make rebuild / make rebuild-app / make build / make test` 几个常用 target。修掉 smoke test 观察到的 stale image 陷阱（容器里跑的是几天前的二进制，看着对但缺新 schema） | — | done |
| Memory raw layer | ✅ | — | done |
| **Memory long-term (reflection)** | ✅ F3.1 `maybeRunReflection` 接到 `ConsolidateDaily` 末尾 + 7 天 cadence 冷却 + LLM Distiller (Standard tier) | — | done |
| Reflection 读端点 | ✅ F3.2 `GET /api/funds/{fundId}/reflections?limit=N` + `reflectionServiceAdapter` + `extractReflectionTheme` | — | done |
| **Skill library**（自动写） | ✅ F4.1–F4.2 反思自动产生 candidate `parsedSkillEntry`（`status=proposed`、`enabled=false`），`skillEntryIsActive` 双闸门防止未审批技能进入 prompt resolver；F4.3 REST `GET /api/agents/{id}/skills` + `POST .../approve` + `DELETE` 提供管理面 | 长期可考虑 P&L 加权自动晋升 | done |
| Agent learning UI | ✅ F3.4 `ReflectionsPanel` + F4.4 `AgentSkillsPanel`（按 PROPOSED/APPROVED 分组 + Approve/Reject 按钮）已接入 `AgentLearning` 页面 | — | done |
| A/B clone fund | ✅ | — | done |
| **A/B memory/reflection scope 隔离** | ✅ F3.3 `runReflectionCycle` 单元测试锁定（`TestRunReflectionCyclePersistsToCorrectFundOnly` + 仓库层 `fund_id` SQL 过滤） | — | done |
| **A/B promote lessons** | ✅ F6.1–F6.4 migration 025 把 `ab_test_learning_promotions` 扩 `promoted_memory_ids/promoted_skill_keys/previous_skill_config`；`applyABLearningPromotion` 升级为三联写：(1) `agents.evolution_config` 注入 recentLessons (沿用旧链路) (2) `MemoryRepo.CreateWithTx` 把 shadow lessons 克隆成 `layer=long_term` 反思入控制基金 `memories` 表 (标签 `ab_promotion`/`ab:<test>`/`variant:<key>`) (3) `mergePromotedSkillsIntoConfig` 把 shadow adjustments 转 `status=proposed`、`enabled=false`、`source=ab_promotion:<test>:<variant>` 的候选技能追加进控制 agent 的 `skill_config`（mode=overwrite 时丢弃前次的 `ab_promotion:*` 旧条目避免堆积，重复 key 仍记入 inserted 列表保证回滚幂等）；`RollbackLearningPromotion` 同事务回滚三件套（restore `evolution_config` snapshot + restore `skill_config` snapshot + `MemoryRepo.DeleteByIDsWithTx` 反思精确清除），并把 `promoted_memory_ids/skill_keys` 清零以便重复回滚安全；前端 `ABTestCompare.tsx` 加 `promotedReflectionIds`/`promotedSkillKeys`/`rolledBackReflectionIds`/`skillKeysReverted` 字段并在结果卡片渲染晋升 / 回滚汇总 | — | done |
| Marketplace buyout | ✅ | — | done |
| Marketplace subscribe | ✅ | — | done |
| **Marketplace auction** | ✅ F5.1–F5.5 `mode='auction'` 扩展 listing + `auction_*` 列；REST `POST /api/marketplace/auctions[+/{id}/bids,/settle]`；`marketplaceAuctionAdapter` + `auctionSettlementLoop` 完整闭环（开拍 / 出价 / anti-sniping 延长 ends_at / 后台自动结算 / 未达保留价退款）；前端 `/auctions` 页 | A/B 拍卖 + 第二价封闭式可后续叠加 | done |
| Agent lineage | ✅ | — | done |
| Wallet 冻结 | ✅ F5.2 `WalletRepo.HoldFundsWithTx/ReleaseHoldWithTx/CaptureHoldWithTx`（带 idempotency_key + 双写 ledger + 与 `TransferWithTx` 同一锁序）；migration 024 新增 `wallet_holds` 表；并发竞态映射为 `ErrIdempotencyConflict`，被出价/结算 / cron 全路径复用 | — | done |

---

## 11. 词汇表

- **Pod / Team**：1 PM + N Researcher + 1 Trader + 1 Risk 的最小作战单元
- **Plan**：PM 当日产出的、待审批的多 Action 集合
- **Action**：单个 buy/sell/hold 决策
- **Reflexion**：把一批 raw memory 蒸馏为长期 lesson 的过程
- **Skill**：可被 prompt 召回的、累积式的执行知识（Voyager 启发）
- **Shadow run**：A/B treatment fund 的执行模式，trade 不下真实单
- **Anti-snipe**：拍卖末段出价自动延长 end_at，防止狙击
- **Hard risk**：确定性、不依赖 LLM 的拒单引擎（如 StaleQuoteGuard）

---

## 12. 参考文献

1. **Hedge fund team structure**: Resonanz Capital, "2025 Hedge Fund Talent Tape" (2026); "Multi-Billion Dollar Hedge Fund Teams: Structure and Skillsets", Phoenix Learning.
2. **Voyager**: Wang et al., "Voyager: An Open-Ended Embodied Agent with Large Language Models", arXiv:2305.16291 (2023).
3. **Reflexion**: Shinn et al., "Reflexion: Language Agents with Verbal Reinforcement Learning", NeurIPS 2023.
4. **Recursive Self-Improvement for Trading**: Saulius, "Recursive Self-Improvement for Trading: How LLMs Can Teach Themselves to Invest" (2025).
5. **Auction theory**: Milgrom & Weber, "A Theory of Auctions and Competitive Bidding" (1982); ApolloDAO et al., "A Framework for Single-Item NFT Auction Mechanism Design", arXiv:2209.11293 (2022).
6. **Market data benchmark**: APIScout, "Best Stock Market and Financial Data APIs in 2026"; TickDB, "Polygon/Tushare/Akshare/Yahoo 六家主流行情数据源底层逻辑拆解" (2026).
