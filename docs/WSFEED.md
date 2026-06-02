# S6.5 — WebSocket 实时行情

## 1. 一句话定位

把 broker 撮合 + 持仓刷新这两条热路径上的报价来源，从 **REST 拉** 换成 **WebSocket 推**。
WS 提供持续推送 → 报价新鲜度从「秒级 poll」压到「亚秒级 push」，同时削掉绝大部分 REST 调用配额。

## 2. 为什么不再继续 REST

| 维度 | REST poll | WebSocket push |
| --- | --- | --- |
| 延迟 | 30 s ~ 5 s (取决于 cadence) | 50–200 ms |
| 配额 | 每 symbol 每 cadence 一次调用 | 一条长连 + 订阅集 |
| 突发 | 财报、停复牌等价跳会被 poll 间隙吞掉 | 上游一推就到 |
| 上限 | 几十只活跃 watchlist 就接近 provider 上限 | 单连数千 symbol 不成问题 |

REST 仍然保留 — WS 不是替代，是「优先 + fallback」。

## 3. 总体架构

```
┌────────────────────┐
│  上游 WS provider  │  Polygon / Alpaca / IEX / mock …
└─────────┬──────────┘
          │ Tick / state events
          ▼
┌─────────────────────────────────────┐
│  internal/wsfeed.Manager            │
│  ├─ provider 注册表（多 provider）  │
│  ├─ 订阅 ref-count + 多消费者多路   │
│  ├─ 事件 fan-out (cache / pos / …)  │
│  └─ 重连 / 重订阅监督               │
└─────────┬───────────────────────────┘
          │ Tick
          ▼
┌─────────────────────────────────────┐
│  internal/quotecache.Cache          │
│  ├─ last-tick per symbol            │
│  ├─ TTL stale 标记                  │
│  └─ LRU 上限                        │
└─────────┬───────────────────────────┘
          │ sync lookup (热路径)
          ├─────────────────────────────┐
          ▼                             ▼
   broker.Simulator              positionQuoteRefresher
   (撮合 / fill 价)              (持仓 holding_positions
   miss → REST 兜底              .current_price 更新；
                                 miss / stale → REST 兜底)
```

订阅与持仓的关系由 `wsFeedSubscriptionBridge` 维护：
每 30 s 把 `holding_positions` 里 qty ≠ 0 的 (instrument, symbol, market) 集合
diff 当前 bridge 自己持有的订阅集，差异部分调用 Manager 的 Subscribe / Unsubscribe。

## 4. 组件清单

### internal/wsfeed/

- **types.go** — `Tick`、`TickType`、`Subscription`、`Provider` 接口、`ConnState`、`ConnStats`、`SubStats`、`Event` 联合类型、`ErrXxx` 错误。
- **manager.go** — `Manager`：注册 provider、ref-count 订阅、fan-out、断开 / 重连 / 重订阅、`OnError` 回调、`DroppedEvents()` / `TotalTicks()` 观测。
- **provider/provider.go** — 内建两个 provider：
  - `NopProvider`：默认 — 假装连上但什么都不推。所有 broker quote 落到 REST fallback。运维通过 `/api/admin/wsfeed/status` 也能立刻看到「WS 没接通」。
  - `MockProvider`：测试 / 仿真用，可以 `EmitTrade / EmitQuote / Disconnect / Reconnect`，单测里给 manager + cache 灌可控数据流。

真实 provider（Polygon、Alpaca…）作为后续 PR 接入：实现 `wsfeed.Provider` + 在 `wiring_wsfeed.go` 的 switch 里加一行即可。

### internal/quotecache/

- **cache.go** — symbol → 最近一条合并后的 Tick（trade/quote/snapshot 各自有自己的合并规则）；TTL stale；LRU 上限；`Lookup` 返回 `(snap, ok, stale)`；`Apply` 是写入端；`Stats()` 给 admin。

### cmd/server/

- **wiring_wsfeed.go** — env-driven 配置；构造 manager / cache / bridge；wrap `broker.QuoteFn` 成 cache-aware 版本；`quoteCacheLookupAdapter` 把 cache 暴露给 positionRefresher。
- **admin_wsfeed.go** — admin REST：`/api/admin/wsfeed/{status,connections,subscriptions,cache,cache/{symbol},subscribe,unsubscribe,cache/evict,reconcile}`。
- **position_quote_refresher.go** — 新增 `SetWSCache(...)`：每次 refresh 在 REST 返回结果之上「叠加」WS cache 命中（更新的盖掉更旧的），减少 REST 调用 + 让持仓价与撮合所读保持一致。
- **main.go** — 起 manager / bridge；用 wrapped quoteFn 替换 broker simulator 的 quoteFn；服务停机时按顺序 stop bridge → stop manager。

