'use strict';
// Jobs, backups, staging, domains & SSL, PHP, SFTP & Adminer. Shares api(),
// h(), table(), status(), showError() and showSecret() with app.js and
// panels.js.

// ---- Jobs: long operations run in the background with progress ----

const JOB_NAMES = {
  create: 'Creating site', backup: 'Backup', restore: 'Restore', 'restore-new': 'Restore as a new site',
  staging: 'Creating staging site', push: 'Push to live', php: 'PHP change', 'primary-domain': 'Primary domain change',
  'repo-upkeep': 'Backup upkeep', images: 'Image conversion',
};
const followed = new Map(); // job ID -> callback when it ends
const finished = []; // jobs that ended while followed, newest first
let jobTimer = null;
let lastActive = [];

function siteLabel(id) {
  const s = (typeof SITES !== 'undefined' && SITES.get(id)) || null;
  return s ? s.primary_domain : id;
}

// followJob shows a job in the tray and calls onDone({job, secret}) when it ends.
function followJob(id, onDone) {
  followed.set(id, onDone);
  pollJobs();
}

async function pollJobs() {
  clearTimeout(jobTimer);
  if (!ME) return;
  try { lastActive = await api('GET', '/jobs?active=1&limit=20'); } catch (e) { jobTimer = setTimeout(pollJobs, 10000); return; }
  for (const [id, onDone] of followed) {
    if (lastActive.some((j) => j.id === id)) continue;
    followed.delete(id);
    try {
      const v = await api('GET', `/jobs/${id}`);
      finished.unshift(v.job);
      finished.splice(5);
      if (onDone) await onDone(v);
    } catch (e) { showError(e); }
  }
  renderJobs();
  jobTimer = setTimeout(pollJobs, lastActive.length || followed.size ? 2000 : 30000);
}

function renderJobs() {
  const rows = [...lastActive.map((j) => jobRow(j, false)), ...finished.map((j) => jobRow(j, true))];
  const box = $('#jobs');
  box.hidden = rows.length === 0;
  box.replaceChildren(...rows);
}

function jobRow(j, done) {
  const detail = j.status === 'failed' ? j.error : j.status === 'succeeded' ? 'Done' :
    j.status === 'queued' ? 'Waiting for another operation on this site…' : j.step || 'Starting…';
  const dismiss = h('button', { class: 'ghost' }, 'Dismiss');
  dismiss.addEventListener('click', () => { finished.splice(finished.indexOf(j), 1); renderJobs(); });
  return h('div', { class: 'job' },
    h('div', {}, h('strong', {}, JOB_NAMES[j.kind] || j.kind), j.site_id ? ` · ${siteLabel(j.site_id)}` : '',
      h('div', { class: j.status === 'failed' ? 'st-failed small' : 'muted small' }, detail)),
    h('progress', { max: 100, value: Math.max(0, j.progress) }),
    done ? dismiss : status(j.status));
}

// startJob runs a request that answers 202 {job_id} and refreshes the
// sites when the job ends (showing its error if it failed).
async function startJob(method, path, body, onDone) {
  const res = await api(method, path, body);
  followJob(res.job_id, async (v) => {
    if (v.job.status === 'failed') showError(new Error(`${JOB_NAMES[v.job.kind] || v.job.kind} failed: ${v.job.error}`));
    if (onDone) await onDone(v, res);
    await load();
  });
  return res;
}

// fill replaces an element's children, skipping null and false (which
// replaceChildren would print).
function fill(el, ...nodes) { el.replaceChildren(...nodes.flat().filter((n) => n != null && n !== false)); }

// ---- Per-site sections ----

