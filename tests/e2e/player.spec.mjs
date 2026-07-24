import { expect, test } from '@playwright/test';

const duration = 2 * 60 * 60;

const fakeHls = String.raw`
  class FakeHls {
    static Events = {
      MANIFEST_PARSED: 'manifestParsed',
      LEVEL_LOADED: 'levelLoaded',
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
      this.subtitleTracks = Array.from({ length: window.__hlsSubtitleTrackCount || 0 }, () => ({}));
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
      media.play = () => {
        window.__videoPlayCalls++;
        if (window.__blockAutoplayOnce && !window.__autoplayRejected) {
          window.__autoplayRejected = true;
          return Promise.reject(new DOMException('Autoplay blocked', 'NotAllowedError'));
        }
        return Promise.resolve();
      };
      media.requestVideoFrameCallback = callback => {
        const id = ++window.__videoFrameCallbackID;
        const timer = setTimeout(() => {
          if (window.__cancelledVideoFrameCallbacks.has(id)) return;
          window.__presentedFrameCallbacks++;
          if (window.__freezeVideoFrames && window.__presentedFrameCallbacks > 1) return;
          const queuedMediaTime = window.__queuedVideoFrameMediaTimes.shift();
          callback(performance.now(), {
            mediaTime: Number.isFinite(queuedMediaTime) ? queuedMediaTime : Number(media.currentTime) || 0,
            presentedFrames: window.__presentedFrameCallbacks,
          });
        }, 100);
        window.__videoFrameTimers.set(id, timer);
        return id;
      };
      media.cancelVideoFrameCallback = id => {
        window.__cancelledVideoFrameCallbacks.add(id);
        clearTimeout(window.__videoFrameTimers.get(id));
      };
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
        const earlySeek = Number(media.currentTime) || 0;
        const start = this.config.timelineOffset || 0;
        this.latestLevelDetails = { fragments: [{ start, duration: 2 }] };
        this.emit(FakeHls.Events.MANIFEST_PARSED);
        this.emit(FakeHls.Events.LEVEL_LOADED);
        const requestedLoad = Number(this.startLoadPosition);
        const loadPosition = requestedLoad >= 0
          ? requestedLoad + start
          : earlySeek > 0.25
          ? earlySeek
          : requestedStart;
        window.__hlsLoadPositions.push(loadPosition);
        this.latestLevelDetails = {
          fragments: [{ start, duration: Math.max(2, requestedStart - start + 2) }],
        };
        if (window.__simulateAttachSeekToEnd && requestedStart > 0 && Number.isFinite(requestedDuration)) {
          media.currentTime = requestedDuration;
          media.dispatchEvent(new Event('seeking'));
        }
        if (window.__bufferInitialFragment && loadPosition <= requestedStart + 0.25) {
          media.currentTime = requestedStart;
          const bufferStart = requestedStart + (requestedStart > 0 ? window.__hlsBufferStartOffset : 0);
          const fragmentDuration = window.__initialBufferSeconds;
          Object.defineProperty(media, 'buffered', {
            configurable: true,
            value: { length: 1, start: () => bufferStart, end: () => requestedStart + fragmentDuration },
          });
          const frag = {
            type: 'main',
            duration: 2,
            stats: { loading: { start: 0, end: window.__fragmentLoadMs } },
          };
          if (window.__fragmentLoadMs > 0) this.emit(FakeHls.Events.FRAG_LOADED, { frag });
          this.emit(FakeHls.Events.FRAG_BUFFERED, { frag });
        }
      };
      queueMicrotask(finishAttachment);
    }
    stopLoad() { window.__hlsStopLoads++; }
    startLoad(position) {
      this.startLoadPosition = position;
      window.__hlsStartLoads.push(position);
    }
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
  prepareResponse,
  fileListError,
  getHlsStatus,
  mediaInfo = {},
  onFileListRequest,
  simulateAttachSeekToEnd = false,
  stableStream = false,
  bufferInitialFragment = true,
  bufferStartOffset = 0,
  hlsSubtitleTrackCount = 0,
  freezeVideoFrames = false,
  suppressVideoFrames = false,
  fragmentLoadMs = 0,
  initialBufferSeconds = 2,
  blockAutoplayOnce = false,
} = {}) {
  const prepareStarts = [];
  let presentationGeneration = 0;

  await page.addInitScript(({ simulateEndSeek, bufferInitial, bufferOffset, subtitleTrackCount, freezeFrames, suppressFrames, loadMs, bufferSeconds, blockAutoplay }) => {
    window.__simulateAttachSeekToEnd = simulateEndSeek;
    window.__bufferInitialFragment = bufferInitial;
    window.__hlsBufferStartOffset = bufferOffset;
    window.__hlsSubtitleTrackCount = subtitleTrackCount;
    window.__hlsConfigs = [];
    window.__hlsInstances = [];
    window.__hlsSources = [];
    window.__hlsAttachments = [];
    window.__hlsLoadPositions = [];
    window.__hlsStopLoads = 0;
    window.__hlsStartLoads = [];
    window.__hlsDestroyCount = 0;
    window.__videoPlayCalls = 0;
    window.__freezeVideoFrames = freezeFrames;
    window.__suppressVideoFrames = suppressFrames;
    window.__fragmentLoadMs = loadMs;
    window.__initialBufferSeconds = bufferSeconds;
    window.__blockAutoplayOnce = blockAutoplay;
    window.__autoplayRejected = false;
    window.__videoFrameCallbackID = 0;
    window.__presentedFrameCallbacks = 0;
    window.__queuedVideoFrameMediaTimes = [];
    window.__cancelledVideoFrameCallbacks = new Set();
    window.__videoFrameTimers = new Map();
    window.__qoeSummaries = [];
    window.__qoeEventTimes = [];
    window.addEventListener('cinemator:qoe', event => {
      window.__qoeSummaries.push(event.detail);
      window.__qoeEventTimes.push(performance.now());
    });
    Object.defineProperty(HTMLVideoElement.prototype, 'requestVideoFrameCallback', {
      configurable: true,
      value(callback) {
        const media = this;
        const id = ++window.__videoFrameCallbackID;
        const timer = setTimeout(() => {
          if (window.__cancelledVideoFrameCallbacks.has(id)) return;
          if (window.__suppressVideoFrames) return;
          window.__presentedFrameCallbacks++;
          if (window.__freezeVideoFrames && window.__presentedFrameCallbacks > 1) return;
          const queuedMediaTime = window.__queuedVideoFrameMediaTimes.shift();
          callback(performance.now(), {
            mediaTime: Number.isFinite(queuedMediaTime) ? queuedMediaTime : Number(media.currentTime) || 0,
            presentedFrames: window.__presentedFrameCallbacks,
          });
        }, 100);
        window.__videoFrameTimers.set(id, timer);
        return id;
      },
    });
    Object.defineProperty(HTMLVideoElement.prototype, 'cancelVideoFrameCallback', {
      configurable: true,
      value(id) {
        window.__cancelledVideoFrameCallbacks.add(id);
        clearTimeout(window.__videoFrameTimers.get(id));
      },
    });
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
  }, {
    simulateEndSeek: simulateAttachSeekToEnd,
    bufferInitial: bufferInitialFragment,
    bufferOffset: bufferStartOffset,
    subtitleTrackCount: hlsSubtitleTrackCount,
    freezeFrames: freezeVideoFrames,
    suppressFrames: suppressVideoFrames,
    loadMs: fragmentLoadMs,
    bufferSeconds: initialBufferSeconds,
    blockAutoplay: blockAutoplayOnce,
  });

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
      const response = await prepareResponse?.(start, prepareStarts.length);
      if (response) {
        await route.fulfill(response);
        return;
      }
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
      const stream = decodeURIComponent(url.pathname.slice('/api/hls/status/'.length));
      const generation = stableStream ? 'generation-stable' : `generation-${presentationGeneration}`;
      const customStatus = await getHlsStatus?.(targetSeconds, { stream, generation });
      if (customStatus?.httpStatus) {
        await route.fulfill({
          status: customStatus.httpStatus,
          body: customStatus.body || `Status request failed (${customStatus.httpStatus})`,
        });
        return;
      }
      await route.fulfill({
        json: customStatus ? { generation, ...customStatus } : {
          phase: 'ready',
          targetSeconds,
          presentationOriginSeconds: stableStream
            ? 0
            : Math.floor(Math.floor(targetSeconds / 2) / 15) * 30,
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

async function openHourlyMovieWithPendingMiddle(page) {
  const middle = 30 * 60;
  let middleReady = false;
  const prepareStarts = await openMovie(page, {
    mediaInfo: { duration: 60 * 60 },
    initialBufferSeconds: 18,
    getHlsStatus: targetSeconds => ({
      phase: targetSeconds === middle && !middleReady ? 'preparing' : 'ready',
      targetSeconds,
      presentationOriginSeconds: Math.floor(Math.floor(targetSeconds / 2) / 15) * 30,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  return {
    middle,
    prepareStarts,
    finishPreparation() {
      middleReady = true;
    },
  };
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

test('bounds the HLS buffer by duration and bytes instead of a fixed 12 seconds', async ({ page }) => {
  await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0])).toMatchObject({
    maxBufferLength: 30,
    maxMaxBufferLength: 60,
    maxBufferSize: 128 * 1024 * 1024,
    liveSyncDurationCount: 1,
    liveMaxLatencyDurationCount: 1_000_000,
    maxLiveSyncPlaybackRate: 1,
    maxBufferHole: 0,
    nudgeMaxRetry: 0,
    nudgeOnVideoHole: false,
  });
  await expect.poll(() => page.evaluate(() => new URL(window.__hlsSources[0], location.href).searchParams.get('v'))).toBe('generation-1');
});

test('reduces 8K forward duration to respect the client byte ceiling', async ({ page }) => {
  await openMovie(page, { mediaInfo: { width: 7680, height: 4320, bitrate: 128_000_000 } });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0]?.maxBufferSize)).toBe(128 * 1024 * 1024);
  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0]?.maxBufferLength)).toBeCloseTo(8.39, 1);
  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0]?.maxMaxBufferLength)).toBeCloseTo(8.39, 1);
});

test('starts from the first buffered fragment when later delivery is marginal', async ({ page }) => {
  await openMovie(page, { fragmentLoadMs: 1900 });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsConfigs[0]?.maxBufferLength)).toBe(60);
  await expect.poll(() => page.evaluate(() => window.__videoPlayCalls)).toBe(1);
  await expect(page.locator('#warn-title')).not.toContainText('Building');
});

test('keeps startup and the source timeline active across a nonfatal playlist reload error', async ({ page }) => {
  await openMovie(page, { bufferInitialFragment: false, suppressVideoFrames: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    const hls = window.__hlsInstances[0];
    hls.emit(window.Hls.Events.ERROR, {
      fatal: false,
      details: 'levelParsingError',
    });
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 0.1, end: () => 2 },
    });
    hls.emit(window.Hls.Events.FRAG_BUFFERED, { frag: { type: 'main' } });
  });

  await expect.poll(() => page.evaluate(() => window.__videoPlayCalls)).toBe(1);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect(page.locator('#playerMsg')).not.toContainText('Playback error');
});

test('reprepares a missing HLS session once and stops polling stale descriptors', async ({ page }) => {
  const statusRequests = new Map();
  const prepareStarts = await openMovie(page, {
    bufferInitialFragment: false,
    suppressVideoFrames: true,
    getHlsStatus: (targetSeconds, { stream }) => {
      const requests = (statusRequests.get(stream) || 0) + 1;
      statusRequests.set(stream, requests);
      if (requests > 1) {
        return { httpStatus: 404, body: 'HLS stream not found' };
      }
      return {
        phase: 'ready',
        stage: 'ready',
        targetSeconds,
        presentationOriginSeconds: 0,
      };
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => prepareStarts).toEqual([0, 0]);
  await expect(page.locator('#playerMsg')).toContainText('Playback error: HLS stream not found');
  await page.waitForTimeout(1500);
  expect(prepareStarts).toEqual([0, 0]);
  expect([...statusRequests.values()]).toEqual([2, 2]);
});

test('reprepares once when the presentation changes before the first frame', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    bufferInitialFragment: false,
    suppressVideoFrames: true,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.evaluate(() => {
    window.__hlsInstances[0].emit(window.Hls.Events.ERROR, {
      fatal: true,
      details: 'levelLoadError',
      response: { code: 409 },
    });
  });

  await expect.poll(() => prepareStarts).toEqual([0, 0]);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);

  await page.evaluate(() => {
    window.__hlsInstances[1].emit(window.Hls.Events.ERROR, {
      fatal: true,
      details: 'levelLoadError',
      response: { code: 409 },
    });
  });
  await page.waitForTimeout(500);

  expect(prepareStarts).toEqual([0, 0]);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(1);
  await expect(page.locator('#playerMsg')).toContainText('The stream presentation changed before the player loaded it');
});

test('retries a temporary HLS status failure without rebuilding the stream', async ({ page }) => {
  let statusRequests = 0;
  const prepareStarts = await openMovie(page, {
    getHlsStatus: targetSeconds => {
      statusRequests++;
      if (statusRequests === 1) {
        return { httpStatus: 503, body: 'HLS is temporarily unavailable' };
      }
      return {
        phase: 'ready',
        stage: 'ready',
        targetSeconds,
        presentationOriginSeconds: 0,
      };
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  expect(prepareStarts).toEqual([0]);
  expect(statusRequests).toBeGreaterThanOrEqual(2);
  await expect(page.locator('#playerMsg')).not.toContainText('Playback error');
});

test('does not turn an attachment seek after a control click into a new presentation', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    bufferInitialFragment: false,
    suppressVideoFrames: true,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.dispatchEvent(new Event('play'));
    video.currentTime = 0.1;
    video.dispatchEvent(new Event('seeking'));
    video.dispatchEvent(new PointerEvent('pointerup'));
  });

  await page.waitForTimeout(500);
  expect(prepareStarts).toEqual([0]);
  expect(await page.evaluate(() => window.__hlsStopLoads)).toBe(0);
  await expect(page.locator('#playerMsg')).not.toContainText('Restoring stream');
});

test('detects frozen video frames even while the player clock can advance', async ({ page }) => {
  const prepareStarts = await openMovie(page, { freezeVideoFrames: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect(page.locator('#warn-title')).toContainText('Playback stalled', { timeout: 2500 });
  await expect.poll(() => page.evaluate(() => window.__qoeSummaries.at(-1))).toMatchObject({
    playbackStallCount: 1,
    stallFreeSession: false,
  });
  expect(prepareStarts).toEqual([0]);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
});

test('waits for the next fragment without rebuilding the stream on buffer underrun', async ({ page }) => {
  const prepareStarts = await openMovie(page, { freezeVideoFrames: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThanOrEqual(1);

  await page.locator('video').evaluate(video => {
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 0, start: () => 0, end: () => 0 },
    });
  });

  await expect.poll(() => page.evaluate(() => window.__qoeSummaries.at(-1)), { timeout: 6000 }).toMatchObject({
    playbackStallCount: 1,
    rebufferCount: 1,
  });
  expect(prepareStarts).toEqual([0]);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
});

test('never replaces or rewinds active playback when the next HLS fragment is delayed', async ({ page }) => {
  const prepareStarts = await openMovie(page, { initialBufferSeconds: 18 });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThanOrEqual(1);

  await page.locator('video').evaluate(video => {
    window.__activeVideoBeforeUnderrun = video;
    video.currentTime = 16;
    video.dispatchEvent(new Event('timeupdate'));
  });
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(16);

  await page.locator('video').evaluate(video => {
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 0, start: () => 0, end: () => 0 },
    });
    video.dispatchEvent(new Event('waiting'));
    window.__hlsInstances[0].emit(window.Hls.Events.ERROR, {
      fatal: true,
      details: 'levelLoadError',
      response: { code: 409 },
    });
  });

  await page.waitForTimeout(500);
  expect(prepareStarts).toEqual([0]);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  expect(await page.evaluate(() => document.getElementById('video') === window.__activeVideoBeforeUnderrun)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(16);
});

test('restores the last presented position after an unsolicited media seek', async ({ page }) => {
  const prepareStarts = await openMovie(page, { initialBufferSeconds: 18 });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThanOrEqual(1);

  await page.locator('video').evaluate(async video => {
    video.currentTime = 16;
    await new Promise(resolve => setTimeout(resolve, 150));
    video.currentTime = 0;
    video.dispatchEvent(new Event('seeking'));
  });

  expect(prepareStarts).toEqual([0]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(16);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
});

test('falls back when the media clock advances before any video frame is decoded', async ({ page }) => {
  const prepareRequests = [];
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.pathname === '/api/hls/prepare') prepareRequests.push(url);
  });
  await openMovie(page, { suppressVideoFrames: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__videoPlayCalls)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__unpresentedClock = setInterval(() => { video.currentTime += 0.1; }, 100);
  });

  await expect.poll(() => prepareRequests.length, { timeout: 4000 }).toBe(2);
  expect(prepareRequests[1].searchParams.get('transcode')).toBe('1');
  await expect(page.locator('#mediaDecision')).toContainText('compatibility fallback');
  await page.evaluate(() => clearInterval(window.__unpresentedClock));
});

test('records autoplay rejection and restarts timing from the confirming play action', async ({ page }) => {
  await openMovie(page, { blockAutoplayOnce: true });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect(page.locator('#playerMsg')).toContainText('Press Play to begin');
  await expect.poll(() => page.evaluate(() => window.__qoeSummaries.at(-1)?.attempts.at(-1)?.result))
    .toBe('autoplay_blocked');

  await page.locator('video').evaluate(async video => {
    video.dispatchEvent(new Event('play'));
    await video.play();
  });
  await page.waitForTimeout(250);
  await page.locator('video').evaluate(video => video.dispatchEvent(new Event('pause')));
  await expect.poll(() => page.evaluate(() => window.__qoeSummaries.at(-1)?.attempts.map(attempt => attempt.result)), { timeout: 7000 })
    .toEqual(['autoplay_blocked', 'presented']);
  const attempts = await page.evaluate(() => window.__qoeSummaries.at(-1).attempts);
  expect(attempts.map(attempt => attempt.id)).toEqual([1, 2]);
  expect(attempts.map(attempt => attempt.kind)).toEqual(['startup', 'autoplay_resume']);
  const publishTimes = await page.evaluate(() => window.__qoeEventTimes);
  expect(publishTimes.at(-1) - publishTimes.at(-2)).toBeGreaterThanOrEqual(4900);
});

test('explains source waiting separately from media packaging', async ({ page }) => {
  let ready = false;
  await openMovie(page, {
    getHlsStatus: targetSeconds => ready ? {
      phase: 'ready',
      stage: 'ready',
      targetSeconds,
      presentationOriginSeconds: 0,
      activePeers: 2,
      totalPeers: 4,
    } : {
      phase: 'preparing',
      stage: 'waiting_source',
      targetSeconds,
      peerBytes: 8 * 1024 * 1024,
      sourceRateBitsPerSecond: 12_000_000,
      cacheBytes: 4 * 1024 * 1024,
      missingPieces: 3,
      rangePieces: 8,
      activePeers: 2,
      totalPeers: 4,
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();

  await expect(page.locator('#warn-title')).toContainText('Waiting for source');
  await expect(page.locator('#warn-status')).toContainText('8.0 MB from peers');
  await expect(page.locator('#warn-status')).toContainText('12.0 Mbps source rate');
  await expect(page.locator('#warn-status')).toContainText('3/8 pieces missing');
  ready = true;
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
});

test('enables the selected text subtitle rendition', async ({ page }) => {
  await openMovie(page, {
    hlsSubtitleTrackCount: 1,
    mediaInfo: { subtitles: [{ index: 0, codec: 'subrip', language: 'eng' }] },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();

  await expect.poll(() => page.evaluate(() => window.__hlsInstances.at(-1)?.subtitleTrack)).toBe(0);
  await expect.poll(() => page.evaluate(() => window.__hlsInstances.at(-1)?.subtitleDisplay)).toBe(true);
});

test('does not attach playback until the selected subtitle target is ready', async ({ page }) => {
  let subtitlesReady = false;
  await openMovie(page, {
    hlsSubtitleTrackCount: 1,
    mediaInfo: { subtitles: [{ index: 0, codec: 'subrip', language: 'eng' }] },
    getHlsStatus: targetSeconds => subtitlesReady ? {
      phase: 'ready',
      stage: 'ready',
      targetSeconds,
      presentationOriginSeconds: 0,
      activePeers: 1,
      totalPeers: 1,
    } : {
      phase: 'preparing',
      stage: 'queued',
      message: 'Preparing selected subtitles',
      targetSeconds,
      activePeers: 1,
      totalPeers: 1,
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();

  await expect(page.locator('#warn-detail')).toContainText('Preparing selected subtitles');
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(0);

  subtitlesReady = true;
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  await expect.poll(() => page.evaluate(() => window.__hlsInstances.at(-1)?.subtitleTrack)).toBe(0);
  await expect.poll(() => page.evaluate(() => window.__hlsInstances.at(-1)?.subtitleDisplay)).toBe(true);
});

test('stops instead of silently continuing without selected subtitles', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    hlsSubtitleTrackCount: 1,
    mediaInfo: { subtitles: [{ index: 0, codec: 'subrip', language: 'eng' }] },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsInstances.length)).toBe(1);

  await page.evaluate(() => {
    window.__hlsInstances[0].emit(window.Hls.Events.ERROR, {
      fatal: false,
      details: 'fragLoadError',
      frag: { type: 'subtitle' },
    });
  });
  await expect(page.locator('#playerMsg')).not.toContainText('Subtitle error');

  await page.evaluate(() => {
    window.__hlsInstances[0].emit(window.Hls.Events.ERROR, {
      fatal: true,
      details: 'fragLoadError',
      frag: { type: 'subtitle' },
    });
  });
  await expect(page.locator('#playerMsg')).toContainText('The selected subtitles could not be loaded');
  await expect.poll(() => page.locator('video').evaluate(video => video.paused)).toBe(true);
  expect(prepareStarts).toEqual([0]);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
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
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads[0])).toBe(0);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 7199;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 7199]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.at(-1).timelineOffset)).toBe(7168.25);
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads.at(-1))).toBe(0);
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions.at(-1))).toBe(7168.25);
});

test('starts from the closest decoded frame after a 22 minute seek', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    bufferStartOffset: 0.062,
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      presentationOriginSeconds: targetSeconds > 0 ? 1319.109 : 0,
      bytesRead: 1024,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  const initialPlayCalls = await page.evaluate(() => window.__videoPlayCalls);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 1320;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 1320]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(1320.062);
  await expect.poll(() => page.evaluate(() => window.__videoPlayCalls)).toBeGreaterThan(initialPlayCalls);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
  await expect.poll(() => page.evaluate(() => window.__hlsLoadPositions.at(-1))).toBe(7170);
  await expect.poll(() => page.evaluate(() => document.getElementById('video') === window.__videoBeforeSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(7199);
  await expect.poll(() => page.locator('video').evaluate(video =>
    video.buffered.length === 1 && video.buffered.start(0) <= video.currentTime && video.buffered.end(0) > video.currentTime,
  )).toBe(true);
});

test('does not reattach a stable server presentation after a cold seek', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    getHlsStatus: targetSeconds => ({
      phase: 'ready',
      targetSeconds,
      presentationOriginSeconds: targetSeconds,
      generation: 'generation-stable',
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeStableSeek = video;
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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

test('keeps an unbuffered seek committed when media snaps back after the gesture', async ({ page }) => {
  const pending = await openHourlyMovieWithPendingMiddle(page);

  await page.locator('video').evaluate((video, middle) => {
    video.currentTime = 1;
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = middle;
    video.dispatchEvent(new Event('seeking'));
    video.dispatchEvent(new PointerEvent('pointerup'));

    // A browser or hls.js can briefly restore the last buffered playhead
    // before the distant presentation is ready. This is not a second user seek.
    video.currentTime = 1;
    video.dispatchEvent(new Event('seeking'));
  }, pending.middle);

  await expect.poll(() => pending.prepareStarts).toEqual([0, pending.middle]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  pending.finishPreparation();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
});

test('keeps a committed seek authoritative over a late frame from the old position', async ({ page }) => {
  const pending = await openHourlyMovieWithPendingMiddle(page);
  const video = page.locator('video');

  const initialFrames = await page.evaluate(() => window.__presentedFrameCallbacks);
  await video.evaluate(element => { element.currentTime = 16; });
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThan(initialFrames);

  const framesBeforeSeek = await page.evaluate(() => window.__presentedFrameCallbacks);
  await video.evaluate((element, middle) => {
    window.__queuedVideoFrameMediaTimes.push(16);
    element.dispatchEvent(new PointerEvent('pointerdown'));
    element.currentTime = middle;
    element.dispatchEvent(new Event('seeking'));
    element.dispatchEvent(new PointerEvent('pointerup'));
  }, pending.middle);
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThan(framesBeforeSeek);

  await video.evaluate(element => {
    element.currentTime = 16;
    element.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => pending.prepareStarts).toEqual([0, pending.middle]);
  await expect.poll(() => video.evaluate(element => element.currentTime)).toBe(pending.middle);
});

test('returns playhead ownership to presented frames after a committed seek', async ({ page }) => {
  const pending = await openHourlyMovieWithPendingMiddle(page);
  const video = page.locator('video');

  await video.evaluate((element, middle) => {
    element.dispatchEvent(new PointerEvent('pointerdown'));
    element.currentTime = middle;
    element.dispatchEvent(new Event('seeking'));
    element.dispatchEvent(new PointerEvent('pointerup'));
  }, pending.middle);
  await expect.poll(() => pending.prepareStarts).toEqual([0, pending.middle]);

  pending.finishPreparation();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => video.evaluate(element => element.currentTime)).toBe(pending.middle);

  const framesAtTarget = await page.evaluate(() => window.__presentedFrameCallbacks);
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThan(framesAtTarget);

  const framesBeforeProgress = await page.evaluate(() => window.__presentedFrameCallbacks);
  await video.evaluate((element, nextTime) => { element.currentTime = nextTime; }, pending.middle + 2);
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThan(framesBeforeProgress);

  await video.evaluate((element, oldTime) => {
    element.currentTime = oldTime;
    element.dispatchEvent(new Event('seeking'));
  }, pending.middle);
  await expect.poll(() => video.evaluate(element => element.currentTime)).toBe(pending.middle + 2);
});

test('accepts one native seek emitted after pointerup without accepting its snapback', async ({ page }) => {
  const pending = await openHourlyMovieWithPendingMiddle(page);

  await page.locator('video').evaluate((video, middle) => {
    video.currentTime = 1;
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.dispatchEvent(new PointerEvent('pointerup'));

    // Some native controls commit their selected position only after pointerup.
    video.currentTime = middle;
    video.dispatchEvent(new Event('seeking'));
    video.currentTime = 1;
    video.dispatchEvent(new Event('seeking'));
  }, pending.middle);

  await expect.poll(() => pending.prepareStarts).toEqual([0, pending.middle]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  pending.finishPreparation();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
});

test('keeps a keyboard seek committed when media snaps back', async ({ page }) => {
  const pending = await openHourlyMovieWithPendingMiddle(page);

  await page.locator('video').evaluate((video, middle) => {
    video.currentTime = 1;
    video.dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight' }));
    video.currentTime = middle;
    video.dispatchEvent(new Event('seeking'));
    video.currentTime = 1;
    video.dispatchEvent(new Event('seeking'));
  }, pending.middle);

  await expect.poll(() => pending.prepareStarts).toEqual([0, pending.middle]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  pending.finishPreparation();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(pending.middle);
});

test('revokes seek intent when a pointer gesture is cancelled', async ({ page }) => {
  const prepareStarts = await openMovie(page, { initialBufferSeconds: 18 });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThanOrEqual(1);

  const presentedFrames = await page.evaluate(() => window.__presentedFrameCallbacks);
  await page.locator('video').evaluate(video => { video.currentTime = 16; });
  await expect.poll(() => page.evaluate(() => window.__presentedFrameCallbacks)).toBeGreaterThan(presentedFrames);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.dispatchEvent(new PointerEvent('pointercancel'));
    video.currentTime = 30 * 60;
    video.dispatchEvent(new Event('seeking'));
  });

  expect(prepareStarts).toEqual([0]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(16);
  await expect.poll(() => page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
});

test('retries transient streaming capacity without replacing the player or losing the seek target', async ({ page }) => {
  let capacityFailures = 0;
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    prepareResponse: start => {
      if (start !== 600 || capacityFailures++ >= 2) return null;
      return {
        status: 503,
        headers: { 'Retry-After': '0.01' },
        body: 'The server is at its configured streaming capacity; retry shortly',
      };
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeCapacitySeek = video;
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 600, 600, 600]);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeCapacitySeek)).toBe(true);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(600);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
  await expect(page.locator('#playerMsg')).not.toContainText('configured streaming capacity');
});

test('cancels capacity retries when the user commits a newer seek', async ({ page }) => {
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    prepareResponse: start => start === 600 ? {
      status: 503,
      headers: { 'Retry-After': '1' },
      body: 'The server is at its configured streaming capacity; retry shortly',
    } : null,
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 600]);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 1200;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 600, 1200]);
  await page.waitForTimeout(1100);
  expect(prepareStarts).toEqual([0, 600, 1200]);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(1200);
});

test('keeps a cold seek pending until its selected subtitle segment is ready', async ({ page }) => {
  let distantSubtitlesReady = false;
  const prepareStarts = await openMovie(page, {
    stableStream: true,
    hlsSubtitleTrackCount: 1,
    mediaInfo: { subtitles: [{ index: 0, codec: 'subrip', language: 'eng' }] },
    getHlsStatus: targetSeconds => ({
      phase: targetSeconds === 600 && !distantSubtitlesReady ? 'preparing' : 'ready',
      stage: targetSeconds === 600 && !distantSubtitlesReady ? 'queued' : 'ready',
      message: targetSeconds === 600 && !distantSubtitlesReady ? 'Preparing selected subtitles' : '',
      targetSeconds,
      presentationOriginSeconds: 0,
      activePeers: 1,
      totalPeers: 1,
    }),
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__videoBeforeSubtitleSeek = video;
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 600]);
  await expect(page.locator('#warn-detail')).toContainText('Preparing selected subtitles');
  expect(await page.evaluate(() => window.__hlsAttachments.length)).toBe(1);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeSubtitleSeek)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(600);

  distantSubtitlesReady = true;
  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads.at(-1))).toBe(600);
  await expect.poll(() => page.evaluate(() => window.__hlsInstances.at(-1)?.subtitleTrack)).toBe(0);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 100;
    video.dispatchEvent(new Event('seeking'));
  });
  await expect.poll(() => prepareStarts).toEqual([0, 100]);

  await page.locator('video').evaluate(video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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

test('coalesces scrub positions emitted across multiple animation frames', async ({ page }) => {
  const statusTargets = [];
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.pathname.startsWith('/api/hls/status/')) statusTargets.push(Number(url.searchParams.get('target')));
  });
  const prepareStarts = await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(async video => {
    video.dispatchEvent(new PointerEvent('pointerdown'));
    for (const target of [100, 200, 300]) {
      video.currentTime = target;
      video.dispatchEvent(new Event('seeking'));
      video.dispatchEvent(new Event('seeked'));
      video.dispatchEvent(new Event('waiting'));
      video.dispatchEvent(new Event('stalled'));
      await new Promise(resolve => setTimeout(resolve, 100));
    }
  });

  await expect.poll(() => prepareStarts).toEqual([0, 300]);
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(2);
  await expect.poll(() => page.locator('video').evaluate(video => video.currentTime)).toBe(300);
  expect(statusTargets.filter(target => target > 0).every(target => target === 300)).toBe(true);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
    video.currentTime = 24;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => page.evaluate(() => window.__hlsStartLoads)).toEqual([0, 24]);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
  await expect.poll(
    () => page.evaluate(() => window.__qoeSummaries.at(-1)?.attempts.at(-1)?.seekClass),
    { timeout: 7000 },
  ).toBe('cached');
});

test('seeking within retained HLS history reuses the current presentation', async ({ page }) => {
  const prepareStarts = await openMovie(page);
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    const activeHls = window.__hlsInstances.at(-1);
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
  await expect.poll(
    () => page.evaluate(() => window.__qoeSummaries.at(-1)?.attempts.at(-1)?.seekClass),
    { timeout: 7000 },
  ).toBe('retained');
});

test('reports a seek that needs preparation as cold', async ({ page }) => {
  let coldPolls = 0;
  const prepareStarts = await openMovie(page, {
    getHlsStatus: targetSeconds => {
      if (targetSeconds < 1) {
        return { phase: 'ready', stage: 'ready', targetSeconds, presentationOriginSeconds: 0 };
      }
      coldPolls++;
      if (coldPolls === 1) {
        return { phase: 'preparing', stage: 'waiting_source', targetSeconds, activePeers: 1, totalPeers: 1 };
      }
      return { phase: 'ready', stage: 'ready', targetSeconds, presentationOriginSeconds: targetSeconds };
    },
  });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  await page.locator('video').evaluate(video => {
    window.__hlsInstances.at(-1).latestLevelDetails = { fragments: [{ start: 0, duration: 2 }] };
    video.dispatchEvent(new PointerEvent('pointerdown'));
    Object.defineProperty(video, 'buffered', {
      configurable: true,
      value: { length: 1, start: () => 0, end: () => 2 },
    });
    video.currentTime = 600;
    video.dispatchEvent(new Event('seeking'));
  });

  await expect.poll(() => prepareStarts).toEqual([0, 600]);
  await expect.poll(
    () => page.evaluate(() => window.__qoeSummaries.at(-1)?.attempts.at(-1)?.seekClass),
    { timeout: 7000 },
  ).toBe('cold');
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

test('keeps source time monotonic while the HLS presentation advances across five windows', async ({ page }) => {
  const prepareStarts = await openMovie(page, { initialBufferSeconds: 180 });
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await expect.poll(() => page.evaluate(() => window.__hlsAttachments.length)).toBe(1);

  const observed = await page.locator('video').evaluate(video => {
    window.__videoBeforeWindowAdvance = video;
    const values = [];
    for (const sourceTime of [1, 31, 61, 91, 121, 151]) {
      video.currentTime = sourceTime;
      video.dispatchEvent(new Event('timeupdate'));
      window.__hlsInstances.at(-1).slideLiveWindow(Math.max(0, sourceTime - 1));
      values.push(video.currentTime);
    }
    return values;
  });

  expect(observed).toEqual([1, 31, 61, 91, 121, 151]);
  expect(prepareStarts).toEqual([0]);
  expect(await page.evaluate(() => window.__hlsDestroyCount)).toBe(0);
  expect(await page.evaluate(() => document.getElementById('video') === window.__videoBeforeWindowAdvance)).toBe(true);
  await expect.poll(() => page.locator('video').evaluate(video => video.duration)).toBe(duration);
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
    video.dispatchEvent(new PointerEvent('pointerdown'));
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
