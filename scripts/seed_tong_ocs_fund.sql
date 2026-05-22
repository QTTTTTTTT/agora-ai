-- Seed script for user `tong` (super_admin): create an OCS-themed A-share fund.
--
-- Scope:
--   1. New company: "OCS 资本"
--   2. New fund:    "OCS 主题精选 1 号" (A-share / SSE / CNY / simulation / 1,000,000 RMB)
--      universe = {688205 德科立, 688195 腾景科技}
--   3. New team of 4 agents bound to the fund:
--        - 1 Portfolio Manager  (role=pm)
--        - 1 Researcher · 德科立 688205   (role=researcher, focus=stock)
--        - 1 Researcher · 腾景科技 688195 (role=researcher, focus=stock)
--        - 1 Trader              (role=trader)
--
-- Idempotent: re-running is a no-op because the inner block aborts if the
-- company name already exists for this user.
--
-- Usage:
--   docker exec -i fundai-postgres psql -U fundai -d fundai \
--     < scripts/seed_tong_ocs_fund.sql

\set ON_ERROR_STOP on

BEGIN;

DO $seed$
DECLARE
    v_user_id        uuid := '9c325e54-3b21-43b3-ab71-d26dcd343ea7';  -- tong
    v_company_name   text := 'OCS 资本';
    v_fund_name      text := 'OCS 主题精选 1 号';
    v_company_id     uuid;
    v_fund_id        uuid;
    v_pm_id          uuid;
    v_research_dkl   uuid;
    v_research_tjkj  uuid;
    v_trader_id      uuid;
    v_fund_config    jsonb;
