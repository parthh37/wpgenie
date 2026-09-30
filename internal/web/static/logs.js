'use strict';
// Logs tab: shipping logs to S3-compatible storage (internal/logship).
// Shares $, h(), api(), table(), ask(), notify(), showError(), icon(),
// svgEl(), fmtBytes(), fmtNum() and fmtTime() with app.js and ux.js.
// Staff see the status; admins change the settings and read the archive.
// The secret key is write-only: the API never returns it.

loaders.logs = loadLogs;

// What each kind of log is, for people who don't read logs every day.
const LOG_TYPES = {
  access: { icon: 'globe', title: 'Visitor requests', desc: 'Every request to your sites: the page, the answer, how long it took and who asked (Caddy\'s access log).' },
  php_errors: { icon: 'bug', title: 'PHP errors', desc: 'Warnings and errors from WordPress, plugins and themes, for each site.' },
  waf: { icon: 'shield-alert', title: 'Firewall matches', desc: 'Requests the web application firewall flagged or blocked, and the rules they matched.' },
  security: { icon: 'shield', title: 'Security events', desc: 'Blocks, bans and attacks the shield saw (the Security page\'s log, kept for good).' },
  audit: { icon: 'clipboard', title: 'Panel activity', desc: 'Who changed what in this panel, and from where.' },
  jobs: { icon: 'list-checks', title: 'Background jobs', desc: 'Backups, restores, clones and updates: when they ran and how they went.' },
  account_events: { icon: 'building', title: 'Account activity', desc: 'Plan changes, suspensions and billing events of your clients\' accounts.' },
  email: { icon: 'mail', title: 'E-mails sent', desc: 'Who the panel e-mailed, about what, and whether it arrived (never the message itself).' },
  mail: { icon: 'inbox', title: 'Mail server', desc: 'The mail server\'s own log: deliveries, rejections and sign-ins.' },
  daemon: { icon: 'terminal', title: 'WPGenie itself', desc: 'The panel\'s own log: what it did, and any trouble it ran into.' },
  containers: { icon: 'box', title: 'Container output', desc: 'Everything WPGenie\'s containers print. Detailed and noisy: turn it on to troubleshoot.' },
};

// Where logs can go. {region} in an endpoint is filled in from the region.
const LOG_PROVIDERS = {
  aws: { name: 'Amazon S3', endpoint: 'https://s3.{region}.amazonaws.com', region: 'us-east-1', pathStyle: false,
    help: 'Create a bucket, then an IAM user whose access key may put, list, get and delete objects in it.' },
  r2: { name: 'Cloudflare R2', endpoint: 'https://ACCOUNT_ID.r2.cloudflarestorage.com', region: 'auto', pathStyle: true,
    help: 'In R2, create an API token with "Object Read & Write" on the bucket. Replace ACCOUNT_ID with your account ID (on the R2 overview page).' },
  b2: { name: 'Backblaze B2', endpoint: 'https://s3.{region}.backblazeb2.com', region: 'us-west-004', pathStyle: true,
    help: 'The bucket\'s page shows its endpoint (the region is in it). Create an application key allowed to use the bucket.' },
  wasabi: { name: 'Wasabi', endpoint: 'https://s3.{region}.wasabisys.com', region: 'us-east-1', pathStyle: false,
    help: 'Create an access key under Access Keys; the region is the bucket\'s (shown in its settings).' },
  spaces: { name: 'DigitalOcean Spaces', endpoint: 'https://{region}.digitaloceanspaces.com', region: 'nyc3', pathStyle: false,
    help: 'Create a Spaces access key limited to the bucket. The region is the Space\'s datacenter (nyc3, fra1, sgp1…).' },
  minio: { name: 'MinIO', endpoint: 'https://minio.example.com:9000', region: 'us-east-1', pathStyle: true,
    help: 'Your MinIO server\'s API address (not its console, usually port 9000) and an access key that may read and write the bucket.' },
  custom: { name: 'Other S3-compatible', endpoint: 'https://s3.example.com', region: '', pathStyle: true,
    help: 'Any storage that speaks the S3 API: its endpoint, region (if it has one) and an access key.' },
};

