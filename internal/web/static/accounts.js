'use strict';
// Accounts, plans, billing settings and API tokens. Shares api(), h(),
// table(), status(), showError(), showSecret(), fmtBytes() and fmtTime()
// with app.js and panels.js. Everything shown here is also enforced by the
// server; hiding a button is only a convenience.

const FEATURES = ['staging', 'backups', 'sftp', 'phpmyadmin', 'certificates', 'cdn', 'smtp'];
const EVENTS = ['account.created', 'account.suspended', 'account.unsuspended', 'account.terminated',
  'account.payment_failed', 'plan.changed', 'usage.threshold', 'site.created', 'site.deleted'];

loaders.accounts = loadAccounts;
loaders.plans = loadPlans;
loaders.billing = loadBilling;

const limitText = (n, unit) => (n ? `${n}${unit}` : 'unlimited');

// usageRow is a label, a meter against the limit (none when unlimited),
// and the numbers.
function usageRow(label, used, limit, fmt) {
  const text = limit ? `${fmt(used)} of ${fmt(limit)}` : `${fmt(used)} (unlimited)`;
  const meter = limit
    ? h('meter', { min: 0, max: limit, value: Math.min(used, limit), low: limit * 0.8, high: limit * 0.99, optimum: 0 })
    : h('span');
  return h('div', { class: 'usage-row' }, h('span', { class: 'small' }, label), meter, h('span', { class: 'small muted' }, text));
}

function usageBars(u) {
  return h('div', {},
    h('p', { class: 'muted small' }, `${new Date(u.month_start).toLocaleDateString([], { month: 'long', year: 'numeric', timeZone: 'UTC' })}` +
      (u.includes_customers ? ' · totals include every customer account' : '')),
    usageRow('Sites', u.sites, u.max_sites, String),
    usageRow('Disk', u.disk_bytes, u.disk_limit_bytes, fmtBytes),
    usageRow('Bandwidth', u.bandwidth_bytes, u.bandwidth_limit_bytes, fmtBytes),
    u.per_site.length ? table(['Site', 'Files', 'Database', 'Bandwidth', 'Measured'], u.per_site.map((s) => [
      SITES.get(s.site_id)?.primary_domain || s.site_id, fmtBytes(s.files_bytes), fmtBytes(s.db_bytes), fmtBytes(s.bandwidth_bytes),
      s.measured_at ? fmtTime(s.measured_at) : 'not yet'])) : null);
}

function planSummary(p) {
  return `${limitText(p.max_sites, ' sites')} · ${limitText(p.disk_mb, ' MB disk')} · ${limitText(p.bandwidth_gb, ' GB/month')} · ` +
    `per site ${limitText(p.max_replicas, ' replicas')} × ${limitText(p.max_memory_mb, ' MB')}, ${limitText(p.max_cpus, ' CPU')}` +
    (p.features.length ? ` · ${p.features.join(', ')}` : '');
}

// ---- Account tab: API tokens, and a tenant's plan and usage ----

async function loadTokens() {
  const list = await api('GET', '/account/tokens');
  $('#account-tokens').replaceChildren(table(['Name', 'Token', 'Created', 'Expires', 'Last used', ''], list.map((t) => {
    const del = h('button', { class: 'ghost danger' }, 'Revoke');
    del.addEventListener('click', async () => {
      if (!await ask(`Revoke the token "${t.name}"? Anything using it stops working at once.`)) return;
      try { await api('DELETE', `/account/tokens/${t.id}`); await loadTokens(); } catch (e) { showError(e); }
    });
    return [t.name, h('td', {}, h('code', {}, t.hint)), fmtTime(t.created_at), t.expires_at ? fmtTime(t.expires_at) : 'never',
      t.last_used_at ? `${fmtTime(t.last_used_at)} from ${t.last_used_ip}` : 'never', h('td', {}, del)];
  })));
}

async function loadAccountPlan() {
  const acct = await api('GET', '/account');
  const box = $('#account-plan');
  if (!acct.account) { box.replaceChildren(); return; }
  const a = acct.account;
  const u = await api('GET', `/accounts/${a.id}/usage`);
  const measure = h('button', { class: 'ghost' }, 'Measure disk now');
  measure.addEventListener('click', async () => {
    measure.disabled = true;
    try { await api('POST', `/accounts/${a.id}/usage/measure`); await loadAccountPlan(); } catch (e) { showError(e); measure.disabled = false; }
  });
  box.replaceChildren(h('div', {},
    h('div', { class: 'site-head' }, h('h2', {}, `${a.name} · plan ${a.plan.name}`),
      h('span', { class: 'pill ' + (a.effectively_suspended ? 'suspended' : 'active') }, a.effectively_suspended ? 'suspended' : a.status)),
    a.effectively_suspended ? h('p', { class: 'suspended small' }, 'This account is suspended: its sites show a "temporarily unavailable" page ' +
      'and nothing can be changed. Contact your provider.') : null,
    h('p', { class: 'muted small' }, planSummary(a.limits)),
    usageBars(u), h('div', { class: 'actions' }, measure)));
}

