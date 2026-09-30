'use strict';
// E-mail to people (internal/mailer): the SMTP server the panel sends
// through, the templates staff can reword, and the log of what was sent.
// A tab of Billing for staff (see billing.js); clients see their own
// messages in the client area.

const EMAIL_SECTIONS = [['', 'Sending', 'admin'], ['templates', 'Templates', 'admin'], ['log', 'Sent e-mail', 'operator']];

async function staffEmail(box, route) {
  const sections = EMAIL_SECTIONS.filter(([, , role]) => canStaff(role));
  const section = sections.some(([k]) => k === route.id) ? route.id : sections[0][0];
  const content = h('div');
  box.replaceChildren(subnav('E-mail sections', sections.map(([key, label]) => ({ key, label, href: '#/billing/email' + (key ? '/' + key : '') })),
    section, (k) => billingGo('email', k), 'small'), content);
  try {
    await ({ '': emailSending, templates: emailTemplates, log: emailLog })[section](content);
  } catch (e) {
    content.replaceChildren(loadError(e, () => staffEmail(box, route)));
  }
}

// ---- Sending: the SMTP server ----

// Providers' usual settings; the username and password are always theirs.
const SMTP_PRESETS = [
  { id: 'custom', name: 'Other / my own server' },
  { id: 'gmail', name: 'Gmail / Google Workspace', host: 'smtp.gmail.com', port: 587, tls: 'starttls',
    hint: 'Username: the full address. Password: an app password (Google Account → Security → App passwords); 2-step verification must be on.' },
  { id: 'm365', name: 'Microsoft 365 / Outlook', host: 'smtp.office365.com', port: 587, tls: 'starttls',
    hint: 'Username: the full address. SMTP AUTH must be enabled for the mailbox in the Microsoft 365 admin center.' },
  { id: 'ses', name: 'Amazon SES', host: 'email-smtp.us-east-1.amazonaws.com', port: 587, tls: 'starttls',
    hint: 'Use your region\'s endpoint, and SMTP credentials made in the SES console (not your AWS keys). Verify the sending domain first.' },
  { id: 'sendgrid', name: 'SendGrid', host: 'smtp.sendgrid.net', port: 587, tls: 'starttls',
    hint: 'Username: apikey (the word). Password: an API key with Mail Send permission.' },
  { id: 'mailgun', name: 'Mailgun', host: 'smtp.mailgun.org', port: 587, tls: 'starttls',
    hint: 'EU accounts use smtp.eu.mailgun.org. Credentials: Sending → Domain settings → SMTP credentials.' },
  { id: 'postmark', name: 'Postmark', host: 'smtp.postmarkapp.com', port: 587, tls: 'starttls',
    hint: 'Username and password: your server API token (both).' },
  { id: 'brevo', name: 'Brevo (Sendinblue)', host: 'smtp-relay.brevo.com', port: 587, tls: 'starttls',
    hint: 'Username: your Brevo login. Password: an SMTP key (SMTP & API → SMTP).' },
  { id: 'zoho', name: 'Zoho Mail', host: 'smtp.zoho.com', port: 465, tls: 'tls',
    hint: 'EU and India accounts use smtp.zoho.eu / smtp.zoho.in. Use an app-specific password with 2FA.' },
  { id: 'local', name: 'This server\'s Mail (Mail tab)', host: 'localhost', port: 587, tls: 'starttls',
    hint: 'A mailbox created under Mail: its full address and password.' },
];

