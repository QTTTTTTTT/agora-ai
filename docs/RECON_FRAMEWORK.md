# P1-3 — 日终 Reconciliation Framework

> Status: 实现完成（mock provider 阶段）
>
> Owner: 平台基础架构 / 风险与运维
>
> Sprint: S5 P1
>
> 相关：P0-9（broker_links）/ P1-1（cash_ledger）/ P1-2（funding requests）/ P1-4（FX provider）

---

## 1. 我们想解决什么

平台从模拟向"实盘"过渡的关键一环：当真实 broker 进场后，**内部账（持仓 / 现金 / 成交）** 与 **broker 账（statement）** 必然会出现 drift，原因可能是：

- partial fill 我们记到 internal 了，broker 回报丢了；
- broker 把同一笔 trade 推了两遍；
- 利息、行情费、stamp duty 直接从 broker 现金扣，但平台 cash_ledger 还没拿到；
- corp action 处理时差。

P0-9 把"实盘"的 hard gate 立起来了，但没有 **持续验证** 内外账一致性的机制；这一项（P1-3）就是补上 daily reconciliation 框架。

设计目标：

1. **存得下** — broker 一份每天的对账单（statement）能按 (fund, date, source) 唯一去重落库。
2. **跑得动** — 一个 pure 的 diff engine，输入 internal snapshot + broker statement，输出 break 列表；带宽容度（tolerances），可在不同场景调严或调松。
3. **查得到** — 全套 break / run 持久化，admin 在 UI 上可分级、可筛选、可下钻；任何手动 acknowledge / resolve / ignore 落到 audit chain。
4. **能告警** — Prometheus 指标曝出 `run_ok` / `run_failed` / 各类 `break_*` / `resolve_*`，可挂 SLO 告警。
5. **自上线起就跑** — 即便还没有真实 broker，先用一个 **mock provider** 周期性产出"perfect mirror"对账单，让 dashboard 和 audit chain 在真 broker 接入之前就有数据。

---

## 2. 数据模型

迁移：`server/migrations/060_reconciliation.sql`。

### `broker_statements`

每天一行，记录"我们摄入了哪份 statement"。

```
id, fund_id, broker_link_id, statement_date, source, payload_hash, raw_payload, ingested_at, ingested_by, status, created_at
UNIQUE (fund_id, statement_date, source, payload_hash)
```

`payload_hash` 是对 (positions, cash, trades) 规范化后做 SHA-256，让"broker 把昨天的 statement 又发一次"在 `IngestStatement` 里直接命中 `ErrAlreadyIngested`，对调用方表现成 no-op。

`source` 是封闭词典：`mock | csv_upload | api | fix`。

### `broker_statement_positions / _cash / _trades`

statement 的三个子表。每条记录一个 line item：

- `_positions`：(symbol, quantity, avg_cost, market_value, currency)
- `_cash`     ：(currency, balance)
- `_trades`   ：(broker_trade_id, broker_order_id, symbol, side, quantity, price, fee, currency, executed_at)

子表都带 `metadata JSONB`，给 broker 特定字段留兜底通道（比如 stamp tax 占比）。

### `reconciliation_runs`

一次 diff 的元数据 + 计数。

```
id, fund_id, statement_id, run_date,
trigger_source ∈ {manual | scheduled | replay},
status ∈ {pending | completed | failed},
break_count_total / _critical / _warning / _info,
summary JSONB, started_at, completed_at, error_message
```

`status = completed` 表示 diff 跑完了，**有 break 不代表 run failed** —— break 是工件，不是失败。

### `reconciliation_breaks`

单条差异。break_type 用 closed vocabulary（见下文 §3）；status 走 `open → acknowledged → resolved | ignored | open`（可重开）。

```
id, run_id, fund_id, break_type, severity ∈ {info|warning|critical},
symbol, currency,
internal_value, broker_value, diff_value, diff_percent,
description, metadata JSONB,
status, resolution_note, resolved_by, resolved_at, created_at
```

索引：

- `(fund_id, status, created_at DESC)` —— 通用列表
- `(fund_id, severity, created_at DESC) WHERE status = 'open'` —— 部分索引专门给 dashboard 上 "open & critical" 视图

---

## 3. Diff engine

代码：`server/internal/recon/engine.go`，**pure**：输入 `(*Statement, *InternalSnapshot)`，输出 `[]Break`。无 I/O、无 DB、无时钟，golden test 友好。

### 三阶段

1. **Positions** — 按 canonicalSymbol 匹配。比较 quantity（绝对带）+ avg_cost（绝对 ∨ 相对带）。
2. **Cash** — 按 canonicalCurrency 匹配。比较 balance（绝对 ∨ 相对带）。
3. **Trades** — 主键 broker_trade_id，回退 broker_order_id（部分 broker EOD 对账单只给 order id 不给 trade id）。匹配后比较 side（任何不一致都是 critical，不走 tolerance）/ quantity / price。

