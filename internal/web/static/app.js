'use strict';

const $ = (sel, el = document) => el.querySelector(sel);
const TOKEN_KEY = 'wpgenie_token';

async function api(method, path, body) {
  const res = await fetch('/api/v1' + path, {
    method,
    headers: {
      Authorization: 'Bearer ' + localStorage.getItem(TOKEN_KEY),
      ...(body ? { 'Content-Type': 'application/json' } : {}),
    },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401) { signOut(); throw new Error('Session expired'); }
  if (res.status === 204) return null;
  const data = await res.json();
  if (!res.ok) throw new Error(data.error || res.statusText);
  return data;
}

function fmtBytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + ' ' + u[i];
}
const fmtNum = (n) => new Intl.NumberFormat().format(n);

function showError(err) {
  const box = $('#error');
  box.textContent = err ? String(err.message || err) : '';
  box.hidden = !err;
}

function signOut() {
  localStorage.removeItem(TOKEN_KEY);
  $('#app').hidden = true;
  $('#logout').hidden = true;
  $('#login').hidden = false;
}

async function load() {
  showError(null);
  const sites = await api('GET', '/sites');
  const list = $('#sites');
  list.replaceChildren(...sites.map(renderSite));
  $('#empty').hidden = sites.length > 0;
  sites.filter((s) => s.status === 'active').forEach(loadStats);
}

function renderSite(site) {
  const el = $('#site-tpl').content.firstElementChild.cloneNode(true);
  el.dataset.id = site.id;
  const a = $('.domain', el);
  a.textContent = site.primary_domain;
  a.href = 'https://' + site.primary_domain;
  $('.id', el).textContent = site.id + ' · PHP ' + site.php_version;
  const pill = $('.status', el);
  pill.textContent = site.status;
  pill.classList.add(site.status);

  const mode = $('.mode', el), ai = $('.ai', el);
  mode.value = site.shield_mode;
  ai.checked = site.block_ai_bots;
  const saveShield = async () => {
    try { await api('PUT', `/sites/${site.id}/shield`, { mode: mode.value, block_ai_bots: ai.checked }); }
    catch (e) { showError(e); }
  };
  mode.addEventListener('change', saveShield);
  ai.addEventListener('change', saveShield);

  renderPerf(el, site);

  $('.delete', el).addEventListener('click', async () => {
    const typed = prompt(`This permanently deletes ${site.primary_domain}, its files and database.\nType the domain to confirm:`);
    if (typed !== site.primary_domain) return;
    try { await api('DELETE', `/sites/${site.id}`); await load(); } catch (e) { showError(e); }
  });
  return el;
}

const MEMORY_MB = [256, 512, 768, 1024, 1536, 2048, 3072, 4096, 6144, 8192];
const CPUS = [0.25, 0.5, 1, 1.5, 2, 3, 4, 6, 8];

function fillSelect(sel, values, current, label) {
  const vals = [...new Set([...values, current])].sort((a, b) => a - b);
  sel.replaceChildren(...vals.map((v) => {
    const o = document.createElement('option');
    o.value = v;
    o.textContent = label(v);
    o.selected = v === current;
    return o;
  }));
}

const fmtMem = (mb) => (mb >= 1024 ? `${mb / 1024} GB` : `${mb} MB`);

