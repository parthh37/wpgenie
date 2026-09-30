'use strict';

const $ = (sel, el = document) => el.querySelector(sel);

// The signed-in user ({username, role, totp_enabled, ...}).
let ME = null;
// Sites by ID, as last loaded (staging links, job labels).
let SITES = new Map();
const isAdmin = () => ME && ME.role === 'admin';
const isTenant = () => ME && (ME.role === 'customer' || ME.role === 'reseller');

// The session lives in an HttpOnly cookie the page can't read. The custom
// header is what the server checks on every change: other sites can't set
// it, so they can't act with your session.
async function api(method, path, body) {
  const res = await fetch('/api/v1' + path, {
    method,
    credentials: 'same-origin',
    headers: { 'X-Requested-With': 'wpgenie', ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  if (res.status === 401 && !path.startsWith('/auth/')) { showSignIn(); throw new Error('Signed out: please sign in again'); }
  if (res.status === 204) return null;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error((data && data.error) || res.statusText);
    err.status = res.status;
    err.data = data;
    if (data && data.code === 'totp_required') openTab('account');
    throw err;
  }
  return data;
}

function fmtBytes(n) {
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  let i = 0;
  while (n >= 1024 && i < u.length - 1) { n /= 1024; i++; }
  return (i ? n.toFixed(1) : n) + ' ' + u[i];
}
const fmtNum = (n) => new Intl.NumberFormat().format(n);
const fmtTime = (t) => new Date(t).toLocaleString([], { dateStyle: 'short', timeStyle: 'short' });

// h builds DOM nodes; text is always set with textContent, never parsed.
function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function table(headers, rows) {
  if (!rows.length) return h('p', { class: 'muted small none' }, 'Nothing here yet.');
  return h('table', {}, h('tr', {}, headers.map((x) => h('th', {}, x))), rows.map((r) => h('tr', {}, r.map((c) => (c instanceof Node && c.tagName === 'TD' ? c : h('td', {}, c))))));
}

const status = (s) => h('span', { class: 'st-' + s }, s.replace(/_/g, ' '));
const splitList = (v) => v.split(',').map((x) => x.trim()).filter(Boolean);

// showError shows a failure as a toast that stays until it's dismissed;
// showError(null) clears them (switching tabs does). toast() is in ux.js.
function showError(err) {
  if (err) toast(String(err.message || err), 'error');
  else clearToasts('error');
}

// hideApp shows one of the signed-out screens.
function hideApp(screen) {
  ME = null;
  document.body.classList.remove('authed');
  document.querySelectorAll('.panel').forEach((p) => { p.hidden = true; });
  $('#tabs').hidden = true;
  $('#who').hidden = true;
  $('#login').hidden = screen !== 'login';
  $('#setup').hidden = screen !== 'setup';
}

function showSignIn() {
  hideApp('login');
  const f = $('#login-form');
  $('.code-step', f).hidden = true;
  f.code.value = '';
  f.password.value = '';
}

async function signOut() {
  try { await api('POST', '/auth/logout'); } catch (e) { /* signed out either way */ }
  showSignIn();
}

function openTab(name) {
  const tab = document.querySelector(`.tab[data-tab="${name}"]`);
  if (tab) tab.click();
}

async function load() {
  await loadNodes();
  const sites = await api('GET', '/sites');
  SITES = new Map(sites.map((x) => [x.id, x]));
  const list = $('#sites');
  // Actions re-render every card: keep the panels that were open, open.
  const open = new Set([...list.querySelectorAll('details[open]')].map((d) => d.closest('[data-id]').dataset.id + '|' + d.className));
  list.replaceChildren(...sites.map(renderSite));
  list.querySelectorAll('details').forEach((d) => {
    if (open.has(d.closest('[data-id]').dataset.id + '|' + d.className)) d.open = true; // fires 'toggle' itself
  });
  $('#empty').hidden = sites.length > 0;
  afterSitesRender();
  sites.filter((s) => s.status === 'active').forEach(loadStats);
}

function renderSite(site) {
  const el = $('#site-tpl').content.firstElementChild.cloneNode(true);
  el.dataset.id = site.id;
  const a = $('.domain', el);
  a.textContent = site.primary_domain;
  a.href = 'https://' + site.primary_domain;
  $('.id', el).textContent = site.id + ' · PHP ' + site.php_version;
  if (site.parent_id) $('.id', el).append(h('span', { class: 'badge' }, `staging of ${SITES.get(site.parent_id)?.primary_domain || site.parent_id}`));
  if (clustered()) $('.id', el).append(h('span', { class: 'badge' }, nodeName(site.node || 'local')));
  if (site.account_id && ME && ME.account_id !== site.account_id) $('.id', el).append(h('span', { class: 'badge' }, `account #${site.account_id}`));
  const pill = $('.status', el);
  pill.textContent = site.status;
  pill.classList.add(site.status);

  if (ME && ME.role === 'viewer') el.classList.add('readonly');
  const mode = $('.mode', el), ai = $('.ai', el);
  mode.value = site.shield_mode;
  ai.checked = site.block_ai_bots;
  const saveShield = async () => {
    try { await api('PUT', `/sites/${site.id}/shield`, { mode: mode.value, block_ai_bots: ai.checked }); }
    catch (e) { showError(e); }
    await load(); // the Security section shows the same settings
  };
  mode.addEventListener('change', saveShield);
  ai.addEventListener('change', saveShield);

  renderPerf(el, site);
  renderAutoscale(el, site);
  renderBurst(el, site);
  renderCDN(el, site);
  renderOffload(el, site);
  renderCluster(el, site);
  renderInsights(el, site);
  renderSecurity(el, site);
  renderProtection(el, site);
  renderPlugins(el, site);
  renderHealth(el, site);
  renderWordPress(el, site);
  renderOptimize(el, site);
  renderUpdates(el, site);
  renderEnvironments(el, site);
  renderFiles(el, site);
  const log = $('.log', el);
  log.addEventListener('toggle', () => { if (log.open) loadEvents(el, site); });

  $('.delete', el).addEventListener('click', async () => {
    const typed = await askText(`This permanently deletes ${site.primary_domain}, its files and database ` +
      '(its backups stay in their destinations and can be restored as a new site).',
    { title: `Delete ${site.primary_domain}?`, label: 'Type the domain to confirm', match: site.primary_domain, ok: 'Delete site' });
    if (typed !== site.primary_domain) return;
    try {
      await api('DELETE', `/sites/${site.id}`);
      notify(`${site.primary_domain} deleted`);
      focusSite(null);
      await load();
    } catch (e) { showError(e); }
  });
  decorateSite(el, site);
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

// renderPerf: the Advanced size controls and the caches (burst: simple.js).
function renderPerf(el, site) {
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

  const page = $('.page-cache', el), object = $('.object-cache', el), mobile = $('.cache-mobile', el);
  page.checked = site.page_cache;
  object.checked = site.object_cache;
  mobile.checked = site.cache_mobile;
  mobile.disabled = mobile.disabled || !site.page_cache;
  const saveCache = async () => {
    page.disabled = object.disabled = mobile.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/cache`, { page_cache: page.checked, object_cache: object.checked, mobile: mobile.checked });
      await load();
    } catch (e) { showError(e); await load(); }
  };
  page.addEventListener('change', saveCache);
  object.addEventListener('change', saveCache);
  mobile.addEventListener('change', saveCache);

  const images = $('.images', el), convert = $('.images-convert', el);
  images.value = (site.image_formats || []).join(',');
  convert.hidden = !images.value;
  images.addEventListener('change', async () => {
    images.disabled = true;
    try {
      const res = await api('PUT', `/sites/${site.id}/images`, { formats: images.value ? images.value.split(',') : [] });
      if (res.job_id) followJob(res.job_id, () => load());
      await load();
    } catch (e) { showError(e); await load(); }
  });
  convert.addEventListener('click', async () => {
    convert.disabled = true;
    try { await startJob('POST', `/sites/${site.id}/images/convert`); } catch (e) { showError(e); convert.disabled = false; }
  });

  const purge = $('.purge', el);
  purge.addEventListener('click', async () => {
    purge.disabled = true;
    try { await api('POST', `/sites/${site.id}/cache/purge`); purge.textContent = 'Purged ✓'; notify(`Cache purged on ${site.primary_domain}`); }
    catch (e) { showError(e); }
    finally { setTimeout(() => { purge.disabled = false; purge.textContent = 'Purge cache'; }, 1500); }
  });
}

const CDN_NAMES = { cloudflare: 'Cloudflare', bunny: 'bunny.net', generic: 'pull zone' };

function renderCDN(el, site) {
  const f = (c) => $('.' + c, el);
  const details = f('cdn'), box = f('cdn-status'), provider = f('cdn-provider'), token = f('cdn-token');
  const zone = f('cdn-zone'), host = f('cdn-host'), edge = f('cdn-edge');
  const save = f('cdn-save'), purge = f('cdn-purge'), off = f('cdn-off');
  const active = site.status === 'active';
  [provider, token, zone, host, edge, save, purge, off].forEach((c) => { c.disabled = !active; });
  f('cdn-origin').textContent = 'https://' + site.primary_domain;
  f('cdn-example').textContent = 'cdn.' + site.primary_domain;
  let current = { provider: '' };
  // Only the fields the chosen provider uses.
  const layout = () => {
    const p = provider.value, cf = p === 'cloudflare', same = p === current.provider;
    f('cdn-help-cloudflare').hidden = !cf;
    f('cdn-help-pull').hidden = cf;
    f('cdn-bunny-only').hidden = p !== 'bunny';
    f('cdn-token-label').hidden = p === 'generic';
    f('cdn-token-label').firstChild.textContent = p === 'bunny' ? 'API key' : 'API token';
    f('cdn-zone-label').hidden = p !== 'bunny';
    f('cdn-host-label').hidden = cf;
    f('cdn-edge-label').hidden = f('cdn-edge-note').hidden = !cf;
    token.placeholder = same ? 'unchanged if empty' : p === 'bunny' ? 'paste the bunny.net API key' : 'paste a Cloudflare API token';
  };
  provider.addEventListener('change', layout);
  const show = (st) => {
    current = st;
    const on = !!st.provider;
    if (on) provider.value = st.provider;
    host.value = st.asset_host || '';
    zone.value = st.pull_zone || '';
    edge.checked = !!st.edge_html;
    layout();
    f('cdn-summary').textContent = !on ? '· off' : st.provider === 'cloudflare'
      ? `· Cloudflare, purging on${st.edge_html ? ', pages at the edge' : ''}` : `· ${CDN_NAMES[st.provider]} at ${st.asset_host}`;
    purge.hidden = !on || st.provider === 'generic';
    off.hidden = !on;
    const proxied = { yes: 'ok', no: 'failed', partly: 'failed', unknown: 'unknown' };
    const ssl = { strict: 'ok', full: 'warning', flexible: 'failed', off: 'failed' };
    // fill() skips the nulls replaceChildren would print. The Cloudflare
    // table is about the whole site: not what a pull zone is.
    const pull = st.provider === 'bunny' || st.provider === 'generic';
    fill(box,
      pull ? null : table(['Domain', 'Through Cloudflare', 'SSL/TLS mode'], st.domains.map((d) => [
        d.domain,
        h('td', { title: d.addrs.join(', ') }, h('span', { class: 'st-' + proxied[d.proxied] }, d.proxied)),
        d.ssl_mode ? h('td', {}, h('span', { class: 'st-' + (ssl[d.ssl_mode] || 'unknown') }, d.ssl_mode)) : '–',
      ])),
      on && st.provider !== 'generic' ? h('p', { class: 'muted small' }, st.purged_at ? `Last purged ${fmtTime(st.purged_at)}.` : 'Not purged yet.') : null,
      st.last_error ? h('p', { class: 'st-failed small' }, 'Last purge failed: ' + st.last_error) : null,
      ...st.warnings.map((w) => h('p', { class: 'small st-warning' }, '⚠ ' + w)),
    );
  };
  const refresh = async () => {
    box.replaceChildren(h('p', { class: 'muted small' }, 'Checking DNS and the CDN…'));
    try { show(await api('GET', `/sites/${site.id}/cdn`)); } catch (e) { box.replaceChildren(); showError(e); }
  };
  details.addEventListener('toggle', () => { if (details.open && active) refresh(); });
  layout();
  save.addEventListener('click', async () => {
    save.disabled = true;
    save.textContent = 'Checking…';
    try {
      show(await api('PUT', `/sites/${site.id}/cdn`, {
        provider: provider.value, api_token: token.value.trim(), asset_host: host.value.trim(),
        pull_zone: zone.value.trim(), edge_html: provider.value === 'cloudflare' && edge.checked,
      }));
      token.value = '';
    } catch (e) { showError(e); }
    finally { save.disabled = false; save.textContent = 'Save'; }
  });
  purge.addEventListener('click', async () => {
    purge.disabled = true;
    try { await api('POST', `/sites/${site.id}/cdn/purge`); purge.textContent = 'Purged ✓'; }
    catch (e) { showError(e); }
    finally { setTimeout(() => { purge.disabled = false; purge.textContent = 'Purge CDN'; }, 1500); }
  });
  off.addEventListener('click', async () => {
    const msg = current.provider === 'cloudflare'
      ? 'Stop purging Cloudflare\'s cache for this site? The stored API token is deleted (and the edge cache rule removed). Cloudflare keeps serving the site.'
      : 'Stop using the CDN? Static files are served from this server again; stored keys are deleted.';
    if (!await ask(msg)) return;
    try { show(await api('PUT', `/sites/${site.id}/cdn`, { provider: '' })); } catch (e) { showError(e); }
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
    siteStatsLoaded(site.id, s, el);
    loadAttack(el, site);
  } catch (e) { /* stats are best-effort */ }
  loadCPU(site);
}

async function loadCPU(site) {
  try {
    const m = await api('GET', `/sites/${site.id}/metrics`);
    const el = document.querySelector(`[data-id="${site.id}"]`);
    if (!el) return;
    const c = m.cpu;
    $('[data-k="cpu"]', el).textContent = c ? `${c.percent}%` : '–';
    const parts = c ? [`CPU ${c.percent}% of each instance's allowance across ${c.replicas} instance(s)`] : [];
    if (c && c.workers_percent != null) parts.push(`PHP workers ${c.workers_percent}% busy` + (c.queued ? `, ${c.queued} request(s) waiting` : ''));
    if (c && c.p95_ms != null) parts.push(`95% of the last minute's ${c.responses} responses within ${Math.round(c.p95_ms)} ms`);
    $('.cpu-line', el).textContent = c ? `${parts.join(' · ')}; sampled ${fmtTime(c.at)}.` : '';
  } catch (e) { /* best-effort */ }
}

function renderAutoscale(el, site) {
  const on = $('.autoscale', el), min = $('.as-min', el), max = $('.as-max', el), target = $('.as-target', el);
  const workers = $('.as-workers', el), ms = $('.as-ms', el);
  on.checked = site.autoscale;
  workers.value = site.target_workers || 0;
  ms.value = site.target_response_ms || 0;
  min.value = site.min_replicas;
  // Suggest room to grow when turning it on; the server enforces its own limit.
  max.value = site.autoscale ? site.max_replicas : Math.max(site.replicas, site.max_replicas, 2);
  target.value = site.target_cpu;
  if (site.autoscale) $('.replicas', el).disabled = true; // the normal size is Min
  const save = $('.as-save', el);
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/autoscale`, {
        enabled: on.checked, min_replicas: Number(min.value), max_replicas: Number(max.value), target_cpu: Number(target.value),
        target_workers: Number(workers.value || 0), target_response_ms: Number(ms.value || 0),
      });
      notify(on.checked ? `Scaling between ${min.value} and ${max.value} instances` : 'Automatic scaling off');
      await load();
    } catch (e) { showError(e); save.disabled = false; }
  });
}

function renderSecurity(el, site) {
  const f = (c) => $('.' + c, el);
  f('waf').checked = site.waf;
  f('body-waf').value = site.body_waf || 'off';
  f('xmlrpc').checked = site.xmlrpc;
  f('admin-allow').value = (site.admin_allow || []).join(', ');
  f('trusted').value = (site.trusted_ips || []).join(', ');
  f('deny').value = (site.deny_ips || []).join(', ');
  f('reputation').value = site.reputation || 'challenge';
  f('country-mode').value = site.country_mode || 'off';
  f('countries').value = (site.countries || []).join(', ');
  f('country-action').value = site.country_action || 'block';
  f('rate').value = site.rate_rps || 0;
  f('burst').value = site.rate_burst || 0;
  f('login-rate').value = site.login_per_min || 0;
  f('difficulty').value = site.challenge_bits || 0;
  const note = f('waf-note');
  if (site.body_waf === 'detect') note.textContent = 'Log only: matches appear under Security → Recent blocks as "detect". Switch to Block once nothing legitimate shows up there.';
  const save = f('sec-save');
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/shield`, {
        mode: f('mode').value, block_ai_bots: f('ai').checked, waf: f('waf').checked, body_waf: f('body-waf').value,
        xmlrpc: f('xmlrpc').checked, admin_allow: splitList(f('admin-allow').value), trusted_ips: splitList(f('trusted').value),
        deny_ips: splitList(f('deny').value), reputation: f('reputation').value, country_mode: f('country-mode').value,
        countries: splitList(f('countries').value), country_action: f('country-action').value,
        rate_rps: Number(f('rate').value || 0), rate_burst: Number(f('burst').value || 0),
        login_per_min: Number(f('login-rate').value || 0), challenge_bits: Number(f('difficulty').value || 0),
      });
      notify(`Advanced security settings saved for ${site.primary_domain}`);
      await load();
    } catch (e) { showError(e); save.disabled = false; }
  });
  const box = f('scan-report'), scan = f('scan'), details = f('sec');
  details.addEventListener('toggle', async () => {
    if (!details.open || box.dataset.loaded) return;
    box.dataset.loaded = '1';
    try { showScan(box, await api('GET', `/sites/${site.id}/scan`)); } catch (e) { showError(e); }
  });
  scan.addEventListener('click', async () => {
    scan.disabled = true;
    scan.textContent = 'Scanning… (up to a minute)';
    try { showScan(box, await api('POST', `/sites/${site.id}/scan`)); }
    catch (e) { showError(e); }
    finally { scan.disabled = false; scan.textContent = 'Scan now'; }
  });
}

