import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import en from './index/locales/en.js';
import ru from './index/locales/ru.js';
import zh from './index/locales/zh-Hans.js';

let instance = 0;
async function openClient({ languages = ['en'], storage = new Map(), blocked = false } = {}) {
  const select = new EventTarget();
  const document = new EventTarget();
  document.documentElement = {};
  document.getElementById = (id) => {
    assert.equal(id, 'languageSelect');
    return select;
  };
  // These tests exercise selection, persistence and formatting. DOM rendering is
  // covered by browser scenarios against the real HTML pages.
  document.querySelectorAll = () => [];
  globalThis.document = document;
  Object.defineProperty(globalThis, 'navigator', { configurable: true, value: { languages } });
  globalThis.localStorage = {
    getItem(key) {
      if (blocked) throw new Error('Storage blocked');
      return storage.get(key) ?? null;
    },
    setItem(key, value) {
      if (blocked) throw new Error('Storage blocked');
      storage.set(key, value);
    },
  };
  const i18n = await import(`./index/i18n.js?test=${++instance}`);
  return { ...i18n, document, storage, select };
}

test('first supported browser preference is selected and persisted', async () => {
  for (const [languages, expected] of [
    [['ru-RU', 'en-US'], 'ru'],
    [['de-DE', 'zh-CN', 'ru'], 'zh-Hans'],
    [['zh-Hans-SG'], 'zh-Hans'],
    [['zh-TW'], 'zh-Hans'],
    [['en-GB', 'ru'], 'en'],
    [['fr', 'de'], 'en'],
    [[], 'en'],
  ]) {
    const client = await openClient({ languages });
    assert.equal(client.document.documentElement.lang, expected);
    assert.equal(client.select.value, expected);
    assert.equal(client.storage.get('cinemator-language'), expected);
  }
});

test('automatically selected language survives a browser language change', async () => {
  const storage = new Map();
  await openClient({ languages: ['ru'], storage });
  const client = await openClient({ languages: ['zh-CN'], storage });
  assert.equal(client.document.documentElement.lang, 'ru');
});

test('English fallback is persisted too', async () => {
  const storage = new Map();
  await openClient({ languages: ['fr'], storage });
  const client = await openClient({ languages: ['ru'], storage });
  assert.equal(client.document.documentElement.lang, 'en');
});

test('invalid stored language is replaced with a supported browser preference', async () => {
  for (const stored of ['fr', '', '__proto__']) {
    const storage = new Map([['cinemator-language', stored]]);
    const client = await openClient({ languages: ['ru'], storage });
    assert.equal(client.document.documentElement.lang, 'ru');
    assert.equal(storage.get('cinemator-language'), 'ru');
  }
});

test('manual selection updates translations, emits one event and persists', async () => {
  const client = await openClient({ languages: ['en'] });
  let events = 0;
  client.document.addEventListener('cinemator:languagechange', () => events++);
  client.select.value = 'ru';
  client.select.dispatchEvent(new Event('change'));
  assert.equal(client.t('auth.password'), 'Пароль');
  assert.equal(client.document.documentElement.lang, 'ru');
  assert.equal(client.storage.get('cinemator-language'), 'ru');
  assert.equal(events, 1);
  const next = await openClient({ languages: ['en'], storage: client.storage });
  assert.equal(next.t('auth.password'), 'Пароль');
});

test('unavailable storage does not prevent localization', async () => {
  const client = await openClient({ languages: ['ru'], blocked: true });
  assert.equal(client.t('auth.password'), 'Пароль');
});

test('Russian plurals and localized numbers cover counts beyond singular/plural', async () => {
  const client = await openClient({ languages: ['ru'] });
  for (const [count, expected] of [
    [0, '0 дней'],
    [1, '1 день'],
    [2, '2 дня'],
    [5, '5 дней'],
    [11, '11 дней'],
    [21, '21 день'],
    [22, '22 дня'],
    [1.5, '1,5 дня'],
  ]) {
    assert.equal(client.t('downloads.days', { count }), expected);
  }
  assert.equal(client.number(1.5), '1,5');
  client.select.value = 'zh-Hans';
  client.select.dispatchEvent(new Event('change'));
  assert.equal(client.t('downloads.days', { count: 21 }), '21 天');
  client.select.value = 'en';
  client.select.dispatchEvent(new Event('change'));
  assert.equal(client.t('downloads.days', { count: 1 }), '1 day');
  assert.equal(client.t('downloads.days', { count: 2 }), '2 days');
});

test('messages retain parameters for retranslation and insert plain text', async () => {
  const client = await openClient({ languages: ['en'] });
  const element = { dataset: {}, textContent: '' };
  const title = '<img src=x onerror=alert(1)>';
  client.setText(element, 'downloads.confirmDelete', { title });
  assert.equal(element.textContent, `Delete ${title}?`);
  assert.equal(JSON.parse(element.dataset.i18nParams).title, title);
  client.setText(element, '');
  assert.equal(element.textContent, '');
  assert.deepEqual(element.dataset, {});
  assert.equal(client.errorKey(new Error('errors.unsupported_magnet'), 'errors.files'), 'errors.unsupported_magnet');
  assert.equal(client.errorKey(new Error('old server message'), 'errors.files'), 'errors.files');
});

test('all languages cover the same keys and interpolation parameters', () => {
  const placeholders = (value) => [...value.matchAll(/\{(\w+)\}/g)].map((m) => m[1]).sort();
  for (const messages of [ru, zh]) {
    assert.deepEqual(Object.keys(messages).sort(), Object.keys(en).sort());
    for (const [key, value] of Object.entries(messages)) {
      const base = typeof en[key] === 'string' ? en[key] : en[key].other;
      const variants = typeof value === 'string' ? [value] : Object.values(value);
      if (typeof value === 'object') assert.ok(value.other, key);
      for (const text of variants) {
        assert.ok(text.length, key);
        assert.deepEqual(placeholders(text), placeholders(base), key);
      }
    }
  }
});

test('HTML translation keys and plural parameters resolve in every language', async () => {
  for (const file of ['index', 'login', 'sign-in-approval']) {
    const html = await readFile(new URL(`./index/${file}.html`, import.meta.url), 'utf8');
    for (const match of html.matchAll(/data-i18n(?:-title|-aria-label|-placeholder|-alt)?="([^"]+)"/g)) {
      assert.ok(Object.hasOwn(en, match[1]), `${file}: ${match[1]}`);
    }
    for (const match of html.matchAll(/data-i18n-params='([^']+)'/g)) assert.doesNotThrow(() => JSON.parse(match[1]));
  }
});
