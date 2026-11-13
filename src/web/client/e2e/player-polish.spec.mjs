import {test, expect} from '@playwright/test';
import {openPlayer} from './player-fixture.mjs';

const files = Array.from({length: 72}, (_, index) => ({
  index: index === 6 ? 4 : 100 + index,
  name: `${String(index + 1).padStart(2, '0')}.mp4`, size: 512,
}));

for (const language of ['ru', 'en', 'zh-Hans', 'zh-Hant']) {
  test(`${language}: a 72-file selector stays readable and the mobile toolbar uses stable rows`, async ({page}) => {
    const row = await openPlayer(page, {files, language, tracks: 2});
    await page.addStyleTag({content: ':root { --font-main: "DejaVu Sans", monospace; }'});
    await row.locator('.download-open').click();
    await expect(page.locator('#videoMenuLabel')).toContainText('7/72');
    await expect(page.locator('#playerTitle')).toHaveText('Two episodes');
    await expect(page.locator('#selectedFile')).toHaveText('07.mp4');
    for (const width of [2048, 1280, 1024, 390, 320]) {
      await page.setViewportSize({width, height: 900});
      for (const size of [16, 32]) {
        await page.evaluate(size => document.documentElement.style.fontSize = `${size}px`, size);
        const label = await page.locator('#videoMenuLabel').evaluate(el => ({
          height: el.getBoundingClientRect().height, line: parseFloat(getComputedStyle(el).lineHeight),
        }));
        expect(label.height).toBeLessThanOrEqual(label.line * (width >= 1024 ? 1 : 2) + 1);
        expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
        const boxes = await page.locator('#previousVideo, #videoMenuButton, #nextVideo, #audioMenuButton, #subtitlesMenuButton, #closePlayer').evaluateAll(elements => elements.map(el => {
          const {x, y, width, height} = el.getBoundingClientRect();
          return {x, y, width, height, overflow: el.scrollWidth > el.clientWidth + 1};
        }));
        for (const box of boxes) {
          expect(box.x).toBeGreaterThanOrEqual(0);
          expect(box.x + box.width).toBeLessThanOrEqual(width);
          expect(box.height).toBeGreaterThanOrEqual(44);
          expect(box.overflow).toBe(false);
        }
        if (width <= 390) {
          const title = await page.locator('.player-identity').boundingBox();
          expect(Math.abs(title.y - boxes[5].y)).toBeLessThan(1);
          expect(boxes[0].y).toBeGreaterThanOrEqual(title.y + title.height);
          expect(boxes[3].y).toBeGreaterThanOrEqual(boxes[1].y + boxes[1].height);
          const settings = await page.locator('.player-settings').boundingBox();
          if (Math.abs(boxes[3].y - boxes[4].y) < 1) {
            expect(Math.abs((boxes[3].x + boxes[4].x + boxes[4].width) / 2 - (settings.x + settings.width / 2))).toBeLessThan(1);
          } else {
            expect(boxes[4].y).toBeGreaterThanOrEqual(boxes[3].y + boxes[3].height);
            for (const box of [boxes[3], boxes[4]]) {
              expect(Math.abs(box.x + box.width / 2 - (settings.x + settings.width / 2))).toBeLessThan(1);
            }
          }
        }
      }
    }
  });
}

