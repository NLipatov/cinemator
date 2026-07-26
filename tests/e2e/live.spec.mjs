import { expect, test } from '@playwright/test';

const baseURL = process.env.CINEMATOR_LIVE_BASE_URL;
const magnet = process.env.CINEMATOR_LIVE_MAGNET;

async function openLiveMovie(page) {
  const telemetry = {
    consoleErrors: [],
    prepareStarts: [],
    prepareURLs: [],
    preparedStreams: [],
    transcodeFlags: [],
  };
  await page.addInitScript(() => {
    Object.defineProperty(navigator, 'mediaCapabilities', {
      configurable: true,
      value: {
        decodingInfo: async () => ({ supported: true, smooth: true, powerEfficient: true }),
      },
    });
  });
  page.on('console', message => {
    if (message.type() === 'error') telemetry.consoleErrors.push(message.text());
  });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.pathname === '/api/hls/prepare') {
      telemetry.prepareStarts.push(Number(url.searchParams.get('start')));
      telemetry.prepareURLs.push(url.toString());
      telemetry.transcodeFlags.push(Number(url.searchParams.get('transcode')));
    }
  });
  page.on('response', response => {
    const url = new URL(response.url());
    if (url.pathname !== '/api/hls/prepare' || !response.ok()) return;
    void response.json()
      .then(body => telemetry.preparedStreams.push(body.stream))
      .catch(() => {});
  });

  await page.goto(baseURL);
  await page.getByLabel('Magnet link').fill(magnet);
  await page.getByRole('button', { name: 'Load', exact: true }).click();
  await expect(page.locator('#filelist')).toContainText('Sintel.mp4', { timeout: 60_000 });
  await page.locator('#filelist').selectOption('5');
  await page.getByRole('button', { name: 'Select tracks' }).click();

  const video = page.locator('video');
  await expect.poll(() => video.evaluate(element => element.duration), { timeout: 120_000 })
    .toBeGreaterThan(800);
  await video.evaluate(element => { void element.play(); });
  await expect.poll(() => video.evaluate(element => element.currentTime > 0.5 && element.currentTime < 30), { timeout: 120_000 })
    .toBe(true);
  await expect.poll(() => video.evaluate(element => element.videoWidth), { timeout: 30_000 })
    .toBeGreaterThan(0);
  const decodedFrames = await video.evaluate(element =>
    element.getVideoPlaybackQuality?.().totalVideoFrames ?? element.webkitDecodedFrameCount ?? 0,
  );
  await expect.poll(() => video.evaluate(element =>
    element.getVideoPlaybackQuality?.().totalVideoFrames ?? element.webkitDecodedFrameCount ?? 0,
  ), { timeout: 30_000 }).toBeGreaterThan(decodedFrames + 5);
  return { telemetry, video };
}

async function prepareAt(page, prepareURL, start) {
  const url = new URL(prepareURL);
  url.searchParams.set('start', String(start));
  const response = await page.request.get(url.toString(), {
    headers: { Accept: 'application/json' },
  });
  expect(response.ok()).toBe(true);
  const prepared = await response.json();
  await expect.poll(async () => {
    const status = await page.request.get(
      new URL(`/api/hls/status/${encodeURIComponent(prepared.stream)}?target=${start}`, baseURL).toString(),
    );
    if (!status.ok()) return `http-${status.status()}`;
    return (await status.json()).phase;
  }, { timeout: 60_000 }).toBe('ready');
  return prepared;
}

async function samplePlaybackClock(video, durationMS = 15_000) {
  return video.evaluate(async (element, sampleDurationMS) => {
    const startedAt = performance.now();
    const samples = [];
    while (performance.now() - startedAt < sampleDurationMS) {
      samples.push({
        mediaTime: element.currentTime,
        wallTime: (performance.now() - startedAt) / 1000,
      });
      await new Promise(resolve => setTimeout(resolve, 250));
    }
    samples.push({
      mediaTime: element.currentTime,
      wallTime: (performance.now() - startedAt) / 1000,
    });
    return {
      playbackRate: element.playbackRate,
      samples,
    };
  }, durationMS);
}

