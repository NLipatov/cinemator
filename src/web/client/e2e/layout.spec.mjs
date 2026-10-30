import { test, expect } from "@playwright/test";
import { openPage as openClientPage } from "./fixtures.mjs";

async function openPage(page, path, language) {
  await openClientPage(page, path, language);
  if (path === "/") {
    await expect(page.locator(".download-row")).toHaveCount(2);
    await page.locator("#addDownloadBtn").click();
    // Expose track controls as they would appear after loading media metadata.
    await page.evaluate(() => {
      document.body.classList.add("has-player");
      document
        .querySelectorAll(".step, #subtitleDelayRow")
        .forEach((element) => (element.style.display = "grid"));
      document.querySelector('[data-action="toggle-extend"]').click();
    });
  }
}

for (const [languages, expected] of [
  [["zh-TW", "en-US"], "zh-Hant"],
  [["zh-Hant-CN", "ru"], "zh-Hant"],
  [["zh-Hans-TW", "en"], "zh-Hans"],
  [["zh-Latn", "en-US"], "en"],
]) {
  test(`browser preferences ${languages.join(", ")} select and persist ${expected}`, async ({
    page,
  }) => {
    await page.addInitScript((languages) => {
      Object.defineProperty(navigator, "languages", { value: languages });
    }, languages);
    await openPage(page, "/login");
    await expect(page.locator("html")).toHaveAttribute("lang", expected);
    await expect(page.locator("#languageSelect")).toHaveValue(expected);
    expect(await page.evaluate(() => localStorage.getItem("cinemator-language"))).toBe(expected);
  });
}

async function layoutProblems(page) {
  return page.evaluate(() => {
    const problems = [];
    if (document.documentElement.scrollWidth > innerWidth)
      problems.push("page overflows horizontally");
    for (const element of document.querySelectorAll(
      "button:not(.download-open), label, .field-label, .panel-title, .delay-value, .download-title, .download-meta-text, .download-size",
    )) {
      if (!element.getClientRects().length || !element.textContent.trim()) continue;
      const style = getComputedStyle(element),
        box = element.getBoundingClientRect();
      const left = box.left + parseFloat(style.borderLeftWidth) + parseFloat(style.paddingLeft);
      const right = box.right - parseFloat(style.borderRightWidth) - parseFloat(style.paddingRight);
      const range = document.createRange();
      range.selectNodeContents(element);
      // scrollWidth alone misses text that spills into a button's padding.
      if (
        [...range.getClientRects()].some((rect) => rect.left < left - 1 || rect.right > right + 1)
      ) {
        problems.push(`${element.id || element.className}: text exceeds its content box`);
      }
      if (box.left < -1 || box.right > innerWidth + 1)
        problems.push(`${element.id || element.className}: outside viewport`);
    }
    const logo = document.querySelector(".brand-name").getBoundingClientRect();
    const actions = document.querySelector(".top-actions").getBoundingClientRect();
    if (logo.right > actions.left && logo.bottom > actions.top && logo.top < actions.bottom)
      problems.push("header items overlap");
    if (
      document.querySelector("main").getBoundingClientRect().top <
      document.querySelector(".topbar").getBoundingClientRect().bottom
    )
      problems.push("header overlaps page content");
    const deletePositions = [...document.querySelectorAll(".download-action.delete")].map(
      (element) => element.getBoundingClientRect().left,
    );
    if (deletePositions.some((left) => Math.abs(left - deletePositions[0]) > 1))
      problems.push("delete buttons are misaligned");
    const source = document.querySelector(".source-panel")?.getBoundingClientRect();
    const downloads = document.querySelector(".downloads-panel")?.getBoundingClientRect();
    if (source && downloads && (Math.abs(source.left - downloads.left) > 1 ||
      Math.abs(source.right - downloads.right) > 1))
      problems.push("downloads do not share the source panel width");
    for (const element of document.querySelectorAll(".download-size, .download-meta-text")) {
      // Numeric values and their units must stay on the same visual line.
      for (const match of element.textContent.matchAll(/\d[\d.,\u00a0\u202f]*[\u00a0 ]+(?:[\p{Script=Latin}\p{Script=Cyrillic}]+|年|个月|個月|周|週|天|小时|小時|分钟|分鐘)/gu)) {
        const range = document.createRange();
        range.setStart(element.firstChild, match.index);
        range.setEnd(element.firstChild, match.index + match[0].length);
        const lines = [...range.getClientRects()];
        if (lines.some((rect) => Math.abs(rect.top - lines[0].top) > 1))
          problems.push(`${element.className}: value and unit wrap onto separate lines`);
      }
    }
    return problems;
  });
}

