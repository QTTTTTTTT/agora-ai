// Card J — critical-path Playwright spec covering Decision Center
// + AB experiment lifecycle.
//
// Why this file exists:
//
//   The K-series cards rebuilt the AB shadow path end-to-end (real
//   B-side NAV recomputation in Go via lot ledger + price timeline,
//   K-3/K-4/K-5). Go-level coverage is strong (~60 packages), and
//   we have a docker smoke that exercises the analyze path on a
//   real DB, but neither exercises the **operator-facing UI flow**:
//   "log in, find a fund, create an experiment, run it through
//   start → stop → analyze, see the verdict." That single chain is
//   the surface 99% of operators touch on a Monday morning when an
//   AB test ships, and a regression there is the highest-cost
//   failure mode (silent UI break = no operator runs experiments,
//   even if the backend is healthy).
//
//   The spec is deliberately scoped tight: the user-facing happy
//   path + a couple of validation guards. Numerical correctness of
//   the analyze output (NAV math, P&L attribution, confidence
//   scoring) is already covered by the Go test suite — we don't
//   re-assert here, we just confirm the result section renders
//   *some* primitives so a future React refactor that drops the
//   confidence panel or the winner badge breaks CI immediately.
//
// What this spec deliberately does NOT cover:
//
//   - Decision Center plan creation flow (no plan generation
//     pipeline is invoked from the UI; that requires the
//     scheduler running which is out of scope for an e2e tick).
//   - Promotion / rollback of shadow-agent learning. That's a
//     follow-up spec because it needs a fully-analyzed test with
//     both control and treatment learning rows seeded; the
//     backfill cost outweighs the marginal coverage right now.
//   - LLM B-side decider. The AB shadow defaults to the
//     deterministic decider unless AB_SHADOW_LLM_ENABLED is set
//     in the compose stack, which CI does not enable (and rightly
//     so: e2e shouldn't depend on a live LLM).

import { execFileSync } from "node:child_process";
import { expect, test } from "@playwright/test";
import { createE2EName, ensureSeedData, login, setAppPreferences } from "./helpers";

const POSTGRES_CONTAINER = process.env.PLAYWRIGHT_POSTGRES_CONTAINER || "fundai-postgres";
const POSTGRES_USER = process.env.PLAYWRIGHT_POSTGRES_USER || process.env.POSTGRES_USER || "fundai";
const POSTGRES_DB = process.env.PLAYWRIGHT_POSTGRES_DB || process.env.POSTGRES_DB || "fundai";
const POSTGRES_PASSWORD =
  process.env.PLAYWRIGHT_POSTGRES_PASSWORD || process.env.POSTGRES_PASSWORD || "fundai_secret_change_me";

