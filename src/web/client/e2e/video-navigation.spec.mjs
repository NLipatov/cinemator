import {test, expect} from '@playwright/test';
import {openPlayer} from './player-fixture.mjs';

const files = [0, 4, 19, 25].map((index, ordinal) => ({index, name: `0${ordinal + 1}.mp4`, size: 512}));

function streams(page) {
  const requests = [];
  page.on('request', request => {
    const url = new URL(request.url());
    if (request.method() === 'GET' && request.resourceType() === 'fetch' && url.pathname === '/api/hls/prepare') {
      requests.push({file: Number(url.searchParams.get('file')), audio: Number(url.searchParams.get('audio')), magnet: url.searchParams.get('magnet')});
    }
  });
  return requests;
}

async function ready(page, ordinal, audio) {
  await expect(page.locator('#selectedFile')).toHaveText(files[ordinal].name);
  await expect(page.locator('#audioSelect')).toHaveValue(String(audio));
  await expect.poll(() => page.locator('#video').evaluate(video => video.duration)).toBe(120);
}

async function chooseAudio(page, index) {
  await page.locator('#audioMenuButton').click();
  await page.locator('#audioSelect').selectOption(String(index));
  await expect.poll(() => page.locator('#video').evaluate(video => video.duration)).toBe(120);
  await page.keyboard.press('Escape');
}

for (const source of ['HLS', 'native']) {
  test(`${source}: the chosen audio follows next, previous, the file menu and reload`, async ({page}) => {
    const row = await openPlayer(page, {source, files, tracks: 2});
    const requests = streams(page);
    await row.locator('.download-open').click();
    await ready(page, 1, 0);
    await chooseAudio(page, 1);
    await page.locator('#video').evaluate(video => { video.currentTime = 42; });
    await page.locator('#nextVideo').click();
    await ready(page, 2, 1);
    expect(await page.locator('#video').evaluate(video => video.currentTime)).toBe(0);
    await page.locator('#nextVideo').press('Enter');
    await ready(page, 3, 1);
    await expect(page.locator('#nextVideo')).toBeDisabled();
    await expect(page.locator('#videoMenuButton')).toBeFocused();
    await page.locator('#previousVideo').click();
    await ready(page, 2, 1);
    await page.locator('#videoMenuButton').click();
    await page.getByRole('menuitemradio', {name: '01.mp4', exact: true}).click();
    await ready(page, 0, 1);
    await expect(page.locator('#previousVideo')).toBeDisabled();
    await page.locator('#closePlayer').click();
    await page.reload();
    await row.locator('.download-open').click();
    await ready(page, 0, 1);
    expect(requests.map(({file, audio}) => ({file, audio}))).toEqual([
      {file: 4, audio: 0}, {file: 4, audio: 1}, {file: 19, audio: 1},
      {file: 25, audio: 1}, {file: 19, audio: 1}, {file: 0, audio: 1}, {file: 0, audio: 1},
    ]);
  });
}

