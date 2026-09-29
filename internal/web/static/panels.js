'use strict';
// Mail, Security and System tabs. Shares api(), h(), table(), status() and
// showError() with app.js.

const loaders = { sites: () => load(), mail: loadMail, security: loadSecurity, system: loadSystem };

document.querySelectorAll('.tab').forEach((tab) => tab.addEventListener('click', () => {
  document.querySelectorAll('.tab').forEach((t) => t.classList.toggle('active', t === tab));
  document.querySelectorAll('.panel').forEach((p) => { p.hidden = p.dataset.panel !== tab.dataset.tab; });
  showError(null);
  loaders[tab.dataset.tab]().catch(showError);
}));

function showSecret(title, lines) {
  const box = $('#creds');
  box.replaceChildren(h('h2', {}, title), h('p', {}, 'Save this now — it is shown only once.'),
    h('pre', {}, lines.join('\n')), h('button', { class: 'ghost', onclick: () => { box.hidden = true; } }, 'Dismiss'));
  box.hidden = false;
}

// ---- Mail ----

async function loadMail() {
  const st = await api('GET', '/mail');
  const box = $('#mail-status');
  $('#mail-body').hidden = !st.enabled;
  if (!st.enabled) {
    const form = h('form', { class: 'inline' },
      h('input', { name: 'hostname', placeholder: 'mail.example.com', required: true }),
      h('button', { type: 'submit' }, 'Enable mail'));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      try { await api('PUT', '/mail', { enabled: true, hostname: form.hostname.value }); await loadMail(); } catch (err) { showError(err); }
    });
    box.replaceChildren(h('h2', {}, 'Mail is off'),
      h('p', { class: 'muted' }, 'Mailboxes with IMAP/SMTP (docker-mailserver: Postfix, Dovecot, Rspamd spam filtering, DKIM) and Roundcube webmail. ' +
        'Choose a hostname for the mail server and point its A record at this server first.'),
      form,
      h('p', { class: 'muted small' }, 'Open ports 25, 465, 587 and 993 in your firewall, and ask your provider to set this server\'s reverse DNS (PTR) to the hostname.'));
    return;
  }
  const disable = h('button', { class: 'ghost danger' }, 'Disable mail');
  disable.addEventListener('click', async () => {
    if (!confirm('Stop the mail server and webmail? Mailboxes and mail are kept on disk.')) return;
    try { await api('PUT', '/mail', { enabled: false }); await loadMail(); } catch (err) { showError(err); }
  });
  box.replaceChildren(
    h('div', { class: 'site-head' }, h('h2', {}, st.hostname), disable),
    table(['Component', 'State'], [['Mail server (SMTP/IMAP)', h('td', {}, status(st.server))], ['Webmail', h('td', {}, status(st.webmail))]]),
    st.detail ? h('p', { class: 'muted small' }, st.detail) : null,
    h('p', { class: 'small' }, 'Webmail: ', h('a', { href: st.webmail_url, target: '_blank', rel: 'noopener' }, st.webmail_url),
      ' · Mail clients: IMAP ', h('code', {}, `${st.hostname}:993`), ' (SSL), SMTP ', h('code', {}, `${st.hostname}:587`), ' (STARTTLS)'),
  );
  const relay = $('#relay-form');
  relay.host.value = st.relay ? st.relay.host : '';
  relay.port.value = st.relay ? st.relay.port : '';
  relay.user.value = st.relay ? st.relay.user : '';
  relay.password.value = '';
  await Promise.all([loadDomains(), loadMailboxes(), loadAliases()]);
}

async function loadDomains() {
  const ds = await api('GET', '/mail/domains');
  $('#mail-domains').replaceChildren(...(ds.length ? ds.map(renderDomain) : [h('p', { class: 'muted small' }, 'Add a domain to create mailboxes on it.')]));
}

