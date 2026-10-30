import { test, expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

test("adding a download preserves drafts and keeps the selected video independent of the form", async ({ page }) => {
  await openPage(page, "/", "ru");
  const add = page.locator("#addDownloadBtn");
  const panel = page.locator("#sourcePanel");
  const magnet = page.locator("#magnet");
  const source = "magnet:?xt=urn:btih:0123456789012345678901234567890123456789";
  await expect(panel).toBeHidden();
  await expect(add).toHaveAttribute("aria-expanded", "false");
  await add.focus();
  await add.press("Enter");
  await expect(panel).toBeVisible();
  await expect(add).toHaveAttribute("aria-expanded", "true");
  await expect(magnet).toBeFocused();
  await magnet.fill(source);
  await magnet.press("Escape");
  await expect(panel).toBeHidden();
  await expect(add).toBeFocused();
  await add.click();
  await expect(magnet).toHaveValue(source);

  let requestedSource;
  await page.route("**/api/torrent/files?*", route => {
    requestedSource = new URL(route.request().url()).searchParams.get("magnet");
    return route.fulfill({ json: [{ index: 0, name: "Video.mkv", size: 1024 }] });
  });
  await magnet.press("Enter");
  await expect(page.locator("#step-files")).toBeVisible();
  expect(requestedSource).toBe(source);
  await expect(panel).toBeHidden();
  await expect(add).toHaveAttribute("aria-expanded", "false");
  await expect(magnet).toHaveValue("");
  await expect(page.locator("#filelist")).toBeFocused();

  await add.click();
  await magnet.fill("magnet:new-draft");
  await add.click();
  let preparedSource;
  await page.route("**/api/hls/prepare?*", route => {
    preparedSource = new URL(route.request().url()).searchParams.get("magnet");
    return route.fulfill({ status: 204 });
  });
  await page.route("**/api/media/info?*", route => route.fulfill({
    json: { audioTracks: [{ index: 0 }, { index: 1 }], subtitles: [] },
  }));
  await page.locator("#selectTracks").click();
  await expect(page.locator("#step-tracks")).toBeVisible();
  expect(preparedSource).toBe(source);
  await add.click();
  await expect(magnet).toHaveValue("magnet:new-draft");
  await page.locator("#languageSelect").selectOption("en");
  await expect(add).toHaveText("Close");
  await magnet.press("Escape");
  await expect(add).toHaveText("Add");
});

test("an empty library still offers adding a download", async ({ page }) => {
  await openPage(page, "/", "en");
  await page.route("**/api/downloads", route => route.fulfill({ json: [] }));
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator(".downloads-empty")).toBeVisible();
  await expect(page.locator("#sourcePanel")).toBeHidden();
  await page.locator("#addDownloadBtn").click();
  await expect(page.locator("#magnet")).toBeFocused();
});

test("a completed lookup does not erase a newer draft", async ({ page }) => {
  await openPage(page, "/", "en");
  let release;
  const response = new Promise(resolve => { release = resolve; });
  await page.route("**/api/torrent/files?*", async route => {
    await response;
    await route.fulfill({ json: [{ index: 0, name: "Video.mkv", size: 1024 }] });
  });
  await page.locator("#addDownloadBtn").click();
  await page.locator("#magnet").fill("magnet:first");
  await page.locator("#nextBtn").click();
  try {
    await expect(page.locator('[data-i18n="player.loadingFiles"]')).toBeVisible();
    await page.locator("#magnet").fill("magnet:next-draft");
  } finally {
    release();
  }
  await expect(page.locator("#step-files")).toBeVisible();
  await expect(page.locator("#sourcePanel")).toBeVisible();
  await expect(page.locator("#magnet")).toHaveValue("magnet:next-draft");
});