for (const width of [390, 1280]) {
  test(`${width}: all settings have a heading, direct choices and a close button`, async ({page}) => {
    await page.setViewportSize({width, height: 900});
    const row = await openPlayer(page, {files, tracks: 2, subtitles: [{index: 0, language: 'rus', title: 'Full captions'}]});
    await row.locator('.download-open').click();
    await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Russian');
    await expect(page.locator('#subtitlesMenuLabel')).toHaveText('Subtitles: Off');
    const bottomEdges = [];
    for (const name of ['video', 'audio', 'subtitles']) {
      await page.locator(`#${name}MenuButton`).click();
      const popup = page.locator(`#${name}Menu`);
      await expect(page.locator('.settings-popup:visible')).toHaveCount(1);
      await expect(popup.getByRole('heading')).toBeVisible();
      await expect(popup.locator('[aria-checked="true"]')).toBeFocused();
      await expect(popup.locator('select')).toHaveCount(0);
      const box = await popup.boundingBox();
      bottomEdges.push(box.y + box.height);
      await popup.getByRole('button', {name: 'Close', exact: true}).click();
      await expect(popup).toBeHidden();
      await expect(page.locator(`#${name}MenuButton`)).toBeFocused();
    }
    if (width === 390) expect(Math.max(...bottomEdges) - Math.min(...bottomEdges)).toBeLessThan(1);
    const requests = [];
    page.on('request', r => {
      if (r.resourceType() === 'fetch' && r.method() === 'GET' && r.url().includes('/api/hls/prepare')) requests.push(new URL(r.url()).searchParams);
    });
    await page.locator('#audioMenuButton').click();
    await page.locator('#audioMenu').getByRole('menuitemradio', {name: '2 · English (aac)', exact: true}).click();
    await expect(page.locator('#audioMenu')).toBeHidden();
    await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: English');
    await expect.poll(() => requests.at(-1)?.get('audio')).toBe('1');
    await page.locator('#subtitlesMenuButton').click();
    await page.getByRole('menuitemradio', {name: '1 · Russian — Full captions'}).click();
    await expect(page.locator('#subtitlesMenu')).toBeHidden();
    await expect(page.locator('#subtitlesMenuLabel')).toHaveText('Subtitles: Russian');
    await expect.poll(() => requests.at(-1)?.get('subtitle')).toBe('0');
    await page.locator('#subtitlesMenuButton').click();
    await expect(page.locator('#subtitleDelayRow')).toBeVisible();
    await page.locator('#subDelayPlus').click();
    await expect(page.locator('#subDelayValue')).toHaveText('0.5s');
    await page.keyboard.press('Escape');
    await expect(page.locator('#subtitlesMenuButton')).toBeFocused();
  });
}

test('audio keyboard selection waits for Enter, survives translation and keeps the saved preference', async ({page}) => {
  const row = await openPlayer(page, {tracks: 20});
  await row.locator('.download-open').click();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Russian');
  await page.locator('#audioMenuButton').press('Enter');
  await page.keyboard.press('End');
  const last = page.locator('#audioTracks [data-value="19"]');
  await expect(last).toBeFocused();
  await expect(last).toBeInViewport();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Russian');
  await page.keyboard.press('Enter');
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: English');
  await expect(last).toHaveAttribute('aria-checked', 'true');
  await expect(page.locator('#audioMenuButton')).toBeFocused();
  await page.locator('#languageSelect').selectOption('ru');
  await expect(page.locator('#audioMenuLabel')).toHaveText('Звук: английский');
  await page.locator('#audioMenuButton').click();
  await expect(last).toBeFocused();
  await expect(last).toContainText('20 · английский');
  await page.keyboard.press('Escape');
  await page.reload();
  await row.locator('.download-open').click();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: English');
});

test('explicitly choosing the fallback track replaces the saved preference without restarting it', async ({page}) => {
  const row = await openPlayer(page, {tracks: 2});
  await page.evaluate(() => localStorage.setItem('cinemator-audio:ready', '1'));
  const singleTrack = route => route.fulfill({json: {audioTracks: [{index: 0, language: 'rus'}], subtitles: []}});
  await page.route('**/api/media/info?*', singleTrack);
  await row.locator('.download-open').click();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Russian');
  await expect.poll(() => page.locator('#video').evaluate(v => v.duration)).toBe(120);
  await page.locator('#video').evaluate(v => { v.currentTime = 42; });
  await page.locator('#audioMenuButton').click();
  await page.getByRole('menuitemradio', {name: '1 · Russian', exact: true}).click();
  expect(await page.locator('#video').evaluate(v => v.currentTime)).toBe(42);
  await page.unroute('**/api/media/info?*', singleTrack);
  await page.locator('#previousVideo').click();
  await expect(page.locator('#audioTracks .track-label')).toHaveCount(2);
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Russian');
});