function renderPlugins(el, site) {
  const details = $('.plugins', el), box = $('.plugins-report', el), run = $('.analyse', el);
  details.addEventListener('toggle', async () => {
    if (!details.open || box.dataset.loaded) return;
    box.dataset.loaded = '1';
    try { showPlugins(el, await api('GET', `/sites/${site.id}/plugins`)); } catch (e) { showError(e); }
  });
  run.addEventListener('click', async () => {
    run.disabled = true;
    run.textContent = 'Analysing… (up to a minute)';
    try { showPlugins(el, await api('POST', `/sites/${site.id}/plugins`)); }
    catch (e) { showError(e); }
    finally { run.disabled = false; run.textContent = 'Analyse now'; }
  });
}

const DIRECTORY = { listed: 'listed', closed: 'CLOSED', not_listed: 'not listed', unknown: '?' };
const CHECKSUMS = { verified: 'verified', modified: 'MODIFIED', unavailable: "can't verify", not_checked: '–' };
const SECURITY_FLAG = /^(closed|contains code|\d+ known|\d+ file)/;

function showPlugins(el, rep) {
  const box = $('.plugins-report', el);
  if (!rep) { box.replaceChildren(h('p', { class: 'muted small' }, 'Not analysed yet.')); return; }
  const bad = rep.plugins.filter((p) => p.flags.some((f) => SECURITY_FLAG.test(f)));
  $('.plugins-summary', el).textContent = `· ${rep.plugins.length} installed` + (bad.length ? ` · ${bad.length} need attention` : '');
  const prof = rep.profile;
  const age = (t) => { const d = (Date.now() - new Date(t)) / 864e5; return d > 365 ? `${(d / 365).toFixed(1)} y ago` : `${Math.round(d)} d ago`; };
  const rows = rep.plugins.map((p) => [
    h('td', {}, h('strong', {}, p.title || p.slug), h('div', { class: 'muted small' }, p.slug)),
    p.version + (p.update_version ? ` → ${p.update_version}` : ''),
    p.status,
    h('td', { class: p.directory === 'closed' ? 'st-failed' : '' }, DIRECTORY[p.directory] || p.directory,
      p.last_updated ? h('div', { class: 'muted small' }, 'updated ' + age(p.last_updated)) : null),
    h('td', { class: p.checksums === 'modified' ? 'st-failed' : '', title: (p.modified || []).join('\n') }, CHECKSUMS[p.checksums] || p.checksums),
    p.perf ? h('td', { title: `load ${p.perf.load_ms} ms (${p.perf.load_kb} KB), hooks ${p.perf.hook_ms} ms in ${p.perf.calls} calls, ${p.perf.queries} queries` },
      `${(p.perf.load_ms + p.perf.hook_ms).toFixed(1)} ms`, h('div', { class: 'muted small' }, `${p.perf.queries} queries`)) : '–',
    h('td', {}, p.flags.map((f) => h('div', { class: SECURITY_FLAG.test(f) ? 'st-failed small' : 'small' }, f)),
      (p.signatures || []).map((sig) => h('div', { class: 'wrap small' }, sig))),
  ]);
  fill(box,
    h('p', { class: 'small' }, `Analysed ${fmtTime(rep.analysed_at)}`,
      prof ? `: front page rendered in ${prof.total_ms.toFixed(0)} ms with ${prof.queries} database queries and ${(prof.peak_memory_kb / 1024).toFixed(0)} MB of memory` : '',
      prof && prof.status >= 400 ? h('span', { class: 'st-failed' }, ` (HTTP ${prof.status}: the page is broken)`) : ''),
    table(['Plugin', 'Version', 'Status', 'wordpress.org', 'Files', 'Cost', 'Findings'], rows),
    prof && Object.keys(prof.others || {}).length ? h('p', { class: 'muted small' }, 'Also: ',
      Object.entries(prof.others).map(([k, v]) => `${k} ${(v.load_ms + v.hook_ms).toFixed(1)} ms, ${v.queries} queries`).join(' · ')) : null,
    (rep.theme_signatures || []).length ? h('p', { class: 'st-failed small' }, 'Suspicious theme files: ' + rep.theme_signatures.join(', ')) : null,
    rep.errors && rep.errors.length ? h('p', { class: 'muted small' }, 'Partial analysis: ' + rep.errors.join('; ')) : null,
  );
}

