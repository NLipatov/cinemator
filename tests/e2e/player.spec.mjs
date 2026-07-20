import { expect, test } from '@playwright/test';

const duration = 2 * 60 * 60;

const fakeHls = String.raw`
  class FakeHls {
    static Events = {
      MANIFEST_PARSED: 'manifestParsed',
      MEDIA_ATTACHED: 'mediaAttached',
      SUBTITLE_TRACK_LOADED: 'subtitleTrackLoaded',
      SUBTITLE_TRACK_SWITCH: 'subtitleTrackSwitch',
      FRAG_LOADED: 'fragLoaded',
      FRAG_BUFFERED: 'fragBuffered',
      ERROR: 'error',
    };
    static ErrorTypes = { MEDIA_ERROR: 'mediaError' };
    static ErrorDetails = { LEVEL_EMPTY_ERROR: 'levelEmptyError' };
    static isSupported() { return true; }
    static getMediaSource() { return { isTypeSupported: () => true }; }

    constructor(config) {
      this.config = config;
      this.handlers = new Map();
      this.subtitleTracks = [];
      window.__hlsConfigs.push(config);
      window.__hlsInstances.push(this);
    }
    on(event, handler) {
      const handlers = this.handlers.get(event) || [];
      handlers.push(handler);
      this.handlers.set(event, handlers);
    }
    emit(event, data) {
      for (const handler of this.handlers.get(event) || []) handler(event, data);
    }
    loadSource(source) {
      this.source = source;
      window.__hlsSources.push(source);
    }
    attachMedia(input) {
      const media = input.media || input;
      this.media = media;
      const requestedDuration = input.overrides?.duration;
      media.play = () => Promise.resolve();
      window.__hlsAttachments.push({
        duration: requestedDuration,
        timelineOffset: this.config.timelineOffset,
      });
      if (Number.isFinite(requestedDuration)) {
        Object.defineProperty(media, 'duration', {
          configurable: true,
          value: requestedDuration,
        });
      }
      const finishAttachment = () => {
        this.emit(FakeHls.Events.MEDIA_ATTACHED, {
          mediaSource: { readyState: 'open', duration: requestedDuration },
        });
        const requestedStart = Number(this.source.match(/stream-([0-9.]+)/)?.[1] || 0);
        if (window.__simulateAttachSeekToEnd && requestedStart > 0 && Number.isFinite(requestedDuration)) {
          media.currentTime = requestedDuration;
          media.dispatchEvent(new Event('seeking'));
        }
        const earlySeek = Number(media.currentTime) || 0;
        const configuredStart = Number(this.config.startPosition);
        const loadPosition = configuredStart >= 0
          ? configuredStart
          : earlySeek > 0.25
          ? earlySeek + (this.config.timelineOffset || 0)
          : requestedStart;
        window.__hlsLoadPositions.push(loadPosition);
        const start = this.config.timelineOffset || 0;
        this.latestLevelDetails = { fragments: [{ start, duration: 2 }] };
        this.emit(FakeHls.Events.MANIFEST_PARSED);
        if (window.__bufferInitialFragment && Math.abs(loadPosition - requestedStart) <= 0.25) {
          media.currentTime = requestedStart;
          Object.defineProperty(media, 'buffered', {
            configurable: true,
            value: { length: 1, start: () => requestedStart, end: () => requestedStart + 2 },
          });
          this.emit(FakeHls.Events.FRAG_BUFFERED, { frag: { type: 'main' } });
        }
      };
      queueMicrotask(finishAttachment);
    }
    stopLoad() { window.__hlsStopLoads++; }
    startLoad(position) { window.__hlsStartLoads.push(position); }
    slideLiveWindow(start) {
      this.latestLevelDetails = { fragments: [{ start, duration: 2 }] };
      if (this.config.liveSyncMode !== 'buffered') this.media.currentTime = start;
    }
    destroy() {
      if (!this.media) return;
      window.__hlsDestroyCount++;
      Object.defineProperty(this.media, 'currentTime', { configurable: true, writable: true, value: 0 });
      Object.defineProperty(this.media, 'duration', { configurable: true, value: NaN });
    }
  }
  window.Hls = FakeHls;
`;