### Break 词典（12 类）

| 类别        | break_type                          | 默认 severity                                  | 触发条件                                         |
| ----------- | ----------------------------------- | --------------------------------------------- | ------------------------------------------------ |
| Position    | `position_quantity_mismatch`         | critical（>1% 偏移）/ warning                 | qty 差异超 tolerance                              |
| Position    | `position_avg_cost_mismatch`         | warning                                       | avg_cost 差异超 tolerance                         |
| Position    | `position_missing_internal`          | critical                                       | broker 有，我们没有                              |
| Position    | `position_missing_broker`            | critical（qty>0）/ info（qty≈0）              | 我们有，broker 没有                              |
| Cash        | `cash_balance_mismatch`              | critical（>0.5%）/ warning                    | balance 差异超 tolerance                          |
| Cash        | `cash_currency_missing_internal`     | critical                                       | broker 有该币种，我们没有                        |
| Cash        | `cash_currency_missing_broker`       | critical（balance>1¢）/ info                   | 我们有该币种，broker 没有                        |
| Trade       | `trade_missing_internal`             | critical                                       | broker 有 trade，我们没有                        |
| Trade       | `trade_missing_broker`               | critical                                       | 我们有 trade，broker 没有                        |
| Trade       | `trade_quantity_mismatch`            | critical（>1%）/ warning                      | 配对成功后 qty 差异                              |
| Trade       | `trade_price_mismatch`               | warning                                       | 配对成功后 price 差异                            |
| Trade       | `trade_side_mismatch`                | critical                                       | 配对成功后 side 不同（数据腐化）                |

### Tolerance 默认值

```
QuantityAbs       = 1e-6
AvgCostAbs        = 1e-4
AvgCostPct        = 5e-5  // 0.005% × broker price
CashAbs           = 0.01  // 1¢
CashPct           = 1e-5  // 0.001% × broker balance
TradePriceAbs     = 1e-4
TradePricePct     = 5e-5
TradeQuantityAbs  = 1e-6
```

收紧场景（月末审计 / 监管报告）可以传入自定义 `Tolerances`；这是 `NewEngine` 的入参之一。

### 排序

输出按 `(severity DESC, break_type, symbol)` 排序。UI 直接按这个顺序铺，不用再排。

---

## 4. Repo & Mock provider

`server/internal/recon/repo.go`：

- `IngestStatement` 事务式写 broker_statements + 三张子表；hash 命中时返回 `ErrAlreadyIngested + existing row`。
- `CreateRun` 事务式写 run + breaks。
- `ListRuns` / `GetRun` / `ListBreaks` / `ResolveBreak`（`ResolveBreak` 允许重开 → status='open'）。

`server/internal/recon/mock_provider.go`：

- `MockProvider.Build(snap)` 把 internal snapshot 镜像成 statement。
- `MockProviderOptions.IncludeDrift = true` 时人为引入 (qty / cash / price) 偏移，方便 demo 演示和 UI 测试。

---

## 5. Snapshot adapter

`server/cmd/server/recon_snapshot.go`：

- 从 `holding_positions` 读 internal positions
- 从 `cash_ledger.SubtotalByCurrency` 读 internal cash（end-of-day asOf 截止）
- 从 `trade_executions WHERE status IN ('filled','partial') AND executed_at within day` 读 internal trades；`ExternalRef` 优先取 `broker_order_id`，fallback `client_idempotency_key`

**这个 adapter 是 cmd/server 而不是 recon 包的原因**：recon 包刻意只依赖自己定义的 `InternalSnapshot` 接口，不直连 repository，避免 internal/recon ↔ internal/repository 的循环依赖。

---

## 6. REST API

全部挂在 `/api/admin/reconciliation/*`，复用与 FX/funding 相同的 admin gate。

| Method | Path                                            | 用途                                              |
| ------ | ----------------------------------------------- | ------------------------------------------------- |
| GET    | `/api/admin/reconciliation/runs`                | 列出最近 N 次 run（可按 fund_id 过滤）            |
| GET    | `/api/admin/reconciliation/runs/{id}`           | 详情 + 该 run 的 breaks                            |
| POST   | `/api/admin/reconciliation/runs`                | 触发一次 on-demand run（当前仅支持 mock provider）|
| GET    | `/api/admin/reconciliation/breaks`              | 列出 break，按 fund_id / status / severity 过滤   |
| POST   | `/api/admin/reconciliation/breaks/{id}/resolve` | 翻转 break.status，note 上链                       |

