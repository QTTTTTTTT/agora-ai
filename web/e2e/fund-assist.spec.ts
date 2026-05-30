import { expect, test } from "@playwright/test";
import { ensureCompanyOnly, login, setAppPreferences } from "./helpers";

// Smoke tests for the AI-assisted fund creation flow
// (FundAssistDialog). We don't need a real LLM key here — the
// /api/companies/{id}/funds:assist endpoint is mocked at the
// network layer with Playwright's route().fulfill() so the test:
//   - is fast and offline-safe (CI doesn't need DEEPSEEK_API_KEY)
//   - exercises the front-end state machine deterministically
//     (preview → plan card → confirm → navigate)
//   - covers the 422 plan_rejected error UI explicitly
//
// The browser still talks to a real backend for everything *else*
// (login, list companies). Only the assist endpoint is intercepted.
//
// We skip the manual "register + create company via UI" dance and
// use the existing ensureCompanyOnly() helper which seeds via the
// API directly. That keeps the test tight (just opens the dialog
// and asserts on the assist UI states) and avoids re-testing the
// onboarding flow that companies.spec already covers.

test.describe("fund assist dialog", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });

  test("opens dialog, previews plan, then commits and navigates to the new fund", async ({ page }) => {
    const { user } = await ensureCompanyOnly();

    // Mock the assist endpoint: first call (dryRun=true) returns
    // a plan; second call (dryRun=false) returns a created fund.
    let calls = 0;
    await page.route("**/api/companies/*/funds:assist", async (route) => {
      const body = JSON.parse(route.request().postData() || "{}") as { dryRun?: boolean };
      calls += 1;
      if (body.dryRun) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify({
            plan: {
              fund: {
                name: "美股 AI 主题基金",
                description: "AI 跟踪 NVDA / AMD / AVGO 的轻仓策略",
                market: "us_equity",
                baseCurrency: "USD",
                initialCapital: 1_000_000,
                primaryDirection: "long",
                assetClass: "equity",
                universe: { mode: "explicit", symbols: ["NVDA", "AMD", "AVGO"] },
                specialization: { markets: ["us_equity"] },
              },
              agents: [
                { role: "pm", name: "Portfolio Manager" },
                { role: "researcher", name: "NVDA Research", focus: "NVDA" },
                { role: "trader", name: "Trader" },
              ],
              rationale: "美股 AI 主题，3 名核心成员",
            },
            warnings: [],
          }),
        });
        return;
      }
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          fundId: "fund-assist-001",
          fund: { id: "fund-assist-001", name: "美股 AI 主题基金", market: "us_equity" },
          agents: [{ id: "ag-pm", role: "pm" }],
          plan: { fund: { name: "美股 AI 主题基金", market: "us_equity" }, agents: [] },
        }),
      });
    });

    await login(page, user);
    // ensureCompanyOnly seeded a company with no funds → the
    // empty-state CTA path renders the "AI 辅助创建" button
    // alongside "创建第一只基金".
    await page.getByRole("button", { name: "AI 辅助创建" }).first().click();
    await expect(page.getByRole("heading", { name: "AI 辅助创建基金" })).toBeVisible();

    await page.getByLabel("基金描述").fill("做一个美股 AI 基金，1 PM + 2 researcher + 1 trader");
    await page.getByRole("button", { name: /生成方案/ }).click();

    // Plan preview should appear.
    await expect(page.getByText("AI 推荐方案")).toBeVisible();
    await expect(page.getByText("美股 AI 主题基金")).toBeVisible();
    await expect(page.getByText(/us_equity/).first()).toBeVisible();
    await expect(page.getByText("Portfolio Manager")).toBeVisible();
    await expect(page.getByText("[NVDA]")).toBeVisible();

    // Commit.
    await page.getByRole("button", { name: /确认创建基金/ }).click();
    await expect(page).toHaveURL(/\/funds\/fund-assist-001/);
    expect(calls).toBe(2);
  });

  test("renders plan-rejected issues when server returns 422", async ({ page }) => {
    const { user } = await ensureCompanyOnly(undefined, "E2E Reject Co");

    await page.route("**/api/companies/*/funds:assist", async (route) => {
      await route.fulfill({
        status: 422,
        contentType: "application/json",
        body: JSON.stringify({
          error: "plan_rejected",
          detail: "LLM 输出的方案未通过校验",
          issues: [
            {
              field: "agents[1].focus",
              code: "market_mismatch",
              message: "研究员的标的 \"600519\" 看起来属于 a_share 市场，与基金市场 us_equity 不匹配",
            },
          ],
          plan: {
            fund: { name: "X", market: "us_equity" },
            agents: [
              { role: "pm" },
              { role: "researcher", focus: "600519" },
            ],
          },
          warnings: [],
        }),
      });
    });

    await login(page, user);
    await page.getByRole("button", { name: "AI 辅助创建" }).first().click();
    await page.getByLabel("基金描述").fill("做美股基金 但是给我一个 A 股研究员（故意写错）");
    await page.getByRole("button", { name: /生成方案/ }).click();

    await expect(page.getByText(/AI 输出的方案未通过校验/).first()).toBeVisible();
    await expect(page.getByText(/agents\[1\]\.focus/)).toBeVisible();
    await expect(page.getByText(/600519.*与基金市场 us_equity 不匹配/)).toBeVisible();
    // Confirm button must NOT appear when validation failed —
    // user has to refine the prompt and re-preview.
    await expect(page.getByRole("button", { name: /确认创建基金/ })).toHaveCount(0);
  });
});