const LOG_RETENTION = [[30, '30 days'], [90, '3 months'], [180, '6 months'], [365, '1 year'], [730, '2 years'], [1825, '5 years'], [0, 'Forever']];
const LOG_HEALTH = {
  ok: { icon: 'check', label: 'Shipping' }, warning: { icon: 'alert', label: 'Needs a look' },
  error: { icon: 'alert', label: 'Not shipping' }, off: { icon: 'hard-drive', label: 'Off' },
};

// LOGS is what was last loaded: settings (admins), status.
const LOGS = { settings: null, status: null, archive: { type: 'access', date: '', server: '' } };

async function loadLogs() {
  const [st, set] = await Promise.all([api('GET', '/logs/status'), isAdmin() ? api('GET', '/logs/settings') : null]);
  LOGS.status = st;
  LOGS.settings = set;
  renderLogs();
}

function renderLogs() {
  const st = LOGS.status, set = LOGS.settings;
  const box = $('#logs');
  box.replaceChildren(
    logsHero(st, set),
    logTypesCard(st, set),
    set ? logDestinationCard(set) : null,
    set ? logRetentionCard(set) : null,
    set ? logArchiveCard(st, set) : null);
}

const logAgo = (t) => {
  const s = Math.round((Date.now() - new Date(t)) / 1000);
  if (s < 60) return 'just now';
  if (s < 3600) return `${Math.round(s / 60)} min ago`;
  if (s < 86400) return `${Math.round(s / 3600)} h ago`;
  return fmtTime(t);
};

// ---- Status ----

function logsHero(st, set) {
  const hl = LOG_HEALTH[st.health] || LOG_HEALTH.warning;
  const d = st.destination;
  const where = d.bucket ? `${(LOG_PROVIDERS[d.provider] || LOG_PROVIDERS.custom).name} · ${d.bucket}/${d.prefix || ''}` : 'no destination yet';
  const actions = h('div', { class: 'actions' });
  const refresh = h('button', { type: 'button', class: 'ghost' }, icon('refresh'), 'Refresh');
  refresh.addEventListener('click', () => loadLogs().catch(showError));
  actions.append(refresh);
  if (set) {
    const toggle = h('button', { type: 'button', class: st.enabled ? 'ghost danger' : '' }, st.enabled ? 'Turn off' : 'Turn on shipping');
    toggle.addEventListener('click', () => setLogShipping(!st.enabled, toggle));
    actions.append(toggle);
  }
  const spoolPct = st.spool.cap ? Math.min(100, (st.spool.bytes / st.spool.cap) * 100) : 0;
  const facts = st.enabled ? h('div', { class: 'logs-facts' },
    logFact('Last upload', st.last_upload ? logAgo(st.last_upload) : 'None yet',
      st.last_upload ? fmtTime(st.last_upload) : 'objects are written every few minutes'),
    logFact('Uploaded', `${fmtNum(st.sent_events)} lines`, st.sent_bytes ? `${fmtBytes(st.sent_bytes)} since the shipper started` : 'since the shipper started'),
    h('div', { class: 'logs-fact' }, h('span', { class: 'tile-k' }, 'Waiting on this server'),
      h('span', { class: 'logs-fact-v' }, fmtBytes(st.spool.bytes)),
      h('meter', { min: 0, max: 100, value: spoolPct, low: 50, high: 80, optimum: 0, 'aria-label': 'Space for waiting logs used' }),
      h('span', { class: 'tile-s' }, `of ${fmtBytes(st.spool.cap)} allowed` + (st.dropped_today ? ` · ${fmtNum(st.dropped_today)} dropped today` : '') +
        (st.buffer_bytes ? ` · ${fmtBytes(st.buffer_bytes)} more in the shipper's buffer (up to 256 MB)` : ''))),
    logFact('Shipper', st.shipper.state ? st.shipper.state[0].toUpperCase() + st.shipper.state.slice(1) : 'Starting',
      `Vector ${st.shipper.version || (st.shipper.image.split('@')[0].split(':')[1] || '').replace('-alpine', '')}`)) : null;
  const servers = (st.servers || []).length ? h('div', { class: 'logs-servers' },
    h('span', { class: 'muted small' }, 'Every server ships its own logs:'),
    h('span', { class: `chip logs-chip ${st.health}` }, `${st.server} (this panel)`),
    ...st.servers.map((s) => h('span', { class: `chip logs-chip ${s.status ? s.status.health : 'error'}`, title: s.status ? s.status.health_message : s.error },
      s.name || s.id, s.status ? '' : ' · unreachable'))) : null;
  return h('div', { class: `card logs-hero ${st.health}` },
    h('div', { class: 'logs-hero-head' },
      h('span', { class: 'logs-hero-icon', 'aria-hidden': 'true' }, icon(hl.icon)),
      h('div', { class: 'logs-hero-text' },
        h('h2', {}, st.enabled ? `${hl.label} to ${where}` : 'Log shipping is off'),
        h('p', { class: 'small', role: st.health === 'error' ? 'alert' : null }, st.health_message),
        !st.enabled ? h('p', { class: 'muted small' }, 'Turn it on to keep every log in your own storage (Amazon S3, Cloudflare R2, Backblaze B2…) ' +
          'instead of on this server\'s disk: nothing fills it up, and you can look back months later.') : null),
      actions),
    facts, servers,
    st.retention && st.retention.error ? h('p', { class: 'st-failed small' }, `Deleting old archives failed: ${st.retention.error}`) : null,
    st.export_error ? h('p', { class: 'st-warning small' }, st.export_error) : null);
}

