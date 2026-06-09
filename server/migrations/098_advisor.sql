-- Migration: 098_advisor
-- Description:
--   Tables that power the /advisor mode — a stock-consultation surface
--   where users feed in a single symbol, the platform fans out the
--   query to N "master investor" agents (Buffett / Munger / Graham /
--   Lynch / Marks / Dalio / O'Neil / Greenblatt / Wood / Druckenmiller)
--   or N "A-share short-term tactic" agents (尾盘狙击/首板低吸/龙头打板/
--   缩量回踩), and returns one verdict per agent plus an aggregate.
--
--   This surface is intentionally isolated from the fund/team system:
--   no Plan, no Trade, no NAV. /advisor consultations live in their
--   own tables, are owned per user, and share zero foreign keys with
--   funds.id. The only thing they share with the fund world is the
--   agent_reputation_outcomes ledger (Phase 5 backfill writes
--   master:* / tactic:* prefixed agent_ids into the existing table).
--
--   Four tables:
--     advisor_persona_presets   admin-curated "consult style → which
--                               masters / tactics vote" routing
--     advisor_consultations     parent row, one per /consult call
--     advisor_master_reports    one per master agent inside a consultation
--                               (verdict + intrinsic value + margin of
--                               safety + master-specific JSONB)
--     advisor_tactic_reports    one per A-share tactic agent (entry /
--                               stop / target / holding window — schema
--                               is materially different from masters)
--
--   Also seeds the `advisor_mode` feature flag so the surface can be
--   killed via admin console without a code deploy.

-- ---------------------------------------------------------------------------
-- 1. Persona preset routing
-- ---------------------------------------------------------------------------
--
-- preset_key is the wire-facing identifier ("conservative" / "garp" /
-- "deep_value" / "disruptive" / "macro" / "quant" / "cn_short" /
-- "custom"); admins can add new ones without a code change.
--
-- master_keys / tactic_keys are TEXT[] of agent identifiers
-- (see server/internal/agent/masters/ and cn_tactics/ JSON filenames).
-- A preset can carry only masters, only tactics, or both — the
-- service router fans out accordingly.
CREATE TABLE IF NOT EXISTS advisor_persona_presets (
    preset_key      VARCHAR(48) PRIMARY KEY,
    label_zh        TEXT NOT NULL,
    label_en        TEXT NOT NULL,
    description_zh  TEXT NOT NULL DEFAULT '',
    description_en  TEXT NOT NULL DEFAULT '',
    master_keys     TEXT[] NOT NULL DEFAULT '{}',
    tactic_keys     TEXT[] NOT NULL DEFAULT '{}',
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    sort_order      INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_advisor_presets_enabled
    ON advisor_persona_presets (enabled, sort_order);

-- Seed 6 production-ready presets + a `custom` placeholder. Phase 5
-- populates master_keys / tactic_keys; in Phase 0 we just register
-- the rows so the front-end picker has something to render.
INSERT INTO advisor_persona_presets
    (preset_key, label_zh, label_en, description_zh, description_en,
     master_keys, tactic_keys, sort_order)
VALUES
    ('conservative', '保守稳健', 'Conservative value',
     '巴菲特 + 芒格 + 格雷厄姆 联合投票，长期价值 + 安全边际，最稳健。',
     'Buffett + Munger + Graham vote. Long-term value with margin of safety.',
     ARRAY['buffett','munger','graham']::TEXT[], '{}'::TEXT[], 10),
    ('garp', 'GARP 成长',
     'Growth at a reasonable price',
     '林奇 + 欧奈尔 联合投票，PEG < 1 的合理价格成长股。',
     'Lynch + O''Neil vote. Growth at a reasonable PEG.',
     ARRAY['lynch','oneil']::TEXT[], '{}'::TEXT[], 20),
    ('deep_value', '深度价值',
     'Deep value',
     '格雷厄姆 + 格林布拉特 联合投票，看资产折扣 + Magic Formula 排名。',
     'Graham + Greenblatt vote. Asset discount + Magic Formula ranking.',
     ARRAY['graham','greenblatt']::TEXT[], '{}'::TEXT[], 30),
    ('disruptive', '颠覆创新',
     'Disruptive innovation',
     'Cathie Wood 主导，关注 AI / 自动驾驶 / 基因 / 能源 / 区块链 5 大颠覆赛道。',
     'Cathie Wood lead. AI / robotics / genomics / energy / blockchain.',
     ARRAY['wood']::TEXT[], '{}'::TEXT[], 40),
    ('macro', '宏观择时',
     'Macro timing',
     '马克斯 + 达里奥 + 德鲁肯米勒 联合投票，关注周期位置 + 央行流动性。',
     'Marks + Dalio + Druckenmiller vote. Cycle + central bank liquidity.',
     ARRAY['marks','dalio','druckenmiller']::TEXT[], '{}'::TEXT[], 50),
    ('quant', '量化系统',
     'Quantitative system',
     'Joel Greenblatt Magic Formula 量化打分，机械系统化决策。',
     'Greenblatt Magic Formula. Systematic quantitative scoring.',
     ARRAY['greenblatt']::TEXT[], '{}'::TEXT[], 60),
    ('cn_short', 'A 股短线',
     'A-share short-term',
     '尾盘狙击 + 首板低吸 + 龙头打板 + 缩量回踩 四战法联合，T+1 ~ T+10 持有。',
     'Tail sniper + dip buyer + dragon-head + pullback. T+1 to T+10 holds.',
     '{}'::TEXT[],
     ARRAY['tail_sniper','first_limit_dip','dragon_head','shrink_pullback']::TEXT[],
     70),
    ('custom', '自定义',
     'Custom',
     '自由选择参与投票的大师 / 战法。',
     'Pick which masters and tactics participate in the vote.',
     '{}'::TEXT[], '{}'::TEXT[], 99)
ON CONFLICT (preset_key) DO NOTHING;

-- ---------------------------------------------------------------------------
-- 2. Consultation parent row
-- ---------------------------------------------------------------------------
--
-- One row per /api/advisor/consult call. We deliberately store the
-- aggregate verdict + confidence here (rather than recomputing from
-- the child rows on every read) so the history list can render
-- "Buffett+Munger+Graham → BUY (84%)" without an extra join.
--
-- consensus_score = stddev-style measure of agreement across the
-- agents that voted. Stored separately so the UI can flag
-- "consensus low, treat with caution" without re-aggregating.
CREATE TABLE IF NOT EXISTS advisor_consultations (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    symbol              TEXT NOT NULL,
    market              TEXT NOT NULL DEFAULT '',
    asset_class         TEXT NOT NULL DEFAULT '',
    preset_key          VARCHAR(48) NOT NULL
                            REFERENCES advisor_persona_presets(preset_key),
    aggregate_verdict   TEXT NOT NULL
                            CHECK (aggregate_verdict IN (
                                'STRONG_BUY','BUY','HOLD','AVOID','SHORT','MIXED','SKIP'
                            )),
    aggregate_confidence INT NOT NULL DEFAULT 0
                            CHECK (aggregate_confidence BETWEEN 0 AND 100),
    consensus_score     NUMERIC(5,2) NOT NULL DEFAULT 0,
    notes               TEXT NOT NULL DEFAULT '',
    price_at_consult    NUMERIC(18,6),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_advisor_consultations_user_created
    ON advisor_consultations (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_advisor_consultations_symbol
    ON advisor_consultations (symbol, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_advisor_consultations_preset
    ON advisor_consultations (preset_key, created_at DESC);

-- ---------------------------------------------------------------------------
-- 3. Master report (one per master agent inside a consultation)
-- ---------------------------------------------------------------------------
--
-- master_specific JSONB carries the per-master extras the
-- standardised columns don't cover: Buffett's intrinsic_value /
-- margin_of_safety / moat_score, Lynch's PEG / category /
-- tenbagger_potential, Graham's graham_number / passes_defensive /
-- is_net_net, etc. The Go service knows which keys to expect per
-- master_key; the DB stays agnostic so adding a new master =
-- adding a JSON file + a row, no DDL.
CREATE TABLE IF NOT EXISTS advisor_master_reports (
    id                  UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id     UUID NOT NULL
                            REFERENCES advisor_consultations(id) ON DELETE CASCADE,
    master_key          VARCHAR(48) NOT NULL,
    master_name_zh      TEXT NOT NULL DEFAULT '',
    master_name_en      TEXT NOT NULL DEFAULT '',
    verdict             TEXT NOT NULL
                            CHECK (verdict IN (
                                'STRONG_BUY','BUY','HOLD','AVOID','SHORT','PASS','SKIP'
                            )),
    confidence          INT NOT NULL DEFAULT 0
                            CHECK (confidence BETWEEN 0 AND 100),
    thesis              TEXT NOT NULL DEFAULT '',
    key_reasons         JSONB NOT NULL DEFAULT '[]'::jsonb,
    key_risks           JSONB NOT NULL DEFAULT '[]'::jsonb,
    master_specific     JSONB NOT NULL DEFAULT '{}'::jsonb,
    red_lines_hit       JSONB NOT NULL DEFAULT '[]'::jsonb,
    llm_model           TEXT NOT NULL DEFAULT '',
    prompt_tokens       INT NOT NULL DEFAULT 0,
    completion_tokens   INT NOT NULL DEFAULT 0,
    generated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (consultation_id, master_key)
);

CREATE INDEX IF NOT EXISTS idx_advisor_master_reports_master
    ON advisor_master_reports (master_key, generated_at DESC);

-- ---------------------------------------------------------------------------
-- 4. Tactic report (A-share short-term, distinct shape from masters)
-- ---------------------------------------------------------------------------
--
-- Tactic agents output trade-execution-shaped data (entry range /
-- stop loss / target price / holding window) rather than the
-- direction/confidence shape masters use. We keep them in a
-- separate table to avoid polluting advisor_master_reports with
-- mostly-NULL price columns.
--
-- red_lines_hit captures which hard-no rules from the tactic's
-- JSON template were triggered; market_regime_pass records whether
-- the global filter (e.g. "全市场炸板率 > 50% 时禁用此战法") allowed
-- the agent to run at all.
CREATE TABLE IF NOT EXISTS advisor_tactic_reports (
    id                      UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    consultation_id         UUID NOT NULL
                                REFERENCES advisor_consultations(id) ON DELETE CASCADE,
    tactic_key              VARCHAR(48) NOT NULL,
    tactic_name_zh          TEXT NOT NULL DEFAULT '',
    tactic_name_en          TEXT NOT NULL DEFAULT '',
    verdict                 TEXT NOT NULL
                                CHECK (verdict IN (
                                    'BUY_TAIL','BUY_DIP','CHASE_LIMIT_UP','BUY_PULLBACK',
                                    'WAIT_FOR_CONFIRMATION','WAIT_FOR_WINDOW','SKIP'
                                )),
    confidence              INT NOT NULL DEFAULT 0
                                CHECK (confidence BETWEEN 0 AND 100),
    thesis                  TEXT NOT NULL DEFAULT '',
    entry_price_low         NUMERIC(18,4),
    entry_price_high        NUMERIC(18,4),
    stop_loss_price         NUMERIC(18,4),
    target_t1               NUMERIC(18,4),
    target_t3               NUMERIC(18,4),
    expected_holding_days   INT,
    score                   NUMERIC(5,2) NOT NULL DEFAULT 0,
    key_reasons             JSONB NOT NULL DEFAULT '[]'::jsonb,
    key_risks               JSONB NOT NULL DEFAULT '[]'::jsonb,
    red_lines_hit           JSONB NOT NULL DEFAULT '[]'::jsonb,
    market_regime_pass      BOOLEAN NOT NULL DEFAULT TRUE,
    market_regime_reason    TEXT NOT NULL DEFAULT '',
    generated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    UNIQUE (consultation_id, tactic_key)
);

CREATE INDEX IF NOT EXISTS idx_advisor_tactic_reports_tactic
    ON advisor_tactic_reports (tactic_key, generated_at DESC);

-- ---------------------------------------------------------------------------
-- 5. Feature flag seed: `advisor_mode`
-- ---------------------------------------------------------------------------
--
-- Registered in the same migration as the tables so the surface
-- is gateable from day one. enforce_server_gate=TRUE means handlers
-- under /api/advisor/* must 503 when admin flips this off (matches
-- the ab_test_compare pattern from migration 097).
--
-- Routes affected: see gatedAPIPathPatterns in feature_flags.go,
-- which is extended in the same Phase 0 commit.
INSERT INTO feature_flags
    (flag_key, label, description, enabled, affects_routes, enforce_server_gate)
VALUES
    (
        'advisor_mode',
        '大师团队咨询',
        '/advisor 大师团队 + A 股短线战法咨询模式。关闭后用户看不到入口，且后端 /api/advisor/* 全部返回 503。',
        TRUE,
        ARRAY['/welcome', '/advisor'],
        TRUE
    )
ON CONFLICT (flag_key) DO NOTHING;