async function emailSending(box) {
  const s = await api('GET', '/settings/email');
  const preset = h('select', { 'aria-label': 'Provider' }, options(SMTP_PRESETS.map((p) => [p.id, p.name]),
    (SMTP_PRESETS.find((p) => p.host && p.host === s.host) || SMTP_PRESETS[0]).id));
  const hint = h('p', { class: 'provider-hint small', 'aria-live': 'polite' });
  const host = h('input', { name: 'host', value: s.host || '', placeholder: 'smtp.example.com', autocomplete: 'off' });
  const port = h('input', { name: 'port', type: 'number', min: 1, max: 65535, value: s.port || 587 });
  const tls = h('select', { name: 'tls' }, options([['starttls', 'STARTTLS (usually port 587)'], ['tls', 'TLS (usually port 465)'],
    ['none', 'None (a relay on this server only)']], s.tls || 'starttls'));
  const showHint = () => {
    const p = SMTP_PRESETS.find((x) => x.id === preset.value);
    hint.textContent = p.hint || 'Your provider\'s SMTP settings: host, port, and a username and password.';
  };
  preset.addEventListener('change', () => {
    const p = SMTP_PRESETS.find((x) => x.id === preset.value);
    if (p.host) { host.value = p.host; port.value = p.port; tls.value = p.tls; }
    showHint();
  });
  showHint();
  const form = settingsForm('Sending e-mail', 'Invoices, receipts, reminders and ticket replies go out through this server.', [
    toggle('enabled', 'Send e-mail', s.enabled, 'Off: messages wait in the log until it\'s on.'),
    field('Provider', preset), hint,
    h('div', { class: 'grid' }, field('SMTP server', host), field('Port', port), field('Encryption', tls),
      field('Username', h('input', { name: 'username', value: s.username || '', autocomplete: 'off' })),
      field('Password', h('input', { name: 'password', type: 'password', autocomplete: 'new-password',
        placeholder: s.password_set ? 'set (unchanged if empty)' : '' }))),
    h('h3', {}, 'From'),
    h('div', { class: 'grid' },
      field('Name', h('input', { name: 'from_name', value: s.from_name || '', placeholder: 'Acme Hosting' })),
      field('Address', h('input', { name: 'from_address', type: 'email', value: s.from_address || '', placeholder: 'billing@example.com' }),
        'Your provider must allow sending as it (SPF and DKIM for the domain).'),
      field('Replies to (optional)', h('input', { name: 'reply_to', type: 'email', value: s.reply_to || '' })),
      field('Copy every message to (optional)', h('input', { name: 'bcc', type: 'email', value: s.bcc || '' }), 'A BCC for your records.'))],
  async (f) => {
    const body = { enabled: f.elements.enabled.checked, host: host.value.trim(), port: Number(port.value || 0), tls: tls.value,
      username: f.elements.username.value.trim(), from_name: f.elements.from_name.value.trim(), from_address: f.elements.from_address.value.trim(),
      reply_to: f.elements.reply_to.value.trim(), bcc: f.elements.bcc.value.trim() };
    if (f.elements.password.value) body.password = f.elements.password.value;
    const saved = await api('PUT', '/settings/email', body);
    f.elements.password.value = '';
    f.elements.password.placeholder = saved.password_set ? 'set (unchanged if empty)' : '';
    notify('E-mail settings saved');
  });

  // A test message, with the server's answer when it fails.
  const to = h('input', { type: 'email', required: true, 'aria-label': 'Send a test to', placeholder: 'you@example.com', value: (ME && ME.email) || '' });
  const result = h('div', { class: 'test-result', 'aria-live': 'polite' });
  const test = h('form', { class: 'with-button' }, to, h('button', { type: 'submit', class: 'ghost' }, icon('send'), 'Send test'));
  test.addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = $('button', test);
    btn.disabled = true;
    result.replaceChildren(h('p', { class: 'muted small' }, 'Sending…'));
    try {
      const r = await api('POST', '/settings/email/test', { to: to.value.trim() });
      result.replaceChildren(r.ok
        ? h('p', { class: 'small st-ok' }, icon('check'), ` Sent. Check ${to.value.trim()} (and its spam folder).`)
        : h('div', { class: 'bbanner bad' }, icon('alert'), h('div', { class: 'bbanner-text' }, h('strong', {}, 'The server said no'), h('pre', { class: 'small' }, r.error))));
    } catch (err) {
      result.replaceChildren(h('p', { class: 'small st-failed' }, err.message));
    } finally { btn.disabled = false; }
  });
  box.replaceChildren(form, h('div', { class: 'card' }, h('h2', {}, 'Send a test'),
    h('p', { class: 'muted small' }, 'Save first: the test uses the saved settings.'), test, result));
}