function renderEnvironments(el, site) {
  const lazy = (cls, fn) => {
    const d = $('.' + cls, el);
    d.addEventListener('toggle', () => { if (d.open) fn().catch(showError); });
  };
  const active = site.status === 'active';
  // Operators may delete staging sites (live ones need an admin).
  if (site.parent_id) $('.delete', el).classList.remove('admin-only', 'can-create');
  $('.php-summary', el).textContent = `· ${site.php_version}`;
  const aliases = site.domains.length - 1 + site.redirect_domains.length;
  $('.dom-summary', el).textContent = aliases ? `· ${aliases} more domain(s)` : '';
  const staging = [...SITES.values()].find((s) => s.parent_id === site.id);
  $('.env-summary', el).textContent = site.parent_id ? `· copy of ${siteLabel(site.parent_id)}` : staging ? `· ${staging.primary_domain}` : '';
  if (!active) {
    el.querySelectorAll('.bk, .env, .dom, .php, .acc').forEach((d) => { d.hidden = true; });
    return;
  }
  lazy('bk', () => showBackups(el, site));
  lazy('env', async () => showStaging(el, site));
  lazy('dom', () => showDomains(el, site));
  lazy('php', () => showPHP(el, site));
  lazy('acc', () => showAccess(el, site));
}

// -- Backups --

const INTERVALS = [[0, 'Manual only'], [1, 'Every hour'], [2, 'Every 2 hours'], [4, 'Every 4 hours'], [6, 'Every 6 hours'],
  [12, 'Every 12 hours'], [24, 'Daily'], [48, 'Every 2 days'], [168, 'Weekly']];

async function showBackups(el, site) {
  const body = $('.bk-body', el);
  fill(body, h('p', { class: 'muted small' }, 'Loading backups…'));
  const [info, repos] = await Promise.all([api('GET', `/sites/${site.id}/backups`), api('GET', `/sites/${site.id}/backups/destinations`)]);
  const p = info.policy || { repo_id: '', interval_hours: 24, keep_last: 0, keep_daily: 7, keep_weekly: 4, keep_monthly: 6 };
  const repoName = (id) => (repos.find((r) => r.id === id) || { name: id }).name;
  $('.bk-summary', el).textContent = info.policy
    ? `· ${INTERVALS.find(([v]) => v === p.interval_hours)?.[1] || p.interval_hours + 'h'} to ${repoName(p.repo_id)}` +
      (p.last_error ? ' · last backup FAILED' : '')
    : '· not scheduled';

  const repo = h('select', {}, h('option', { value: '' }, 'No scheduled backups'),
    repos.map((r) => h('option', { value: r.id, selected: r.id === (info.policy ? p.repo_id : '') }, r.name)));
  const every = h('select', {}, INTERVALS.map(([v, l]) => h('option', { value: v, selected: v === p.interval_hours }, l)));
  const keep = (v) => h('input', { type: 'number', min: 0, max: 1000, value: v });
  const kLast = keep(p.keep_last), kDaily = keep(p.keep_daily), kWeekly = keep(p.keep_weekly), kMonthly = keep(p.keep_monthly);
  const save = h('button', { class: 'ghost' }, 'Save schedule');
  save.addEventListener('click', async () => {
    try {
      await api('PUT', `/sites/${site.id}/backups/policy`, {
        repo_id: repo.value, interval_hours: Number(every.value), keep_last: Number(kLast.value), keep_daily: Number(kDaily.value),
        keep_weekly: Number(kWeekly.value), keep_monthly: Number(kMonthly.value),
      });
      await showBackups(el, site);
    } catch (e) { showError(e); }
  });
  const now = h('button', {}, 'Back up now');
  now.addEventListener('click', async () => {
    now.disabled = true;
    try { await startJob('POST', `/sites/${site.id}/backups`, null, () => showBackups(el, site)); } catch (e) { showError(e); now.disabled = false; }
  });

  const rows = info.backups.map((b) => {
    const what = h('select', {}, h('option', { value: 'both' }, 'Files + database'), h('option', { value: 'files' }, 'Files only'),
      h('option', { value: 'db' }, 'Database only'));
    const restore = h('button', { class: 'ghost' }, 'Restore');
    restore.addEventListener('click', async () => {
      const label = what.options[what.selectedIndex].textContent.toLowerCase();
      if (!await ask(`Restore ${label} of ${site.primary_domain} to ${fmtTime(b.time)}?\n\n` +
        'The site as it is now is backed up first (kept 7 days), so this can be undone.')) return;
      restore.disabled = true;
      try {
        await startJob('POST', `/sites/${site.id}/backups/restore`,
          { repo_id: b.repo_id, backup_id: b.id, files: what.value !== 'db', database: what.value !== 'files' }, () => showBackups(el, site));
      } catch (e) { showError(e); restore.disabled = false; }
    });
    const dl = h('a', { href: `/api/v1/sites/${site.id}/backups/${encodeURIComponent(b.repo_id)}/${b.id}/download`, class: 'small' }, 'Download');
    const del = h('button', { class: 'ghost danger admin-only' }, 'Delete');
    del.addEventListener('click', async () => {
      if (!await ask(`Delete the backup of ${fmtTime(b.time)}? This can't be undone.`)) return;
      try { await api('DELETE', `/sites/${site.id}/backups/${encodeURIComponent(b.repo_id)}/${b.id}`); await showBackups(el, site); } catch (e) { showError(e); }
    });
    return [fmtTime(b.time), h('td', {}, status(b.kind || 'manual')), repoName(b.repo_id), fmtBytes(b.size),
      h('td', { title: 'New data this backup stored (compressed, deduplicated)' }, fmtBytes(b.added)),
      h('td', {}, h('div', { class: 'actions' }, what, restore, dl, del))];
  });
  const errs = Object.entries(info.errors || {}).map(([r, e]) => h('p', { class: 'st-failed small' }, `${repoName(r)} unreadable: ${e}`));
  fill(body,
    h('div', { class: 'controls' }, h('label', {}, 'Destination', repo), h('label', {}, 'Schedule', every),
      h('label', {}, 'Keep last', kLast), h('label', {}, 'Daily', kDaily), h('label', {}, 'Weekly', kWeekly),
      h('label', {}, 'Monthly', kMonthly), save),
    h('p', { class: 'muted small' }, 'Daily and weekly backups run in the nightly maintenance window. Scheduled backups follow the ' +
      '"keep" rules; manual ones stay until deleted; safety backups (taken before restores and pushes) are kept 7 days.'),
    p.last_error ? h('p', { class: 'st-failed small' }, `Last scheduled backup failed (${fmtTime(p.last_attempt_at)}): ${p.last_error}`) : null,
    h('div', { class: 'actions' }, now),
    ...errs,
    table(['Time', 'Kind', 'Where', 'Size', 'New data', ''], rows));
}