BEGIN
    -- Guardrail: refuse to run twice for the same (user, company name).
    IF EXISTS (
        SELECT 1 FROM fund_companies
        WHERE owner_user_id = v_user_id AND name = v_company_name
    ) THEN
        RAISE EXCEPTION 'Company % already exists for user tong; aborting to keep this script idempotent.', v_company_name;
    END IF;

    -- 1. Company ----------------------------------------------------------
    INSERT INTO fund_companies (owner_user_id, name, description)
    VALUES (
        v_user_id,
        v_company_name,
        'A 股 OCS（光交换 / Optical Circuit Switching）主题资产管理 — 聚焦数据中心互联、硅光与光通信器件产业链。'
    )
    RETURNING id INTO v_company_id;

    -- 2. Fund -------------------------------------------------------------
    v_fund_config := jsonb_build_object(
        'market',            'a_share',
        'exchange',          'SSE',
        'assetClass',        'equity',
        'baseCurrency',      'CNY',
        'calendarCode',      'CN-SSE',
        'timeZone',          'Asia/Shanghai',
        'benchmarkSymbol',   '000688',           -- 科创50
        'primaryDirection',  'stocks',
        'universe', jsonb_build_object(
            'mode',    'manual',
            'symbols', jsonb_build_array('688205', '688195'),
            'themes',  jsonb_build_array('OCS', '光交换', '光通信', '硅光', 'AI 数据中心互联'),
            'sectors', jsonb_build_array('光器件', '光通信', '半导体'),
            'customFilters', jsonb_build_array()
        ),
        'specialization', jsonb_build_object(
            'team', jsonb_build_object(
                'markets',       jsonb_build_array('a_share'),
                'assetClasses',  jsonb_build_array('equity'),
                'themes',        jsonb_build_array('OCS', '光交换', '硅光', 'AI 数据中心互联'),
                'instruments',   jsonb_build_array('688205', '688195'),
                'styleHints',    jsonb_build_array('科创板成长', '光通信周期', '数据中心 AI 资本开支驱动')
            )
        )
    );

    INSERT INTO funds (
        company_id, name, description,
        trading_mode, initial_capital, current_capital, total_assets, nav,
        status, config
    )
    VALUES (
        v_company_id,
        v_fund_name,
        'A 股 OCS 主题模拟盘 1 号 — 标的：德科立 (688205)、腾景科技 (688195)。',
        'simulation', 1000000, 1000000, 1000000, 1.0,
        'active', v_fund_config
    )
    RETURNING id INTO v_fund_id;

    -- 3. Agents -----------------------------------------------------------
    -- 3a. Portfolio Manager
    INSERT INTO agents (
        user_id, name, role, focus,
        llm_model, model_provider, model_name,
        system_prompt,
        skill_config, domain_config, evolution_config,
        pending_marketplace_snapshot, status
    )
    VALUES (
        v_user_id, 'Portfolio Manager', 'pm', NULL,
        'deepseek-chat', 'deepseek', 'deepseek-chat',
        '你是 OCS 主题精选 1 号基金的组合经理。覆盖范围：A 股科创板 OCS / 光交换 / 硅光 / 数据中心互联产业链。当前组合标的：688205 德科立、688195 腾景科技。请基于研究员的个股研报与交易员的执行回报，综合做出仓位与权重决策；严格遵守基金风控（单笔下单不超过总资产的硬上限）。',
        '{}'::jsonb,
        jsonb_build_object(
            'coverage', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'directions',   jsonb_build_array('stocks')
            ),
            'specialization', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'themes',       jsonb_build_array('OCS', '光交换', '硅光', 'AI 数据中心互联'),
                'instruments',  jsonb_build_array('688205', '688195'),
                'styleHints',   jsonb_build_array('科创板成长', '光通信周期')
            )
        ),
        '{}'::jsonb,
        '{}'::jsonb,
        'active'
    )
    RETURNING id INTO v_pm_id;

    -- 3b. Researcher · 德科立 (688205)
    INSERT INTO agents (
        user_id, name, role, focus,
        llm_model, model_provider, model_name,
        system_prompt,
        skill_config, domain_config, evolution_config,
        pending_marketplace_snapshot, status
    )
    VALUES (
        v_user_id, 'Research Agent · STOCK', 'researcher', 'stock',
        'deepseek-chat', 'deepseek', 'deepseek-chat',
        '你是 OCS 主题精选 1 号基金中负责【688205 德科立】的股票研究员。覆盖范围仅限德科立 (688205.SH)：光通信传输设备、相干光模块、OTN/OCS 上游模块。请基于公司公告、东方财富 / 新浪等 A 股新闻、行业景气度（CPO、800G、AI 集群互联），输出方向（看多/中性/看空）、置信度、关键催化与风险。',
        '{}'::jsonb,
        jsonb_build_object(
            'coverage', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'directions',   jsonb_build_array('stocks')
            ),
            'specialization', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'themes',       jsonb_build_array('OCS', '光交换', '光通信', '相干光模块', 'OTN'),
                'instruments',  jsonb_build_array('688205'),
                'styleHints',   jsonb_build_array('科创板成长', '光通信周期'),
                'patterns',     jsonb_build_array('CPO 渗透率', '800G 出货拐点', 'AI 集群互联资本开支')
            )
        ),
        '{}'::jsonb,
        '{}'::jsonb,
        'active'
    )
    RETURNING id INTO v_research_dkl;

    -- 3c. Researcher · 腾景科技 (688195)
    INSERT INTO agents (
        user_id, name, role, focus,
        llm_model, model_provider, model_name,
        system_prompt,
        skill_config, domain_config, evolution_config,
        pending_marketplace_snapshot, status
    )
    VALUES (
        v_user_id, 'Research Agent · STOCK', 'researcher', 'stock',
        'deepseek-chat', 'deepseek', 'deepseek-chat',
        '你是 OCS 主题精选 1 号基金中负责【688195 腾景科技】的股票研究员。覆盖范围仅限腾景科技 (688195.SH)：精密光学元件、硅光器件、光通信 / 激光 / 量子精密光学。请基于公司公告、东方财富 / 新浪等 A 股新闻、行业景气度（硅光、800G、量子）, 输出方向（看多/中性/看空）、置信度、关键催化与风险。',
        '{}'::jsonb,
        jsonb_build_object(
            'coverage', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'directions',   jsonb_build_array('stocks')
            ),
            'specialization', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'themes',       jsonb_build_array('OCS', '光交换', '硅光', '精密光学', '量子'),
                'instruments',  jsonb_build_array('688195'),
                'styleHints',   jsonb_build_array('科创板成长', '硅光升级周期'),
                'patterns',     jsonb_build_array('硅光渗透率', '量子精密光学订单', '激光设备订单')
            )
        ),
        '{}'::jsonb,
        '{}'::jsonb,
        'active'
    )
    RETURNING id INTO v_research_tjkj;

    -- 3d. Trader
    INSERT INTO agents (
        user_id, name, role, focus,
        llm_model, model_provider, model_name,
        system_prompt,
        skill_config, domain_config, evolution_config,
        pending_marketplace_snapshot, status
    )
    VALUES (
        v_user_id, 'Trader Agent', 'trader', NULL,
        'deepseek-chat', 'deepseek', 'deepseek-chat',
        '你是 OCS 主题精选 1 号基金的交易员。负责执行 PM 下达的 688205 / 688195 订单，按 A 股交易规则（9:30-11:30、13:00-15:00；T+1；最小单位 100 股）输出执行计划。若实时报价已过期或与计划价偏离 ≥0.5%，请刷新报价后再决定是否执行；触发硬风控时请立即拒单并报告原因。',
        '{}'::jsonb,
        jsonb_build_object(
            'coverage', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'directions',   jsonb_build_array('stocks')
            ),
            'specialization', jsonb_build_object(
                'markets',      jsonb_build_array('a_share'),
                'assetClasses', jsonb_build_array('equity'),
                'themes',       jsonb_build_array('OCS', '光交换'),
                'instruments',  jsonb_build_array('688205', '688195'),
                'styleHints',   jsonb_build_array('A股 T+1', '科创板涨跌停 20%')
            )
        ),
        '{}'::jsonb,
        '{}'::jsonb,
        'active'
    )
    RETURNING id INTO v_trader_id;

    -- 4. Bind agents into fund team --------------------------------------
    INSERT INTO fund_team_members (fund_id, agent_id, role, focus, status)
    VALUES
        (v_fund_id, v_pm_id,         'pm',         NULL,    'active'),
        (v_fund_id, v_research_dkl,  'researcher', 'stock', 'active'),
        (v_fund_id, v_research_tjkj, 'researcher', 'stock', 'active'),
        (v_fund_id, v_trader_id,     'trader',     NULL,    'active');

    -- Surface a friendly summary in the psql output.
    RAISE NOTICE 'Created company % (%) with fund % (%) and 4 agents.',
        v_company_name, v_company_id, v_fund_name, v_fund_id;
    RAISE NOTICE '  pm            = %', v_pm_id;
    RAISE NOTICE '  researcher德科立 = %', v_research_dkl;
    RAISE NOTICE '  researcher腾景科技 = %', v_research_tjkj;
    RAISE NOTICE '  trader        = %', v_trader_id;
END
$seed$;

COMMIT;

-- Verification queries -----------------------------------------------------
\echo ''
\echo '== Company =='
SELECT id, name, owner_user_id, created_at
FROM   fund_companies
WHERE  name = 'OCS 资本';

\echo ''
\echo '== Fund =='
SELECT id, name, trading_mode, initial_capital, status,
       config->>'market'  AS market,
       config->'universe'->'symbols' AS universe_symbols
FROM   funds
WHERE  name = 'OCS 主题精选 1 号';

\echo ''
\echo '== Team =='
SELECT ftm.role,
       ftm.focus,
       a.name,
       a.domain_config->'specialization'->'instruments' AS instruments,
       a.llm_model
FROM   fund_team_members ftm
JOIN   agents a ON a.id = ftm.agent_id
JOIN   funds  f ON f.id = ftm.fund_id
WHERE  f.name = 'OCS 主题精选 1 号'
ORDER  BY CASE ftm.role
            WHEN 'pm'         THEN 1
            WHEN 'researcher' THEN 2
            WHEN 'trader'     THEN 3
            WHEN 'risk'       THEN 4
            ELSE 5
          END,
          a.created_at;
