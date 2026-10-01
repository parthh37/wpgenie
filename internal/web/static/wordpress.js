'use strict';
// WordPress itself: wp-admin without a password, administrators' passwords,
// performance tweaks, the site analyser (Health) and the brand WordPress's
// admin shows. Shares api(), h(), table(), ask(), notify() and showError()
// with app.js, ux.js and panels.js.

const HEALTH = new Map(); // site ID -> the analysis last shown
let OPTIMIZATIONS = null; // the tweaks' catalogue (GET /optimizations), once
const canOperate = () => ME && ME.role !== 'viewer';

// openFresh opens a tab now, in the click (one opened after the request would
// be blocked as a pop-up), then sends it to the URL get() resolves to.
async function openFresh(get, fallbackTitle) {
  const win = window.open('about:blank', '_blank');
  try {
    const url = await get();
    if (win) { win.opener = null; win.location = url; } else { showSecret(fallbackTitle, [url]); }
  } catch (e) { if (win) win.close(); showError(e); }
}

// wpLogin signs in to a site's wp-admin as an administrator (0: the oldest).
function wpLogin(site, userID = 0) {
  return openFresh(async () => (await api('POST', `/sites/${site.id}/wp-admin/login`, { user_id: userID })).url,
    'wp-admin sign-in link (single use, 2 minutes)');
}

// ---- WordPress admin: sign in without a password, reset passwords ----

function renderWordPress(el, site) {
  const d = $('.wpa', el);
  if (site.status !== 'active') { d.hidden = true; return; }
  const link = $('.wp-admin', el);
  link.title = 'Sign in to wp-admin (no WordPress password needed)';
  // Viewers can't make sessions: for them it stays a plain link.
  link.addEventListener('click', (e) => {
    if (!canOperate()) return;
    e.preventDefault();
    wpLogin(site);
  });
  d.addEventListener('toggle', () => { if (d.open) showAdmins(el, site).catch(showError); });
}

async function showAdmins(el, site) {
  const body = $('.wpa-body', el);
  body.replaceChildren(h('p', { class: 'muted small' }, 'Loading…'));
  const users = await api('GET', `/sites/${site.id}/wp-admin/users`);
  $('.wpa-summary', el).textContent = `· ${users.length} administrator${users.length === 1 ? '' : 's'}`;
  body.replaceChildren(table(['Administrator', 'E-mail', ''], users.map((u) => [
    h('td', {}, h('strong', {}, u.login), u.name && u.name !== u.login ? h('div', { class: 'muted small' }, u.name) : null),
    u.email,
    h('td', {}, h('div', { class: 'actions' },
      h('button', { class: 'ghost', onclick: () => wpLogin(site, u.id) }, 'Sign in as ' + u.login),
      h('button', { class: 'ghost', onclick: () => resetWPPassword(site, u) }, 'Reset password'))),
  ])));
}

async function resetWPPassword(site, u) {
  const pw = await askText(`Choose a new password, or leave it empty for a strong random one (shown once). ` +
    `Every session of ${u.login} ends: anyone signed in as them is signed out.`,
  { title: `Reset ${u.login}'s password on ${site.primary_domain}?`, label: 'New password (12+ characters, optional)',
    type: 'password', autocomplete: 'new-password', ok: 'Reset password', danger: true });
  if (pw === null) return;
  try {
    const r = await api('POST', `/sites/${site.id}/wp-admin/password`, { user_id: u.id, password: pw });
    if (pw) notify(`Password of ${r.user} changed; their sessions ended`);
    else showSecret(`New WordPress password on ${site.primary_domain}`, [`Username: ${r.user}`, `Password: ${r.password}`]);
  } catch (e) { showError(e); }
}

// ---- Performance tweaks (the optimize mu-plugin) ----

