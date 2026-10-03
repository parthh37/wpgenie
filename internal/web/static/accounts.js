'use strict';
// Accounts, plans and API tokens (billing, its settings and webhooks are in
// billing.js and billing-admin.js). Shares api(), h(), table(), status(),
// showError(), showSecret(), fmtBytes() and fmtTime() with app.js and
// panels.js. Everything shown here is also enforced by the server; hiding a
// button is only a convenience.

const FEATURES = ['staging', 'backups', 'sftp', 'files', 'phpmyadmin', 'certificates', 'cdn', 'smtp', 'burst'];

loaders.accounts = loadAccounts;
loaders.plans = loadPlans;

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

// usageBars shows an account's month; b is its burst balance (optional).
function usageBars(u, b) {
  return h('div', {},
    h('p', { class: 'muted small' }, `${new Date(u.month_start).toLocaleDateString([], { month: 'long', year: 'numeric', timeZone: 'UTC' })}` +
      (u.includes_customers ? ' · totals include every customer account' : '')),
    usageRow('Sites', u.sites, u.max_sites, String),
    usageRow('Disk', u.disk_bytes, u.disk_limit_bytes, fmtBytes),
    usageRow('Bandwidth', u.bandwidth_bytes, u.bandwidth_limit_bytes, fmtBytes),
    b && b.allowed ? burstRow(b) : null,
    u.per_site.length ? table(['Site', 'Files', 'Database', 'Bandwidth', 'Measured'], u.per_site.map((s) => [
      SITES.get(s.site_id)?.primary_domain || s.site_id, fmtBytes(s.files_bytes), fmtBytes(s.db_bytes), fmtBytes(s.bandwidth_bytes),
      s.measured_at ? fmtTime(s.measured_at) : 'not yet'])) : null);
}

// burstRow: burst minutes used this month against the plan's, and the
// bought minutes left.
function burstRow(b) {
  const row = usageRow('Burst minutes', b.used, b.unlimited ? 0 : b.included, fmtNum);
  if (b.credit) row.lastChild.textContent += ` · ${fmtNum(b.credit)} extra`;
  return row;
}

function planSummary(p) {
  return `${limitText(p.max_sites, ' sites')} · ${limitText(p.disk_mb, ' MB disk')} · ${limitText(p.bandwidth_gb, ' GB/month')} · ` +
    `per site ${limitText(p.max_replicas, ' replicas')} × ${limitText(p.max_memory_mb, ' MB')}, ${limitText(p.max_cpus, ' CPU')}` +
    (p.features.includes('burst') ? ` · burst ${limitText(p.burst_minutes, ' min/month')}` : '') +
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
  const [u, b] = await Promise.all([api('GET', `/accounts/${a.id}/usage`), api('GET', `/accounts/${a.id}/burst`).catch(() => null)]);
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
    usageBars(u, b),
    // Out of burst minutes: buy more (clientarea.js), when this account may.
    b && b.allowed && !b.unlimited && b.remaining <= 0 && typeof burstBuyButton === 'function'
      ? h('p', { class: 'small st-warning' }, 'Out of burst minutes: sites stay at their normal size. ', burstBuyButton()) : null,
    h('div', { class: 'actions' }, measure)));
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
  $('#plan-cancel').addEventListener('click', resetPlanForm);
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
  const [a, u, events, burst] = await Promise.all([api('GET', `/accounts/${id}`), api('GET', `/accounts/${id}/usage`),
    api('GET', `/accounts/${id}/events?limit=20`), api('GET', `/accounts/${id}/burst`).catch(() => null)]);
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
  if (isAdmin() && burst && burst.allowed && !burst.unlimited) {
    actions.push(act('Add burst minutes…', async () => {
      const n = await askText(`${a.name} has ${fmtNum(burst.remaining)} burst minutes left (${fmtNum(burst.credit)} of them bought). ` +
        'Bought minutes never expire and are used once the month\'s are gone. A negative number takes minutes away.',
      { title: 'Add burst minutes', label: 'Minutes', type: 'number', value: '600', ok: 'Add' });
      if (!n || !Number(n)) throw new Error('Cancelled');
      await api('POST', `/accounts/${id}/burst-credit`, { minutes: Math.trunc(Number(n)) });
      notify(`${fmtNum(Math.trunc(Number(n)))} burst minutes added to ${a.name}`);
    }));
  }

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
    table(['', ''], [['E-mail', a.email || '–'], ['Reseller', a.parent_name || '–'], ['WHMCS service', a.whmcs_service_id || '–'],
      ['Stripe customer', a.stripe_customer_id || '–'], ['Created', fmtTime(a.created_at)]]),
    usageBars(u, burst),
    planPicker,
    h('div', { class: 'actions' }, ...actions),
    // Staff bill accounts here; resellers bill their customers elsewhere.
    !isTenant() && typeof accountBilling === 'function' ? accountBilling(a) : null,
    users.length || canManage ? h('h2', {}, 'Users') : null,
    users.length ? table(['User', 'Role', '2FA', 'State', ''], userRows) : null,
    canManage && a.status !== 'terminated' ? addUser : null,
    h('h2', {}, 'Activity'),
    events.length ? h('div', {}, events.map((ev) => h('div', { class: 'small' }, h('time', {}, fmtTime(ev.time)), ` [${ev.kind}] ${ev.message}`)))
      : h('p', { class: 'muted small' }, 'Nothing yet.')));
}