每次手动 trigger / resolve 都写一条 `audit.MutationEvent`：

- `recon_run.trigger` —— after = {fund_id, as_of, provider, break_count, break_count_crit}
- `recon_break.resolve` —— after = {status, note, break_type, fund_id}

---

## 7. 调度 loop

`server/cmd/server/recon_loop.go`：

- 默认 24h 间隔，jitter 5%。
- `runOnce` 用 `FundLister` 拿基金列表（生产用 `fund_repo.ListActive`），逐基金跑 `runFund`。
- `runFund` 流程：snapshot → mock statement → ingest → diff → CreateRun。每段失败都更新对应 metric 但不阻塞下一个 fund。
- 单 fund 超时 30s（防一份 statement 跑很久把整 wave 卡住）。
- 起始一轮等满 `nextDelay()`，不立即跑（recon 比 FX 重，避免容器启动后立刻刷一波负载）。

未来 leader 选举（多副本部署）：复用 `scheduler.Coordinator`，跟 fx_loop 的方案保持一致。

---

## 8. 指标

`fundai_recon_events_total{event}`，事件分四组：

- `ingest_ok` / `ingest_duplicate` / `ingest_error`
- `run_ok` / `run_failed` / `scheduled_skip`
- `break_<break_type>` —— 12 类
- `resolve_acknowledged` / `resolve_resolved` / `resolve_ignored` / `resolve_open`

详细 PromQL + 告警规则：见 [PROMETHEUS_QUERIES.md §13](./PROMETHEUS_QUERIES.md#13-日终对账reconciliationp1-3)。

---

## 9. UI

`web/src/components/AdminReconSection.tsx`，挂载在 `/admin`：

- 表头：runDate · trigger · status · 严重 / 警告 / 提示 / 总差异 · 展开按钮
- 行展开 → 该 run 的 breaks 表（type · symbol · currency · internal · broker · diff · diff% · status · 操作）
- 每条 break 在 `status='open'` 时提供 [Acknowledge / Resolve / Ignore]，否则提供 [Re-open]。
- "Trigger run" 折叠面板：输入 fund_id + 可选 drift 参数，立即跑 mock-provider run 并把结果展开在表顶。
- 解决对话框 modal：备注必填，按钮在提交中禁用。

i18n（zh-CN / en-US）通过 `shared/api-client/src/i18n.ts` 的 `recon` 命名空间集中管理。

---

## 10. 后续 / 真 broker 接入路径

当前阶段唯一的 provider 是 mock。一旦第一个真 broker 适配器上线（CSV / FIX / REST），改动面：

1. 在 `internal/recon/` 下新增 `csv_provider.go` 等，参考 `mock_provider.go` 的接口。
2. 给 `admin_recon.handleTriggerReconRun` 增加 source 选择字段（当前 hardcoded 走 mock）。
3. 给 daily loop 在 `runFund` 里按 `broker_links.broker_kind` dispatch 到对应 provider。
4. CSV upload endpoint：`POST /api/admin/reconciliation/statements`（multipart）。

整个 framework 的设计前提是：**provider 的输出形状**（statement + 三张子表）是稳定的；不同 broker 只是不同的解析器。所以这一步不需要改 schema，也不需要动 diff engine。

---

## 11. 文件清单

### 后端

- `server/migrations/060_reconciliation.sql` / `.down.sql`
- `server/internal/recon/types.go`
- `server/internal/recon/engine.go` + `engine_test.go`
- `server/internal/recon/repo.go` + `repo_test.go`
- `server/internal/recon/mock_provider.go` + `mock_provider_test.go`
- `server/cmd/server/recon_snapshot.go`
- `server/cmd/server/admin_recon.go` + `admin_recon_test.go`
- `server/cmd/server/recon_loop.go` + `recon_loop_test.go`
- `server/cmd/server/main.go` —— 调度 wire + `serverMetrics.RecordReconEvent`
- `server/cmd/server/admin_handler.go` —— 注册 `registerReconAdminRoutes`

### 前端 / 共享

- `shared/api-client/src/index.ts` —— `ReconciliationRun` / `ReconciliationBreak` / `ReconciliationBreakType` 类型
- `shared/api-client/src/i18n.ts` —— `recon` 命名空间
- `web/src/lib/api.ts` —— `listAdminReconRuns` / `getAdminReconRun` / `listAdminReconBreaks` / `triggerAdminReconRun` / `resolveAdminReconBreak`
- `web/src/components/AdminReconSection.tsx`
- `web/src/pages/Admin.tsx` —— 挂载

### 文档

- `docs/PROMETHEUS_QUERIES.md` —— §13 reconciliation 段
- `docs/RECON_FRAMEWORK.md` —— 本文
