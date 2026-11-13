import {test, expect} from '@playwright/test';
import {openPage} from './fixtures.mjs';

const choice = (page, mode) => page.locator(`input[name="theme-mode"][value="${mode}"]`);
const trigger = (page, path) => page.locator(path === '/' ? '#appMenuButton' : '#themeToggle');

for (const path of ['/', '/login', '/sign-in-approvals/test']) {
  test(`${path}: system theme follows OS changes and manual choices persist`, async ({page}) => {
    await page.emulateMedia({colorScheme:'dark'});
    await openPage(page, path, 'ru');
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await trigger(page, path).click();
    await expect(choice(page, 'system')).toBeChecked();
    await page.emulateMedia({colorScheme:'light'});
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
    await choice(page, 'dark').check();
    await page.emulateMedia({colorScheme:'dark'});
    await page.emulateMedia({colorScheme:'light'});
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await page.reload();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await trigger(page, path).click();
    await expect(choice(page, 'dark')).toBeChecked();
    await choice(page, 'light').check();
    await page.emulateMedia({colorScheme:'dark'});
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
    await choice(page, 'system').check();
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'dark');
    await page.reload();
    await trigger(page, path).click();
    await expect(choice(page, 'system')).toBeChecked();
    await page.emulateMedia({colorScheme:'light'});
    await expect(page.locator('html')).toHaveAttribute('data-theme', 'light');
    await page.keyboard.press('Escape');
    await expect(trigger(page, path)).toBeFocused();
  });
}

for (const saved of ['light', 'dark', 'unknown']) {
  test(`existing ${saved} theme preference is handled`, async ({page}) => {
    await page.addInitScript(saved => localStorage.setItem('theme-mode', saved), saved);
    await page.emulateMedia({colorScheme:'dark'});
    await openPage(page, '/', 'en');
    await expect(page.locator('html')).toHaveAttribute('data-theme', saved === 'light' ? 'light' : 'dark');
    await trigger(page, '/').click();
    await expect(choice(page, saved === 'unknown' ? 'system' : saved)).toBeChecked();
  });
}

test('theme choices remain usable when browser storage is unavailable', async ({page}) => {
  await page.addInitScript(() => {
    Object.defineProperty(window, 'localStorage', {get() {throw new DOMException('Unavailable', 'SecurityError');}});
  });
  await page.emulateMedia({colorScheme:'light'});
  await openPage(page, '/login');
  await trigger(page, '/login').click();
  await choice(page, 'dark').check();
  await expect(page.locator('html')).toHaveAttribute('data-theme','dark');
  await choice(page, 'system').check();
  await expect(page.locator('html')).toHaveAttribute('data-theme','light');
});

test('theme preference stays in sync between tabs', async ({page, context}) => {
  await page.emulateMedia({colorScheme:'light'});
  await openPage(page, '/', 'en');
  const other = await context.newPage();
  await other.emulateMedia({colorScheme:'light'});
  await openPage(other, '/login', 'en');
  await trigger(page, '/').click();
  await choice(page, 'dark').check();
  await expect(other.locator('html')).toHaveAttribute('data-theme','dark');
  await trigger(other, '/login').click();
  await expect(choice(other, 'dark')).toBeChecked();
  await choice(other, 'system').check();
  await expect(page.locator('html')).toHaveAttribute('data-theme','light');
  await expect(choice(page, 'system')).toBeChecked();
  await other.close();
});

for (const language of ['ru', 'en', 'zh-Hans', 'zh-Hant']) {
  test(`${language}: theme picker fits narrow screens with enlarged text and supports keyboard`, async ({page}) => {
    await openPage(page, '/', language);
    await page.setViewportSize({width:320,height:844});
    await page.evaluate(() => document.documentElement.style.fontSize = '200%');
    await trigger(page, '/').press('Enter');
    await choice(page, 'system').focus();
    await page.keyboard.press('ArrowDown');
    await expect(choice(page, 'light')).toBeChecked();
    await expect(page.locator('html')).toHaveAttribute('data-theme','light');
    for (const label of await page.locator('.theme-options label').all()) {
      await label.scrollIntoViewIfNeeded();
      const box = await label.boundingBox();
      expect(box.x).toBeGreaterThanOrEqual(0);
      expect(box.x + box.width).toBeLessThanOrEqual(320);
      expect(await label.evaluate(el => el.scrollWidth <= el.clientWidth + 1)).toBe(true);
    }
    await page.keyboard.press('Escape');
    await expect(trigger(page, '/')).toBeFocused();
  });
}
