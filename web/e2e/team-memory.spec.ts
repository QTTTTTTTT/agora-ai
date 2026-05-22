import { execFileSync } from "node:child_process";
import { expect, request as playwrightRequest, test } from "@playwright/test";
import { createE2EName, ensureSeedData, login, setAppPreferences } from "./helpers";

const POSTGRES_CONTAINER = process.env.PLAYWRIGHT_POSTGRES_CONTAINER || "fundai-postgres";
const POSTGRES_USER = process.env.PLAYWRIGHT_POSTGRES_USER || process.env.POSTGRES_USER || "fundai";
const POSTGRES_DB = process.env.PLAYWRIGHT_POSTGRES_DB || process.env.POSTGRES_DB || "fundai";
const POSTGRES_PASSWORD = process.env.PLAYWRIGHT_POSTGRES_PASSWORD || process.env.POSTGRES_PASSWORD || "fundai_secret_change_me";

interface TeamAgent {
  id: string;
  role: string;
  skillConfig?: unknown;
  domainConfig?: {
    coverage?: {
      markets?: string[];
      assetClasses?: string[];
      directions?: string[];
    };
    specialization?: {
      markets?: string[];
      assetClasses?: string[];
      themes?: string[];
      instruments?: string[];
      styleHints?: string[];
      patterns?: string[];
    };
    [key: string]: unknown;
  };
  latestLearningSummary?: string;
  latestLearningReturn?: number;
  latestLearningTags?: string[];
}

interface LearningExpectation {
  roleLabel: string;
  summary: string;
  returnText: string;
  tagsText: string;
}

