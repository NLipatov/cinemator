import { test, expect } from "@playwright/test";
import { fileURLToPath } from "node:url";

async function openPage(page, path, language) {
  await page.addInitScript(
    (language) => localStorage.setItem("cinemator-language", language),
    language,
  );
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.hostname !== "cinemator.test") {
      // The HLS library is not involved in layout. Keep this suite offline.
      return route.fulfill({ contentType: "text/javascript", body: "" });
    }
    if (url.pathname.startsWith("/api/")) {
      if (url.pathname === "/api/downloads/events") return route.fulfill({ status: 204 });
      if (url.pathname === "/api/downloads")
        return route.fulfill({
          json: [
            {
              id: "ready",
              title: "Video ready to play",
              status: "ready",
              size: 1024,
              expiresAt: "2099-01-01T00:00:00Z",
            },
            { id: "expired", title: "Expired video", status: "expired", size: 1024 },
          ],
        });
      if (url.pathname === "/api/auth/status") return route.fulfill({ json: { enabled: true } });
      if (url.pathname === "/api/version") return route.fulfill({ json: { version: "test" } });
      if (url.pathname.startsWith("/api/auth/sign-in-approvals/")) {
        return route.fulfill({
          json: { code: "ABCD-1234", expiresAt: new Date(Date.now() + 300000).toISOString() },
        });
      }
      if (url.pathname === "/api/auth/sign-in-requests") return route.fulfill({ status: 503 });
      return route.fulfill({ json: [] });
    }
    const filename =
      {
        "/": "index.html",
        "/login": "login.html",
        "/sign-in-approvals/test": "sign-in-approval.html",
      }[url.pathname] ?? url.pathname.slice(1);
    if (!/^[\w./-]+$/.test(filename) || filename.includes("..")) return route.abort();
    const directory =
      filename.startsWith("assets/") || filename === "favicon.ico" ? "static" : "index";
    await route.fulfill({
      path: fileURLToPath(new URL(`../${directory}/${filename}`, import.meta.url)),
    });
  });
  await page.goto(`http://cinemator.test${path}`);
  await expect(page.locator("html")).toHaveAttribute("lang", language);
  await page.addStyleTag({
    content: "* { animation: none !important; transition: none !important; }",
  });
  if (path === "/") {
    await expect(page.locator(".download-row")).toHaveCount(2);
    // Expose track controls as they would appear after loading media metadata.
    await page.evaluate(() => {
      document
        .querySelectorAll(".step, #subtitleDelayRow")
        .forEach((element) => (element.style.display = "grid"));
      document.querySelector('[data-action="toggle-extend"]').click();
    });
  }
}

async function layoutProblems(page) {
  return page.evaluate(() => {
    const problems = [];
    if (document.documentElement.scrollWidth > innerWidth)
      problems.push("page overflows horizontally");
    for (const element of document.querySelectorAll(
      "button:not(.download-open), label, .field-label, .panel-title, .delay-value",
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
    return problems;
  });
}

for (const language of ["en", "ru", "zh-Hans"]) {
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