test.describe('live torrent playback', () => {
  test.skip(!baseURL || !magnet, 'requires CINEMATOR_LIVE_BASE_URL and CINEMATOR_LIVE_MAGNET');

  test('isolates concurrent viewer presentations on one torrent runtime', async ({ browser }) => {
    test.setTimeout(2 * 60 * 1000);
    const context = await browser.newContext();
    const firstPage = await context.newPage();
    const secondPage = await context.newPage();
    const [first, second] = await Promise.all([
      openLiveMovie(firstPage),
      openLiveMovie(secondPage),
    ]);
    await expect.poll(() => first.telemetry.preparedStreams.length).toBeGreaterThan(0);
    await expect.poll(() => second.telemetry.preparedStreams.length).toBeGreaterThan(0);

    const firstPrepare = new URL(first.telemetry.prepareURLs[0]);
    const secondPrepare = new URL(second.telemetry.prepareURLs[0]);
    expect(firstPrepare.searchParams.get('session')).toMatch(/^[A-Za-z0-9-]{1,64}$/);
    expect(secondPrepare.searchParams.get('session')).toMatch(/^[A-Za-z0-9-]{1,64}$/);
    expect(firstPrepare.searchParams.get('session')).not.toBe(secondPrepare.searchParams.get('session'));
    expect(first.telemetry.preparedStreams.at(-1)).not.toBe(second.telemetry.preparedStreams.at(-1));
    expect(first.telemetry.transcodeFlags[0]).toBe(0);
    expect(second.telemetry.transcodeFlags[0]).toBe(0);
    const secondTime = await second.video.evaluate(element => element.currentTime);
    await firstPage.close();
    await expect.poll(() => second.video.evaluate(element => element.currentTime), { timeout: 10_000 })
      .toBeGreaterThan(secondTime + 0.5);
    await context.close();
  });

  test('reuses retained HLS history after preparing a distant position', async ({ page }) => {
    test.setTimeout(2 * 60 * 1000);
    const { telemetry } = await openLiveMovie(page);
    await expect.poll(() => telemetry.preparedStreams.length).toBeGreaterThan(0);
    const activeStream = telemetry.preparedStreams.at(-1);
    const activePrepareURL = telemetry.prepareURLs.at(-1);
    const distant = await prepareAt(page, activePrepareURL, 600);
    const retained = await prepareAt(page, activePrepareURL, 0);
    expect(distant.stream).toBe(activeStream);
    expect(retained.stream).toBe(activeStream);
    expect(new URL(telemetry.prepareURLs[0]).searchParams.get('transcode')).toBe('0');
    expect(telemetry.consoleErrors).toEqual([]);
  });

  test('keeps one presentation while seeking', async ({ page }) => {
    test.setTimeout(2 * 60 * 1000);
    const { telemetry, video } = await openLiveMovie(page);
    const activeStream = telemetry.preparedStreams.at(-1);
    const preparedBeforeSeek = telemetry.preparedStreams.length;
    await video.evaluate(element => {
      window.__liveVideo = element;
      element.dispatchEvent(new PointerEvent('pointerdown', { bubbles: true }));
      element.currentTime = 120;
    });
    await expect.poll(() => telemetry.prepareStarts.at(-1)).toBe(120);
    await expect.poll(() => telemetry.preparedStreams.length).toBeGreaterThan(preparedBeforeSeek);
    expect(new Set(telemetry.preparedStreams.slice(preparedBeforeSeek))).toEqual(new Set([activeStream]));
    expect(await page.evaluate(() => document.getElementById('video') === window.__liveVideo)).toBe(true);
    await expect.poll(() => video.evaluate(element => element.currentTime), { timeout: 120_000 })
      .toBeGreaterThan(120.5);
    const resumedAt = await video.evaluate(element => element.currentTime);
    expect(resumedAt).toBeLessThan(124);
    const decodedFrames = await video.evaluate(element =>
      element.getVideoPlaybackQuality?.().totalVideoFrames ?? element.webkitDecodedFrameCount ?? 0,
    );
    await expect.poll(() => video.evaluate(element =>
      element.getVideoPlaybackQuality?.().totalVideoFrames ?? element.webkitDecodedFrameCount ?? 0,
    ), { timeout: 30_000 }).toBeGreaterThan(decodedFrames + 5);
    const clock = await samplePlaybackClock(video);
    expect(clock.playbackRate).toBe(1);
    for (let index = 1; index < clock.samples.length; index += 1) {
      const previous = clock.samples[index - 1];
      const current = clock.samples[index];
      expect(current.mediaTime).toBeGreaterThanOrEqual(previous.mediaTime - 0.1);
      expect(current.mediaTime - previous.mediaTime)
        .toBeLessThanOrEqual(current.wallTime - previous.wallTime + 0.75);
    }
    const first = clock.samples[0];
    const last = clock.samples.at(-1);
    expect(last.mediaTime).toBeGreaterThan(first.mediaTime + 1);
    expect(last.mediaTime - first.mediaTime).toBeLessThanOrEqual(last.wallTime - first.wallTime + 1.5);
    expect(telemetry.consoleErrors).toEqual([]);
  });
});
