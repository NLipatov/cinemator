import { test, expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

async function openFailedDownload(page) {
  await openPage(page, "/", "en");
  const download = {
    id: "failed",
    title: "Failed video",
    magnet: "magnet:?xt=urn:btih:0123456789012345678901234567890123456789",
    status: "failed",
    size: 1024,
    selectedFileIndex: 0,
    files: [{ index: 0, name: "Video.mkv", size: 1024 }],
    preparationError: "Decoder failed\n<img src=x onerror=alert(1)>\n" + "x".repeat(400),
  };
  await page.route(/\/api\/downloads(?:\?|$)/, (route) =>
    route.fulfill({
      json: [download, { id: "ready", title: "Ready video", status: "ready", size: 1024 }],
    }),
  );
  await page.route("**/api/hls/prepare?*", (route) => route.fulfill({ status: 204 }));
  await page.route("**/api/media/info?*", (route) =>
    route.fulfill({ json: { audioTracks: [{ index: 0 }, { index: 1 }], subtitles: [] } }),
  );
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator('.download-row[data-id="failed"]')).toBeVisible();
  return download;
}

for (const input of ["touch", "keyboard"]) {
  test.describe(`${input} preparation diagnostics`, () => {
    test.use({ hasTouch: input === "touch", viewport: { width: 320, height: 900 } });

    async function openDetails(summary, page, precedingControl) {
      if (input === "touch") {
        await summary.tap();
      } else {
        await precedingControl.focus();
        await page.keyboard.press("Tab");
        await expect(summary).toBeFocused();
        await page.keyboard.press("Enter");
      }
    }

    test("download disclosure survives refresh and language changes without opening the video", async ({
      page,
    }) => {
      const download = await openFailedDownload(page);
      const row = page.locator('.download-row[data-id="failed"]');
      const summary = row.locator("summary");
      const diagnostic = row.locator("pre");
      await expect(summary).toBeVisible();
      await expect(diagnostic).toBeHidden();
      await openDetails(summary, page, row.locator('[data-action="delete"]'));
      await expect(diagnostic).toBeVisible();
      expect(await diagnostic.textContent()).toBe(download.preparationError);
      await expect(row.locator("img")).toHaveCount(0);
      await expect(page.locator("#step-files")).toBeHidden();
      expect(await summary.evaluate((element) => !!element.closest("button"))).toBe(false);

      await page.locator("#languageSelect").selectOption("ru");
      await expect(summary).toHaveText("Подробности");
      await expect(diagnostic).toBeVisible();
      await summary.focus();
      download.title = "Refreshed video";
      download.preparationError += "\nUpdated diagnostic";
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      await expect(row.locator(".download-title")).toHaveText(download.title);
      await expect(summary).toBeFocused();
      await expect(diagnostic).toBeVisible();
      expect(await diagnostic.textContent()).toBe(download.preparationError);

      for (const fontSize of [16, 32]) {
        await page.evaluate(
          (size) => (document.documentElement.style.fontSize = `${size}px`),
          fontSize,
        );
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
          true,
        );
        const positions = await page
          .locator(".download-action.delete")
          .evaluateAll((buttons) => buttons.map((button) => button.getBoundingClientRect().left));
        expect(Math.abs(positions[0] - positions[1])).toBeLessThanOrEqual(1);
      }

      download.preparationError = "";
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      await expect(row.locator("details")).toHaveCount(0);
    });

    test("selected preparation exposes diagnostics and removes them when preparation restarts", async ({
      page,
    }) => {
      const download = await openFailedDownload(page);
      await page.locator('.download-row[data-id="failed"] .download-open').click();
      await page.locator("#selectTracks").click();
      await expect(page.locator("#step-tracks")).toBeVisible();
      const warning = page.locator("#warnMsg");
      const summary = warning.locator("summary");
      const diagnostic = warning.locator("pre");
      await expect(page.locator("#preparationDetails")).toHaveText("Could not prepare video");
      await expect(summary).toBeVisible();
      await openDetails(summary, page, page.locator("#play"));
      await expect(diagnostic).toBeVisible();
      expect(await diagnostic.textContent()).toBe(download.preparationError);
      await expect(warning.locator("img")).toHaveCount(0);

      await page.locator("#languageSelect").selectOption("zh-Hant");
      await expect(summary).toHaveText("詳細資訊");
      await expect(diagnostic).toBeVisible();
      download.title = "Refreshed preparation";
      await page.evaluate(() => window.dispatchEvent(new Event("focus")));
      await expect(page.locator('.download-row[data-id="failed"] .download-title')).toHaveText(
        download.title,
      );
      await expect(diagnostic).toBeVisible();
      await page.evaluate(() => (document.documentElement.style.fontSize = "32px"));
      expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(
        true,
      );

      download.status = "preparing";
      download.preparationError = "";
      await page.locator("#selectTracks").click();
      await expect(warning.locator("details")).toHaveCount(0);
      await expect(page.locator("#preparationDetails")).toBeEmpty();
    });
  });
}

for (const source of ["HLS", "native media"]) {
  test(`${source} playback diagnostic is operable and cleared when playback recovers`, async ({
    page,
  }) => {
    // Exercise the player's event wiring without fetching media or loading the HLS library.
    await page.addInitScript(() => {
      window.Hls = class {
        static Events = { ERROR: "error", FRAG_LOADED: "fragment", MANIFEST_PARSED: "manifest" };
        static isSupported() {
          return true;
        }
        constructor() {
          window.testHls = this;
          this.handlers = new Map();
        }
        on(event, handler) {
          this.handlers.set(event, handler);
        }
        loadSource() {}
        attachMedia() {}
        destroy() {}
      };
    });
    await openFailedDownload(page);
    await page.locator('.download-row[data-id="failed"] .download-open').click();
    await page.locator("#selectTracks").click();
    await page.locator("#play").click();
    await expect(page.locator("#player-block")).toBeVisible();
    const text = source === "HLS" ? "manifestLoadError" : "Native media error";
    await page.evaluate((source) => {
      if (source === "HLS")
        window.testHls.handlers.get("error")("error", {
          fatal: true,
          details: "manifestLoadError",
        });
      else document.querySelector("#video").dispatchEvent(new Event("error"));
    }, source);
    const message = page.locator("#playerMsg");
    const summary = message.locator("summary");
    await expect(message.locator('[data-i18n="player.playbackError"]')).toBeVisible();
    await expect(summary).toBeVisible();
    await summary.focus();
    await page.keyboard.press("Enter");
    await expect(message.locator("pre")).toBeVisible();
    await expect(message.locator("pre")).toHaveText(text);
    await page.locator("#languageSelect").selectOption("ru");
    await expect(summary).toHaveText("Подробности");
    await expect(message.locator("pre")).toBeVisible();
    await page.evaluate(() => window.testHls.handlers.get("fragment")());
    await expect(message).toBeEmpty();
    await expect(message).not.toHaveAttribute("title");
  });
}
