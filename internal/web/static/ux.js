'use strict';
// The dashboard's shell: notifications, dialogs, addresses (#/sites/<id>/…),
// the site workspace, traffic charts, the fleet summary and the command
// palette. Shares $, h(), api(), fmtNum(), fmtBytes(), ME, SITES, load() and
// openTab() with app.js.

const NS = 'http://www.w3.org/2000/svg';

function svgEl(tag, attrs = {}) {
  const el = document.createElementNS(NS, tag);
  for (const [k, v] of Object.entries(attrs)) el.setAttribute(k, v);
  return el;
}

// icon('shield') is <svg class="i"><use href="#i-shield"/></svg> (index.html's sprite).
function icon(name) {
  const svg = svgEl('svg', { class: 'i', 'aria-hidden': 'true' });
  svg.append(svgEl('use', { href: '#i-' + name }));
  return svg;
}

// ---- Notifications ----

const TOAST_MS = 4500;

function dismissToast(el) {
  el.classList.add('leaving');
  setTimeout(() => el.remove(), 160);
}

// toast shows a message in the corner. Errors stay until dismissed (they
// matter, and may be long); the rest go away on their own, not while the
// pointer is on them.
function toast(message, kind = 'ok') {
  const box = $('#toasts');
  for (const t of box.children) if (t.dataset.msg === message) t.remove(); // a retried action shows once
  const close = h('button', { type: 'button', class: 'icon-btn', 'aria-label': 'Dismiss' }, icon('x'));
  const el = h('div', { class: 'toast ' + kind, role: kind === 'error' ? 'alert' : null, 'data-msg': message },
    icon(kind === 'error' ? 'alert' : 'check'), h('p', {}, message), close);
  close.addEventListener('click', () => dismissToast(el));
  box.append(el);
  while (box.children.length > 4) box.firstElementChild.remove();
  if (kind !== 'error') {
    let timer = setTimeout(() => dismissToast(el), TOAST_MS);
    el.addEventListener('pointerenter', () => clearTimeout(timer));
    el.addEventListener('pointerleave', () => { timer = setTimeout(() => dismissToast(el), TOAST_MS / 2); });
  }
  return el;
}

const notify = (message) => toast(message, 'ok');

// pageTitle is a page's header: its icon on a lit tile, then the title and
// the line under it (children of the text column).
function pageTitle(iconName, ...children) {
  return h('div', { class: 'page-title' }, h('span', { class: 'page-icon', 'aria-hidden': 'true' }, icon(iconName)), h('div', {}, ...children));
}

// statusHero is a page's verdict at a glance ("All systems normal"): kind
// "ok" or "bad", a title and one line under it.
function statusHero(kind, title, sub) {
  return h('div', { class: 'status-hero ' + kind, role: 'status' },
    h('span', { class: 'status-hero-icon', 'aria-hidden': 'true' }, icon(kind === 'ok' ? 'check' : 'alert')),
    h('div', {}, h('strong', {}, title), sub ? h('span', { class: 'muted small' }, sub) : null));
}

function clearToasts(kind) {
  document.querySelectorAll(`#toasts .toast.${kind}`).forEach((t) => t.remove());
}

// ---- Dialogs: confirm() and prompt(), in the panel's style ----

const DANGER = /\b(delete|remove|terminate|revoke|forget|disable|stop|replace|suspend|turn off|push|restore)\b/i;
const VERBS = ['Delete', 'Remove', 'Move', 'Stop', 'Restore', 'Push', 'Switch', 'Update', 'Turn off', 'Forget', 'Copy',
  'Issue', 'Replace', 'Suspend', 'Revoke', 'Disable'];
let askChain = Promise.resolve();

// splitQuestion turns "Move a.com to web-2? Files are copied…" into the
// question (the dialog's title) and the explanation around it.
function splitQuestion(message) {
  const q = message.indexOf('?');
  if (q < 0) return { title: 'Are you sure?', body: message };
  const dot = message.lastIndexOf('. ', q), nl = message.lastIndexOf('\n', q);
  const start = Math.max(dot < 0 ? 0 : dot + 2, nl + 1);
  return { title: message.slice(start, q + 1), body: (message.slice(0, start) + ' ' + message.slice(q + 1)).trim() };
}

