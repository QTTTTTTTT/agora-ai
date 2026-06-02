# 大单冲击模型 / Market Impact (S6.2)

> Status: shipped 2026-06-01.
>
> Replaces the simulator's flat-bps slippage with a per-instrument
> square-root impact model. Calibration rows live in
> `instrument_liquidity`, are cached in memory, and are consumed
> via a `matching.SlippageModel` adapter plugged into the broker
> simulator.

## 为什么需要 (motivation)

到 S6.1 为止，模拟器的滑点只有两档：`ZeroSlippage`（0 bps）或者 `SpreadCrossSlippage`（半个买卖价差）。这意味着：

1. **大单 P&L 过度乐观**：把 100 万股市价单丢给一支日均成交量 50 万股的票，按 last 成交，结果回测里这一笔挣了 1%；真实账户里则可能跌掉 100 bps 才填完。
2. **校准粒度太粗**：固定 5 bps 的全局参数对 TSLA 偏小、对 KO 偏大；调一个值会同时把所有标的的滑点都调走。
3. **没有可观测性**：撮合后看不出"为什么这笔 fill 比 last 偏了多少"，团队无法回答 "我们模拟的滑点和真实成交差多少"。

S6.2 把模型升级为业界标准的 **平方根冲击律**（Almgren-Chriss / 经验微观结构文献）：

```
adverse_bps = σ_daily · k · (Q / ADV)^α · 10000
adverse_bps ∈ [min_bps, max_bps]
```

- `σ_daily`：标的日波动率
- `Q`：订单数量
- `ADV`：过去 N 天的平均日成交量
- `α`：默认 0.5（square-root），可调
- `k`：标的特异系数

并通过 bps 上下限保证：

- 极小单（Q ≪ ADV）也至少付 `min_bps`（覆盖买卖价差的一半）
- 极大单（Q ≫ ADV）也不会算出"99% 滑点"这种荒谬数字

## 架构 (architecture)

```
┌─────────────────┐
│ admin UI / API  │ ← 校准 ADV / σ / coef
└──────┬──────────┘
       │ PUT /api/admin/marketimpact/instruments/{key}
       ▼
┌─────────────────┐     ┌──────────────────┐
│ marketimpact    │     │ instrument_      │
│   .Repo         │ ◄───│ liquidity table  │
└──────┬──────────┘     └──────────────────┘
       │
       │ ListAll() 启动 + 5min 周期刷新
       ▼
┌─────────────────┐
│ marketimpact    │ ← admin 写入后 ApplyChange()
│   .Cache        │     立刻可见
└──────┬──────────┘
       │
       │ Lookup(instrument_key)
       ▼
┌─────────────────┐
│ marketimpact    │
│   .Engine       │ ← 纯函数, square-root 公式
└──────┬──────────┘
       │
       │ Estimate(probe, calib) → AdverseBps
       ▼
┌─────────────────┐     ┌──────────────────┐
│ marketimpact    │ ──► │ matching         │
│   .Slippage     │     │   .Marketable    │
│    Adapter      │     │    Engine        │
└─────────────────┘     └──────┬───────────┘
                               │
                               │ Match(order, quote)
                               ▼
                        ┌──────────────┐
                        │ broker       │
                        │   .Simulator │
                        └──────────────┘
```

### 包职责

| 包 | 职责 |
|---|---|
| `internal/marketimpact/types.go` | `Liquidity`, `OrderProbe`, `Estimate`, `Engine`, asset-class defaults。**纯函数**, 不依赖 DB。 |
| `internal/marketimpact/repo.go` | `instrument_liquidity` 的 CRUD。`UpsertParams` pointer 字段语义 = "保留旧值"。 |
| `internal/marketimpact/cache.go` | 内存快照 + 周期刷新 + `ApplyChange()` 写后失效。撮合热路径只走这里。 |
| `internal/marketimpact/slippage.go` | 实现 `matching.SlippageModel`：`SpreadCrossSlippage` 决定 base price，引擎给出 bps，`ApplyAdverse` 加权得到最终 fill。 |
| `cmd/server/wiring_broker.go` | 启动时构造 cache + adapter，并通过 `broker.WithMatchEngine` 注入到模拟器。 |
| `cmd/server/admin_marketimpact.go` | admin REST：CRUD + Preview + Cache stats / refresh。 |

## 数据模型 (schema)

`server/migrations/064_market_impact.sql`：

