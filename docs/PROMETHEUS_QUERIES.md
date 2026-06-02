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

## 10. 资金账本 (Cash ledger, P1-1)

### 10.1 cash-ledger 写入失败率

**场景**: 每笔成交本应在 cash_ledger 留下 4 条记录（本金 + 佣金 + 过户费 + 印花税），dividend 留 1 条。
失败时 trade 仍然提交（不阻塞下单），但 funds.current_capital 与 SUM(cash_ledger.amount)
会出现偏差，需要由对账作业重跑补齐。

```promql
sum(increase(fundai_cash_ledger_write_failures_total[1h]))
```

**解读**: 任意大于 0 的值都需要立刻处理。
- 关注 `entry_type=` 标签：如果只有 `trade_*_commission` 涨说明手续费列写挂了，不是 schema 问题。
- 配套：在每日 reconcile 跑 `SELECT id, current_capital, (SELECT COALESCE(SUM(amount),0) FROM cash_ledger WHERE fund_id = funds.id) FROM funds`，差值 > 0.01 报警。

### 10.2 报警建议

```yaml
groups:
- name: cash-ledger
  rules:
  - alert: CashLedgerWriteFailing
    expr: sum(increase(fundai_cash_ledger_write_failures_total[15m])) > 0
    for: 5m
    labels: { severity: page }
    annotations:
      summary: "cash_ledger 写入失败 — 资金对账风险"
      runbook: "检查 server log 关键字 'cash_ledger: append failed'，确认 DB 是否仍可写。失败的行可通过相同 idempotency_key 重放。"
```

---

## 11. 出入金 (Funding requests, P1-2)

`fundai_funding_request_events_total{event}` 记录出入金请求生命周期：

- `event=created` 用户提交一笔新请求
- `event=cancelled` 提交人撤回 pending 请求
- `event=approved` 不同 super_admin 通过审批，cash_ledger + funds.current_capital 已写入
- `event=rejected` 不同 super_admin 拒绝（带 reason）

### 关键 PromQL

| 名字           | 用途                                    | 表达式                                                                                                                                                                  |
| -------------- | --------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 审批通过率     | 24h 内 `approved / created`             | `sum(increase(fundai_funding_request_events_total{event="approved"}[24h])) / sum(increase(fundai_funding_request_events_total{event="created"}[24h]))` |
| 拒绝率         | 24h 拒绝占比                            | `sum(increase(fundai_funding_request_events_total{event="rejected"}[24h])) / sum(increase(fundai_funding_request_events_total{event="created"}[24h]))` |
| Pending 长尾   | 卡在审批队列里的请求 (用 access log 推算) | `sum(rate(fundai_funding_request_events_total{event="created"}[6h])) - sum(rate(fundai_funding_request_events_total{event=~"approved\|rejected\|cancelled"}[6h]))`     |

### 推荐告警

```yaml
- alert: FundingApprovalStuck
  expr: |
    sum(increase(fundai_funding_request_events_total{event="created"}[6h]))
      -
    sum(increase(fundai_funding_request_events_total{event=~"approved|rejected|cancelled"}[6h])) > 5
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "5+ funding requests pending more than ~6h"
    runbook: "/api/admin/funding-requests?status=pending 查看队列；超过 24h 的应通知 ops。"
```

---

## 12. FX 汇率（FX rates / cross-currency NAV，P1-4）

`fundai_fx_events_total{event}` 记录 FX 模块的生命周期事件：

- `event=fetch_ok` — 调度器从 Yahoo 抓到一次新汇率（每 6h 一轮，6 个对，所以稳定状态下 24 / round-trip）
- `event=fetch_error` — Yahoo 返回 429 / 5xx / 0 价格 / 网络超时
- `event=upsert_manual` — 操作员人工录入一笔 fx_rates
- `event=upsert_override` — 操作员覆盖之前抓到的错误值
- `event=convert_stale` — NAV 或 cash_ledger summary 在折算时遇到了缺失的汇率（保留原币种数值，前端渲染 `≈`）

| 场景             | 含义                                        | PromQL                                                                                                |
| ---------------- | ------------------------------------------- | ----------------------------------------------------------------------------------------------------- |
| 抓取健康度       | 24h 内 `fetch_ok / (ok + error)`            | `sum(increase(fundai_fx_events_total{event="fetch_ok"}[24h])) / clamp_min(sum(increase(fundai_fx_events_total{event=~"fetch_ok|fetch_error"}[24h])), 1)` |
| 抓取量基线       | 6h 期望抓 6 次成功（≈ 24/24h）              | `sum(increase(fundai_fx_events_total{event="fetch_ok"}[6h]))`                                         |
| 人工干预频次     | 24h 人工 upsert + override 总数             | `sum(increase(fundai_fx_events_total{event=~"upsert_manual|upsert_override"}[24h]))`                  |
| NAV 折算"陈旧"占比 | 5m 内 convert_stale 比例                  | `sum(increase(fundai_fx_events_total{event="convert_stale"}[5m]))`                                    |