function logFact(k, v, s) {
  return h('div', { class: 'logs-fact' }, h('span', { class: 'tile-k' }, k), h('span', { class: 'logs-fact-v' }, v), h('span', { class: 'tile-s' }, s));
}

async function setLogShipping(on, btn) {
  const set = LOGS.settings;
  if (on) {
    const d = set.destination;
    if (!d.bucket || !d.endpoint || !d.access_key_id || !d.secret_key_set) {
      notify('First tell WPGenie where logs go, then "Save and turn on shipping".');
      const f = $('#logs-dest');
      f.scrollIntoView({ behavior: 'smooth', block: 'start' });
      f.endpoint.focus({ preventScroll: true });
      return;
    }
  } else if (!await ask('Turn off log shipping? Logs stay on this server only from now on (and old ones are rotated away as before). ' +
    'What is already in the bucket stays there.')) return;
  btn.disabled = true;
  try {
    await saveLogSettings({ enabled: on });
    notify(on ? 'Log shipping is on. The shipper starts in a moment.' : 'Log shipping is off.');
  } catch (e) { showError(e); btn.disabled = false; }
}

// logSettingsBody is what PUT /logs/settings takes: the settings as loaded,
// with changes (the secret only when one was typed).
function logSettingsBody(set, patch = {}) {
  const d = { ...set.destination, ...(patch.destination || {}) };
  const body = {
    enabled: set.enabled, types: { ...set.types }, compression: set.compression, batch_max_mb: set.batch_max_mb,
    batch_max_seconds: set.batch_max_seconds, spool_cap_mb: set.spool_cap_mb, archive_retention_days: set.archive_retention_days,
    local_access_logs: set.local_access_logs, container_log_mb: set.container_log_mb,
    ...patch,
    destination: { provider: d.provider, endpoint: d.endpoint, region: d.region, bucket: d.bucket, prefix: d.prefix,
      access_key_id: d.access_key_id, secret_key: d.secret_key || '', path_style: d.path_style },
  };
  if (patch.types) body.types = { ...set.types, ...patch.types };
  return body;
}

async function saveLogSettings(patch) {
  LOGS.settings = await api('PUT', '/logs/settings', logSettingsBody(LOGS.settings, patch));
  LOGS.status = await api('GET', '/logs/status');
  renderLogs();
}

// ---- What's collected ----

