# Monday Triage Playbook — PM Decision Pipeline

> 创建日期: 2026-05-23 · 适用范围: Sprint A/B/C/D 上线后的第一次开市日决策回看
> 受众: 系统运维 + 基金 owner · 目标: 周一开盘后 30 分钟内判断"系统是否按设计跑了"

---

## 0. 一句话使用说明

周一上午 09:25 之前过一遍 [§1 Pre-open](#1-pre-open-08000900-本地时间-开盘前-30-分钟)；
开盘后第一笔 PM plan 落库就走 [§2 First-plan triage](#2-first-plan-triage-开盘后第一笔-plan)；
午盘 12:00 拿 [§3 Mid-day health check](#3-mid-day-health-check-午盘-1200-1400) 那张表过一遍 Prometheus 指标；
收盘后跑一次 [§4 Post-close review](#4-post-close-review-收盘后) 看 Plan.Reasoning 是否引用了所有信号块。

任何一个步骤里命中 [§5 Failure modes](#5-failure-modes-按出现频率排序) 表里的 symptom，按对应 playbook 走。

---

## 1. Pre-open (08:00–09:00 本地时间, 开盘前 30 分钟)

### 1.1 容器健康

```bash
docker compose ps
docker compose logs --tail=200 app | grep -E 'ERROR|FATAL|panic'
```

期望: `fundai-app` / `fundai-postgres` / `fundai-prometheus` 均 `healthy`。
不健康 → `docker compose restart app`，重启后等 90 秒再看 metrics。

### 1.2 LLM 链路自检

```bash
docker exec fundai-app curl -sS -X POST http://localhost:8080/api/internal/llm/ping \
  -H 'X-Internal-Token: '"$INTERNAL_TOKEN" -d '{"provider":"gemini","prompt":"ping"}'
```

期望: HTTP 200 + 一段非空文本回包。
若失败 → 跳到 [§5.1 LLM 全链路失败](#51-llm-全链路失败) 。

### 1.3 关键基金的 PM agent 路由确认

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT f.id, f.name, a.id AS pm_agent_id,
       a.model_provider, a.model_name
  FROM funds f
  JOIN agents a ON a.id = f.pm_agent_id
 WHERE f.status = 'active'
 ORDER BY f.created_at DESC LIMIT 10;
"
```

期望:
- `model_provider` 为空表示走平台默认（推荐配置），有值表示该 agent 想用指定 provider。
- 任何 `model_provider` ∈ {claude, openai} 且对应 `*_API_KEY` 未配置 → 已经被 `WithPlatformDefault` 兜底到 gemini，但应主动加上 key 或清空 `model_provider` 让 router 走默认路径，否则每次 PM 调用都会先打一次失败请求。
  对应 fix:
  ```sql
  UPDATE agents SET model_provider = NULL, model_name = NULL WHERE id = '<pm_agent_id>';
  ```

### 1.4 Fund-level policy 抽查

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT id, name,
       config->>'exitPolicy'        AS exit_policy,
       config->>'exposurePolicy'    AS exposure_policy,
       config->>'correlationPolicy' AS correlation_policy
  FROM funds WHERE status='active'
 ORDER BY name;
"
```

期望: 每只活跃基金都至少应有 `exitPolicy.enabled = true` 才能享受 deterministic exit 保护（Sprint 3A-2 + Sprint D #3 ATR stop）。
`exposurePolicy` / `correlationPolicy` 可为空（走平台默认 25%/50%/60%/5% 与 60d/0.7 阈值），但若该基金是窄主题（≤8 个标的）建议覆盖一个更严格的 `singleNameCapPct`。

---

## 2. First-plan triage (开盘后第一笔 plan)

### 2.1 找到刚刚生成的 plan

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT id, fund_id, status, confidence,
       LEFT(reasoning, 200) AS reasoning_preview, created_at
  FROM investment_plans
 WHERE created_at > NOW() - INTERVAL '15 minutes'
 ORDER BY created_at DESC LIMIT 5;
"
```

### 2.2 读 fingerprint trace（决定输入是否完整）

每次 PM 调用前会写一条 `decision_input_fingerprint` slog：

```bash
docker compose logs --since=15m app | grep decision_input_fingerprint | tail -1 | sed 's/^[^{]*//'
```

健康基线（在有 universe + 至少一条持仓的情况下）:
- `count_universe > 0`, `count_quant_snapshots > 0`, `count_universe_ranking > 0`
- 至少 8 个 `p_*` 字段为 `true`，包括 `p_roundtable_stance`、`p_bull_case`、`p_bear_case`、`p_quant_case`、`p_symbol_verdicts`。
- 若 `count_positions > 0` 则 `p_risk_budget=true`、`p_exposure=true`、`p_correlations=true`。
- 若上一次决策已成交：`p_cooldowns=true`、`count_cooldowns > 0`。

如果 `p_roundtable_stance / p_bull_case / p_bear_case / p_quant_case` **全为 false** → 看 [§5.2 Debate 信号全失](#52-debate-信号全失)。

### 2.3 抽查 PM 是否真在引用这些信号

`Plan.Reasoning` 应该提到至少一个具体的信号块（regime / exposure / correlation / news / cooldown）。如果整段 reasoning 都是泛泛的 "based on technicals" → 模型在偷懒，把当前 plan 标记为 [§5.5 Reasoning vacuous](#55-reasoning-vacuous) 进入观察。

---

## 3. Mid-day health check (午盘 12:00–14:00)

打开 `http://localhost:9090/graph` (Prometheus)，过下面这张表：

| 指标 | 期望值 / 阈值 | 触发动作 |
|---|---|---|
| `fundai_decision_input_calls_total` | 单只活跃基金每小时 ≥ 1 | 0 → 触发链路未跑，去看 workflow scheduler |
| `fundai_decision_input_blocks_total{block="roundtable_stance",present="true"}` | / `_calls_total` 比 > 0.9 | < 0.5 → debate 频繁失败，[§5.2](#52-debate-信号全失) |
| `fundai_decision_input_blocks_total{block="risk_budget",present="true"}` | 有持仓的基金应≥1/h | 0 → NAV snapshots 未更新，[§5.4](#54-risk-budget-缺失) |
| `fundai_decision_exposure_breaches_total{kind=...}` | 任一 kind 累计 > 5/h | 集中度真的高了 → 该 fund 应该收紧 `exposurePolicy` 或主动 reduce |
| `fundai_decision_correlation_high_pairs_total` | 任一基金 > 30/h | 投资域高度同质化，提示 PM 用 candidateSummaries 截掉重复 buy |
| `fundai_decision_cooldown_vetos_total{symbol=...}` | 任一 symbol > 3/h | 同一标的频繁触发冷却 → 调查是否过度交易 |
| `fundai_decision_risk_budget_throttled_total{reason="drawdown"}` | > 1/h | 基金已进入 drawdown 缩仓状态，看 NAV 曲线 |
| `fundai_decision_risk_budget_throttled_total{reason="vol_target"}` | > 5/h | 实现波动率持续高于目标，PM 应缩 R 而不是加仓 |

---

## 4. Post-close review (收盘后)

### 4.1 当日 plan 与执行对账

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT p.fund_id, p.id, p.status, p.confidence,
       (SELECT COUNT(*) FROM plan_actions WHERE plan_id=p.id) AS actions,
       (SELECT COUNT(*) FROM trade_executions WHERE plan_id=p.id AND status='filled') AS filled
  FROM investment_plans p
 WHERE p.trading_date = CURRENT_DATE
 ORDER BY p.created_at DESC;
"
```

期望: 已批准 plan 的 `filled` ≥ 1（除非 plan 全是 `watch` 行动）。
`filled = 0` 但 actions > 0 → 路由 / 撮合环节有问题，去看 `trade_executions` 表 `error_message`。

### 4.2 Exit manager 是否正常触发

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT fund_id, signal_source, exit_reason, COUNT(*) AS n
  FROM plan_actions
 WHERE created_at::date = CURRENT_DATE
   AND sleeve = 'exit_manager'
 GROUP BY 1,2,3 ORDER BY n DESC;
"
```

新出现的 `exit_reason='atr_stop'` 是 Sprint D #3 上线后预期会看到的新种类。
若所有 exit 都是 `time_stop` → 大概率 `stopLoss` / `trailing` / `atrStop` 都没配置（或所有标的当天都没波动到阈值）。

### 4.3 PM 决策是否引用了新信号块（人工抽样）

随机取 1 只基金的当日 plan：

```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT reasoning FROM investment_plans
 WHERE trading_date = CURRENT_DATE
 ORDER BY random() LIMIT 1;
" | head -c 4000
```

期望: reasoning 段里能找到下列至少 3 个关键词：
- `ATR` / `regime`（Sprint A #1）
- `momentum` / `Q1` / `Q4` / `volatilityZ`（Sprint A #2）
- `cooldown` / `RECENT FILL`（Sprint B #1）
- `volScalar` / `ddScalar` / `drawdown` / `vol target`（Sprint B #2）
- `news` / `catalyst` / `headline`（Sprint B #3）
- `exposure` / `BREACH` / `single-name` / `sector cap`（Sprint C #1）
- `correlation` / `rho` / `heldCluster` / `candidateSummaries`（Sprint C #2）

3 个以上 → ✓ 模型在用新信号决策。
0–1 个 → 模型只在用老信号，[§5.5](#55-reasoning-vacuous) 。

---

## 5. Failure modes (按出现频率排序)

### 5.1 LLM 全链路失败

**Symptom**: `decision_input_fingerprint` 完全消失 / plan 长时间 `executing`。
**先看**: `docker compose logs --tail=200 app | grep -E 'llm relay|circuit'`。

| 诊断 | 处置 |
|---|---|
| `circuit breaker open` | 上游 provider 间歇性故障；等 30 秒看是否恢复，不行就 `LLM_PROVIDER=gemini` 兜底重启 |
| 大量 `missing provider credentials` | 该 agent 的 `model_provider` 指向了没有 API key 的 provider；Sprint D #1 后已 failover，但应该清掉 `model_provider` 让流量回到 platform default |
| `unmarshal: unexpected end of JSON` | 上游返回截断；切到次要 provider 重试一次 |

### 5.2 Debate 信号全失

**Symptom**: fingerprint 里 `p_roundtable_stance / p_bull_case / p_bear_case / p_quant_case` 全 false。
**原因**: 研究员 agents 全部失败 → 回退到 legacy consensus。
**确认**: `docker compose logs --since=10m app | grep -E 'researcher .* failed|legacy consensus'`。
**处置**:
1. 检查研究员 agent 的 `model_provider`（同 §1.3）；
2. 若是 platform default provider 自己挂了，临时降级到 OpenAI:
   ```bash
   docker compose exec app sh -c 'export LLM_PROVIDER=openai && kill -HUP 1'
   ```
3. Sprint D #1 之后失败的 provider 会被 `WithPlatformDefault` 兜底，但若兜底也失败 → 真的是网络问题，停掉决策窗口。

### 5.3 Exposure breach 暴增

**Symptom**: `fundai_decision_exposure_breaches_total{kind="single_name"}` 1 小时内 > 10。
**含义**: 某一只标的占比已经突破阈值（默认 25% 单名 / 50% 单行业 / 60% top-3 / 5% 现金）。
**先看**:
```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT fund_id, symbol, market_value / NULLIF(total_assets,0) AS weight
  FROM holding_positions
 WHERE quantity > 0
 ORDER BY weight DESC LIMIT 20;
"
```
**处置**:
- 若是合理的高 conviction → 在该基金 config 上把 `exposurePolicy.singleNameCapPct` 调高到 0.30；
- 若是漂移积累 → 等 PM 自然 reduce（exposure breach 现在会被 cite 进 reasoning，模型应该会触发 `qtyPct` 减半或 `watch`）；
- 若 PM 反复忽视 breach → 把对应 `model_provider` 临时换成更强的（claude / openai）。

### 5.4 Risk budget 缺失

**Symptom**: `p_risk_budget=false` 但 `count_positions > 0`。
**原因**: `nav_snapshots` 表当天还没 NAV → 波动率 + drawdown 计算不出来。
**确认**:
```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT fund_id, MAX(as_of) AS last_nav FROM nav_snapshots
 GROUP BY fund_id ORDER BY last_nav ASC LIMIT 5;
"
```
**处置**: 若 last_nav 已经 > 24 小时旧，触发 NAV recompute job:
```bash
docker exec fundai-app /app/scripts/nav-recompute.sh <fund_id>
```

### 5.5 Reasoning vacuous

**Symptom**: §4.3 抽样命中关键词 0–1 个。
**含义**: 模型在调用，但没引用 Sprint A–C 信号。
**最常见原因**:
1. 信号块在 prompt 里其实是 nil（看 fingerprint 是不是同样缺）；
2. 模型本身能力不足以处理结构化 JSON 信号（gemini-flash, gpt-3.5-tier）。
**处置**:
- (1) 先去 §5.2 / §5.4 修信号源；
- (2) 切到 critical tier：`UPDATE agents SET model_name='gemini-3.1-pro-preview' WHERE id=<pm_agent_id>`，下一轮决策窗口生效。

### 5.6 ATR stop 一直不响

**Symptom**: `plan_actions` 里从来没见过 `exit_reason='atr_stop'`，但持仓里有明显回撤的标的。
**先确认配置**:
```bash
docker exec fundai-postgres psql -U fundai -d fundai -c "
SELECT id, name, config->'exitPolicy'->'atrStop' AS atr_stop
  FROM funds WHERE status='active';
"
```
**处置**:
- 没有 `atrStop` 字段 → 加上：
  ```sql
  UPDATE funds
     SET config = jsonb_set(config, '{exitPolicy,atrStop}',
                            '{"multiplier":3.0,"anchorMode":"peak"}'::jsonb, true)
   WHERE id = '<fund_id>';
  ```
- 已配置但仍不响 → 大概率 quantsnapshot builder 没产出 ATR（标的 OHLC 历史太短或 ohlc fetcher 报错）。看 `decision_input_fingerprint.count_quant_snapshots` 是否 > 0；为 0 则 ATR-stop 全员 no-op，去查 ohlc provider 的 `ErrNoData` 日志。

---

## 6. 安全开关 (kill switch)

紧急情况下按以下顺序降级，每一步都是可独立操作的 SQL 一句话：

```sql
-- 1) 关掉某只基金的 deterministic exit（保留 LLM 决策）
UPDATE funds SET config = jsonb_set(config, '{exitPolicy,enabled}', 'false'::jsonb)
 WHERE id = '<fund_id>';

-- 2) 关掉 ATR stop 但保留其他 exit 规则
UPDATE funds SET config = config #- '{exitPolicy,atrStop}'
 WHERE id = '<fund_id>';

-- 3) 把某只基金切到全人工模式（PM 仍会出 plan, 但不会自动执行）
UPDATE funds SET trading_mode = 'manual' WHERE id = '<fund_id>';

-- 4) 全平台一键回退到 legacy provider（重启后生效）
-- (set in compose env, then restart)
LLM_PROVIDER=openai
```

---

## 7. 回归用 smoke test

### 7.1 决策管线 24 块 smoke (最高 leverage)

每次部署后 / 开盘第一笔 plan 落库后跑这一条，<1s 给出 24 块的 present/absent 表：

```bash
./scripts/smoke-decision.sh                  # 当前最后一笔决策
./scripts/smoke-decision.sh <fund_id>        # 指定基金最后一笔决策
./scripts/smoke-decision.sh --json | jq .    # 机读模式，可入 CI
```

返回 exit code:
- `0` = 全部 critical 块到位
- `1` = 至少一个 critical 块缺失 (instrumentHints / quantSnapshots) → 阻断决策
- `2` = 还没有任何决策被记录 (PM 没跑 / 日志窗口太短，加 `--tail 5000`)
- `3` = 容器没起 (`docker compose up -d` 没做)

> 24 块 = 21 (Sprint A→E) + 3 (Sprint F)。Sprint F 新增: `valueScores`
> (Fama-French HML 横截面), `lowBetaScores` (Frazzini-Pedersen 防御性 tilt),
> `pead` (Bernard-Thomas 盈利公告后漂移). 这三块缺失不阻断决策（PM 会回退到原有
> 的 21 块组合），但缺失意味着 Sprint F 链路有问题。

### 7.1b 一段时间窗内各 block 的"被引用率" (G1 #2)

`smoke-decision.sh` 看的是"最近一笔决策有没有 block"，但有时候我们想看
"过去 7 天 PM 实际上引用了哪些 block"。这才是"PM 是不是真的在用我们喂的信号"
的指标。

```bash
./scripts/block-attribution-report.sh                       # 最近 7 天
./scripts/block-attribution-report.sh --days 30             # 最近 30 天
./scripts/block-attribution-report.sh --days 14 --fund X    # 单个基金
./scripts/block-attribution-report.sh --json | jq .         # 机读模式
```

返回 exit code:
- `0` = 表渲染成功
- `1` = 窗口内没有带 attribution 的 plan（writer 还没部署 / fund 没在跑）
- `2` = postgres container 没起

颜色含义:
- **绿色**: 健康 — 喂进去的 block 被引用率 ≥ 20%
- **红色**: 喂了 block 但 PM 几乎不引用 (cited < 20% × present) — 说明 prompt
  教学规则没起作用，或者 LLM 在用它但没有显式 cite (后者可以接受)
- **黄色**: cited > present — PM 引用了 wiring 没喂的 block (prompt drift)

这报告依赖 migration `040_plan_block_contributions.sql` + writer
(`runtimePMAgent.persistBlockContributions`)。如果跑出来 exit=1 而你确认
PM 在跑，先检查 migration 是不是上了:

```sql
\d investment_plans
-- 应该看到 block_contributions jsonb NOT NULL DEFAULT '{}'
```

> **2026-05-24 hotfix 备忘**: 早期版本的 citation vocabulary 只识别英文，
> 而 LLM 在 reasoning 字段里写的几乎全是中文 (e.g. "动量排名 Q1"、"低Beta得分"、
> "辩论结论"、"宇宙排名 Q2"、"分歧票数=2"、"MACD" 等)。`attribution.go` 已经在
> 这次升级里加齐双语 vocabulary，rebuild 之后 cite rate 应该会从 ~0 直接抬到
> 健康区间。同期还把 `combineReasoning(reasoning, actions)` 接进 writer，把
> per-action 的 LLM 自然语言一起喂给 regex (`investment_plans.reasoning` 只是
> 摘要文本，几乎不会显式 cite 任何 block)。如果你看到 cite rate 远低于
> present rate，先看 `plan_actions.reasoning` 列里实际用了什么短语，必要时
> 往 `internal/decision/attribution.go::citationVocabulary` 里再补一行 alias。

### 7.2 Go 单元 / 集成测试 smoke

```bash
cd server && go test \
  ./internal/exitmanager/ \
  ./internal/correlation/ \
  ./internal/exposure/ \
  ./internal/decision/ \
  ./internal/earnings/ \
  ./internal/quality/ \
  ./internal/value/ \
  ./internal/lowbeta/ \
  ./internal/pead/ \
  ./internal/pairspread/ \
  ./internal/strategy/ \
  ./internal/llm/ \
  -count=1 -short
```

如果只想验证 Sprint D 全套：

```bash
cd server && go test ./cmd/server/ \
  -run 'TestFetchATRForPositions|TestResolve|TestDecodeFundMarket|TestObserveDecisionInput|TestMetricsExportIncludesDecisionInputSignals' \
  -count=1 -short
```

### 7.3 G1 #3 factorlab — per因子 IS Sharpe / DD / 命中率

这是 LLM-free 的纯规则回测，用来快速看 momentum / low_beta / low_vol 几个
策略的 IS 表现。默认跑合成 fixture (2 年模拟 OHLC + 5 个 profile 已经预置好)，
也可以接真实历史 CSV。

```bash
# 跑合成 fixture（确定性，CI 友好）
cd server && go run ./cmd/factorlab/

# 跑真实 fixture（你提供 CSV 目录，per-symbol 一个 CSV，
# 列要求 date,close 最少，open/high/low/volume 可选）
cd server && go run ./cmd/factorlab/ --fixture /path/to/csv-dir

# 调滑点 / 调起始 NAV / 只跑子集
cd server && go run ./cmd/factorlab/ --slippage 10 --nav 100000
cd server && go run ./cmd/factorlab/ --strategies momentum_12_1m,low_beta
```

输出 markdown 表，含每个策略的 `TotalRet / AnnRet / AnnVol / Sharpe / MaxDD /
HitRate`。表头加 `*` 是 cohort 内最高 Sharpe，加 `!` 是 Sharpe 比
equal_weight_long 还低 (说明这个因子在当前 fixture 上没产生 alpha 或被换手
吃掉了)。

合成 fixture 是用来跑通流程验证 framework 本身的，**不是用来下结论的**。
真实结论要喂真实历史 CSV — 推荐 SPY + 10-20 个大盘股 × 2 年。

---

## 8. 上线后的"看板顺序"建议

> 同事接手的话推荐按这个顺序"看 3 个东西就够了"

1. **`./scripts/smoke-decision.sh`** — 30 秒看最近一笔 PM 决策 24 块的健康表，含 `block_contributions.cited` 交叉验证 (优先) / Plan.Reasoning 关键字 fallback。
2. **Prometheus** — `docs/PROMETHEUS_QUERIES.md §6` 综合验收 dashboard 一页 6 张图：决策速率 + 24 块出现率 + exposure 触发 + risk-budget throttle + LLM 成功率 + workflow 失败率。
3. **`docker compose logs app | grep decision_input_fingerprint | tail -50`** — 眼力快速过一遍，看哪些 `p_*=false` 是预期外的。

如果没拉 Prometheus，直接 `curl http://localhost:8080/api/metrics | grep fundai_decision_` 也能拿到同样的数据，只是要手工算 ratio。

---

## 9. 相关文档

- `docs/PROMETHEUS_QUERIES.md` — PromQL 速查（10 条最常用查询 + 综合面板 PromQL）
- `scripts/smoke-decision.sh` — 24 块 smoke 健康检查（终端 / CI 友好，自带 JSON 输出）
- `scripts/block-attribution-report.sh` — 时间窗内 block 引用率报告（G1 #2）
- `scripts/fetch-yahoo-ohlc.sh` — Yahoo / NASDAQ 日 K CSV 拉取，给 factorlab 喂真实数据
- `cmd/factorlab/` — LLM-free 规则回测 CLI，输出 markdown 表
- `docs/SYSTEM_SPEC.md` — 系统全局架构说明