// -- Staging --

function showStaging(el, site) {
  const body = $('.env-body', el);
  if (site.parent_id) {
    const files = h('select', {}, h('option', { value: '' }, 'No files'),
      h('option', { value: 'code', selected: true }, 'Code: core, plugins, themes (not uploads)'), h('option', { value: 'all' }, 'All files, uploads too'));
    const db = h('input', { type: 'checkbox' });
    const tablesBox = h('div', { class: 'small' });
    let boxes = [];
    db.addEventListener('change', async () => {
      if (!db.checked) { tablesBox.replaceChildren(); boxes = []; return; }
      try {
        const tables = await api('GET', `/sites/${site.id}/tables`);
        boxes = tables.map((t) => h('input', { type: 'checkbox', value: t }));
        tablesBox.replaceChildren(h('p', { class: 'muted' }, 'Tables: none ticked = the whole database (live tables staging doesn\'t have are dropped). ' +
          'Tick some to push only those (e.g. posts, not orders).'),
        h('div', { class: 'controls' }, tables.map((t, i) => h('label', { class: 'check' }, boxes[i], t))));
      } catch (e) { showError(e); }
    });
    const push = h('button', {}, `Push to ${siteLabel(site.parent_id)}`);
    push.addEventListener('click', async () => {
      const tables = boxes.filter((b) => b.checked).map((b) => b.value);
      const what = [files.value && files.options[files.selectedIndex].textContent, db.checked && (tables.length ? `${tables.length} table(s)` : 'the whole database')].filter(Boolean);
      if (!what.length) { showError(new Error('Choose files, the database or both.')); return; }
      if (!await ask(`Push ${what.join(' and ')} to the LIVE site ${siteLabel(site.parent_id)}?\n\nThe live site is backed up first, so this can be undone from its Backups.`)) return;
      push.disabled = true;
      try { await startJob('POST', `/sites/${site.id}/push`, { files: files.value, database: db.checked, tables }); } catch (e) { showError(e); }
      finally { push.disabled = false; }
    });
    fill(body, 
      h('p', { class: 'small' }, 'A staging copy of ', h('strong', {}, siteLabel(site.parent_id)),
        '. Search engines are asked not to index it, it sends no mail through the mail server and WPGenie runs no cron for it.'),
      h('div', { class: 'controls' }, h('label', {}, 'Files', files), h('label', { class: 'check' }, db, 'Database'), push),
      tablesBox,
      h('p', { class: 'muted small' }, 'Links are rewritten to the live domain on the way; the live site\'s search engine setting is kept.'));
    return;
  }
  const staging = [...SITES.values()].find((s) => s.parent_id === site.id);
  if (staging) {
    fill(body, h('p', { class: 'small' }, 'Staging site: ',
      h('a', { href: 'https://' + staging.primary_domain, target: '_blank', rel: 'noopener' }, staging.primary_domain),
      ` (${staging.id}). Push changes from its card; delete it there to create a fresh copy.`));
    return;
  }
  const domain = h('input', { placeholder: 'staging.' + site.primary_domain.replace(/^www\./, '') });
  const create = h('button', {}, 'Create staging site');
  create.addEventListener('click', async () => {
    create.disabled = true;
    try { await startJob('POST', `/sites/${site.id}/staging`, { domain: domain.value.trim() }); } catch (e) { showError(e); }
    finally { create.disabled = false; }
  });
  fill(body, 
    h('p', { class: 'muted small' }, 'A full copy of this site (files and database) on its own domain, to try updates, themes and changes safely. ' +
      'Point the domain\'s DNS at this server first.'),
    h('div', { class: 'controls' }, h('label', {}, 'Domain', domain), create));
}

