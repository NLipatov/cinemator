import { defineConfig } from "@playwright/test";

export default defineConfig({
  testDir: "./e2e",
  fullyParallel: true,
  workers: 2,
  retries: 0,
  use: { channel: "chromium", screenshot: "only-on-failure" },
});
