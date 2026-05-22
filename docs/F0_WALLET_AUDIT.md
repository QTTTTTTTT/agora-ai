# F0 — Wallet Ledger Audit Report

> 审计日期: 2026-05-19 · Read-only audit, no code change · 输入: 现有 wallet/marketplace 代码与 migrations

---

## 总评

| 维度 | 评级 | 备注 |
|------|------|------|
| Schema 完整性 | **B+** | 双腿 ledger + idempotency + reconcile，无 hold/escrow |
| Repository 实现质量 | **A** | UoW + FOR UPDATE + 确定性锁序 + 类型化错误 |
| 服务/业务层使用 | **A** | marketplace buyout 是教科书级别 |
| 并发安全 | **A−** | 行锁 + idempotency 都到位；缺 bid 表的 idempotency |
| 测试覆盖 | **B** | wallet 走 marketplace_repo_test.go 间接覆盖；无独立 wallet_repo_test.go |
| **F5 (auction) 可行性** | **🟢 可以推进** | **不需要返工**，**需要附加** wallet_holds 表 + bid 增量 |

**结论：现状对 buyout/subscribe 是 production-ready；auction 需要做加法（新增 holds 表 + bid 表增量 + 拍卖 mode）而不是返工**。

---

## 1. Schema 现状

### 1.1 表清单

| 表 | 状态 | 说明 |
|----|------|------|
| `wallet_accounts` | ✅ | 每 user 1 行；`balance_minor`（无 frozen_minor） |
| `wallet_ledger_entries` | ✅ | 每笔运动 1 行；含 `idempotency_key`（migration 015） |
| `agent_market_listings` | ✅ | buyout + subscribe 双模式（migration 017） |
| `agent_market_bids` | ⚠️ | 表存在但**业务半成品**——只有 CreateBid/ListBids，没有 AcceptBid 路径；status 流转没人写 |
| `agent_market_orders` | ✅ | 含 `idempotency_key` + reconcile 字段（migration 015） |
| `agent_market_subscriptions` | ✅ | 周期、续费窗口完整 |
| `marketplace_reconcile_log` | ✅ | append-only 审计 |
| `wallet_holds` / `wallet_freezes` | ❌ | **不存在** |

### 1.2 关键观察

- `wallet_ledger_entries` 每次 transfer 写**两条**（debit + credit），共享 `reference_id`——**功能上**是 double-entry ledger，但 schema 层没有约束 `SUM=0`
- `idempotency_key` 是 nullable 但**唯一**（partial unique index）——好做法，retry 安全
- balance materialized 在 `wallet_accounts.balance_minor`，不是从 ledger 求和——读快但需要严格守护写路径（已在 `TransferWithTx` 做到）

---

## 2. Repository 层实现

### 2.1 `WalletRepo.TransferWithTx`（教科书级别）

`server/internal/repository/wallet_repo.go:239-342`

正确实现的部分：

| 模式 | 现状 | 评价 |
|------|------|------|
| 强制传入 *sql.Tx | ✅ `tx == nil` 返回 `ErrNoTx` | 防止业务方裸调引起跨事务不一致 |
| 金额校验 | ✅ `AmountMinor <= 0` 拒绝 | |
| 同用户校验 | ✅ `from == to` 拒绝 | |
| **确定性锁顺序** | ✅ 按 sorted userID 锁，避免死锁 | 这是 hedge fund 级别的工程细节 |
| `SELECT ... FOR UPDATE` | ✅ 两个账户都行锁 | 防并发超扣 |
| `ErrInsufficientBalance` 类型 | ✅ 调用方能精确分支 | |
| Idempotency | ✅ debit + credit 各自一个 key | 不会因为重试漏掉一条 |
| 唯一冲突 → 类型化 | ✅ SQLSTATE 23505 → `ErrIdempotencyConflict` | 调用方可短路 |

### 2.2 `UnitOfWork.WithinTx`

`server/internal/repository/uow.go:46-84`

- ✅ Panic-safe（rollback 后 re-panic）
- ✅ 默认 rollback（commit 成功才标 `committed`）
- ✅ 鼓励调用方用 `*_WithTx` 变体串联多 repo

### 2.3 业务方使用（marketplace buyout）

`server/cmd/server/marketplace_adapter.go:217-298`

