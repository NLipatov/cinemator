import { test, expect } from "@playwright/test";
import { openPage } from "./fixtures.mjs";

const storageKey = "cinemator-playback:ready";

// A silent WAV lets the real media element play, seek, pause and end offline.
const sampleRate = 8000;
const audio = Buffer.alloc(44 + sampleRate * 2 * 120);
audio.write("RIFF", 0);
audio.writeUInt32LE(audio.length - 8, 4);
audio.write("WAVEfmt ", 8);
audio.writeUInt32LE(16, 16);
audio.writeUInt16LE(1, 20);
audio.writeUInt16LE(1, 22);
audio.writeUInt32LE(sampleRate, 24);
audio.writeUInt32LE(sampleRate * 2, 28);
audio.writeUInt16LE(2, 32);
audio.writeUInt16LE(16, 34);
audio.write("data", 36);
audio.writeUInt32LE(audio.length - 44, 40);

async function openPlayer(page, { source = "HLS", tracks = 1 } = {}) {
  await page.addInitScript((source) => {
    // Tests start playback explicitly so seek assertions do not race autoplay.
    document.addEventListener("DOMContentLoaded", () => {
      document.querySelector("#video").autoplay = false;
    });
    if (source === "native") {
      const canPlayType = HTMLMediaElement.prototype.canPlayType;
      HTMLMediaElement.prototype.canPlayType = function (type) {
        return type === "application/vnd.apple.mpegurl" ? "probably" : canPlayType.call(this, type);
      };
      return;
    }
    // Replace only the HLS transport; playback and media events remain real.
    window.Hls = class {
      static Events = { MANIFEST_PARSED: "manifest", FRAG_LOADED: "fragment", ERROR: "error" };
      static isSupported() {
        return true;
      }
      constructor(options) {
        window.testHls = this;
        this.options = options;
        this.handlers = new Map();
      }
      on(event, handler) {
        this.handlers.set(event, handler);
      }
      loadSource(src) {
        this.src = src;
      }
      attachMedia(video) {
        video.src = this.src;
        video.addEventListener("loadedmetadata", () => this.handlers.get("manifest")?.(), {
          once: true,
        });
      }
      destroy() {}
    };
  }, source);
  await openPage(page, "/", "en");
  const download = {
    id: "ready",
    title: "Two episodes",
    magnet: "magnet:?xt=urn:btih:0123456789012345678901234567890123456789",
    status: "ready",
    size: 1024,
    expiresAt: "2099-01-01T00:00:00Z",
    selectedFileIndex: 4,
    files: [
      { index: 2, name: "Episode 1.mkv", size: 512 },
      { index: 4, name: "Episode 2.mkv", size: 512 },
    ],
  };
  await page.route(/\/api\/downloads(?:\?|$)/, (route) =>
    route.fulfill({
      json: [download, { ...download, id: "other", title: "Other video", magnet: "magnet:other" }],
    }),
  );
  await page.route("**/api/hls/prepare?*", (route) => {
    if (route.request().method() === "POST") return route.fulfill({ status: 204 });
    const range = route
      .request()
      .headers()
      .range?.match(/^bytes=(\d+)-(\d*)$/);
    const start = range ? Number(range[1]) : 0;
    const end = range?.[2] ? Number(range[2]) : audio.length - 1;
    return route.fulfill({
      status: range ? 206 : 200,
      contentType: "audio/wav",
      headers: {
        "Accept-Ranges": "bytes",
        ...(range ? { "Content-Range": `bytes ${start}-${end}/${audio.length}` } : {}),
      },
      body: audio.subarray(start, end + 1),
    });
  });
  await page.route("**/api/media/info?*", (route) =>
    route.fulfill({
      json: {
        audioTracks: Array.from({ length: tracks }, (_, index) => ({ index })),
        subtitles: [],
      },
    }),
  );
  await page.evaluate(() => window.dispatchEvent(new Event("focus")));
  await expect(page.locator('.download-row[data-id="ready"] .download-title')).toHaveText(
    download.title,
  );
  return page.locator('.download-row[data-id="ready"]');
}

async function savedPosition(page) {
  return page.evaluate((key) => JSON.parse(localStorage.getItem(key)), storageKey);
}

