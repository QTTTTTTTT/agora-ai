import { expect, test } from "@playwright/test";
import {
  createCompanyViaUI,
  createE2EName,
  createFundViaUI,
  createE2EUser,
  ensureCompanyOnly,
  ensureSeedData,
  login,
  register,
  setAppPreferences,
} from "./helpers";

test.describe("companies page", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });
  test("renders seeded company and fund entry after login", async ({
    page,
  }) => {
    const { user } = await ensureSeedData();
    await login(page, user);
    await expect(page.getByRole("heading", { name: "公司列表" })).toBeVisible();
    await expect(page.getByText("E2E Company")).toBeVisible();
    await expect(page.getByText("E2E Primary Fund")).toBeVisible();
    await expect(page.getByRole("link", { name: "打开主基金" })).toBeVisible();
  });

  test("lets a fresh user create first company and first fund from the UI", async ({
    page,
  }) => {
    const user = createE2EUser("ui-onboarding");
    const companyName = createE2EName("UI Company");
    const fundName = createE2EName("UI Fund");

    await register(page, user);
    await createCompanyViaUI(
      page,
      companyName,
      "Created from the onboarding flow",
    );
    await expect(
      page.getByRole("button", { name: "展开高级设置" }),
    ).toBeVisible();
    await expect(page.getByRole("textbox", { name: "主题标签" })).toHaveCount(
      0,
    );
    await createFundViaUI(page, {
      name: fundName,
      description: "Created from the onboarding flow",
      tradingMode: "simulation",
      initialCapital: "120000",
      market: "a_share",
      exchange: "SSE",
      assetClass: "equity",
      baseCurrency: "CNY",
      benchmarkSymbol: "000300",
      primaryDirection: "stocks",
      universeMode: "manual",
      universeSymbols: ["600519", "000858"],
      universeThemes: ["高股息", "白酒"],
      universeSectors: ["consumer"],
      universeCustomFilters: ["pe<30"],
      specializationMarkets: ["a_share"],
      specializationAssetClasses: ["equity"],
      specializationThemes: ["高股息", "消费龙头"],
      specializationInstruments: ["600519", "000858"],
      specializationStyleHints: ["quality", "mean-reversion"],
    });

    await expect(page).toHaveURL(/\/funds\/.+/);
    await expect(page.getByText("策略流程状态")).toBeVisible();
    await expect(page.getByText("SSE").first()).toBeVisible();
    await expect(page.getByText("CNY").first()).toBeVisible();
    await expect(page.getByText(/基准 000300/)).toBeVisible();
    await expect(page.getByText(/标的池 600519, 000858/)).toBeVisible();

    await page
      .getByRole("navigation")
      .getByRole("link", { name: "基金设置" })
      .click();
    await expect(page.locator("select").nth(2)).toHaveValue("a_share");
    await expect(page.getByRole("textbox", { name: "自选标的" })).toHaveValue(
      "600519, 000858",
    );
    await expect(page.getByRole("textbox", { name: "主题标签" })).toHaveValue(
      "高股息, 白酒",
    );
    await expect(page.getByRole("textbox", { name: "行业/板块" })).toHaveValue(
      "consumer",
    );
    await expect(
      page.getByRole("textbox", { name: "自定义筛选条件" }),
    ).toHaveValue("pe<30");
    await expect(page.getByRole("textbox", { name: "专精市场" })).toHaveValue(
      "a_share",
    );
    await expect(
      page.getByRole("textbox", { name: "专精资产类别" }),
    ).toHaveValue("equity");
    await expect(page.getByRole("textbox", { name: "专精主题" })).toHaveValue(
      "高股息, 消费龙头",
    );
    await expect(page.getByRole("textbox", { name: "专精标的" })).toHaveValue(
      "600519, 000858",
    );
    await expect(
      page.getByRole("textbox", { name: "专精风格提示" }),
    ).toHaveValue("quality, mean-reversion");
  });

  test("lets a zero-fund company create its first fund from the company card", async ({
    page,
  }) => {
    const companyName = createE2EName("Zero Fund Company");
    const fundName = createE2EName("Zero Fund");
    const { user } = await ensureCompanyOnly(
      createE2EUser("company-only"),
      companyName,
    );

    await login(page, user);
    await expect(page.getByText(companyName)).toBeVisible();
    await expect(
      page.getByRole("button", { name: "创建首只基金" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "创建首只基金" }).click();
    await createFundViaUI(page, {
      name: fundName,
      tradingMode: "simulation",
      initialCapital: "90000",
      market: "us_equity",
      exchange: "NASDAQ",
      assetClass: "equity",
      baseCurrency: "USD",
      benchmarkSymbol: "QQQ",
      primaryDirection: "stocks",
      universeMode: "manual",
      universeSymbols: ["AAPL", "NVDA"],
    });

    await expect(page).toHaveURL(/\/funds\/.+/);
    await expect(page.getByText("策略流程状态")).toBeVisible();
    await expect(page.getByText("NASDAQ")).toBeVisible();
    await expect(page.getByText("股票")).toBeVisible();

    await page
      .getByRole("navigation")
      .getByRole("link", { name: "基金设置" })
      .click();
    await expect(page.getByLabel("交易日历代码", { exact: true })).toHaveValue(
      "US-XNAS",
    );
    await expect(page.getByLabel("时区", { exact: true })).toHaveValue(
      "America/New_York",
    );
  });

  test("saves fund specialization in fund settings", async ({ page }) => {
    const { user } = await ensureSeedData();

    await login(page, user);
    await page.getByRole("link", { name: "打开主基金" }).click();
    await expect(page.getByText("策略流程状态")).toBeVisible();

    await page
      .getByRole("navigation")
      .getByRole("link", { name: "基金设置" })
      .click();
    await expect(page.getByRole("heading", { name: "基金设置" })).toBeVisible();

    await page.getByLabel("专精市场").fill("us_equity, crypto");
    await page.getByLabel("专精资产类别").fill("equity, crypto");
    await page.getByLabel("专精主题").fill("CPO, AI infra");
    await page.getByLabel("专精标的").fill("NVDA, BTCUSDT");
    await page.getByLabel("专精风格提示").fill("growth, event-driven");
    await page.getByRole("button", { name: "保存设置" }).click();

    await expect(
      page.getByText("基金设置已保存。", { exact: true }),
    ).toBeVisible();
    await page.reload();

    await expect(page.getByLabel("专精市场")).toHaveValue("us_equity, crypto");
    await expect(page.getByLabel("专精资产类别")).toHaveValue("equity, crypto");
    await expect(page.getByLabel("专精主题")).toHaveValue("CPO, AI infra");
    await expect(page.getByLabel("专精标的")).toHaveValue("NVDA, BTCUSDT");
    await expect(page.getByLabel("专精风格提示")).toHaveValue(
      "growth, event-driven",
    );
    await expect(page.getByRole("heading", { name: "团队专精" })).toBeVisible();
    await expect(page.getByText("us_equity, crypto").first()).toBeVisible();
    await expect(page.getByText("CPO, AI infra").first()).toBeVisible();
  });

  test("logs out back to login page", async ({ page }) => {
    const { user } = await ensureSeedData();
    await login(page, user);
    await page.getByRole("button", { name: "退出登录" }).click();
    await expect(page).toHaveURL(/\/login$/);
    await expect(
      page.getByRole("heading", { name: "登录控制台" }),
    ).toBeVisible();
  });
});
