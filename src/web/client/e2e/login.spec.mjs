import { test, expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

test.beforeEach(async ({ page }) => {
  await openPage(page, "/");
});

test("an external login link redirects an existing session home without starting QR sign-in", async ({ page, context }) => {
  await context.addCookies([{
    name: "__Host-cinemator_session",
    value: "existing-session",
    domain: "cinemator.test",
    path: "/",
    secure: true,
    httpOnly: true,
    sameSite: "Strict",
  }]);
  let signInRequests = 0;
  await page.route("**/api/auth/sign-in-requests", route => {
    signInRequests++;
    return route.fulfill({ status: 503 });
  });
  await page.route("**/api/auth/status", async route => {
    const headers = await route.request().allHeaders();
    return route.fulfill({ json: {
      enabled: true,
      authenticated: headers.cookie?.includes("__Host-cinemator_session=existing-session") ?? false,
    } });
  });
  await page.route("https://external.test/", route => route.fulfill({
    contentType: "text/html",
    body: '<a href="https://cinemator.test/login?next=/sign-in-approvals/test">Open Cinemator</a>',
  }));
  await page.goto("https://external.test/");
  const loginRequest = page.waitForRequest(request => new URL(request.url()).pathname === "/login");
  await page.getByRole("link", { name: "Open Cinemator" }).click();
  expect((await (await loginRequest).allHeaders()).cookie).toBeUndefined();
  await expect(page).toHaveURL("https://cinemator.test/");
  expect(signInRequests).toBe(0);
  await page.goBack();
  await expect(page).toHaveURL("https://external.test/");
});

test("disabled authentication redirects home", async ({ page }) => {
  await page.route("**/api/auth/status", route => route.fulfill({ json: { enabled: false } }));
  await page.goto("http://cinemator.test/login");
  await expect(page).toHaveURL("http://cinemator.test/");
});

for (const status of ["anonymous", "server error", "network error"]) {
  test(`${status}: password and QR sign-in remain available`, async ({ page }) => {
    await page.route("**/api/auth/status", route => {
      if (status === "server error") return route.fulfill({ status: 503 });
      if (status === "network error") return route.abort();
      return route.fulfill({ json: { enabled: true, authenticated: false } });
    });
    const signInRequest = page.waitForRequest("**/api/auth/sign-in-requests");
    await page.goto("http://cinemator.test/login");
    await signInRequest;
    await expect(page).toHaveURL("http://cinemator.test/login");
    await expect(page.locator("#password")).toBeVisible();
    await expect(page.locator("#loginButton")).toBeEnabled();
  });
}