function renderDomain(d) {
  const recs = h('div');
  const show = (info) => recs.replaceChildren(table(['Status', 'Type', 'Name', 'Value'], info.records.map((r) =>
    [h('td', {}, status(r.status)), r.type, h('td', { class: 'wrap' }, r.name), h('td', { class: 'wrap', title: r.found ? 'found: ' + r.found : '' }, r.value)])));
  show(d);
  const check = h('button', { class: 'ghost' }, 'Check DNS');
  check.addEventListener('click', async () => {
    check.disabled = true;
    try { show(await api('GET', `/mail/domains/${encodeURIComponent(d.domain)}?check=1`)); } catch (e) { showError(e); }
    finally { check.disabled = false; }
  });
  const rm = h('button', { class: 'ghost danger' }, 'Remove');
  rm.addEventListener('click', async () => {
    if (!confirm(`Stop accepting mail for ${d.domain}?`)) return;
    try { await api('DELETE', `/mail/domains/${encodeURIComponent(d.domain)}`); await loadDomains(); } catch (e) { showError(e); }
  });
  return h('div', { class: 'card' }, h('div', { class: 'site-head' },
    h('strong', {}, d.domain, h('span', { class: 'muted small' }, ` · ${d.mailboxes} mailbox(es)`)), h('div', { class: 'actions' }, check, rm)),
  h('p', { class: 'muted small' }, 'Publish these records at your DNS provider. The DKIM key is long: most providers split it automatically.'), recs);
}

async function loadMailboxes() {
  const boxes = await api('GET', '/mail/mailboxes');
  $('#mailboxes').replaceChildren(table(['Address', 'Quota', '', ''], boxes.map((b) => {
    const pw = h('button', { class: 'ghost' }, 'New password');
    pw.addEventListener('click', async () => {
      if (!confirm(`Replace the password of ${b.address}? Mail apps using the old one stop working.`)) return;
      try {
        const r = await api('PUT', `/mail/mailboxes/${encodeURIComponent(b.address)}/password`, {});
        showSecret(`New password for ${b.address}`, [`Password: ${r.password}`]);
      } catch (e) { showError(e); }
    });
    const rm = h('button', { class: 'ghost danger' }, 'Delete');
    rm.addEventListener('click', async () => {
      if (prompt(`This deletes ${b.address} and ALL its mail.\nType the address to confirm:`) !== b.address) return;
      try { await api('DELETE', `/mail/mailboxes/${encodeURIComponent(b.address)}`); await loadMailboxes(); } catch (e) { showError(e); }
    });
    return [b.address + (b.site_id ? ` (WordPress sender of ${b.site_id})` : ''), b.quota_mb ? `${b.quota_mb} MB` : 'unlimited',
      h('td', {}, b.site_id ? '' : pw), h('td', {}, b.site_id ? '' : rm)];
  })));
}

async function loadAliases() {
  const as = await api('GET', '/mail/aliases');
  $('#aliases').replaceChildren(table(['Alias', 'Delivers to', ''], as.map((a) => {
    const rm = h('button', { class: 'ghost danger' }, 'Remove');
    rm.addEventListener('click', async () => {
      try {
        await api('DELETE', `/mail/aliases?alias=${encodeURIComponent(a.alias)}&target=${encodeURIComponent(a.target)}`);
        await loadAliases();
      } catch (e) { showError(e); }
    });
    return [a.alias, a.target, h('td', {}, rm)];
  })));
}

$('#mail-domain-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try { await api('POST', '/mail/domains', { domain: e.target.domain.value }); e.target.reset(); await loadDomains(); } catch (err) { showError(err); }
});
$('#mailbox-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    const r = await api('POST', '/mail/mailboxes', { address: f.address.value, quota_mb: Number(f.quota_mb.value || 0) });
    showSecret(`Mailbox ${r.mailbox.address} created`, [`Address:  ${r.mailbox.address}`, `Password: ${r.password}`]);
    f.reset();
    await loadMail();
  } catch (err) { showError(err); }
});
$('#alias-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try { await api('POST', '/mail/aliases', { alias: e.target.alias.value, target: e.target.target.value }); e.target.reset(); await loadAliases(); }
  catch (err) { showError(err); }
});
$('#relay-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    await api('PUT', '/mail/relay', { host: f.host.value, port: Number(f.port.value || 587), user: f.user.value, password: f.password.value });
    await loadMail();
  } catch (err) { showError(err); }
});
$('#relay-clear').addEventListener('click', async () => {
  try { await api('DELETE', '/mail/relay'); await loadMail(); } catch (err) { showError(err); }
});

