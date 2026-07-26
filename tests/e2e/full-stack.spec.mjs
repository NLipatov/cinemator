import { expect, test } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';

const appURL = 'http://127.0.0.1:4174';
const fixtureURL = 'http://127.0.0.1:4175/fixture';
const hlsSource = readFileSync(resolve('node_modules/hls.js/dist/hls.min.js'));

test.afterEach(async ({ page }) => {
  if (page.isClosed() || !page.url().startsWith(appURL)) return;
  const releaseRequest = page.waitForRequest(request =>
    new URL(request.url()).pathname === '/api/hls/release',
  );
  await page.goto('about:blank');
  expect((await releaseRequest).method()).toBe('POST');
});

async function decodedFrames(video) {
  return video.evaluate(element =>
    element.getVideoPlaybackQuality?.().totalVideoFrames ??
      element.webkitDecodedFrameCount ??
      0,
  );
}

async function playbackSnapshot(video) {
  return video.evaluate(element => {
    const quality = element.getVideoPlaybackQuality?.();
    return {
      currentTime: element.currentTime,
      duration: element.duration,
      paused: element.paused,
      ended: element.ended,
      seeking: element.seeking,
      readyState: element.readyState,
      networkState: element.networkState,
      decodedFrames: quality?.totalVideoFrames ?? element.webkitDecodedFrameCount ?? 0,
      droppedFrames: quality?.droppedVideoFrames ?? 0,
      buffered: Array.from(
        { length: element.buffered.length },
        (_, index) => [element.buffered.start(index), element.buffered.end(index)],
      ),
      tracks: Array.from(element.textTracks, track => ({
        mode: track.mode,
        cues: Array.from(track.cues || [], cue => ({
          start: cue.startTime,
          end: cue.endTime,
          text: cue.text,
        })),
      })),
      mediaError: element.error
        ? { code: element.error.code, message: element.error.message }
        : null,
      videoEvents: window.__videoEvents || [],
      hlsErrors: window.__hlsErrors || [],
      hlsSubtitleEvents: window.__hlsSubtitleEvents || [],
    };
  });
}

async function expectDecodedPlayback(
  video,
  minimumTime,
  cueText,
  cueStart,
  maximumTime = Number.POSITIVE_INFINITY,
) {
  try {
    await expect.poll(
      () => video.evaluate(element => element.currentTime),
      { timeout: 90_000 },
    ).toBeGreaterThan(minimumTime);
  } catch (error) {
    throw new Error(`${error.message}\nPlayback state: ${JSON.stringify(await playbackSnapshot(video))}`);
  }
  expect(await video.evaluate(element => element.currentTime)).toBeLessThan(maximumTime);

  const frames = await decodedFrames(video);
  try {
    await expect.poll(
      () => decodedFrames(video),
      { timeout: 30_000 },
    ).toBeGreaterThan(frames + 5);
  } catch (error) {
    throw new Error(`${error.message}\nPlayback state: ${JSON.stringify(await playbackSnapshot(video))}`);
  }
  expect(await video.evaluate(element => element.currentTime)).toBeLessThan(maximumTime);

  try {
    await expect.poll(
      () => video.evaluate(
        (element, expectedText) => {
          const cue = Array.from(element.textTracks[0]?.cues || [])
            .find(candidate => candidate.text === expectedText);
          return cue ? cue.startTime : null;
        },
        cueText,
      ),
      { timeout: 30_000 },
    ).not.toBeNull();
  } catch (error) {
    throw new Error(`${error.message}\nPlayback state: ${JSON.stringify(await playbackSnapshot(video))}`);
  }
  const actualCueStart = await video.evaluate(
    (element, expectedText) => Array.from(element.textTracks[0]?.cues || [])
      .find(candidate => candidate.text === expectedText)?.startTime,
    cueText,
  );
  expect(actualCueStart).toBeCloseTo(cueStart, 1);
}

async function commitNativeSeek(page, video, target, duration) {
  await video.evaluate(element => element.scrollIntoView({ block: 'center' }));
  await video.hover();
  const box = await video.boundingBox();
  expect(box).not.toBeNull();
  const inset = 12;
  await page.mouse.click(
    box.x + inset + (box.width - 2 * inset) * target / duration,
    box.y + box.height - 5,
  );
  await expect.poll(
    () => video.evaluate(element => element.currentTime),
    { timeout: 10_000 },
  ).toBeGreaterThan(target - 5);
  return video.evaluate(element => element.currentTime);
}

