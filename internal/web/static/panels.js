'use strict';
// Mail, Security and System tabs. Shares api(), h(), table(), status() and
// showError() with app.js.

const loaders = { sites: () => load(), mail: loadMail, security: loadSecurity, system: loadSystem, account: loadAccount, users: loadUsers };

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
    if (!await ask('Stop the mail server and webmail? Mailboxes and mail are kept on disk.')) return;
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
    if (!await ask(`Stop accepting mail for ${d.domain}?`)) return;
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
      if (!await ask(`Replace the password of ${b.address}? Mail apps using the old one stop working.`)) return;
      try {
        const r = await api('PUT', `/mail/mailboxes/${encodeURIComponent(b.address)}/password`, {});
        showSecret(`New password for ${b.address}`, [`Password: ${r.password}`]);
      } catch (e) { showError(e); }
    });
    const rm = h('button', { class: 'ghost danger' }, 'Delete');
    rm.addEventListener('click', async () => {
      if (await askText(`This deletes ${b.address} and all its mail.`,
        { title: `Delete ${b.address}?`, label: 'Type the address to confirm', match: b.address, ok: 'Delete mailbox' }) !== b.address) return;
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
  const [bans, events, rep, lists] = await Promise.all([api('GET', '/security/bans'), api('GET', '/security/events?limit=100'),
    api('GET', '/security/reputation'), api('GET', '/security/settings')]);
  showReputation(rep);
  const gl = $('#global-lists');
  gl.allow.value = lists.allow.join(', ');
  gl.deny.value = lists.deny.join(', ');
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

function showReputation(rep) {
  const refresh = h('button', { class: 'ghost admin-only' }, 'Refresh now');
  refresh.addEventListener('click', async () => {
    refresh.disabled = true;
    refresh.textContent = 'Refreshing…';
    try { showReputation(await api('POST', '/security/reputation/refresh')); } catch (e) { showError(e); refresh.disabled = false; }
  });
  const c = rep.countries;
  $('#reputation').replaceChildren(
    table(['List', 'Entries', 'Updated', ''], rep.lists.map((l) => [l.title, fmtNum(l.entries),
      l.updated_at ? fmtTime(l.updated_at) : 'never', h('td', { class: l.error ? 'st-failed small' : '' }, l.error || '')])),
    h('p', { class: 'small' }, 'Country database: ', c && c.loaded ? `${c.month} release, ${fmtNum(c.ranges)} ranges` :
      'downloaded when a site first uses country rules', c && c.error ? h('span', { class: 'st-failed' }, ` (${c.error})`) : '',
      c ? h('span', { class: 'muted' }, `. ${c.attribution}`) : ''),
    h('p', { class: 'small' }, 'Request-body inspection (Coraza + OWASP CRS): ',
      rep.waf_available ? h('span', { class: 'st-ok' }, 'available') :
        h('span', { class: 'st-warning' }, 'not detected yet: it is checked when a site turns it on; Caddy needs the wpgenie/caddy image')),
    h('div', { class: 'actions' }, refresh));
}

$('#global-lists').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('PUT', '/security/settings', { allow: splitList(e.target.allow.value), deny: splitList(e.target.deny.value) });
    await loadSecurity();
  } catch (err) { showError(err); }
});

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
    if (!await ask(`Update WPGenie to ${info.latest.version}? The panel restarts; sites keep serving. ` +
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

// ---- Account ----

async function loadAccount() {
  const [acct, sessions] = await Promise.all([api('GET', '/account'), api('GET', '/account/sessions')]);
  const u = acct.user;
  ME = u;
  const notice = $('#account-notice');
  notice.hidden = !(acct.require_2fa && !u.totp_enabled);
  notice.replaceChildren(h('strong', {}, 'This panel requires two-factor authentication.'), ' Set it up below to continue.');
  $('#account-profile').replaceChildren(h('h2', {}, u.username), table(['', ''], [
    ['Role', u.role], ['Member since', fmtTime(u.created_at)], ['Last sign-in', u.last_login_at ? fmtTime(u.last_login_at) : '–']]));
  show2FA(u);
  $('#account-sessions').replaceChildren(sessionsTable(sessions, false, loadAccount));
}

function show2FA(u) {
  const box = $('#account-2fa');
  if (u.totp_enabled) {
    const regen = h('button', { class: 'ghost' }, 'New recovery codes');
    regen.addEventListener('click', async () => {
      const password = await askText('The old recovery codes stop working.',
        { title: 'New recovery codes', label: 'Your password', type: 'password', autocomplete: 'current-password', ok: 'Create codes' });
      if (!password) return;
      try { const r = await api('POST', '/account/recovery-codes', { password }); showSecret('Recovery codes', r.recovery_codes); await loadAccount(); }
      catch (e) { showError(e); }
    });
    const off = h('button', { class: 'ghost danger' }, 'Turn off');
    off.addEventListener('click', async () => {
      const password = await askText('Signing in will only need your password.',
        { title: 'Turn off two-factor authentication?', label: 'Your password', type: 'password', autocomplete: 'current-password', ok: 'Turn off' });
      if (!password) return;
      try { await api('DELETE', '/account/totp', { password }); await loadAccount(); } catch (e) { showError(e); }
    });
    box.replaceChildren(h('p', {}, h('span', { class: 'st-ok' }, 'On.'),
      ` Signing in asks for a code from your authenticator app. ${u.recovery_codes_left} recovery code(s) left.`),
    h('div', { class: 'actions' }, regen, off));
    return;
  }
  const start = h('button', {}, 'Set up two-factor authentication');
  start.addEventListener('click', async () => {
    try {
      const r = await api('POST', '/account/totp');
      const code = h('input', { placeholder: '6-digit code', inputmode: 'numeric', autocomplete: 'one-time-code', required: true });
      const form = h('form', { class: 'inline' }, code, h('button', { type: 'submit' }, 'Turn on'));
      form.addEventListener('submit', async (e) => {
        e.preventDefault();
        try {
          const locked = !$('#account-notice').hidden; // the panel was waiting for this
          const res = await api('PUT', '/account/totp', { code: code.value.trim() });
          await loadAccount();
          if (locked) signedIn(ME);
          showSecret('Recovery codes: each signs you in once if you lose your phone', res.recovery_codes);
        } catch (err) { showError(err); }
      });
      box.replaceChildren(
        h('p', {}, 'Scan this with an authenticator app (1Password, Google Authenticator, Authy, …), then enter the code it shows.'),
        h('img', { class: 'qr', src: '/api/v1/account/totp/qr.svg?' + Date.now(), alt: 'QR code for your authenticator app', width: 200, height: 200 }),
        h('p', { class: 'muted small' }, 'Can\'t scan? Enter this key: ', h('code', {}, r.secret.replace(/(.{4})/g, '$1 ').trim())),
        form);
      code.focus();
    } catch (e) { showError(e); }
  });
  box.replaceChildren(h('p', {}, h('span', { class: 'st-warning' }, 'Off.'),
    ' A stolen password is enough to get into your account. Add a code from your phone.'), start);
}

function sessionsTable(sessions, withUser, reload) {
  return table([withUser ? 'User' : null, 'Signed in', 'Last active', 'From', 'Browser', ''].filter((x) => x !== null), sessions.map((s) => {
    const out = h('button', { class: 'ghost' }, s.current ? 'Sign out' : 'Revoke');
    out.addEventListener('click', async () => {
      try {
        await api('DELETE', (withUser ? '/sessions/' : '/account/sessions/') + encodeURIComponent(s.id));
        if (s.current) { showSignIn(); return; }
        await reload();
      } catch (e) { showError(e); }
    });
    const row = [fmtTime(s.created_at), fmtTime(s.last_seen_at), s.ip, h('td', { class: 'small', title: s.user_agent }, shortUA(s.user_agent) + (s.current ? ' (this one)' : '')), h('td', {}, out)];
    return withUser ? [s.username, ...row] : row;
  }));
}

function shortUA(ua) {
  const m = /(Firefox|Edg|Chrome|Safari)\/[\d.]+/.exec(ua || '');
  const os = /(Windows|Mac OS X|Android|iPhone|Linux)/.exec(ua || '');
  return [m ? m[1].replace('Edg', 'Edge') : 'unknown', os ? os[1].replace('Mac OS X', 'macOS') : ''].filter(Boolean).join(' on ');
}

$('#password-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  try {
    await api('PUT', '/account/password', { current: e.target.current.value, new: e.target.new.value });
    e.target.reset();
    showSecret('Password changed', ['Your other sessions were signed out.']);
  } catch (err) { showError(err); }
});

