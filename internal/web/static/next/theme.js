'use strict';
// Sets the theme before the first paint (a module script would flash the
// other one): the one chosen in the panel, else the system's, following it
// when it changes. Shares its key with the legacy panel.
(() => {
  const system = matchMedia('(prefers-color-scheme: light)');
  const saved = () => { try { return localStorage.getItem('wpgenie_theme'); } catch (e) { return null; } };
  const apply = () => {
    const theme = saved() || (system.matches ? 'light' : 'dark');
    document.documentElement.classList.toggle('dark', theme === 'dark');
    document.documentElement.dataset.theme = theme;
  };
  apply();
  system.addEventListener('change', apply);
})();