test('title and now-playing marker follow the active download and clear on close', async ({page}) => {
  const row = await openPlayer(page);
  await expect(page.locator('.download-current:visible')).toHaveCount(0);
  await row.locator('.download-open').click();
  await expect(page.locator('#playerTitle')).toHaveText('Two episodes');
  await expect(row.locator('.download-current')).toHaveText('Now playing');
  await expect(page.locator('.download-current:visible')).toHaveCount(1);
  const other = page.locator('.download-row[data-id="other"]');
  await other.locator('.download-open').click();
  await expect(page.locator('#playerTitle')).toHaveText('Other video');
  await expect(row.locator('.download-current')).toBeHidden();
  await expect(other.locator('.download-current')).toBeVisible();
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(page.locator('.download-current:visible')).toHaveCount(1);
  await page.locator('#closePlayer').click();
  await expect(page.locator('.download-current:visible')).toHaveCount(0);
});

for (const width of [390, 1280]) {
  test(`${width}: application menu contains secondary controls and restores focus`, async ({page}) => {
    await page.setViewportSize({width, height:900});
    await openPlayer(page);
    const menu = page.locator('#appMenu');
    const button = page.locator('#appMenuButton');
    await expect(page.locator('#logoutBtn')).toBeHidden();
    await button.press('Enter');
    await expect(menu).toBeVisible();
    await expect(page.locator('#appVersion')).toHaveText('[test]');
    await expect(page.locator('#logoutBtn')).toBeVisible();
    const before = await page.locator('html').getAttribute('data-theme');
    await page.locator(`input[name="theme-mode"][value="${before === 'light' ? 'dark' : 'light'}"]`).check();
    await expect(page.locator('html')).not.toHaveAttribute('data-theme', before);
    await page.keyboard.press('Escape');
    await expect(menu).toBeHidden();
    await expect(button).toBeFocused();
    await button.click();
    await page.locator('.brand').click();
    await expect(menu).toBeHidden();
    let signedOut = false;
    await page.route('**/api/auth/logout', route => {
      signedOut = route.request().method() === 'POST';
      return route.fulfill({status:204});
    });
    await button.click();
    await page.locator('#logoutBtn').click();
    await expect(page).toHaveURL('http://cinemator.test/login');
    expect(signedOut).toBe(true);
  });
}

test('track summaries handle unnamed tracks and translate the disabled subtitle state', async ({page}) => {
  const row = await openPlayer(page);
  await page.route('**/api/media/info?*', r => r.fulfill({json:{
    audioTracks:[{index:0,language:'und'},{index:1,title:'Director commentary'}],
    subtitles:[{index:0,language:'eng'}],
  }}));
  await row.locator('.download-open').click();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: 1');
  await page.locator('#audioMenuButton').click();
  await page.locator('#audioTracks [data-value="1"]').click();
  await expect(page.locator('#audioMenuLabel')).toHaveText('Sound: Director commentary');
  await page.locator('#languageSelect').selectOption('ru');
  await expect(page.locator('#subtitlesMenuLabel')).toHaveText('Субтитры: выкл.');
  await page.locator('#subtitlesMenuButton').click();
  await page.locator('#subtitleTracks [data-value="0"]').click();
  await expect(page.locator('#subtitlesMenuLabel')).toHaveText('Субтитры: английский');
  await page.locator('#subtitlesMenuButton').click();
  await page.locator('#subtitleTracks [data-value="-1"]').click();
  await expect(page.locator('#subtitlesMenuLabel')).toHaveText('Субтитры: выкл.');
});