// -- Domains & SSL --

async function showDomains(el, site) {
  const body = $('.dom-body', el);
  const cert = await api('GET', `/sites/${site.id}/certificate`);
  const act = (label, fn, cls = 'ghost') => {
    const b = h('button', { class: cls }, label);
    b.addEventListener('click', async () => { b.disabled = true; try { await fn(); } catch (e) { showError(e); b.disabled = false; } });
    return b;
  };
  const refresh = async () => { await load(); };
  const makePrimary = (d) => act('Make primary', async () => {
    if (!await ask(`Make ${d} the primary domain? Links in the database are rewritten to it and ${site.primary_domain} redirects to it.`)) return;
    await startJob('PUT', `/sites/${site.id}/primary-domain`, { domain: d });
  });
  const remove = (d) => act('Remove', async () => {
    if (!await ask(`Stop answering on ${d}?`)) return;
    await api('DELETE', `/sites/${site.id}/domains/${encodeURIComponent(d)}`); await refresh();
  }, 'ghost danger');
  const rows = [[h('td', {}, h('strong', {}, site.primary_domain), ' ', h('span', { class: 'badge' }, 'primary')), 'serves the site', '']];
  for (const d of site.domains.filter((x) => x !== site.primary_domain)) {
    rows.push([d, 'serves the site', h('td', {}, h('div', { class: 'actions' }, makePrimary(d),
      act('Redirect instead', async () => { await api('PUT', `/sites/${site.id}/domains/${encodeURIComponent(d)}`, { redirect: true }); await refresh(); }), remove(d)))]);
  }
  for (const d of site.redirect_domains) {
    rows.push([d, `redirects to ${site.primary_domain}`, h('td', {}, h('div', { class: 'actions' }, makePrimary(d),
      act('Serve instead', async () => { await api('PUT', `/sites/${site.id}/domains/${encodeURIComponent(d)}`, { redirect: false }); await refresh(); }), remove(d)))]);
  }
  const apex = site.primary_domain.replace(/^www\./, '');
  const twin = site.primary_domain.startsWith('www.') ? apex : 'www.' + site.primary_domain;
  const known = [...site.domains, ...site.redirect_domains];
  const www = known.includes(twin) ? null : act(`Redirect ${twin} here`, async () => {
    await api('POST', `/sites/${site.id}/domains`, { domain: twin, redirect: true }); await refresh();
  });
  const input = h('input', { placeholder: 'example.org' });
  const redirect = h('input', { type: 'checkbox', checked: true });
  const add = act('Add domain', async () => {
    await api('POST', `/sites/${site.id}/domains`, { domain: input.value.trim(), redirect: redirect.checked }); await refresh();
  }, '');

  const certBox = h('div');
  const certPem = h('textarea', { placeholder: '-----BEGIN CERTIFICATE-----  (the certificate, then any intermediates)' });
  const keyPem = h('textarea', { placeholder: '-----BEGIN PRIVATE KEY-----' });
  const upload = act('Upload certificate', async () => {
    await api('PUT', `/sites/${site.id}/certificate`, { certificate: certPem.value, key: keyPem.value });
    certPem.value = keyPem.value = '';
    await showDomains(el, site);
  }, '');
  if (cert) {
    const days = Math.round((new Date(cert.not_after) - Date.now()) / 864e5);
    certBox.append(h('dl', { class: 'kv' },
      h('dt', {}, 'Certificate'), h('dd', {}, 'Your own (uploaded): ', cert.names.join(', ')),
      h('dt', {}, 'Issuer'), h('dd', {}, cert.issuer),
      h('dt', {}, 'Expires'), h('dd', { class: days < 14 ? 'st-failed' : '' }, `${fmtTime(cert.not_after)} (${days} days) — not renewed automatically`),
      h('dt', {}, 'Trusted by browsers'), h('dd', {}, cert.trusted ? 'yes' : 'no (fine behind Cloudflare with an origin certificate)')),
    h('div', { class: 'actions' }, act('Use automatic certificates', async () => {
      if (!await ask('Remove the uploaded certificate? Caddy obtains one from Let\'s Encrypt again.')) return;
      await api('DELETE', `/sites/${site.id}/certificate`); await showDomains(el, site);
    }, 'ghost danger')));
  } else {
    certBox.append(h('p', { class: 'small' }, 'Certificates: automatic (Let\'s Encrypt), renewed by Caddy.'));
  }
  fill(body, 
    table(['Domain', '', ''], rows),
    h('div', { class: 'controls' }, h('label', {}, 'Add a domain', input), h('label', { class: 'check' }, redirect, 'Redirect to the primary domain'), add, www),
    h('p', { class: 'muted small' }, 'Point every domain\'s DNS here first. Redirects keep the path (301). ' +
      'Changing the primary domain rewrites the site\'s links (like www ↔ bare domain).'),
    h('h3', {}, 'TLS certificate'), certBox,
    h('details', {}, h('summary', { class: 'small' }, cert ? 'Replace the certificate' : 'Use your own certificate instead'),
      h('p', { class: 'muted small' }, 'It must cover every domain the site serves (not the redirects). PEM format.'),
      h('label', {}, 'Certificate chain', certPem), h('label', {}, 'Private key', keyPem), h('div', { class: 'actions' }, upload)));
}

