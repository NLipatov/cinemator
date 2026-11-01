import { test, expect } from '@playwright/test';
import { openPlayer } from './player-fixture.mjs';

async function ready(page) {
  const row = await openPlayer(page, { tracks: 2, subtitles: [{ index: 3, language: 'eng' }] });
  return row;
}
async function play(page) {
  await page.locator('.download-row[data-id="ready"] [data-action="open"]').click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
}

test('settings are optional; only one menu opens and keyboard focus returns', async ({ page }) => {
  await ready(page);
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  await play(page);
  await expect(page.locator('#selectedFile')).toHaveText('Episode 2.mkv');
  await expect(page.locator('.settings-popup:visible, #videoMenu:visible')).toHaveCount(0);
  await page.locator('#videoMenuButton').press('Enter');
  await expect(page.getByRole('menuitemradio', {name:'Episode 2.mkv', exact:true})).toBeFocused();
  await page.keyboard.press('Escape');
  await expect(page.locator('#videoMenuButton')).toBeFocused();
  await page.locator('#audioMenuButton').click();
  await expect(page.locator('#audioSelect')).toBeEnabled();
  await page.locator('#subtitlesMenuButton').click();
  await expect(page.locator('.settings-popup:visible, #videoMenu:visible')).toHaveCount(1);
  await expect(page.locator('#subtitleDelayRow')).toBeHidden();
  await page.locator('#subtitleSelect').selectOption('3');
  await expect(page.locator('#subtitleDelayRow')).toBeVisible();
  await page.locator('#subDelayPlus').click();
  await expect(page.locator('#subDelayValue')).toHaveText('0.5s');
  await page.locator('#subtitleSelect').selectOption('-1');
  await expect(page.locator('#subtitleDelayRow')).toBeHidden();
  await page.locator('.brand').click();
  await expect(page.locator('.settings-popup:visible, #videoMenu:visible')).toHaveCount(0);
  await expect(page.locator('#subtitlesMenuButton')).toBeFocused();
  await expect(page.locator('#play')).toBeHidden();
});

for (const [language, label] of [['en', 'Russian'], ['ru', 'русский'], ['zh-Hans', '俄语'], ['zh-Hant', '俄文']]) {
  test(`${language}: track languages are localized`, async ({ page }) => {
    await openPlayer(page, { language, tracks: 2 });
    await play(page);
      await page.locator('#audioMenuButton').click();
    await expect(page.locator('#audioSelect')).toBeEnabled();
    await expect(page.locator('#audioSelect option').first()).toContainText(label);
    await page.locator('#languageSelect').selectOption(language === 'en' ? 'ru' : 'en');
    await expect(page.locator('#audioSelect option').first()).toContainText(language === 'en' ? 'русский' : 'Russian');
  });
}

test('stream preparation keeps its size and retry preserves the seek position', async ({ page }) => {
  await ready(page);
  await play(page);
  await page.locator('#video').evaluate(v => { v.currentTime = 47; });
  await expect.poll(() => page.locator('#video').evaluate(v => v.currentTime)).toBe(47);
  const height = (await page.locator('#video').boundingBox()).height;
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  const failStream = async route => {
    if (route.request().method() !== 'GET') return route.fallback();
    await pending;
    await route.fulfill({ status: 503, body: 'Temporary stream failure' });
  };
  await page.route('**/api/hls/prepare?*', failStream);
  await page.locator('#audioMenuButton').click();
  await page.locator('#audioSelect').selectOption('1');
  await expect(page.locator('#warnMsg')).toBeVisible();
  expect((await page.locator('#video').boundingBox()).height).toBe(height);
  await expect(page.locator('#video')).toHaveCSS('opacity', '1');
  release();
  await expect(page.locator('#play')).toHaveText('Retry');
  await page.unroute('**/api/hls/prepare?*', failStream);
  await page.locator('#play').click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.currentTime)).toBe(47);
  expect(await page.evaluate(() => window.testHls.options.startPosition)).toBe(47);
});