function logTypesCard(st, set) {
  const cards = st.types.map((t) => {
    const info = LOG_TYPES[t.name] || { icon: 'file', title: t.name, desc: '' };
    const on = t.enabled && st.enabled;
    const sw = h('input', { type: 'checkbox', role: 'switch', class: 'logs-switch', checked: t.enabled,
      disabled: !set || !t.available, 'aria-label': `Ship ${info.title}` });
    sw.addEventListener('change', async () => {
      sw.disabled = true;
      try {
        await saveLogSettings({ types: { [t.name]: sw.checked } });
        notify(`${info.title}: ${sw.checked ? 'shipped' : 'not shipped any more'}`);
      } catch (e) { showError(e); sw.checked = !sw.checked; sw.disabled = false; }
    });
    const today = t.today;
    return h('article', { class: `logs-type${on ? ' on' : ''}${t.available ? '' : ' unavailable'}` },
      h('div', { class: 'logs-type-head' },
        h('span', { class: 'logs-type-icon', 'aria-hidden': 'true' }, icon(info.icon)),
        h('h3', {}, info.title),
        set ? sw : h('span', { class: `badge${on ? ' on' : ''}` }, on ? 'shipped' : 'not shipped')),
      h('p', { class: 'muted small' }, info.desc),
      !t.available ? h('p', { class: 'small logs-note' }, t.name === 'mail' ? 'No mail server runs on this server.'
        : t.name === 'containers' ? 'Needs Docker\'s default json-file logging.' : 'Not available on this server.') : null,
      h('div', { class: 'logs-type-foot' },
        h('div', {}, h('span', { class: 'tile-k' }, 'Today'),
          h('span', { class: 'logs-type-v' }, today.events ? `${fmtNum(today.events)} lines` : 'Nothing yet'),
          today.events ? h('span', { class: 'tile-s' }, fmtBytes(today.bytes)) : null,
          today.dropped ? h('span', { class: 'st-failed small' }, `${fmtNum(today.dropped)} dropped`) : null),
        logVolumeBars(t.history, info.title)));
  });
  return h('div', { class: 'card' },
    h('h2', {}, 'What\'s collected'),
    h('p', { class: 'muted small' }, 'Each kind of log can be shipped or not. The numbers are this server\'s: what was collected today, ' +
      'and the last two weeks.'),
    h('div', { class: 'logs-types' }, cards));
}

// logVolumeBars is a small bar chart of a type's last days (bytes), with a
// text alternative.
function logVolumeBars(history, title) {
  const w = 112, hgt = 28, n = history.length, gap = 2;
  const max = Math.max(1, ...history.map((d) => d.bytes));
  const bw = (w - gap * (n - 1)) / n;
  const total = history.reduce((a, d) => a + d.bytes, 0);
  const svg = svgEl('svg', { viewBox: `0 0 ${w} ${hgt}`, class: 'logs-bars', role: 'img',
    'aria-label': `${title}, last ${n} days: ${fmtBytes(total)} in total` });
  history.forEach((d, i) => {
    const bh = d.bytes ? Math.max(2, (d.bytes / max) * (hgt - 2)) : 1;
    const r = svgEl('rect', { x: (i * (bw + gap)).toFixed(1), y: (hgt - bh).toFixed(1), width: bw.toFixed(1), height: bh.toFixed(1),
      rx: 1, class: d.bytes ? (i === n - 1 ? 'today' : '') : 'empty' });
    const tip = svgEl('title');
    tip.textContent = `${d.day}: ${fmtNum(d.events)} lines, ${fmtBytes(d.bytes)}` + (d.dropped ? `, ${fmtNum(d.dropped)} dropped` : '');
    r.append(tip);
    svg.append(r);
  });
  return svg;
}

// ---- Destination ----

