const themeKey = 'theme-mode';
const themes = ['dark', 'light'];
const themeToggle = document.getElementById('themeToggle');
const moon = document.getElementById('icon-moon');
const sun = document.getElementById('icon-sun');
let themeIndex = 0;

function setTheme(index, save = true) {
  document.documentElement.setAttribute('data-theme', themes[index]);
  moon.style.display = index === 0 ? '' : 'none';
  sun.style.display = index === 1 ? '' : 'none';
  themeIndex = index;
  if (save) localStorage.setItem(themeKey, themes[index]);
}

let savedTheme = localStorage.getItem(themeKey);
if (!themes.includes(savedTheme)) {
  savedTheme = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}
setTheme(themes.indexOf(savedTheme), false);
themeToggle.addEventListener('click', () => setTheme(1 - themeIndex));

const form = document.getElementById('loginForm');
const password = document.getElementById('password');
const button = document.getElementById('loginButton');
const message = document.getElementById('loginMessage');
const qrCode = document.getElementById('deviceQRCode');
const qrPlaceholder = document.getElementById('qrPlaceholder');
const deviceCode = document.getElementById('deviceCode');
const deviceExpiry = document.getElementById('deviceExpiry');
const deviceMessage = document.getElementById('deviceMessage');

function loginDestination() {
  const next = new URLSearchParams(window.location.search).get('next');
  if (!next || !next.startsWith('/') || next.startsWith('//') || next.includes('\\')) return '/';
  const destination = new URL(next, window.location.origin);
  if (destination.origin !== window.location.origin) return '/';
  return destination.pathname + destination.search + destination.hash;
}

form.addEventListener('submit', async event => {
  event.preventDefault();
  button.disabled = true;
  message.textContent = '';
  message.className = 'msg';
  try {
    const response = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: password.value }),
    });
    if (!response.ok) {
      throw new Error(response.status === 401 ? 'Incorrect password' : 'Could not sign in');
    }
    window.location.replace(loginDestination());
  } catch (error) {
    message.textContent = error.message || 'Could not sign in';
    message.className = 'msg error';
    password.select();
    button.disabled = false;
  }
});

let signInRequest;
let sessionRequestController;
let retryTimer;
let startingSignInRequest = false;

function showQRCode(path) {
  qrCode.onload = () => {
    qrCode.hidden = false;
    qrPlaceholder.hidden = true;
  };
  qrCode.onerror = () => {
    qrCode.hidden = true;
    qrPlaceholder.hidden = false;
    qrPlaceholder.textContent = 'QR code unavailable';
  };
  qrCode.src = path;
}

function updateSignInExpiry() {
  if (!signInRequest) return;
  const seconds = Math.max(0, Math.ceil((new Date(signInRequest.expiresAt).getTime() - Date.now()) / 1000));
  const minutes = Math.floor(seconds / 60);
  deviceExpiry.textContent = seconds > 0
    ? `Expires in ${minutes}:${String(seconds % 60).padStart(2, '0')}`
    : 'Refreshing…';
  if (seconds === 0) startSignInRequest();
}

async function waitForSignInSession() {
  if (!signInRequest) return;
  const deviceToken = signInRequest.deviceToken;
  const controller = new AbortController();
  sessionRequestController = controller;
  deviceMessage.textContent = '';
  deviceMessage.className = 'msg';
  try {
    const response = await fetch(`/api/auth/sign-in-requests/${encodeURIComponent(deviceToken)}/session`, {
      method: 'POST',
      signal: controller.signal,
    });
    if (response.status === 204) {
      window.location.replace(loginDestination());
      return;
    }
    if (response.status === 410) {
      if (signInRequest?.deviceToken === deviceToken) await startSignInRequest();
      return;
    }
    throw new Error('Could not complete QR sign-in');
  } catch (error) {
    if (error.name === 'AbortError' || signInRequest?.deviceToken !== deviceToken) return;
    deviceMessage.textContent = 'Reconnecting QR sign-in…';
    deviceMessage.className = 'msg error';
    retryTimer = window.setTimeout(waitForSignInSession, 1000);
  } finally {
    if (sessionRequestController === controller) sessionRequestController = undefined;
  }
}

async function startSignInRequest() {
  if (startingSignInRequest) return;
  startingSignInRequest = true;
  let retryDelay = 5000;
  window.clearTimeout(retryTimer);
  sessionRequestController?.abort();
  sessionRequestController = undefined;
  signInRequest = undefined;
  qrCode.hidden = true;
  qrPlaceholder.hidden = false;
  qrPlaceholder.textContent = 'Generating QR code…';
  deviceCode.textContent = '';
  deviceExpiry.textContent = '';
  deviceMessage.textContent = '';
  deviceMessage.className = 'msg';
  try {
    const response = await fetch('/api/auth/sign-in-requests', { method: 'POST' });
    if (!response.ok) {
      const retryAfter = Number(response.headers.get('Retry-After'));
      if (response.status === 429 && Number.isFinite(retryAfter) && retryAfter > 0) {
        retryDelay = retryAfter * 1000;
      }
      throw new Error('QR sign-in is temporarily unavailable');
    }
    signInRequest = await response.json();
    deviceCode.textContent = `Code ${signInRequest.code}`;
    showQRCode(signInRequest.qrCode);
    updateSignInExpiry();
    waitForSignInSession();
  } catch (error) {
    qrPlaceholder.textContent = 'QR code unavailable';
    deviceMessage.textContent = error.message || 'QR sign-in is temporarily unavailable';
    deviceMessage.className = 'msg error';
    retryTimer = window.setTimeout(startSignInRequest, retryDelay);
  } finally {
    startingSignInRequest = false;
  }
}

window.setInterval(updateSignInExpiry, 1000);
startSignInRequest();