function renderOptimize(el, site) {
  const box = $('.opt', el);
  if (site.status !== 'active') { box.hidden = true; return; }
  const list = $('.opt-list', box);
  const checked = () => [...list.querySelectorAll('input:checked')].map((i) => i.dataset.key);
  const put = async (keys, btn, msg) => {
    btn.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/optimize`, { optimizations: keys });
      notify(msg);
      HEALTH.delete(site.id);
      await load();
    } catch (e) { showError(e); btn.disabled = false; }
  };
  // The catalogue loads with the section, once for every site.
  box.closest('details').addEventListener('toggle', async (e) => {
    if (!e.target.open || list.childElementCount) return;
    try { OPTIMIZATIONS = OPTIMIZATIONS || await api('GET', '/optimizations'); } catch (err) { showError(err); return; }
    list.replaceChildren(...OPTIMIZATIONS.map((o) => h('label', { class: 'check opt-item', title: o.description },
      h('input', { type: 'checkbox', 'data-key': o.key, checked: (site.optimize || []).includes(o.key) }),
      h('span', {}, o.title, o.default ? '' : h('span', { class: 'muted' }, ' (optional)'),
        h('span', { class: 'muted small opt-desc' }, o.description)))));
  });
  const save = $('.opt-save', box), rec = $('.opt-recommended', box), clean = $('.opt-cleanup', box);
  save.addEventListener('click', () => put(checked(), save, `Tweaks saved for ${site.primary_domain}`));
  rec.addEventListener('click', () => {
    const keys = new Set([...(site.optimize || []), ...(OPTIMIZATIONS || []).filter((o) => o.default).map((o) => o.key)]);
    put([...keys], rec, `Recommended tweaks applied to ${site.primary_domain}`);
  });
  clean.addEventListener('click', async () => {
    if (!await ask(`Clean ${site.primary_domain}'s database now? Expired transients, auto-drafts older than a week, spam older ` +
      'than a month and revisions older than a month beyond the newest five per post are deleted.', { ok: 'Clean up' })) return;
    clean.disabled = true;
    try {
      const r = await api('POST', `/sites/${site.id}/optimize/cleanup`);
      notify(`Removed ${r.transients} expired transients, ${r.auto_drafts} auto-drafts, ${r.spam} spam comments, ${r.revisions} old revisions`);
      HEALTH.delete(site.id);
    } catch (e) { showError(e); }
    finally { clean.disabled = false; }
  });
}

// ---- Health: the site analyser ----

const FIX_LABELS = {
  page_cache: 'Turn on', object_cache: 'Turn on', images: 'Convert to WebP', optimize: 'Apply tweaks', db_cleanup: 'Clean up',
  scan: 'Scan now', update_security: 'Update', update_all: 'Update all', auto_update: 'Turn on', default_role: 'Make subscriber',
  search_visible: 'Allow indexing', shield: 'Turn on', waf: 'Turn on',
};
const SEV_ORDER = ['critical', 'high', 'medium', 'low'];

function renderHealth(el, site) {
  const d = $('.analysis', el);
  const cached = HEALTH.get(site.id);
  if (cached) healthSummary(el, cached);
  d.addEventListener('toggle', () => {
    if (!d.open) return;
    const a = HEALTH.get(site.id);
    if (a) showHealth(el, site, a); else analyseSite(el, site);
  });
  $('.analyse-site', el).addEventListener('click', () => analyseSite(el, site));
}

function healthSummary(el, a) {
  const todo = a.findings.filter((f) => f.severity !== 'info').length;
  $('.health-summary', el).textContent = `· ${a.grade} (${a.score}/100)` + (todo ? ` · ${todo} to look at` : '');
}

async function analyseSite(el, site) {
  const btn = $('.analyse-site', el), body = $('.health-body', el);
  btn.disabled = true;
  btn.textContent = 'Analysing…';
  if (!HEALTH.has(site.id)) body.replaceChildren(h('p', { class: 'muted small' }, 'Analysing… (a few seconds)'));
  try {
    const a = await api('GET', `/sites/${site.id}/analysis`);
    HEALTH.set(site.id, a);
    showHealth(el, site, a);
  } catch (e) {
    showError(e);
    if (!HEALTH.has(site.id)) body.replaceChildren();
  } finally {
    btn.disabled = false;
    btn.textContent = 'Analyse now';
  }
}

function fixButton(site, f) {
  const b = h('button', { class: 'ghost' }, FIX_LABELS[f.fix] || 'Fix');
  b.addEventListener('click', async () => {
    if ((f.fix === 'update_all' || f.fix === 'update_security') && !await ask(`Update ${site.primary_domain} now? ` +
      'A snapshot is taken first and restored automatically if the site stops working.', { ok: 'Update' })) return;
    b.disabled = true;
    try {
      const r = await api('POST', `/sites/${site.id}/analysis/fix`, { fix: f.fix });
      notify(r.update_run ? `${r.message}. Follow it under Updates.` : r.message);
      HEALTH.delete(site.id);
      await load(); // the open section re-analyses as it re-renders
    } catch (e) { showError(e); b.disabled = false; }
  });
  return b;
}