async function openMovie(page, {
  beforePrepare,
  fileListError,
  getHlsStatus,
  mediaInfo = {},
  onFileListRequest,
  simulateAttachSeekToEnd = false,
  stableStream = false,
  bufferInitialFragment = true,
} = {}) {
  const prepareStarts = [];
  let presentationGeneration = 0;

  await page.addInitScript(({ simulateEndSeek, bufferInitial }) => {
    window.__simulateAttachSeekToEnd = simulateEndSeek;
    window.__bufferInitialFragment = bufferInitial;
    window.__hlsConfigs = [];
    window.__hlsInstances = [];
    window.__hlsSources = [];
    window.__hlsAttachments = [];
    window.__hlsLoadPositions = [];
    window.__hlsStopLoads = 0;
    window.__hlsStartLoads = [];
    window.__hlsDestroyCount = 0;
    class FakeEventSource {
      addEventListener() {}
      close() {}
    }
    window.EventSource = FakeEventSource;
    Object.defineProperty(navigator, 'mediaCapabilities', {
      configurable: true,
      value: {
        decodingInfo: async () => ({
          supported: true,
          smooth: true,
          powerEfficient: true,
        }),
      },
    });
  }, { simulateEndSeek: simulateAttachSeekToEnd, bufferInitial: bufferInitialFragment });

  await page.route('https://cdn.jsdelivr.net/npm/hls.js@1.6.13', route => route.fulfill({
    contentType: 'text/javascript',
    body: fakeHls,
  }));
  await page.route('**/api/**', async route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/api/auth/status') {
      await route.fulfill({ json: { enabled: false, version: 'test-build' } });
      return;
    }
    if (url.pathname === '/api/downloads') {
      await route.fulfill({ json: [] });
      return;
    }
    if (url.pathname === '/api/torrent/files') {
      onFileListRequest?.();
      if (fileListError) {
        await route.fulfill({ status: 500, body: fileListError });
      } else {
        await route.fulfill({ json: [{ index: 0, name: 'movie.mkv', size: 4_000_000_000 }] });
      }
      return;
    }
    if (url.pathname === '/api/media/info') {
      await route.fulfill({
        json: {
          duration,
          seekable: true,
          videoCodec: 'h264',
          videoCodecString: 'avc1.640028',
          videoProfile: 'high',
          videoLevel: 40,
          pixelFormat: 'yuv420p',
          width: 1920,
          height: 1080,
          frameRate: 24,
          bitrate: 12_000_000,
          audioTracks: [{ index: 0, codec: 'aac', channels: 2 }],
          subtitles: [],
          ...mediaInfo,
        },
      });
      return;
    }
    if (url.pathname === '/api/hls/prepare') {
      const start = Number(url.searchParams.get('start'));
      prepareStarts.push(start);
      await beforePrepare?.(start);
      presentationGeneration++;
      const stream = stableStream ? 'stream-stable' : `stream-${start}-${presentationGeneration}`;
      await route.fulfill({
        json: {
          playlist: `/api/hls/${stream}/master.m3u8`,
          stream,
          segmentDurationSeconds: 2,
          windowSegments: 15,
        },
      });
      return;
    }
    if (url.pathname.startsWith('/api/hls/status/')) {
      const targetSeconds = Number(url.searchParams.get('target'));
      const generation = stableStream ? 'generation-stable' : `generation-${presentationGeneration}`;
      const customStatus = getHlsStatus?.(targetSeconds);
      await route.fulfill({
        json: customStatus ? { generation, ...customStatus } : {
          phase: 'ready',
          targetSeconds,
          presentationOriginSeconds: Math.floor(Math.floor(targetSeconds / 2) / 15) * 30,
          generation,
          bytesRead: 1024,
          activePeers: 1,
          totalPeers: 1,
        },
      });
      return;
    }
    await route.fulfill({ status: 404 });
  });

  await page.goto('/');
  await page.getByLabel('Magnet link').fill('magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567');
  await page.getByRole('button', { name: 'Load' }).click();

  return prepareStarts;
}

test('exposes the complete source duration before the full torrent is available', async ({ page }) => {
  await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments[0])).toEqual({
    duration,
    timelineOffset: 0,
  });
});

