(() => {
  const themeKey = 'theme-mode';
  const modes = ['system', 'light', 'dark'];
  const systemTheme = window.matchMedia('(prefers-color-scheme: dark)');
  const choices = document.querySelectorAll('input[name="theme-mode"]');
  const moon = document.getElementById('icon-moon');
  const sun = document.getElementById('icon-sun');

  function readMode() {
    try {
      const saved = localStorage.getItem(themeKey);
      if (modes.includes(saved)) return saved;
    } catch {}
    return 'system';
  }

  let mode = readMode();
  function applyTheme() {
    const theme = mode === 'system' ? (systemTheme.matches ? 'dark' : 'light') : mode;
    document.documentElement.setAttribute('data-theme', theme);
    for (const choice of choices) choice.checked = choice.value === mode;
    if (moon) moon.style.display = theme === 'dark' ? '' : 'none';
    if (sun) sun.style.display = theme === 'light' ? '' : 'none';
  }

  for (const choice of choices) {
    choice.addEventListener('change', () => {
      if (!choice.checked) return;
      mode = choice.value;
      try { localStorage.setItem(themeKey, mode); } catch {}
      applyTheme();
    });
  }
  systemTheme.addEventListener('change', () => {
    if (mode === 'system') applyTheme();
  });
  window.addEventListener('storage', event => {
    if (event.key !== null && event.key !== themeKey) return;
    mode = readMode();
    applyTheme();
  });
  document.getElementById('themeMenu')?.addEventListener('toggle', event => {
    document.getElementById('themeToggle').setAttribute('aria-expanded', String(event.newState === 'open'));
  });
  applyTheme();
})();
