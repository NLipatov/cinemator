import en from './locales/en.js';
import ru from './locales/ru.js';
import zh from './locales/zh-Hans.js';

const messages = { en, ru, 'zh-Hans': zh };
const storageKey = 'cinemator-language';
let language;
try {
  language = localStorage.getItem(storageKey);
} catch {}
if (!Object.hasOwn(messages, language)) {
  language =
    (navigator.languages || [navigator.language])
      .map((tag) => {
        const base = tag.toLowerCase().split('-')[0];
        return base === 'zh' ? 'zh-Hans' : base;
      })
      .find((tag) => Object.hasOwn(messages, tag)) || 'en';
}

export function number(value, options = {}) {
  return new Intl.NumberFormat(language, options).format(value);
}

export function t(key, params = {}) {
  let message = messages[language][key] ?? en[key] ?? key;
  if (typeof message === 'object') {
    message = message[new Intl.PluralRules(language).select(params.count)] ?? message.other;
  }
  return message.replace(/\{(\w+)\}/g, (_, name) =>
    typeof params[name] === 'number' ? number(params[name]) : String(params[name] ?? `{${name}}`),
  );
}

export function errorKey(error, fallback) {
  return Object.hasOwn(en, error?.message) ? error.message : fallback;
}

export function setText(element, key, params = {}) {
  if (!key) {
    delete element.dataset.i18n;
    delete element.dataset.i18nParams;
    element.textContent = '';
    return;
  }
  element.dataset.i18n = key;
  element.dataset.i18nParams = JSON.stringify(params);
  element.textContent = t(key, params);
}

function localize(root = document) {
  for (const element of root.querySelectorAll('[data-i18n]')) {
    element.textContent = t(element.dataset.i18n, JSON.parse(element.dataset.i18nParams || '{}'));
  }
  for (const attribute of ['title', 'aria-label', 'placeholder', 'alt']) {
    for (const element of root.querySelectorAll(`[data-i18n-${attribute}]`)) {
      element.setAttribute(attribute, t(element.getAttribute(`data-i18n-${attribute}`)));
    }
  }
  for (const template of root.querySelectorAll('template')) localize(template.content);
}

function applyLanguage() {
  try {
    localStorage.setItem(storageKey, language);
  } catch {}
  document.documentElement.lang = language;
  document.getElementById('languageSelect').value = language;
  localize();
}

document.getElementById('languageSelect').addEventListener('change', (event) => {
  if (!Object.hasOwn(messages, event.target.value)) return;
  language = event.target.value;
  applyLanguage();
  document.dispatchEvent(new Event('cinemator:languagechange'));
});
applyLanguage();