function showScan(box, rep) {
  if (!rep) { box.replaceChildren(h('p', { class: 'muted small' }, 'Not scanned yet. Sites are scanned daily.')); return; }
  const inv = rep.inventory;
  const vulnerable = [inv.core, ...inv.plugins, ...inv.themes].filter((c) => c.vulns && c.vulns.length);
  const rows = [];
  for (const c of vulnerable) {
    for (const v of c.vulns) {
      rows.push([`${c.slug} ${c.version}`, h('td', { class: 'sev-' + (v.severity || '') }, v.severity || '?'),
        v.link ? h('a', { href: v.link, target: '_blank', rel: 'noopener' }, v.title) : v.title,
        v.unfixed ? 'no fix yet' : c.update_fixes ? `update to ${c.update_version}` : v.fixed_in ? `fixed in ${v.fixed_in}` : '']);
    }
  }
  const integ = rep.integrity;
  const issues = [
    ...integ.core_modified.map((f) => ['Modified core file', f]),
    ...integ.plugins_modified.map((f) => ['Modified plugin file', f]),
    ...integ.uploads_php.map((f) => ['PHP file in uploads', f]),
  ];
  fill(box,
    h('p', { class: 'small' }, `Scanned ${fmtTime(rep.scanned_at)}: `,
      vulnerable.length ? h('span', { class: 'st-failed' }, `${vulnerable.length} vulnerable component(s)`) : h('span', { class: 'st-ok' }, 'no known vulnerabilities'),
      ', ', issues.length ? h('span', { class: 'st-failed' }, `${issues.length} integrity issue(s)`) : h('span', { class: 'st-ok' }, 'files intact')),
    vulnerable.length ? table(['Component', 'Severity', 'Vulnerability', 'Fix'], rows) : null,
    issues.length ? table(['Finding', 'File'], issues.map(([a, b]) => [a, h('td', { class: 'wrap' }, b)])) : null,
    rep.errors && rep.errors.length ? h('p', { class: 'muted small' }, 'Partial scan: ' + rep.errors.join('; ')) : null,
  );
}

