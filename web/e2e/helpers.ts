import { expect, Page, request as playwrightRequest } from "@playwright/test";

const API_URL = process.env.PLAYWRIGHT_API_URL || "http://localhost:8080";
const DEFAULT_PASSWORD = "Passw0rd!";

export interface E2EUser {
  email: string;
  password: string;
  displayName: string;
}

export interface E2EFundInput {
  name: string;
  description?: string;
  tradingMode?: "simulation" | "live";
  initialCapital?: string;
  market?: string;
  exchange?: string;
  assetClass?: string;
  baseCurrency?: string;
  benchmarkSymbol?: string;
  primaryDirection?: string;
  calendarCode?: string;
  timeZone?: string;
  universeMode?: string;
  universeSymbols?: string[] | string;
  universeThemes?: string[] | string;
  universeSectors?: string[] | string;
  universeCustomFilters?: string[] | string;
  specializationMarkets?: string[] | string;
  specializationAssetClasses?: string[] | string;
  specializationThemes?: string[] | string;
  specializationInstruments?: string[] | string;
  specializationStyleHints?: string[] | string;
}

export async function setAppPreferences(
  page: Page,
  preferences: {
    language?: "zh-CN" | "en-US";
    displayCurrency?: "USD" | "CNY";
  } = {},
) {
  const { language = "zh-CN", displayCurrency = "USD" } = preferences;
  await page.addInitScript(
    ({ nextLanguage, nextDisplayCurrency }) => {
      window.localStorage.setItem("fundai.language", nextLanguage);
      window.localStorage.setItem(
        "fundai.display_currency",
        nextDisplayCurrency,
      );
    },
    { nextLanguage: language, nextDisplayCurrency: displayCurrency },
  );
}

const DEFAULT_FUND_PROFILE = {
  market: "us_equity",
  exchange: "NASDAQ",
  assetClass: "equity",
  baseCurrency: "USD",
  benchmarkSymbol: "SPY",
  primaryDirection: "stocks",
  universeMode: "manual",
} as const;

export function createE2EUser(prefix = "e2e"): E2EUser {
  const suffix = `${Date.now()}-${Math.random().toString(36).slice(2, 8)}`;
  return {
    email: `${prefix}-${suffix}@example.com`,
    password: DEFAULT_PASSWORD,
    displayName: `${prefix.toUpperCase()} User`,
  };
}

export function createE2EName(prefix: string): string {
  return `${prefix}-${Date.now()}-${Math.random().toString(36).slice(2, 6)}`;
}

function normalizeUniverseSymbols(value?: string[] | string): string[] {
  if (!value) {
    return [];
  }
  if (Array.isArray(value)) {
    return value.map((item) => item.trim()).filter(Boolean);
  }
  return value
    .split(/[\n,]/)
    .map((item) => item.trim())
    .filter(Boolean);
}

async function registerViaApi(user: E2EUser) {
  const api = await playwrightRequest.newContext({
    baseURL: API_URL,
    extraHTTPHeaders: {
      "Content-Type": "application/json",
      "X-Request-ID": `e2e-seed-${Date.now()}`,
    },
  });

  const registerResponse = await api.post("/api/auth/register", {
    data: {
      email: user.email,
      password: user.password,
      displayName: user.displayName,
    },
  });
  expect(registerResponse.ok()).toBeTruthy();

  const registerPayload = await registerResponse.json();
  const token = registerPayload.token as string;
  expect(token).toBeTruthy();

  return {
    api,
    authHeaders: {
      Authorization: `Bearer ${token}`,
      "X-Request-ID": `e2e-auth-${Date.now()}`,
    },
  };
}

export async function login(page: Page, user: E2EUser) {
  await page.goto("/login");
  await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  await page.getByLabel("邮箱").fill(user.email);
  await page.getByPlaceholder("请输入至少 8 位密码").fill(user.password);
  await page.getByRole("button", { name: "登录并进入系统" }).click();
  await expect(page).toHaveURL(/\/companies$/);
}