async function seedPosition(page, position) {
  await page.evaluate(
    ({ key, position }) => {
      localStorage.setItem(key, JSON.stringify(position));
      window.dispatchEvent(new Event("focus"));
    },
    { key: storageKey, position },
  );
}

async function seek(page, time) {
  await page.locator("#video").evaluate(async (video, time) => {
    const seeked = new Promise((resolve) =>
      video.addEventListener("seeked", resolve, { once: true }),
    );
    video.currentTime = time;
    await seeked;
  }, time);
}

async function startVideo(page, row) {
  await row.locator('[data-action="open"]').click();
  await page.locator("#selectTracks").click();
  await expect.poll(() => page.locator("#video").evaluate((video) => video.duration)).toBe(120);
}

for (const source of ["HLS", "native"]) {
  test(`${source}: resumes the saved file after reload and clears the position on completion`, async ({
    page,
  }) => {
    const row = await openPlayer(page, { source });
    await seedPosition(page, { fileIndex: 2, time: 42 });
    await page.reload();
    const resume = row.locator('[data-action="resume"]');
    await expect(resume).toHaveText("Continue from 0:42");
    await resume.click();
    await expect(page.locator("#filelist")).toHaveValue("2");
    await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(42);
    if (source === "HLS") {
      expect(await page.evaluate(() => window.testHls.options.startPosition)).toBe(42);
    }

    await seek(page, 55);
    await expect.poll(() => savedPosition(page)).toEqual({ fileIndex: 2, time: 55 });
    await expect(resume).toHaveText("Continue from 0:55");
    await page.reload();
    await expect(resume).toHaveText("Continue from 0:55");
    await resume.click();
    await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(55);

    await seek(page, 119.9);
    await page.locator("#video").evaluate((video) => video.play());
    await expect.poll(() => savedPosition(page)).toBeNull();
    await expect(resume).toBeHidden();
    await page.evaluate(() => window.dispatchEvent(new Event("pagehide")));
    expect(await savedPosition(page)).toBeNull();
    await row.locator('[data-action="open"]').click();
    expect(await savedPosition(page)).toBeNull();
  });
}

test("saves media events with throttling and flushes on pause, pagehide and video replacement", async ({
  page,
}) => {
  const row = await openPlayer(page);
  await startVideo(page, row);
  await seek(page, 10);
  await page.locator("#video").evaluate((video) => video.play());
  await expect
    .poll(() => page.locator("#video").evaluate((video) => video.currentTime))
    .toBeGreaterThan(10.3);
  // Frequent timeupdate events must not write before the five-second interval.
  expect(await savedPosition(page)).toEqual({ fileIndex: 4, time: 10 });
  // This update must happen while playing, without a pause or seek to flush it.
  await expect
    .poll(async () => (await savedPosition(page)).time, { timeout: 10000 })
    .toBeGreaterThan(14);
  await page.locator("#video").evaluate((video) => video.pause());
  expect((await savedPosition(page)).time).toBe(
    await page.locator("#video").evaluate((video) => video.currentTime),
  );

  await page.locator("#video").evaluate((video) => video.play());
  const pagehidePosition = await page.locator("#video").evaluate(async (video, key) => {
    await new Promise((resolve) => video.addEventListener("timeupdate", resolve, { once: true }));
    window.dispatchEvent(new Event("pagehide"));
    const result = { saved: JSON.parse(localStorage.getItem(key)).time, time: video.currentTime };
    video.pause();
    return result;
  }, storageKey);
  expect(pagehidePosition.saved).toBe(pagehidePosition.time);

  // Changing the form's selected file must not reassign the playing video's progress.
  await page.locator("#filelist").selectOption("2");
  await seek(page, 30);
  expect(await savedPosition(page)).toEqual({ fileIndex: 4, time: 30 });
  await page.locator("#video").evaluate((video) => {
    window.oldVideo = video;
    video.currentTime = 31;
    // Replace synchronously, before the queued seeked event can save the position.
    document.querySelector('.download-row[data-id="other"] [data-action="open"]').click();
  });
  expect(await savedPosition(page)).toEqual({ fileIndex: 4, time: 31 });
  await page.evaluate(() => {
    window.oldVideo.currentTime = 90;
    window.oldVideo.dispatchEvent(new Event("pause"));
  });
  expect(await savedPosition(page)).toEqual({ fileIndex: 4, time: 31 });
  expect(await page.evaluate(() => localStorage.getItem("cinemator-playback:other"))).toBeNull();
});