一笔买入 = 一个 `WithinTx` 内做完：

1. `LockListingForUpdate`（serialize concurrent buyers）
2. `CreatePendingOrderWithTx`（含 idempotency_key）
3. **Replay 检测**：若 order 已 `completed` → 短路返回（用户已付）
4. `cloneMarketplaceAgentTx`（agent clone 同 tx）
5. `AddEdgeWithTx`（lineage 同 tx）
6. `TransferWithTx`（钱同 tx）
7. `CompleteOrderWithTx` + `MarkListingSoldWithTx`

→ 任何一步失败，全部回滚。**包括 agent clone**——这意味着不会出现"agent 已 clone 但钱没扣"或反之的悬挂态。

---

## 3. 缺失项（对 F5 影响）

### 3.1 没有 freeze / hold 机制 ⚠️

**搜索结果**: `frozen|hold|escrow|wallet_holds` 在 wallet 代码 0 匹配。

**当前能力**:
- 转账 = 同一事务内"扣 from + 加 to"，原子
- 充值 = "加 balance + 写 ledger"，原子
- **没有"扣到中间态等待结果"**

**对 auction 的影响**:
- 用户出价时需要冻结钱（防赖账），现在没有"冻结余额"概念
- 多个用户连续出价时，前一个 bidder 的钱需要解冻、新 bidder 的钱需要冻结——纯转账模型表达不了

### 3.2 `agent_market_bids` 表是半成品 ⚠️

- 表存在、`CreateBid`/`ListBidsByListing` 存在
- **但**：没有 `AcceptBid` / `WithdrawBid` 实现；status (`pending`/`accepted`/`rejected`/`retracted`) 没人写入流转
- 没有 `idempotency_key` 字段
- 没有 `ends_at` / `reserve_price` / `anti_snipe_window` 列
- 当前定位：被动"求购意向板"，不是 auction

### 3.3 其他次要缺失

- `wallet_accounts` 没有 `frozen_minor` 字段（可加可不加；若走独立 holds 表则不需要）
- 没有独立的 `wallet_repo_test.go`（测试在 `marketplace_repo_test.go` 里间接覆盖）

---

## 4. F5 (Auction) 实施建议

### 4.1 推荐方案：新增 `wallet_holds` 表（Option A）

```sql
CREATE TABLE wallet_holds (
    id UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    account_id UUID NOT NULL REFERENCES wallet_accounts(id) ON DELETE CASCADE,
    amount_minor BIGINT NOT NULL CHECK (amount_minor > 0),
    currency VARCHAR(8) NOT NULL DEFAULT 'USD',
    reference_type VARCHAR(40) NOT NULL,  -- e.g. 'auction_bid'
    reference_id TEXT NOT NULL,            -- e.g. bid id
    status VARCHAR(20) NOT NULL DEFAULT 'active'
        CHECK (status IN ('active', 'released', 'consumed')),
    idempotency_key TEXT,
    expires_at TIMESTAMPTZ,                -- optional auto-expiry safety net
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    released_at TIMESTAMPTZ,
    consumed_at TIMESTAMPTZ
);
CREATE INDEX idx_wallet_holds_account_active ON wallet_holds (account_id, status);
CREATE UNIQUE INDEX idx_wallet_holds_idempotency_key ON wallet_holds (idempotency_key) WHERE idempotency_key IS NOT NULL;
```

**配套 repo 操作**：
- `PlaceHoldWithTx(account_id, amount, ref, idem)` — 扣 balance + 插 hold（active）
- `ReleaseHoldWithTx(hold_id, idem)` — hold 标 released + 加回 balance
- `ConsumeHoldWithTx(hold_id, to_user_id, idem)` — hold 标 consumed + ledger 写 transfer 到 to_user

**可用余额公式**: `wallet_accounts.balance_minor`（**already excludes active holds**——因为 PlaceHold 已经扣了 balance）。这是关键设计：保持 balance 字段是"可用余额"，holds 只用于"知道这些钱还能 release 回来"。

**优势**:
- 复用现有 `TransferWithTx` 的 idempotency + 锁序 pattern
- 不破坏现有 buyout/subscribe 流程
- `expires_at` 提供"忘记 release 的安全网"——cron 兜底解冻

### 4.2 Bid 表增量