function logDestinationCard(set) {
  const d = set.destination;
  const f = h('form', { id: 'logs-dest', class: 'card', autocomplete: 'off' });
  const provider = d.provider || 'aws';
  const choices = h('div', { class: 'choices logs-providers', role: 'radiogroup', 'aria-label': 'Storage provider' },
    Object.entries(LOG_PROVIDERS).map(([id, p]) => h('label', { class: 'choice' },
      h('input', { type: 'radio', name: 'provider', value: id, checked: id === provider }),
      h('span', {}, h('strong', {}, p.name)))));
  const help = h('p', { class: 'muted small logs-provider-help' });
  const field = (label, name, attrs = {}, hint) => h('label', {}, label, hint ? h('span', { class: 'muted' }, ` ${hint}`) : null,
    h('input', { name, ...attrs }));
  const grid = h('div', { class: 'grid' },
    field('Endpoint', 'endpoint', { value: d.endpoint || '', inputmode: 'url', spellcheck: 'false' }),
    field('Region', 'region', { value: d.region || '', spellcheck: 'false' }, '(if the storage has one)'),
    field('Bucket', 'bucket', { value: d.bucket || '', spellcheck: 'false', placeholder: 'my-logs' }),
    field('Folder in the bucket', 'prefix', { value: d.prefix || '', spellcheck: 'false', placeholder: 'logs/' }, '(optional)'),
    field('Access key ID', 'access_key_id', { value: d.access_key_id || '', spellcheck: 'false' }),
    field('Secret access key', 'secret_key', { type: 'password', autocomplete: 'new-password',
      placeholder: d.secret_key_set ? 'saved (unchanged if empty)' : 'the key\'s secret' }));
  const adv = h('details', { class: 'advanced' }, h('summary', {}, 'Advanced'),
    h('label', { class: 'check small' }, h('input', { type: 'checkbox', name: 'path_style', checked: !!d.path_style }),
      ' Path-style addresses (the bucket in the URL\'s path: MinIO and most self-hosted storage)'));
  const result = h('div', { class: 'logs-test-result', 'aria-live': 'polite' });
  const test = h('button', { type: 'button', class: 'ghost' }, icon('zap'), 'Test connection');
  // While shipping is off, saving can also turn it on (the usual first run).
  const save = h('button', { type: 'submit', class: set.enabled ? '' : 'ghost' }, 'Save destination');
  const saveOn = set.enabled ? null : h('button', { type: 'submit', value: 'on' }, 'Save and turn on shipping');
  f.append(h('h2', {}, icon('cloud'), ' Where logs go'),
    h('p', { class: 'muted small' }, 'Any S3-compatible storage. Logs are grouped by server, kind and day: ',
      h('code', {}, `${d.prefix || ''}${LOGS.settings.server}/access/${new Date().toISOString().slice(0, 10).replaceAll('-', '/')}/14-….log.gz`), '. Test the connection before saving: ' +
      'it writes a small file and deletes it again.'),
    choices, help, grid, adv, result, h('div', { class: 'actions' }, test, save, saveOn));

  // An endpoint (or region) filled in for another provider is replaced
  // when the provider changes; one typed for this one is left alone.
  const presetHost = /(amazonaws\.com|r2\.cloudflarestorage\.com|backblazeb2\.com|wasabisys\.com|digitaloceanspaces\.com)$/i;
  const syncProvider = (fresh) => {
    const p = LOG_PROVIDERS[f.provider.value] || LOG_PROVIDERS.custom;
    help.textContent = p.help;
    f.endpoint.placeholder = p.endpoint.replace('{region}', p.region || 'region');
    f.region.placeholder = p.region || 'none';
    if (!fresh) return;
    let host = '';
    try { host = new URL(f.endpoint.value).hostname; } catch (e) { /* empty or not a URL yet */ }
    if (presetHost.test(host)) f.endpoint.dataset.auto = f.region.dataset.auto = '1';
    if (!f.region.value || f.region.dataset.auto === '1') { f.region.value = p.region; f.region.dataset.auto = '1'; }
    if (!f.endpoint.value || f.endpoint.dataset.auto === '1') {
      f.endpoint.value = p.endpoint.includes('ACCOUNT_ID') || p.endpoint.includes('example.com') ? '' : p.endpoint.replace('{region}', f.region.value);
      f.endpoint.dataset.auto = '1';
    }
    f.path_style.checked = p.pathStyle;
  };
  f.addEventListener('change', (e) => { if (e.target.name === 'provider') syncProvider(true); });
  f.region.addEventListener('input', () => {
    f.region.dataset.auto = '';
    const p = LOG_PROVIDERS[f.provider.value];
    if (p && f.endpoint.dataset.auto === '1' && p.endpoint.includes('{region}')) f.endpoint.value = p.endpoint.replace('{region}', f.region.value.trim());
  });
  f.endpoint.addEventListener('input', () => { f.endpoint.dataset.auto = ''; });
  syncProvider(!d.endpoint);

  const form = () => ({
    provider: f.provider.value, endpoint: f.endpoint.value.trim(), region: f.region.value.trim(), bucket: f.bucket.value.trim(),
    prefix: f.prefix.value.trim(), access_key_id: f.access_key_id.value.trim(), secret_key: f.secret_key.value, path_style: f.path_style.checked,
  });
  test.addEventListener('click', async () => {
    test.disabled = true;
    result.replaceChildren(h('p', { class: 'muted small' }, 'Writing a test file to the bucket…'));
    try {
      const r = await api('POST', '/logs/test', form());
      result.replaceChildren(r.ok
        ? h('p', { class: 'logs-ok small' }, icon('check'), 'It works: WPGenie can write to this bucket and delete from it.')
        : h('div', { class: 'logs-bad small', role: 'alert' }, icon('alert'), h('div', {}, h('strong', {}, 'The storage said no.'), h('p', {}, r.error),
          h('p', { class: 'muted' }, logTestHint(r.error)))));
    } catch (e) {
      result.replaceChildren(h('div', { class: 'logs-bad small', role: 'alert' }, icon('alert'), h('p', {}, e.message)));
    } finally { test.disabled = false; }
  });
  f.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const on = ev.submitter && ev.submitter.value === 'on';
    save.disabled = true;
    if (saveOn) saveOn.disabled = true;
    try {
      await saveLogSettings(on ? { destination: form(), enabled: true } : { destination: form() });
      notify(on ? 'Log shipping is on. The shipper starts in a moment.'
        : set.enabled ? 'Destination saved.' : 'Destination saved. Turn shipping on when you\'re ready.');
    } catch (e) {
      showError(e);
      save.disabled = false;
      if (saveOn) saveOn.disabled = false;
    }
  });
  return f;
}

