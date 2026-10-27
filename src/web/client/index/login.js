import { setText, errorKey } from './i18n.js';

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
  setText(message, '');
  message.className = 'msg';
  try {
    const response = await fetch('/api/auth/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: password.value }),
    });
    if (!response.ok) {
      throw new Error(response.status === 401 ? 'auth.incorrectPassword' : response.status === 429 ? 'auth.tooManyAttempts' : 'auth.failed');
    }
    window.location.replace(loginDestination());
  } catch (error) {
    setText(message, errorKey(error, 'auth.failed'));
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
    setText(qrPlaceholder, 'auth.qrUnavailable');
  };
  qrCode.src = path;
}

function updateSignInExpiry() {
  if (!signInRequest) return;
  const seconds = Math.max(0, Math.ceil((new Date(signInRequest.expiresAt).getTime() - Date.now()) / 1000));
  const minutes = Math.floor(seconds / 60);
  setText(deviceExpiry, seconds > 0 ? 'auth.expiresIn' : 'auth.refreshing', { time: `${minutes}:${String(seconds % 60).padStart(2, '0')}` });
  if (seconds === 0) startSignInRequest();
}

async function waitForSignInSession() {
  if (!signInRequest) return;
  const deviceToken = signInRequest.deviceToken;
  const controller = new AbortController();
  sessionRequestController = controller;
  setText(deviceMessage, '');
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
    throw new Error('auth.qrFailed');
  } catch (error) {
    if (error.name === 'AbortError' || signInRequest?.deviceToken !== deviceToken) return;
    setText(deviceMessage, 'auth.qrReconnecting');
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
  setText(qrPlaceholder, 'auth.generatingQR');
  setText(deviceCode, '');
  setText(deviceExpiry, '');
  setText(deviceMessage, '');
  deviceMessage.className = 'msg';
  try {
    const response = await fetch('/api/auth/sign-in-requests', { method: 'POST' });
    if (!response.ok) {
      const retryAfter = Number(response.headers.get('Retry-After'));
      if (response.status === 429 && Number.isFinite(retryAfter) && retryAfter > 0) {
        retryDelay = retryAfter * 1000;
      }
      throw new Error('auth.qrTemporary');
    }
    signInRequest = await response.json();
    setText(deviceCode, 'auth.code', { code: signInRequest.code });
    showQRCode(signInRequest.qrCode);
    updateSignInExpiry();
    waitForSignInSession();
  } catch (error) {
    setText(qrPlaceholder, 'auth.qrUnavailable');
    setText(deviceMessage, errorKey(error, 'auth.qrTemporary'));
    deviceMessage.className = 'msg error';
    retryTimer = window.setTimeout(startSignInRequest, retryDelay);
  } finally {
    startingSignInRequest = false;
  }
}

window.setInterval(updateSignInExpiry, 1000);
startSignInRequest();