// -- PHP --

async function showPHP(el, site) {
  const info = await api('GET', '/php');
  const version = h('select', {}, info.versions.map((v) => h('option', { value: v, selected: v === site.php_version }, `PHP ${v}`)));
  const s = site.php || {};
  const num = (v, ph) => h('input', { type: 'number', min: 0, value: v || 0, placeholder: ph });
  const mem = num(s.memory_limit_mb), up = num(s.upload_max_mb), exec = num(s.max_execution_time), vars = num(s.max_input_vars);
  const apply = h('button', {}, 'Apply');
  apply.addEventListener('click', async () => {
    const switching = version.value !== site.php_version;
    if (switching && !await ask(`Switch ${site.primary_domain} to PHP ${version.value}? Replicas are replaced with no downtime; ` +
      'if the site stops working on the new version, it is switched back automatically.')) return;
    apply.disabled = true;
    try {
      await startJob('PUT', `/sites/${site.id}/php`, {
        version: version.value, settings: {
          memory_limit_mb: Number(mem.value), upload_max_mb: Number(up.value), max_execution_time: Number(exec.value), max_input_vars: Number(vars.value),
        },
      });
    } catch (e) { showError(e); } finally { apply.disabled = false; }
  });
  fill($('.php-body', el), 
    h('div', { class: 'controls' }, h('label', {}, 'Version', version),
      h('label', {}, 'memory_limit MB', h('span', { class: 'muted' }, ' (0 = 256)'), mem),
      h('label', {}, 'Max upload MB', h('span', { class: 'muted' }, ' (0 = 64)'), up),
      h('label', {}, 'max_execution_time s', h('span', { class: 'muted' }, ' (0 = 60)'), exec),
      h('label', {}, 'max_input_vars', h('span', { class: 'muted' }, ' (0 = 5000)'), vars), apply),
    h('p', { class: 'muted small' }, 'The first switch to a version builds its image (a few minutes). memory_limit is per request, ' +
      'up to the replica\'s memory; long requests also need a proxy/CDN that waits for them.'));
}

