import { test, expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

test.describe("touch error diagnostics", () => {
  test.use({ hasTouch: true, viewport: { width: 320, height: 900 } });

  test("uncoded diagnostics can be tapped, wrap, and survive language changes as plain text", async ({
    page,
  }) => {
    await openPage(page, "/", "ru");
    await page.locator("#addDownloadBtn").tap();
    const diagnostic =
      'Invalid magnet: missing hash\n<img src=x onerror="alert(1)">\n' + "x".repeat(400);
    await page.route("**/api/torrent/files?*", (route) =>
      route.fulfill({ status: 400, body: diagnostic }),
    );
    await page.locator("#magnet").fill("invalid-magnet");
    await page.locator("#nextBtn").tap();
    const message = page.locator("#magnetMsg");
    await expect(message.locator('[data-i18n="errors.files"]')).toBeVisible();
    const summary = message.locator("summary");
    const detail = message.locator("pre");
    await expect(summary).toHaveText("Подробности");
    await expect(detail).toBeHidden();
    await summary.tap();
    await expect(detail).toBeVisible();
    expect(await detail.textContent()).toBe(diagnostic);
    await expect(message.locator("img")).toHaveCount(0);
    for (const fontSize of [16, 32]) {
      await page.evaluate(
        (size) => (document.documentElement.style.fontSize = `${size}px`),
        fontSize,
      );
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
        true,
      );
      expect(
        await detail.evaluate((element) => element.scrollWidth <= element.clientWidth + 1),
      ).toBe(true);
    }
    for (const [language, label] of [
      ["en", "Details"],
      ["zh-Hans", "详细信息"],
      ["zh-Hant", "詳細資訊"],
    ]) {
      await page.locator("#languageSelect").selectOption(language);
      await expect(summary).toHaveText(label);
      await expect(detail).toBeVisible();
      expect(await detail.textContent()).toBe(diagnostic);
    }
  });
});

for (const [code, key] of [
  ["unsupported_magnet", "errors.unsupported_magnet"],
  ["future_error", "errors.files"],
]) {
  test(`${code}: diagnostic disclosure is keyboard accessible`, async ({ page }) => {
    await openPage(page, "/", "en");
    await page.locator("#addDownloadBtn").click();
    await page.route("**/api/torrent/files?*", (route) =>
      route.fulfill({
        status: 400,
        headers: { "X-Error-Code": code },
        body: "Server diagnostic",
      }),
    );
    await page.locator("#magnet").fill("invalid-magnet");
    await page.locator("#nextBtn").click();
    const message = page.locator("#magnetMsg");
    await expect(message.locator(`[data-i18n="${key}"]`)).toBeVisible();
    const summary = message.locator("summary");
    const detail = message.locator("pre");
    await page.locator("#nextBtn").focus();
    await page.keyboard.press("Tab");
    await expect(summary).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(detail).toBeVisible();
    await expect(detail).toHaveText("Server diagnostic");
    await page.keyboard.press("Space");
    await expect(detail).toBeHidden();
  });
}

test("retry clears diagnostics and empty responses do not leave an empty disclosure", async ({
  page,
}) => {
  await openPage(page, "/", "en");
  await page.locator("#addDownloadBtn").click();
  await page.route("**/api/torrent/files?*", (route) =>
    route.fulfill({ status: 400, body: "Invalid magnet" }),
  );
  await page.locator("#magnet").fill("invalid-magnet");
  await page.locator("#nextBtn").click();
  const message = page.locator("#magnetMsg");
  await expect(message.locator("summary")).toBeVisible();

  let release;
  const response = new Promise((resolve) => {
    release = resolve;
  });
  await page.route(
    "**/api/torrent/files?*",
    async (route) => {
      await response;
      await route.fulfill({ status: 500, body: "" });
    },
    { times: 1 },
  );
  await page.locator("#nextBtn").click();
  try {
    await expect(message.locator('[data-i18n="player.loadingFiles"]')).toBeVisible();
    await expect(message.locator("details")).toHaveCount(0);
  } finally {
    release();
  }
  await expect(message.locator('[data-i18n="errors.files"]')).toBeVisible();
  await expect(message.locator("details")).toHaveCount(0);

  await page.route("**/api/torrent/files?*", (route) =>
    route.fulfill({
      json: [{ index: 0, name: "Video.mkv", size: 1024 }],
    }),
  );
  await page.locator("#nextBtn").click();
  await expect(page.locator("#step-files")).toBeVisible();
  await expect(message).toBeEmpty();
});