// openAsk shows the dialog and resolves to {value, checked} once it's
// confirmed, null if it's cancelled. One at a time: the rest wait their turn.
function openAsk(o) {
  const run = () => new Promise((resolve) => {
    const dlg = $('#ask'), ok = $('.ask-ok', dlg), cancel = $('button[value=cancel]', dlg);
    const field = $('.ask-field', dlg), input = $('input', field), check = $('.ask-check', dlg), box = $('input', check);
    $('#ask-title').textContent = o.title;
    $('.ask-body', dlg).replaceChildren(...o.body.split(/\n+/).filter(Boolean).map((p) => h('p', {}, p)));
    ok.textContent = o.ok;
    ok.classList.toggle('danger-solid', !!o.danger);
    field.hidden = !o.field;
    if (o.field) {
      $('.ask-label', field).textContent = o.field.label || '';
      input.type = o.field.type || 'text';
      input.value = o.field.value || '';
      input.placeholder = o.field.placeholder || '';
      input.autocomplete = o.field.autocomplete || 'off';
    }
    check.hidden = !o.check;
    box.checked = false;
    if (o.check) $('span', check).textContent = o.check;
    // Typing the name of what's deleted: the button waits for it.
    const sync = () => { ok.disabled = !!(o.field && o.field.match != null && input.value.trim() !== o.field.match); };
    input.oninput = sync;
    input.onkeydown = (e) => {
      if (e.key !== 'Enter') return;
      e.preventDefault(); // implicit submission would press Cancel, the form's first button
      if (!ok.disabled) dlg.close('ok');
    };
    dlg.returnValue = '';
    dlg.addEventListener('close', () => {
      const value = o.field && o.field.type !== 'password' ? input.value.trim() : input.value;
      input.value = ''; // passwords don't linger in the page
      resolve(dlg.returnValue === 'ok' ? { value, checked: box.checked } : null);
    }, { once: true });
    dlg.showModal();
    sync();
    // Destructive: Cancel has the focus, so Enter can't do harm by accident.
    (o.field ? input : o.danger ? cancel : ok).focus();
  });
  const p = askChain.then(run);
  askChain = p.catch(() => {});
  return p;
}

// ask(message) is an awaitable confirm(). The question in the message is
// the title; its first word the button (Delete, Move…) when it's a verb.
async function ask(message, opts = {}) {
  const { title, body } = opts.title ? { title: opts.title, body: message } : splitQuestion(message);
  const verb = VERBS.find((v) => title.startsWith(v + ' '));
  const res = await openAsk({ title, body, ok: opts.ok || verb || 'Continue', danger: opts.danger ?? DANGER.test(title) });
  return res !== null;
}

// askText(message, {title, label, value, type, match, ok, check}) is an
// awaitable prompt(): the text, or null if cancelled. With match, the button
// is only enabled once the text equals it. With check (a checkbox's label)
// it resolves to {value, checked} instead.
async function askText(message, opts = {}) {
  const res = await openAsk({
    title: opts.title || 'Confirm', body: message, ok: opts.ok || 'OK',
    danger: opts.danger ?? (opts.match != null || DANGER.test(opts.title || '')), check: opts.check,
    field: { label: opts.label, value: opts.value, type: opts.type, placeholder: opts.placeholder, autocomplete: opts.autocomplete, match: opts.match },
  });
  if (!res) return null;
  return opts.check ? res : res.value;
}

// ---- Theme ----

function syncThemeButton() {
  const light = document.documentElement.dataset.theme === 'light';
  const btn = $('#theme-toggle');
  btn.setAttribute('aria-label', light ? 'Switch to dark theme' : 'Switch to light theme');
  btn.title = btn.getAttribute('aria-label');
}

function toggleTheme() {
  const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
  document.documentElement.dataset.theme = next;
  try { localStorage.setItem('wpgenie_theme', next); } catch (e) { /* this page only */ }
  syncThemeButton();
}

// ---- Addresses: #/<tab>, #/sites/<id>, #/sites/<id>/<section> ----

let routing = false; // applying an address: don't push another

function setRoute(path, replace) {
  if (routing) return;
  const hash = '#/' + path;
  if (location.hash === hash) return;
  if (replace) history.replaceState(null, '', hash);
  else history.pushState(null, '', hash);
}