test('starts at the first published timestamp when it is later than zero', async ({ page }) => {
  await openMovie(page, {
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      presentationOriginSeconds: 0.083333,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsConfigs.length)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0].startPosition)).toBe(0.083333);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments[0].timelineOffset)).toBe(0.083333);
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions[0])).toBe(0.083333);
});

test('uses the server presentation origin without deriving a nominal window origin', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      presentationOriginSeconds: targetSeconds > 0 ? 7168.25 : 0,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.currentTime = 7199;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 7199]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.at(-1).timelineOffset)).toBe(7168.25);
  await expect.poll(() => page.evaluate(() => window.__hlsConfigs.at(-1).startPosition)).toBe(7199);
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions.at(-1))).toBe(7199);
});

test('rejects seekable playback when the server omits the presentation origin', async ({ page }) => {
  await openMovie(page, {
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect(page.locator('#trackMsg')).toContainText('invalid presentation origin');
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(0);
});

test('keeps the same player and buffers an unmaterialized HLS position', async ({ page }) => {
  let releasePrepare;
  const prepareBlocked = new Promise(resolve => { releasePrepare = resolve; });
  const prepareStarts = await openMovie(page, {
    beforePrepare: start => start > 0 ? prepareBlocked : undefined,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => prepareStarts.length).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeSeek = video;
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 7199 });
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 7199]);
  await expect.poll(() => page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  releasePrepare();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.at(-1))).toEqual({
    duration,
    timelineOffset: 7170,
  });
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions.at(-1))).toBe(7199);
  await expect.poll(() => page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(7199);
  await expect.poll(() => page.locator('video').evaluate(video =>
    video.buffered.length === 1 && video.buffered.start(0) <= video.currentTime && video.buffered.end(0) > video.currentTime,
  )).toBe(true);
});

test('does not reattach a stable server presentation after a cold seek', async ({ page }) => {
  const prepareStarts = await openMovie(page, { stableStream: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeStableSeek = video;
    video.currentTime = 7199;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 7199]);
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads.at(-1))).toBe(7199);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeStableSeek)).toBe(true);
});

test('reattaches a stable stream when its playlist generation changed', async ({ page }) => {
  let generation = 'first';
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      presentationOriginSeconds: 0,
      generation,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  generation = 'second';
  await page.locator('video').evaluate(video => {
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 600]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(1);
});

test('accepts a seek before the first media fragment is buffered', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    bufferInitialFragment: false,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 600]);
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads.at(-1))).toBe(600);
});

test('does not snap back or replace the player while a clicked position is loading', async ({ page }) => {
  let targetReady = false;
  const prepareStarts = await openMovie(page, {
    getHlsStatus: targetSeconds => ({
      phase: targetSeconds > 0 && !targetReady ? 'preparing' : 'ready',
      targetSeconds,
      presentationOriginSeconds: Math.floor(Math.floor(targetSeconds / 2) / 15) * 30,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeSeek = video;
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 7199 });
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 7199]);
  await page.waitForTimeout(750);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  await expect.poll(() => page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(7199);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);

  targetReady = true;
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(1);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(7199);
});

test('only the latest cold seek may replace the active presentation', async ({ page }) => {
  let releaseFirstSeek;
  const firstSeekBlocked = new Promise(resolve => { releaseFirstSeek = resolve; });
  const prepareStarts = await openMovie(page, {
    beforePrepare: start => start === 100 ? firstSeekBlocked : undefined,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.currentTime = 100;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 100]);

  await page.locator('video').evaluate(video => {
    video.currentTime = 200;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 100, 200]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(200);

  releaseFirstSeek();
  await page.waitForTimeout(50);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
});

test('coalesces a burst of nonsequential seeks and keeps only the final target', async ({ page }) => {
  const prepareStarts = await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeSeekBurst = video;
    for (const target of [100, 900, 200, 800, 300, 700, 400, 600, 500, 1000]) {
      video.currentTime = target;
      video.dispatchEvent(new Event('seeking'));
    }
  });

  await expect.poll(() => prepareStarts.at(-1)).toBe(1000);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  expect(prepareStarts).toEqual([0, 1000]);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeekBurst)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(1000);
});

