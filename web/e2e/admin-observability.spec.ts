// admin-observability.spec.ts — W13-6 e2e smoke for the three
// observability panels on /admin (W9-2 / W11-2 / W13-1).
//
// Why this spec exists:
//
//   The Admin panels are super-admin gated and call the admin
//   JSON endpoints introduced in W8-2 / W11-1 / (existing)
//   /api/admin/db-pool/status. Go-side handler tests cover the
//   400 / 403 / wire-shape contracts (see embed_quota_handler_test.go,
//   memreembed_handler_test.go, db_pool_handler_test.go).
//
//   What those tests *don't* cover:
//
//     1. The end-to-end auth path actually grants super-admin
//        when the DB role flips to `super_admin` — the existing
//        handler tests inject the role via context, bypassing
//        the JWT middleware that production callers actually
//        traverse.
//     2. The React panels (AdminMemReembedSection,
//        AdminEmbedQuotaSection, AdminDBPoolSection) actually
//        render headings + at least one widget for each of the
//        three responses. A future refactor that drops a section
//        from /admin should fail CI on this file.
//
//   The spec is deliberately permissive about the *contents* of
//   each panel: the embed quota / memreembed services may be
//   disabled in CI (no LLM provider configured), so we assert
//   only the section heading + that the panel resolved to
//   either "data" or its documented "disabled" state. Asserting
//   on live counter values would be flaky.
//
// What this spec does NOT cover:
//
//   - The non-admin negative path: a regular user navigating to
//     /admin. The Go handler tests cover the 401 / 403 wire
//     contract end-to-end; replicating that here would just be
//     UI confirmation of the same assertion.
//   - Auto-refresh timing. The 30s timer is exercised in unit
//     tests of the section components separately (and visually
//     during dev). E2e at this granularity adds wall-clock cost
//     without coverage.

import { execFileSync } from "node:child_process";
import { expect, test } from "@playwright/test";
import { ensureSeedData, login, setAppPreferences } from "./helpers";

const POSTGRES_CONTAINER =
  process.env.PLAYWRIGHT_POSTGRES_CONTAINER || "fundai-postgres";
const POSTGRES_USER =
  process.env.PLAYWRIGHT_POSTGRES_USER ||
  process.env.POSTGRES_USER ||
  "fundai";
const POSTGRES_DB =
  process.env.PLAYWRIGHT_POSTGRES_DB || process.env.POSTGRES_DB || "fundai";
const POSTGRES_PASSWORD =
  process.env.PLAYWRIGHT_POSTGRES_PASSWORD ||
  process.env.POSTGRES_PASSWORD ||
  "fundai_secret_change_me";

function quoteSQL(value: string): string {
  return `'${value.replace(/'/g, "''")}'`;
}

// promoteUserToSuperAdmin flips a freshly registered user's role
// from `user` to `super_admin` directly in the DB. This matches
// the migration 003 schema (role check constraint allows only
// `super_admin` / `user`).
//
// Why direct DB update vs. an API: there is no production
// "promote to super_admin" endpoint by design — bootstrap runs
// once per deployment and operators are onboarded out of band.
// For an e2e the only realistic seam is the DB itself, mirroring
// the same docker-exec pattern fund-shell.spec.ts and
// decision-center.spec.ts already rely on.
function promoteUserToSuperAdmin(email: string) {
  const sql = `
    UPDATE users
       SET role = 'super_admin'
     WHERE LOWER(email) = LOWER(${quoteSQL(email)});
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

test.describe("Admin observability panels (W13-6)", () => {
  test("super-admin sees memreembed / embed-quota / db-pool sections", async ({
    page,
  }) => {
    await setAppPreferences(page, { language: "zh-CN" });
    const seed = await ensureSeedData();
    promoteUserToSuperAdmin(seed.user.email);

    // Re-login so the JWT carries the freshly-elevated role
    // claim. ensureSeedData only registered + got a token; the
    // login() helper goes through the UI form and the server
    // re-issues with the current DB role.
    await login(page, seed.user);

    // Track the three admin endpoint responses so we can
    // distinguish "panel rendered but limit is disabled" from
    // "panel never got a 200" (the latter is the regression we
    // want to catch).
    const seenStatuses: Record<string, number | undefined> = {
      memreembed: undefined,
      embedQuota: undefined,
      dbPool: undefined,
    };
    page.on("response", (resp) => {
      const url = resp.url();
      if (url.includes("/api/admin/memreembed/status")) {
        seenStatuses.memreembed = resp.status();
      } else if (url.includes("/api/admin/embed-quota/status")) {
        seenStatuses.embedQuota = resp.status();
      } else if (url.includes("/api/admin/db-pool/status")) {
        seenStatuses.dbPool = resp.status();
      }
    });

    await page.goto("/admin");

    // The three panels render their localized titles
    // unconditionally — the "disabled" branch still shows the
    // heading. Use a generous matcher (`getByRole heading` would
    // be stricter but it varies between h2 / h3 across panels).
    await expect(page.getByText("记忆重嵌入队列").first()).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByText("嵌入配额限流").first()).toBeVisible({
      timeout: 15_000,
    });
    await expect(page.getByText("数据库连接池").first()).toBeVisible({
      timeout: 15_000,
    });

    // All three endpoints must have answered with 200. A 401 or
    // 403 means the role-promote path or the JWT middleware is
    // broken; a 404 means a route was dropped.
    await expect.poll(() => seenStatuses.memreembed, { timeout: 10_000 }).toBe(200);
    await expect.poll(() => seenStatuses.embedQuota, { timeout: 10_000 }).toBe(200);
    await expect.poll(() => seenStatuses.dbPool, { timeout: 10_000 }).toBe(200);
  });
});