test('late metadata from a replaced file cannot select tracks or start the old video', async ({ page }) => {
  await ready(page);
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  let oldRequested = false;
  await page.route('**/api/media/info?*', async route => {
    if (new URL(route.request().url()).searchParams.get('file') !== '4') return route.fallback();
    oldRequested = true;
    await pending;
    await route.fulfill({ json: { audioTracks: [{ index: 99, title: 'Stale track' }], subtitles: [] } });
  });
  const streams = [];
  page.on('request', r => { if (r.method() === 'GET' && r.url().includes('/api/hls/prepare')) streams.push(new URL(r.url()).searchParams.get('file')); });
  await page.locator('.download-row[data-id="ready"] [data-action="open"]').click();
  await expect.poll(() => oldRequested).toBe(true);
  await page.locator('#videoMenuButton').click();
  await page.getByRole('menuitemradio', {name:'Episode 1.mkv', exact:true}).click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  release();
  await expect(page.locator('#selectedFile')).toContainText('Episode 1');
  await expect(page.locator('#audioSelect option')).toHaveCount(2);
  expect(streams.every(file => file === '2')).toBe(true);
});

for (const width of [390, 1280]) {
  test(`${width}: menus fit and desktop controls stay below the full-width player`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await ready(page);
    await play(page);
    const video = await page.locator('#video').boundingBox();
    const workspace = await page.locator('.workspace').boundingBox();
    expect(Math.abs(video.width - workspace.width)).toBeLessThan(2);
    for (const name of ['video', 'audio', 'subtitles']) {
      await page.locator(`#${name}MenuButton`).click();
      const box = await page.locator(`#${name}Menu`).boundingBox();
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(width);
      expect(box.y).toBeGreaterThanOrEqual(0);
      expect(box.y + box.height).toBeLessThanOrEqual(name === 'video' && width === 390 ? 900 : video.y + video.height + 1);
    }
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
  });
}

test('seeking back to zero before a track switch does not reuse the resume time', async ({ page }) => {
  const row = await ready(page);
  await page.evaluate(() => {
    localStorage.setItem('cinemator-playback:ready', JSON.stringify({ fileIndex: 4, time: 42 }));
    window.dispatchEvent(new Event('focus'));
  });
  await row.locator('.download-open').click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.currentTime)).toBe(42);
  await page.locator('#video').evaluate(v => { v.currentTime = 0; });
  await page.locator('#audioMenuButton').click();
  await page.locator('#audioSelect').selectOption('1');
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  expect(await page.locator('#video').evaluate(v => v.currentTime)).toBe(0);
  expect(await page.evaluate(() => window.testHls.options.startPosition)).toBe(0);
});

test('late stream response cannot replace the newly selected file', async ({ page }) => {
  await ready(page);
  let release;
  const pending = new Promise(resolve => { release = resolve; });
  let oldRequested = false;
  await page.route('**/api/hls/prepare?*', async route => {
    if (route.request().method() !== 'GET' || new URL(route.request().url()).searchParams.get('file') !== '4') return route.fallback();
    oldRequested = true;
    await pending;
    return route.fulfill({status:204});
  });
  await page.locator('.download-row[data-id="ready"] [data-action="open"]').click();
  await expect.poll(() => oldRequested).toBe(true);
  await page.locator('#videoMenuButton').click();
  await page.getByRole('menuitemradio', {name:'Episode 1.mkv', exact:true}).click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  const src = await page.locator('#video').getAttribute('src');
  release();
  await expect(page.locator('#selectedFile')).toContainText('Episode 1');
  await expect(page.locator('#video')).toHaveAttribute('src',src);
  expect(new URL(src, 'http://cinemator.test').searchParams.get('file')).toBe('2');
});

