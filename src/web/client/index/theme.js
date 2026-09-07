(() => {
  const themeKey = 'theme-mode';
  const themes = ['dark', 'light'];
  const toggle = document.getElementById('themeToggle');
  const moon = document.getElementById('icon-moon');
  const sun = document.getElementById('icon-sun');
  let theme = localStorage.getItem(themeKey);
  if (!themes.includes(theme)) {
    theme = window.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  function applyTheme() {
    document.documentElement.setAttribute('data-theme', theme);
    moon.style.display = theme === 'dark' ? '' : 'none';
    sun.style.display = theme === 'light' ? '' : 'none';
  }
  applyTheme();
  toggle.addEventListener('click', () => {
    theme = theme === 'dark' ? 'light' : 'dark';
    localStorage.setItem(themeKey, theme);
    applyTheme();
  });
})();
