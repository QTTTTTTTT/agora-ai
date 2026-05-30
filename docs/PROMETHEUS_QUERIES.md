# Prometheus 查询速查 — PM 决策管线 (Sprint A → E)

> 创建日期: 2026-05-23 · 配套文档: `MONDAY_TRIAGE_PLAYBOOK.md`
> 受众: 系统运维 + 基金 owner · 目标: 不依赖 Grafana 即可用 `curl + promtool` 验收新信号块

应用通过 `GET /api/metrics` 暴露 Prometheus text format 指标 (`fundai_*`)。
本档把周一开盘后**最常问的 10 个问题**翻译成可直接粘贴到
`promtool query instant` / Prometheus Web UI / Grafana **Explore** 面板的 PromQL。

每条查询都包含三段：
- **场景** — 这条查询回答的具体问题
- **查询** — 可直接粘贴的 PromQL
- **解读** — 期望值 / 偏离时往哪儿查

---

## 0. 前置条件

- Prometheus scrape config 抓 `/api/metrics`（推荐 15s 间隔）。
- 如果没有 Prometheus，本档所有查询都可以走
  `curl -s http://localhost:8080/api/metrics | grep <metric>` + 手动算 ratio。
- Sprint D #1 引入的所有 decision 计数器以 `fundai_decision_` 开头，统一命名。
- 在快速验收阶段，把所有窗口收紧到 `[1h]` / `[10m]`；上线稳定后再放宽到 `[24h]`。

---

## 1. 决策管线是否在跑？(sanity)

### 1.1 PM 决策每分钟产出数

**场景**: 周一开盘后 30 分钟内必须看到 PM 决策被调度起来。0 → 调度未跑。

```promql
sum(rate(fundai_decision_input_calls_total[5m])) * 60
```

**解读**: 每分钟决策数。
- US 单基金日内调度: 大约每 5–15 分钟一次 → 期望值 0.07 ~ 0.2
- A-share 单基金: 大约每 10–30 分钟一次 → 期望值 0.03 ~ 0.1
- 0 持续 > 10 分钟 → 检查 leader lease (`fundai_scheduler_leader_state`)
  + market calendar gating + LLM 余额。

### 1.2 距离上一次决策多久了

```promql
time() - max(timestamp(fundai_decision_input_calls_total))
```

**解读**: 单位秒。
- 开盘期间应 < 1800s (30 分钟)。
- 收盘后 > 6h 是正常的。

---

## 2. 信号块覆盖率 (Sprint A → E 验证核心)

Sprint A/B/C/D/E 总共把决策输入块从 6 个扩到 21 个。
`fundai_decision_input_blocks_total{block,present}` 是验收的核心指标。

### 2.1 每个块的当前**出现率** (over last hour)

**场景**: 一眼看哪些信号块周一真在喂给 PM。
任何 < 0.5 的 critical 块（universe / instrument hints / quant snapshots）都是事故；
新块（qualityScores / earningsCalendar / pairSpreads / cooldowns / riskBudget）
出现率长期为 0 → 上游依赖未接通。

```promql
sum by (block) (increase(fundai_decision_input_blocks_total{present="true"}[1h]))
  /
clamp_min(sum by (block) (increase(fundai_decision_input_blocks_total[1h])), 1)
```

**解读**:
- 1.0 表示这个 hour 内每次决策都包含该块
- 0.0 表示该块完全没出现 → 检查上游依赖：
  - `qualityScores` 缺 → `fundamental.Fetcher` 未配 / Yahoo 401
  - `earningsCalendar` 缺 → `YAHOO_EARNINGS_DISABLED=1` 或 Yahoo throttle
  - `pairSpreads` 缺 → universe 内没有相关度 > 0.7 的对子（正常）
  - `cooldowns` 缺 → 周一没有持仓刚成交 (正常)
  - `riskBudget` 缺 → fund.config 未启用动态风险预算 (正常)

### 2.2 每个块的累计出现次数 (绝对值排序)

**场景**: 验证最近 24h 哪个块出现得最频繁、最少。

```promql
sort_desc(
  sum by (block) (increase(fundai_decision_input_blocks_total{present="true"}[24h]))
)
```