```sql
CREATE TABLE instrument_liquidity (
    instrument_key      VARCHAR(64) PRIMARY KEY,
    symbol              VARCHAR(64) NOT NULL,
    market              VARCHAR(16) NOT NULL,
    asset_class         VARCHAR(24) NOT NULL DEFAULT 'equity',
    adv_shares          NUMERIC(20, 4),       -- NULL = 未知，引擎走 fallback
    adv_notional        NUMERIC(20, 2),
    adv_window_days     INT NOT NULL DEFAULT 20 CHECK (adv_window_days BETWEEN 1 AND 252),
    daily_volatility    NUMERIC(8, 6),         -- σ, e.g. 0.02 = 2%/day
    impact_coefficient  NUMERIC(8, 4) NOT NULL DEFAULT 1.0
                          CHECK (impact_coefficient > 0 AND impact_coefficient <= 10),
    impact_exponent     NUMERIC(4, 3) NOT NULL DEFAULT 0.5
                          CHECK (impact_exponent > 0 AND impact_exponent <= 1),
    min_slippage_bps    NUMERIC(8, 2) NOT NULL DEFAULT 1
                          CHECK (min_slippage_bps >= 0 AND min_slippage_bps <= 1000),
    max_slippage_bps    NUMERIC(8, 2) NOT NULL DEFAULT 500
                          CHECK (max_slippage_bps >= 0 AND max_slippage_bps <= 5000),
    last_calibrated_at  TIMESTAMPTZ,
    calibration_source  VARCHAR(24) NOT NULL DEFAULT 'manual'
                          CHECK (calibration_source IN ('manual','historical','broker_reported')),
    note                TEXT,
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_by          UUID,
    CHECK (min_slippage_bps <= max_slippage_bps)
);
```

> **为什么 ADV 用 pointer 而不是 NOT NULL**：操作员经常先把"我知道这只票的 σ"录进去再慢慢算 ADV。强制 NOT NULL 会让部分校准的行被拒，于是引擎设计成 ADV 缺时回退到 `min_bps` 这个软地板。

## 引擎语义 (engine semantics)

Code: `server/internal/marketimpact/types.go::Engine.Estimate`

```
input  : OrderProbe + (optional) *Liquidity
output : Estimate { AdverseBps, UsedDefaults, UsedADVFallback, Reason, ... }

行为分层：
  1. probe.Quantity <= 0 OR probe.ReferencePx <= 0 →
     AdverseBps = 0, Reason = "invalid probe"
  2. calib == nil (没有校准行) →
     使用 AssetClassDefault(probe.AssetClass)
     UsedDefaults=true, UsedADVFallback=true
     AdverseBps = defaults.MinSlippageBps
  3. calib != nil 但 ADV 缺 →
     UsedADVFallback=true
     AdverseBps = calib.MinSlippageBps
  4. calib + ADV 都齐 →
     ratio = Quantity / ADVShares
     bps   = sigma * coef * ratio^alpha * 10000
     bps   = clamp(bps, MinSlippageBps, MaxSlippageBps)
     AdverseBps = round(bps, 2 decimals)
```

字段缺失时，引擎按字段级别回退（"操作员只填了 ADV，coef 留空" → coef 走资产类默认）；这让部分校准行立刻可用。

## 资产类别默认 (asset-class defaults)

来自 `AssetClassDefault`：

| asset_class | σ | coef | α | min_bps | max_bps |
|---|---|---|---|---|---|
| equity (default) | 0.02 | 1.0 | 0.5 | 1 | 200 |
| etf | 0.015 | 0.8 | 0.5 | 1 | 150 |
| futures | 0.012 | 0.8 | 0.5 | 0.5 | 100 |
| crypto | 0.04 | 1.5 | 0.5 | 2 | 500 |
| option | 0.04 | 1.5 | 0.5 | 2 | 500 |
| bond | 0.005 | 0.5 | 0.5 | 1 | 100 |
| otc | 0.005 | 0.5 | 0.5 | 1 | 100 |

数值经过简单标定：1% ADV、equity 默认 → 20 bps；10% ADV → 63 bps，封顶 200 bps。

## 接入 simulator (integration)

`internal/broker/simulator.go::WithMatchEngine` 已存在；S6.2 不改 simulator 的接口，只在启动时换掉撮合引擎：

```go
// cmd/server/main.go (excerpt)
marketImpactRepo := marketimpact.NewRepo(db)
marketImpactCache, marketImpactAdapter := newMarketImpactStack(
    context.Background(), marketImpactRepo, metrics)
marketImpactEngine := newMatchingEngineWithImpact(marketImpactAdapter)

services.BrokerSimulator = newBrokerSimulator(
    marketDataService,
    broker.WithMarketStatusGate(marketStatusGate),
    broker.WithMatchEngine(marketImpactEngine),    // ← 新接入点
)
```

`SlippageAdapter` 同时实现 `matching.SlippageModel`（撮合时被调用）和 `EstimateForProbe`（admin Preview 时被调用），二者共用同一个 cache + engine。

## Admin REST