test('cards open directly, close saves progress, and the next tap resumes the saved file', async ({ page }) => {
  const row = await ready(page);
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  await expect(row.locator('select')).toHaveCount(0);
  await row.locator('.download-title').click();
  await expect(page.locator('#playerWorkspace')).toBeVisible();
  await expect(page.locator('#videoMenuLabel')).toHaveText('Video: 2/2');
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  await page.locator('#videoMenuButton').click();
  await page.getByRole('menuitemradio', {name:'Episode 1.mkv', exact:true}).click();
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  await page.locator('#video').evaluate(v => { v.currentTime = 38; });
  await expect.poll(() => page.locator('#video').evaluate(v => v.currentTime)).toBe(38);
  await page.locator('#closePlayer').click();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
  await expect(row.locator('.download-open')).toBeFocused();
  await expect(row.locator('.download-videos')).toHaveText('2 videos');
  await expect(row.locator('.download-watched')).toBeVisible();
  expect(await page.locator('#video').getAttribute('src')).toBeNull();
  await row.locator('.download-thumbnail').click();
  await expect(page.locator('#videoMenuLabel')).toHaveText('Video: 1/2');
  await expect.poll(() => page.locator('#video').evaluate(v => v.currentTime)).toBe(38);
  await page.locator('#closePlayer').click();
  await row.locator('.download-more').click();
  await row.locator('[data-action="restart"]').click();
  await expect(page.locator('#videoMenuLabel')).toHaveText('Video: 1/2');
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  expect(await page.locator('#video').evaluate(v => v.currentTime)).toBe(0);
});

for (const phase of ['metadata', 'stream']) {
  test(`closing during ${phase} preparation prevents a late response from reopening the player`, async ({ page }) => {
    const row = await ready(page);
    let release;
    const pending = new Promise(resolve => { release = resolve; });
    let requested = false;
    const endpoint = phase === 'metadata' ? '**/api/media/info?*' : '**/api/hls/prepare?*';
    await page.route(endpoint, async route => {
      if (route.request().method() !== 'GET') return route.fallback();
      requested = true;
      await pending;
      await route.fulfill({json: {audioTracks: [{index: 9}], subtitles: []}});
    });
    await row.locator('[data-action="open"]').click();
    await expect.poll(() => requested).toBe(true);
    await page.locator('#closePlayer').click();
    release();
    await expect(page.locator('#playerWorkspace')).toBeHidden();
    await expect(row.locator('[data-action="open"]')).toBeFocused();
    expect(await page.locator('#video').getAttribute('src')).toBeNull();
    await expect(page.locator('#warnMsg')).toBeHidden();
  });
}

test('adding a magnet while watching keeps the player and its position', async ({ page }) => {
  await ready(page);
  await play(page);
  await page.locator('#video').evaluate(v => { v.currentTime = 24; });
  const src = await page.locator('#video').getAttribute('src');
  await page.route('**/api/torrent/files?*', route => route.fulfill({json: [{index: 0, name: 'Added.mkv', size: 512}]}));
  await page.locator('#addDownloadBtn').click();
  await page.locator('#magnet').fill('magnet:new');
  await page.locator('#nextBtn').click();
  await expect(page.locator('#sourcePanel')).toBeHidden();
  await expect(page.locator('#playerWorkspace')).toBeVisible();
  await expect(page.locator('#video')).toHaveAttribute('src', src);
  expect(await page.locator('#video').evaluate(v => v.currentTime)).toBe(24);
  await expect(page.locator('#videoMenuLabel')).toHaveText('Video: 2/2');
});

for (const language of ['en', 'ru', 'zh-Hans', 'zh-Hant']) {
  test(`${language}: card previews and actions fit narrow and enlarged layouts`, async ({ page }) => {
    await openPlayer(page, {language});
    for (const fontSize of [16, 32]) {
      await page.evaluate(size => { document.documentElement.style.fontSize = `${size}px`; }, fontSize);
      for (const width of [320, 390, 1280]) {
        await page.setViewportSize({width, height: 1000});
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        for (const control of await page.locator('.download-open, .download-more').all()) {
          const box = await control.boundingBox();
          expect(box.width).toBeGreaterThan(40);
          expect(box.x).toBeGreaterThanOrEqual(0);
          expect(box.x + box.width).toBeLessThanOrEqual(width);
        }
      }
    }
  });
}