// ---- Users (administrators) ----

async function loadUsers() {
  const [users, sessions, settings] = await Promise.all([api('GET', '/users'), api('GET', '/sessions'), api('GET', '/settings/auth')]);
  $('#require-2fa').checked = settings.require_2fa;
  $('#users').replaceChildren(table(['User', 'Role', '2FA', 'Last sign-in', ''], users.map((u) => {
    const role = h('select', {}, ['viewer', 'operator', 'admin'].map((r) => h('option', { value: r, selected: r === u.role }, r)));
    role.addEventListener('change', async () => {
      try { await api('PUT', `/users/${u.id}`, { role: role.value }); } catch (e) { showError(e); role.value = u.role; }
    });
    const act = (label, fn, cls = 'ghost') => { const b = h('button', { class: cls }, label); b.addEventListener('click', fn); return b; };
    const self = ME && ME.id === u.id;
    const actions = h('div', { class: 'actions' },
      act(u.disabled ? 'Enable' : 'Disable', async () => {
        try { await api('PUT', `/users/${u.id}`, { disabled: !u.disabled }); await loadUsers(); } catch (e) { showError(e); }
      }),
      act('Reset password', async () => {
        if (!await ask(`Give ${u.username} a new password? They are signed out everywhere.`)) return;
        try { const r = await api('POST', `/users/${u.id}/password`); showSecret(`New password for ${u.username}`, [`Password: ${r.password}`]); } catch (e) { showError(e); }
      }),
      u.totp_enabled ? act('Reset 2FA', async () => {
        if (!await ask(`Turn off two-factor authentication for ${u.username} (lost phone)? They are signed out everywhere.`)) return;
        try { await api('DELETE', `/users/${u.id}/totp`); await loadUsers(); } catch (e) { showError(e); }
      }) : null,
      self ? null : act('Delete', async () => {
        if (await askText('They are signed out everywhere and can no longer sign in.',
          { title: `Delete ${u.username}?`, label: 'Type the username to confirm', match: u.username, ok: 'Delete user' }) !== u.username) return;
        try { await api('DELETE', `/users/${u.id}`); await loadUsers(); } catch (e) { showError(e); }
      }, 'ghost danger'));
    return [h('td', {}, u.username, u.disabled ? h('span', { class: 'st-failed small' }, ' disabled') : '', self ? h('span', { class: 'muted small' }, ' (you)') : ''),
      h('td', {}, role), h('td', {}, u.totp_enabled ? h('span', { class: 'st-ok' }, 'on') : h('span', { class: 'st-warning' }, 'off')),
      u.last_login_at ? fmtTime(u.last_login_at) : 'never', h('td', {}, actions)];
  })));
  $('#all-sessions').replaceChildren(sessionsTable(sessions, true, loadUsers));
  await loadAudit();
}