function renderPerf(el, site) {
  $('.shape', el).textContent =
    `· ${site.replicas} × ${fmtMem(site.memory_mb)} / ${site.cpus} CPU` +
    (site.page_cache ? ' · page cache' : '') + (site.object_cache ? ' · object cache' : '');
  const replicas = $('.replicas', el), memory = $('.memory', el), cpus = $('.cpus', el);
  fillSelect(replicas, [1, 2, 3, 4, 5, 6, 7, 8], site.replicas, (v) => String(v));
  fillSelect(memory, MEMORY_MB, site.memory_mb, fmtMem);
  fillSelect(cpus, CPUS, site.cpus, (v) => `${v} CPU`);
  const active = site.status === 'active';
  el.querySelectorAll('.perf select, .perf input, .perf button').forEach((c) => { c.disabled = !active; });

  const apply = $('.apply', el);
  apply.addEventListener('click', async () => {
    apply.disabled = true;
    apply.textContent = 'Scaling…';
    try {
      await api('PUT', `/sites/${site.id}/resources`, {
        replicas: Number(replicas.value), memory_mb: Number(memory.value), cpus: Number(cpus.value),
      });
      await load();
    } catch (e) { showError(e); apply.disabled = false; apply.textContent = 'Apply'; }
  });

  const page = $('.page-cache', el), object = $('.object-cache', el);
  page.checked = site.page_cache;
  object.checked = site.object_cache;
  const saveCache = async () => {
    page.disabled = object.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/cache`, { page_cache: page.checked, object_cache: object.checked });
      await load();
    } catch (e) { showError(e); await load(); }
  };
  page.addEventListener('change', saveCache);
  object.addEventListener('change', saveCache);

  const purge = $('.purge', el);
  purge.addEventListener('click', async () => {
    purge.disabled = true;
    try { await api('POST', `/sites/${site.id}/cache/purge`); purge.textContent = 'Purged ✓'; }
    catch (e) { showError(e); }
    finally { setTimeout(() => { purge.disabled = false; purge.textContent = 'Purge cache'; }, 1500); }
  });
}

async function loadStats(site) {
  try {
    const s = await api('GET', `/sites/${site.id}/stats?hours=24`);
    const el = document.querySelector(`[data-id="${site.id}"]`);
    if (!el) return;
    const set = (k, v) => { $(`[data-k="${k}"]`, el).textContent = v; };
    set('visitors', fmtNum(s.unique_visitors));
    set('page_views', fmtNum(s.totals.page_views));
    set('bytes_out', fmtBytes(s.totals.bytes_out));
    set('blocked', fmtNum(s.totals.blocked));
    set('bot_hits', fmtNum(s.totals.bot_hits));
  } catch (e) { /* stats are best-effort */ }
}

function showCredentials(res) {
  const c = res.credentials, box = $('#creds');
  box.replaceChildren();
  const h = document.createElement('h2');
  h.textContent = `${res.site.primary_domain} is live`;
  const p = document.createElement('p');
  p.textContent = 'Save these credentials now — they are shown only once.';
  const pre = document.createElement('pre');
  pre.textContent = `Admin URL: ${c.admin_url}\nUsername:  ${c.username}\nPassword:  ${c.password}`;
  const btn = document.createElement('button');
  btn.className = 'ghost';
  btn.textContent = 'Dismiss';
  btn.onclick = () => { box.hidden = true; };
  box.append(h, p, pre, btn);
  box.hidden = false;
}

function init() {
  $('#login-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    localStorage.setItem(TOKEN_KEY, e.target.token.value.trim());
    start();
  });
  $('#logout').addEventListener('click', signOut);
  $('#new-site-btn').addEventListener('click', () => { $('#new-site').hidden = false; });
  $('#cancel-new').addEventListener('click', () => { $('#new-site').hidden = true; });
  $('#new-site').addEventListener('submit', async (e) => {
    e.preventDefault();
    const form = e.target, btn = $('button[type=submit]', form);
    const body = Object.fromEntries(new FormData(form));
    btn.disabled = true;
    btn.textContent = 'Provisioning… (up to a minute)';
    try {
      const res = await api('POST', '/sites', body);
      form.reset();
      form.hidden = true;
      showCredentials(res);
      await load();
    } catch (err) { showError(err); }
    finally { btn.disabled = false; btn.textContent = 'Create site'; }
  });
  start();
}

async function start() {
  if (!localStorage.getItem(TOKEN_KEY)) return signOut();
  $('#login').hidden = true;
  $('#app').hidden = false;
  $('#logout').hidden = false;
  try { await load(); } catch (e) { showError(e); }
}

init();