function renderUpdates(el, site) {
  const policy = $('.auto-update', el), smtp = $('.smtp', el), details = $('.upd', el);
  policy.value = site.auto_update;
  smtp.checked = site.smtp;
  $('.upd-summary', el).textContent = `· automatic: ${site.auto_update}`;
  policy.addEventListener('change', async () => {
    try { await api('PUT', `/sites/${site.id}/auto-update`, { policy: policy.value }); await load(); } catch (e) { showError(e); }
  });
  smtp.addEventListener('change', async () => {
    smtp.disabled = true;
    try { await api('PUT', `/sites/${site.id}/smtp`, { enabled: smtp.checked }); await load(); }
    catch (e) { showError(e); smtp.checked = !smtp.checked; smtp.disabled = false; }
  });
  const check = $('.check-updates', el);
  check.addEventListener('click', async () => {
    check.disabled = true;
    check.textContent = 'Checking…';
    try { showInventory(el, site, await api('GET', `/sites/${site.id}/updates`)); }
    catch (e) { showError(e); }
    finally { check.disabled = false; check.textContent = 'Check for updates'; }
  });
  details.addEventListener('toggle', () => { if (details.open) loadHistory(el, site); });
}

function showInventory(el, site, inv) {
  const box = $('.upd-list', el);
  const pending = [inv.core, ...inv.plugins, ...inv.themes].filter((c) => c.update_version);
  if (!pending.length) { box.replaceChildren(h('p', { class: 'st-ok small' }, 'Everything is up to date.')); return; }
  const boxes = pending.map((c) => h('input', { type: 'checkbox', checked: true, 'data-type': c.type, 'data-slug': c.slug }));
  const run = h('button', {}, `Update ${pending.length} selected`);
  run.addEventListener('click', async () => {
    const req = { core: false, plugins: [], themes: [] };
    boxes.filter((b) => b.checked).forEach((b) => {
      if (b.dataset.type === 'core') req.core = true;
      else req[b.dataset.type + 's'].push(b.dataset.slug);
    });
    run.disabled = true;
    try {
      await api('POST', `/sites/${site.id}/updates`, req);
      box.replaceChildren(h('p', { class: 'small' }, 'Update running: snapshot, update, health check. The result appears below.'));
      pollHistory(el, site);
    } catch (e) { showError(e); run.disabled = false; }
  });
  box.replaceChildren(table(['', 'Component', 'Installed', 'Available'],
    pending.map((c, i) => [boxes[i], `${c.type === 'core' ? 'WordPress' : c.slug} (${c.type})`, c.version, c.update_version])), h('div', { class: 'actions' }, run));
}