### 推荐告警

```yaml
- alert: FXFetchUnhealthy
  expr: |
    sum(increase(fundai_fx_events_total{event="fetch_ok"}[6h])) < 1
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "FX scheduler 6h 内一次成功抓取都没有"
    runbook: "/api/admin/fx-rates 查看最新行；如缺失关键 USD 主导对，操作员可点击 'manual' 录入临时值。"

- alert: FXManualOverridesSpiking
  expr: |
    sum(increase(fundai_fx_events_total{event="upsert_override"}[1h])) > 5
  for: 15m
  labels:
    severity: info
  annotations:
    summary: "1h 内 >5 笔 override，请确认是否 Yahoo 输出异常"
    runbook: "对照 /api/admin/fx-rates?source=yahoo 与历史值，必要时切换备用 provider。"
```

注：cross-rate（如 CNY/HKD）一律通过 USD 三角化得到，因此监控只要看 USD 主导对（USD/CNY、USD/HKD、USD/EUR、USD/JPY、USD/GBP、USD/SGD）即可。

---

## 13. 日终对账（Reconciliation，P1-3）

`fundai_recon_events_total{event}` 记录 reconciliation 模块的生命周期事件。事件按"摄入 → 运行 → 差异 → 解决"分四组：

- 摄入：`event=ingest_ok` / `ingest_duplicate` / `ingest_error`
  分别对应一份 broker_statement 成功落库、命中去重哈希被忽略、或写入失败。
- 运行：`event=run_ok` / `run_failed`
  对应一次 reconciliation_run 完整跑完（包括 break 写库）/ 中途失败。
  `event=scheduled_skip` 表示日终 loop 拿不到 fund 列表，整轮跳过（目前只有 nil-FundLister 兜底场景会触发）。
- 差异：`event=break_<break_type>` 每条 break 各计一次。closed vocabulary 列在
  `internal/recon.BreakType`：
  `break_position_quantity_mismatch` /
  `break_position_avg_cost_mismatch` /
  `break_position_missing_internal` /
  `break_position_missing_broker` /
  `break_cash_balance_mismatch` /
  `break_cash_currency_missing_internal` /
  `break_cash_currency_missing_broker` /
  `break_trade_missing_internal` /
  `break_trade_missing_broker` /
  `break_trade_quantity_mismatch` /
  `break_trade_price_mismatch` /
  `break_trade_side_mismatch`
- 解决：`event=resolve_acknowledged` / `resolve_resolved` / `resolve_ignored` / `resolve_open`（重开）

| 场景             | 含义                                                    | PromQL                                                                                                            |
| ---------------- | ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- |
| 日终运行健康度   | 24h 内 `run_ok / (ok + failed)`                          | `sum(increase(fundai_recon_events_total{event="run_ok"}[24h])) / clamp_min(sum(increase(fundai_recon_events_total{event=~"run_ok\|run_failed"}[24h])), 1)` |
| 严重 break 速率  | 24h 内出现的 critical break 总数（含 sym/trade 各类）    | `sum(increase(fundai_recon_events_total{event=~"break_position_missing_internal\|break_position_quantity_mismatch\|break_trade_side_mismatch\|break_trade_missing_internal\|break_trade_missing_broker\|break_cash_currency_missing_internal"}[24h]))` |
| 操作员处理速度   | 1h 内 acknowledge + resolve 总数                         | `sum(increase(fundai_recon_events_total{event=~"resolve_acknowledged\|resolve_resolved\|resolve_ignored"}[1h]))`  |
| 重复摄入频次     | 24h 内命中重复哈希的次数（健康，多源摄入 mock 时常见）   | `sum(increase(fundai_recon_events_total{event="ingest_duplicate"}[24h]))`                                         |

### 推荐告警

```yaml
- alert: ReconDailyRunMissing
  expr: |
    sum(increase(fundai_recon_events_total{event="run_ok"}[36h])) < 1
  for: 1h
  labels:
    severity: critical
  annotations:
    summary: "36h 内一次成功的 reconciliation 都没有"
    runbook: "1) /api/admin/reconciliation/runs 查看最新运行；2) 检查 fund_repo.ListActive 是否仍然返回基金；3) 必要时手动 POST /api/admin/reconciliation/runs 触发一次。"

- alert: ReconCriticalBreakBacklog
  expr: |
    sum(increase(fundai_recon_events_total{event=~"break_position_missing_internal|break_position_quantity_mismatch|break_trade_side_mismatch|break_trade_missing_internal|break_trade_missing_broker"}[24h]))
      - sum(increase(fundai_recon_events_total{event=~"resolve_resolved|resolve_ignored"}[24h])) > 5
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "24h 内未处理的 critical break 累计 >5"
    runbook: "/api/admin/reconciliation/breaks?status=open&severity=critical"

- alert: ReconRunFailing
  expr: |
    sum(increase(fundai_recon_events_total{event="run_failed"}[1h])) > 3
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: "1h 内 >3 次 run_failed"
    runbook: "看日志中 'recon loop:' 前缀；常见原因：DB 不可达 / fund_id 越权 / 序列化问题。"
```