// -- SFTP & database --

async function showAccess(el, site) {
  const info = await api('GET', `/sites/${site.id}/sftp`);
  const body = $('.acc-body', el);
  const rows = info.users.map((u) => {
    const pw = h('button', { class: 'ghost' }, u.password ? 'New password' : 'Add password');
    pw.addEventListener('click', async () => {
      try { const r = await api('PUT', `/sites/${site.id}/sftp/${u.username}/password`, { enabled: true }); showSecret(`SFTP password for ${u.username}`, [`Login:    ${u.username}`, `Password: ${r.password}`]); await showAccess(el, site); }
      catch (e) { showError(e); }
    });
    const nopw = h('button', { class: 'ghost', hidden: !u.password || !u.public_keys.length }, 'Keys only');
    nopw.addEventListener('click', async () => {
      try { await api('PUT', `/sites/${site.id}/sftp/${u.username}/password`, { enabled: false }); await showAccess(el, site); } catch (e) { showError(e); }
    });
    const keys = h('button', { class: 'ghost' }, 'Keys');
    keys.addEventListener('click', async () => {
      const v = await askText('One authorized_keys line per key, separated by ";".',
        { title: `SFTP keys for ${u.username}`, label: 'Public keys', value: u.public_keys.join(' ; '), ok: 'Save keys' });
      if (v === null) return;
      try { await api('PUT', `/sites/${site.id}/sftp/${u.username}/keys`, { public_keys: v.split(';') }); await showAccess(el, site); } catch (e) { showError(e); }
    });
    const del = h('button', { class: 'ghost danger' }, 'Delete');
    del.addEventListener('click', async () => {
      if (!await ask(`Delete the SFTP login ${u.username}? Its open sessions end now.`)) return;
      try { await api('DELETE', `/sites/${site.id}/sftp/${u.username}`); await showAccess(el, site); } catch (e) { showError(e); }
    });
    return [u.username, u.password ? 'yes' : 'no', String(u.public_keys.length), h('td', {}, h('div', { class: 'actions' }, pw, nopw, keys, del))];
  });
  const suffix = h('input', { placeholder: 'optional, e.g. dev' });
  const withPw = h('input', { type: 'checkbox', checked: true });
  const keys = h('textarea', { placeholder: 'ssh-ed25519 AAAA… you@laptop  (optional)' });
  const add = h('button', {}, 'Add login');
  add.addEventListener('click', async () => {
    add.disabled = true;
    try {
      const r = await api('POST', `/sites/${site.id}/sftp`, { suffix: suffix.value.trim(), password: withPw.checked, public_keys: [keys.value] });
      if (r.password) showSecret(`SFTP login ${r.user.username}`, [`Host:     ${info.host}`, `Port:     ${info.server.port}`, `Login:    ${r.user.username}`, `Password: ${r.password}`]);
      await showAccess(el, site);
    } catch (e) { showError(e); add.disabled = false; }
  });
  const adminer = h('button', {}, 'Open Adminer');
  adminer.addEventListener('click', async () => {
    // Opened now, in the click: a window opened after the request would be blocked as a pop-up.
    const win = window.open('about:blank', '_blank');
    adminer.disabled = true;
    try {
      const r = await api('POST', `/sites/${site.id}/adminer`);
      if (win) { win.opener = null; win.location = r.url; } else { showSecret('Adminer link (single use, 2 minutes)', [r.url]); }
    } catch (e) { if (win) win.close(); showError(e); }
    finally { adminer.disabled = false; }
  });
  fill(body, 
    h('h3', {}, 'SFTP'),
    h('dl', { class: 'kv' }, h('dt', {}, 'Host'), h('dd', {}, info.host, h('span', { class: 'muted' }, ' (or this server\'s IP; not through Cloudflare\'s proxy)')),
      h('dt', {}, 'Port'), h('dd', {}, String(info.server.port)),
      ...(info.server.host_keys.length ? [h('dt', {}, 'Server key'), h('dd', {}, info.server.host_keys.join(' · '))] : [])),
    info.users.length ? table(['Login', 'Password', 'Keys', ''], rows) : h('p', { class: 'muted small' }, 'No SFTP logins yet.'),
    h('div', { class: 'controls' }, h('label', {}, `Login name ${site.id}-…`, suffix), h('label', { class: 'check' }, withPw, 'Generate a password')),
    h('label', {}, 'Public keys', keys), h('div', { class: 'actions' }, add),
    h('p', { class: 'muted small' }, 'Logins only see this site\'s directory (the WordPress install in public/) and can\'t run commands. ' +
      'Files they upload belong to the site, like WordPress\'s own.'),
    h('h3', {}, 'Database'),
    h('p', { class: 'muted small' }, 'Adminer opens on the site\'s own domain with a temporary database account for this site only. ' +
      'The link works once, within 2 minutes; the session ends after 15 minutes idle or an hour.' +
      (info.adminer_sessions ? ` Open sessions: ${info.adminer_sessions}.` : '')),
    h('div', { class: 'actions' }, adminer));
}

