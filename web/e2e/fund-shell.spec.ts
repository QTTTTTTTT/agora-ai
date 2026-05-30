import { execFileSync } from "node:child_process";
import { expect, test } from "@playwright/test";
import { createE2EName, ensureSeedData, login, setAppPreferences } from "./helpers";

const POSTGRES_CONTAINER = process.env.PLAYWRIGHT_POSTGRES_CONTAINER || "fundai-postgres";
const POSTGRES_USER = process.env.PLAYWRIGHT_POSTGRES_USER || process.env.POSTGRES_USER || "fundai";
const POSTGRES_DB = process.env.PLAYWRIGHT_POSTGRES_DB || process.env.POSTGRES_DB || "fundai";
const POSTGRES_PASSWORD = process.env.PLAYWRIGHT_POSTGRES_PASSWORD || process.env.POSTGRES_PASSWORD || "fundai_secret_change_me";

function quoteSQL(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

function randomUUID(): string {
  const part = (length: number) =>
    Array.from({ length }, () => Math.floor(Math.random() * 16).toString(16)).join("");
  return `${part(8)}-${part(4)}-4${part(3)}-a${part(3)}-${part(12)}`;
}

function insertDashboardFixtures(fundId: string) {
  const tradingDate = "2026-05-12";
  const planId = randomUUID();
  const actionId = randomUUID();
  const tradeId = randomUUID();
  const positionId = randomUUID();
  const navId = randomUUID();

  const sql = `
    INSERT INTO investment_plans (id, fund_id, trading_date, status, reasoning, risk_score, expected_return, risk_review)
    VALUES (
      ${quoteSQL(planId)},
      ${quoteSQL(fundId)},
      DATE '${tradingDate}',
      'approved',
      'Crypto 多市场验收计划',
      18,
      6.5,
      '{"verdict":"approved","overallNote":"验收用风控说明","checks":[{"id":"coverage","name":"Coverage","result":"pass","detail":"team coverage 与市场画像一致"}]}'::jsonb
    );

    INSERT INTO plan_actions (
      id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close,
      quantity, price, amount, stop_loss, take_profit, reasoning, confidence, supported_by, opposed_by, execution_status,
      sort_order, quote_currency, settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only
    )
    VALUES (
      ${quoteSQL(actionId)},
      ${quoteSQL(planId)},
      'BINANCE:BTCUSDT',
      'BTCUSDT',
      'crypto',
      'BINANCE',
      'crypto',
      'spot',
      'buy',
      'long',
      'open',
      0.25,
      62000,
      15500,
      58000,
      70000,
      '验收用 action',
      0.92,
      ARRAY['pm','researcher'],
      ARRAY[]::text[],
      'filled',
      1,
      'USDT',
      'USDT',
      NULL,
      NULL,
      1,
      NULL,
      false
    );

    INSERT INTO trade_executions (
      id, fund_id, plan_id, plan_action_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, side,
      position_side, open_close, order_type, quantity, price, amount, filled_qty, filled_price, fee_commission,
      fee_stamp_tax, fee_transfer, trading_mode, broker_order_id, mcp_server_id, status, executed_at, quote_currency,
      settlement_currency, margin_mode, leverage, contract_multiplier, expiry_date, reduce_only
    )
    VALUES (
      ${quoteSQL(tradeId)},
      ${quoteSQL(fundId)},
      ${quoteSQL(planId)},
      ${quoteSQL(actionId)},
      'BINANCE:BTCUSDT',
      'BTCUSDT',
      'crypto',
      'BINANCE',
      'crypto',
      'spot',
      'buy',
      'long',
      'open',
      'market',
      0.25,
      62000,
      15500,
      0.25,
      62000,
      12,
      0,
      0,
      'simulation',
      NULL,
      NULL,
      'filled',
      NOW(),
      'USDT',
      'USDT',
      NULL,
      NULL,
      1,
      NULL,
      false
    );

    INSERT INTO holding_positions (
      id, fund_id, instrument_key, symbol, name, market, exchange, asset_class, instrument_type, position_side,
      quote_currency, settlement_currency, margin_mode, quantity, available_qty, cost_price, current_price, market_value,
      weight, leverage, contract_multiplier, expiry_date, unrealized_pnl, margin_used
    )
    VALUES (
      ${quoteSQL(positionId)},
      ${quoteSQL(fundId)},
      'BINANCE:BTCUSDT',
      'BTCUSDT',
      'Bitcoin',
      'crypto',
      'BINANCE',
      'crypto',
      'spot',
      'long',
      'USDT',
      'USDT',
      NULL,
      0.25,
      0.25,
      62000,
      64500,
      16125,
      0.52,
      NULL,
      1,
      NULL,
      625,
      NULL
    );

    INSERT INTO nav_snapshots (
      id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return, positions_snapshot
    )
    VALUES (
      ${quoteSQL(navId)},
      ${quoteSQL(fundId)},
      DATE '${tradingDate}',
      1.0845,
      310000,
      16125,
      293875,
      0.018,
      0.0845,
      '[{"instrumentKey":"BINANCE:BTCUSDT","symbol":"BTCUSDT","market":"crypto","exchange":"BINANCE"}]'::jsonb
    );
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

test.describe("fund shell smoke", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });
  test("navigates through dashboard, subscription, models, and usage pages", async ({ page }) => {
    const { user, fundId } = await ensureSeedData();

    await login(page, user);
    await page.goto(`/funds/${fundId}`);

    await expect(page.getByRole("navigation").getByRole("link", { name: "基金总览" })).toBeVisible();
    await expect(page.getByText("策略流程状态")).toBeVisible();
    // The phrase "查看用量与账单" appears in two places now: a global
    // header dock that links to `/usage`, and the per-fund card on the
    // dashboard that links to `/funds/{fundId}/usage`. Strict mode (the
    // default) trips on the duplicate. We want the per-fund one
    // because that's what this test is about — the fund-shell
    // navigation surface.
    await expect(
      page.getByRole("link", { name: "查看用量与账单" }).filter({ hasText: "查看用量与账单" }).last(),
    ).toBeVisible();

    await page.getByRole("link", { name: "订阅管理" }).click();
    await expect(page).toHaveURL(new RegExp(`/funds/${fundId}/subscription$`));
    await expect(page.getByRole("heading", { name: "订阅管理" })).toBeVisible();
    await expect(page.getByText("当前订阅状态")).toBeVisible();

    await page.getByRole("link", { name: "模型配置" }).click();
    await expect(page).toHaveURL(new RegExp(`/funds/${fundId}/models$`));
    await expect(page.getByRole("heading", { name: "模型配置" })).toBeVisible();
    await expect(page.getByText("自定义端点配置")).toBeVisible();

    await page.getByRole("link", { name: "用量与账单" }).click();
    await expect(page).toHaveURL(new RegExp(`/funds/${fundId}/usage$`));
    await expect(page.getByRole("heading", { name: "用量与账单" })).toBeVisible();
    await expect(page.getByRole("button", { name: "今日" })).toBeVisible();
    await expect(page.getByRole("button", { name: "本月" })).toBeVisible();
    await expect(page.getByRole("button", { name: "历史" })).toBeVisible();
    await expect(page.getByRole("button", { name: "账单" })).toBeVisible();
  });

  test("saves market profile in fund settings and shows it on dashboard", async ({ page }) => {
    const { user, fundId } = await ensureSeedData(
      undefined,
      createE2EName("Multi Market Company"),
      createE2EName("Futures Fund"),
      {
        market: "futures",
        exchange: "CME",
        assetClass: "futures",
        baseCurrency: "USD",
        benchmarkSymbol: "ESU2026",
        primaryDirection: "futures",
        universeMode: "manual",
        universeSymbols: ["ESU2026", "NQU2026"],
        universeThemes: ["宏观", "股指期货"],
        universeSectors: ["index-futures"],
        universeCustomFilters: ["volume>100000"],
      },
    );

    await login(page, user);
    await page.goto(`/funds/${fundId}/settings`);

    await expect(page.getByRole("heading", { name: "基金设置" })).toBeVisible();
    await expect(page.getByRole("combobox", { name: "市场" })).toHaveValue("futures");
    await expect(page.getByRole("textbox", { name: "交易所" })).toHaveValue("CME");
    await expect(page.getByRole("combobox", { name: "资产类别" })).toHaveValue("futures");
    await expect(page.getByRole("textbox", { name: "基础货币" })).toHaveValue("USD");
    await expect(page.getByRole("textbox", { name: "基准标的" })).toHaveValue("ESU2026");
    await expect(page.getByRole("combobox", { name: "主方向" })).toHaveValue("futures");
    await expect(page.getByRole("textbox", { name: "自选标的" })).toHaveValue("ESU2026, NQU2026");
    await expect(page.getByRole("textbox", { name: "主题标签" })).toHaveValue("宏观, 股指期货");
    await expect(page.getByRole("textbox", { name: "行业/板块" })).toHaveValue("index-futures");
    await expect(page.getByRole("textbox", { name: "自定义筛选条件" })).toHaveValue("volume>100000");

    await page.getByRole("combobox", { name: "市场" }).selectOption("crypto");
    await page.getByRole("textbox", { name: "交易所" }).fill("BINANCE");
    await page.getByRole("combobox", { name: "资产类别" }).selectOption("crypto");
    await page.getByRole("textbox", { name: "基础货币" }).fill("USDT");
    await page.getByRole("textbox", { name: "基准标的" }).fill("BTCUSDT");
    await page.getByRole("combobox", { name: "主方向" }).selectOption("crypto");
    await page.getByRole("textbox", { name: "自选标的" }).fill("BTCUSDT, ETHUSDT");
    await page.getByRole("textbox", { name: "主题标签" }).fill("CEX, AI infra");
    await page.getByRole("textbox", { name: "行业/板块" }).fill("crypto");
    await page.getByRole("textbox", { name: "自定义筛选条件" }).fill("marketCap>5B");
    await page.getByRole("button", { name: "保存设置" }).click();

    await expect(page.getByText("基金设置已保存。", { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByRole("combobox", { name: "市场" })).toHaveValue("crypto");
    await expect(page.getByRole("textbox", { name: "交易所" })).toHaveValue("BINANCE");
    await expect(page.getByRole("combobox", { name: "资产类别" })).toHaveValue("crypto");
    await expect(page.getByRole("textbox", { name: "基础货币" })).toHaveValue("USDT");
    await expect(page.getByRole("textbox", { name: "基准标的" })).toHaveValue("BTCUSDT");
    await expect(page.getByRole("combobox", { name: "主方向" })).toHaveValue("crypto");
    await expect(page.getByRole("textbox", { name: "自选标的" })).toHaveValue("BTCUSDT, ETHUSDT");
    await expect(page.getByRole("textbox", { name: "主题标签" })).toHaveValue("CEX, AI infra");
    await expect(page.getByRole("textbox", { name: "行业/板块" })).toHaveValue("crypto");
    await expect(page.getByRole("textbox", { name: "自定义筛选条件" })).toHaveValue("marketCap>5B");
    await expect(page.getByText("crypto · BINANCE · crypto")).toBeVisible();

    await page.goto(`/funds/${fundId}`);
    await expect(page.getByText("Crypto").first()).toBeVisible();
    await expect(page.getByText("BINANCE").first()).toBeVisible();
    await expect(page.getByText("USDT").first()).toBeVisible();
    await expect(page.getByText(/基准 BTCUSDT/)).toBeVisible();
    await expect(page.getByText(/标的池 BTCUSDT, ETHUSDT/)).toBeVisible();
  });

  test("shows seeded multi-market plan, trade, and position fields across dashboard pages", async ({ page }) => {
    const { user, fundId } = await ensureSeedData(
      undefined,
      createE2EName("Seeded Market Company"),
      createE2EName("Seeded Crypto Fund"),
      {
        market: "crypto",
        exchange: "BINANCE",
        assetClass: "crypto",
        baseCurrency: "USDT",
        benchmarkSymbol: "BTCUSDT",
        primaryDirection: "crypto",
        universeMode: "manual",
        universeSymbols: ["BTCUSDT", "ETHUSDT"],
      },
    );

    insertDashboardFixtures(fundId);

    await login(page, user);
    await page.goto(`/funds/${fundId}`);

    await expect(page.getByText("BTCUSDT").first()).toBeVisible();
    await expect(page.getByText("BINANCE").first()).toBeVisible();
    await expect(page.getByText("USDT").first()).toBeVisible();

    await page.getByRole("navigation").getByRole("link", { name: "决策中心" }).click();
    await expect(page).toHaveURL(new RegExp(`/funds/${fundId}/decisions$`));
    await expect(page.getByRole("heading", { name: "决策中心" })).toBeVisible();
    await expect(page.getByText("BTCUSDT").first()).toBeVisible();
    await expect(page.getByText("BINANCE").first()).toBeVisible();
    await expect(page.getByText("验收用 action")).toBeVisible();

    await page.getByRole("link", { name: "交易记录" }).click();
    await expect(page).toHaveURL(new RegExp(`/funds/${fundId}/trades$`));
    await expect(page.getByRole("heading", { name: "交易记录" })).toBeVisible();
    await expect(page.getByText("BTCUSDT").first()).toBeVisible();
    await expect(page.getByText("BINANCE").first()).toBeVisible();
    await expect(page.getByText("USDT").first()).toBeVisible();
  });
});