// Deferred scripts run once the document is parsed, before app.js's init.
{
  // The Account tab also shows tokens (and, for tenants, the plan).
  const baseAccount = loaders.account;
  loaders.account = async () => {
    await baseAccount();
    await Promise.all([loadTokens(), isTenant() ? loadAccountPlan() : Promise.resolve()]);
  };
  $('#token-form').addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = e.target;
    try {
      const r = await api('POST', '/account/tokens', { name: f.name.value.trim(), expires_days: Number(f.expires_days.value || 0) });
      f.reset();
      showSecret(`API token "${r.api_token.name}"`, [r.token, '', 'Use it as: Authorization: Bearer <token>']);
      await loadTokens();
    } catch (err) { showError(err); }
  });
  $('#new-account-btn').addEventListener('click', () => { $('#new-account').hidden = false; });
  $('#cancel-account').addEventListener('click', () => { $('#new-account').hidden = true; });
  $('#new-account').addEventListener('submit', createAccount);
  $('#plan-form').addEventListener('submit', savePlan);
  $('#plan-features').replaceChildren(...FEATURES.map((f) => h('label', { class: 'check' }, h('input', { type: 'checkbox', name: 'feature', value: f }), f)));
  $('#webhook-events').replaceChildren(h('span', { class: 'small muted' }, 'Events (none checked: all):'),
    ...EVENTS.map((ev) => h('label', { class: 'check small' }, h('input', { type: 'checkbox', name: 'event', value: ev }), ev)));
  $('#stripe-form').addEventListener('submit', saveStripe);
  $('#webhook-form').addEventListener('submit', addWebhook);
}

// ---- Accounts ----

async function loadAccounts() {
  const [accounts, plans] = await Promise.all([api('GET', '/accounts'), api('GET', '/plans')]);
  const form = $('#new-account');
  form.plan_id.replaceChildren(...plans.filter((p) => !isTenant() || p.resellable).map((p) => h('option', { value: p.id }, `${p.name} (${p.id})`)));
  form.parent_id.replaceChildren(h('option', { value: '0' }, 'None'),
    ...accounts.filter((a) => a.kind === 'reseller').map((a) => h('option', { value: String(a.id) }, a.name)));
  $('#accounts').replaceChildren(table(['ID', 'Name', 'Kind', 'Status', 'Plan', 'Sites', 'Reseller', ''], accounts.map((a) => {
    const open = h('button', { class: 'ghost' }, 'Open');
    open.addEventListener('click', () => showAccountDetail(a.id).catch(showError));
    const st = a.effectively_suspended && a.status === 'active' ? 'suspended (reseller)' : a.status + (a.suspend_reason ? ` (${a.suspend_reason})` : '');
    return [String(a.id), a.name, a.kind, h('td', { class: a.effectively_suspended ? 'suspended' : '' }, st), a.plan_id,
      String(a.sites), a.parent_name || '', h('td', {}, open)];
  })));
}

async function createAccount(e) {
  e.preventDefault();
  const f = e.target;
  const body = { name: f.name.value.trim(), plan_id: f.plan_id.value, email: f.email.value.trim() };
  if (!isTenant()) {
    body.kind = f.kind.value;
    body.parent_id = Number(f.parent_id.value || 0);
  }
  if (f.username.value.trim()) body.user = { username: f.username.value.trim() };
  try {
    const r = await api('POST', '/accounts', body);
    f.reset();
    f.hidden = true;
    if (r.password) showSecret(`Account ${r.account.name}`, [`User:     ${r.user.username}`, `Password: ${r.password}`]);
    await loadAccounts();
    await showAccountDetail(r.account.id);
  } catch (err) { showError(err); }
}

