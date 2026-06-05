// e2e/visual-baseline.spec.ts — visual regression baselines for the
// auth-front routes.
//
// WHY THIS FILE EXISTS
// --------------------
// The production-readiness review (U8) flagged "no automated check
// catches a regression in the auth/landing chrome — a stray padding
// change, an accidental Tailwind purge, or a router redirect that
// drops the marketing copy could ship without anyone noticing
// until a customer complained". This spec is the smallest useful
// floor: take pixel screenshots of the routes that DON'T need a
// seeded user / DB and compare them against committed baselines.
//
// SCOPE
// -----
// Only unauthenticated routes — `/login` (default + register tab),
// `/forgot-password`, `/reset-password`. These are deterministic
// (no timestamps, no API data, no animations) so they make stable
// baselines without elaborate masking.
//
// Authenticated route baselines (companies list, marketplace, fund
// dashboards) are explicitly out of scope here because they depend
// on seeded API state which is non-deterministic across runs.
// Adding those needs (a) a deterministic seed harness, (b) date /
// money mask helpers — see the file's "Future work" footer.
//
// HOW TO UPDATE BASELINES
// -----------------------
// When a real visual change is intended:
//
//   cd web && npx playwright test e2e/visual-baseline.spec.ts --update-snapshots
//
// Then `git add web/e2e/visual-baseline.spec.ts-snapshots/*.png`
// alongside the source change in the same commit so reviewers see
// the diff in both the code and the baseline.
//
// HOW IT FAILS IN CI
// ------------------
// Pixel-perfect matching is intentionally avoided — Playwright's
// `maxDiffPixelRatio: 0.01` lets us tolerate rendering jitter on
// font / GPU differences across host machines. A real regression
// (button removed, layout shift) easily clears that 1% ceiling.
// If a baseline drift is unexpectedly small but persistent, raise
// the ratio rather than suppress; the goal is to catch outright
// breakage, not to pin everything down.

import { expect, test } from "@playwright/test";

test.describe("visual baseline — unauthenticated routes", () => {
  // Hold the viewport stable across runs so the baselines are
  // reproducible. The Desktop Chrome project default is 1280x720
  // already, but pinning here documents the contract and shields
  // us from a project-config tweak later that would silently
  // invalidate every baseline at once.
  test.use({ viewport: { width: 1280, height: 800 } });

  test.beforeEach(async ({ page }) => {
    // Disable CSS transitions / animations so screenshots are
    // taken on a fully-settled DOM.
    await page.addStyleTag({
      content: `
        *, *::before, *::after {
          animation: none !important;
          transition: none !important;
          caret-color: transparent !important;
        }
      `,
    });
  });

  test("/login — default sign-in form", async ({ page }) => {
    await page.goto("/login");
    // Wait for the heading we know the login page renders before
    // we take the shot — without this the screenshot can race the
    // initial render and capture a blank skeleton.
    await expect(
      page.getByRole("heading", { name: "登录控制台" }),
    ).toBeVisible();
    await expect(page).toHaveScreenshot("login-default.png", {
      fullPage: true,
      maxDiffPixelRatio: 0.01,
    });
  });

  test("/login — register form variant", async ({ page }) => {
    await page.goto("/login");
    await expect(
      page.getByRole("heading", { name: "登录控制台" }),
    ).toBeVisible();
    await page.getByRole("button", { name: "注册" }).click();
    // The register form has its own submit button — wait for it
    // before snapshotting so we don't capture a half-swap.
    await expect(
      page.getByRole("button", { name: "注册并进入系统" }),
    ).toBeVisible();
    await expect(page).toHaveScreenshot("login-register.png", {
      fullPage: true,
      maxDiffPixelRatio: 0.01,
    });
  });

  test("/forgot-password — request reset link", async ({ page }) => {
    await page.goto("/forgot-password");
    // Either there's a heading we can wait on, or we wait for any
    // form to be visible. Try heading first; fall back to the
    // submit button so this test still asserts a settled DOM if
    // copy changes.
    const headingByName = page.getByRole("heading", {
      name: /忘记密码|Reset password/i,
    });
    if (await headingByName.isVisible().catch(() => false)) {
      await expect(headingByName).toBeVisible();
    } else {
      await expect(page.locator("form").first()).toBeVisible();
    }
    await expect(page).toHaveScreenshot("forgot-password.png", {
      fullPage: true,
      maxDiffPixelRatio: 0.01,
    });
  });

  test("/reset-password — token entry / new password form", async ({
    page,
  }) => {
    // Real flow needs a ?token=… query param; without one the
    // page typically renders a "missing token" empty state. Both
    // are useful baselines — we capture the empty-state since
    // that's the deterministic surface.
    await page.goto("/reset-password");
    await expect(page.locator("body")).toBeVisible();
    // Give the page a moment to render its terminal state (either
    // form or empty-state copy) before snapshotting.
    await page.waitForLoadState("networkidle");
    await expect(page).toHaveScreenshot("reset-password.png", {
      fullPage: true,
      maxDiffPixelRatio: 0.01,
    });
  });
});

// Future work:
//
//   - Authenticated baselines for /companies, /marketplace, and a
//     fund dashboard, fronted by a deterministic seed harness so
//     timestamps / IDs / money values render identically across
//     runs (or are masked with `mask:` selectors).
//   - Locale variant baselines (zh-CN vs en-US) once react-i18next
//     migration is in place — the auth pages currently mix Chinese
//     literals so a per-locale screenshot pair would catch
//     translation drift.
//   - Mobile viewport baselines (390x844 iPhone 14) — separate
//     project in playwright.config.ts so it doesn't double-flake
//     desktop.