test('coalesces repeated seeking events for the same cold position', async ({ page }) => {
  let releasePrepare;
  const prepareBlocked = new Promise(resolve => { releasePrepare = resolve; });
  const prepareStarts = await openMovie(page, {
    beforePrepare: start => start === 100 ? prepareBlocked : undefined,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeRepeatedSeek = video;
    video.currentTime = 100;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 100]);
  await page.locator('video').evaluate(video => video.dispatchEvent(new Event('seeking')));
  await page.waitForTimeout(100);
  expect(prepareStarts).toEqual([0, 100]);
  expect(await page.evaluate(() => window.__hlsStopLoads)).toBe(1);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeRepeatedSeek)).toBe(true);
  releasePrepare();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(100);
});

test('ignores a media attachment seek to the live edge', async ({ page }) => {
  const prepareStarts = await openMovie(page, { simulateAttachSeekToEnd: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await page.waitForTimeout(100);
  expect(prepareStarts).toEqual([0, 600]);
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions.at(-1))).toBe(600);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(600);
});

test('cancels an unready forward seek when returning to retained history', async ({ page }) => {
  let releasePrepare;
  const prepareBlocked = new Promise(resolve => { releasePrepare = resolve; });
  const prepareStarts = await openMovie(page, {
    beforePrepare: start => start > 0 ? prepareBlocked : undefined,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeSeek = video;
    window.__hlsInstances.at(-1).latestLevelDetails = {
      fragments: Array.from({ length: 31 }, (_, index) => ({ start: index * 2, duration: 2 })),
    };
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 48, end: () => 52 },
    });
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 7199 });
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 7199]);

  await page.locator('video').evaluate(video => {
    video.currentTime = 24;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads)).toEqual([24]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  await expect.poll(() => page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(24);

  releasePrepare();
  await page.waitForTimeout(50);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
});

test('seeking back to zero creates a fresh presentation instead of jumping to the old live edge', async ({ page }) => {
  const prepareStarts = await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    const activeHls = window.__hlsInstances.at(-1);
    activeHls.latestLevelDetails = { fragments: [{ start: 16, duration: 2 }] };
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 16, end: () => 18 },
    });
    video.currentTime = 0;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 0]);
  await expect.poll(() => page.evaluate(() => window.__hlsSources.length)).toBe(2);
  const sources = await page.evaluate(() => window.__hlsSources.map(source => source.split('?')[0]));
  expect(new Set(sources).size).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(0);
});

test('seeking within retained HLS history reuses the current presentation', async ({ page }) => {
  const prepareStarts = await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    const activeHls = window.__hlsInstances.at(-1);
    activeHls.latestLevelDetails = {
      fragments: Array.from({ length: 16 }, (_, index) => ({ start: index * 2, duration: 2 })),
    };
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 8, end: () => 12 },
    });
    video.currentTime = 0;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(0);
});

test('does not chase the live edge when the prepared window advances', async ({ page }) => {
  await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    Object.defineProperty(video, 'currentTime', { configurable: true, writable: true, value: 50 });
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 48, end: () => 62 },
    });
    window.__hlsInstances.at(-1).slideLiveWindow(70);
  });

  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(50);
});

test('surfaces a failed torrent request without retrying it in a loop', async ({ page }) => {
  let requests = 0;
  await openMovie(page, {
    fileListError: 'unsupported tracker scheme',
    onFileListRequest: () => requests++,
  });

  await expect(page.locator('#magnetMsg')).toHaveText('unsupported tracker scheme');
  await page.waitForTimeout(500);
  expect(requests).toBe(1);
});

test('keeps unknown-duration playback progressive instead of inventing a full timeline', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    mediaInfo: { duration: 0, seekable: false },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments[0])).toEqual({
    duration: undefined,
    timelineOffset: undefined,
  });
  await page.locator('video').evaluate(video => {
    video.currentTime = 50;
    video.dispatchEvent(new Event('seeking'));
  });
  await page.waitForTimeout(100);
  expect(prepareStarts).toEqual([0]);
});

test('surfaces a terminal HLS preparation error without attaching a broken presentation', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    getHlsStatus: targetSeconds => ({
      phase: 'error',
      targetSeconds,
      message: 'required torrent pieces are unavailable',
      activePeers: 0,
      totalPeers: 0,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect(page.locator('#trackMsg')).toHaveText('required torrent pieces are unavailable');
  await expect(page.locator('#warnMsg')).toBeHidden();
  expect(prepareStarts).toEqual([0]);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(0);
});