function quoteSQL(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

async function expectLearningPanel(page: import("@playwright/test").Page, expectation: LearningExpectation) {
  const learningPanel = page.locator(".rounded-xl.border.border-indigo-200.bg-indigo-50");
  await page.locator("button").filter({ hasText: expectation.roleLabel }).first().click();
  await expect(learningPanel.getByText(expectation.summary, { exact: true })).toBeVisible();
  await expect(learningPanel.getByText(expectation.returnText, { exact: true })).toBeVisible();
  await expect(learningPanel.getByText(expectation.tagsText, { exact: true })).toBeVisible();
}

function insertMemories(
  records: Array<{
    fundId: string;
    agentId: string;
    layer: "long_term" | "daily" | "dreams" | "agent";
    title: string;
    content: string;
    tradingDate?: string;
    tags?: string[];
  }>,
) {
  const values = records
    .map((record) => {
      const tradingDate = record.tradingDate ? quoteSQL(record.tradingDate) : "NULL";
      const tags = (record.tags ?? []).map((tag) => quoteSQL(tag)).join(", ");
      return `(${quoteSQL(record.fundId)}, ${quoteSQL(record.agentId)}, ${quoteSQL(record.layer)}, ${quoteSQL(record.title)}, ${quoteSQL(record.content)}, ${tradingDate}, ARRAY[${tags}]::text[])`;
    })
    .join(",\n");

  const sql = `
    INSERT INTO memories (fund_id, agent_id, layer, title, content, trading_date, tags)
    VALUES
    ${values};
  `;

  execFileSync(
    "docker",
    [
      "exec",
      "-i",
      "-e",
      `PGPASSWORD=${POSTGRES_PASSWORD}`,
      POSTGRES_CONTAINER,
      "psql",
      "-U",
      POSTGRES_USER,
      "-d",
      POSTGRES_DB,
      "-v",
      "ON_ERROR_STOP=1",
    ],
    {
      input: sql,
      stdio: ["pipe", "pipe", "pipe"],
    },
  );
}

test.describe("team management and memory center", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });
  test("edits per-agent config and filters memory by agent", async ({ page }) => {
    const { user, fundId } = await ensureSeedData();

    await login(page, user);
    await page.goto(`/funds/${fundId}/team`);
    await expect(page.getByRole("heading", { name: "团队管理" })).toBeVisible();

    await page.getByRole("button", { name: "新增成员" }).click();
    await page.getByRole("heading", { name: "新增成员" }).waitFor();
    await page.locator("#hire-role").selectOption("trader");
    await page.getByRole("button", { name: "确认新增" }).click();

    await expect(page.getByText("独立模型配置").first()).toBeVisible();
    await expect(page.getByRole("button", { name: "保存配置" })).toBeVisible();

    const selectedAgentIdText = await page.getByText(/^成员标识[:：]/).textContent();
    const selectedAgentId = selectedAgentIdText?.replace(/^成员标识[:：]\s*/, "").trim() ?? "";
    expect(selectedAgentId).not.toBe("");

    const systemPrompt = createE2EName("trader-system-prompt");
    const expectedSkillConfig = {
      enabled: true,
      skills: [
        {
          key: createE2EName("trader-skill"),
          name: "成交拆单模板",
          content: "高波动时优先拆单并限制尾盘追价。",
          enabled: true,
          priority: 90,
          match: {
            roles: ["trader"],
            workflowSteps: ["trade_execution"],
          },
        },
      ],
    };
    const skillConfig = JSON.stringify(expectedSkillConfig);

    await page.getByLabel("系统提示词").fill(systemPrompt);
    await page.getByLabel("技能配置").fill(skillConfig);
    await page.getByLabel("领域配置").fill('{"style":"short-term"}');
    await page.getByLabel("进化配置").fill('{"enabled":true}');
    await page.getByRole("button", { name: "保存配置" }).click();

    await expect(page.getByText("成员配置已保存。", { exact: true })).toBeVisible();

    await page.reload();
    await expect(page.getByLabel("系统提示词")).toHaveValue(systemPrompt);
    const reloadedSkillConfig = JSON.parse(await page.getByLabel("技能配置").inputValue());
    expect(reloadedSkillConfig).toEqual(expectedSkillConfig);
    const reloadedDomainConfig = JSON.parse(await page.getByLabel("领域配置").inputValue());
    expect(reloadedDomainConfig).toEqual({
      style: "short-term",
      coverage: {
        markets: [],
        directions: [],
        assetClasses: [],
      },
      specialization: {
        themes: [],
        markets: [],
        patterns: [],
        styleHints: [],
        instruments: [],
        assetClasses: [],
      },
    });
    await expect(page.getByLabel("进化配置")).toHaveValue('{\n  "enabled": true\n}');

    await page.getByRole("button", { name: "新增成员" }).click();
    await page.getByRole("heading", { name: "新增成员" }).waitFor();
    await page.locator("#hire-role").selectOption("risk");
    await page.getByRole("button", { name: "确认新增" }).click();

    await page.getByRole("button", { name: "新增成员" }).click();
    await page.getByRole("heading", { name: "新增成员" }).waitFor();
    await page.locator("#hire-role").selectOption("pm");
    await page.getByRole("button", { name: "确认新增" }).click();

    const token = await page.evaluate(() => window.localStorage.getItem("fundai.jwt") || "");
    expect(token).not.toBe("");

    const api = await playwrightRequest.newContext({
      baseURL: process.env.PLAYWRIGHT_API_URL || "http://127.0.0.1:8080",
      extraHTTPHeaders: {
        Authorization: `Bearer ${token}`,
      },
    });

    const teamResponse = await api.get(`/api/funds/${fundId}/team`);
    expect(teamResponse.ok()).toBeTruthy();
    const teamAgents = (await teamResponse.json()) as TeamAgent[];
    expect(teamAgents).toHaveLength(3);

    const traderAgent = teamAgents.find((agent) => agent.role === "trader");
    const riskAgent = teamAgents.find((agent) => agent.role === "risk");
    const pmAgent = teamAgents.find((agent) => agent.role === "pm");
    expect(traderAgent?.id).toBeTruthy();
    expect(riskAgent?.id).toBeTruthy();
    expect(pmAgent?.id).toBeTruthy();
    expect(traderAgent?.id).toBe(selectedAgentId);
    expect(traderAgent?.skillConfig).toEqual(expectedSkillConfig);

    const traderMemoryTitle = createE2EName("trader-memory");
    const riskMemoryTitle = createE2EName("risk-memory");

    const traderLearningTitle = createE2EName("trader-learning");
    const traderLearningSummary = `${traderLearningTitle} summary`;
    const riskLearningTitle = createE2EName("risk-learning");
    const riskLearningSummary = `${riskLearningTitle} summary`;
    const pmLearningTitle = createE2EName("pm-learning");
    const pmLearningSummary = `${pmLearningTitle} summary`;

    insertMemories([
      {
        fundId,
        agentId: traderAgent!.id,
        layer: "long_term",
        title: traderMemoryTitle,
        content: `${traderMemoryTitle} content`,
        tradingDate: "2026-05-11",
        tags: ["e2e", "trader"],
      },
      {
        fundId,
        agentId: traderAgent!.id,
        layer: "agent",
        title: traderLearningTitle,
        content: JSON.stringify({
          summary: traderLearningSummary,
          hits: ["成交节奏稳定"],
          misses: ["部分成交偏多"],
          lessons: ["优先拆单"],
          adjustments: ["减少尾盘追价"],
          dailyReturn: 0.0215,
          tags: ["e2e", "trader", "self_learning"],
        }),
        tradingDate: "2026-05-11",
        tags: ["e2e", "trader", "self_learning"],
      },
      {
        fundId,
        agentId: riskAgent!.id,
        layer: "long_term",
        title: riskMemoryTitle,
        content: `${riskMemoryTitle} content`,
        tradingDate: "2026-05-11",
        tags: ["e2e", "risk"],
      },
      {
        fundId,
        agentId: riskAgent!.id,
        layer: "agent",
        title: riskLearningTitle,
        content: JSON.stringify({
          summary: riskLearningSummary,
          hits: ["回撤控制稳定"],
          misses: ["盘中告警偏慢"],
          lessons: ["缩短高波动窗口监控间隔"],
          adjustments: ["提升尾盘仓位阈值敏感度"],
          dailyReturn: -0.0042,
          tags: ["e2e", "risk", "self_learning"],
        }),
        tradingDate: "2026-05-11",
        tags: ["e2e", "risk", "self_learning"],
      },
      {
        fundId,
        agentId: pmAgent!.id,
        layer: "agent",
        title: pmLearningTitle,
        content: JSON.stringify({
          summary: pmLearningSummary,
          hits: ["计划执行与收益方向一致"],
          misses: ["板块轮动应对偏慢"],
          lessons: ["盘前提高候选主题优先级排序"],
          adjustments: ["增加午后再平衡检查"],
          dailyReturn: 0.0132,
          tags: ["e2e", "pm", "self_learning"],
        }),
        tradingDate: "2026-05-11",
        tags: ["e2e", "pm", "self_learning"],
      },
    ]);

    const learningResponse = await api.get(`/api/funds/${fundId}/team`);
    expect(learningResponse.ok()).toBeTruthy();
    const learningAgents = (await learningResponse.json()) as TeamAgent[];
    const updatedTraderAgent = learningAgents.find((agent) => agent.id === traderAgent!.id);
    const updatedRiskAgent = learningAgents.find((agent) => agent.id === riskAgent!.id);
    const updatedPmAgent = learningAgents.find((agent) => agent.id === pmAgent!.id);
    expect(updatedTraderAgent?.latestLearningSummary).toBe(traderLearningSummary);
    expect(updatedTraderAgent?.latestLearningReturn).toBe(0.0215);
    expect(updatedTraderAgent?.latestLearningTags).toEqual(expect.arrayContaining(["self_learning", "trader"]));
    expect(updatedRiskAgent?.latestLearningSummary).toBe(riskLearningSummary);
    expect(updatedRiskAgent?.latestLearningReturn).toBe(-0.0042);
    expect(updatedRiskAgent?.latestLearningTags).toEqual(expect.arrayContaining(["self_learning", "risk"]));
    expect(updatedPmAgent?.latestLearningSummary).toBe(pmLearningSummary);
    expect(updatedPmAgent?.latestLearningReturn).toBe(0.0132);
    expect(updatedPmAgent?.latestLearningTags).toEqual(expect.arrayContaining(["self_learning", "pm"]));

    await api.dispose();

    await page.goto(`/funds/${fundId}/team`);
    await expect(page.getByText("自主学习 / 每日复盘", { exact: true })).toBeVisible();
    await expectLearningPanel(page, {
      roleLabel: "交易员",
      summary: traderLearningSummary,
      returnText: "2.15%",
      tagsText: "e2e / trader / self_learning",
    });
    await expectLearningPanel(page, {
      roleLabel: "风控",
      summary: riskLearningSummary,
      returnText: "-0.42%",
      tagsText: "e2e / risk / self_learning",
    });
    await expectLearningPanel(page, {
      roleLabel: "组合经理",
      summary: pmLearningSummary,
      returnText: "1.32%",
      tagsText: "e2e / pm / self_learning",
    });

    await page.goto(`/funds/${fundId}/memory`);
    await expect(page.getByRole("heading", { name: "记忆中心" })).toBeVisible();
    await expect(page.getByText(traderMemoryTitle, { exact: true }).first()).toBeVisible();
    await expect(page.getByText(riskMemoryTitle, { exact: true }).first()).toBeVisible();

    await page.getByLabel("成员筛选").selectOption(traderAgent!.id);
    await expect(page.getByLabel("成员筛选")).toHaveValue(traderAgent!.id);
    await expect(page.getByText(traderMemoryTitle, { exact: true }).first()).toBeVisible();
    await expect(page.getByText(riskMemoryTitle, { exact: true })).toHaveCount(0);
  });

  test("saves structured coverage and specialization and reloads them", async ({ page }) => {
    const { user, fundId } = await ensureSeedData();

    await login(page, user);
    await page.goto(`/funds/${fundId}/team`);
    await expect(page.getByRole("heading", { name: "团队管理" })).toBeVisible();

    await page.getByRole("button", { name: "新增成员" }).click();
    await page.getByRole("heading", { name: "新增成员" }).waitFor();
    await page.locator("#hire-role").selectOption("researcher");
    await page.locator("#hire-focus").selectOption("stock");
    await page.getByRole("button", { name: "确认新增" }).click();

    await expect(page.getByLabel("角色")).toHaveValue("researcher");

    await page.locator("select[multiple]").nth(0).selectOption(["a_share", "crypto"]);
    await page.locator("select[multiple]").nth(1).selectOption(["equity", "crypto"]);
    await page.locator("select[multiple]").nth(2).selectOption(["stocks", "crypto"]);
    await page.getByLabel("专精市场").fill("a_share, crypto");
    await page.getByLabel("专精资产类别").fill("equity, crypto");
    await page.getByLabel("专精主题").fill("CPO, AI infra");
    await page.getByLabel("专精标的").fill("NVDA, BTCUSDT");
    await page.getByLabel("专精风格提示").fill("growth, breakout");
    await page.getByLabel("专精模式").fill("supply chain mapping, liquidity squeeze");
    await page.getByLabel("领域配置").fill('{"style":"swing"}');
    await page.getByRole("button", { name: "保存配置" }).click();

    await expect(page.getByText("成员配置已保存。", { exact: true })).toBeVisible();
    await page.reload();

    await expect(page.locator("select[multiple]").nth(0).locator("option:checked")).toHaveText(["A股", "Crypto"]);
    await expect(page.locator("select[multiple]").nth(1).locator("option:checked")).toHaveText(["股票", "Crypto"]);
    await expect(page.locator("select[multiple]").nth(2).locator("option:checked")).toHaveText(["股票", "Crypto"]);
    await expect(page.getByLabel("专精市场")).toHaveValue("a_share, crypto");
    await expect(page.getByLabel("专精资产类别")).toHaveValue("equity, crypto");
    await expect(page.getByLabel("专精主题")).toHaveValue("CPO, AI infra");
    await expect(page.getByLabel("专精标的")).toHaveValue("NVDA, BTCUSDT");
    await expect(page.getByLabel("专精风格提示")).toHaveValue("growth, breakout");
    await expect(page.getByLabel("专精模式")).toHaveValue("supply chain mapping, liquidity squeeze");

    const token = await page.evaluate(() => window.localStorage.getItem("fundai.jwt") || "");
    expect(token).not.toBe("");

    const api = await playwrightRequest.newContext({
      baseURL: process.env.PLAYWRIGHT_API_URL || "http://127.0.0.1:8080",
      extraHTTPHeaders: {
        Authorization: `Bearer ${token}`,
      },
    });

    const teamResponse = await api.get(`/api/funds/${fundId}/team`);
    expect(teamResponse.ok()).toBeTruthy();
    const teamAgents = (await teamResponse.json()) as TeamAgent[];
    const researcher = teamAgents.find((agent) => agent.role === "researcher");
    expect(researcher?.domainConfig?.coverage?.markets).toEqual(expect.arrayContaining(["a_share", "crypto"]));
    expect(researcher?.domainConfig?.coverage?.assetClasses).toEqual(expect.arrayContaining(["equity", "crypto"]));
    expect(researcher?.domainConfig?.coverage?.directions).toEqual(expect.arrayContaining(["stocks", "crypto"]));
    expect(researcher?.domainConfig?.specialization?.markets).toEqual(expect.arrayContaining(["a_share", "crypto"]));
    expect(researcher?.domainConfig?.specialization?.assetClasses).toEqual(expect.arrayContaining(["equity", "crypto"]));
    expect(researcher?.domainConfig?.specialization?.themes).toEqual(expect.arrayContaining(["CPO", "AI infra"]));
    expect(researcher?.domainConfig?.specialization?.instruments).toEqual(expect.arrayContaining(["NVDA", "BTCUSDT"]));
    expect(researcher?.domainConfig?.specialization?.styleHints).toEqual(expect.arrayContaining(["growth", "breakout"]));
    expect(researcher?.domainConfig?.specialization?.patterns).toEqual(expect.arrayContaining(["supply chain mapping", "liquidity squeeze"]));

    await api.dispose();
  });
});