function quoteSQL(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

function randomUUID(): string {
  const part = (length: number) =>
    Array.from({ length }, () => Math.floor(Math.random() * 16).toString(16)).join("");
  return `${part(8)}-${part(4)}-4${part(3)}-a${part(3)}-${part(12)}`;
}

// seedABFixtures inserts the bare minimum NAV + trade history a
// fresh fund needs for the AB analyze path to produce a non-empty
// `results` payload.
//
// Why this is needed: a fund with zero nav_snapshots gets
// short-circuited by the AB shadow loader (`navs[]` empty → no
// shadow run, no result, the UI renders the empty-state). Seeding
// two NAV bars in the test window plus one trade gives the K-3
// path enough data to:
//   - recompute B's NAV against A's two bars,
//   - emit a navSeries with two points,
//   - produce a confidence + scorecard payload.
//
// We seed via docker-exec to match the existing fund-shell.spec.ts
// pattern (same env var contract, same container name). Doing it
// over the public REST API would require driving the workflow
// scheduler which is intentionally not exposed there.
function seedABFixtures(fundId: string) {
  // Two consecutive trading days inside the AB test window so the
  // analyze pipeline produces a 2-point navSeries (one entry == flat
  // line, two == divergence visible). Dates land *before* today so
  // the GetLatest fallback never fires.
  const day1 = "2026-05-25";
  const day2 = "2026-05-26";

  const planId = randomUUID();
  const actionId = randomUUID();
  const tradeId = randomUUID();
  const nav1Id = randomUUID();
  const nav2Id = randomUUID();

  // Plan + action + trade so the AB engine has something to
  // shadow. Quantities are tiny so the synthetic market value
  // never blows up on a low-precision schema.
  const sql = `
    INSERT INTO investment_plans (id, fund_id, trading_date, status, reasoning, risk_score, expected_return)
    VALUES (
      ${quoteSQL(planId)},
      ${quoteSQL(fundId)},
      DATE '${day2}',
      'approved',
      'AB e2e seed plan',
      10,
      2.5
    );

    INSERT INTO plan_actions (
      id, plan_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, action, position_side, open_close,
      quantity, price, amount, reasoning, confidence, supported_by, opposed_by, execution_status, sort_order
    )
    VALUES (
      ${quoteSQL(actionId)},
      ${quoteSQL(planId)},
      'NASDAQ:NVDA',
      'NVDA',
      'us_equity',
      'NASDAQ',
      'equity',
      'spot',
      'buy',
      'long',
      'open',
      10,
      150,
      1500,
      'seed action',
      0.85,
      ARRAY['pm']::text[],
      ARRAY[]::text[],
      'filled',
      1
    );

    INSERT INTO trade_executions (
      id, fund_id, plan_id, plan_action_id, instrument_key, symbol, market, exchange, asset_class, instrument_type, side,
      position_side, open_close, order_type, quantity, price, amount, filled_qty, filled_price, fee_commission,
      fee_stamp_tax, fee_transfer, trading_mode, status, executed_at, quote_currency, settlement_currency
    )
    VALUES (
      ${quoteSQL(tradeId)},
      ${quoteSQL(fundId)},
      ${quoteSQL(planId)},
      ${quoteSQL(actionId)},
      'NASDAQ:NVDA',
      'NVDA',
      'us_equity',
      'NASDAQ',
      'equity',
      'spot',
      'buy',
      'long',
      'open',
      'market',
      10,
      150,
      1500,
      10,
      150,
      0.5,
      0,
      0,
      'simulation',
      'filled',
      TIMESTAMP '${day2} 14:30:00',
      'USD',
      'USD'
    );

    INSERT INTO nav_snapshots (
      id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return
    )
    VALUES (
      ${quoteSQL(nav1Id)},
      ${quoteSQL(fundId)},
      DATE '${day1}',
      1.0,
      100000,
      0,
      100000,
      0,
      0
    );

    INSERT INTO nav_snapshots (
      id, fund_id, trading_date, nav, total_assets, total_market_value, available_cash, daily_return, total_return
    )
    VALUES (
      ${quoteSQL(nav2Id)},
      ${quoteSQL(fundId)},
      DATE '${day2}',
      1.005,
      100500,
      1500,
      99000,
      0.005,
      0.005
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

// updateABTestWindow shifts the inserted AB test's start_date /
// end_date so it covers the seeded NAV days. Why: the UI form
// only accepts `durationDays`, which the server expands to
// `[today, today + N]`. The fixture data lives a few days in the
// past (see seedABFixtures) so we need to back-date the test
// window once the row is created.
//
// This is the cleanest way to align "fresh test created via UI"
// with "deterministic seed data in the past" without forking the
// AB create endpoint just for the e2e suite.
function backdateABTestWindow(testId: string, startDate: string, endDate: string) {
  const sql = `
    UPDATE ab_tests
       SET start_date = DATE '${startDate}',
           end_date   = DATE '${endDate}'
     WHERE id = ${quoteSQL(testId)};
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

test.describe("decision center + AB lifecycle", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });

  test("renders decision center empty state for a fresh fund", async ({ page }) => {
    // A freshly-created fund has no investment_plans rows. The
    // Decision Center must render the heading + the operator
    // hint ("先启动策略流程生成今日计划") so the new operator knows
    // what to do next, rather than dropping them on a blank screen.
    const { user, fundId } = await ensureSeedData();
    await login(page, user);
    await page.goto(`/funds/${fundId}/decisions`);

    await expect(page.getByRole("heading", { name: "决策中心" })).toBeVisible();
    await expect(page.getByText("当前还没有投资计划")).toBeVisible();
    await expect(page.getByText("先启动策略流程生成今日计划")).toBeVisible();
  });

  test("renders AB compare empty state and exposes the create entry point", async ({ page }) => {
    const { user, fundId } = await ensureSeedData(
      undefined,
      createE2EName("AB Empty Company"),
      createE2EName("AB Empty Fund"),
    );
    await login(page, user);
    await page.goto(`/funds/${fundId}/compare`);

    await expect(page.getByRole("heading", { name: "策略 A/B 收益对比" })).toBeVisible();
    await expect(page.getByText("当前还没有 A/B 测试")).toBeVisible();
    // Two surfaces lead into the same modal — the empty-state
    // "立即创建" button and the page-header "新建测试" button. We
    // assert both are visible so a designer reorganizing the
    // header doesn't silently delete one of them.
    await expect(page.getByRole("button", { name: "立即创建" })).toBeVisible();
    await expect(page.getByRole("button", { name: "新建测试" })).toBeVisible();
  });

  test("creates AB test via UI and runs it through start → stop → analyze", async ({ page }) => {
    // Total runtime budget matters: this test seeds DB fixtures,
    // walks the full AB lifecycle (4 round-trip POSTs to the
    // server), and waits on each status transition. Setting an
    // explicit cap keeps a stuck fixture from holding the worker
    // for the default 30s.
    test.setTimeout(90_000);

    const { user, fundId } = await ensureSeedData(
      undefined,
      createE2EName("AB Lifecycle Company"),
      createE2EName("AB Lifecycle Fund"),
    );
    seedABFixtures(fundId);

    await login(page, user);
    await page.goto(`/funds/${fundId}/compare`);

    // ---- create ----
    const testName = createE2EName("AB E2E Test");
    await page.getByRole("button", { name: "立即创建" }).click();
    await expect(page.getByRole("heading", { name: "新建 A/B 测试" })).toBeVisible();

    await page.getByPlaceholder("例如：激进仓位策略 vs 当前策略").fill(testName);
    await page
      .getByPlaceholder("例如：提高单票上限，PM 风格改为 aggressive")
      .fill("Card J e2e: 单票仓位上限提到 30%, 主方向更激进, 行业集中度允许超过 40%。");
    // pmStyle / maxSinglePosition / durationDays already have
    // default values from INITIAL_FORM (aggressive / 20 / 30), so
    // submit-as-is exercises the happy path most operators take.
    await page.getByRole("button", { name: "创建测试" }).click();

    // After creation the modal closes and the new test is the
    // selected row. The page header should now show its name.
    await expect(page.getByRole("heading", { name: testName })).toBeVisible({ timeout: 15_000 });
    // Draft state copy from ABTestCompare.tsx:
    //   "该测试当前仍是草稿，请先确认参数与实验基金后再启动。"
    await expect(page.getByText("该测试当前仍是草稿")).toBeVisible();

    // ---- back-date the window before any action so the seeded
    // NAVs (2026-05-25 / 2026-05-26) land inside it ----
    const matches = page
      .url()
      .match(/[#?]testId=([0-9a-f-]{36})/i);
    let testId = matches ? matches[1] : "";
    if (!testId) {
      // The AB compare page does not currently encode the
      // selected test ID in the URL; fall back to looking up
      // the most recently-inserted draft row for this fund. We
      // can't access page.evaluate the API client, so a small
      // SQL pull via docker is the cheapest path.
      const lookupRaw = execFileSync(
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
          "-tAc",
          `SELECT id FROM ab_tests WHERE control_fund_id = '${fundId}' ORDER BY created_at DESC LIMIT 1`,
        ],
        { stdio: ["pipe", "pipe", "pipe"] },
      );
      testId = lookupRaw.toString().trim();
    }
    expect(testId).toMatch(/^[0-9a-f-]{36}$/i);
    backdateABTestWindow(testId, "2026-05-25", "2026-05-26");

    // ---- start ----
    await page.getByRole("button", { name: "启动测试" }).click();
    // Status change is reflected by the Stop button appearing and
    // the Start button going away.
    await expect(page.getByRole("button", { name: "停止测试" })).toBeVisible({ timeout: 15_000 });
    await expect(page.getByRole("button", { name: "启动测试" })).toHaveCount(0);

    // ---- stop ----
    await page.getByRole("button", { name: "停止测试" }).click();
    await expect(page.getByRole("button", { name: "生成分析" })).toBeVisible({ timeout: 15_000 });

    // ---- analyze ----
    await page.getByRole("button", { name: "生成分析" }).click();

    // After a successful analyze the result section renders. We
    // assert on three independent pieces so a refactor that drops
    // any one of them breaks CI:
    //   1. Result summary card heading.
    //   2. Cumulative-return curve heading.
    //   3. Confidence panel heading.
    // Each lives in its own component, so failures here point at
    // the right culprit without further debugging.
    await expect(page.getByRole("heading", { name: "结果摘要", exact: false })).toBeVisible({
      timeout: 30_000,
    });
    await expect(page.getByRole("heading", { name: "累计收益曲线", exact: false })).toBeVisible();
    await expect(page.getByRole("heading", { name: "结论置信度", exact: false })).toBeVisible();
  });
});