// ---- Templates ----

async function emailTemplates(box) {
  const list = await api('GET', '/email/templates');
  const groups = new Map();
  for (const t of list) {
    if (!groups.has(t.group || 'Other')) groups.set(t.group || 'Other', []);
    groups.get(t.group || 'Other').push(t);
  }
  const reload = () => emailTemplates(box).catch((e) => box.replaceChildren(loadError(e)));
  if (!list.length) {
    box.replaceChildren(emptyState('mail', 'No templates', 'Features register their messages when they\'re installed.'));
    return;
  }
  box.replaceChildren(h('p', { class: 'muted small' }, 'Every message the panel sends, grouped by what sends it. Change the wording; ' +
    'the layout, your logo and the footer are added around it.'),
  ...[...groups].map(([group, ts]) => h('div', { class: 'card' }, h('h2', {}, group), h('ul', { class: 'tpl-list' }, ts.map((t) => {
    const open = h('button', { type: 'button', class: 'tpl-item' }, h('span', { class: 'tpl-name' }, t.subject || t.name,
      t.customized ? h('span', { class: 'badge' }, 'customized') : null),
    h('span', { class: 'muted small' }, t.description || t.name), h('code', { class: 'small' }, t.name));
    open.addEventListener('click', async () => { if (await editTemplate(t)) reload(); });
    return h('li', {}, open);
  })))));
}