async function showAccountDetail(id) {
  const [a, u, events] = await Promise.all([api('GET', `/accounts/${id}`), api('GET', `/accounts/${id}/usage`),
    api('GET', `/accounts/${id}/events?limit=20`)]);
  const own = isTenant() && ME.account_id === a.id; // a reseller's own account: read-only here
  const canManage = isAdmin() || (ME.role === 'reseller' && !own);
  const box = $('#account-detail');
  const act = (label, fn, cls = 'ghost') => {
    const b = h('button', { class: cls }, label);
    b.addEventListener('click', async () => {
      b.disabled = true;
      try { await fn(); await loadAccounts(); await showAccountDetail(id); } catch (e) { showError(e); b.disabled = false; }
    });
    return b;
  };
  const actions = [];
  if (canManage) {
    if (a.status === 'active') {
      actions.push(act('Suspend', async () => {
        if (!await ask(`Suspend ${a.name}? Its sites show a "temporarily unavailable" page until it is unsuspended; nothing is deleted.`)) throw new Error('Cancelled');
        await api('POST', `/accounts/${id}/suspend`, {});
      }, 'ghost danger'));
    } else if (a.status === 'suspended' || isAdmin()) {
      actions.push(act(a.status === 'terminated' ? 'Reactivate' : 'Unsuspend', () => api('POST', `/accounts/${id}/unsuspend`)));
    }
    actions.push(act('Terminate…', async () => {
      const res = await askText(`Its users can no longer sign in and its sites are suspended.`, {
        title: `Terminate ${a.name}?`, label: `Type the account's ID (${id}) to confirm`, match: String(id), ok: 'Terminate',
        check: 'Also delete its sites (files and databases; their backups stay)',
      });
      if (!res || res.value !== String(id)) throw new Error('Cancelled');
      const typed = res.value, del = res.checked;
      await api('POST', `/accounts/${id}/terminate`, { confirm: typed, delete_sites: del });
    }, 'ghost danger'));
    if (isAdmin() && a.status === 'terminated') {
      actions.push(act('Delete account', async () => {
        if (!await ask(`Delete ${a.name} and its users for good?`)) throw new Error('Cancelled');
        await api('DELETE', `/accounts/${id}`);
        box.replaceChildren();
      }, 'ghost danger'));
    }
  }
  actions.push(act('Measure disk now', () => api('POST', `/accounts/${id}/usage/measure`)));

  let planPicker = null;
  if (canManage) {
    const plans = await api('GET', '/plans');
    const sel = h('select', {}, plans.filter((p) => !isTenant() || p.resellable || p.id === a.plan_id)
      .map((p) => h('option', { value: p.id, selected: p.id === a.plan_id }, `${p.name} (${p.id})`)));
    const save = act('Change plan', () => api('PUT', `/accounts/${id}`, { plan_id: sel.value }));
    planPicker = h('div', { class: 'controls' }, h('label', {}, 'Plan', sel), save);
  }

  const users = ME.role === 'reseller' || !isTenant() ? await api('GET', `/accounts/${id}/users`).catch(() => []) : [];
  const userRows = users.map((usr) => {
    const cells = [usr.username, usr.role, usr.totp_enabled ? 'on' : 'off', usr.disabled ? 'disabled' : 'active'];
    const btns = [];
    if (canManage) {
      btns.push(act('Reset password', async () => {
        const r = await api('POST', `/accounts/${id}/users/${usr.id}/password`, {});
        showSecret(`New password for ${usr.username}`, [r.password, '', 'They were signed out everywhere.']);
      }));
      btns.push(act('Sign-in link', async () => {
        const r = await api('POST', `/accounts/${id}/sso`, { username: usr.username });
        showSecret(`One-time sign-in link for ${usr.username} (2 minutes)`, [r.url]);
      }));
      btns.push(act(usr.disabled ? 'Enable' : 'Disable', () => api('PUT', `/accounts/${id}/users/${usr.id}`, { disabled: !usr.disabled })));
    }
    return [...cells, h('td', {}, ...btns)];
  });
  const addUser = h('form', { class: 'inline' }, h('input', { name: 'username', placeholder: 'new user', required: true }),
    h('button', { type: 'submit', class: 'ghost' }, 'Add user'));
  addUser.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      const r = await api('POST', `/accounts/${id}/users`, { username: addUser.username.value.trim() });
      showSecret(`User ${r.user.username}`, [`Password: ${r.password}`]);
      await showAccountDetail(id);
    } catch (err) { showError(err); }
  });

  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'site-head' }, h('h2', {}, `${a.name} (#${a.id}, ${a.kind})`),
      h('span', { class: 'pill ' + (a.effectively_suspended ? 'suspended' : 'active') },
        a.status + (a.suspend_reason ? ` · ${a.suspend_reason}` : ''))),
    h('p', { class: 'muted small' }, `Plan ${a.plan.name}: ${planSummary(a.limits)}`),
    table(['', ''], [['Email', a.email || '–'], ['Reseller', a.parent_name || '–'], ['WHMCS service', a.whmcs_service_id || '–'],
      ['Stripe customer', a.stripe_customer_id || '–'], ['Created', fmtTime(a.created_at)]]),
    usageBars(u),
    planPicker,
    h('div', { class: 'actions' }, ...actions),
    users.length || canManage ? h('h2', {}, 'Users') : null,
    users.length ? table(['User', 'Role', '2FA', 'State', ''], userRows) : null,
    canManage && a.status !== 'terminated' ? addUser : null,
    h('h2', {}, 'Activity'),
    events.length ? h('div', {}, events.map((ev) => h('div', { class: 'small' }, h('time', {}, fmtTime(ev.time)), ` [${ev.kind}] ${ev.message}`)))
      : h('p', { class: 'muted small' }, 'Nothing yet.')));
}