// ---- Backups tab: destinations and every backup in them ----

async function loadBackups() {
  const repos = await api('GET', '/backups/repos');
  const act = (label, fn, cls = 'ghost') => {
    const b = h('button', { class: cls }, label);
    b.addEventListener('click', async () => { b.disabled = true; try { await fn(); } catch (e) { showError(e); } finally { b.disabled = false; } });
    return b;
  };
  $('#repos').replaceChildren(table(['Name', 'Type', 'Location', 'Sites', 'Checked', ''], repos.map((r) => [
    h('td', {}, h('strong', {}, r.name), r.public_key ? h('div', { class: 'wrap small', title: 'Add to the SFTP server\'s authorized_keys' }, r.public_key) : null,
      r.host_key_fingerprint ? h('div', { class: 'muted small' }, 'Server key ' + r.host_key_fingerprint) : null),
    r.kind, h('td', { class: 'wrap' }, r.location), String(r.sites_using),
    h('td', { class: r.check_error ? 'st-failed small' : 'small' }, r.check_error || (r.checked_at ? `OK ${fmtTime(r.checked_at)}` : '–'),
      r.pruned_at ? h('div', { class: 'muted' }, `pruned ${fmtTime(r.pruned_at)}`) : null),
    h('td', {}, h('div', { class: 'actions' },
      act('Check', async () => { await api('POST', `/backups/repos/${r.id}/check`); await loadBackups(); }),
      isAdmin() ? act('Password', async () => {
        const p = await api('POST', `/backups/repos/${r.id}/password`);
        showSecret(`Password of ${r.name}`, [`Repository: ${r.location}`, `Password:   ${p.password}`, '',
          'Needed to restore these backups anywhere else (another server, or restic by hand).']);
      }) : null,
      isAdmin() && r.id !== 'local' ? act('Remove', async () => {
        if (!await ask(`Forget the destination ${r.name}? Its backups stay where they are.`)) return;
        await api('DELETE', `/backups/repos/${r.id}`); await loadBackups();
      }, 'ghost danger') : null)),
  ])));
  const sel = $('#browse-form').repo;
  const cur = sel.value;
  sel.replaceChildren(...repos.map((r) => h('option', { value: r.id, selected: r.id === cur }, r.name)));
}