for (const language of ["en", "ru", "zh-Hans", "zh-Hant"]) {
  for (const path of ["/", "/login", "/sign-in-approvals/test"]) {
    test(`${language} ${path}: labels fit at different widths and font sizes`, async ({ page }) => {
      await openPage(page, path, language);
      for (const fontSize of [16, 32]) {
        await page.evaluate(
          (size) => (document.documentElement.style.fontSize = `${size}px`),
          fontSize,
        );
        for (const width of [320, 512, 513, 768, 1025, 1280]) {
          await page.setViewportSize({ width, height: 1000 });
          expect(
            await layoutProblems(page),
            `${language} ${path} ${width}px / ${fontSize}px font`,
          ).toEqual([]);
        }
      }
    });
  }
}

for (const [path, key, translated] of [
  ["/", "player.load", "開啟"],
  ["/login", "auth.continue", "登入"],
  ["/sign-in-approvals/test", "approval.allow", "允許登入"],
]) {
  test(`${path}: selecting Traditional Chinese updates the page and survives reload`, async ({
    page,
  }) => {
    await page.addInitScript(() => {
      Object.defineProperty(navigator, "languages", { value: ["en-US"] });
    });
    await openPage(page, path);
    await expect(page.locator("html")).toHaveAttribute("lang", "en");
    await page.locator("#languageSelect").selectOption("zh-Hant");
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-Hant");
    await expect(page.locator(`[data-i18n="${key}"]`)).toHaveText(translated);
    await page.reload();
    await expect(page.locator("html")).toHaveAttribute("lang", "zh-Hant");
    await expect(page.locator("#languageSelect")).toHaveValue("zh-Hant");
    await expect(page.locator(`[data-i18n="${key}"]`)).toHaveText(translated);
  });
}

for (const path of ["/", "/login", "/sign-in-approvals/test"]) {
  test(`${path}: language menu fits the viewport and supports keyboard selection`, async ({ page }) => {
    await openClientPage(page, path, "en");
    const select = page.locator("#languageSelect");
    for (const theme of ["light", "dark"]) {
      for (const fontSize of [16, 32]) {
        await page.evaluate(({ theme, fontSize }) => {
          document.documentElement.dataset.theme = theme;
          document.documentElement.style.fontSize = `${fontSize}px`;
        }, { theme, fontSize });
        for (const width of [320, 390, 1280]) {
          await page.setViewportSize({ width, height: 1000 });
          await select.click();
          const options = page.getByRole("option");
          await expect(options).toHaveCount(4);
          for (const option of await options.all()) {
            await expect(option).toBeVisible();
            const box = await option.boundingBox();
            expect(box.x).toBeGreaterThanOrEqual(0);
            expect(box.x + box.width).toBeLessThanOrEqual(width);
            expect(box.y + box.height).toBeLessThanOrEqual(1000);
            expect(await option.evaluate(el => el.scrollWidth <= el.clientWidth)).toBe(true);
          }
          await page.keyboard.press("Escape");
          await expect(select).toBeFocused();
        }
      }
    }
    await select.press("Space");
    await page.keyboard.press("ArrowDown");
    await page.keyboard.press("Enter");
    await expect(select).toHaveValue("ru");
    await expect(page.locator("html")).toHaveAttribute("lang", "ru");
    expect(await page.evaluate(() => localStorage.getItem("cinemator-language"))).toBe("ru");
  });
}

test("long and unbroken translated labels wrap without squeezing the magnet field away", async ({
  page,
}) => {
  await openPage(page, "/", "ru");
  for (const label of [
    "Загрузить выбранный торрент и показать доступные видео",
    "ОченьДлинноеСловоБезПробелов".repeat(6),
  ]) {
    await page.locator("#nextBtn").evaluate((element, text) => (element.textContent = text), label);
    for (const width of [320, 513, 768, 1280]) {
      await page.setViewportSize({ width, height: 1000 });
      expect(await layoutProblems(page), `${label} at ${width}px`).toEqual([]);
      expect((await page.locator("#magnet").boundingBox()).width).toBeGreaterThan(150);
    }
  }
});
