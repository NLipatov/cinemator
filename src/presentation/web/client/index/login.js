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
    window.location.replace('/');
  } catch (error) {
    message.textContent = error.message || 'Could not sign in';
    message.className = 'msg error';
    password.select();
    button.disabled = false;
  }
});