// logTestHint turns the usual storage refusals into what to check.
function logTestHint(msg) {
  if (/403|AccessDenied|Forbidden|SignatureDoesNotMatch|InvalidAccessKeyId/i.test(msg)) return 'Check the access key and its secret, and that the key may write to this bucket.';
  if (/NoSuchBucket|404/i.test(msg)) return 'Check the bucket\'s name (and that it exists in this region).';
  if (/PermanentRedirect|region|AuthorizationHeaderMalformed/i.test(msg)) return 'The region doesn\'t match the bucket\'s: check it (and the endpoint).';
  if (/no such host|dial|timeout|connection refused|certificate|x509/i.test(msg)) return 'The endpoint can\'t be reached from this server: check its address.';
  return 'Check the endpoint, region, bucket and key.';
}

// ---- Retention ----

function logRetentionCard(set) {
  const f = h('form', { class: 'card logs-retention' });
  const days = h('select', { name: 'archive_retention_days' }, LOG_RETENTION.map(([v, l]) =>
    h('option', { value: v, selected: v === set.archive_retention_days }, l)));
  if (!LOG_RETENTION.some(([v]) => v === set.archive_retention_days)) {
    days.append(h('option', { value: set.archive_retention_days, selected: true }, `${set.archive_retention_days} days`));
  }
  const num = (name, min, max, step = 1) => h('input', { type: 'number', name, min, max, step, value: set[name], required: true });
  f.append(h('h2', {}, 'How long logs are kept'),
    h('div', { class: 'logs-keep' },
      h('div', { class: 'logs-keep-row' }, h('span', { class: 'logs-type-icon', 'aria-hidden': 'true' }, icon('cloud')),
        h('label', {}, 'Keep logs in the bucket for', days),
        h('p', { class: 'muted small' }, 'Older ones are deleted from the bucket every night. "Forever" never deletes anything.')),
      h('div', { class: 'logs-keep-row' }, h('span', { class: 'logs-type-icon', 'aria-hidden': 'true' }, icon('hard-drive')),
        h('label', {}, 'Old access log files kept on this server', num('local_access_logs', 1, 10)),
        h('p', { class: 'muted small logs-keep-access' })),
      h('div', { class: 'logs-keep-row' }, h('span', { class: 'logs-type-icon', 'aria-hidden': 'true' }, icon('box')),
        h('label', {}, 'Each container\'s own log, at most (MB)', num('container_log_mb', 1, 1024)),
        h('p', { class: 'muted small' }, 'Applies to containers started from now on, while logs are shipped (two files of this size).'))),
    h('details', { class: 'advanced' }, h('summary', {}, 'Advanced'),
      h('div', { class: 'grid' },
        h('label', {}, 'Compression', h('select', { name: 'compression' },
          h('option', { value: 'gzip', selected: set.compression === 'gzip' }, 'gzip (readable everywhere, and here)'),
          h('option', { value: 'zstd', selected: set.compression === 'zstd' }, 'zstd (smaller, download to read)'))),
        h('label', {}, 'Write a file every (seconds)', num('batch_max_seconds', 30, 3600)),
        h('label', {}, '…or at this size (MB, before compression)', num('batch_max_mb', 1, 16)),
        h('label', {}, 'Space for logs waiting on this server (MB)', num('spool_cap_mb', 64, 102400))),
      h('p', { class: 'muted small' }, 'When the storage can\'t be reached, logs wait on this server; past this space the oldest are dropped (and counted). The shipper\'s own buffer (up to 256 MB) comes on top.')),
    h('div', { class: 'actions' }, h('button', { type: 'submit' }, 'Save')));
  const keepHelp = () => {
    const n = Number(f.local_access_logs.value) || 0;
    $('.logs-keep-access', f).textContent = `Keep ${n} old access log file${n === 1 ? '' : 's'} (up to 100 MB each, about ` +
      `${fmtBytes(n * 100 * 1024 * 1024)}) on this server while logs are shipped; 10 are kept when shipping is off.`;
  };
  f.local_access_logs.addEventListener('input', keepHelp);
  keepHelp();
  f.addEventListener('submit', async (ev) => {
    ev.preventDefault();
    const n = (k) => Number(f[k].value);
    try {
      await saveLogSettings({
        archive_retention_days: n('archive_retention_days'), local_access_logs: n('local_access_logs'), container_log_mb: n('container_log_mb'),
        compression: f.compression.value, batch_max_seconds: n('batch_max_seconds'), batch_max_mb: n('batch_max_mb'), spool_cap_mb: n('spool_cap_mb'),
      });
      notify('Retention saved.');
    } catch (e) { showError(e); }
  });
  return f;
}