注：mock provider 阶段产生的 break 数量通常为 0（perfect mirror）。一旦真实 broker 适配器（CSV/FIX/REST）落地，这些指标就直接成为日终运维的主面板。

---

## 14. 交易监控（Trade Surveillance，P1-7）

`fundai_surveillance_events_total{event}` 记录监控模块每次 hourly 扫描以及合规复核动作的生命周期事件。
事件按"扫描运行 → 命中（按规则 / 按级别） → 复核动作"三组：

| 维度       | 事件标签                                                | 含义                                                                 |
| ---        | ---                                                     | ---                                                                  |
| 扫描运行   | `run_ok`、`run_failed`、`scheduled_skip`、`insert_error`| 单次扫描 wave 的成败 / 跳过 / 单事件写入失败                          |
| 命中（规则）| `event_wash_trade`、`event_marking_close`、`event_self_trade_pair`、`event_rapid_fire_reversal`、`event_layering_suspect` | 扫描产出某个规则的命中事件（每条新事件 +1，dedupe 不计）             |
| 命中（级别）| `severity_critical`、`severity_warning`、`severity_info`| 维度补充，让 dashboard 可以按级别画堆叠图                            |
| 复核动作   | `review_open`、`review_reviewing`、`review_cleared`、`review_escalated` | 操作员把某事件改成对应 status；用于分析复核 SLA                     |

### 推荐 PromQL

| 想看              | PromQL                                                                                                        |
| ---               | ---                                                                                                           |
| 扫描健康度        | `sum(increase(fundai_surveillance_events_total{event="run_ok"}[24h])) / clamp_min(sum(increase(fundai_surveillance_events_total{event=~"run_ok|run_failed"}[24h])), 1)` |
| 当日命中速率      | `sum(increase(fundai_surveillance_events_total{event=~"event_.*"}[24h]))`                                     |
| 严重命中速率      | `sum(increase(fundai_surveillance_events_total{event="severity_critical"}[24h]))`                              |
| 待复核积压（粗估）| 抓 DB `surveillance_events.status='open'` 的 count；指标只跟踪状态 *变化*，不跟踪当前积压                     |
| 复核效率          | `sum(increase(fundai_surveillance_events_total{event=~"review_cleared|review_escalated"}[1h]))`                |

### 推荐告警

```yaml

- alert: SurveillanceLoopMissing
  expr: |
    sum(increase(fundai_surveillance_events_total{event="run_ok"}[3h])) < 1
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "3h 内没有 surveillance run_ok（loop 应每小时跑一次）"
    runbook: "看 server 日志的 `surveillance loop:` 前缀；常见原因：DB 连接 / fund 列表为空 / panic。"

- alert: SurveillanceCriticalSpike
  expr: |
    sum(increase(fundai_surveillance_events_total{event="severity_critical"}[1h])) > 5
  for: 10m
  labels:
    severity: critical
  annotations:
    summary: "1h 内 >5 起 critical 监控事件"
    runbook: "立刻打开 /admin → 交易监控 面板；critical = self-trade 或 marking-close 同时命中尺寸+VWAP；优先 escalate。"

- alert: SurveillanceInsertErrorBurst
  expr: |
    sum(increase(fundai_surveillance_events_total{event="insert_error"}[15m])) > 10
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "15min 内 >10 次 surveillance insert_error"
    runbook: "通常是 DB 写入失败（约束 / 连接超时）。检查 `surveillance_events` 表当前是否被锁。"
```

注：监控事件命中本身 *不是* alert——命中是日常工作量。Alert 跟踪的是命中"突发"（severity_critical 暴涨）和系统层（loop 没跑、insert 失败）。

---

## 15. 回撤软熔断（Drawdown soft circuit breaker，P3-5）

`fundai_drawdown_events_total{event}` 记录每次 5 分钟 DD 扫描以及操作员处置动作的生命周期事件。

| event 标签                | 含义                                                                                  |
| ---                        | ---                                                                                   |
| `check_ok`                | 单基金扫描完成（含 evaluate）                                                          |
| `check_failed`            | 扫描或写入失败（snapshot / evaluate / insert）                                         |
| `breach_tier_1` … `_5`    | 命中第 N 档阈值                                                                        |
| `action_trim_proportional` | 命中事件的处置动作分布（冗余维度，配合 tier 看采纳率）                                  |
| `action_flatten`          | 命中事件的处置动作分布                                                                |
| `action_defensive_only`   | 命中事件的处置动作分布                                                                |
| `auto_executed`           | auto_execute=true 的档位通过 handler 成功挂单                                          |
| `review_approved`         | 操作员批准建议（可被 auto-execute 替代）                                              |
| `review_dismissed`        | 操作员驳回                                                                             |
| `review_superseded`       | 后续更深档位事件接管，前面建议被废止                                                   |
| `review_proposed`         | 操作员重开历史事件（事件维护用）                                                       |
| `policy_upsert`           | 任意档位被 upsert（含新增/修改）                                                      |
| `policy_delete`           | 删除一档                                                                              |
| `scheduled_skip`          | scheduler 没有 fund lister 或 lister 报错时的兜底标识                                  |

