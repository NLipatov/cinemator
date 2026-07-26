import { expect, test } from '@playwright/test';
import fs from 'node:fs/promises';
import path from 'node:path';

const baseURL = process.env.CINEMATOR_REPRO_URL;
const magnet = process.env.CINEMATOR_REPRO_MAGNET;
const fileIndex = process.env.CINEMATOR_REPRO_FILE_INDEX || '0';

test('starts external torrent media with selected subtitles', async ({ page }) => {
  test.skip(
    !baseURL || !magnet,
    'Requires CINEMATOR_REPRO_URL, CINEMATOR_REPRO_MAGNET, and available torrent peers',
  );
  test.setTimeout(5 * 60 * 1000);
  await page.addInitScript(() => {
    if (!navigator.mediaCapabilities) return;
    Object.defineProperty(navigator.mediaCapabilities, 'decodingInfo', {
      configurable: true,
      value: async () => ({
        supported: true,
        smooth: true,
        powerEfficient: true,
        keySystemAccess: null,
        configuration: null,
      }),
    });
  });
  const hlsSource = await fs.readFile(
    path.join(process.cwd(), 'node_modules/hls.js/dist/hls.min.js'),
    'utf8',
  );
  await page.route('https://cdn.jsdelivr.net/npm/hls.js@1.6.13', route =>
    route.fulfill({ contentType: 'text/javascript', body: hlsSource }));

  const hlsErrors = [];
  const playbackErrors = [];
  page.on('response', response => {
    if (response.url().includes('/api/hls/') && !response.ok()) {
      hlsErrors.push({
        url: response.url(),
        status: response.status(),
      });
    }
  });

  await page.goto(baseURL);
  await page.evaluate(() => {
    window.__observedHlsErrors = [];
    window.__observedSubtitleEvents = [];
    window.__observedVideoEvents = [];
    window.__hlsInstanceSequence = 0;
    window.__latestHlsInstance = 0;
    const RealHls = window.Hls;
    window.Hls = class ObservableHls extends RealHls {
      constructor(config) {
        super(config);
        this.__observableID = ++window.__hlsInstanceSequence;
        window.__latestHlsInstance = this.__observableID;
        this.on(RealHls.Events.ERROR, (_event, data) => {
          window.__observedHlsErrors.push({
            instance: this.__observableID,
            type: data?.type,
            details: data?.details,
            fatal: Boolean(data?.fatal),
            reason: data?.reason,
            url: data?.url || data?.context?.url || data?.frag?.url,
            responseCode: data?.response?.code,
            responseText: data?.response?.text,
          });
        });
        this.on(RealHls.Events.SUBTITLE_FRAG_PROCESSED, (_event, data) => {
          window.__observedSubtitleEvents.push({
            instance: this.__observableID,
            success: Boolean(data?.success),
            paused: document.querySelector('video').paused,
            url: data?.frag?.url,
            at: performance.now(),
          });
        });
      }
    };
    const video = document.querySelector('video');
    video.addEventListener('playing', () => {
      window.__observedVideoEvents.push({
        instance: window.__latestHlsInstance,
        event: 'playing',
        at: performance.now(),
      });
    });
    const message = document.querySelector('#playerMsg');
    new MutationObserver(() => {
      const text = message?.textContent?.trim() || '';
      if (text.includes('Playback error')) {
        window.__observedPlaybackErrors ||= [];
        window.__observedPlaybackErrors.push(text);
      }
    }).observe(message, {
      childList: true,
      subtree: true,
      characterData: true,
    });
  });
  await page.getByLabel('Magnet link').fill(magnet);
  await page.getByRole('button', { name: 'Load', exact: true }).click();
  await expect.poll(
    () => page.locator('#filelist').evaluate(
      (select, expectedIndex) =>
        Array.from(select.options).some(option => option.value === expectedIndex),
      fileIndex,
    ),
    { timeout: 60_000 },
  ).toBe(true);
  await page.locator('#filelist').selectOption(fileIndex);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#audioSelect').selectOption('0');
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();

  const video = page.locator('video');
  await expect.poll(
    () => video.evaluate(element => element.currentTime),
    { timeout: 120_000 },
  ).toBeGreaterThan(0.5);
  const initialFrames = await video.evaluate(element =>
    element.getVideoPlaybackQuality?.().totalVideoFrames ??
      element.webkitDecodedFrameCount ??
      0);
  try {
    await expect.poll(
      () => video.evaluate(element =>
        element.getVideoPlaybackQuality?.().totalVideoFrames ??
          element.webkitDecodedFrameCount ??
          0),
      { timeout: 30_000 },
    ).toBeGreaterThan(initialFrames + 5);
  } catch (error) {
    const state = await video.evaluate(element => ({
      currentTime: element.currentTime,
      duration: element.duration,
      paused: element.paused,
      readyState: element.readyState,
      networkState: element.networkState,
      mediaError: element.error
        ? { code: element.error.code, message: element.error.message }
        : null,
      hlsErrors: window.__observedHlsErrors,
      playbackErrors: window.__observedPlaybackErrors || [],
    }));
    throw new Error(
      `${error.message}\nState: ${JSON.stringify(state)}\nFailed HLS responses: ${JSON.stringify(hlsErrors)}`,
    );
  }
  playbackErrors.push(
    ...await page.evaluate(() => window.__observedPlaybackErrors || []),
  );
  const observedHlsErrors = await page.evaluate(() => window.__observedHlsErrors);
  const subtitleReadiness = await page.evaluate(() => {
    return {
      hlsInstances: window.__hlsInstanceSequence,
      latestHlsInstance: window.__latestHlsInstance,
      textTracks: document.querySelector('video').textTracks.length,
      processed: window.__observedSubtitleEvents,
      playing: window.__observedVideoEvents.filter(event => event.event === 'playing'),
    };
  });
  const subtitleMessage = JSON.stringify(subtitleReadiness);
  expect(playbackErrors).toEqual([]);
  expect(
    observedHlsErrors.filter(error => error.details === 'levelParsingError'),
  ).toEqual([]);
  expect(hlsErrors.filter(error => error.status !== 409)).toEqual([]);
  expect(subtitleReadiness.textTracks).toBeGreaterThan(0);
  expect(subtitleReadiness.hlsInstances, subtitleMessage).toBeGreaterThan(0);
  expect(subtitleReadiness.processed.filter(event => event.success).length, subtitleMessage)
    .toBeGreaterThan(0);
  expect(subtitleReadiness.playing.length, subtitleMessage).toBeGreaterThan(0);
  for (const playing of subtitleReadiness.playing) {
    const processed = subtitleReadiness.processed.find(event =>
      event.instance === playing.instance && event.success && event.at <= playing.at);
    expect(processed, JSON.stringify(subtitleReadiness)).toBeDefined();
    expect(processed.paused).toBe(true);
  }
});