const expiryDays = [26000, 60, 7, 10, 21, 5 / 24, 10 / (24 * 60), -730, null];
for (const [language, labels] of [
  ["ru", ["До автоудаления: 71 год", "До автоудаления: 2 месяца", "До автоудаления: 1 неделя",
    "До автоудаления: 1 неделя и 3 дня", "До автоудаления: 3 недели",
    "До автоудаления: 5 ч", "До автоудаления: 10 мин", "Срок хранения истёк 2 года назад", "Без автоудаления"]],
  ["en", ["Auto-delete in 71 years", "Auto-delete in 2 months", "Auto-delete in 1 week",
    "Auto-delete in 1 week and 3 days", "Auto-delete in 3 weeks",
    "Auto-delete in 5h", "Auto-delete in 10m", "Storage period ended 2 years ago", "No automatic deletion"]],
  ["zh-Hans", ["71 年后自动删除", "2 个月后自动删除", "1 周后自动删除",
    "1 周 3 天后自动删除", "3 周后自动删除",
    "5 小时后自动删除", "10 分钟后自动删除", "保留期限已于2 年前到期", "不会自动删除"]],
  ["zh-Hant", ["71 年後自動刪除", "2 個月後自動刪除", "1 週後自動刪除",
    "1 週 3 天後自動刪除", "3 週後自動刪除",
    "5 小時後自動刪除", "10 分鐘後自動刪除", "保留期限已於2 年前到期", "不會自動刪除"]],
]) {
  test(`${language}: expiry uses human-readable units from minutes to years`, async ({ page }) => {
    const now = new Date("2026-01-01T00:00:00Z");
    await page.clock.setFixedTime(now);
    await openPage(page, "/", language);
    await page.route("**/api/downloads", route => route.fulfill({
      json: expiryDays.map((days, i) => ({
        id: `expiry-${i}`, title: `Video ${i}`, status: "ready", size: 1024,
        expiresAt: days === null ? "" : new Date(+now + days * 86400000).toISOString(),
      })),
    }));
    await page.evaluate(() => window.dispatchEvent(new Event("focus")));
    await expect(page.locator(".download-meta-text")).toHaveText(labels);
  });
}

test("Russian years and months use the correct plural forms", async ({ page }) => {
  const now = new Date("2026-01-01T00:00:00Z");
  await page.clock.setFixedTime(now);
  await openPage(page, "/", "ru");
  await page.route("**/api/downloads", route => route.fulfill({
    json: [365, 730, 1825, 7665, 30, 60, 150].map((days, i) => ({
      id: `plural-${i}`, title: `Video ${i}`, status: "ready", size: 1024,
      expiresAt: new Date(+now + days * 86400000).toISOString(),
    })),
  }));
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator(".download-meta-text")).toHaveText([
    "До автоудаления: 1 год", "До автоудаления: 2 года", "До автоудаления: 5 лет",
    "До автоудаления: 21 год", "До автоудаления: 1 месяц", "До автоудаления: 2 месяца",
    "До автоудаления: 5 месяцев",
  ]);
});

test("weeks retain remaining days and handle rounding and expired durations", async ({ page }) => {
  const now = new Date("2026-01-01T00:00:00Z");
  await page.clock.setFixedTime(now);
  await openPage(page, "/", "ru");
  const days = [1, 2, 5, 6, 6.99, 7, 8, 9, 12, 13, 13.99, 14, 21, 22, 29, 30, -7, -10];
  await page.route("**/api/downloads", route => route.fulfill({
    json: days.map((days, i) => ({
      id: `weeks-${i}`, title: `Video ${i}`, status: "ready", size: 1024,
      expiresAt: new Date(+now + days * 86400000).toISOString(),
    })),
  }));
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator(".download-meta-text")).toHaveText([
    "До автоудаления: 1 день", "До автоудаления: 2 дня", "До автоудаления: 5 дней",
    "До автоудаления: 6 дней", "До автоудаления: 1 неделя", "До автоудаления: 1 неделя",
    "До автоудаления: 1 неделя и 1 день", "До автоудаления: 1 неделя и 2 дня",
    "До автоудаления: 1 неделя и 5 дней", "До автоудаления: 1 неделя и 6 дней",
    "До автоудаления: 2 недели", "До автоудаления: 2 недели", "До автоудаления: 3 недели",
    "До автоудаления: 3 недели и 1 день", "До автоудаления: 4 недели и 1 день",
    "До автоудаления: 1 месяц", "Срок хранения истёк 1 неделю назад", "Срок хранения истёк 1 неделю и 3 дня назад",
  ]);
});
