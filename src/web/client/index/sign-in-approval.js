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

const token = window.location.pathname.split('/').filter(Boolean).pop();
const title = document.getElementById('deviceTitle');
const description = document.getElementById('deviceDescription');
const code = document.getElementById('approvalCode');
const expiry = document.getElementById('approvalExpiry');
const allowButton = document.getElementById('allowDevice');
const message = document.getElementById('approvalMessage');
let expiresAt;

function showUnavailable(text) {
  title.textContent = 'Sign-in unavailable';
  description.textContent = text;
  code.hidden = true;
  expiry.textContent = '';
  allowButton.hidden = true;
}

function updateExpiry() {
  if (!expiresAt) return;
  const seconds = Math.max(0, Math.ceil((expiresAt.getTime() - Date.now()) / 1000));
  const minutes = Math.floor(seconds / 60);
  expiry.textContent = seconds > 0
    ? `Expires in ${minutes}:${String(seconds % 60).padStart(2, '0')}`
    : 'Expired';
  if (seconds === 0) showUnavailable('This QR code has expired. Refresh it on the other device.');
}

async function loadAuthorization() {
  if (!token) {
    showUnavailable('The sign-in link is invalid.');
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
      showUnavailable('This QR code has expired. Refresh it on the other device.');
      return;
    }
    if (!response.ok) throw new Error('Could not load the sign-in request.');
    const authorization = await response.json();
    code.textContent = authorization.code;
    expiresAt = new Date(authorization.expiresAt);
    updateExpiry();
  } catch (error) {
    showUnavailable(error.message || 'Could not load the sign-in request.');
  }
}

async function approve() {
  allowButton.disabled = true;
  message.textContent = '';
  message.className = 'msg';
  try {
    const response = await fetch(`/api/auth/sign-in-approvals/${encodeURIComponent(token)}`, {
      method: 'POST',
    });
    if (response.status === 410) {
      showUnavailable('This QR code has expired. Refresh it on the other device.');
      return;
    }
    if (!response.ok) throw new Error('Could not update the sign-in request.');
    expiresAt = undefined;
    title.textContent = 'Sign-in allowed';
    description.textContent = 'You can return to the other device.';
    code.hidden = true;
    expiry.textContent = '';
    allowButton.hidden = true;
  } catch (error) {
    message.textContent = error.message || 'Could not update the sign-in request.';
    message.className = 'msg error';
    allowButton.disabled = false;
  }
}

allowButton.addEventListener('click', approve);
window.setInterval(updateExpiry, 1000);
loadAuthorization();
