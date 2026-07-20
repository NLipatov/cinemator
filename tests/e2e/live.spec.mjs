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

test.describe('live torrent playback', () => {
  test.skip(!baseURL || !magnet, 'requires CINEMATOR_LIVE_BASE_URL and CINEMATOR_LIVE_MAGNET');

  test('shares one initial presentation between concurrent viewers', async ({ browser }) => {
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

    expect(first.telemetry.preparedStreams[0]).toBe(second.telemetry.preparedStreams[0]);
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
    const initialStream = telemetry.preparedStreams[0];
    const distant = await prepareAt(page, telemetry.prepareURLs[0], 600);
    const retained = await prepareAt(page, telemetry.prepareURLs[0], 0);
    expect(distant.stream).toBe(initialStream);
    expect(retained.stream).toBe(initialStream);
    expect(new URL(telemetry.prepareURLs[0]).searchParams.get('transcode')).toBe('0');
    expect(telemetry.consoleErrors).toEqual([]);
  });

  test('keeps one presentation while seeking', async ({ page }) => {
    test.setTimeout(2 * 60 * 1000);
    const { telemetry, video } = await openLiveMovie(page);
    await video.evaluate(element => {
      window.__liveVideo = element;
      element.currentTime = 120;
    });
    await expect.poll(() => telemetry.prepareStarts.at(-1)).toBe(120);
    await expect.poll(() => telemetry.preparedStreams.length).toBeGreaterThan(1);
    expect(new Set(telemetry.preparedStreams)).toEqual(new Set([telemetry.preparedStreams[0]]));
    expect(await page.evaluate(() => document.getElementById('video') === window.__liveVideo)).toBe(true);
    await expect.poll(() => video.evaluate(element => element.currentTime), { timeout: 120_000 })
      .toBeGreaterThan(120.5);
    expect(telemetry.consoleErrors).toEqual([]);
  });
});