// ---- Archive browser ----

function logArchiveCard(st, set) {
  const card = h('div', { class: 'card logs-archive' });
  const a = LOGS.archive;
  if (!a.date) a.date = new Date().toISOString().slice(0, 10);
  if (!a.server) a.server = set.server;
  const servers = [st.server, ...(st.servers || []).map((s) => s.id)];
  const f = h('form', { class: 'controls' },
    servers.length > 1 ? h('label', {}, 'Server', h('select', { name: 'server' }, servers.map((s) => h('option', { value: s, selected: s === a.server }, s)))) : null,
    h('label', {}, 'Kind of log', h('select', { name: 'type' }, st.types.map((t) =>
      h('option', { value: t.name, selected: t.name === a.type }, (LOG_TYPES[t.name] || { title: t.name }).title)))),
    h('label', {}, 'Day (UTC)', h('input', { type: 'date', name: 'date', value: a.date, max: new Date().toISOString().slice(0, 10), required: true })),
    h('button', { type: 'submit' }, icon('search'), 'Show files'));
  const list = h('div', { class: 'logs-archive-list', 'aria-live': 'polite' });
  const viewer = h('div', { class: 'logs-viewer-box' });
  card.append(h('h2', {}, 'Archive'),
    h('p', { class: 'muted small' }, 'What\'s in the bucket: pick a kind of log and a day, then view a file here or download it.'),
    f, list, viewer);
  const show = async () => {
    list.replaceChildren(h('p', { class: 'muted small' }, 'Listing the bucket…'));
    viewer.replaceChildren();
    try {
      const q = new URLSearchParams({ type: a.type, date: a.date, server: a.server });
      const res = await api('GET', `/logs/archives?${q}`);
      list.replaceChildren(res.objects.length
        ? table(['Hour (UTC)', 'File', 'Size', 'Uploaded', ''], res.objects.map((o) => {
          const view = h('button', { type: 'button', class: 'ghost' }, icon('eye'), 'View');
          view.addEventListener('click', () => viewLogArchive(o, viewer));
          const dl = h('a', { class: 'button ghost', href: `/api/v1/logs/archives/object?${new URLSearchParams({ key: o.key, download: '1' })}`,
            download: '' }, icon('download'), 'Download');
          return [o.name.slice(0, 2) + ':00', h('td', { class: 'wrap' }, o.name), fmtBytes(o.size), o.modified ? fmtTime(o.modified) : '–',
            h('td', {}, h('div', { class: 'actions' }, view, dl))];
        }))
        : h('div', { class: 'empty logs-empty' }, h('span', { class: 'empty-icon' }, icon('archive')),
          h('h2', {}, 'No files that day'),
          h('p', { class: 'muted small' }, st.enabled ? 'Nothing of this kind was uploaded that day (files appear a few minutes after the logs).'
            : 'Log shipping is off: nothing is uploaded.')));
    } catch (e) {
      list.replaceChildren(h('p', { class: 'st-failed small', role: 'alert' }, e.message));
    }
  };
  f.addEventListener('submit', (ev) => {
    ev.preventDefault();
    a.type = f.type.value;
    a.date = f.date.value;
    if (f.server) a.server = f.server.value;
    show();
  });
  return card;
}

