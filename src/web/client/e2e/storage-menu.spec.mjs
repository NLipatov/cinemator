import {test, expect} from '@playwright/test';
import {openPlayer} from './player-fixture.mjs';

test('storage and action menus share keyboard navigation, dismissal and focus restoration', async ({page}) => {
  const row = await openPlayer(page);
  const storage = row.locator('.download-storage-button');
  const more = row.locator('.download-more');
  const popup = row.locator('.download-options');
  await storage.focus();
  await storage.press('Enter');
  await expect(storage).toHaveAttribute('aria-expanded', 'true');
  await expect(popup).toHaveAccessibleName('Storage period');
  await expect(row.locator('.download-expiry-date')).toHaveAttribute('datetime', '2099-01-01T00:00:00.000Z');
  await expect(row.locator('[data-days="1"]')).toBeFocused();
  await expect(row.locator('[data-action="restart"]')).toBeHidden();
  await expect(row.locator('[data-action="delete"]')).toBeHidden();
  await page.keyboard.press('ArrowDown');
  await expect(row.locator('[data-days="7"]')).toBeFocused();
  await page.keyboard.press('End');
  await expect(row.locator('[data-days="30"]')).toBeFocused();
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(row.locator('[data-days="30"]')).toBeFocused();
  await expect(storage).toHaveAttribute('aria-expanded', 'true');
  await page.keyboard.press('Escape');
  await expect(storage).toBeFocused();
  await expect(popup).toBeHidden();
  await more.click();
  await expect(row.locator('[data-action="restart"]')).toBeVisible();
  await expect(row.locator('.download-storage-summary')).toBeHidden();
  await storage.click();
  await expect(more).toHaveAttribute('aria-expanded', 'false');
  await expect(storage).toHaveAttribute('aria-expanded', 'true');
  await expect(row.locator('.download-storage-summary')).toBeVisible();
  await page.locator('.download-row[data-id="other"] .download-more').click();
  await expect(popup).toBeHidden();
  await expect(storage).toHaveAttribute('aria-expanded', 'false');
  await expect(page.locator('.download-options:visible')).toHaveCount(1);
  await page.getByRole('heading', {name: 'Downloads', exact: true}).click();
  await expect(page.locator('.download-options:visible')).toHaveCount(0);
  await expect(page.locator('#playerWorkspace')).toBeHidden();
});

test('extending from the storage pill updates the countdown without starting playback', async ({page}) => {
  const now = new Date('2026-01-01T00:00:00Z');
  await page.clock.setFixedTime(now);
  const row = await openPlayer(page, {language: 'ru'});
  const download = {id: 'ready', title: 'Film', status: 'ready', size: 1024, expiresAt: '2026-01-02T00:00:00Z'};
  await page.route(/\/api\/downloads(?:\?|$)/, route => route.fulfill({json: [download]}));
  let requested;
  await page.route('**/api/downloads/ready/extend', route => {
    requested = route.request().postDataJSON();
    download.expiresAt = new Date(+new Date(download.expiresAt) + requested.days * 86400000).toISOString();
    return route.fulfill({status: 204});
  });
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  const storage = row.locator('.download-storage-button');
  await expect(storage).toHaveText('1 д');
  await storage.click();
  await expect(row.locator('.download-expiry')).toHaveText('Удалится через 1 день');
  await row.locator('[data-days="7"]').click();
  await expect(storage).toHaveText('1 нед');
  expect(requested).toEqual({days: 7});
  await expect(storage).toBeFocused();
  await expect(row.locator('.download-options')).toBeHidden();
  await storage.click();
  await expect(row.locator('.download-expiry')).toHaveText('Удалится через 1 неделю и 1 день');
  await expect(row.locator('.download-expiry-date')).toHaveAttribute('datetime', '2026-01-09T00:00:00.000Z');
  await expect(page.locator('#playerWorkspace')).toBeHidden();
});

test('storage details follow the chosen language and browser timezone', async ({page}) => {
  await page.clock.setFixedTime(new Date('2026-01-01T00:00:00Z'));
  const row = await openPlayer(page);
  const storage = row.locator('.download-storage-button');
  const dates = new Set();
  for (const language of ['en', 'ru', 'zh-Hans', 'zh-Hant']) {
    await page.locator('#languageSelect').selectOption(language);
    await storage.click();
    const expected = await page.evaluate(language => new Intl.DateTimeFormat(language, {dateStyle: 'long', timeStyle: 'short'}).format(new Date('2099-01-01T00:00:00Z')), language);
    await expect(row.locator('.download-expiry-date')).toHaveText(expected);
    dates.add(expected);
    await page.keyboard.press('Escape');
    await expect(storage).toBeFocused();
  }
  expect(dates.size).toBeGreaterThanOrEqual(3);
});

test('missing expiry is explicit and preparation only exposes existing expiry details', async ({page}) => {
  await openPlayer(page);
  const downloads = [
    {id: 'unlimited', status: 'ready', expiresAt: ''},
    {id: 'invalid', status: 'ready', expiresAt: 'invalid'},
    {id: 'zero', status: 'preparing', expiresAt: '0001-01-01T00:00:00Z'},
    {id: 'preparing', status: 'preparing', expiresAt: '2099-01-01T00:00:00Z'},
  ].map(download => ({title: 'Film', size: 1024, ...download}));
  await page.route(/\/api\/downloads(?:\?|$)/, route => route.fulfill({json: downloads}));
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  for (const id of ['unlimited', 'invalid']) {
    const row = page.locator(`.download-row[data-id="${id}"]`);
    await expect(row.locator('.download-storage-button')).toHaveText('∞');
    await row.locator('.download-storage-button').click();
    await expect(row.locator('.download-expiry')).toHaveText('No automatic deletion');
    await expect(row.locator('.download-expiry-date')).toBeHidden();
    await page.keyboard.press('Escape');
  }
  await expect(page.locator('.download-row[data-id="zero"] .download-storage-button')).toBeHidden();
  const preparing = page.locator('.download-row[data-id="preparing"]');
  await preparing.locator('.download-storage-button').click();
  await expect(preparing.locator('.download-expiry-date')).toBeVisible();
  await expect(preparing.locator('.download-extend-group')).toBeHidden();
  downloads[3].expiresAt = '';
  await page.evaluate(() => window.dispatchEvent(new Event('focus')));
  await expect(preparing.locator('.download-options')).toBeHidden();
  await expect(preparing.locator('.download-more')).toBeFocused();
  await expect(page.locator('#playerWorkspace')).toBeHidden();
});