// ---- Security ----

async function loadSecurity() {
  const [bans, events] = await Promise.all([api('GET', '/security/bans'), api('GET', '/security/events?limit=100')]);
  $('#bans').replaceChildren(table(['Address', 'Until', 'Reason', ''], bans.map((b) => {
    const un = h('button', { class: 'ghost' }, 'Unban');
    un.addEventListener('click', async () => {
      try { await api('DELETE', `/security/bans?addr=${encodeURIComponent(b.addr)}`); await loadSecurity(); } catch (e) { showError(e); }
    });
    return [b.addr, fmtTime(b.until), b.reason + (b.manual ? ' (manual)' : ''), h('td', {}, un)];
  })));
  $('#sec-events').replaceChildren(table(['Time', 'Site', 'Client', 'Action', 'Reason', 'Path'],
    events.map((e) => [fmtTime(e.time), e.site, e.ip, h('td', {}, status(e.verdict === 'ban' ? 'failed' : e.verdict)), e.reason,
      h('td', { class: 'wrap' }, e.path)])));
}

$('#ban-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('POST', '/security/bans', { addr: e.target.addr.value, hours: Number(e.target.hours.value || 24) });
    e.target.reset();
    await loadSecurity();
  } catch (err) { showError(err); }
});

// ---- System (WPGenie self-update) ----

async function checkSystem() {
  try {
    const info = await api('GET', '/system');
    $('#update-dot').hidden = !info.available;
    return info;
  } catch (e) { return null; }
}

async function loadSystem(fresh) {
  const info = fresh || await api('GET', '/system');
  $('#update-dot').hidden = !info.available;
  const box = $('#system');
  const rows = [['Running', info.current]];
  // Release data isn't signed: only ever link to GitHub.
  const safeURL = (u) => (typeof u === 'string' && u.startsWith('https://github.com/') ? u : null);
  if (info.latest) rows.push(['Latest release', h('td', {}, h('a', { href: safeURL(info.latest.url), target: '_blank', rel: 'noopener' }, info.latest.version))]);
  if (info.check_error) rows.push(['Last check', info.check_error]);
  const st = info.status;
  if (st) rows.push(['Last update', h('td', {}, status(st.phase), ` ${st.from} → ${st.to}: ${st.message} (${fmtTime(st.at)})`)]);

  const check = h('button', { class: 'ghost' }, 'Check now');
  check.addEventListener('click', async () => {
    check.disabled = true;
    try { await loadSystem(await api('POST', '/system/update/check')); } catch (e) { showError(e); check.disabled = false; }
  });
  const active = st && ['staged', 'applying', 'restarting'].includes(st.phase);
  const update = h('button', { disabled: !info.available || !info.signing_key || active },
    active ? 'Updating…' : info.available ? `Update to ${info.latest.version}` : 'Up to date');
  update.addEventListener('click', async () => {
    if (!confirm(`Update WPGenie to ${info.latest.version}? The panel restarts; sites keep serving. ` +
      'If the new version does not start, the previous one is restored automatically.')) return;
    update.disabled = true;
    try { await api('POST', '/system/update'); pollSystem(); } catch (e) { showError(e); update.disabled = false; }
  });
  box.replaceChildren(h('h2', {}, 'WPGenie'), table(['', ''], rows),
    !info.signing_key ? h('p', { class: 'muted small' }, 'This build has no release signing key, so it cannot update itself.') : null,
    info.latest && info.available ? h('details', {}, h('summary', {}, 'Release notes'), h('pre', { class: 'small' }, info.latest.notes)) : null,
    h('div', { class: 'actions' }, check, update));
  if (active) setTimeout(() => pollSystem(), 3000);
}

// While an update runs the daemon restarts: keep polling through the gap.
async function pollSystem() {
  for (let i = 0; i < 120; i++) {
    await new Promise((r) => setTimeout(r, 3000));
    try {
      const info = await api('GET', '/system');
      await loadSystem(info);
      if (!info.status || !['staged', 'applying', 'restarting'].includes(info.status.phase)) return;
    } catch (e) { /* restarting */ }
  }
}