export async function register(page: Page, user: E2EUser) {
  await page.goto("/login");
  await page.getByRole("button", { name: "注册" }).click();
  await page.getByLabel("邮箱").fill(user.email);
  await page.getByLabel("显示名称").fill(user.displayName);
  await page.getByPlaceholder("请输入至少 8 位密码").fill(user.password);
  await page.getByPlaceholder("请再次输入密码").fill(user.password);
  await page.getByRole("button", { name: "注册并进入系统" }).click();
  await expect(page).toHaveURL(/\/companies$/);
}

export async function createCompanyViaUI(
  page: Page,
  name: string,
  description = "",
) {
  const emptyStateButton = page.getByRole("button", { name: "创建第一家公司" });
  if (await emptyStateButton.isVisible().catch(() => false)) {
    await emptyStateButton.click();
  } else {
    await page.getByRole("button", { name: "创建公司" }).click();
  }

  await expect(page.getByRole("heading", { name: "创建公司" })).toBeVisible();
  await page.getByLabel("公司名称").fill(name);
  if (description) {
    await page.getByLabel("公司描述").fill(description);
  }
  await page.getByRole("button", { name: "创建公司" }).last().click();
}

export async function createFundViaUI(page: Page, fund: E2EFundInput) {
  await expect(
    page.getByRole("heading", { name: "创建首只基金" }),
  ).toBeVisible();
  await page.getByLabel("基金名称").fill(fund.name);
  if (fund.description) {
    await page.getByLabel("基金描述").fill(fund.description);
  }
  if (fund.tradingMode) {
    await page.getByLabel("交易模式").selectOption(fund.tradingMode);
  }
  if (fund.initialCapital) {
    await page.getByLabel("初始资金").fill(fund.initialCapital);
  }
  if (fund.market) {
    await page.getByLabel("市场", { exact: true }).selectOption(fund.market);
  }

  const needsAdvanced =
    !!fund.exchange ||
    !!fund.assetClass ||
    !!fund.baseCurrency ||
    !!fund.benchmarkSymbol ||
    !!fund.primaryDirection ||
    !!fund.calendarCode ||
    !!fund.timeZone ||
    !!fund.universeMode ||
    !!fund.universeSymbols ||
    !!fund.universeThemes ||
    !!fund.universeSectors ||
    !!fund.universeCustomFilters ||
    !!fund.specializationMarkets ||
    !!fund.specializationAssetClasses ||
    !!fund.specializationThemes ||
    !!fund.specializationInstruments ||
    !!fund.specializationStyleHints;

  if (needsAdvanced) {
    await page.getByRole("button", { name: "展开高级设置" }).click();
  }

  if (fund.exchange) {
    await page.getByLabel("交易所", { exact: true }).fill(fund.exchange);
  }
  if (fund.assetClass) {
    await page
      .getByLabel("资产类别", { exact: true })
      .selectOption(fund.assetClass);
  }
  if (fund.baseCurrency) {
    await page.getByLabel("基础货币", { exact: true }).fill(fund.baseCurrency);
  }
  if (fund.benchmarkSymbol) {
    await page
      .getByLabel("基准标的", { exact: true })
      .fill(fund.benchmarkSymbol);
  }
  if (fund.primaryDirection) {
    await page
      .getByLabel("主方向", { exact: true })
      .selectOption(fund.primaryDirection);
  }
  if (fund.calendarCode) {
    await page
      .getByLabel("交易日历代码", { exact: true })
      .fill(fund.calendarCode);
  }
  if (fund.timeZone) {
    await page.getByLabel("时区", { exact: true }).fill(fund.timeZone);
  }
  if (fund.universeMode) {
    await page
      .getByLabel("标的池模式", { exact: true })
      .selectOption(fund.universeMode);
  }
  if (fund.universeSymbols) {
    await page
      .getByLabel("自选标的", { exact: true })
      .fill(normalizeUniverseSymbols(fund.universeSymbols).join(", "));
  }
  if (fund.universeThemes) {
    await page
      .getByLabel("主题标签", { exact: true })
      .fill(normalizeUniverseSymbols(fund.universeThemes).join(", "));
  }
  if (fund.universeSectors) {
    await page
      .getByLabel("行业/板块", { exact: true })
      .fill(normalizeUniverseSymbols(fund.universeSectors).join(", "));
  }
  if (fund.universeCustomFilters) {
    await page
      .getByLabel("自定义筛选条件", { exact: true })
      .fill(normalizeUniverseSymbols(fund.universeCustomFilters).join(", "));
  }
  if (fund.specializationMarkets) {
    await page
      .getByLabel("专精市场", { exact: true })
      .fill(normalizeUniverseSymbols(fund.specializationMarkets).join(", "));
  }
  if (fund.specializationAssetClasses) {
    await page
      .getByLabel("专精资产类别", { exact: true })
      .fill(
        normalizeUniverseSymbols(fund.specializationAssetClasses).join(", "),
      );
  }
  if (fund.specializationThemes) {
    await page
      .getByLabel("专精主题", { exact: true })
      .fill(normalizeUniverseSymbols(fund.specializationThemes).join(", "));
  }
  if (fund.specializationInstruments) {
    await page
      .getByLabel("专精标的", { exact: true })
      .fill(
        normalizeUniverseSymbols(fund.specializationInstruments).join(", "),
      );
  }
  if (fund.specializationStyleHints) {
    await page
      .getByLabel("专精风格提示", { exact: true })
      .fill(normalizeUniverseSymbols(fund.specializationStyleHints).join(", "));
  }
  await page
    .getByRole("button", { name: "创建基金并进入系统" })
    .evaluate((button) => {
      const form = button.closest("form") as HTMLFormElement | null;
      form?.requestSubmit();
    });
}

