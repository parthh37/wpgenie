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
  if (!rows.length) return h('p', { class: 'muted small' }, 'Nothing here yet.');
  return h('table', {}, h('tr', {}, headers.map((x) => h('th', {}, x))), rows.map((r) => h('tr', {}, r.map((c) => (c instanceof Node && c.tagName === 'TD' ? c : h('td', {}, c))))));
}

const status = (s) => h('span', { class: 'st-' + s }, s.replace(/_/g, ' '));
const splitList = (v) => v.split(',').map((x) => x.trim()).filter(Boolean);

function showError(err) {
  const box = $('#error');
  box.textContent = err ? String(err.message || err) : '';
  box.hidden = !err;
}

function signOut() {
  localStorage.removeItem(TOKEN_KEY);
  document.querySelectorAll('.panel').forEach((p) => { p.hidden = true; });
  $('#tabs').hidden = true;
  $('#app').hidden = true;
  $('#logout').hidden = true;
  $('#login').hidden = false;
}

async function load() {
  showError(null);
  const sites = await api('GET', '/sites');
  const list = $('#sites');
  // Actions re-render every card: keep the panels that were open, open.
  const open = new Set([...list.querySelectorAll('details[open]')].map((d) => d.closest('[data-id]').dataset.id + '|' + d.className));
  list.replaceChildren(...sites.map(renderSite));
  list.querySelectorAll('details').forEach((d) => {
    if (open.has(d.closest('[data-id]').dataset.id + '|' + d.className)) d.open = true; // fires 'toggle' itself
  });
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
  renderAutoscale(el, site);
  renderCDN(el, site);
  renderSecurity(el, site);
  renderUpdates(el, site);
  const log = $('.log', el);
  log.addEventListener('toggle', () => { if (log.open) loadEvents(el, site); });

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

function renderCDN(el, site) {
  const details = $('.cdn', el), box = $('.cdn-status', el), token = $('.cdn-token', el);
  const save = $('.cdn-save', el), purge = $('.cdn-purge', el), off = $('.cdn-off', el);
  const active = site.status === 'active';
  [token, save, purge, off].forEach((c) => { c.disabled = !active; });
  const show = (st) => {
    const on = st.provider === 'cloudflare';
    $('.cdn-summary', el).textContent = on ? '· Cloudflare, purging on' : '· Cloudflare (free plan)';
    purge.hidden = off.hidden = !on;
    token.placeholder = on ? 'unchanged if empty' : 'paste a Cloudflare API token';
    const proxied = { yes: 'ok', no: 'failed', partly: 'failed', unknown: 'unknown' };
    const ssl = { strict: 'ok', full: 'warning', flexible: 'failed', off: 'failed' };
    box.replaceChildren(
      table(['Domain', 'Through Cloudflare', 'SSL/TLS mode'], st.domains.map((d) => [
        d.domain,
        h('td', { title: d.addrs.join(', ') }, h('span', { class: 'st-' + proxied[d.proxied] }, d.proxied)),
        d.ssl_mode ? h('td', {}, h('span', { class: 'st-' + (ssl[d.ssl_mode] || 'unknown') }, d.ssl_mode)) : '–',
      ])),
      on ? h('p', { class: 'muted small' }, st.purged_at ? `Last purged ${fmtTime(st.purged_at)}.` : 'Not purged yet.') : null,
      st.last_error ? h('p', { class: 'st-failed small' }, 'Last purge failed: ' + st.last_error) : null,
      ...st.warnings.map((w) => h('p', { class: 'small st-warning' }, '⚠ ' + w)),
    );
  };
  const refresh = async () => {
    box.replaceChildren(h('p', { class: 'muted small' }, 'Checking DNS and Cloudflare…'));
    try { show(await api('GET', `/sites/${site.id}/cdn`)); } catch (e) { box.replaceChildren(); showError(e); }
  };
  details.addEventListener('toggle', () => { if (details.open && active) refresh(); });
  save.addEventListener('click', async () => {
    save.disabled = true;
    save.textContent = 'Checking token…';
    try {
      show(await api('PUT', `/sites/${site.id}/cdn`, { provider: 'cloudflare', api_token: token.value.trim() }));
      token.value = '';
    } catch (e) { showError(e); }
    finally { save.disabled = false; save.textContent = 'Save token'; }
  });
  purge.addEventListener('click', async () => {
    purge.disabled = true;
    try { await api('POST', `/sites/${site.id}/cdn/purge`); purge.textContent = 'Purged ✓'; }
    catch (e) { showError(e); }
    finally { setTimeout(() => { purge.disabled = false; purge.textContent = 'Purge CDN'; }, 1500); }
  });
  off.addEventListener('click', async () => {
    if (!confirm('Stop purging Cloudflare\'s cache for this site? The stored API token is deleted. Cloudflare keeps serving the site.')) return;
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
  } catch (e) { /* stats are best-effort */ }
  loadCPU(site);
}

async function loadCPU(site) {
  try {
    const m = await api('GET', `/sites/${site.id}/metrics`);
    const el = document.querySelector(`[data-id="${site.id}"]`);
    if (!el) return;
    $('[data-k="cpu"]', el).textContent = m.cpu ? `${m.cpu.percent}%` : '–';
    $('.cpu-line', el).textContent = m.cpu
      ? `CPU: ${m.cpu.percent}% of each replica's allowance across ${m.cpu.replicas} replica(s), sampled ${fmtTime(m.cpu.at)}.`
      : '';
  } catch (e) { /* best-effort */ }
}

function renderAutoscale(el, site) {
  const on = $('.autoscale', el), min = $('.as-min', el), max = $('.as-max', el), target = $('.as-target', el);
  on.checked = site.autoscale;
  min.value = site.min_replicas;
  // Suggest room to grow when turning it on; the server enforces its own limit.
  max.value = site.autoscale ? site.max_replicas : Math.max(site.replicas, site.max_replicas, 2);
  target.value = site.target_cpu;
  if (site.autoscale) {
    $('.replicas', el).disabled = true;
    $('.shape', el).textContent += ` · autoscaling ${site.min_replicas}–${site.max_replicas}`;
  }
  const save = $('.as-save', el);
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/autoscale`, {
        enabled: on.checked, min_replicas: Number(min.value), max_replicas: Number(max.value), target_cpu: Number(target.value),
      });
      await load();
    } catch (e) { showError(e); save.disabled = false; }
  });
}

function renderSecurity(el, site) {
  const waf = $('.waf', el), admin = $('.admin-allow', el), trusted = $('.trusted', el);
  waf.checked = site.waf;
  admin.value = (site.admin_allow || []).join(', ');
  trusted.value = (site.trusted_ips || []).join(', ');
  $('.sec-summary', el).textContent = `· WAF ${site.waf ? 'on' : 'off'}` + (site.admin_allow.length ? ' · admin allowlist' : '');
  const save = $('.sec-save', el);
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/shield`, {
        mode: $('.mode', el).value, block_ai_bots: $('.ai', el).checked, waf: waf.checked,
        admin_allow: splitList(admin.value), trusted_ips: splitList(trusted.value),
      });
      await load();
    } catch (e) { showError(e); save.disabled = false; }
  });
  const box = $('.scan-report', el), scan = $('.scan', el), details = $('.sec', el);
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
  box.replaceChildren(
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
  $('#tabs').hidden = false;
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t.dataset.tab === 'sites'));
  try { await load(); } catch (e) { showError(e); }
  if (typeof checkSystem === 'function') checkSystem();
}

// Live CPU readings refresh with the autoscaler's sampling.
setInterval(() => {
  if ($('#app').hidden) return;
  document.querySelectorAll('#sites [data-id]').forEach((el) => loadCPU({ id: el.dataset.id }));
}, 15000);

init();