async function loadHistory(el, site) {
  try {
    const runs = await api('GET', `/sites/${site.id}/updates/history?limit=10`);
    $('.upd-history', el).replaceChildren(h('h2', {}, 'History'), table(['Started', 'Trigger', 'Result', 'Summary'],
      runs.map(({ run }) => [fmtTime(run.started_at), run.trigger, h('td', {}, status(run.status)), run.summary])));
    return runs;
  } catch (e) { showError(e); return []; }
}

async function pollHistory(el, site) {
  for (let i = 0; i < 180; i++) {
    const runs = await loadHistory(el, site);
    if (runs.length && runs[0].run.status !== 'running') return;
    await new Promise((r) => setTimeout(r, 5000));
  }
}

async function loadEvents(el, site) {
  try {
    const ev = await api('GET', `/sites/${site.id}/events?limit=50`);
    $('.events', el).replaceChildren(...(ev.length ? ev.map((e) => h('div', {}, h('time', {}, fmtTime(e.time)), `[${e.kind}] ${e.message}`))
      : [h('p', { class: 'muted small' }, 'No activity yet.')]));
  } catch (e) { showError(e); }
}

// showCredentials shows a new site's admin credentials (a job's secret:
// only in memory on the server, for the user who created the site).
function showCredentials(site, c, jobID) {
  const box = $('#creds');
  const done = h('button', { class: 'ghost' }, 'I saved them');
  done.addEventListener('click', async () => {
    box.hidden = true;
    try { await api('DELETE', `/jobs/${jobID}/secret`); } catch (e) { /* expires anyway */ }
  });
  box.replaceChildren(h('h2', {}, `${site.primary_domain} is live`),
    h('p', {}, 'Save these credentials now — they are shown only once.'),
    h('pre', {}, `Admin URL: ${c.admin_url}\nUsername:  ${c.username}\nPassword:  ${c.password}`), done);
  box.hidden = false;
}