async function browseRepo(repoID) {
  const box = $('#browse');
  box.replaceChildren(h('p', { class: 'muted small' }, 'Listing…'));
  const list = await api('GET', `/backups/repos/${encodeURIComponent(repoID)}/backups`);
  box.replaceChildren(table(['Time', 'Site', 'Domain', 'Kind', 'Size', ''], list.map((b) => {
    const exists = SITES.has(b.site_id);
    const restore = h('button', { class: 'ghost admin-only' }, 'Restore as new site');
    restore.addEventListener('click', async () => {
      const domain = await askText(`A new site from the backup of ${b.domain} (${fmtTime(b.time)}). Its DNS must point here.`,
        { title: 'Restore as a new site', label: 'Domain for the new site', value: exists ? '' : b.domain, placeholder: 'example.com', ok: 'Restore' });
      if (!domain) return;
      try { await startJob('POST', '/backups/restore-new', { repo_id: repoID, backup_id: b.id, domain }); openTab('sites'); } catch (e) { showError(e); }
    });
    const del = h('button', { class: 'ghost danger admin-only' }, 'Delete');
    del.addEventListener('click', async () => {
      if (!await ask(`Delete the backup of ${b.domain} from ${fmtTime(b.time)}? This can't be undone.`)) return;
      try { await api('DELETE', `/backups/repos/${encodeURIComponent(repoID)}/backups/${b.id}`); await browseRepo(repoID); } catch (e) { showError(e); }
    });
    return [fmtTime(b.time), exists ? b.site_id : h('td', {}, b.site_id, h('span', { class: 'badge' }, 'deleted')), b.domain,
      h('td', {}, status(b.kind || 'manual')), fmtBytes(b.size), h('td', {}, h('div', { class: 'actions' }, restore, del))];
  })));
}

function repoFormKind() {
  const f = $('#repo-form');
  f.querySelectorAll('[data-kinds]').forEach((l) => { l.hidden = !l.dataset.kinds.split(' ').includes(f.kind.value); });
}

$('#repo-form').kind.addEventListener('change', repoFormKind);
repoFormKind();
$('#repo-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target, btn = $('button[type=submit]', f);
  const val = (n) => f[n].value.trim();
  btn.disabled = true;
  btn.textContent = 'Connecting…';
  try {
    const r = await api('POST', '/backups/repos', {
      name: val('name'), kind: val('kind'), endpoint: val('endpoint'), bucket: val('bucket'), prefix: val('prefix'), region: val('region'),
      key_id: val('key_id'), secret: f.secret.value, host: val('host'), port: Number(val('port') || 0), user: val('user'), path: val('path'),
      password: f.password.value,
    });
    const lines = [];
    if (r.password) lines.push('Repository password (keep it OFF this server: without it these backups can\'t be restored if the server is lost):', '', r.password);
    if (r.repo.public_key) lines.push('', `Add this line to ~/.ssh/authorized_keys of ${val('user')} on ${val('host')}, then press Check:`, '', r.repo.public_key,
      '', `Server key pinned: ${r.repo.host_key_fingerprint}`);
    if (lines.length) showSecret(`Destination ${r.repo.name} added`, lines);
    f.reset();
    repoFormKind();
    await loadBackups();
  } catch (err) { showError(err); } finally { btn.disabled = false; btn.textContent = 'Add destination'; }
});
$('#browse-form').addEventListener('submit', (e) => {
  e.preventDefault();
  browseRepo(e.target.repo.value).catch(showError);
});

loaders.backups = loadBackups;
