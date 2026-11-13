import { test, expect } from "@playwright/test";
import { openPlayer } from "./player-fixture.mjs";

const storageKey = "cinemator-playback:ready";

async function savedPosition(page) {
  return page.evaluate((key) => JSON.parse(localStorage.getItem(key)), storageKey);
}

async function watchedFraction(progress) {
  return progress.evaluate(el => parseFloat(getComputedStyle(el, '::before').width) / el.getBoundingClientRect().width);
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
  await expect.poll(() => page.locator("#video").evaluate((video) => video.duration)).toBe(120);
}

for (const source of ["HLS", "native"]) {
  test(`${source}: resumes the saved file after reload and clears the position on completion`, async ({
    page,
  }) => {
    const row = await openPlayer(page, { source });
    await seedPosition(page, { fileIndex: 2, time: 42, duration: 120 });
    await page.reload();
    const progress = row.locator('.download-watched');
    await expect(progress).toBeVisible();
    await expect.poll(() => watchedFraction(progress)).toBeCloseTo(42 / 120, 2);
    await expect(row.locator('.download-caption')).not.toContainText('Continue from');
    await row.locator('.download-open').click();
    await expect(page.locator("#videoMenuLabel")).toHaveText("Video: 1/2");
    await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(42);
    if (source === "HLS") {
      expect(await page.evaluate(() => window.testHls.options.startPosition)).toBe(42);
    }

    await seek(page, 55);
    await expect.poll(() => savedPosition(page)).toMatchObject({ fileIndex: 2, time: 55 });
    await expect.poll(() => watchedFraction(progress)).toBeCloseTo(55 / 120, 2);
    await page.reload();
    await expect.poll(() => watchedFraction(progress)).toBeCloseTo(55 / 120, 2);
    await row.locator('.download-open').click();
    await expect.poll(() => page.locator("#video").evaluate((video) => video.currentTime)).toBe(55);

    await seek(page, 119.9);
    await page.locator("#video").evaluate((video) => video.play());
    await expect.poll(() => savedPosition(page)).toBeNull();
    await expect(progress).toBeHidden();
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
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 4, time: 10 });
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

  // Replacing the playing video must flush its own position and detach old events.
  await seek(page, 30);
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 4, time: 30 });
  await page.locator("#video").evaluate((video) => {
    window.oldVideo = video;
    video.currentTime = 31;
    // Replace synchronously, before the queued seeked event can save the position.
    document.querySelector('.download-row[data-id="other"] [data-action="open"]').click();
  });
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 4, time: 31 });
  await page.evaluate(() => {
    window.oldVideo.currentTime = 90;
    window.oldVideo.dispatchEvent(new Event("pause"));
  });
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 4, time: 31 });
  expect(await page.evaluate(() => localStorage.getItem("cinemator-playback:other"))).toBeNull();
});

test("resume starts immediately with multiple tracks and switching preserves the position", async ({ page }) => {
  const row = await openPlayer(page, { tracks: 2 });
  await seedPosition(page, { fileIndex: 2, time: 42 });
  await row.locator('.download-open').click();
  await expect.poll(() => page.locator("#video").evaluate(v => v.currentTime)).toBe(42);
  await seek(page, 50);
  await page.locator("#audioMenuButton").click();
  await page.locator('#audioTracks [data-value="1"]').click();
  await expect.poll(() => page.locator("#video").evaluate(v => v.currentTime)).toBe(50);
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 2, time: 50 });
  await row.locator('.download-more').click();
  await row.locator('[data-action="restart"]').click();
  await expect.poll(() => page.locator("#video").evaluate(v => v.duration)).toBe(120);
  expect(await page.locator("#video").evaluate(v => v.currentTime)).toBe(0);
});

test("switching the video during playback starts the new file at zero", async ({ page }) => {
  const row = await openPlayer(page, { tracks: 2 });
  await seedPosition(page, { fileIndex: 2, time: 42 });
  await row.locator('.download-open').click();
  await expect.poll(() => page.locator("#video").evaluate(v => v.currentTime)).toBe(42);
  await page.locator("#videoMenuButton").click();
  await page.getByRole('menuitemradio', {name:'Episode 2.mkv', exact:true}).click();
  await expect.poll(() => page.locator("#video").evaluate(v => v.duration)).toBe(120);
  expect(await page.locator("#video").evaluate(v => v.currentTime)).toBe(0);
  await seek(page, 15);
  expect(await savedPosition(page)).toMatchObject({ fileIndex: 4, time: 15 });
});

test("invalid saved data and storage write failures do not prevent playback", async ({ page }) => {
  const errors = [];
  page.on("pageerror", (error) => errors.push(error.message));
  const row = await openPlayer(page);
  for (const position of [null, { fileIndex: 2, time: -1 }, { fileIndex: 99, time: 42 }]) {
    await seedPosition(page, position);
    await expect(row.locator('.download-watched')).toBeHidden();
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
  await expect(row.locator('.download-watched')).toBeHidden();
  await startVideo(page, row);
  await seek(page, 10);
  await page.locator("#video").evaluate((video) => video.play());
  await expect
    .poll(() => page.locator("#video").evaluate((video) => video.currentTime))
    .toBeGreaterThan(10);
  expect(errors).toEqual([]);
});

test("playback progress fits a narrow viewport and resume remains accessible without a caption", async ({
  page,
}, testInfo) => {
  await page.setViewportSize({ width: 320, height: 900 });
  const row = await openPlayer(page);
  await seedPosition(page, { fileIndex: 2, time: 4455, duration: 7200 });
  const progress = row.locator('.download-watched');
  await expect(progress).toBeVisible();
  await expect.poll(() => watchedFraction(progress)).toBeCloseTo(4455 / 7200, 2);
  await expect(row.locator('.download-open')).toHaveAccessibleName(/Continue from 1:14:15/);
  await expect(row.locator('.download-caption')).not.toContainText('Continue from');
  await page.locator("#languageSelect").selectOption("ru");
  await expect(row.locator('.download-open')).toHaveAccessibleName(/Продолжить с 1:14:15/);
  await expect(row.locator('.download-caption')).not.toContainText('Продолжить');
  await page.screenshot({ path: testInfo.outputPath("resume-mobile.png"), fullPage: true });
  await row.locator('.download-open').focus();
  await page.evaluate(() => {
    document.documentElement.style.fontSize = "32px";
    window.dispatchEvent(new Event("focus"));
  });
  await expect(row.locator('.download-open')).toBeFocused();
  expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  const box = await progress.boundingBox();
  expect(box.x + box.width).toBeLessThanOrEqual(320);
  await page.screenshot({ path: testInfo.outputPath("resume-large-text.png"), fullPage: true });
});