async function loadAudit() {
  const actor = $('#audit-filter').actor.value.trim();
  const entries = await api('GET', '/audit?limit=200' + (actor ? '&actor=' + encodeURIComponent(actor) : ''));
  $('#audit').replaceChildren(table(['Time', 'User', 'From', 'Action', 'Result', 'Detail'], entries.map((e) => [
    fmtTime(e.time), e.actor, e.ip, h('td', { class: 'wrap' }, e.action),
    h('td', { class: e.status >= 400 ? 'st-failed' : 'st-ok' }, e.status || ''), h('td', { class: 'small' }, e.detail || '')])));
}

$('#audit-filter').addEventListener('submit', (e) => { e.preventDefault(); loadAudit().catch(showError); });

$('#user-form').addEventListener('submit', async (e) => {
  e.preventDefault();
  const f = e.target;
  try {
    const r = await api('POST', '/users', { username: f.username.value.trim(), role: f.role.value });
    showSecret(`User ${r.user.username} created`, [`Username: ${r.user.username}`, `Password: ${r.password}`, '', 'They can change the password and set up two-factor authentication under Account.']);
    f.reset();
    await loadUsers();
  } catch (err) { showError(err); }
});

$('#require-2fa').addEventListener('change', async (e) => {
  try {
    await api('PUT', '/settings/auth', { require_2fa: e.target.checked });
    if (e.target.checked && !ME.totp_enabled) openTab('account');
  } catch (err) { showError(err); e.target.checked = !e.target.checked; }
});
