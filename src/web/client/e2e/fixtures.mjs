import { expect } from "@playwright/test";
import { fileURLToPath } from "node:url";

export async function openPage(page, path, language) {
  if (language) {
    await page.addInitScript(
      (language) => localStorage.setItem("cinemator-language", language),
      language,
    );
  }
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    if (url.hostname !== "cinemator.test") {
      // The HLS library is not involved in these UI tests. Keep them offline.
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
  if (language) await expect(page.locator("html")).toHaveAttribute("lang", language);
  await page.addStyleTag({
    content: "* { animation: none !important; transition: none !important; }",
  });
}