function init() {
  const login = $('#login-form');
  login.addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = $('button', login), err = $('#login-error');
    btn.disabled = true;
    err.hidden = true;
    try {
      const res = await api('POST', '/auth/login', { username: login.username.value.trim(), password: login.password.value, code: login.code.value.trim() });
      signedIn(res.user);
    } catch (ex) {
      if (ex.data && ex.data.need_code) {
        $('.code-step', login).hidden = false;
        login.code.focus();
      } else {
        err.textContent = ex.message;
        err.hidden = false;
      }
    } finally { btn.disabled = false; }
  });
  const setup = $('#setup-form');
  setup.addEventListener('submit', async (e) => {
    e.preventDefault();
    const err = $('#setup-error');
    err.hidden = true;
    try {
      const res = await api('POST', '/auth/setup', Object.fromEntries(new FormData(setup)));
      signedIn(res.user);
    } catch (ex) { err.textContent = ex.message; err.hidden = false; }
  });
  $('#logout').addEventListener('click', signOut);
  $('#new-site-btn').addEventListener('click', () => openNewSiteWizard().catch(showError));
  start();
}

async function start() {
  localStorage.removeItem('wpgenie_token'); // older versions kept the API token here
  if (typeof handleSSO === 'function' && handleSSO()) return; // a one-time sign-in link (accounts.js)
  let st;
  try { st = await api('GET', '/auth/state'); } catch (e) { showError(e); return; }
  if (st.setup) { hideApp('setup'); return; }
  if (!st.user) { showSignIn(); return; }
  signedIn(st.user, st.require_2fa);
}