async function expectSelectedSubtitleBeforePlayback(page, target) {
  const evidence = await page.evaluate(() => {
    const instance = window.__latestHlsInstance;
    const attached = window.__hlsLifecycleEvents.find(event =>
      event.instance === instance &&
      event.event === 'media-attached',
    );
    const constructed = window.__hlsLifecycleEvents.find(event =>
      event.instance === instance &&
      event.event === 'constructed',
    );
    return {
      instance,
      attached,
      timelineOffset: constructed?.timelineOffset || 0,
      subtitles: window.__hlsSubtitleEvents.filter(event =>
        event.instance === instance &&
        event.event === 'processed' &&
        event.success,
      ),
      playingEvents: window.__videoEvents.filter(event =>
        event.hlsInstance === instance &&
        event.event === 'playing',
      ),
      presentedFrames: window.__presentedFrames.filter(frame =>
        frame.hlsInstance === instance,
      ),
    };
  });
  expect(evidence.attached, JSON.stringify(evidence)).not.toBeUndefined();
  const expectedAsset = `subs_${String(Math.floor(target / 2)).padStart(6, '0')}.vtt`;
  const subtitle = evidence.subtitles.find(event =>
    String(event.url || '').split(/[?#]/, 1)[0].endsWith(expectedAsset),
  );
  const firstFrame = evidence.presentedFrames.find(frame =>
    frame.at >= evidence.attached.at &&
    !frame.paused &&
    frame.mediaTime >= target - 3 &&
    frame.mediaTime < target + 3,
  );
  evidence.subtitle = subtitle;
  evidence.firstFrame = firstFrame;
  const message = JSON.stringify(evidence);
  expect(evidence.subtitle, message).not.toBeUndefined();
  expect(evidence.subtitle.paused, message).toBe(true);
  expect(evidence.firstFrame, message).not.toBeUndefined();
  expect(evidence.subtitle.at).toBeLessThanOrEqual(evidence.firstFrame.observedAt);
}

async function expectHealthyActiveHls(page, target) {
  const errors = await page.evaluate(() => {
    const activeInstance = window.__latestHlsInstance;
    return window.__hlsErrors.filter(error => error.instance === activeInstance);
  });
  const startupStalls = errors.filter(error => error.details === 'bufferStalledError');
  const prerollSubtitleGaps = errors.filter(error =>
    error.details === 'fragGap' &&
    error.fragType === 'subtitle' &&
    error.fragStart + error.fragDuration <= target + 0.25,
  );
  const startupHoleSeeks = errors.filter(error => {
    if (error.details !== 'bufferSeekOverHole') return false;
    const match = String(error.reason || '').match(/seeking from ([\d.]+) to ([\d.]+)/);
    return match && Math.abs(Number(match[2]) - Number(match[1])) <= 0.25;
  });
  const allowed = target < 1
    ? new Set([...startupStalls, ...startupHoleSeeks, ...prerollSubtitleGaps])
    : new Set([...startupStalls, ...prerollSubtitleGaps]);
  expect(errors.filter(error => !allowed.has(error))).toEqual([]);
  expect(startupStalls.length).toBeLessThanOrEqual(1);
  expect(startupHoleSeeks.length).toBeLessThanOrEqual(target < 1 ? 2 : 0);
  expect(prerollSubtitleGaps.length).toBeLessThanOrEqual(2);
}

async function expectActivePlaylistAlignment(page) {
  const readAlignment = () => page.evaluate(async () => {
    const instance = window.__latestHlsInstance;
    const source = window.__hlsLifecycleEvents.findLast(event =>
      event.instance === instance && event.event === 'source-loading',
    )?.url;
    if (!source) return { ready: false };

    const masterResponse = await fetch(source, { cache: 'no-store' });
    const master = await masterResponse.text();
    const lines = master.split(/\r?\n/);
    const subtitleLine = lines.find(line =>
      line.startsWith('#EXT-X-MEDIA:') && line.includes('TYPE=SUBTITLES'),
    );
    const subtitleURI = subtitleLine?.match(/URI="([^"]+)"/)?.[1];
    const videoURI = lines.find(line => line && !line.startsWith('#'));
    if (!masterResponse.ok || !subtitleURI || !videoURI) return { ready: false };

    const [videoResponse, subtitleResponse] = await Promise.all([
      fetch(new URL(videoURI, masterResponse.url), { cache: 'no-store' }),
      fetch(new URL(subtitleURI, masterResponse.url), { cache: 'no-store' }),
    ]);
    const [video, subtitles] = await Promise.all([
      videoResponse.text(),
      subtitleResponse.text(),
    ]);
    const sequence = text => Number(
      text.match(/#EXT-X-MEDIA-SEQUENCE:(\d+)/)?.[1],
    );
    const programDates = text => Array.from(
      text.matchAll(/#EXT-X-PROGRAM-DATE-TIME:([^\r\n]+)/g),
      match => Date.parse(match[1]),
    );
    return {
      ready: videoResponse.ok && subtitleResponse.ok,
      videoSequence: sequence(video),
      subtitleSequence: sequence(subtitles),
      videoSegments: (video.match(/#EXTINF:/g) || []).length,
      subtitleSegments: (subtitles.match(/#EXTINF:/g) || []).length,
      videoDates: programDates(video),
      subtitleDates: programDates(subtitles),
      hasGap: video.includes('#EXT-X-GAP') || subtitles.includes('#EXT-X-GAP'),
    };
  });

  await expect.poll(async () => {
    const alignment = await readAlignment();
    return {
      ready: alignment.ready,
      sequencesMatch: alignment.videoSequence === alignment.subtitleSequence,
      completeVideoDates: alignment.videoDates?.length === alignment.videoSegments,
      completeSubtitleDates: alignment.subtitleDates?.length === alignment.subtitleSegments,
      firstDateDeltaMs: Math.abs(
        alignment.videoDates?.[0] - alignment.subtitleDates?.[0],
      ),
      hasGap: alignment.hasGap,
    };
  }, { timeout: 15_000 }).toMatchObject({
    ready: true,
    sequencesMatch: true,
    completeVideoDates: true,
    completeSubtitleDates: true,
    hasGap: false,
  });

  const alignment = await readAlignment();
  expect(alignment.videoSegments).toBeGreaterThan(0);
  expect(alignment.subtitleSegments).toBeGreaterThan(0);
  expect(Math.abs(alignment.videoDates[0] - alignment.subtitleDates[0])).toBeLessThan(250);
}

test('keeps selected subtitles through consecutive real cold seeks', async ({ page, request }) => {
  test.setTimeout(3 * 60 * 1000);

  const fixtureResponse = await request.get(fixtureURL);
  expect(fixtureResponse.ok()).toBe(true);
  const fixture = await fixtureResponse.json();
  const prepareStarts = [];
  const subtitleRequestSegments = [];
  const failedHlsResponses = [];
  const consoleErrors = [];

  page.on('console', message => {
    if (message.type() === 'error') consoleErrors.push(message.text());
  });
  page.on('request', request => {
    const url = new URL(request.url());
    if (url.pathname === '/api/hls/prepare') {
      prepareStarts.push(Number(url.searchParams.get('start')));
    }
    const subtitleMatch = url.pathname.match(/\/subs_(\d+)\.vtt$/);
    if (subtitleMatch) {
      subtitleRequestSegments.push(Number(subtitleMatch[1]));
    }
  });
  page.on('response', response => {
    const url = new URL(response.url());
    if (url.pathname.startsWith('/api/hls/') && !response.ok()) {
      failedHlsResponses.push({
        path: url.pathname,
        status: response.status(),
      });
    }
  });
  await page.route('https://cdn.jsdelivr.net/npm/hls.js@1.6.13', route => route.fulfill({
    contentType: 'text/javascript',
    body: hlsSource,
  }));
  await page.addInitScript(() => {
    // Headless Chromium reports software-only decoding and would otherwise make
    // the product choose full transcode. The browser still decodes the real HLS
    // output; this only exercises the hybrid/remux path used by capable clients.
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
  });

  await page.goto(appURL);
  await page.evaluate(() => {
    window.__playbackErrors = [];
    window.__hlsErrors = [];
    window.__hlsSubtitleEvents = [];
    window.__hlsLifecycleEvents = [];
    window.__videoEvents = [];
    window.__presentedFrames = [];
    window.__hlsInstanceSequence = 0;
    window.__latestHlsInstance = 0;
    const video = document.querySelector('video');
    if (video.requestVideoFrameCallback) {
      const observeFrame = (at, metadata) => {
        window.__presentedFrames.push({
          hlsInstance: window.__latestHlsInstance,
          mediaTime: metadata.mediaTime,
          paused: video.paused,
          at,
          observedAt: performance.now(),
        });
        video.requestVideoFrameCallback(observeFrame);
      };
      video.requestVideoFrameCallback(observeFrame);
    }
    for (const eventName of ['pointerdown', 'pointerup', 'pause', 'seeking', 'seeked', 'play', 'playing']) {
      video.addEventListener(eventName, () => {
        window.__videoEvents.push({
          event: eventName,
          currentTime: video.currentTime,
          paused: video.paused,
          hlsInstance: window.__latestHlsInstance,
          at: performance.now(),
        });
      });
    }
    const RealHls = window.Hls;
    window.Hls = class ObservableHls extends RealHls {
      constructor(config) {
        super(config);
        this.__observableID = ++window.__hlsInstanceSequence;
        window.__latestHlsInstance = this.__observableID;
        window.__hlsLifecycleEvents.push({
          instance: this.__observableID,
          event: 'constructed',
          timelineOffset: Number(config.timelineOffset) || 0,
          at: performance.now(),
        });
        this.on(RealHls.Events.MEDIA_ATTACHED, () => {
          window.__hlsLifecycleEvents.push({
            instance: this.__observableID,
            event: 'media-attached',
            at: performance.now(),
          });
        });
        this.on(RealHls.Events.ERROR, (_event, data) => {
          window.__hlsErrors.push({
            instance: this.__observableID,
            type: data?.type,
            details: data?.details,
            fatal: Boolean(data?.fatal),
            reason: data?.reason,
            url: data?.url || data?.frag?.url || data?.context?.url,
            fragType: data?.frag?.type,
            fragStart: data?.frag?.start,
            fragDuration: data?.frag?.duration,
            level: data?.level,
            responseCode: data?.response?.code || data?.response?.status,
          });
        });
        this.on(RealHls.Events.SUBTITLE_FRAG_PROCESSED, (_event, data) => {
          window.__hlsSubtitleEvents.push({
            instance: this.__observableID,
            event: 'processed',
            success: data?.success,
            start: data?.frag?.start,
            duration: data?.frag?.duration,
            sn: data?.frag?.sn,
            cc: data?.frag?.cc,
            url: data?.frag?.url,
            paused: document.querySelector('video').paused,
            at: performance.now(),
          });
        });
        this.on(RealHls.Events.FRAG_LOADED, (_event, data) => {
          if (data?.frag?.type !== 'subtitle') return;
          window.__hlsSubtitleEvents.push({
            instance: this.__observableID,
            event: 'loaded',
            start: data.frag.start,
            duration: data.frag.duration,
            sn: data.frag.sn,
            cc: data.frag.cc,
            url: data.frag.url,
            at: performance.now(),
          });
        });
        this.on(RealHls.Events.INIT_PTS_FOUND, (_event, data) => {
          window.__hlsSubtitleEvents.push({
            instance: this.__observableID,
            event: 'init-pts',
            id: data?.id,
            cc: data?.frag?.cc,
            start: data?.frag?.start,
            initPTS: data?.initPTS,
            at: performance.now(),
          });
        });
        this.on(RealHls.Events.CUES_PARSED, (_event, data) => {
          window.__hlsSubtitleEvents.push({
            instance: this.__observableID,
            event: 'cues',
            type: data?.type,
            cues: Array.from(data?.cues || [], cue => ({
              start: cue.startTime,
              end: cue.endTime,
              text: cue.text,
            })),
            at: performance.now(),
          });
        });
      }
      loadSource(url) {
        window.__hlsLifecycleEvents.push({
          instance: this.__observableID,
          event: 'source-loading',
          url: String(url),
          at: performance.now(),
        });
        return super.loadSource(url);
      }
    };
    const message = document.querySelector('#playerMsg');
    new MutationObserver(() => {
      const text = message?.textContent?.trim() || '';
      if (text.includes('Playback error')) {
        window.__playbackErrors.push(text);
      }
    }).observe(message, { childList: true, subtree: true, characterData: true });
  });
  await page.getByLabel('Magnet link').fill(fixture.magnet);
  await page.getByRole('button', { name: 'Load', exact: true }).click();
  await expect(page.locator('#filelist')).toContainText(fixture.fileName, { timeout: 60_000 });
  await page.locator('#filelist').selectOption('0');
  await page.getByRole('button', { name: 'Select tracks' }).click();
  await page.locator('#subtitleSelect').selectOption('0');
  await page.getByRole('button', { name: 'Play', exact: true }).click();

  const video = page.locator('video');
  await expect.poll(
    () => video.evaluate(element => element.duration),
    { timeout: 90_000 },
  ).toBeGreaterThan(fixture.duration - 1);
  await expect.poll(
    () => video.evaluate(element => element.textTracks.length),
    { timeout: 90_000 },
  ).toBe(1);
  await expect.poll(
    () => video.evaluate(element => element.textTracks[0].mode),
    { timeout: 30_000 },
  ).toBe('showing');
  await expectDecodedPlayback(video, 0.2, 'initial full-stack subtitle', 0.4);
  await expectSelectedSubtitleBeforePlayback(page, 0.4);
  await expectHealthyActiveHls(page, 0.4);
  await expectActivePlaylistAlignment(page);
  expect(subtitleRequestSegments.length).toBeGreaterThan(0);
  expect(Math.max(...subtitleRequestSegments)).toBeLessThanOrEqual(8);

  const firstSeekTarget = await commitNativeSeek(
    page,
    video,
    fixture.targets[0],
    fixture.duration,
  );
  await expect.poll(
    () => prepareStarts.some(start => start >= fixture.targets[0] - 5),
    { timeout: 10_000 },
  ).toBe(true);
  await expect(page.locator('#playerMsg')).not.toContainText(
    'The selected subtitles could not be loaded',
    { timeout: 15_000 },
  );
  await expectDecodedPlayback(
    video,
    fixture.targets[0] + 0.2,
    'first cold-seek subtitle',
    fixture.targets[0] + 0.2,
  );
  await expectSelectedSubtitleBeforePlayback(page, firstSeekTarget);
  await expectHealthyActiveHls(page, fixture.targets[0] + 0.2);
  await expectActivePlaylistAlignment(page);
  expect(
    subtitleRequestSegments.some(segment =>
      Math.abs(segment - Math.floor(fixture.targets[0] / 2)) <= 1,
    ),
  ).toBe(true);
  expect(
    subtitleRequestSegments.some(segment =>
      segment >= Math.floor(fixture.targets[1] / 2) - 2,
    ),
  ).toBe(false);
  await expect(page.locator('#playerMsg')).not.toContainText('Playback error');

  const secondSeekTarget = await commitNativeSeek(
    page,
    video,
    fixture.targets[1],
    fixture.duration,
  );
  await expect.poll(
    () => prepareStarts.some(start => start >= fixture.targets[1] - 5),
    { timeout: 10_000 },
  ).toBe(true);
  await expectDecodedPlayback(
    video,
    fixture.targets[1] + 0.2,
    'second cold-seek subtitle',
    fixture.targets[1] + 0.2,
  );
  await expectSelectedSubtitleBeforePlayback(page, secondSeekTarget);
  await expectHealthyActiveHls(page, fixture.targets[1] + 0.2);
  await expectActivePlaylistAlignment(page);
  expect(
    subtitleRequestSegments.some(segment =>
      Math.abs(segment - Math.floor(fixture.targets[1] / 2)) <= 1,
    ),
  ).toBe(true);
  await expect(page.locator('#playerMsg')).not.toContainText('Playback error');

  await commitNativeSeek(page, video, 0.8, fixture.duration);
  try {
    await expect.poll(
      () => prepareStarts.filter(start => start < 5).length,
      { timeout: 10_000 },
    ).toBeGreaterThanOrEqual(2);
  } catch (error) {
    throw new Error(
      `${error.message}\nPrepare starts: ${JSON.stringify(prepareStarts)}\nPlayback state: ${JSON.stringify(await playbackSnapshot(video))}`,
    );
  }
  await expectDecodedPlayback(
    video,
    0.2,
    'initial full-stack subtitle',
    0.4,
    5,
  );
  await expectSelectedSubtitleBeforePlayback(page, 0.4);
  await expectHealthyActiveHls(page, 0.4);
  await expectActivePlaylistAlignment(page);

  const presentationChanges = failedHlsResponses.filter(response => response.status === 409);
  expect(
    failedHlsResponses.filter(response => response.status !== 409),
  ).toEqual([]);
  expect(presentationChanges.length).toBeLessThanOrEqual(2);
  const hlsInstances = await page.evaluate(() => window.__hlsInstanceSequence);
  expect(hlsInstances).toBeGreaterThanOrEqual(1);
  // The initial attachment plus three explicit cold seek commands may each
  // own a presentation. At most one extra attachment is allowed for a stale
  // generation detected before its first frame.
  expect(hlsInstances).toBeLessThanOrEqual(5);
  expect(await page.evaluate(() => window.__playbackErrors)).toEqual([]);
  expect(
    await page.evaluate(() => window.__hlsErrors.filter(error =>
      error.fatal && Number(error.responseCode) !== 409,
    )),
  ).toEqual([]);
  expect(consoleErrors).toEqual([]);
});