test("resume survives track selection and switching while an ordinary open starts at zero", async ({
  page,
}) => {
  const row = await openPlayer(page, { tracks: 2 });
  await seedPosition(page, { fileIndex: 2, time: 42 });
  await row.locator('[data-action="resume"]').click();
  await expect(page.locator("#step-tracks")).toBeVisible();
  await expect(page.locator("#player-block")).toBeHidden();
  await page.locator("#audioSelect").selectOption("1");
  await page.locator("#play").click();
  await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(42);
  await seek(page, 50);
  const nextStream = page.waitForRequest(
    (request) => request.method() === "GET" && request.url().includes("/api/hls/prepare"),
  );
  await page.locator("#audioSelect").selectOption("0");
  await nextStream;
  await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(50);
  expect(await savedPosition(page)).toEqual({ fileIndex: 2, time: 50 });

  await row.locator('[data-action="open"]').click();
  await page.locator("#selectTracks").click();
  await expect(page.locator("#step-tracks")).toBeVisible();
  await page.locator("#play").click();
  await expect.poll(() => page.locator("#video").evaluate((video) => video.duration)).toBe(120);
  expect(await page.locator("#video").evaluate((video) => video.currentTime)).toBe(0);
  expect(await savedPosition(page)).toEqual({ fileIndex: 2, time: 50 });
});

test("changing the saved file before playback cancels the pending resume", async ({ page }) => {
  const row = await openPlayer(page, { tracks: 2 });
  await seedPosition(page, { fileIndex: 2, time: 42 });
  await row.locator('[data-action="resume"]').click();
  await expect(page.locator("#step-tracks")).toBeVisible();
  await page.locator("#filelist").selectOption("4");
  await page.locator("#selectTracks").click();
  await expect(page.locator("#step-tracks")).toBeVisible();
  await page.locator("#play").click();
  await expect.poll(() => page.locator("#video").evaluate((video) => video.duration)).toBe(120);
  expect(await page.locator("#video").evaluate((video) => video.currentTime)).toBe(0);
});

test("invalid saved data and storage write failures do not prevent playback", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const row = await openPlayer(page);
  for (const position of [null, { fileIndex: 2, time: -1 }, { fileIndex: 99, time: 42 }]) {
    await seedPosition(page, position);
    await expect(row.locator('[data-action="resume"]')).toBeHidden();
  }
  await page.evaluate((key) => {
    localStorage.setItem(key, "not json");
    window.dispatchEvent(new Event("focus"));
    const setItem = Storage.prototype.setItem;
    Storage.prototype.setItem = function (key, value) {
      if (key.startsWith("cinemator-playback:"))
        throw new DOMException("Quota exceeded", "QuotaExceededError");
      setItem.call(this, key, value);
    };
  }, storageKey);
  await expect(row.locator('[data-action="resume"]')).toBeHidden();
  await startVideo(page, row);
  await seek(page, 10);
  await page.locator("#video").evaluate((video) => video.play());
  await expect
    .poll(() => page.locator("#video").evaluate((video) => video.currentTime))
    .toBeGreaterThan(10);
  expect(errors).toEqual([]);
});

test("resume labels localize and fit a narrow viewport with enlarged text", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 320, height: 900 });
  const row = await openPlayer(page);
  await seedPosition(page, { fileIndex: 2, time: 4455 });
  const resume = row.locator('[data-action="resume"]');
  await expect(resume).toHaveText("Continue from 1:14:15");
  await page.locator("#languageSelect").selectOption("ru");
  await expect(resume).toHaveText("Продолжить с 1:14:15");
  await page.screenshot({ path: testInfo.outputPath("resume-mobile.png"), fullPage: true });
  await resume.focus();
  await page.evaluate(() => {
    document.documentElement.style.fontSize = "32px";
    window.dispatchEvent(new Event("focus"));
  });
  await expect(resume).toBeFocused();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const box = await resume.boundingBox();
  expect(box.x + box.width).toBeLessThanOrEqual(320);
  await page.screenshot({ path: testInfo.outputPath("resume-large-text.png"), fullPage: true });
});
