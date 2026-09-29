'use strict';
// Sets the theme before the first paint (a deferred script would flash the
// other one): the one chosen in the panel, else the system's, following it
// when it changes.
(() => {
  const system = matchMedia('(prefers-color-scheme: light)');
  const saved = () => { try { return localStorage.getItem('wpgenie_theme'); } catch (e) { return null; } };
  const apply = () => { document.documentElement.dataset.theme = saved() || (system.matches ? 'light' : 'dark'); };
  apply();
  system.addEventListener('change', apply);
})();