### 关键 PromQL

| 用途              | 表达式                                                                                                                                                              |
| ---               | ---                                                                                                                                                                 |
| 扫描健康度        | `sum(increase(fundai_drawdown_events_total{event="check_ok"}[1h]))`                                                                                                |
| 命中速率          | `sum(increase(fundai_drawdown_events_total{event=~"breach_tier_.*"}[24h]))`                                                                                        |
| 各档位命中分布    | `sum by (event) (increase(fundai_drawdown_events_total{event=~"breach_tier_.*"}[24h]))`                                                                            |
| 自动执行成功率    | `sum(increase(fundai_drawdown_events_total{event="auto_executed"}[24h])) / clamp_min(sum(increase(fundai_drawdown_events_total{event=~"breach_tier_.*"}[24h])), 1)`|
| 拒绝率            | `sum(increase(fundai_drawdown_events_total{event="review_dismissed"}[7d])) / clamp_min(sum(increase(fundai_drawdown_events_total{event=~"breach_tier_.*"}[7d])), 1)`|

### 推荐告警

```yaml
- alert: DrawdownLoopStalled
  expr: |
    sum(increase(fundai_drawdown_events_total{event="check_ok"}[30m])) < 1
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "30min 内未观测到任何 drawdown 检查 ok"
    runbook: "确认 scheduler 进程正在运行，检查 fund_repo.ListActive 是否报错。"

- alert: DrawdownDeepTierBurst
  expr: |
    sum(increase(fundai_drawdown_events_total{event=~"breach_tier_3|breach_tier_4|breach_tier_5"}[15m])) > 3
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "15min 内 >3 次深档位 (>=3) 回撤事件"
    runbook: "有可能多只基金同时被市场剧烈波动击穿，确认是否需要人工统一处置。"

- alert: DrawdownAutoExecuteFailures
  expr: |
    sum(increase(fundai_drawdown_events_total{event="check_failed"}[15m])) > 5
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "15min 内 >5 次 drawdown check_failed"
    runbook: "通常是 snapshot / DB 写失败；也可能是 auto-execute handler 报错。查看 fundai_drawdown 日志。"
```

注：跟 P1-7 类似，普通 breach 事件本身不是 alert——配置回撤阈值的目的就是希望它们触发。Alert 关心的是异常突发（深档位短时间集中）和系统层（loop 不跑、写入失败）。

---

## 16. 市场状态门控（Market-status gate，S6.1）

`fundai_marketstatus_events_total{event}` 记录每次订单进入撮合引擎前的可达性检查（停牌 / 涨跌停 / 陈旧报价 / 交易日历）以及操作员对状态/日历的修改动作。

| event 标签                       | 含义                                                                            |
| ---                              | ---                                                                              |
| `allow`                          | 订单通过所有规则                                                                |
| `reject_halted`                  | 因停牌被拒                                                                      |
| `reject_suspended`               | 因长期暂停被拒                                                                  |
| `reject_price_limit`             | 限价超出涨跌停被拒                                                              |
| `reject_market_closed`           | 当日市场未开 / 已收盘被拒                                                       |
| `reject_half_day_closed`         | 半天市早盘后被拒                                                                |
| `warn_stale_quote`               | 报价过陈旧告警（订单仍放行，警告附在 Order.Warnings）                          |
| `warn_half_day_closed`           | 仍处于半天市时段告警                                                            |
| `warn_market_closed`             | 时区配置异常导致无法判定，降级为告警                                            |
| `lookup_failed`                  | 标的状态查表失败（fail-open）                                                   |
| `calendar_lookup_failed`         | 日历查表失败（fail-open）                                                       |
| `evaluate_failed`                | 引擎报错（fail-open）                                                           |
| `persist_failed`                 | 事件持久化失败（best-effort，不改变判定）                                       |
| `admin_halt`/`admin_unhalt`      | 操作员便捷接口动作                                                              |
| `admin_set_limits`               | 设置涨跌停                                                                      |
| `admin_upsert`                   | 通用 upsert                                                                     |
| `admin_calendar_upsert`          | 日历日维度 upsert                                                               |

### 关键 PromQL