// editTemplate: subject and body, placeholders that insert themselves, and
// a preview rendered by the server with sample data as you type.
function editTemplate(t) {
  const subject = h('input', { name: 'subject', value: t.subject, required: true, spellcheck: 'true' });
  const body = h('textarea', { name: 'body', rows: 14, required: true, spellcheck: 'true' }, t.body);
  let last = body; // where a placeholder goes
  subject.addEventListener('focus', () => { last = subject; });
  body.addEventListener('focus', () => { last = body; });
  const vars = h('div', { class: 'var-chips', role: 'group', 'aria-label': 'Insert a placeholder' },
    [...(t.vars || []), 'Brand.Name', 'Brand.URL', 'PanelURL'].filter((v, i, a) => a.indexOf(v) === i).map((v) => {
      const b = h('button', { type: 'button', class: 'chip var-chip', title: `Insert {{.${v}}}` }, v);
      // Keep the caret where it was: don't take the focus on press.
      b.addEventListener('mousedown', (e) => e.preventDefault());
      b.addEventListener('click', () => {
        last.setRangeText(`{{.${v}}}`, last.selectionStart, last.selectionEnd, 'end');
        last.focus();
        last.dispatchEvent(new Event('input', { bubbles: true }));
      });
      return b;
    }));
  const frame = h('iframe', { class: 'mail-frame', title: 'Preview', sandbox: 'allow-popups allow-popups-to-escape-sandbox', referrerpolicy: 'no-referrer' });
  const previewSubject = h('p', { class: 'preview-subject' });
  const state = h('p', { class: 'muted small', 'aria-live': 'polite' });
  let seq = 0;
  const preview = debounce(async () => {
    const n = ++seq;
    state.textContent = 'Updating the preview…';
    try {
      const r = await api('POST', `/email/templates/${encodeURIComponent(t.name)}/preview`, { subject: subject.value, body: body.value });
      if (n !== seq) return;
      previewSubject.textContent = r.subject;
      frame.src = r.html_url;
      state.textContent = 'Preview with sample data.';
      state.className = 'muted small';
    } catch (e) {
      if (n !== seq) return;
      state.textContent = e.message;
      state.className = 'form-error';
    }
  }, 500);
  subject.addEventListener('input', preview);
  body.addEventListener('input', preview);
  const reset = t.customized ? h('button', { type: 'button', class: 'ghost' }, icon('refresh'), 'Back to the default') : null;
  let dialogCtx = null;
  if (reset) {
    reset.addEventListener('click', async () => {
      if (!await ask(`Go back to the default wording of "${t.name}"? Your changes to it are lost.`, { ok: 'Use the default', danger: true })) return;
      try { await api('DELETE', `/email/templates/${encodeURIComponent(t.name)}`); notify('Template reset'); dialogCtx.close(true); } catch (e) { dialogCtx.error(e); }
    });
  }
  return openDialog({
    title: t.description || t.name, wide: true, ok: 'Save template',
    body: h('div', { class: 'tpl-editor' },
      h('div', { class: 'tpl-form' },
        field('Subject', subject), field('Message', body, 'A blank line starts a paragraph. A line [[Pay now|{{.Invoice.URL}}]] is a button.'),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Placeholders'), vars), reset),
      h('div', { class: 'tpl-preview' }, h('span', { class: 'field-label' }, 'Preview'), previewSubject, frame, state)),
    onOpen(ctx) { dialogCtx = ctx; ctx.dlg.classList.add('xwide'); preview(); },
    async submit() {
      await api('PUT', `/email/templates/${encodeURIComponent(t.name)}`, { subject: subject.value, body: body.value });
      notify('Template saved');
    },
  });
}

// ---- The log ----

let MAIL_STATUS = '';
const MAIL_STATES = { pending: 'Waiting', sent: 'Sent', failed: 'Failed' };

async function emailLog(box) {
  const results = h('div', { 'aria-live': 'polite' });
  let items = [], more = false;
  const admin = canStaff('admin');
  const load = async (reset) => {
    if (reset) items = [];
    results.setAttribute('aria-busy', 'true');
    try {
      const q = new URLSearchParams({ limit: 50 });
      if (MAIL_STATUS) q.set('status', MAIL_STATUS);
      if (!reset && items.length) q.set('before', items[items.length - 1].id);
      const list = await api('GET', '/email/log?' + q);
      items = items.concat(list);
      more = list.length === 50;
      results.replaceChildren(btable(['Time', 'To', 'Subject', 'Status', ''], items.map((m) => [
        fmtTime(m.sent_at || m.created_at), h('td', { class: 'small' }, (m.to || []).join(', ')),
        h('td', {}, m.subject, h('span', { class: 'sub-line' }, m.template || 'written by hand')),
        h('td', {}, bpill('mail-' + m.status, MAIL_STATES[m.status] || m.status),
          m.status !== 'sent' && m.attempts ? h('span', { class: 'sub-line' }, `${m.attempts} attempt${m.attempts === 1 ? '' : 's'}`) : null,
          m.last_error ? h('span', { class: 'sub-line st-failed', title: m.last_error }, m.last_error.slice(0, 80)) : null),
        h('td', { class: 'row-actions' }, linkButton('View', () => openFrame(m.subject, `/api/v1/email/log/${m.id}/html`), 'ghost'),
          admin ? actionButton('Resend', async () => { await api('POST', `/email/log/${m.id}/resend`); notify('Queued again'); load(true); }) : null)]), {
        caption: 'Sent e-mail',
        empty: emptyState('mail', MAIL_STATUS ? 'Nothing here' : 'Nothing sent yet', 'Every message the panel sends shows up here, with the server\'s answer if it failed.'),
      }), more ? h('div', { class: 'actions center-actions' }, actionButton('Load more', () => load(false))) : '');
    } catch (e) {
      results.replaceChildren(loadError(e, () => load(true)));
    } finally { results.removeAttribute('aria-busy'); }
  };
  const chipBox = h('div');
  const drawChips = () => chipBox.replaceChildren(chips('Show messages', [['', 'All'], ['pending', 'Waiting'], ['sent', 'Sent'], ['failed', 'Failed']], MAIL_STATUS,
    (k) => { MAIL_STATUS = k; drawChips(); load(true); }));
  drawChips();
  box.replaceChildren(h('div', { class: 'card' }, h('div', { class: 'toolbar' }, chipBox), results));
  await load(true);
}
