import { setText, errorKey } from './i18n.js';

const token = window.location.pathname.split('/').filter(Boolean).pop();
const title = document.getElementById('deviceTitle');
const description = document.getElementById('deviceDescription');
const code = document.getElementById('approvalCode');
const expiry = document.getElementById('approvalExpiry');
const allowButton = document.getElementById('allowDevice');
const message = document.getElementById('approvalMessage');
let expiresAt;

function showUnavailable(key) {
  setText(title, 'approval.unavailable');
  setText(description, key);
  code.hidden = true;
  setText(expiry, '');
  allowButton.hidden = true;
}

function updateExpiry() {
  if (!expiresAt) return;
  const seconds = Math.max(0, Math.ceil((expiresAt.getTime() - Date.now()) / 1000));
  const minutes = Math.floor(seconds / 60);
  setText(expiry, seconds > 0 ? 'auth.expiresIn' : 'auth.expired', { time: `${minutes}:${String(seconds % 60).padStart(2, '0')}` });
  if (seconds === 0) showUnavailable('approval.expired');
}

async function loadAuthorization() {
  if (!token) {
    showUnavailable('approval.invalid');
    return;
  }
  try {
    const response = await fetch(`/api/auth/sign-in-approvals/${encodeURIComponent(token)}`);
    if (response.status === 401) {
      const next = window.location.pathname + window.location.search;
      window.location.replace(`/login?next=${encodeURIComponent(next)}`);
      return;
    }
    if (response.status === 410) {
      showUnavailable('approval.expired');
      return;
    }
    if (!response.ok) throw new Error('approval.loadFailed');
    const authorization = await response.json();
    setText(code, '');
    code.textContent = authorization.code;
    expiresAt = new Date(authorization.expiresAt);
    updateExpiry();
  } catch (error) {
    showUnavailable(errorKey(error, 'approval.loadFailed'));
  }
}

async function approve() {
  allowButton.disabled = true;
  setText(message, '');
  message.className = 'msg';
  try {
    const response = await fetch(`/api/auth/sign-in-approvals/${encodeURIComponent(token)}`, {
      method: 'POST',
    });
    if (response.status === 410) {
      showUnavailable('approval.expired');
      return;
    }
    if (!response.ok) throw new Error('approval.updateFailed');
    expiresAt = undefined;
    setText(title, 'approval.allowed');
    setText(description, 'approval.return');
    code.hidden = true;
    setText(expiry, '');
    allowButton.hidden = true;
  } catch (error) {
    setText(message, errorKey(error, 'approval.updateFailed'));
    message.className = 'msg error';
    allowButton.disabled = false;
  }
}

allowButton.addEventListener('click', approve);
window.setInterval(updateExpiry, 1000);
loadAuthorization();