| 用途              | 表达式                                                                                                                                                                |
| ---               | ---                                                                                                                                                                   |
| 拒单速率          | `sum(increase(fundai_marketstatus_events_total{event=~"reject_.*"}[1h]))`                                                                                            |
| 通过率            | `sum(increase(fundai_marketstatus_events_total{event="allow"}[24h])) / clamp_min(sum(increase(fundai_marketstatus_events_total{event=~"allow|reject_.*|warn_.*"}[24h])), 1)` |
| 告警速率（陈旧报价主因） | `sum(increase(fundai_marketstatus_events_total{event=~"warn_.*"}[1h]))`                                                                                              |
| 内部失败          | `sum(increase(fundai_marketstatus_events_total{event=~"lookup_failed|calendar_lookup_failed|evaluate_failed|persist_failed"}[15m]))`                                  |

### 推荐告警

```yaml
- alert: MarketStatusInternalFailures
  expr: |
    sum(increase(fundai_marketstatus_events_total{event=~"lookup_failed|calendar_lookup_failed|evaluate_failed|persist_failed"}[15m])) > 50
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: "15min 内 >50 次 marketstatus 门控内部失败"
    runbook: "确认 instrument_market_status / trading_calendar 表是否可达；该路径默认 fail-open，所以堆积不会立刻拒单，但已影响审计完整性。"

- alert: MarketStatusRejectBurst
  expr: |
    sum(increase(fundai_marketstatus_events_total{event=~"reject_.*"}[5m])) > 100
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "5min 内 >100 次门控拒单"
    runbook: "通常是某个标的被错误标为 halted 或涨跌停设置错误。在 admin UI -> 市场状态门控 中检查最近 admin_* 修改。"

- alert: MarketStatusStaleQuoteSurge
  expr: |
    sum(increase(fundai_marketstatus_events_total{event="warn_stale_quote"}[5m])) > 200
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: "5min 内 >200 条 stale_quote 警告"
    runbook: "市场数据 ingest 可能落后；检查 marketdata 进程与 quote_metadata 写入速率。"
```

注：拒单本身不是异常——配置涨跌停/停牌的目的就是希望命中。Alert 跟踪的是异常突发（短时间集中拒单）和系统层（fail-open 路径在持续兜底）。

## 17. 市场冲击 / 大单滑点（Market-impact, S6.2）

S6.2 把模拟器的 slippage 由固定 bps 升级为带 ADV / σ 的平方根冲击模型。每个标的可在 admin UI 录入校准（`instrument_liquidity`），也可空缺让引擎走资产类别默认值。

### 主要指标

```
fundai_marketimpact_events_total{event="..."}
```

`event` 子集与含义：

| event | 含义 |
|---|---|
| `estimate` | 引擎被调用一次（每笔下单都会 +1） |
| `used_defaults` | 该标的没有校准行，引擎使用资产类别默认 |
| `used_adv_fallback` | 校准行存在但 ADV 缺失，引擎只返回 `min_bps` |
| `bucket_<asset>_<bps_bucket>` | 估算结果落入哪一档（`0_5`/`5_20`/`20_50`/`50_100`/`100_250`/`250_plus`） |
| `admin_upsert` | admin 在 UI 录入或修改一行校准 |
| `admin_delete` | admin 删除一行校准 |
| `admin_preview` | admin 跑了一次预演（不下单） |
| `admin_cache_refresh` | admin 强制刷新内存缓存 |
| `cache_refresh_ok` / `cache_refresh_err` | 启动 + 周期性 5min 刷新结果 |

### 推荐查询

```promql
# 每秒 estimate 调用速率（应该 ≈ 下单速率）
sum(rate(fundai_marketimpact_events_total{event="estimate"}[5m]))

# 默认值占比（比例越高说明覆盖率越低，需要继续校准）
sum(rate(fundai_marketimpact_events_total{event="used_defaults"}[1h]))
  / clamp_min(sum(rate(fundai_marketimpact_events_total{event="estimate"}[1h])), 1)

# ADV 回退占比（行有但 ADV 缺）
sum(rate(fundai_marketimpact_events_total{event="used_adv_fallback"}[1h]))
  / clamp_min(sum(rate(fundai_marketimpact_events_total{event="estimate"}[1h])), 1)

# 大单（>100 bps）占比 by 资产类别
sum by (event)(rate(fundai_marketimpact_events_total{event=~"bucket_.*_(100_250|250_plus)"}[1h]))
  / clamp_min(sum(rate(fundai_marketimpact_events_total{event="estimate"}[1h])), 1)

# 缓存刷新失败（应当为 0）
increase(fundai_marketimpact_events_total{event="cache_refresh_err"}[1h])
```

### 推荐告警