// viewLogArchive shows a file's lines (up to 20 MB) with a filter.
async function viewLogArchive(o, box) {
  box.replaceChildren(h('p', { class: 'muted small' }, `Fetching ${o.name}…`));
  try {
    const res = await fetch(`/api/v1/logs/archives/object?${new URLSearchParams({ key: o.key })}`,
      { credentials: 'same-origin', headers: { 'X-Requested-With': 'wpgenie' } });
    if (!res.ok) throw new Error((await res.json().catch(() => ({}))).error || res.statusText);
    if ((res.headers.get('Content-Type') || '').includes('zstd')) {
      box.replaceChildren(h('p', { class: 'small' }, 'This file is compressed with zstd: download it to read it (zstd -d).'));
      return;
    }
    const lines = (await res.text()).split('\n').filter(Boolean);
    const pre = h('pre', { class: 'logs-viewer', tabindex: 0, 'aria-label': `Contents of ${o.name}` });
    const count = h('span', { class: 'muted small' });
    const filter = h('input', { type: 'search', placeholder: 'Filter lines (e.g. a site, an IP, "status":500)', 'aria-label': 'Filter lines' });
    const MAX = 2000;
    const draw = () => {
      const q = filter.value.trim().toLowerCase();
      const shown = q ? lines.filter((l) => l.toLowerCase().includes(q)) : lines;
      pre.replaceChildren(...shown.slice(0, MAX).map(logPrettyLine));
      count.textContent = `${fmtNum(shown.length)} of ${fmtNum(lines.length)} lines` + (shown.length > MAX ? ` (first ${fmtNum(MAX)} shown)` : '');
    };
    filter.addEventListener('input', draw);
    const close = h('button', { type: 'button', class: 'ghost' }, icon('x'), 'Close');
    close.addEventListener('click', () => box.replaceChildren());
    box.replaceChildren(h('div', { class: 'logs-viewer-head' }, h('strong', {}, o.name), count, close), filter, pre);
    draw();
    box.scrollIntoView({ behavior: 'smooth', block: 'nearest' });
  } catch (e) {
    box.replaceChildren(h('p', { class: 'st-failed small', role: 'alert' }, e.message));
  }
}

// logPrettyLine is a line of the file: its time and gist when it's JSON
// (an access log line: method, address and status), then the line itself.
function logPrettyLine(l) {
  let gist = '', when = '';
  try {
    const o = JSON.parse(l);
    const t = o.time || o.ts || o.timestamp;
    when = typeof t === 'number' ? new Date(t * 1000).toISOString() : String(t || '');
    const r = o.request;
    gist = r && r.method ? `${r.method} ${r.host || ''}${r.uri || ''} → ${o.status}` + (o.site ? ` (${o.site})` : '')
      : o.msg || o.message || o.reason || o.action || o.subject || o.kind || '';
  } catch (e) { /* not JSON: the line as it is */ }
  return h('span', { class: 'logs-line' },
    gist || when ? h('span', { class: 'logs-gist' }, `${when.replace('T', ' ').replace(/\.\d+Z$/, 'Z')}  ${gist}`.trim()) : null,
    h('span', { class: 'logs-raw' }, l));
}