test('a missing audio track uses a fallback without replacing the preference, including retry', async ({page}) => {
  const row = await openPlayer(page, {files, tracks: 2});
  await page.route('**/api/media/info?*', route => {
    const file = Number(new URL(route.request().url()).searchParams.get('file'));
    return route.fulfill({json: {audioTracks: (file === 19 ? [0] : [0, 1]).map(index => ({index, language: 'rus'})), subtitles: []}});
  });
  const requests = streams(page);
  await row.locator('.download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  await page.locator('#nextVideo').click();
  await ready(page, 2, 0);
  const fail = route => route.request().method() === 'GET' ? route.fulfill({status: 503}) : route.fallback();
  await page.route('**/api/hls/prepare?*', fail);
  await page.locator('#nextVideo').click();
  await expect(page.locator('#play')).toHaveText('Retry');
  await expect(page.locator('#play')).toBeVisible();
  await page.unroute('**/api/hls/prepare?*', fail);
  await page.locator('#play').click();
  await ready(page, 3, 1);
  expect(requests.map(({file, audio}) => ({file, audio}))).toEqual([
    {file: 4, audio: 0}, {file: 4, audio: 1}, {file: 19, audio: 0}, {file: 25, audio: 1}, {file: 25, audio: 1},
  ]);
});

test('explicit audio changes replace the preference only for that download', async ({page}) => {
  const row = await openPlayer(page, {files, tracks: 2});
  await row.locator('.download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  await page.locator('.download-row[data-id="other"] .download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  await row.locator('.download-open').click();
  await ready(page, 1, 1);
  await chooseAudio(page, 0);
  await page.locator('#nextVideo').click();
  await ready(page, 2, 0);
  await page.reload();
  await row.locator('.download-open').click();
  await ready(page, 2, 0);
  await page.locator('.download-row[data-id="other"] .download-open').click();
  await expect(page.locator('#audioSelect')).toHaveValue('1');
  await expect.poll(() => page.locator('#video').evaluate(video => video.duration)).toBe(120);
});

test('invalid or unavailable saved audio indices do not prevent playback', async ({page}) => {
  const row = await openPlayer(page, {files, tracks: 2});
  for (const value of ['broken json', '-1', '1.5', '99', 'null', '"1"']) {
    await page.evaluate(value => localStorage.setItem('cinemator-audio:ready', value), value);
    await page.reload();
    await row.locator('.download-open').click();
    await ready(page, 1, 0);
  }
});

test('unavailable storage still preserves audio while navigating in the current page', async ({page}) => {
  await page.addInitScript(() => {
    for (const method of ['getItem', 'setItem']) {
      const original = Storage.prototype[method];
      Storage.prototype[method] = function (key, ...args) {
        if (key.startsWith('cinemator-audio:')) throw new Error('Storage unavailable');
        return original.call(this, key, ...args);
      };
    }
  });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  const row = await openPlayer(page, {files, tracks: 2});
  await row.locator('.download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  await page.locator('#nextVideo').click();
  await ready(page, 2, 1);
  expect(errors).toEqual([]);
});

test('rapid navigation ignores the previous file metadata and keeps the audio preference', async ({page}) => {
  const row = await openPlayer(page, {files, tracks: 2});
  await row.locator('.download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  let release, requested = false;
  const pending = new Promise(resolve => { release = resolve; });
  await page.route('**/api/media/info?*', async route => {
    if (new URL(route.request().url()).searchParams.get('file') !== '19') return route.fallback();
    requested = true;
    await pending;
    await route.fulfill({json: {audioTracks: [{index: 0}], subtitles: []}});
  });
  const requests = streams(page);
  await page.locator('#nextVideo').click();
  await expect.poll(() => requested).toBe(true);
  await page.locator('#nextVideo').click();
  await ready(page, 3, 1);
  release();
  await expect(page.locator('#audioSelect option')).toHaveCount(2);
  await ready(page, 3, 1);
  expect(requests.map(({file, audio}) => ({file, audio}))).toEqual([{file: 25, audio: 1}]);
});

test('Media Session navigation uses the same audio preference and clears actions on close', async ({page}) => {
  await page.addInitScript(() => {
    window.mediaActions = {};
    const session = navigator.mediaSession;
    const register = session.setActionHandler.bind(session);
    session.setActionHandler = (action, handler) => {
      register(action, handler);
      window.mediaActions[action] = handler;
    };
  });
  const row = await openPlayer(page, {files, tracks: 2});
  await row.locator('.download-open').click();
  await ready(page, 1, 0);
  await chooseAudio(page, 1);
  await page.evaluate(() => window.mediaActions.nexttrack());
  await ready(page, 2, 1);
  expect(await page.evaluate(() => navigator.mediaSession.metadata.title)).toBe('03.mp4');
  await page.evaluate(() => window.mediaActions.nexttrack());
  await ready(page, 3, 1);
  expect(await page.evaluate(() => window.mediaActions.nexttrack)).toBeNull();
  for (const ordinal of [2, 1, 0]) {
    await page.evaluate(() => window.mediaActions.previoustrack());
    await ready(page, ordinal, 1);
  }
  expect(await page.evaluate(() => window.mediaActions.previoustrack)).toBeNull();
  await page.evaluate(() => { window.oldNext = window.mediaActions.nexttrack; });
  await page.locator('#closePlayer').click();
  expect(await page.evaluate(() => Object.values(window.mediaActions).every(action => action === null))).toBe(true);
  expect(await page.evaluate(() => navigator.mediaSession.metadata)).toBeNull();
  await page.evaluate(() => window.oldNext());
  await expect(page.locator('#playerWorkspace')).toBeHidden();
});

for (const support of ['absent', 'unsupported actions']) {
  test(`navigation works with Media Session ${support}`, async ({page}) => {
    await page.addInitScript(support => {
      if (support === 'absent') Object.defineProperty(navigator, 'mediaSession', {value: undefined});
      else navigator.mediaSession.setActionHandler = () => { throw new DOMException('Unsupported', 'NotSupportedError'); };
    }, support);
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const row = await openPlayer(page, {files, tracks: 2});
    await row.locator('.download-open').click();
    await ready(page, 1, 0);
    await chooseAudio(page, 1);
    await page.locator('#nextVideo').click();
    await ready(page, 2, 1);
    expect(errors).toEqual([]);
  });
}
