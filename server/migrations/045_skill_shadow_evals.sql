-- Sprint 3 / M5: skill shadow evaluation history.
--
-- 每次 admin 在 inbox 里手动触发或 cron 自动触发的 shadow eval 都落一行。
-- (fund_id, skill_key, evaluated_at) 形成天然时间索引；最近 3 次都过门槛
-- 触发 auto-approve（见 cmd/server/skill_inbox.go）。
--
-- 我们没有 FK 到 agents 或 funds 上 —— skill_key 是 reflection-id 形态、
-- 跨 agent 一致，single FK 反而会让 fund 删除时把整段评估历史 cascade
-- 掉，影响审计回溯。如果将来需要严格关联，新增独立的 audit 视图。

CREATE TABLE IF NOT EXISTS skill_shadow_evals (
    id BIGSERIAL PRIMARY KEY,
    fund_id UUID NOT NULL,
    skill_key TEXT NOT NULL,
    strategy TEXT NOT NULL,
    sharpe NUMERIC(8,4) NOT NULL,
    hit_rate_pct NUMERIC(6,2) NOT NULL,
    evaluated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_skill_shadow_fund_skill
    ON skill_shadow_evals(fund_id, skill_key, evaluated_at DESC);