**解读**:
- 前几名应该是 always-on 的：`roundtableStance`, `bullCase`, `bearCase`,
  `quantCase`, `symbolVerdicts`, `instrumentHints`, `quantSnapshots`,
  `universeRanking`, `newsCatalysts`。
- 中段应该是 sometimes-on：`exposure`, `correlations`, `qualityScores`。
- 末尾应该是 rarely-on（仅当事件发生时）：`cooldowns`, `pairSpreads`,
  `earningsCalendar`, `riskBudget`, `lessonReplay`, `sleeveScorecard`。

### 2.3 完全 absent 的 block (最快暴露断链)

**场景**: 一行 PromQL 暴露所有 24h 内**从未出现过**的块。

```promql
sum by (block) (increase(fundai_decision_input_blocks_total{present="true"}[24h])) == 0
```

**解读**: 返回值如果非空，说明这些块上线后从未被注入到 prompt 里。
**优先级**:
- `instrumentHints` / `quantSnapshots` → 阻断决策，立刻 fix
- `qualityScores` / `earningsCalendar` / `pairSpreads` → Sprint E 新块，
  逐个查上游 fetcher
- 其他 → 可能配置如此（如 fund.config 未启用 `lessonReplay`），可接受

---

## 3. 风控信号触发频次

### 3.1 Exposure 触发分布（按 kind）

**场景**: 一个交易日里哪类组合限制最常被触发？

```promql
sum by (kind) (increase(fundai_decision_exposure_breaches_total[24h]))
```