### shared / web

- **shared/api-client/src/index.ts** — `WSFeedState / WSFeedStatus / WSFeedConnection / WSFeedSubscription / WSFeedCacheSnapshot / WSFeedCacheListResponse`。
- **shared/api-client/src/i18n.ts** — `wsfeed` 命名空间下 en / zh-CN。
- **web/src/lib/api.ts** — 8 个 API client 函数。
- **web/src/components/AdminWSFeedSection.tsx** — 监控面板：状态卡 / 连接表 / 订阅表 / 缓存表 / 手动订阅 / 强制 reconcile / 清缓存。
- **web/src/pages/Admin.tsx** — 接入。

## 5. 关键决策

### 5.1 默认禁用，opt-in 开启

`WSFEED_ENABLED=false`（默认）→ 完全不动现有路径，broker / refresher 行为字节级与 pre-S6.5 一致。
开启后默认 provider 是 `nop`，不会触碰任何真实上游 — 真实 provider 由后续 PR 单独引入。

环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `WSFEED_ENABLED` | `false` | 总开关 |
| `WSFEED_PROVIDERS` | `nop` | 逗号分隔的 provider 名 |
| `WSFEED_CACHE_STALE_AFTER` | `10s` | cache 多久后视为 stale |
| `WSFEED_CACHE_MAX_ENTRIES` | `5000` | LRU 上限 |
| `WSFEED_RECONCILE_INTERVAL` | `30s` | bridge 对齐周期 |
| `WSFEED_RECONCILE_INITIAL_DELAY` | `15s` | 启动后多久跑第一轮 |

### 5.2 Cache-first，错失不抛 → REST 兜底

```
cache hit & fresh  →  return cached
cache hit & stale  →  call REST，成功用 REST，失败 fallback 到 stale cache
cache miss         →  call REST
```

实盘场景下 REST 也挂掉 + cache 也 stale 时，宁愿成交在 stale price 也不要全单拒掉。
若要严格「stale 一定拒单」是单独的 broker-level flag — 不在 S6.5 范围。

### 5.3 订阅 ref-count + 同 provider 广播

manager 用 ref-count 处理多个 in-process 消费者订阅同一 symbol 的情形；第一份引用进来就调上游 Subscribe，最后一份退订才调上游 Unsubscribe。
v1 是「每个 provider 都听每条订阅」（广播），后续可以扩展成按 market 路由（HK 走某个 provider，US 走另一个）。

### 5.4 Reconnect 后自动重订阅

provider 发 state event 切到 Connected 时（无论是首次还是重连），manager 把当前 ref-count 表里所有 symbol 重新 Subscribe 一遍。
这是 manager 的契约 — provider 实现不用自己记忆订阅集。

### 5.5 Fan-out 是同步的

Handler 由 dispatcher 单 goroutine **同步** 调用 — 不为每 tick 起新 goroutine。
所以 handler 契约就是「O(1) 写 map 或非阻塞 channel send」。
panic 被 recover 并计入 `manager_error` 计数 + `OnError` 回调，绝不让一个坏 handler 拖死整个分发循环。

### 5.6 持仓刷新走「WS 叠加 REST」

不让 position refresher 直接订阅 ticks 去写 DB — 那会出现 1000 tick/s 跑 1000 次 UPDATE 的灾难。
做法是 refresher 维持原有 30s/5min cadence，但每轮 REST 拿到结果后，对每个 symbol 再查一次 wsCache：cache 比 REST 新就盖掉。
结果：DB 写入 cadence 不变，但被写入的价是「同一时刻 broker 看到的那一个」。

## 6. 数据流

### 6.1 撮合热路径（broker.PlaceOrder → fill）

```
broker.Simulator.PlaceOrder
   ↓
matching.Engine.Match  (需要当前价)
   ↓
QuoteFn  (现在被 wrap 成 cache-aware)
   ↓
1. quotecache.Lookup(symbol)
   - hit & fresh  → return  (≈ µs)
   - hit & stale → 下一步
   - miss        → 下一步
2. marketdata.GetQuote  (singleflight + 10s REST cache)
   - ok          → return
   - 错 + stale → return stale snapshot
   - 错 + miss   → return ErrQuoteUnavailable
```