function showHealth(el, site, a) {
  healthSummary(el, a);
  const f = a.facts;
  const facts = f ? [`WordPress ${f.wp_version}`, `PHP ${f.php_version}`, `${f.plugins_active} active plugin${f.plugins_active === 1 ? '' : 's'}`,
    `database ${fmtBytes(f.db_bytes)}`, `autoloaded options ${fmtBytes(f.autoload_bytes)}`] : [];
  const when = `Analysed ${fmtTime(a.analysed_at)}` +
    (a.scanned_at ? ` · vulnerabilities as of the scan of ${fmtTime(a.scanned_at)}` : ' · never scanned');
  const worst = (vs) => SEV_ORDER.find((s) => vs.some((v) => v.severity === s)) || 'medium';
  fill($('.health-body', el),
    h('div', { class: 'health-score' },
      h('div', { class: `grade grade-${a.grade}`, role: 'img', 'aria-label': `Grade ${a.grade}` }, a.grade),
      h('div', {}, h('strong', {}, `${a.score} / 100`), h('div', { class: 'muted small' }, facts.join(' · ')), h('div', { class: 'muted small' }, when))),
    a.findings.length ? table(['Severity', 'Area', 'Finding', ''], a.findings.map((x) => [
      h('td', { class: 'sev-' + x.severity }, x.severity),
      x.category,
      h('td', {}, h('strong', {}, x.title), x.detail ? h('div', { class: 'muted small' }, x.detail) : null),
      h('td', {}, x.fix ? h('div', { class: 'actions' }, fixButton(site, x)) : null),
    ])) : h('p', { class: 'st-ok small' }, 'Nothing to fix.'),
    a.components.length ? h('h2', {}, 'Installed versions') : null,
    a.components.length ? table(['Component', 'Type', 'Installed', 'Available', 'Known vulnerabilities'], a.components.map((c) => [
      c.type === 'core' ? 'WordPress' : c.slug, c.type + (c.status && c.status !== 'active' ? ` (${c.status})` : ''), c.version,
      c.update_version || '–',
      c.vulns && c.vulns.length
        ? h('td', { class: 'sev-' + worst(c.vulns), title: c.vulns.map((v) => v.title).join('\n') }, `${c.vulns.length} (${worst(c.vulns)})`)
        : h('td', { class: 'st-ok' }, 'none'),
    ])) : null,
    (a.errors || []).length ? h('p', { class: 'muted small' }, 'Partial analysis: ' + a.errors.join('; ')) : null,
  );
}

// ---- Branding (System tab, administrators) ----

async function loadBranding() {
  const box = $('#branding');
  if (!isAdmin()) { box.replaceChildren(); return; }
  const b = await api('GET', '/settings/branding');
  const name = h('input', { value: b.name, maxlength: '80', placeholder: 'Acme Hosting' });
  const link = h('input', { value: b.url, type: 'url', placeholder: 'https://acme.example' });
  const file = h('input', { type: 'file', accept: 'image/png,image/jpeg,image/gif,image/webp,image/svg+xml' });
  const preview = h('div', { class: 'brand-preview' });
  let logo; // undefined: unchanged; '': remove it; a data: URI: the new one
  const showLogo = (node) => preview.replaceChildren(node);
  const current = () => (b.has_logo
    ? h('img', { src: `/api/v1/settings/branding/logo?v=${b.logo_version}`, alt: 'Current logo' })
    : h('span', { class: 'muted small' }, 'No logo: WordPress shows the name instead.'));
  showLogo(current());
  file.addEventListener('change', () => {
    const f = file.files[0];
    if (!f) return;
    if (f.size > 256 * 1024) { showError(new Error('The logo must be 256 KB or smaller')); file.value = ''; return; }
    const r = new FileReader();
    // The page's CSP keeps it from showing a local file: it's previewed once saved.
    r.onload = () => { logo = r.result; showLogo(h('span', { class: 'small' }, `New logo: ${f.name} (${fmtBytes(f.size)}), shown once saved`)); };
    r.readAsDataURL(f);
  });
  const remove = h('button', { class: 'ghost danger', type: 'button', onclick: () => { logo = ''; file.value = ''; showLogo(h('span', { class: 'muted small' }, 'The logo is removed when you save.')); } }, 'Remove logo');
  const save = h('button', {}, 'Save branding');
  save.addEventListener('click', async () => {
    save.disabled = true;
    try {
      const body = { name: name.value.trim(), url: link.value.trim() };
      if (logo !== undefined) body.logo = logo;
      const v = await api('PUT', '/settings/branding', body);
      notify(v.enabled ? 'Branding saved: every site\'s WordPress admin shows it now' : 'Branding removed: sites show WordPress\'s own again');
      await loadBranding();
    } catch (e) { showError(e); save.disabled = false; }
  });
  box.replaceChildren(
    h('h2', {}, 'Branding'),
    h('p', { class: 'muted small' }, 'Your brand instead of WordPress\'s in every site\'s admin: the logo and link on the login page, ' +
      'the logo menu in the admin bar, the admin footer and page titles. WordPress\'s news widget and welcome panel go too. ' +
      'Leave the name empty and remove the logo to show WordPress\'s own again.'),
    h('div', { class: 'grid' },
      h('label', {}, 'Brand name', name),
      h('label', {}, 'Link ', h('span', { class: 'muted' }, '(where the logo points; empty: the site itself)'), link),
      h('label', {}, 'Logo ', h('span', { class: 'muted' }, '(PNG, JPEG, WebP, GIF or SVG, up to 256 KB; wide logos look best)'), file)),
    preview,
    h('div', { class: 'actions' }, remove, save));
}

// The System tab also shows branding.
{
  const baseSystem = loaders.system;
  loaders.system = async () => { await Promise.all([baseSystem(), loadBranding()]); };
}