export async function ensureCompanyOnly(
  user = createE2EUser("company-only"),
  companyName = "E2E Company",
) {
  const { api, authHeaders } = await registerViaApi(user);

  const createCompany = await api.post("/api/companies", {
    headers: authHeaders,
    data: {
      name: companyName,
      description: "Regression suite company",
    },
  });
  expect(createCompany.ok()).toBeTruthy();
  const companyPayload = await createCompany.json();

  await api.dispose();
  return { user, companyId: companyPayload.id as string, companyName };
}

export async function ensureSeedData(
  user = createE2EUser("seed"),
  companyName = "E2E Company",
  fundName = "E2E Primary Fund",
  fundOverrides: Partial<E2EFundInput> = {},
) {
  const { api, authHeaders } = await registerViaApi(user);

  const createCompany = await api.post("/api/companies", {
    headers: authHeaders,
    data: {
      name: companyName,
      description: "Regression suite company",
    },
  });
  expect(createCompany.ok()).toBeTruthy();
  const companyPayload = await createCompany.json();
  const companyId = companyPayload.id as string;

  const profile = {
    ...DEFAULT_FUND_PROFILE,
    ...fundOverrides,
  };

  const createFund = await api.post(`/api/companies/${companyId}/funds`, {
    headers: authHeaders,
    data: {
      name: fundName,
      description: fundOverrides.description ?? "Regression suite fund",
      tradingMode: fundOverrides.tradingMode ?? "simulation",
      initialCapital: Number(fundOverrides.initialCapital ?? "100000"),
      market: profile.market,
      exchange: profile.exchange,
      assetClass: profile.assetClass,
      baseCurrency: profile.baseCurrency,
      benchmarkSymbol: profile.benchmarkSymbol,
      primaryDirection: profile.primaryDirection,
      universe: {
        mode: profile.universeMode,
        symbols: normalizeUniverseSymbols(profile.universeSymbols),
        themes: normalizeUniverseSymbols(profile.universeThemes),
        sectors: normalizeUniverseSymbols(profile.universeSectors),
        customFilters: normalizeUniverseSymbols(profile.universeCustomFilters),
      },
    },
  });
  expect(createFund.ok()).toBeTruthy();
  const fundPayload = await createFund.json();

  await api.dispose();
  return {
    user,
    companyId,
    companyName,
    fundId: fundPayload.id as string,
    fundName,
  };
}