```sql
ALTER TABLE agent_market_bids
    ADD COLUMN IF NOT EXISTS hold_id UUID REFERENCES wallet_holds(id) ON DELETE SET NULL,
    ADD COLUMN IF NOT EXISTS idempotency_key TEXT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_agent_market_bids_idempotency_key
    ON agent_market_bids (idempotency_key) WHERE idempotency_key IS NOT NULL;
```

每个 active bid → 关联一个 active hold；新 highest bid 进来时，旧 bid 的 hold release。

### 4.3 Auction 元数据：在 `agent_market_listings` 上扩展

```sql
ALTER TABLE agent_market_listings
    ADD COLUMN IF NOT EXISTS reserve_price_minor BIGINT
        CHECK (reserve_price_minor IS NULL OR reserve_price_minor > 0),
    ADD COLUMN IF NOT EXISTS auction_ends_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS auction_anti_snipe_seconds INT,
    ADD COLUMN IF NOT EXISTS auction_min_increment_minor BIGINT;

ALTER TABLE agent_market_listings
    DROP CONSTRAINT IF EXISTS agent_market_listings_mode_check;
ALTER TABLE agent_market_listings
    ADD CONSTRAINT agent_market_listings_mode_check
    CHECK (mode IN ('buyout', 'subscribe', 'auction'));
```

### 4.4 估算

| 项 | 工时（小时） |
|----|------------|
| migration 024 (wallet_holds) | 1 |
| migration 025 (bids 增量 + listings auction 列) | 0.5 |
| `WalletRepo` Place/Release/Consume Hold + tests | 3 |
| `MarketplaceRepo` auction CRUD + bid status flow | 3 |
| Auction service (anti-snipe + settlement cron) | 4 |
| HTTP handler + auth + rate limit | 2 |
| Frontend Marketplace 拍卖页 | 4 |
| 端到端集成测试 | 2 |
| Docs (.env / README / SYSTEM_SPEC 更新) | 1 |
| **合计** | **~20.5h** |

---

## 5. 风险提示

| 风险 | 严重度 | 缓解 |
|------|--------|------|
| 卖家 / bidder 在同一 KYC 下自买自卖 | High | 在 PlaceBid 校验 `bidder.kyc_subject != seller.kyc_subject` |
| 拍卖结束 cron 漏跑或重复跑 | Medium | 已有 `scheduler/lease.go`（migration 016 leases 表），拍卖结算 cron 必须用 lease 防多实例 |
| 长时间未结算的 hold 累积 | Medium | `expires_at` + 兜底 cron 清理 |
| 出价 → 网络断开 → 用户重试 → 两个 hold | High | 用 `idempotency_key` (clientReqId) 短路 |
| 反狙击窗口被恶意利用拖延拍卖 | Low | 总延长封顶（e.g., 总延长不能超过初始 duration 的 50%）|
| 流拍后 hold 没被释放 | Critical | 设计上必须在同一事务里：标 listing `expired` + release 所有 active holds |

---

## 6. 对 plan 的影响

**结论：F5 可以直接开，不需要前置返工 phase。**

调整后的 plan 顺序（F5 内部任务序）：

```
F5.0  Schema 加 wallet_holds + bids 增量 + auction 列  ← 新增
F5.1  WalletRepo Hold APIs (Place/Release/Consume) + tests
F5.2  MarketplaceRepo auction-mode CRUD + Bid status flow
F5.3  Auction service: PlaceBid (含 hold 替换) + 反狙击
F5.4  结算 cron (用现有 scheduler/lease)
F5.5  HTTP handler + RBAC + rate limit
F5.6  Frontend 拍卖 UI
F5.7  E2E + docs
```

补充 F0 顺手做的两个低成本改进（建议作为 F5.0 的一部分）：

- **F5.0a**: 加 `wallet_repo_test.go` 单独单测文件（现状只在 marketplace 测试里间接覆盖；auction 引入新 Hold APIs 之后必须独立测）
- **F5.0b**: 给 `agent_market_bids` 加 `idempotency_key`

---

## 7. 总结一句话

**钱袋子的"工程质量"超出预期，写得是 hedge-fund-grade 的代码；唯一缺一个 hold 概念，加上去就能直接支撑 auction，不需要把现有 buyout/subscribe 推倒。可以放心开 F1。**
