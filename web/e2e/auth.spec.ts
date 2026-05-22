import { expect, test } from "@playwright/test";
import { createE2EUser, login, register, setAppPreferences } from "./helpers";

test.describe("auth flow", () => {
  test.beforeEach(async ({ page }) => {
    await setAppPreferences(page);
  });
  test("redirects unauthenticated users to login", async ({ page }) => {
    await page.goto("/companies");
    await expect(page).toHaveURL(/\/login$/);
    await expect(page.getByRole("heading", { name: "登录控制台" })).toBeVisible();
  });

  test("shows validation for invalid email", async ({ page }) => {
    await page.goto("/login");
    await page.getByLabel("邮箱").fill("not-an-email");
    await page.getByPlaceholder("请输入至少 8 位密码").fill("Passw0rd!");
    await page.getByRole("button", { name: "登录并进入系统" }).click();
    await expect(page.getByText("请输入合法的邮箱地址。")).toBeVisible();
  });

  test("shows validation for mismatched passwords during registration", async ({ page }) => {
    const user = createE2EUser("mismatch");
    await page.goto("/login");
    await page.getByRole("button", { name: "注册" }).click();
    await page.getByLabel("邮箱").fill(user.email);
    await page.getByLabel("显示名称").fill(user.displayName);
    await page.getByPlaceholder("请输入至少 8 位密码").fill(user.password);
    await page.getByPlaceholder("请再次输入密码").fill("DifferentPass1!");
    await page.getByRole("button", { name: "注册并进入系统" }).click();
    await expect(page.getByText("两次输入的密码不一致。")).toBeVisible();
  });

  test("registers first user and shows company onboarding", async ({ page }) => {
    const user = createE2EUser("first-user");
    await register(page, user);
    await expect(page.getByRole("heading", { name: "公司列表" })).toBeVisible();
    await expect(page.getByText(new RegExp(user.displayName))).toBeVisible();
    await expect(page.getByRole("button", { name: "创建第一家公司" })).toBeVisible();
  });

  test("logs out and logs back in with email and password", async ({ page }) => {
    const user = createE2EUser("login-again");
    await register(page, user);
    await page.getByRole("button", { name: "退出登录" }).click();
    await expect(page).toHaveURL(/\/login$/);
    await login(page, user);
    await expect(page.getByRole("heading", { name: "公司列表" })).toBeVisible();
  });
});