// applyRoute opens what the address names; a page this user can't open (or
// none) is Sites.
function applyRoute() {
  const [tab, id, section] = location.hash.replace(/^#\/?/, '').split('/').map((x) => decodeURIComponent(x || ''));
  const btn = tab && document.querySelector(`.tab[data-tab="${CSS.escape(tab)}"]`);
  const name = btn && btn.getClientRects().length ? tab : 'sites';
  routing = true;
  try {
    FOCUS = name === 'sites' && id ? { id, section: section || 'overview' } : null;
    openTab(name);
    applyFocus(false);
  } finally { routing = false; }
}

// goSite opens a site's workspace from anywhere (the palette).
function goSite(id, section = 'overview') {
  history.pushState(null, '', `#/sites/${encodeURIComponent(id)}` + (section === 'overview' ? '' : '/' + section));
  applyRoute();
}

// ---- The site workspace: one site full-page, its sections in a rail ----

// The site open in the workspace ({id, section}), or null for the list.
let FOCUS = null;
const SHIELD_LABELS = { off: 'Shield off', auto: 'Shield on', standard: 'Shield on', under_attack: 'Checking everyone' };

// A section is a <details class="perf"> of the site template, keyed by the
// first word of its summary: performance, insights, cdn, … activity.
const sectionLabel = (d) => d.querySelector('summary').firstChild.textContent.trim();
const sectionKey = (d) => sectionLabel(d).split(/\s/)[0].toLowerCase();

function siteSections() {
  return [['overview', 'Overview'], ...[...$('#site-tpl').content.querySelectorAll('details.perf')]
    .filter((d) => !d.classList.contains('cluster') || clustered()).map((d) => [sectionKey(d), sectionLabel(d)])];
}

function shieldChip(el, mode) {
  el.dataset.shield = mode;
  $('.shield-chip', el).textContent = SHIELD_LABELS[mode] || mode;
}

// The rail's groups, in order, and each section's group and icon, by key.
// Sections not listed here (new ones) go under Settings with a list icon.
const RAIL_GROUPS = ['', 'Speed', 'Protection', 'Data', 'Settings'];
const RAIL = {
  overview: ['', 'dashboard'], health: ['', 'pulse'], wordpress: ['', 'key'],
  performance: ['Speed', 'gauge'], cdn: ['Speed', 'cloud-plain'], insights: ['Speed', 'chart'], uploads: ['Speed', 'upload'],
  security: ['Protection', 'shield'], plugins: ['Protection', 'plug'], updates: ['Protection', 'refresh'],
  backups: ['Data', 'archive'], staging: ['Data', 'branch'], files: ['Data', 'folder'], sftp: ['Data', 'database'],
  domains: ['Settings', 'link'], php: ['Settings', 'code'], server: ['Settings', 'server'], activity: ['Settings', 'history'],
};
const railOf = (key) => RAIL[key] || ['Settings', 'list'];

// railButtons lays the sections out by group, a label before each group.
function railButtons(sections, onPick) {
  const order = (key) => RAIL_GROUPS.indexOf(railOf(key)[0]);
  const sorted = sections.map((s, i) => [s, i]).sort((a, b) => order(a[0][0]) - order(b[0][0]) || a[1] - b[1]).map(([s]) => s);
  const out = [];
  let group = '';
  for (const [key, label] of sorted) {
    const [g, ic] = railOf(key);
    if (g !== group) out.push(h('p', { class: 'rail-label', 'aria-hidden': 'true' }, g));
    group = g;
    out.push(h('button', { type: 'button', 'data-key': key, onclick: () => onPick(key) }, icon(ic), label));
  }
  return out;
}

// A site's monogram hue: the same site, the same colour, every time.
const siteHue = (id) => [...id].reduce((n, c) => n + c.charCodeAt(0), 0) % 6;

// decorateSite adds the workspace's parts to a freshly rendered site card
// (renderSite calls it last, so every section's visibility is decided).
function decorateSite(el, site) {
  el.dataset.domain = site.primary_domain.toLowerCase();
  el.dataset.status = site.status;
  const avatar = $('.site-avatar', el);
  $('.site-initial', avatar).textContent = site.primary_domain.replace(/^www\./, '').slice(0, 1);
  avatar.classList.add('hue-' + siteHue(site.id));
  shieldChip(el, site.shield_mode);
  const mode = $('.mode', el);
  mode.addEventListener('change', () => shieldChip(el, mode.value));
  const url = 'https://' + site.primary_domain;
  $('.visit', el).href = url;
  $('.wp-admin', el).href = url + '/wp-admin/';

  const sections = [['overview', 'Overview']];
  for (const d of el.querySelectorAll('details.perf')) {
    d.dataset.key = sectionKey(d);
    if (!d.hidden) sections.push([d.dataset.key, sectionLabel(d)]);
    // In the workspace a summary is the section's title, not a toggle.
    $('summary', d).addEventListener('click', (e) => { if (el.classList.contains('is-open')) e.preventDefault(); });
  }
  $('.site-rail', el).replaceChildren(...railButtons(sections, (key) => focusSite(site.id, key)));

  $('.open', el).addEventListener('click', () => focusSite(site.id));
  $('.open', el).setAttribute('aria-label', `Manage ${site.primary_domain}`);
  $('.back', el).addEventListener('click', () => focusSite(null));
  // The whole row opens the site, except the controls in it.
  el.addEventListener('click', (e) => {
    if (!el.classList.contains('is-open') && !e.target.closest('a, button, input, select, label, summary, details')) focusSite(site.id);
  });
  if (STATS.has(site.id)) drawSpark(el, STATS.get(site.id));
}

function showSection(card, key) {
  if (key !== 'overview' && !card.querySelector(`details.perf[data-key="${CSS.escape(key)}"]:not([hidden])`)) key = 'overview';
  card.dataset.section = key;
  const rail = $('.site-rail', card);
  for (const b of rail.children) {
    if (b.dataset.key !== key) { b.removeAttribute('aria-current'); continue; }
    b.setAttribute('aria-current', 'page');
    // On narrow screens the rail scrolls sideways: keep the current one in it
    // (scrollIntoView could scroll the page as well).
    if (rail.scrollWidth > rail.clientWidth) rail.scrollLeft = b.offsetLeft - (rail.clientWidth - b.offsetWidth) / 2;
  }
  // Opening a section fires its 'toggle': it loads as it always has.
  for (const d of card.querySelectorAll('details.perf')) {
    const want = d.dataset.key === key;
    if (d.open !== want) d.open = want;
  }
  return key;
}

// applyFocus shows the list or the open site. validate: the list was just
// rendered, so a site that isn't in it (deleted, or not yours) closes.
function applyFocus(validate) {
  const cards = [...document.querySelectorAll('#sites > .site')];
  if (FOCUS && validate && !cards.some((c) => c.dataset.id === FOCUS.id)) {
    FOCUS = null;
    setRoute('sites', true);
  }
  document.body.classList.toggle('site-focus', !!FOCUS);
  for (const c of cards) {
    const on = !!FOCUS && c.dataset.id === FOCUS.id;
    c.classList.toggle('is-open', on);
    if (on) FOCUS.section = showSection(c, FOCUS.section);
    else c.querySelectorAll('details[open]').forEach((d) => { d.open = false; });
  }
}

// focusSite opens a site (a section of it) in the workspace; null goes
// back to the list.
function focusSite(id, section = 'overview') {
  const was = FOCUS && FOCUS.id;
  FOCUS = id ? { id, section } : null;
  applyFocus(false);
  const path = id ? `sites/${encodeURIComponent(id)}` + (FOCUS.section === 'overview' ? '' : '/' + FOCUS.section) : 'sites';
  // Opening or leaving a site is a step back can undo; changing section isn't.
  setRoute(path, !!id && was === id);
  if (was === (id || null)) return;
  const card = document.querySelector(`#sites > .site[data-id="${CSS.escape(id || was || '')}"]`);
  if (id) {
    window.scrollTo(0, 0);
    if (card) $('.back', card).focus({ preventScroll: true });
  } else if (card) {
    // Back in the list where you left it.
    card.scrollIntoView({ block: 'center' });
    $('.open', card).focus({ preventScroll: true });
  }
}

// afterSitesRender runs at the end of every load() of the list.
function afterSitesRender() {
  applyFocus(true);
  applyFilter();
  updateFleet();
  renderAttention();
}

// ---- Needs attention: what to look at first, above the list ----

// attentionFor says why a site needs a look and which section to open, or
// null. "bad": broken or under fire now; "warn": a choice worth a second look.
// el is the site's card: an automatically detected attack is on it (data-attack).
function attentionFor(site, el) {
  if (site.status === 'failed') return { level: 'bad', text: 'Setting it up failed. Activity says what went wrong.', section: 'activity' };
  if (site.status === 'suspended') return { level: 'bad', text: 'Suspended: visitors can’t reach it.', section: 'overview' };
  if (el && el.dataset.attack === '1') return { level: 'bad', text: 'Under attack. Every visitor is being checked and the site stays online.', section: 'security' };
  if (site.status !== 'active') return null; // still being set up: its row says so
  if (site.shield_mode === 'off') return { level: 'warn', text: 'Protection is off: nothing is checked or blocked.', section: 'security' };
  if (site.shield_mode === 'under_attack') return { level: 'warn', text: 'Every visitor is checked until you switch it back.', section: 'security' };
  return null;
}

// renderAttention lists the sites attentionFor picks, the worst first. It
// runs after every load and whenever an attack starts or ends (loadAttack).
function renderAttention() {
  const box = $('#attention');
  const items = [];
  for (const s of SITES.values()) {
    const a = attentionFor(s, document.querySelector(`#sites > .site[data-id="${CSS.escape(s.id)}"]`));
    if (a) items.push({ site: s, ...a });
  }
  items.sort((a, b) => (a.level === b.level ? 0 : a.level === 'bad' ? -1 : 1));
  box.hidden = $('#list-head').hidden = !items.length;
  box.classList.toggle('bad', items.some((x) => x.level === 'bad'));
  $('#attention-list').replaceChildren(...items.map(({ site, level, text, section }) => {
    const go = h('button', { type: 'button', class: 'ghost', 'aria-label': `Review ${site.primary_domain}` }, 'Review', icon('chevron'));
    go.addEventListener('click', () => focusSite(site.id, section));
    return h('div', { class: 'attn ' + level },
      h('span', { class: 'attn-dot', 'aria-hidden': 'true' }),
      h('div', { class: 'attn-text' }, h('strong', {}, site.primary_domain), h('span', {}, text)), go);
  }));
}

function applyFilter() {
  const q = $('#site-filter').value.trim().toLowerCase();
  let shown = 0;
  for (const c of document.querySelectorAll('#sites > .site')) {
    c.hidden = !!q && !c.dataset.domain.includes(q) && !(FOCUS && FOCUS.id === c.dataset.id);
    if (!c.hidden) shown++;
  }
  $('#no-match').hidden = !q || shown > 0;
}

// ---- Traffic: sparklines per site and for the whole fleet ----

const STATS = new Map(); // site ID -> its last 24 hours (GET /sites/{id}/stats)

// hourly lays a sparse hourly series out as n values from start, oldest first.
function hourly(series, start, n, key) {
  const out = new Array(n).fill(0);
  for (const p of series) {
    const i = Math.round((new Date(p.hour).getTime() - start) / 36e5);
    if (i >= 0 && i < n) out[i] += p[key] || 0;
  }
  return out;
}

const hoursSince = (start) => Math.min(24 * 90, Math.max(2, Math.ceil((Date.now() - start) / 36e5)));
const sum = (xs) => xs.reduce((a, b) => a + b, 0);

// chart draws lines (one scale for all, so they compare) stretched to the
// box's width by CSS.
function chart(lines, { width = 300, height = 60, label }) {
  const max = Math.max(1, ...lines.flatMap((l) => l.values));
  const svg = svgEl('svg', { viewBox: `0 0 ${width} ${height}`, preserveAspectRatio: 'none', role: 'img', 'aria-label': label, class: 'chart' });
  for (const { values, cls, area } of lines) {
    const step = width / Math.max(1, values.length - 1);
    const d = values.map((v, i) => `${i ? 'L' : 'M'}${(i * step).toFixed(1)} ${(height - 2 - (v / max) * (height - 6)).toFixed(1)}`).join('');
    if (area) svg.append(svgEl('path', { d: `${d}L${width} ${height}L0 ${height}Z`, class: `area ${cls}` }));
    svg.append(svgEl('path', { d, class: `line ${cls}`, 'vector-effect': 'non-scaling-stroke' }));
  }
  return svg;
}

// drawSpark fills a site's chart: page views, and blocked requests on the
// same scale (a site under attack shows it), with a readout under the pointer.
function drawSpark(el, s) {
  const box = $('.spark', el);
  const start = new Date(s.since).getTime(), n = hoursSince(start);
  const views = hourly(s.series, start, n, 'page_views'), blocked = hourly(s.series, start, n, 'blocked');
  const W = 300, H = 60;
  const svg = chart([{ values: views, cls: 'views', area: true }, { values: blocked, cls: 'blocked' }], {
    width: W, height: H,
    label: `Last 24 hours: ${fmtNum(sum(views))} page views (at most ${fmtNum(Math.max(...views))} an hour), ${fmtNum(sum(blocked))} requests blocked`,
  });
  const cursor = svgEl('line', { class: 'cursor', x1: 0, x2: 0, y1: 0, y2: H, 'vector-effect': 'non-scaling-stroke' });
  svg.append(cursor);
  const readout = h('div', { class: 'spark-readout', hidden: true });
  svg.addEventListener('pointermove', (e) => {
    const r = svg.getBoundingClientRect();
    const i = Math.max(0, Math.min(n - 1, Math.round(((e.clientX - r.left) / r.width) * (n - 1))));
    const x = (i * W) / (n - 1);
    cursor.setAttribute('x1', x);
    cursor.setAttribute('x2', x);
    cursor.classList.add('on');
    const at = new Date(start + i * 36e5).toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' });
    readout.textContent = `${at} · ${fmtNum(views[i])} views · ${fmtNum(blocked[i])} blocked`;
    readout.hidden = false;
    // CSSOM, not a style attribute: the panel's CSP allows this.
    readout.style.left = `${Math.max(0, Math.min(r.width - readout.offsetWidth, e.clientX - r.left - readout.offsetWidth / 2))}px`;
  });
  svg.addEventListener('pointerleave', () => { cursor.classList.remove('on'); readout.hidden = true; });
  box.replaceChildren(
    h('div', { class: 'spark-legend' }, h('span', { class: 'lg views' }, 'Page views'), h('span', { class: 'lg blocked' }, 'Blocked'), h('span', { class: 'muted' }, 'last 24 h')),
    svg, readout);
}

function siteStatsLoaded(id, s, el) {
  STATS.set(id, s);
  drawSpark(el, s);
  updateFleet();
}

// updateFleet sums every site's last 24 hours into the tiles above the list.
function updateFleet() {
  const sites = [...SITES.values()];
  const fleet = $('#fleet');
  fleet.hidden = sites.length === 0;
  const set = (k, v) => { $(`[data-f="${k}"]`, fleet).textContent = v; };
  const live = sites.filter((s) => s.status === 'active').length;
  const busy = sites.length - live;
  set('live', fmtNum(live));
  set('live-sub', `of ${sites.length}` + (busy ? ` · ${busy} not live` : ''));
  // A dot per site (the first 60): green live, amber on its way, red broken.
  const dot = (s) => (s.status === 'active' ? '' : s.status === 'failed' || s.status === 'suspended' ? 'hs-bad' : 'hs-busy') + (s.parent_id ? ' hs-staging' : '');
  $('[data-f="strip"]', fleet).replaceChildren(...sites.slice(0, 60).map((s) => h('span', { class: dot(s), title: `${s.primary_domain}: ${s.status}` })));
  $('#fleet-sub').textContent = `${sites.length} site${sites.length === 1 ? '' : 's'}` +
    (sites.some((s) => s.parent_id) ? `, ${sites.filter((s) => s.parent_id).length} staging` : '');

  const stats = sites.map((s) => STATS.get(s.id)).filter(Boolean);
  const total = (k) => sum(stats.map((s) => s.totals[k] || 0));
  set('views', stats.length ? fmtNum(total('page_views')) : '–');
  set('visitors', stats.length ? fmtNum(sum(stats.map((s) => s.unique_visitors))) : '–');
  set('bandwidth', stats.length ? `${fmtBytes(total('bytes_out'))} served` : '');
  set('blocked', stats.length ? fmtNum(total('blocked')) : '–');
  const attacked = sites.filter((s) => s.shield_mode === 'under_attack').length;
  const off = sites.filter((s) => s.shield_mode === 'off').length;
  set('attack', attacked ? `${attacked} in Under attack mode` : off ? `${off} with the shield off` : 'Every shield on');
  fleet.classList.toggle('attack', attacked > 0);

  const box = $('[data-f="chart"]', fleet);
  if (!stats.length) { box.replaceChildren(); return; }
  const start = Math.min(...stats.map((s) => new Date(s.since).getTime())), n = hoursSince(start);
  const views = new Array(n).fill(0);
  for (const s of stats) hourly(s.series, start, n, 'page_views').forEach((v, i) => { views[i] += v; });
  box.replaceChildren(chart([{ values: views, cls: 'views', area: true }], { width: 200, height: 40, label: 'Page views on every site, last 24 hours' }));
}

// ---- Command palette (⌘K / Ctrl+K, or /) ----

const PAL = { items: [], shown: [], sel: 0 };

// The text of a nav button, without its badges' screen-reader text.
const ownText = (el) => [...el.childNodes].filter((n) => n.nodeType === Node.TEXT_NODE).map((n) => n.textContent).join('').trim();

function paletteItems() {
  const items = [];
  const shown = (el) => el && el.getClientRects().length > 0;
  const canChange = ME && ME.role !== 'viewer';
  const sections = siteSections();
  for (const s of SITES.values()) {
    items.push({ kind: 'Site', label: s.primary_domain, icon: 'globe', hint: s.status === 'active' ? '' : s.status, run: () => goSite(s.id) });
    // Found by typing: a domain and a section ("north back"), or an action.
    for (const [key, label] of sections.slice(1)) {
      items.push({ kind: 'Section', label: `${s.primary_domain} › ${label}`, icon: 'chevron', deep: true, run: () => goSite(s.id, key) });
    }
    if (!canChange || s.status !== 'active') continue;
    const attack = s.shield_mode === 'under_attack';
    items.push({
      kind: 'Action', icon: 'shield', deep: true,
      label: `${attack ? 'Turn off' : 'Turn on'} Under attack mode · ${s.primary_domain}`,
      run: async () => {
        await api('PUT', `/sites/${s.id}/shield`, { mode: attack ? 'standard' : 'under_attack', block_ai_bots: s.block_ai_bots });
        notify(attack ? `${s.primary_domain}: back to the standard shield` : `${s.primary_domain}: every visitor is challenged now`);
        if (!$('#app').hidden) await load();
      },
    });
    if (s.page_cache) {
      items.push({
        kind: 'Action', icon: 'zap', deep: true, label: `Purge cache · ${s.primary_domain}`,
        run: async () => { await api('POST', `/sites/${s.id}/cache/purge`); notify(`Cache purged on ${s.primary_domain}`); },
      });
    }
  }
  for (const t of document.querySelectorAll('#tabs .tab')) {
    if (!shown(t)) continue;
    items.push({ kind: 'Page', label: ownText(t), icon: t.querySelector('use').getAttribute('href').slice(3), run: () => openTab(t.dataset.tab) });
  }
  items.push({ kind: 'Page', label: 'Your account', icon: 'users', run: () => openTab('account') });
  if (shown($('#new-site-btn'))) {
    items.push({ kind: 'Action', label: 'Create a new site', icon: 'plus', run: () => { openTab('sites'); $('#new-site-btn').click(); } });
  }
  const light = document.documentElement.dataset.theme === 'light';
  items.push({ kind: 'Action', label: `Switch to ${light ? 'dark' : 'light'} theme`, icon: light ? 'moon' : 'sun', run: toggleTheme });
  items.push({ kind: 'Action', label: 'Sign out', icon: 'logout', run: signOut });
  return items;
}

// matchScore ranks an item for a query: every word of the query must be in
// the label, as text (better at the start of a word) or as letters in order
// ("nwc" finds northwind-coffee). -1: no match.
function matchScore(label, query) {
  const hay = label.toLowerCase();
  let score = 0;
  for (const word of query.toLowerCase().split(/\s+/).filter(Boolean)) {
    const i = hay.indexOf(word);
    if (i >= 0) {
      score += 100 - Math.min(i, 50) + (i === 0 || /[\s.›·-]/.test(hay[i - 1]) ? 50 : 0);
      continue;
    }
    let j = -1;
    for (const ch of word) {
      j = hay.indexOf(ch, j + 1);
      if (j < 0) return -1;
    }
    score += 10;
  }
  return score;
}

function renderPalette(query) {
  const q = query.trim();
  PAL.shown = q
    ? PAL.items.map((it) => ({ it, s: matchScore(it.label, q) - (it.deep ? 5 : 0) })).filter((x) => x.s >= 0)
      .sort((a, b) => b.s - a.s).slice(0, 40).map((x) => x.it)
    : PAL.items.filter((it) => !it.deep);
  PAL.sel = 0;
  const list = $('#palette-list');
  list.replaceChildren(...PAL.shown.map((it, i) => {
    const li = h('li', { role: 'option', id: 'pal-' + i, 'aria-selected': String(i === 0) },
      icon(it.icon), h('span', { class: 'pal-label' }, it.label),
      it.hint ? h('span', { class: 'pill small' }, it.hint) : null, h('span', { class: 'pal-kind' }, it.kind));
    li.addEventListener('pointermove', () => selectPalette(i));
    li.addEventListener('click', () => runPalette(i));
    return li;
  }));
  if (!PAL.shown.length) list.append(h('li', { class: 'pal-empty muted' }, `Nothing matches “${q}”.`));
  $('#palette input').setAttribute('aria-activedescendant', PAL.shown.length ? 'pal-0' : '');
}

function selectPalette(i) {
  if (!PAL.shown.length) return;
  PAL.sel = (i + PAL.shown.length) % PAL.shown.length;
  document.querySelectorAll('#palette-list [role=option]').forEach((li, j) => li.setAttribute('aria-selected', String(j === PAL.sel)));
  const cur = $('#pal-' + PAL.sel);
  cur.scrollIntoView({ block: 'nearest' });
  $('#palette input').setAttribute('aria-activedescendant', cur.id);
}

async function runPalette(i) {
  const it = PAL.shown[i];
  if (!it) return;
  $('#palette').close();
  try { await it.run(); } catch (e) { showError(e); }
}

function openPalette() {
  if (!ME || $('#palette').open || $('#ask').open) return;
  closeNav();
  PAL.items = paletteItems();
  const input = $('#palette input');
  input.value = '';
  renderPalette('');
  $('#palette').showModal();
  input.focus();
}

// ---- Navigation drawer (narrow screens) ----

function closeNav() {
  document.body.classList.remove('nav-open');
  $('#scrim').hidden = true;
  $('#nav-open').setAttribute('aria-expanded', 'false');
}

function openNav() {
  document.body.classList.add('nav-open');
  $('#scrim').hidden = false;
  $('#nav-open').setAttribute('aria-expanded', 'true');
  $('#tabs .tab.active')?.focus();
}

// ---- Wiring ----

document.addEventListener('DOMContentLoaded', () => {
  syncThemeButton();
  matchMedia('(prefers-color-scheme: light)').addEventListener('change', syncThemeButton);
  $('#theme-toggle').addEventListener('click', toggleTheme);
  $('#nav-open').addEventListener('click', openNav);
  $('#scrim').addEventListener('click', closeNav);
  $('#palette-open').addEventListener('click', openPalette);
  $('#palette-open-m').addEventListener('click', openPalette);
  $('#site-filter').addEventListener('input', applyFilter);
  $('#empty-new').addEventListener('click', () => $('#new-site-btn').click());
  const mac = /Mac|iPhone|iPad/.test(navigator.platform);
  document.querySelectorAll('.kbd-mod').forEach((k) => { k.textContent = mac ? '⌘K' : 'Ctrl K'; });

  // Every page change: its own address, the top of the page, drawer closed.
  document.querySelectorAll('.tab').forEach((tab) => tab.addEventListener('click', () => {
    if (!routing) {
      FOCUS = null;
      setRoute(tab.dataset.tab);
    }
    closeNav();
    window.scrollTo(0, 0);
  }));
  window.addEventListener('popstate', () => { if (ME) applyRoute(); });

  const input = $('#palette input');
  input.addEventListener('input', () => renderPalette(input.value));
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); selectPalette(PAL.sel + 1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); selectPalette(PAL.sel - 1); }
    else if (e.key === 'Enter') { e.preventDefault(); runPalette(PAL.sel); }
  });
  // A click on the backdrop (the dialog itself, outside its content) closes it.
  $('#palette').addEventListener('click', (e) => { if (e.target === e.currentTarget) e.currentTarget.close(); });

  document.addEventListener('keydown', (e) => {
    const typing = e.target.closest('input, textarea, select, [contenteditable]');
    if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === 'k') { e.preventDefault(); openPalette(); return; }
    if (typing || e.metaKey || e.ctrlKey || e.altKey || document.querySelector('dialog[open]')) return;
    if (e.key === '/') { e.preventDefault(); openPalette(); }
    else if (e.key === 'Escape' && document.body.classList.contains('nav-open')) closeNav();
    else if (e.key === 'Escape' && FOCUS) focusSite(null);
  });
});