**预期 kind**:
- `single_name` — 单票超过最大权重 (Sprint C #1)
- `sector` — 行业集中度超阈值
- `cash_floor` — 现金底线触发
- `gross_exposure` / `net_exposure` — 杠杆约束

**解读**:
- 持续 0 → 要么真的没触发（小持仓基金常态），要么 `exposure.Service`
  没传 fund.config 的 `ExposurePolicy`（看 fund.config 的 `riskPolicy`）。
- 某 kind 异常多 → 该限制配得过严，PM 多数操作都会被驳回；考虑调整。

### 3.2 Cooldown 命中最频繁的 symbol

**场景**: 哪些标的最常在冷却期内被 PM 想动？

```promql
topk(5,
  sum by (symbol) (increase(fundai_decision_cooldown_vetos_total[24h]))
)
```

**解读**: 同一 symbol 一天里命中冷却 > 3 次 → PM 反复想交易同一标的；
检查 fund.config 的 `cooldownBars`（Sprint B #1 默认 5 根 K 线）是否过短。

### 3.3 Risk budget 节流原因分布

```promql
sum by (reason) (increase(fundai_decision_risk_budget_throttled_total[24h]))
```

**预期 reason**:
- `drawdown_throttle` — 回撤超阈值，每笔风险减半
- `vol_target_zero` — 实现波动率为零（数据缺失？）
- `disabled` — fund.config 关掉了风险预算

**解读**: `drawdown_throttle` 持续 > 0 → fund 处于风险压缩态；
是设计行为，但同时检查 NAV / drawdown 是否合理。

### 3.4 Correlation 高相关对子数

```promql
increase(fundai_decision_correlation_high_pairs_total[1h])
```

**解读**: 每次决策注入的高相关对子总数。
- 大盘震荡日典型值 0–5
- 风险事件日（如美债崩盘）可能飙到 30+
- 总是 0 且 universe 有 > 5 标的 → 检查 OHLC 数据完整性

---

## 4. LLM 调用健康（PM 决策依赖）

### 4.1 LLM 成功率（最近 10 分钟）

```promql
sum(rate(fundai_llm_calls_total{status="success"}[10m]))
  /
clamp_min(sum(rate(fundai_llm_calls_total[10m])), 0.001)
```

**解读**: < 0.9 持续 5 分钟 → 跳 `MONDAY_TRIAGE_PLAYBOOK.md §5.1`。

### 4.2 各 provider 失败率

```promql
sum by (provider, status) (rate(fundai_llm_calls_total{status!="success"}[10m]))
```

**解读**: 单 provider 集中失败 → 该 provider 限流或 API key 失效；
auto-failover 应在 30 秒内切换 (Sprint A bonus fix 验证点)。

---

## 5. 工作流推进 (PM 决策对应的 plan workflow)

### 5.1 工作流终态分布 (24h)

```promql
sum by (state) (increase(fundai_workflow_transitions_total[24h]))
```

**期望** (US 交易日):
- `succeeded` >> `failed` + `cancelled`
- `failed` 占比 > 5% → 配 `FundAIWorkflowFailedTransitions` 告警（已默认开启）

### 5.2 硬风控拒单频次

```promql
sum(increase(fundai_hard_risk_rejections_total[1h]))
```

**解读**: > 5 / 小时 → execution-layer 风控太紧，PM 的 plan 被反复刷回；
检查 fund.config `hardRisk` 限制是否合理。

---

## 6. 综合验收 dashboard (单页 PromQL)

适合贴进 Grafana **Explore** 当作单次开盘验收：

```promql
# (1) 决策每分钟数
sum(rate(fundai_decision_input_calls_total[5m])) * 60

# (2) 21 块出现率 (横向 bar)
sum by (block) (increase(fundai_decision_input_blocks_total{present="true"}[1h]))
  /
clamp_min(sum by (block) (increase(fundai_decision_input_blocks_total[1h])), 1)

# (3) Exposure 触发分布
sum by (kind) (increase(fundai_decision_exposure_breaches_total[1h]))

# (4) Risk-budget throttle
sum by (reason) (increase(fundai_decision_risk_budget_throttled_total[1h]))

# (5) LLM 成功率
sum(rate(fundai_llm_calls_total{status="success"}[10m]))
  /
clamp_min(sum(rate(fundai_llm_calls_total[10m])), 0.001)

# (6) 工作流失败率
sum(rate(fundai_workflow_transitions_total{state=~"failed|cancelled"}[1h]))
  /
clamp_min(sum(rate(fundai_workflow_transitions_total[1h])), 0.001)
```

---

## 7. 不用 Prometheus 时的退路

如果还没拉起 Prometheus，下面是单次 curl 等效：

```bash
# 21 块出现次数（自上次进程启动起的累计值）
docker exec fundai-app curl -s http://localhost:8080/api/metrics \
  | grep 'fundai_decision_input_blocks_total' \
  | sort
```

或者直接用本仓库的 smoke 脚本，它一次拉一行 slog
fingerprint + 21 个 present 标记：

```bash
./scripts/smoke-decision.sh
./scripts/smoke-decision.sh --json | jq .
```

smoke 脚本和 PromQL 二者搭配：
- **smoke** 看**最近一次**决策的健康度 (单点)
- **PromQL** 看**最近一段时间**的趋势 (累计 / 速率)

---

## 8. corp-action 摄入健康度 (Card G)

`corpActionIngestLoop` 每 12h 在 leader replica 上扫描所有活跃基金的持仓 → 调用对应市场 provider 拉过去 90 天的 split / cash dividend → upsert + 应用到持仓 + 现金。Card G 把 loop 内部的 `slog` 观测点搬到 Prometheus，方便：

> Card H 后市场→provider 的路由表为：
>   - `a_share`   → `EastmoneyProvider`（A 股 RPT_SHAREBONUS_DET）
>   - `us_equity` → `YahooProvider`（chart events div|split）
>   - `hk_equity` → `HKEXProvider`（Eastmoney HK RPT_HKF10_DIVIDENDPLAN）
>
> Card H 之前 `hk_equity` 也走 Yahoo —— Yahoo 的 HK 源缺中期/特别股息和送股，会在每次派息后给 HK 持仓造成幻影 PnL。新 provider 复用 Eastmoney 同一上游主机，所以错误标签里 `market="hk_equity"` 的 fatal/transient 与 `a_share` 同源，告警时合并看即可。


1. 在 Grafana 上看「最近一次成功 ingest 是什么时候」。
2. 设报警「`now() - last_success > 7d`」 → 上游被 WAF 封了 / cron 卡住。
3. 看 split / dividend 数据流的体量是否正常（节假日/盘后应该几乎为 0，盘中开盘前后会有一波）。

### 8.1 距离上一次成功 ingest 多久

**场景**: 周一开盘前快速确认 corp-action 表「是否还在被填」。

```promql
time() - fundai_corp_action_ingest_last_success_unix
```

**解读**: 单位秒。
- < 12 * 3600 (12h) → 健康，loop 在跑。
- 12h ~ 24h → 一次 tick 漏了。slog 看 `corp-action ingest: collect holdings`。
- > 7 * 24 * 3600 (7d) → **告警**。Eastmoney/Yahoo 长期不可达，或 leader 切换出了问题。

### 8.2 距离上一次 tick 多久（包含 skip）

```promql
time() - fundai_corp_action_ingest_last_tick_unix
```

**解读**: 任何 tick（含 `skipped_not_leader`）都会更新这个 gauge，能区分「loop 还在跑只是没拿到 lease」和「loop goroutine 卡死」。
- `last_tick_unix` 持续推进 + `last_success_unix` 不动 → leader 一直在别处 (检查 `fundai_scheduler_leader_state`)。
- `last_tick_unix` 也不动 > 13h → goroutine 真死了，看 panic 日志。

### 8.3 每 tick 的 status 分布

```promql
sum by (status) (increase(fundai_corp_action_ingest_ticks_total[24h]))
```

**解读**: 24h 窗口内 ticks 计数：
- `status=ok` → 正常 leader 跑 + 拿到 holdings。
- `status=skipped_no_holdings` → 系统里没活跃持仓（dev/staging 常见）。
- `status=skipped_not_leader` → N-1 个 replica 的常态。N replica 时该值 ≈ 2 * (N - 1)（每 12h 一次）。

### 8.4 provider 错误率（按市场 + transient/fatal 拆）

```promql
sum by (market, outcome) (increase(fundai_corp_action_ingest_provider_errors_total[1h]))
```

**解读**:
- `outcome=transient` → 上游 EOF / connection reset。loop 会自动重试一次，重试结果在 `fundai_corp_action_ingest_retries_total` 里。
- `outcome=fatal` → 4xx / 解析失败 / 自定义错误。**告警**：`market=a_share` 长期 fatal 通常意味着 Eastmoney 把出口 IP 拉黑了；`market=us_equity` fatal 通常是 Yahoo schema 改了。

### 8.5 重试成功率

```promql
sum by (market) (increase(fundai_corp_action_ingest_retries_total{outcome="succeeded"}[24h]))
  /
clamp_min(sum by (market) (increase(fundai_corp_action_ingest_retries_total[24h])), 1)
```

**解读**: 重试中**成功**的占比。健康值 > 0.5（说明上游真的只是抖动）。
- < 0.2 长期持续 → 重试预算（默认 1 次）不够，或 retryable 误把 fatal 误判为 transient。

### 8.6 事件流量分布（split / cash_dividend / combined）

```promql
sum by (action) (increase(fundai_corp_action_ingest_events_total{phase="upserted"}[24h]))
```

**解读**: 24h 内成功入表的事件数，按动作类型拆。
- A-share 财报季（4 月、8 月、10 月）`cash_dividend` 会有明显的尖峰。
- 美股 `split` 全年大约 5-10 起，平时该值是 0 完全正常。
- `combined`（送股 + 派息一次性公告）只有 A-share 会出现。

### 8.7 应用结果分布

```promql
sum by (outcome) (increase(fundai_corp_action_ingest_apply_total[24h]))
```

**解读**:
- `outcome=applied` → 持仓 / 现金已经更新。这是主流。
- `outcome=missing` → 入表后发现该 fund 在持仓表里已被清零（settle 的 race，无害，下次 tick 不会再触发因为 collect 里也没了）。
- `outcome=error` → applier 真的报错。**告警**：通常意味着 corp-action 数学异常（split ratio 0）或 DB 锁竞争。

### 8.8 综合验收（单页 PromQL）

```promql
# (1) 距离上一次成功 ingest（应 < 12h）
time() - fundai_corp_action_ingest_last_success_unix

# (2) 24h 内 tick 状态分布
sum by (status) (increase(fundai_corp_action_ingest_ticks_total[24h]))

# (3) 24h 内 provider 错误（按市场+transient/fatal）
sum by (market, outcome) (increase(fundai_corp_action_ingest_provider_errors_total[24h]))

# (4) 24h 内事件入表（按 action）
sum by (action) (increase(fundai_corp_action_ingest_events_total{phase="upserted"}[24h]))

# (5) 24h 内 apply 结果（按 outcome）
sum by (outcome) (increase(fundai_corp_action_ingest_apply_total[24h]))
```

### 8.9 Grafana 面板建议

在 `fundai-overview` dashboard 加一个 *Corp Action Ingest* row，按这个布局：

| 行 | 面板类型 | PromQL                                                                 | 说明                          |
| -- | --------- | ---------------------------------------------------------------------- | ------------------------------ |
| 1  | stat      | `time() - fundai_corp_action_ingest_last_success_unix`                 | 单位 → human duration，红线 7d |
| 1  | stat      | `time() - fundai_corp_action_ingest_last_tick_unix`                    | 单位 → human duration，红线 13h |
| 2  | bar gauge | 8.3 的 status 分布                                                      | 看 leader 是否健康            |
| 2  | bar gauge | 8.4 的 provider error 分布                                              | 一眼看出哪个市场出问题        |
| 3  | time series | `rate(fundai_corp_action_ingest_events_total[1h])` 按 action          | 流量趋势                      |
| 3  | time series | `rate(fundai_corp_action_ingest_apply_total{outcome="error"}[1h])`    | apply 错误率                  |

报警建议（写进 `prometheus.rules.yml`）：

```yaml
groups:
- name: corp-action-ingest
  rules:
  - alert: CorpActionIngestStalled
    expr: time() - fundai_corp_action_ingest_last_success_unix > 7 * 24 * 3600
    for: 1h
    labels: { severity: page }
    annotations:
      summary: "Corp-action ingest 已 7 天未成功"
      runbook: "scripts/smoke-test.sh + 检查 corpActionIngest leader lease"
  - alert: CorpActionIngestProviderFatal
    expr: sum by (market) (increase(fundai_corp_action_ingest_provider_errors_total{outcome="fatal"}[1h])) > 5
    for: 30m
    labels: { severity: ticket }
    annotations:
      summary: "Corp-action provider {{ $labels.market }} fatal 错误激增"
      runbook: "看 server slog `corp-action ingest: provider fetch`，常见原因 = WAF block / vendor schema drift"
```

## 9. AB shadow LLM 调用与成本（Card K-5）

`AB_SHADOW_LLM_ENABLED=1` 打开后，每次 `AnalyzeTest`（变量类型 = `strategy_compare`）会：

- 对 A 组每笔交易调一次 LLM（决策 B 组动作） → `outcome=decided_by_llm` 或某个 `fallback_*`
- 在 run 结束时调一次 LLM（生成 B 组学习总结） → `outcome=recap_*`

每次 LLM 调用都会 +1 到唯一一个 counter：

```
fundai_ab_shadow_llm_calls_total{outcome="..."}
```

`outcome` 取值：

| outcome | 含义 |
| --- | --- |
| `decided_by_llm` | 单笔 trade decision 走完了 LLM 路径，模型给了合法 JSON |
| `fallback_llm_error` | LLM 调用失败（超时/拒答/网络），用 deterministic 兜底 |
| `fallback_parse_error` | LLM 返回了内容但 JSON 不合法，用 deterministic 兜底 |
| `fallback_budget_cap` | 该 run 已超过 `AB_SHADOW_LLM_MAX_CALLS`，剩余 trade 跳过 LLM |
| `recap_decided_by_llm` | end-of-run learning recap 成功 |
| `recap_fallback_llm_error` | recap LLM 调用失败 |
| `recap_fallback_parse_error` | recap JSON 不合法 |

### 9.1 LLM 烧钱速率（calls/秒）

```promql
sum(rate(fundai_ab_shadow_llm_calls_total{outcome=~"decided_by_llm|recap_decided_by_llm"}[5m]))
```

× 单次调用平均 token 数 × 模型每 1k token 单价 = $/秒。

### 9.2 fallback 比例（健康度，>5% 报警）

```promql
sum(rate(fundai_ab_shadow_llm_calls_total{outcome=~"fallback_.*|recap_fallback_.*"}[15m]))
/
clamp_min(sum(rate(fundai_ab_shadow_llm_calls_total[15m])), 1)
```

- 持续 > 0.05 → prompt 设计或模型版本可能漂移了
- 突然冲到 1.0 → LLM provider 完全挂了（看 `fallback_llm_error` 是否独自占满）

### 9.3 budget cap 命中率（容量是否够）

```promql
sum(increase(fundai_ab_shadow_llm_calls_total{outcome="fallback_budget_cap"}[24h]))
/
clamp_min(sum(increase(fundai_ab_shadow_llm_calls_total[24h])), 1)
```

> 0.10 即说明 `AB_SHADOW_LLM_MAX_CALLS` 太小，B 组只看到了一半 trade 就被截断了。先调大再考虑成本控制。

### 9.4 单次 Analyze 的平均 LLM 调用量

```promql
sum(increase(fundai_ab_shadow_llm_calls_total[24h])) / clamp_min(sum(increase(fundai_workflow_transitions_total{to="analyzed"}[24h])), 1)
```

用来确认：每次跑 AB 实验大致花多少次 LLM。结合定价做日预算。

### 9.5 推荐 Grafana 面板

| 区 | 类型 | PromQL | 备注 |
| - | - | - | - |
| 1 | stat | `sum(rate(fundai_ab_shadow_llm_calls_total[5m])) * 60` | calls/分钟，直观看烧钱 |
| 1 | stat | `9.2 的 fallback 比例` | 健康度，红线 0.05 |
| 2 | bar gauge | `sum by (outcome) (increase(fundai_ab_shadow_llm_calls_total[1h]))` | outcome 分布，一眼看出谁占大头 |
| 3 | time series | `sum by (outcome) (rate(fundai_ab_shadow_llm_calls_total[5m]))` | 趋势 + 异常事件定位 |

### 9.6 报警建议

```yaml
groups:
- name: ab-shadow-llm
  rules:
  - alert: ABShadowLLMFallbackHigh
    expr: |
      sum(rate(fundai_ab_shadow_llm_calls_total{outcome=~"fallback_.*|recap_fallback_.*"}[15m]))
      / clamp_min(sum(rate(fundai_ab_shadow_llm_calls_total[15m])), 1) > 0.05
    for: 30m
    labels: { severity: ticket }
    annotations:
      summary: "AB shadow LLM fallback 比例 > 5%"
      runbook: "确认 AB_SHADOW_LLM_MODEL 是否仍可用 + 检查 prompt 模板有没有规模溢出"
  - alert: ABShadowLLMBudgetCapHot
    expr: |
      sum(increase(fundai_ab_shadow_llm_calls_total{outcome="fallback_budget_cap"}[24h]))
      / clamp_min(sum(increase(fundai_ab_shadow_llm_calls_total[24h])), 1) > 0.10
    for: 1h
    labels: { severity: ticket }
    annotations:
      summary: "AB shadow LLM budget cap 命中率 > 10%"
      runbook: "调高 AB_SHADOW_LLM_MAX_CALLS 或缩短 AB 实验窗口"
```

---

## 附 A. 命名约定备忘

| 前缀                              | 含义                                       |
| ---                               | ---                                        |
| `fundai_http_*`                   | HTTP 路由层（请求数/延迟/状态码）         |
| `fundai_llm_*`                    | LLM provider 调用                          |
| `fundai_workflow_*`               | Plan workflow 状态流转                     |
| `fundai_decision_*`               | PM 决策输入装配（Sprint D #1 引入）        |
| `fundai_hard_risk_*`              | Execution-layer 硬风控                     |
| `fundai_db_*`                     | 数据库连接池                               |
| `fundai_scheduler_leader_state`   | 调度 leader lease (split-brain 检测)       |
| `fundai_marketplace_*`            | Agent marketplace reconciler              |
| `fundai_marketdata_*`             | Market-data provider 健康 + 位价刷新       |
| `fundai_corp_action_ingest_*`     | Corp-action 12h 摄入循环（Card G 引入）    |
| `fundai_ab_shadow_llm_calls_total`| AB shadow B-side LLM 调用计数（Card K-5 引入） |

## 附 B. 后续可加的指标 (TODO)

为了让 Sprint E 块在 dashboard 上有专属一列，下一波可加：

- `fundai_decision_quality_score_blocks_total{quartile}` —
  QMJ 四分位分布
- `fundai_decision_earnings_calendar_horizon_days_bucket` —
  上线后看 PM 实际感知到的事件距离分布
- `fundai_decision_pair_spread_z_abs_bucket` —
  pair-spread |z| 分布

短期暂无强需求，因为 `fundai_decision_input_blocks_total{block,present}`
已能回答 "是否注入了" 这个核心问题。