// ---- Plans ----

async function loadPlans() {
  const [plans] = await Promise.all([api('GET', '/plans'), typeof billingConfig === 'function' ? billingConfig().catch(() => null) : null]);
  // Prices per billing period, when the server has them (billing.js).
  const pricing = typeof renderPriceEditor === 'function' && plans.some((p) => 'prices' in p);
  if (pricing) PLAN_PRICING = true;
  $('#plan-pricing').hidden = !PLAN_PRICING;
  const form = $('#plan-form');
  if (PLAN_PRICING && !form.dataset.editing) renderPriceEditor($('#plan-prices'), {});
  const heads = PLAN_PRICING ? ['ID', 'Name', 'Price', 'Limits', 'Overage', 'Resellable', ''] : ['ID', 'Name', 'Limits', 'Overage', 'Resellable', ''];
  $('#plans').replaceChildren(table(heads, plans.map((p) => {
    const edit = h('button', { class: 'ghost admin-only' }, 'Edit');
    edit.addEventListener('click', () => fillPlanForm(p));
    const del = h('button', { class: 'ghost danger admin-only' }, 'Delete');
    del.addEventListener('click', async () => {
      if (!await ask(`Delete plan ${p.id}?`)) return;
      try { await api('DELETE', `/plans/${encodeURIComponent(p.id)}`); await loadPlans(); } catch (e) { showError(e); }
    });
    const name = h('td', {}, p.name, p.public ? [' ', h('span', { class: 'badge' }, 'on the order page')] : null,
      p.description ? h('div', { class: 'muted small' }, p.description) : null);
    return [p.id, name, PLAN_PRICING ? h('td', {}, planPriceText(p)) : null, h('td', { class: 'small' }, planSummary(p)), p.overage,
      p.resellable ? 'yes' : 'no', h('td', {}, edit, del)].filter((c) => c !== null);
  })));
}

function fillPlanForm(p) {
  const f = $('#plan-form');
  for (const k of ['id', 'name', 'max_sites', 'disk_mb', 'bandwidth_gb', 'max_replicas', 'max_memory_mb', 'max_cpus', 'max_domains',
    'burst_minutes', 'overage']) {
    f[k].value = p[k];
  }
  f.backup_repos.value = p.backup_repos.join(', ');
  f.resellable.checked = p.resellable;
  f.querySelectorAll('input[name=feature]').forEach((c) => { c.checked = p.features.includes(c.value); });
  if (PLAN_PRICING) {
    f.description.value = p.description || '';
    f.public.checked = !!p.public;
    f.account_kind.value = p.account_kind || 'customer';
    f.sort.value = p.sort || 0;
    f.overage_gb_price.value = fromMinor(p.overage_gb_price || 0);
    renderPriceEditor($('#plan-prices'), p);
  }
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
    max_cpus: num('max_cpus'), max_domains: num('max_domains'), burst_minutes: num('burst_minutes'),
    overage: f.overage.value, resellable: f.resellable.checked,
    features: [...f.querySelectorAll('input[name=feature]:checked')].map((c) => c.value),
    backup_repos: splitList(f.backup_repos.value),
  };
  try {
    if (PLAN_PRICING) {
      const overage = toMinor(f.overage_gb_price.value);
      if (Number.isNaN(overage) || overage < 0) throw new Error('The price per extra GB isn\'t an amount.');
      Object.assign(body, { description: f.description.value.trim(), public: f.public.checked, account_kind: f.account_kind.value,
        sort: Number(f.sort.value || 0), overage_gb_price: overage || 0, prices: readPriceEditor(f) });
    }
    if (f.dataset.editing === body.id) await api('PUT', `/plans/${encodeURIComponent(body.id)}`, body);
    else await api('POST', '/plans', body);
    resetPlanForm();
    await loadPlans();
  } catch (err) { showError(err); }
}

function resetPlanForm() {
  const f = $('#plan-form');
  delete f.dataset.editing;
  f.reset();
  if (PLAN_PRICING) renderPriceEditor($('#plan-prices'), {});
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