```yaml
- alert: MarketImpactDefaultsHigh
  expr: |
    (sum(rate(fundai_marketimpact_events_total{event="used_defaults"}[1h])) /
     clamp_min(sum(rate(fundai_marketimpact_events_total{event="estimate"}[1h])), 1))
     > 0.6
  for: 30m
  labels:
    severity: warning
  annotations:
    summary: "60% 以上的下单仍走资产类别默认值"
    runbook: "大量标的还没有校准行；运营需要继续往 instrument_liquidity 里录入 ADV/波动率。"

- alert: MarketImpactCacheRefreshErrors
  expr: |
    increase(fundai_marketimpact_events_total{event="cache_refresh_err"}[10m]) > 3
  for: 10m
  labels:
    severity: warning
  annotations:
    summary: "marketimpact cache 周期刷新连续失败"
    runbook: "通常是 DB 连接抖动；检查 cmd/server 日志中的 cache_refresh_err 记录与 fundai_db_* 健康。短期撮合仍可继续——cache 持有最后一次成功刷新的行。"

- alert: MarketImpactRunawayBps
  expr: |
    sum(increase(fundai_marketimpact_events_total{event=~"bucket_.*_250_plus"}[15m])) > 50
  for: 15m
  labels:
    severity: warning
  annotations:
    summary: "15min 内 >50 笔订单冲击 ≥ 250 bps"
    runbook: "可能是 (1) 某只标的 ADV 配置错误（数量级偏小） 或 (2) 撮合策略生成了远超 ADV 的大单。点开 admin UI > 大单冲击模型，按 bucket_ 过滤近期 estimate。"
```

### 触发条件

冲击模型在以下时点被调用：

- 模拟器 `PlaceOrder` 撮合每一笔订单时（adapter 在 `FillPrice` 入口收集 metric）。
- admin UI 的 `Preview` 按钮点击（不下单，仅算 bps）。
- 强制 `cache/refresh` 后会刷新 `cache_refresh_ok`，下一次 estimate 仍会正常计数。


---

## 18. IPO / 受限股 lock-up 门控（Lock-up gate, S6.3）

S6.3 给 broker 模拟器接了第二个 pre-trade gate：当 SELL 数量超过
(`持仓 - 活跃 lock-up qty`) 时拒单（`broker.ErrLockupRejected`）。
buy 永不触发；DB hiccup 全部 fail-open。

### 18.1 当前 1 小时内：lock-up 拒单次数

```promql
increase(fundai_lockup_events_total{event="check_reject_locked"}[1h])
```

预期 `0` 或个位数。突然飙升 → admin 把锁定期或 qty 配多了；用 admin UI 的
`released_at` 面板回放最近的提前释放是否漏发。

### 18.2 fail-open 比例 = lookup 失败次数 ÷ 总检查次数

```promql
sum(rate(fundai_lockup_events_total{event=~"gate_lookup_failed|position_lookup_failed"}[15m]))
  /
sum(rate(fundai_lockup_events_total{event=~"check_.*"}[15m]))
```

阈值 `> 0.005`（0.5% 的下单触发了 fail-open）→ DB / position table 异常，
立即调查；门控当前已经放行，可能漏挡受限卖单。

### 18.3 SELL 中受 lock-up 影响的比例

```promql
sum(rate(fundai_lockup_events_total{event=~"check_reject_locked|check_allow"}[24h]))
  /
sum(rate(fundai_lockup_events_total{event!~"check_allow_non_sell|admin_.*"}[24h]))
```

> 0 但合理；持续 0 通常意味着 lock-up 表是空的（基金没有任何 IPO/RSU 配置），
检查是否漏建。

### 18.4 admin 写操作 24h 节奏

```promql
sum by (event) (
  increase(fundai_lockup_events_total{event=~"admin_.*"}[24h])
)
```

按 event 拆分能看到 `admin_create / admin_update / admin_release / admin_delete`
的节奏。`admin_release` 集中爆发 → 接近批量解锁日（IPO 锁定期到期），合预期；
`admin_delete` 应当 ≈ 0（删除是 typo-fix 的兜底，UI 强制走 release 路径）。

### 18.5 推荐告警

| 名称                       | 表达式                                                                                        | for | 说明                                          |
| -------------------------- | --------------------------------------------------------------------------------------------- | --- | --------------------------------------------- |
| `LockupFailOpenRateHigh`   | `sum(rate(fundai_lockup_events_total{event=~"gate_lookup_failed|position_lookup_failed"}[15m])) / sum(rate(fundai_lockup_events_total{event=~"check_.*"}[15m])) > 0.005` | 5m  | DB 抖动 → 门控变形同虚设，立即排查            |
| `LockupHardDeleteSpike`    | `increase(fundai_lockup_events_total{event="admin_delete"}[1h]) > 5`                          | 5m  | 短时间多次 hard-delete → 通常是误操作或脚本   |
| `LockupRejectsBurst`       | `increase(fundai_lockup_events_total{event="check_reject_locked"}[5m]) > 50`                  | 5m  | 5 分钟内 50 + 单被拒 → 配置错误（qty / until），通知 PM |

### 触发条件

锁定期门控会在以下时点写 metric：

- broker `PlaceOrder` 收到 SELL：`check_allow / check_reject_locked / check_reject_no_position / check_allow_no_lockup`。
- broker `PlaceOrder` 收到 BUY：`check_allow_non_sell`（短路，不查 DB）。
- admin REST 写操作：`admin_create / admin_update / admin_release / admin_delete`。
- 故障路径：`gate_lookup_failed / position_lookup_failed / check_no_repo`（fail-open 已生效）。


