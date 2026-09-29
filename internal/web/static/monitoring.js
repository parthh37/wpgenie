'use strict';

// Monitoring tab: active alerts, history, thresholds and notification
// channels, the Prometheus scrape token. Shares api(), h(), table(),
// showSecret() and showError() with app.js and panels.js.

const KIND_LABELS = { site_down: 'Site down', certificate: 'Certificate', disk: 'Disk', backup: 'Backup' };
const sev = (s) => (s ? h('span', { class: 'sev-' + s }, s) : h('span', { class: 'muted' }, '–'));

async function loadMonitoring() {
  const o = await api('GET', '/monitoring/alerts?limit=200');
  $('#alert-dot').hidden = !o.active.length;
  const checked = o.evaluated_at ? `last checked ${fmtTime(o.evaluated_at)}` : 'not checked yet since the panel started';
  $('#mon-active').replaceChildren(
    o.active.length
      ? table(['Severity', 'Since', 'Kind', 'Target', 'Message'], o.active.map((a) =>
        [h('td', {}, sev(a.severity)), fmtTime(a.since), KIND_LABELS[a.kind] || a.kind, h('td', { class: 'wrap' }, a.target), a.message]))
      : h('p', {}, h('span', { class: 'st-ok' }, 'All clear.'), ` ${o.watched} target(s) watched, ${checked}.`),
    o.active.length ? h('p', { class: 'muted small' }, `${o.watched} target(s) watched, ${checked}.`) : null);
  $('#mon-history').replaceChildren(table(['Time', 'State', 'Severity', 'Target', 'Message'], o.history.map((e) =>
    [fmtTime(e.time), h('td', { class: e.state === 'resolved' ? 'st-ok' : 'st-failed' }, e.state), h('td', {}, sev(e.severity)),
      h('td', { class: 'wrap' }, e.target), e.message])));
  if (isAdmin()) await loadMonitoringSettings();
}

async function loadMonitoringSettings(fresh) {
  const set = fresh || await api('GET', '/monitoring/settings');
  const f = $('#mon-settings');
  for (const k of ['disk_warn_percent', 'disk_critical_percent', 'cert_warn_days', 'cert_critical_days', 'down_after', 'renotify_hours']) {
    f[k].value = set[k];
  }
  const e = set.email;
  f.email_enabled.checked = e.enabled;
  f.email_host.value = e.host || '';
  f.email_port.value = e.port || '';
  f.email_tls.value = e.tls || 'starttls';
  f.email_username.value = e.username || '';
  f.email_password.value = '';
  f.email_password.placeholder = e.password_set ? 'unchanged if empty' : 'none';
  f.email_from.value = e.from || '';
  f.email_to.value = (e.to || []).join(', ');
  $('#mon-webhooks').replaceChildren(...set.webhooks.map(webhookRow));
  showMetricsToken(set.metrics_token);
}

// webhookRow edits one webhook. The stored URL and secret are never sent
// back: empty fields keep them.
function webhookRow(w = {}) {
  const row = h('div', { class: 'grid webhook' });
  row.dataset.id = w.id || '';
  const rm = h('button', { type: 'button', class: 'ghost danger' }, 'Remove');
  rm.addEventListener('click', () => row.remove());
  row.append(
    h('label', {}, 'Name', h('input', { name: 'name', value: w.name || '', placeholder: 'Team chat' })),
    h('label', {}, 'URL', h('input', { name: 'url', type: 'url', autocomplete: 'off',
      placeholder: w.url_hint ? `${w.url_hint} (unchanged if empty)` : 'https://hooks.slack.com/services/…', required: !w.id })),
    h('label', {}, 'Signing secret', h('input', { name: 'secret', type: 'password', autocomplete: 'new-password',
      placeholder: w.secret_set ? 'unchanged if empty' : 'generated if empty' })),
    h('div', { class: 'actions' }, h('label', { class: 'check small' }, h('input', { name: 'enabled', type: 'checkbox', checked: w.enabled !== false }), ' On'), rm));
  return row;
}