// ---- Plans ----

async function loadPlans() {
  const plans = await api('GET', '/plans');
  $('#plans').replaceChildren(table(['ID', 'Name', 'Limits', 'Overage', 'Resellable', ''], plans.map((p) => {
    const edit = h('button', { class: 'ghost admin-only' }, 'Edit');
    edit.addEventListener('click', () => fillPlanForm(p));
    const del = h('button', { class: 'ghost danger admin-only' }, 'Delete');
    del.addEventListener('click', async () => {
      if (!await ask(`Delete plan ${p.id}?`)) return;
      try { await api('DELETE', `/plans/${encodeURIComponent(p.id)}`); await loadPlans(); } catch (e) { showError(e); }
    });
    return [p.id, p.name, h('td', { class: 'small' }, planSummary(p)), p.overage, p.resellable ? 'yes' : 'no', h('td', {}, edit, del)];
  })));
}

function fillPlanForm(p) {
  const f = $('#plan-form');
  for (const k of ['id', 'name', 'max_sites', 'disk_mb', 'bandwidth_gb', 'max_replicas', 'max_memory_mb', 'max_cpus', 'max_domains', 'overage']) {
    f[k].value = p[k];
  }
  f.backup_repos.value = p.backup_repos.join(', ');
  f.resellable.checked = p.resellable;
  f.querySelectorAll('input[name=feature]').forEach((c) => { c.checked = p.features.includes(c.value); });
  f.dataset.editing = p.id;
  f.scrollIntoView({ behavior: 'smooth' });
}

async function savePlan(e) {
  e.preventDefault();
  const f = e.target;
  const num = (k) => Number(f[k].value || 0);
  const body = {
    id: f.id.value.trim(), name: f.name.value.trim(), max_sites: num('max_sites'), disk_mb: num('disk_mb'),
    bandwidth_gb: num('bandwidth_gb'), max_replicas: num('max_replicas'), max_memory_mb: num('max_memory_mb'),
    max_cpus: num('max_cpus'), max_domains: num('max_domains'), overage: f.overage.value, resellable: f.resellable.checked,
    features: [...f.querySelectorAll('input[name=feature]:checked')].map((c) => c.value),
    backup_repos: splitList(f.backup_repos.value),
  };
  try {
    if (f.dataset.editing === body.id) await api('PUT', `/plans/${encodeURIComponent(body.id)}`, body);
    else await api('POST', '/plans', body);
    delete f.dataset.editing;
    f.reset();
    await loadPlans();
  } catch (err) { showError(err); }
}

// ---- Billing ----