### 6.2 推送侧（provider → cache → handler）

```
upstream WS → Provider.Start emits Tick
              ↓
        Manager.events chan
              ↓
        dispatch goroutine
              ↓
   ├─ stats 累计 (ConnStats / SubStats)
   ├─ tickHandlers 同步调用：
   │     ├─ quotecache.Apply
   │     └─ （未来）SSE forward 到 web
   └─ stateHandlers (metrics + slog)
```

### 6.3 订阅自动对齐

```
wsFeedSubscriptionBridge.run
   ↓ 每 30s
heldSymbols query (holding_positions WHERE qty ≠ 0)
   ↓
diff vs bridge.subscribed
   ├─ 新增 → manager.Subscribe
   └─ 缺失 → manager.Unsubscribe
```

operator 也可 POST `/api/admin/wsfeed/reconcile` 触发立即对齐。

## 7. 失败模式

| 故障 | 表现 | 影响 | 回收 |
| --- | --- | --- | --- |
| provider Start 失败 | startErr，state 卡在 Unknown | 该 provider 不出 tick；其他 provider 不受影响；broker 走 REST fallback | 看日志 + 修配置；admin 可点 reconcile 重连 |
| 连接抖动 | state Reconnecting；provider 自己 retry | 短期 cache 仍有上一个 tick；broker miss / stale 时走 REST | 自动；ReconnectCount 累计 |
| inbound channel 满 | manager.droppedEvents 累加 | 部分 tick 丢失，cache 暂时落后 | 临时；调大 InboundBuffer 或排查 handler 慢的根因 |
| handler panic | recover，metrics 计数 | 该 tick 这一个 handler 没执行；其他 handler 正常 | 修 handler 实现 |
| bridge DB 查询失败 | reconcile_query_err；不动订阅集 | 新开仓晚 ≤ 1 个 interval 才会被订阅 | 自动；DB 恢复即可 |
| WS + REST 双挂 | cache stale + REST 错 | broker 收到 stale 价格的成交（带告警） | upstream 恢复任一即可 |

## 8. 测试

- `internal/wsfeed/manager_test.go` — ref-count、fan-out、重连重订阅、Stop 幂等、handler panic 恢复。
- `internal/quotecache/cache_test.go` — trade/quote/snapshot 合并规则、stale 计算、LRU、并发 apply+lookup。
- `cmd/server/wiring_wsfeed_test.go` — cache-aware QuoteFn 全部分支（hit / miss / stale / REST 错 + cache 救场）+ env 解析。
- `cmd/server/wiring_wsfeed_bridge_test.go` — bridge 新增、删除、DB 错误回收。
- `cmd/server/admin_wsfeed_test.go` — admin REST：未授权 / 拒绝 / 健康 / subscribe 落地到 provider / cache get / evict / unsubscribe。
- `cmd/server/position_quote_refresher_wsfeed_test.go` — WS overlay 替换更新的 REST quote、保留更新的 REST quote、stale 不替换、nil cache no-op。

## 9. Roll-out

1. **第一波（默认 nop）**：env 不动，全部走 nop provider，validate 整条 manager / cache / bridge / admin / 监控通道在线（应该 0 影响）。
2. **第二波（mock 在 dev）**：dev 环境 `WSFEED_PROVIDERS=mock`，配合 e2e 测试模拟 burst tick 流。
3. **第三波（真实 provider）**：单独 PR 接入 Polygon/Alpaca；先 1 个 fund 灰度，验证命中率指标，再放量。

## 10. 未做、留给后续 PR

- 真实 provider 实现（Polygon、Alpaca、IEX）。
- SSE forward：把 server 侧 WS 流转发给 web/mobile 客户端，UI 实时刷价。
- Tick 持久化：`quote_ticks` 表 + 回放工具（盘后做 backtest / 重现成交）。
- 「stale 强制拒单」开关：broker 侧添加 flag，让运营在不允许 stale 成交的市场（如 HK / CN）能强制走 REST。
- 按 market 路由订阅：HK 走 broker A，US 走 broker B，CN 走 broker C，省 provider 费用。