// signedIn shows the panel for a user, or only Account while the panel
// requires two-factor authentication they haven't set up.
function signedIn(user, require2fa) {
  ME = user;
  document.body.classList.add('authed');
  $('#login').hidden = true;
  $('#setup').hidden = true;
  $('#tabs').hidden = false;
  $('#who').hidden = false;
  $('#me-tab').replaceChildren(h('span', { class: 'avatar', 'aria-hidden': 'true' }, user.username.slice(0, 1).toUpperCase()),
    h('span', { class: 'me-text' }, h('strong', {}, user.username), h('span', { class: 'muted small' }, user.role)));
  document.body.classList.toggle('is-admin', isAdmin());
  // Users of customer and reseller accounts get a reduced navigation.
  const tenant = isTenant();
  document.body.classList.toggle('is-tenant', tenant);
  document.body.classList.toggle('is-reseller', user.role === 'reseller');
  document.body.classList.toggle('is-staff', !tenant);
  if (require2fa && !user.totp_enabled) { openTab('account'); return; }
  if (typeof billingSignedIn === 'function') billingSignedIn(); // clientarea.js: tenants' Billing tab
  applyRoute();
  if (!tenant && typeof checkSystem === 'function') checkSystem();
  if (typeof pollJobs === 'function') pollJobs();
}

// Live CPU readings refresh with the autoscaler's sampling.
setInterval(() => {
  if ($('#app').hidden || !ME) return;
  document.querySelectorAll('#sites [data-id]').forEach((el) => loadCPU({ id: el.dataset.id }));
}, 15000);

// An attack starts and ends by itself: look again every minute.
setInterval(() => {
  if ($('#app').hidden || !ME) return;
  document.querySelectorAll('#sites [data-id][data-status="active"]').forEach((el) => loadAttack(el, { id: el.dataset.id }));
}, 60000);

// After every deferred script (accounts.js adds sign-on links) has run.
document.addEventListener('DOMContentLoaded', init);