function showMetricsToken(info) {
  const rotate = h('button', { class: 'ghost' }, info.set ? 'Replace token' : 'Create token');
  rotate.addEventListener('click', async () => {
    if (info.set && !await ask('Replace the scrape token? Prometheus stops getting metrics until it has the new one.')) return;
    try {
      const r = await api('POST', '/monitoring/metrics-token');
      showSecret('Prometheus scrape token', [r.token]);
      await loadMonitoringSettings();
    } catch (err) { showError(err); }
  });
  const scheme = location.protocol.replace(':', '');
  $('#mon-metrics').replaceChildren(
    h('p', { class: 'small' }, info.set ? `Scrape token created ${fmtTime(info.created_at)}.` : 'No scrape token yet: /metrics answers 401 to everyone.'),
    h('pre', { class: 'small' }, [
      'scrape_configs:',
      '  - job_name: wpgenie',
      `    scheme: ${scheme}`,
      '    static_configs:',
      `      - targets: ['${location.host}']`,
      '    authorization:',
      '      type: Bearer',
      '      credentials_file: /etc/prometheus/wpgenie-token',
    ].join('\n')),
    h('p', { class: 'muted small' }, 'Scrape from this server (the daemon listens on loopback), through an SSH tunnel, or via the panel domain if you publish it.'),
    h('div', { class: 'actions' }, rotate));
}

$('#mon-add-webhook').addEventListener('click', () => $('#mon-webhooks').append(webhookRow()));

$('#mon-settings').addEventListener('submit', async (ev) => {
  ev.preventDefault();
  const f = ev.target;
  const num = (k) => Number(f[k].value);
  const body = {
    disk_warn_percent: num('disk_warn_percent'), disk_critical_percent: num('disk_critical_percent'),
    cert_warn_days: num('cert_warn_days'), cert_critical_days: num('cert_critical_days'),
    down_after: num('down_after'), renotify_hours: num('renotify_hours'),
    email: {
      enabled: f.email_enabled.checked, host: f.email_host.value.trim(), port: Number(f.email_port.value || 0), tls: f.email_tls.value,
      username: f.email_username.value, password: f.email_password.value, from: f.email_from.value.trim(), to: splitList(f.email_to.value),
    },
    webhooks: [...document.querySelectorAll('#mon-webhooks .webhook')].map((row) => ({
      id: row.dataset.id, name: $('[name=name]', row).value, enabled: $('[name=enabled]', row).checked,
      url: $('[name=url]', row).value.trim(), secret: $('[name=secret]', row).value,
    })),
  };
  try {
    const saved = await api('PUT', '/monitoring/settings', body);
    const gen = saved.generated_secrets || {};
    const ids = Object.keys(gen);
    if (ids.length) {
      showSecret('Webhook signing secrets', ids.map((id) => {
        const w = saved.webhooks.find((x) => x.id === id);
        return `${(w && w.name) || id}: ${gen[id]}`;
      }));
    }
    await loadMonitoringSettings(saved);
  } catch (err) { showError(err); }
});

$('#mon-test').addEventListener('click', async (ev) => {
  const btn = ev.target;
  btn.disabled = true;
  const box = $('#mon-test-results');
  box.replaceChildren(h('p', { class: 'muted small' }, 'Sending… (save your changes first: the test uses the saved settings)'));
  try {
    const res = await api('POST', '/monitoring/test');
    box.replaceChildren(res.length
      ? table(['Channel', 'Result'], res.map((r) => [r.channel, h('td', {}, r.ok ? status('ok') : h('span', { class: 'st-failed' }, r.error))]))
      : h('p', { class: 'muted small' }, 'No channel is configured.'));
  } catch (err) { box.replaceChildren(); showError(err); }
  finally { btn.disabled = false; }
});

loaders.monitoring = loadMonitoring;