---

## 19. 借券 / locate 费（Securities-borrow gate + accrual, S6.4）

S6.4 给 broker 增加了第三个 pre-trade gate（继 marketstatus / lockup
之后）并新增一个 EOD 日终循环：

- pre-trade locate gate：SHORT 开仓时按 `instrument_key` 查
  `security_borrow_rates`，若 `availability='unavailable'`
  或 `available_shares < requested_qty` 直接拒单
  （`broker.ErrBorrowRejected`），HTB 时附带 fee 提示。
- daily accrual loop：每天 23 时启动一次，扫所有
  `holding_positions.quantity < 0` 的短仓，按
  `notional × rate / 365` 写一条 `borrow_fee` 到 cash_ledger
  并 upsert 一条短仓借券台账（fund×instrument×day 唯一）。

两条链路共用同一个 metric 名：`fundai_borrow_events_total{event="..."}`。
event 命名约定：`check_*` 是 gate 路径，`accrual_*` 是日终循环路径，
`admin_*` 是 admin REST 路径。

### 19.1 当前 1 小时内：locate 拒单次数（按原因拆）

```promql
sum by (event) (
  increase(fundai_borrow_events_total{event=~"check_reject_.*"}[1h])
)
```

`check_reject_unavailable` 突增 → 通常是 admin 把某些 ticker 切到
`unavailable`；`check_reject_insufficient` 突增 → 计划仓位超过了
agent lender 当日可融券规模。

### 19.2 fail-open 比例 = 失败次数 ÷ 所有 short check 次数

```promql
sum(rate(fundai_borrow_events_total{event=~"position_lookup_failed|audit_log_failed|no_calibration"}[15m]))
  /
sum(rate(fundai_borrow_events_total{event=~"check_.*"}[15m]))
```

阈值 `> 0.01` → 短仓没有真正被借券 gate 约束，需要立刻排查
（DB 抖动、calibration 表为空）。

### 19.3 当日 borrow_fee 累计（USD）

```promql
sum(increase(fundai_borrow_events_total{event="accrual_booked"}[24h]))
```

注意这是「次数」，不是金额；金额查 cash_ledger
（`SELECT SUM(amount) FROM cash_ledger_entries WHERE entry_type='borrow_fee' AND posted_at >= NOW() - INTERVAL '1 day'`）。

如果次数为 0 而存在 active 短仓 → 日终循环没跑（leader 抢锁失败 / 启动
小时配错了）；优先看 `run_completed` 和 `scan_failed`。

### 19.4 admin 操作 24h 节奏

```promql
sum by (event) (
  increase(fundai_borrow_events_total{event=~"admin_.*"}[24h])
)
```

`admin_upsert_rate` 集中 → 是 calibration 批量刷新；
`admin_delete_rate` 应当 ≈ 0（删除等价于 unavailable，谁会真的删）。

### 19.5 推荐告警

| 名称                          | 表达式                                                                                          | for | 说明                                                       |
| ----------------------------- | ----------------------------------------------------------------------------------------------- | --- | ---------------------------------------------------------- |
| `BorrowFailOpenHigh`          | `sum(rate(fundai_borrow_events_total{event=~"position_lookup_failed|audit_log_failed|no_calibration"}[15m])) / sum(rate(fundai_borrow_events_total{event=~"check_.*"}[15m])) > 0.01` | 5m  | 借券 gate 失灵；短仓没有 locate 约束                       |
| `BorrowAccrualMissed`         | `absent_over_time(fundai_borrow_events_total{event="run_completed"}[26h])`                       | 26h | 24h+ 没有跑过一次日终；leader 抢锁失败或调度死循环         |
| `BorrowRejectsBurst`          | `increase(fundai_borrow_events_total{event=~"check_reject_.*"}[5m]) > 50`                       | 5m  | 5 分钟内 50+ 单短仓被拒；通常是 calibration 突然改成 HTB   |
| `BorrowCacheRefreshErrors`    | `increase(fundai_borrow_events_total{event="admin_cache_refresh"}[1h])` _without alerting_      |     | 仅用于审计：admin 手动 refresh 次数（不报警，看趋势）      |

### 触发条件

- broker `PlaceOrder` 进入 borrow gate：`check_allow_short / check_allow_no_borrow / check_allow_non_sell / check_reject_*`。
- borrow gate 内部异常路径：`position_lookup_failed / audit_log_failed / no_calibration`。
- 日终 loop：`run_completed / scan_failed / scan_row_failed / accrual_booked / accrual_skipped_* / book_failed`。
- admin REST：`admin_upsert_rate / admin_delete_rate / admin_cache_refresh`。


---

## 20. WebSocket 实时行情（S6.5）

### 全部事件名