| Method | Path | 用途 |
|---|---|---|
| `GET` | `/api/admin/marketimpact/instruments` | 列表，支持 `?market`、`?asset_class`、`?limit`、`?offset` |
| `GET` | `/api/admin/marketimpact/instruments/{key}` | 单条 |
| `PUT` | `/api/admin/marketimpact/instruments/{key}` | upsert；写后立刻 `ApplyChange` 进缓存 |
| `DELETE` | `/api/admin/marketimpact/instruments/{key}` | 删除并从缓存淘汰；返回 404 表示行原本就不存在 |
| `POST` | `/api/admin/marketimpact/preview` | 不下单，仅运行 engine |
| `GET` | `/api/admin/marketimpact/cache` | 返回 `{size, last_refresh}` |
| `POST` | `/api/admin/marketimpact/cache/refresh` | 强制重新拉表 |

每个写入端点都会：

1. 校验请求体 + 校验 `instrument_key/symbol/market` 必填
2. 写入 `instrument_liquidity`
3. `marketImpactCache.ApplyChange(...)` —— 撮合立刻可见
4. `auditLogger.LogMutation(...)`
5. `metrics.RecordMarketImpactEvent("admin_*")`

## Web UI

`web/src/components/AdminMarketImpactSection.tsx`：

- **校准列表**：表格按 market/asset_class 过滤；inline upsert / delete。
- **预演面板**：录入 `instrument_key + side + qty + ref_px`，调用 `/preview` 返回 bps + 隐含成交价 + 冲击成本。前端显式渲染 `used_defaults` / `used_adv_fallback` 标记，操作员一眼能看出这次预演用的是默认还是校准。
- **缓存面板**：显示 cache size + last_refresh，"强制刷新" 按钮触发 `/cache/refresh`。

i18n 在 `shared/api-client/src/i18n.ts::marketImpact` 命名空间下，zh-CN 和 en-US 都齐。

## Metrics

`fundai_marketimpact_events_total{event="..."}` —— 详见 `docs/PROMETHEUS_QUERIES.md` 第 17 节。关键 event：

- `estimate` —— 每次 FillPrice 调用 +1（≈ 撮合速率）
- `used_defaults` / `used_adv_fallback` —— 覆盖率反向指标
- `bucket_<asset>_<bps_bucket>` —— bps 分布直方图
- `admin_*` —— 操作员 UI 行为
- `cache_refresh_ok` / `cache_refresh_err` —— 后台 5min 循环健康

## Fail-open 行为

撮合热路径不会因为 marketimpact 故障而拒单：

- cache 启动失败 → cache 持有空 map → 引擎走 asset-class defaults，`AdverseBps = min_bps`
- 周期刷新失败 → cache 保持上一次成功的快照，OnError 回调 +`cache_refresh_err`
- adapter.Engine == nil → adapter 直接返回 base price（spread cross 价格）
- 引擎收到 `Quantity ≤ 0` 或 `ReferencePx ≤ 0` → AdverseBps = 0

也就是说，最坏情况下 S6.2 退化回 S6.1 之前的 `SpreadCrossSlippage` 行为。

## 测试覆盖

- `internal/marketimpact/types_test.go`（10 个）—— 引擎纯函数测：invalid probe、no calib、square-root scaling、min/max clamp、ADV missing、partial calibration。
- `internal/marketimpact/repo_test.go`（9 个）—— GetByKey / List / Upsert / Delete 路径，含校验报错。
- `internal/marketimpact/cache_test.go`（10 个）—— Lookup / ApplyChange / SetRows / Start-Stop 幂等 / 周期 ticker。
- `internal/marketimpact/slippage_test.go`（6 个）—— matching.SlippageModel 适配，覆盖 buy/sell/无校准/nil engine。
- `cmd/server/admin_marketimpact_test.go`（11 个）—— admin REST 鉴权、CRUD、Preview、Cache stats，含 cache invalidate 校验。

## 后续工作 (future)

S6.2 之后可以做的增量：

1. **Permanent impact**：当前模型只把全部 bps 当作"临时冲击"打到 fill price。未来可分裂出永久冲击份额，并把它作为后续订单的 mid-price shift（影响连续大单的成交价）。
2. **历史回算 calibrator**：批处理 job 拉过去 N 天的 quote 数据，自动算 ADV / σ 并以 `calibration_source='historical'` 写入。
3. **券商上报**：与真实券商接入后，把对方分析报告里的 impact bps 直接以 `calibration_source='broker_reported'` 录入做对照。
4. **WS 实时 ADV**：S6.5 落地后，可基于实时成交流维护"过去 1 小时 ADV"，让日内大单冲击随流动性变化而变化。
5. **Pair / basket trading**：批量单的冲击不是各腿之和；可以做 portfolio-level impact 模型。

—— end ——