async function loadBilling() {
  const [settings, hooks, deliveries] = await Promise.all([api('GET', '/billing/settings'), api('GET', '/billing/webhooks'),
    api('GET', '/billing/deliveries?limit=50')]);
  $('#stripe-url').textContent = settings.stripe_webhook_url;
  const f = $('#stripe-form');
  f.webhook_secret.placeholder = settings.stripe_webhook_secret_set ? 'set (unchanged if empty)' : 'whsec_…';
  f.secret_key.placeholder = settings.stripe_secret_key_set ? 'set (unchanged if empty)' : 'sk_… or rk_…';
  f.meter_event.value = settings.stripe_meter_event || '';
  f.prices.value = Object.entries(settings.stripe_prices || {}).map(([k, v]) => `${k} = ${v}`).join('\n');
  $('#webhooks').replaceChildren(table(['URL', 'Events', 'State', ''], hooks.map((ep) => {
    const act = (label, fn, cls = 'ghost') => {
      const b = h('button', { class: cls }, label);
      b.addEventListener('click', async () => { try { await fn(); await loadBilling(); } catch (e) { showError(e); } });
      return b;
    };
    return [h('td', { class: 'wrap' }, ep.url), ep.events.length ? ep.events.join(', ') : 'all', ep.enabled ? 'enabled' : 'disabled',
      h('td', {},
        act('Test', () => api('POST', `/billing/webhooks/${ep.id}/test`)),
        act(ep.enabled ? 'Disable' : 'Enable', () => api('PUT', `/billing/webhooks/${ep.id}`, { enabled: !ep.enabled })),
        act('New secret', async () => {
          if (!await ask('Issue a new signing secret? The old one stops working at once.')) return;
          const r = await api('PUT', `/billing/webhooks/${ep.id}`, { rotate_secret: true });
          showSecret('Webhook signing secret', [r.secret]);
        }),
        act('Delete', async () => { if (await ask(`Delete ${ep.url}?`)) await api('DELETE', `/billing/webhooks/${ep.id}`); }, 'ghost danger'))];
  })));
  $('#deliveries').replaceChildren(table(['Time', 'Event', 'Endpoint', 'State', 'Attempts', 'Last answer', ''], deliveries.map((d) => {
    const retry = h('button', { class: 'ghost' }, 'Retry');
    retry.addEventListener('click', async () => { try { await api('POST', `/billing/deliveries/${d.id}/retry`); await loadBilling(); } catch (e) { showError(e); } });
    return [fmtTime(d.created_at), d.event, String(d.endpoint_id), h('td', {}, status(d.status)), String(d.attempts),
      h('td', { class: 'wrap small' }, d.last_error || (d.last_status ? String(d.last_status) : '')),
      h('td', {}, d.status === 'delivered' ? null : retry)];
  })));
}

async function saveStripe(e) {
  e.preventDefault();
  const f = e.target;
  const prices = {};
  for (const line of f.prices.value.split('\n')) {
    const [k, v] = line.split('=').map((x) => (x || '').trim());
    if (k && v) prices[k] = v;
  }
  const body = { meter_event: f.meter_event.value.trim(), prices };
  if (f.webhook_secret.value) body.webhook_secret = f.webhook_secret.value.trim();
  if (f.secret_key.value) body.secret_key = f.secret_key.value.trim();
  try {
    await api('PUT', '/billing/settings', body);
    f.webhook_secret.value = '';
    f.secret_key.value = '';
    await loadBilling();
  } catch (err) { showError(err); }
}

async function addWebhook(e) {
  e.preventDefault();
  const f = e.target;
  const events = [...document.querySelectorAll('#webhook-events input:checked')].map((c) => c.value);
  try {
    const r = await api('POST', '/billing/webhooks', { url: f.url.value.trim(), events });
    f.reset();
    document.querySelectorAll('#webhook-events input').forEach((c) => { c.checked = false; });
    showSecret('Webhook signing secret', [r.secret, '', 'Verify X-WPGenie-Signature with it on every delivery.']);
    await loadBilling();
  } catch (err) { showError(err); }
}

// ---- Single sign-on links (#sso=<token>) ----

// A billing portal sends the user to https://panel/#sso=<token>: the token
// is in the fragment, which never reaches a server or a Referer. It is
// exchanged only after the user confirms, so a link can't sign anyone in
// behind their back.
function handleSSO() {
  const m = location.hash.match(/^#sso=([A-Za-z0-9_-]{20,100})$/);
  if (!m) return false;
  history.replaceState(null, '', location.pathname);
  const token = m[1];
  const box = $('#login');
  const form = $('#login-form');
  const err = $('#login-error');
  document.querySelectorAll('.panel').forEach((p) => { p.hidden = true; });
  box.hidden = false;
  form.hidden = true;
  const code = h('input', { autocomplete: 'one-time-code', inputmode: 'numeric', placeholder: 'Code from your authenticator app' });
  const codeLabel = h('label', { hidden: true }, 'Two-factor code', code);
  const go = h('button', {}, 'Continue to the panel');
  const panel = h('div', { id: 'sso-box' }, h('p', {}, 'You followed a one-time sign-in link from your billing portal.'), codeLabel, go);
  box.insertBefore(panel, err);
  go.addEventListener('click', async () => {
    go.disabled = true;
    err.hidden = true;
    try {
      const res = await api('POST', '/auth/sso', { token, code: code.value.trim() });
      panel.remove();
      form.hidden = false;
      signedIn(res.user);
    } catch (ex) {
      if (ex.data && ex.data.need_code) {
        codeLabel.hidden = false;
        code.focus();
      } else {
        err.textContent = 'This link is invalid, expired or already used. Sign in with your password, or ask for a new link.';
        err.hidden = false;
        panel.remove();
        form.hidden = false;
      }
    } finally { go.disabled = false; }
  });
  return true;
}