for (const width of [390, 1280]) {
  test(`${width}: preparation status is inside the video and closing remains available`, async ({ page }) => {
    await page.setViewportSize({width, height:900});
    const row = await ready(page);
    let release;
    const pending = new Promise(resolve => {release = resolve;});
    await page.route('**/api/media/info?*', async route => {
      await pending;
      await route.fulfill({json:{audioTracks:[], subtitles:[]}});
    });
    await row.locator('[data-action="open"]').click();
    const warning = page.locator('#warnMsg');
    await expect(warning).toBeVisible();
    const frame = await page.locator('.video-frame').boundingBox();
    const status = await page.locator('#preparationStage').boundingBox();
    expect(status.x).toBeGreaterThanOrEqual(frame.x);
    expect(status.x+status.width).toBeLessThanOrEqual(frame.x+frame.width);
    expect(status.y).toBeGreaterThan(frame.y);
    expect(status.y+status.height).toBeLessThan(frame.y+frame.height);
    expect(await page.locator('#fileMsg').textContent()).toBe('');
    await page.locator('#closePlayer').click();
    release();
    await expect(page.locator('#playerWorkspace')).toBeHidden();
  });
}

for (const language of ['en', 'ru', 'zh-Hans', 'zh-Hant']) {
  test(`${language}: preview stays anchored with optional episodes and saved progress`, async ({ page }) => {
    await openPlayer(page, {language});
    const files = [
      {index: 2, name: 'Series — 01.mkv', size: 512},
      {index: 4, name: 'Series — 02.mkv', size: 512},
    ];
    const downloads = ['single', 'series', 'single-saved', 'series-saved'].map(id => ({
      id, title: 'A collection', magnet: `magnet:${id}`, status: 'ready', size: 1024,
      selectedFileIndex: 4, expiresAt: '2099-01-01T00:00:00Z',
      files: id.startsWith('series') ? files : [files[1]],
    }));
    await page.route(/\/api\/downloads(?:\?|$)/, route => route.fulfill({json: downloads}));
    await page.evaluate(() => {
      for (const id of ['single-saved', 'series-saved']) {
        localStorage.setItem(`cinemator-playback:${id}`, JSON.stringify({fileIndex: 4, time: 4455, duration: 7200}));
      }
      window.dispatchEvent(new Event('focus'));
    });
    await expect(page.locator('.download-row')).toHaveCount(4);
    for (const fontSize of [16, 32]) {
      await page.evaluate(size => {document.documentElement.style.fontSize = `${size}px`;}, fontSize);
      for (const width of [320, 390, 768, 1280]) {
        await page.setViewportSize({width, height: 1000});
        const positions = await page.locator('.download-row').evaluateAll(rows => rows.map(row => {
          const card = row.getBoundingClientRect();
          const play = row.querySelector('.download-thumbnail').getBoundingClientRect();
          return {left: play.left-card.left, right: card.right-play.right, top: play.top-card.top, width: play.width};
        }));
        for (const position of positions) {
          for (const key of ['left', 'right', 'top', 'width']) {
            expect(Math.abs(position[key]-positions[0][key]), `${width}px / ${fontSize}px / ${key}`).toBeLessThanOrEqual(1);
          }
        }
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
      }
    }
    // Removing progress and an episode option on a live row must preserve the same anchor.
    const row = page.locator('.download-row[data-id="series-saved"]');
    const anchor = async () => row.evaluate(row => {
      const card = row.getBoundingClientRect(), play = row.querySelector('.download-thumbnail').getBoundingClientRect();
      return {right: card.right-play.right, top: play.top-card.top};
    });
    const before = await anchor();
    downloads[3].files = [files[1]];
    await page.evaluate(() => {
      localStorage.removeItem('cinemator-playback:series-saved');
      window.dispatchEvent(new Event('focus'));
    });
    await expect(row.locator('.download-videos')).toBeHidden();
    await expect(row.locator('.download-watched')).toBeHidden();
    expect(await anchor()).toEqual(before);
  });
}