`fundai_wsfeed_events_total{event="..."}`。

按链路分类：

| 链路 | 事件 |
| --- | --- |
| 报价 / 撮合热路径 | `tick_applied`、`quote_cache_hit`、`quote_miss_fallback_ok`、`quote_miss_fallback_err`、`quote_stale_fallback_ok`、`quote_stale_served_on_error` |
| Provider 生命周期 | `state_connecting`、`state_connected`、`state_reconnecting`、`state_backoff`、`state_disconnected`、`state_closed`、`state_unknown`、`manager_error` |
| 订阅 bridge | `reconcile_ok`、`reconcile_added`、`reconcile_removed`、`reconcile_query_err`、`reconcile_subscribe_err`、`reconcile_unsubscribe_err` |
| Admin | `admin_subscribe`、`admin_unsubscribe`、`admin_cache_evict`、`admin_reconcile`、`admin_force_reconnect` |

### 1) WS 是否真的在分担 REST 压力

```promql
sum(rate(fundai_wsfeed_events_total{event="quote_cache_hit"}[5m]))
/
sum(rate(fundai_wsfeed_events_total{event=~"quote_(cache_hit|miss_.*|stale_.*)"}[5m]))
```

预期：WS 配通后，活跃交易时段命中率 > 80%。低于 50% 通常意味着订阅没追上持仓（看下 `reconcile_added`）或 staleAfter 设得太短。

### 2) Provider 是否健康

```promql
# 当前每个 provider 的状态（来自 admin /status；这里用 reconnect count 做近似）
increase(fundai_wsfeed_events_total{event=~"state_(reconnecting|disconnected|backoff)"}[1h])
```

告警建议：单 provider 1h 内 reconnect ≥ 3 → 翻一下网络 / 上游凭证。

### 3) 同步订阅的 bridge 是不是在干活

```promql
sum(rate(fundai_wsfeed_events_total{event="reconcile_ok"}[5m]))
```

bridge 默认每 30s 一次 → 5m 期望 ≈ 10。降到 0 → bridge 没起来（看 startup 日志）。

```promql
sum(increase(fundai_wsfeed_events_total{event="reconcile_query_err"}[15m]))
```

DB 抖动会让 reconcile 失败；非 0 时去看 holding_positions 查询性能。

### 4) Fan-out 有没有丢

```promql
sum(rate(fundai_wsfeed_events_total{event="manager_error"}[15m]))
```

通常 ≈ 0。> 0 意味着 handler panic（已 recover，但日志里会看到 stack）。

`/api/admin/wsfeed/status` 的 `dropped_events` gauge 也建议接进 Grafana：>0 表示有 handler 太慢吞掉了 inbound channel buffer。

### 5) Stale 服务

```promql
sum(rate(fundai_wsfeed_events_total{event="quote_stale_served_on_error"}[15m]))
```

当 REST 也挂掉 + cache 已过期时，broker 会 serve stale price 而不是拒单。这条 > 0 是个软告警：实盘场景下需要评估是否要切到「stale 一定拒单」策略（broker 侧未来可加 flag）。

### 6) 报警阈值清单

| 告警 | 表达式 | 严重度 |
| --- | --- | --- |
| WS 完全没在分担 REST | 30 min 命中率 < 5% 且 `total_ticks > 0` | warning |
| Provider 飞速重连 | `increase(fundai_wsfeed_events_total{event="state_reconnecting"}[10m]) > 5` | warning |
| Bridge 卡死 | `rate(fundai_wsfeed_events_total{event="reconcile_ok"}[10m]) == 0` 且服务运行 > 5 min | warning |
| Handler panic | `rate(fundai_wsfeed_events_total{event="manager_error"}[5m]) > 0` | critical |
| Dropped events 持续上涨 | `delta(<status.dropped_events>[10m]) > 100` | critical |


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
| `fundai_cash_ledger_*`            | 基金资金账本写入健康（P1-1 引入）          |
| `fundai_funding_request_events_total` | 出入金审批生命周期（P1-2 引入）        |
| `fundai_recon_events_total`       | 日终对账 ingest / run / break / resolve（P1-3 引入） |
| `fundai_surveillance_events_total`| 交易监控 run / event / severity / review（P1-7 引入）|
| `fundai_drawdown_events_total`    | 回撤软熔断 check / breach / review / policy（P3-5 引入）|
| `fundai_marketstatus_events_total`| 市场状态门控 allow / reject / warn / admin（S6.1 引入）|
| `fundai_marketimpact_events_total`| 大单冲击模型 estimate / bucket / admin（S6.2 引入）|
| `fundai_lockup_events_total`      | IPO / 受限股 lock-up 门控（S6.3 引入）              |
| `fundai_borrow_events_total`      | 借券 locate gate + 日终计费（S6.4 引入）             |
| `fundai_lot_ledger_failures_total`| FIFO lot ledger 写入失败（Phase 3A-1 引入）|

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
