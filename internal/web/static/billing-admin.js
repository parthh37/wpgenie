'use strict';
// Billing for administrators: promotions, tax rules and the settings
// (company, numbering, taxes, reminders and suspension, payment methods,
// outgoing webhooks). Uses billing.js's helpers (field, toggle, openDialog,
// money…). The server checks every setting again; nothing here is trusted.

// ---- Promotions ----

async function staffPromotions(box) {
  const [promos, plans] = await Promise.all([api('GET', '/billing/promotions').then((r) => listOf(r, 'promotions')), api('GET', '/plans')]);
  const reload = () => staffPromotions(box).catch((e) => box.replaceChildren(loadError(e)));
  const add = actionButton('New promotion', async () => { if (await editPromotion(null, plans)) reload(); }, { cls: '', ic: 'plus' });
  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'card-head' }, h('div', {}, h('h2', {}, 'Promotions'),
      h('p', { class: 'muted small' }, 'Codes clients enter on the order page (or you apply to an invoice) for a discount.')), add),
    btable(['Code', 'Discount', 'For', 'Used', 'Valid', 'On', ''], promos.map((p) => {
      const on = h('input', { type: 'checkbox', class: 'switch', checked: !!p.enabled, 'aria-label': `${p.code} enabled` });
      on.addEventListener('change', async () => {
        on.disabled = true;
        try { await api('PUT', `/billing/promotions/${p.id}`, promoBody({ ...p, enabled: on.checked })); p.enabled = on.checked; }
        catch (e) { showError(e); on.checked = !on.checked; } finally { on.disabled = false; }
      });
      return {
        cls: p.enabled ? '' : 'row-off',
        cells: [h('td', {}, h('code', { class: 'promo-code' }, p.code), p.description ? h('span', { class: 'sub-line' }, p.description) : null),
          h('td', {}, promoValue(p), h('span', { class: 'sub-line' }, p.recurring ? 'every invoice' : 'first invoice only')),
          h('td', { class: 'small' }, promoScope(p, plans)),
          `${fmtNum(p.uses || 0)}${p.max_uses ? ' of ' + fmtNum(p.max_uses) : ''}`,
          h('td', { class: 'small' }, promoDates(p)), h('td', {}, on),
          h('td', { class: 'row-actions' },
            actionButton('Edit', async () => { if (await editPromotion(p, plans)) reload(); }),
            actionButton('Delete', async () => {
              if (!await ask(`Delete the promotion ${p.code}? Invoices that used it keep their discount.`)) return;
              await api('DELETE', `/billing/promotions/${p.id}`);
              reload();
            }, { cls: 'ghost danger' }))],
      };
    }), {
      caption: 'Promotions',
      empty: emptyState('percent', 'No promotions yet', 'A code like LAUNCH20 for 20% off the first invoice, or a fixed amount off every renewal ' +
        'for a partner. You choose the plans, billing periods, dates and how many times it can be used.', add.cloneNode(true)),
    })));
  // The empty state's button is a copy: give it the action.
  const copy = box.querySelector('.b-empty button');
  if (copy) copy.addEventListener('click', () => add.click());
}

const promoValue = (p) => (p.type === 'percent' ? `${fmtRate(p.value)} off` : `${money(p.value)} off`);
const promoDates = (p) => (p.starts_at || p.expires_at
  ? `${p.starts_at ? fmtDate(p.starts_at) : 'now'} – ${p.expires_at ? fmtDate(p.expires_at) : 'no end'}` : 'always');

function promoScope(p, plans, withClients = true) {
  const names = (p.plans || []).map((id) => (plans.find((x) => x.id === id) || { name: id }).name);
  const parts = [names.length ? names.join(', ') : 'every plan'];
  if ((p.cycles || []).length) parts.push(p.cycles.map((c) => cycleOf(c).label.toLowerCase()).join(', '));
  if (withClients && p.new_clients_only) parts.push('new clients only');
  return parts.join(' · ');
}

// promoBody is what the API takes (id and uses are the server's).
function promoBody(p) {
  const { id, uses, ...body } = p; // eslint-disable-line no-unused-vars
  return body;
}

function editPromotion(p, plans) {
  p = p || { code: '', description: '', type: 'percent', value: 1000, plans: [], cycles: [], recurring: false, max_uses: 0,
    starts_at: null, expires_at: null, new_clients_only: true, enabled: true };
  const code = h('input', { name: 'code', required: true, value: p.code, pattern: '[A-Za-z0-9_-]{2,32}', autocomplete: 'off', spellcheck: 'false', class: 'promo-code' });
  const gen = h('button', { type: 'button', class: 'ghost' }, 'Make one up');
  gen.addEventListener('click', () => {
    const abc = 'ABCDEFGHJKLMNPQRSTUVWXYZ23456789', b = crypto.getRandomValues(new Uint8Array(8));
    code.value = [...b].map((x) => abc[x % abc.length]).join('');
    sync();
  });
  const valueBox = h('div');
  const summary = liveHint();
  const planChecks = h('div', { class: 'check-list' }, plans.map((pl) => h('label', { class: 'check' },
    h('input', { type: 'checkbox', name: 'plans', value: pl.id, checked: (p.plans || []).includes(pl.id) }), pl.name)));
  const cycleChecks = h('div', { class: 'check-list' }, CYCLES.map((c) => h('label', { class: 'check' },
    h('input', { type: 'checkbox', name: 'cycles', value: c.id, checked: (p.cycles || []).includes(c.id) }), c.label)));
  const types = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Kind of discount' },
    h('label', {}, h('input', { type: 'radio', name: 'type', value: 'percent', checked: p.type === 'percent' }), h('span', {}, 'Percentage')),
    h('label', {}, h('input', { type: 'radio', name: 'type', value: 'fixed', checked: p.type === 'fixed' }), h('span', {}, 'Fixed amount')));
  let form = null;
  const drawValue = (type, value) => valueBox.replaceChildren(field(type === 'percent' ? 'Percent off' : 'Amount off',
    type === 'percent' ? percentInput('value', value, { required: true }) : moneyInput('value', value, { required: true })));
  const read = (f) => {
    const type = f.elements.type.value;
    return {
      code: f.elements.code.value.trim().toUpperCase(), description: f.elements.description.value.trim(), type,
      value: type === 'percent' ? toRate(f.elements.value.value) : toMinor(f.elements.value.value),
      plans: [...f.querySelectorAll('input[name=plans]:checked')].map((x) => x.value),
      cycles: [...f.querySelectorAll('input[name=cycles]:checked')].map((x) => x.value),
      recurring: f.elements.recurring.checked, max_uses: Math.max(0, Math.trunc(Number(f.elements.max_uses.value || 0))),
      starts_at: fromDateInput(f.elements.starts_at.value), expires_at: fromDateInput(f.elements.expires_at.value),
      new_clients_only: f.elements.new_clients_only.checked, enabled: f.elements.enabled.checked,
    };
  };
  function sync() {
    if (!form) return;
    const v = read(form);
    const off = Number.isNaN(v.value) || v.value == null ? '…' : v.type === 'percent' ? fmtRate(v.value) : money(v.value);
    summary.textContent = `${v.code || 'The code'} takes ${off} off ${v.recurring ? 'every invoice' : 'the first invoice'}` +
      ` of ${v.new_clients_only ? 'new clients' : 'anyone'}, on ${promoScope(v, plans, false)}` +
      (v.max_uses ? `, ${v.max_uses} time${v.max_uses === 1 ? '' : 's'} at most` : '') +
      (v.expires_at ? `, until ${fmtDate(v.expires_at)}` : '') + '.';
  }
  drawValue(p.type, p.value);
  types.addEventListener('change', () => { drawValue(form.elements.type.value, null); sync(); });
  return openDialog({
    title: p.id ? `Edit ${p.code}` : 'New promotion', wide: true,
    body: [h('div', { class: 'grid' },
      field('Code', h('span', { class: 'with-button' }, code, gen), 'Letters, digits, - and _. Clients type it; case doesn\'t matter.'),
      field('Description (optional)', h('input', { name: 'description', value: p.description || '', maxlength: 200 }), 'Shown on the invoice line.')),
    h('div', { class: 'grid' }, h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Discount'), types), valueBox),
    h('div', { class: 'grid' },
      h('fieldset', { class: 'field' }, h('legend', { class: 'field-label' }, 'Plans (none ticked: every plan)'), planChecks),
      h('fieldset', { class: 'field' }, h('legend', { class: 'field-label' }, 'Billing periods (none ticked: all)'), cycleChecks)),
    h('div', { class: 'grid' },
      field('Starts', h('input', { type: 'date', name: 'starts_at', value: dateInput(p.starts_at) }), 'Empty: now.'),
      field('Ends', h('input', { type: 'date', name: 'expires_at', value: dateInput(p.expires_at) }), 'Empty: no end.'),
      field('Uses at most', h('input', { type: 'number', name: 'max_uses', min: 0, value: p.max_uses || 0 }), '0: no limit.')),
    toggle('recurring', 'Also on renewals', p.recurring, 'Off: only the first invoice gets the discount.'),
    toggle('new_clients_only', 'New clients only', p.new_clients_only),
    toggle('enabled', 'Enabled', p.enabled !== false),
    summary],
    onOpen(ctx) { form = ctx.form; form.addEventListener('input', sync); form.addEventListener('change', sync); sync(); },
    async submit(f) {
      const body = read(f);
      if (body.value == null || Number.isNaN(body.value) || body.value <= 0) throw fieldError('value', 'Enter the discount.');
      if (body.type === 'percent' && body.value > 10000) throw fieldError('value', 'A percentage goes up to 100.');
      if (p.id) await api('PUT', `/billing/promotions/${p.id}`, body);
      else await api('POST', '/billing/promotions', body);
      notify(`Promotion ${body.code} saved`);
    },
  });
}

// ---- Tax rules ----

// Standard VAT rates of the EU countries (hundredths of a percent), 2025.
const EU_VAT = [['AT', 2000], ['BE', 2100], ['BG', 2000], ['HR', 2500], ['CY', 1900], ['CZ', 2100], ['DK', 2500], ['EE', 2400],
  ['FI', 2550], ['FR', 2000], ['DE', 1900], ['GR', 2400], ['HU', 2700], ['IE', 2300], ['IT', 2200], ['LV', 2100], ['LT', 2100],
  ['LU', 1700], ['MT', 1800], ['NL', 2100], ['PL', 2300], ['PT', 2300], ['RO', 2100], ['SK', 2300], ['SI', 2200], ['ES', 2100], ['SE', 2500]];

const rule = (name, country, state, rate, level = 1, compound = false) => ({ name, country, state: state || '', rate, level, compound });

// Presets write the usual rules; the operator checks the list first.
const TAX_PRESETS = [
  {
    id: 'in', label: 'India GST 18% (CGST 9% + SGST 9%)', needState: 'Your state',
    help: 'Clients in your state pay CGST and SGST; clients in the rest of India pay IGST. Write the state as your clients\' addresses do.',
    rules: (st) => [rule('CGST', 'IN', st, 900, 1), rule('SGST', 'IN', st, 900, 2), rule('IGST', 'IN', '', 1800, 1)],
  },
  {
    id: 'eu', label: 'EU VAT (each country\'s standard rate)',
    help: 'Charges the VAT of the client\'s country (the One-Stop Shop scheme). For businesses with a VAT number, turn on ' +
      '"No tax with a tax ID" in Settings → Taxes (reverse charge). Rates as of 2025: check them before you rely on them.',
    rules: () => EU_VAT.map(([c, r]) => rule('VAT', c, '', r, 1)),
  },
  {
    id: 'ca', label: 'Canada GST/HST (+ PST/QST)',
    help: 'GST everywhere; HST instead in ON, NB, NS, NL and PE; provincial sales tax on top in BC, MB, SK and QC. QST (9.975%) ' +
      'is rounded to 9.98%. Provinces as two-letter codes.',
    rules: () => [rule('GST', 'CA', '', 500, 1), rule('HST', 'CA', 'ON', 1300, 1), rule('HST', 'CA', 'NB', 1500, 1), rule('HST', 'CA', 'NS', 1400, 1),
      rule('HST', 'CA', 'NL', 1500, 1), rule('HST', 'CA', 'PE', 1500, 1), rule('PST', 'CA', 'BC', 700, 2), rule('RST', 'CA', 'MB', 700, 2),
      rule('PST', 'CA', 'SK', 600, 2), rule('QST', 'CA', 'QC', 998, 2)],
  },
  { id: 'uk', label: 'UK VAT 20%', rules: () => [rule('VAT', 'GB', '', 2000, 1)] },
  { id: 'au', label: 'Australia GST 10%', rules: () => [rule('GST', 'AU', '', 1000, 1)] },
];

const ruleWhere = (r) => (r.country ? countryName(r.country) + (r.state ? `, ${r.state}` : '') : 'Everywhere');
const sameRule = (a, b) => a.name === b.name && a.country === b.country && (a.state || '') === (b.state || '') && a.level === b.level;

async function staffTaxes(box) {
  const [rules, settings] = await Promise.all([api('GET', '/billing/tax-rules').then((r) => listOf(r, 'rules')),
    api('GET', '/billing/invoicing').catch(() => null)]);
  const reload = () => staffTaxes(box).catch((e) => box.replaceChildren(loadError(e)));
  const add = actionButton('New rule', async () => { if (await editTaxRule(null)) reload(); }, { cls: '', ic: 'plus' });
  const presets = h('div', { class: 'preset-row' }, TAX_PRESETS.map((p) =>
    actionButton(p.label, async () => { if (await applyTaxPreset(p, rules)) reload(); }, { cls: 'ghost preset' })));
  const sorted = rules.slice().sort((a, b) => ruleWhere(a).localeCompare(ruleWhere(b)) || a.level - b.level);
  box.replaceChildren(
    settings && settings.tax && !settings.tax.enabled
      ? banner('warn', 'alert', 'Taxes are off', 'These rules aren\'t applied until taxes are turned on.',
        actionButton('Turn on taxes', () => billingGo('settings', 'taxes'), { cls: '' })) : '',
    h('div', { class: 'card' },
      h('div', { class: 'card-head' }, h('div', {}, h('h2', {}, 'Tax rules'),
        h('p', { class: 'muted small' }, 'Each client pays the rules for where they are: a rule for their state beats one for their country, ' +
          'which beats one for everywhere. Level 2 is a second tax on top (like SGST after CGST, or PST after GST); ' +
          'a compound level 2 is also charged on the level 1 tax.')), add),
      h('h3', {}, 'Start from a preset'), presets,
      btable(['Name', 'Where', { label: 'Rate', num: true }, 'Level', ''], sorted.map((r) => [
        h('td', {}, h('strong', {}, r.name)), ruleWhere(r), fmtRate(r.rate), r.level === 2 ? `2${r.compound ? ', compound' : ''}` : '1',
        h('td', { class: 'row-actions' },
          actionButton('Edit', async () => { if (await editTaxRule(r)) reload(); }),
          actionButton('Delete', async () => {
            if (!await ask(`Delete the ${r.name} rule for ${ruleWhere(r)}? Invoices already issued keep their tax.`)) return;
            await api('DELETE', `/billing/tax-rules/${r.id}`);
            reload();
          }, { cls: 'ghost danger' }))]), {
        caption: 'Tax rules',
        empty: emptyState('landmark', 'No tax rules', 'Without rules no tax is charged. Start from a preset above, or add a rule for your country.'),
      })));
}

async function applyTaxPreset(preset, existing) {
  const state = preset.needState ? h('input', { name: 'state', required: true, placeholder: 'e.g. Maharashtra' }) : null;
  const list = h('div');
  // Which rules take the state typed in (a marker tells them apart).
  const takesState = preset.rules('\u0001').map((r) => r.state === '\u0001');
  const draw = () => {
    const st = state ? state.value.trim() : '';
    const rules = preset.rules(st);
    list.replaceChildren(btable(['Name', 'Where', { label: 'Rate', num: true }, 'Level', ''], rules.map((r, i) => [r.name,
      takesState[i] && !st ? `${countryName(r.country)}, your state` : ruleWhere(r), fmtRate(r.rate), String(r.level),
      existing.some((x) => sameRule(x, r)) ? h('span', { class: 'muted small' }, 'already there') : ''])));
  };
  if (state) state.addEventListener('input', draw);
  draw();
  return openDialog({
    title: preset.label, wide: true, ok: 'Add these rules', intro: preset.help,
    body: [state ? field(preset.needState, state) : null, list],
    async submit() {
      const rules = preset.rules(state ? state.value.trim() : '').filter((r) => !existing.some((x) => sameRule(x, r)));
      for (const r of rules) await api('POST', '/billing/tax-rules', r);
      notify(`${rules.length} tax rule${rules.length === 1 ? '' : 's'} added`);
    },
  });
}

function editTaxRule(r) {
  r = r || rule('', '', '', null, 1, false);
  const country = h('select', { name: 'country' }, h('option', { value: '' }, 'Everywhere'),
    countryList().map(([code, name]) => h('option', { value: code, selected: code === r.country }, name)));
  const state = h('input', { name: 'state', value: r.state || '', placeholder: 'every state' });
  const compound = toggle('compound', 'Compound: charged on the price plus the level 1 tax', r.compound);
  const levels = h('div', { class: 'seg', role: 'radiogroup', 'aria-label': 'Level' },
    h('label', {}, h('input', { type: 'radio', name: 'level', value: '1', checked: r.level !== 2 }), h('span', {}, '1 · the main tax')),
    h('label', {}, h('input', { type: 'radio', name: 'level', value: '2', checked: r.level === 2 }), h('span', {}, '2 · a second tax')));
  const hint = liveHint();
  let form = null;
  const sync = () => {
    if (!form) return;
    const lvl = form.elements.level.value;
    compound.hidden = lvl !== '2';
    state.disabled = !country.value;
    const rate = toRate(form.elements.rate.value);
    hint.textContent = `${Number.isNaN(rate) ? '…' : fmtRate(rate)} ${form.elements.name.value.trim() || 'tax'} for clients ` +
      (country.value ? `in ${country.value && state.value.trim() ? state.value.trim() + ', ' : ''}${countryName(country.value)}` : 'anywhere without a more specific rule') +
      (lvl === '2' ? ', on top of the level 1 tax' : '') + '.';
  };
  return openDialog({
    title: r.id ? `Edit ${r.name}` : 'New tax rule',
    body: [h('div', { class: 'grid' },
      field('Name', h('input', { name: 'name', required: true, value: r.name, maxlength: 40, placeholder: 'GST, VAT…' }), 'Printed on invoices.'),
      field('Rate', percentInput('rate', r.rate, { required: true }))),
    h('div', { class: 'grid' }, field('Country', country), field('State / province', state, 'Empty: the whole country.')),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Level'), levels), compound, hint],
    onOpen(ctx) { form = ctx.form; form.addEventListener('input', sync); form.addEventListener('change', sync); sync(); },
    async submit(f) {
      const rate = toRate(f.elements.rate.value);
      if (Number.isNaN(rate) || rate < 0 || rate > 10000) throw fieldError('rate', 'Enter a rate between 0 and 100%.');
      const level = Number(f.elements.level.value);
      const body = { name: f.elements.name.value.trim(), country: country.value, state: country.value ? state.value.trim() : '', rate, level,
        compound: level === 2 && f.elements.compound.checked };
      if (r.id) await api('PUT', `/billing/tax-rules/${r.id}`, body);
      else await api('POST', '/billing/tax-rules', body);
      notify('Tax rule saved');
    },
  });
}

// ---- Settings ----

const SETTINGS_SECTIONS = [['', 'Company'], ['invoices', 'Invoices'], ['taxes', 'Taxes'], ['automation', 'Reminders & suspension'],
  ['payments', 'Payment methods'], ['burst', 'Burst minutes'], ['webhooks', 'Webhooks']];

async function staffSettings(box, route) {
  // Without built-in billing only Stripe and webhooks have settings.
  const section = SETTINGS_SECTIONS.some(([k]) => k === route.id) ? route.id : BILLING.unavailable ? 'payments' : '';
  const content = h('div', { 'aria-live': 'off' });
  box.replaceChildren(subnav('Settings sections', SETTINGS_SECTIONS.map(([key, label]) =>
    ({ key, label, href: '#/billing/settings' + (key ? '/' + key : '') })), section, (k) => billingGo('settings', k), 'small'), content);
  const render = { '': settingsCompany, invoices: settingsInvoices, taxes: settingsTaxes, automation: settingsAutomation,
    payments: settingsPayments, burst: settingsBurst, webhooks: settingsWebhooks }[section];
  try {
    await render(content);
  } catch (e) {
    content.replaceChildren(loadError(e, () => staffSettings(box, route)));
  }
}

// The invoicing settings' output-only fields: the API rejects them back.
function invoicingBody(s) {
  const body = JSON.parse(JSON.stringify(s));
  delete body.stripe_ready;
  for (const m of Object.values(body.methods || {})) {
    for (const k of Object.keys(m)) if (k.endsWith('_set') || k === 'webhook_url') delete m[k];
  }
  return body;
}

// saveInvoicing merges a change into the settings and saves them.
async function saveInvoicing(s, change) {
  const body = invoicingBody(s);
  for (const [k, v] of Object.entries(change)) body[k] = v && typeof v === 'object' && !Array.isArray(v) ? { ...(body[k] || {}), ...v } : v;
  const saved = await api('PUT', '/billing/invoicing', body);
  Object.assign(s, saved || body);
  BILLING = null; // the currency, methods… the client area sees
  notify('Settings saved');
  return s;
}

// settingsForm is a card whose form saves one part of the settings.
function settingsForm(title, intro, body, save, ok = 'Save') {
  const status = h('span', { class: 'muted small save-state', 'aria-live': 'polite' });
  const btn = h('button', { type: 'submit' }, ok);
  const form = h('form', { class: 'card settings-card' }, h('h2', {}, title), intro ? h('p', { class: 'muted small' }, intro) : null, body,
    h('div', { class: 'actions' }, status, btn));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (!form.reportValidity()) return;
    btn.disabled = true;
    status.textContent = 'Saving…';
    try { await save(form); status.textContent = 'Saved.'; } catch (err) {
      status.textContent = '';
      showError(friendly(err));
      const name = err.data && err.data.field;
      const el = name && form.elements[name.split('.').pop()];
      if (el && el.focus) { el.setAttribute('aria-invalid', 'true'); el.focus(); }
    } finally { btn.disabled = false; }
  });
  form.addEventListener('input', () => { status.textContent = ''; });
  return form;
}

const CURRENCIES = ['USD', 'EUR', 'GBP', 'INR', 'CAD', 'AUD', 'NZD', 'SGD', 'AED', 'CHF', 'SEK', 'NOK', 'DKK', 'PLN', 'CZK', 'JPY', 'BRL', 'MXN', 'ZAR'];

async function settingsCompany(box) {
  const s = await api('GET', '/billing/invoicing');
  const c = s.company || {}, cur = s.currency || {};
  const master = toggle('enabled', 'Built-in billing', s.enabled,
    'Invoices for the accounts billed by invoice, card and bank payments, reminders, and your public order page.');
  const input = master.querySelector('input');
  input.addEventListener('change', async () => {
    if (!input.checked && !await ask('Turn off built-in billing? No invoices or reminders are sent and the order page closes. ' +
      'Existing invoices stay; clients can still see them.', { ok: 'Turn off', danger: true })) { input.checked = true; return; }
    input.disabled = true;
    try { await saveInvoicing(s, { enabled: input.checked }); } catch (e) { showError(e); input.checked = !input.checked; } finally { input.disabled = false; }
  });
  const code = h('select', { name: 'code' }, options([...new Set([...CURRENCIES, cur.code || 'USD'])].map((x) => [x, x]), cur.code || 'USD'));
  const symbol = h('input', { name: 'symbol', value: cur.symbol || '', maxlength: 5 });
  const decimals = h('input', { name: 'decimals', type: 'number', min: 0, max: 3, value: cur.decimals ?? 2 });
  const example = liveHint();
  const sync = () => {
    try {
      const f = new Intl.NumberFormat(undefined, { style: 'currency', currency: code.value, minimumFractionDigits: Number(decimals.value), maximumFractionDigits: Number(decimals.value) });
      example.textContent = `Prices look like ${f.format(1234.5)}.`;
    } catch (e) { example.textContent = ''; }
  };
  code.addEventListener('change', () => {
    const f = new Intl.NumberFormat('en', { style: 'currency', currency: code.value });
    symbol.value = (f.formatToParts(0).find((p) => p.type === 'currency') || {}).value || code.value;
    decimals.value = f.resolvedOptions().maximumFractionDigits;
    sync();
  });
  decimals.addEventListener('input', sync);
  sync();
  box.replaceChildren(
    h('div', { class: 'card settings-card' }, master),
    settingsForm('Your company', 'Printed at the top of invoices and in the footer of e-mails.', h('div', { class: 'grid' },
      field('Company name', h('input', { name: 'name', value: c.name || '', required: true, autocomplete: 'organization' })),
      field('E-mail', h('input', { name: 'email', type: 'email', value: c.email || '' }), 'Where clients reply about invoices.'),
      field('Phone', h('input', { name: 'phone', type: 'tel', value: c.phone || '' })),
      field('Website', h('input', { name: 'website', type: 'url', value: c.website || '', placeholder: 'https://' })),
      field('Tax ID', h('input', { name: 'tax_id', value: c.tax_id || '' }), 'Your VAT, GST or company number.'),
      field('Address', h('textarea', { name: 'address', rows: 4 }, c.address || ''))),
    (f) => saveInvoicing(s, { company: Object.fromEntries(['name', 'email', 'phone', 'website', 'tax_id', 'address'].map((k) => [k, f.elements[k].value.trim()])) })),
    settingsForm('Currency', 'One currency for the whole store. Set it before the first invoice: amounts aren\'t converted when it changes.',
      [h('div', { class: 'grid' }, field('Currency', code), field('Symbol', symbol), field('Decimals', decimals)), example],
      (f) => saveInvoicing(s, { currency: { code: f.elements.code.value, symbol: f.elements.symbol.value.trim(), decimals: Number(f.elements.decimals.value) } })));
}

async function settingsInvoices(box) {
  const s = await api('GET', '/billing/invoicing');
  const inv = s.invoice || {};
  const example = liveHint();
  const prefix = h('input', { name: 'prefix', value: inv.prefix ?? 'INV-', maxlength: 20 });
  const next = h('input', { name: 'next_number', type: 'number', min: 1, value: inv.next_number || 1 });
  const sync = () => { example.textContent = `The next invoice will be ${prefix.value}${String(Number(next.value) || 1).padStart(6, '0')}.`; };
  prefix.addEventListener('input', sync);
  next.addEventListener('input', sync);
  sync();
  box.replaceChildren(settingsForm('Invoices and numbering', null, [
    h('div', { class: 'grid' }, field('Number prefix', prefix), field('Next number', next, 'Numbers have no gaps; raise it to continue an old series.'),
      field('Create renewal invoices', h('span', { class: 'affix suffix' }, h('input', { name: 'days_before_due', type: 'number', min: 0, max: 60, value: inv.days_before_due ?? 7 }),
        h('span', { class: 'affix-text' }, 'days before due')), 'So clients have time to pay before the due date.')),
    example,
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Give invoices their number'), h('div', { class: 'choices' },
      choice('number_on', 'issue', 'When they\'re issued', 'The usual way.', (inv.number_on || 'issue') === 'issue'),
      choice('number_on', 'payment', 'When they\'re paid', 'Unpaid ones are proforma invoices; required in some countries so numbers only go to real sales.', inv.number_on === 'payment'))),
    h('div', { class: 'grid' },
      field('Terms of service URL', h('input', { name: 'terms_url', type: 'url', value: inv.terms_url || '', placeholder: 'https://' }), 'Clients accept them when they order.'),
      field('Invoice footer', h('textarea', { name: 'footer', rows: 3 }, inv.footer || ''), 'e.g. bank details or a thank-you.'))],
  (f) => saveInvoicing(s, { invoice: { prefix: f.elements.prefix.value, next_number: Number(f.elements.next_number.value),
    number_on: f.elements.number_on.value, days_before_due: Number(f.elements.days_before_due.value), terms_url: f.elements.terms_url.value.trim(),
    footer: f.elements.footer.value } })));
}

async function settingsTaxes(box) {
  const s = await api('GET', '/billing/invoicing');
  const t = s.tax || {};
  box.replaceChildren(settingsForm('Taxes', null, [
    toggle('enabled', 'Charge tax', t.enabled, 'Using your tax rules, by where each client is.'),
    h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Your prices'), h('div', { class: 'choices' },
      choice('inclusive', 'no', 'Don\'t include tax', 'Tax is added on top: a 100.00 plan with 18% tax costs 118.00.', !t.inclusive),
      choice('inclusive', 'yes', 'Include tax', 'The price is what clients pay: a 100.00 plan with 18% tax is 84.75 plus 15.25 tax.', !!t.inclusive))),
    toggle('exempt_with_tax_id', 'No tax for clients with a tax ID', t.exempt_with_tax_id,
      'For businesses that account for the tax themselves (EU reverse charge).'),
    h('p', { class: 'small' }, linkButton('Edit the tax rules →', () => billingGo('taxes')))],
  (f) => saveInvoicing(s, { tax: { enabled: f.elements.enabled.checked, inclusive: f.elements.inclusive.value === 'yes',
    exempt_with_tax_id: f.elements.exempt_with_tax_id.checked } })));
}

// dunningSteps is what happens to an unpaid invoice, day by day.
function dunningSteps(a, daysBeforeDue) {
  const steps = [{ day: -daysBeforeDue, label: 'Invoice e-mailed', ic: 'receipt', tone: 'info' }];
  if (a.reminder_days_before > 0 && a.reminder_days_before < daysBeforeDue) steps.push({ day: -a.reminder_days_before, label: 'Friendly reminder', ic: 'mail', tone: 'info' });
  steps.push({ day: 0, label: a.autocharge ? 'Due: saved cards charged' : 'Due date', ic: 'calendar', tone: 'due' });
  for (const d of a.overdue_reminder_days || []) if (d > 0) steps.push({ day: d, label: 'Overdue reminder', ic: 'mail', tone: 'warn' });
  if (a.late_fee && a.late_fee.type !== 'none' && a.late_fee.amount > 0) {
    steps.push({ day: a.late_fee_after_days, label: `Late fee ${a.late_fee.type === 'percent' ? fmtRate(a.late_fee.amount) : money(a.late_fee.amount)}`, ic: 'tag', tone: 'warn' });
  }
  if (a.suspend_after_days > 0) steps.push({ day: a.suspend_after_days, label: 'Sites suspended', ic: 'lock', tone: 'bad' });
  if (a.terminate_after_days > 0) steps.push({ day: a.terminate_after_days, label: a.terminate_deletes_sites ? 'Closed, sites deleted' : 'Account closed', ic: 'ban', tone: 'bad' });
  return steps.sort((x, y) => x.day - y.day);
}

const dayText = (d) => (d < 0 ? `${-d} day${d === -1 ? '' : 's'} before` : d === 0 ? 'Due date' : `${d} day${d === 1 ? '' : 's'} late`);

function dunningTimeline(steps) {
  return h('ol', { class: 'dunning', 'aria-label': 'What happens to an unpaid invoice' }, steps.map((s) =>
    h('li', { class: 'dn ' + s.tone }, h('span', { class: 'dn-dot' }, icon(s.ic)), h('span', { class: 'dn-day' }, dayText(s.day)),
      h('span', { class: 'dn-label' }, s.label))));
}

async function settingsAutomation(box) {
  const s = await api('GET', '/billing/invoicing');
  const a = s.automation || {};
  const lf = a.late_fee || { type: 'none', amount: 0 };
  const timeline = h('div', { class: 'dunning-wrap' });
  const days = (name, value, max = 365) => h('span', { class: 'affix suffix' }, h('input', { name, type: 'number', min: 0, max, value: value ?? 0 }),
    h('span', { class: 'affix-text' }, 'days'));
  const feeType = h('select', { name: 'late_fee_type' }, options([['none', 'No late fee'], ['fixed', 'A fixed amount'], ['percent', 'A percentage of the balance']], lf.type));
  const feeBox = h('div');
  const drawFee = (type, amount) => feeBox.replaceChildren(type === 'none' ? '' : field(type === 'percent' ? 'Late fee' : 'Late fee amount',
    type === 'percent' ? percentInput('late_fee_amount', amount) : moneyInput('late_fee_amount', amount)));
  drawFee(lf.type, lf.amount);
  let form = null;
  const read = (f) => {
    const type = f.elements.late_fee_type.value;
    const amt = type === 'percent' ? toRate(f.elements.late_fee_amount.value) : type === 'fixed' ? toMinor(f.elements.late_fee_amount.value) : 0;
    return {
      reminder_days_before: Number(f.elements.reminder_days_before.value || 0),
      overdue_reminder_days: f.elements.overdue_reminder_days.value.split(/[,\s]+/).map(Number).filter((n) => Number.isInteger(n) && n > 0)
        .sort((x, y) => x - y),
      suspend_after_days: Number(f.elements.suspend_after_days.value || 0),
      terminate_after_days: Number(f.elements.terminate_after_days.value || 0),
      terminate_deletes_sites: f.elements.terminate_deletes_sites.checked,
      late_fee: { type, amount: Number.isNaN(amt) || amt == null ? 0 : amt },
      late_fee_after_days: Number(f.elements.late_fee_after_days.value || 0),
      auto_apply_credit: f.elements.auto_apply_credit.checked,
      orders_need_approval: f.elements.orders_need_approval.checked,
      autocharge: f.elements.autocharge.checked,
    };
  };
  const sync = () => {
    if (form) timeline.replaceChildren(dunningTimeline(dunningSteps(read(form), (s.invoice && s.invoice.days_before_due) ?? 7)));
  };
  feeType.addEventListener('change', () => { drawFee(feeType.value, null); sync(); });
  const settings = settingsForm('Reminders and suspension', 'What happens when an invoice isn\'t paid. The timeline follows the numbers as you change them.', [
    timeline,
    h('div', { class: 'grid' },
      field('Friendly reminder', days('reminder_days_before', a.reminder_days_before, 60), 'Days before the due date; 0: none.'),
      field('Overdue reminders', h('input', { name: 'overdue_reminder_days', value: (a.overdue_reminder_days || []).join(', '), placeholder: '1, 3, 7' }),
        'Days after the due date, separated by commas.'),
      field('Suspend sites', days('suspend_after_days', a.suspend_after_days), 'Days after the due date; 0: never. Paying lifts it by itself.'),
      field('Close the account', days('terminate_after_days', a.terminate_after_days), 'Days after the due date; 0: never.')),
    toggle('terminate_deletes_sites', 'Delete the sites of closed accounts', a.terminate_deletes_sites,
      'Off: closed accounts\' sites stay suspended on the server until you delete them.'),
    h('h3', {}, 'Late fee'),
    h('div', { class: 'grid' }, field('Late fee', feeType), feeBox, field('Added', days('late_fee_after_days', a.late_fee_after_days), 'Days after the due date, once.')),
    h('h3', {}, 'Payments and orders'),
    toggle('autocharge', 'Charge saved cards on the due date', a.autocharge ?? true, 'For clients who turned on automatic payment.'),
    toggle('auto_apply_credit', 'Pay new invoices from account credit', a.auto_apply_credit ?? true),
    toggle('orders_need_approval', 'Approve new orders by hand', a.orders_need_approval, 'Paid orders wait under Orders until you accept them.')],
  async (f) => {
    const v = read(f);
    if (v.late_fee.type !== 'none' && !v.late_fee.amount) throw fieldError('late_fee_amount', 'Enter the late fee, or choose no late fee.');
    await saveInvoicing(s, { automation: v });
  });
  form = settings;
  form.addEventListener('input', sync);
  form.addEventListener('change', sync);
  sync();

  const runs = h('div', { 'aria-live': 'polite' });
  const loadRuns = async () => {
    try {
      const r = await api('GET', '/billing/automation');
      const list = listOf(r, 'runs');
      runs.replaceChildren(btable(['Started', 'Took', 'Done', 'Problems'], list.map((x) => [fmtTime(x.started_at || x.at),
        x.finished_at && x.started_at ? `${Math.max(0, Math.round((new Date(x.finished_at) - new Date(x.started_at)) / 1000))} s` : '–',
        h('td', { class: 'small' }, Object.entries(x.counts || {}).filter(([, n]) => n).map(([k, n]) => `${n} ${k.replace(/_/g, ' ')}`).join(', ') || 'nothing to do'),
        h('td', { class: 'small ' + ((x.errors || []).length || x.error ? 'st-failed' : 'muted') }, x.error || (x.errors || []).join('; ') || 'none')]),
      { empty: h('p', { class: 'muted small' }, 'It hasn\'t run yet. It runs every 15 minutes.') }));
    } catch (e) { runs.replaceChildren(h('p', { class: 'muted small' }, e.status === 404 ? 'Not available on this server yet.' : e.message)); }
  };
  const run = actionButton('Run now', async () => { await api('POST', '/billing/automation/run'); notify('Billing automation ran'); await loadRuns(); }, { ic: 'refresh' });
  box.replaceChildren(settings, h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('div', {}, h('h2', {}, 'Recent runs'),
    h('p', { class: 'muted small' }, 'Every 15 minutes WPGenie creates renewal invoices, charges saved cards, sends reminders and suspends or unsuspends accounts.')), run), runs));
  await loadRuns();
}

// ---- Burst minute packs ----

// settingsBurst: the packs of burst minutes clients can buy (a pack's ID
// follows its minutes: b500).
async function settingsBurst(box) {
  const s = await api('GET', '/billing/invoicing');
  const rows = h('div', { class: 'lines packs', role: 'list' });
  const add = (p = {}) => {
    const i = rows.children.length + 1;
    const minutes = h('input', { type: 'number', min: 1, step: 1, 'data-k': 'minutes', value: p.minutes || '', 'aria-label': `Pack ${i}: minutes`, placeholder: '500' });
    const price = h('input', { inputmode: 'decimal', 'data-k': 'price', value: fromMinor(p.price), 'aria-label': `Pack ${i}: price`, placeholder: fromMinor(0) });
    const per = h('span', { class: 'line-amount muted small' });
    const del = h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Remove pack ${i}` }, icon('trash'));
    const row = h('div', { class: 'line pack-line', role: 'listitem', 'data-id': p.id || '' },
      h('span', { class: 'affix suffix' }, minutes, h('span', { class: 'affix-text' }, 'minutes')),
      h('span', { class: 'affix' }, h('span', { class: 'affix-text', 'aria-hidden': 'true' }, currencySymbol()), price), per, del);
    const sync = () => {
      const m = Number(minutes.value), v = toMinor(price.value);
      per.textContent = m > 0 && v > 0 ? `${money(Math.round((v / m) * 60))} an hour` : '';
    };
    row.addEventListener('input', sync);
    del.addEventListener('click', () => { row.remove(); });
    sync();
    rows.append(row);
    return row;
  };
  (s.burst_packs || []).forEach(add);
  const addBtn = h('button', { type: 'button', class: 'ghost' }, icon('plus'), 'Add a pack');
  addBtn.addEventListener('click', () => add().querySelector('input').focus());
  box.replaceChildren(settingsForm('Burst minute packs', 'Clients billed by invoice can buy extra burst minutes when their plan\'s run out. ' +
    'Bought minutes never expire. No packs: nothing is for sale.', [
    h('div', { class: 'lines-head pack-head', 'aria-hidden': 'true' }, h('span', {}, 'Minutes'), h('span', {}, 'Price'), h('span', { class: 'num' }, 'Works out at'), h('span', {}, '')),
    rows, h('div', { class: 'lines-foot' }, addBtn)],
  async () => {
    const seen = new Set();
    const packs = [];
    for (const row of rows.children) {
      const m = Math.trunc(Number(row.querySelector('[data-k=minutes]').value));
      const v = toMinor(row.querySelector('[data-k=price]').value);
      if (!m && v == null) continue;
      if (!(m > 0)) { row.querySelector('[data-k=minutes]').focus(); throw new Error('Each pack needs a number of minutes.'); }
      if (v == null || Number.isNaN(v) || v <= 0) { row.querySelector('[data-k=price]').focus(); throw new Error(`Give the ${fmtNum(m)}-minute pack a price.`); }
      // Keep a pack's ID while its minutes don't change; else b<minutes>.
      let id = row.dataset.id && row.dataset.id.replace(/-\d+$/, '') === `b${m}` ? row.dataset.id : `b${m}`;
      for (let n = 2; seen.has(id); n++) id = `b${m}-${n}`;
      seen.add(id);
      packs.push({ id, minutes: m, price: v });
    }
    await saveInvoicing(s, { burst_packs: packs.sort((a, b) => a.minutes - b.minutes) });
    settingsBurst(box).catch(showError);
  }, 'Save packs'));
}

// ---- Payment methods: Stripe, Razorpay, bank transfer ----

async function settingsPayments(box) {
  const [s, stripe] = await Promise.all([api('GET', '/billing/invoicing').catch((e) => { if (e.status === 404) return null; throw e; }),
    api('GET', '/billing/settings')]);
  const m = (s && s.methods) || {};
  const cards = [];
  const status = (ok, yes, no) => h('span', { class: 'pill ' + (ok ? 'active' : 'provisioning') }, ok ? yes : no);
  const method = (id, change) => saveInvoicing(s, { methods: { ...invoicingBody(s).methods, [id]: { ...invoicingBody(s).methods[id], ...change } } });

  if (s) {
    const st = m.stripe || {};
    cards.push(settingsForm('Card payments with Stripe', null, [
      h('div', { class: 'method-status' }, status(s.stripe_ready, 'Connected', 'Needs keys'),
        h('span', { class: 'muted small' }, s.stripe_ready ? 'Clients pay by card on Stripe\'s page and can save the card.' : 'Add a secret key and the webhook secret below.')),
      toggle('enabled', 'Offer card payments', st.enabled),
      field('Name clients see', h('input', { name: 'name', value: st.name || 'Card', maxlength: 60 }))],
    (f) => method('stripe', { enabled: f.elements.enabled.checked, name: f.elements.name.value.trim() })));
  }
  cards.push(stripeKeysForm(stripe, !!s));
  if (s) {
    const rp = m.razorpay || {};
    cards.push(settingsForm('Razorpay', null, [
      h('div', { class: 'method-status' }, status(rp.key_id && rp.key_secret_set && rp.webhook_secret_set, 'Connected', 'Needs keys'),
        h('span', { class: 'muted small' }, 'UPI, cards, netbanking and wallets in India, through a payment link.')),
      toggle('enabled', 'Offer Razorpay', rp.enabled),
      h('div', { class: 'grid' },
        field('Name clients see', h('input', { name: 'name', value: rp.name || 'UPI, cards & netbanking', maxlength: 60 })),
        field('Key ID', h('input', { name: 'key_id', value: rp.key_id || '', autocomplete: 'off', placeholder: 'rzp_live_…' })),
        field('Key secret', h('input', { name: 'key_secret', type: 'password', autocomplete: 'off', placeholder: rp.key_secret_set ? 'set (unchanged if empty)' : '' })),
        field('Webhook secret', h('input', { name: 'webhook_secret', type: 'password', autocomplete: 'off', placeholder: rp.webhook_secret_set ? 'set (unchanged if empty)' : '' }))),
      rp.webhook_url ? field('Webhook URL', copyText(rp.webhook_url)) : null,
      providerHint('Setting up Razorpay', [
        'In the Razorpay Dashboard open Account & Settings → API keys and generate a key; paste the Key ID and secret here.',
        'Under Webhooks, add a webhook with the URL above, a secret of your choice (paste it here too), and the events payment_link.paid and refund.processed.',
        'Test mode keys (rzp_test_…) work for trying it out.'])],
    (f) => {
      const change = { enabled: f.elements.enabled.checked, name: f.elements.name.value.trim(), key_id: f.elements.key_id.value.trim() };
      if (f.elements.key_secret.value) change.key_secret = f.elements.key_secret.value.trim();
      if (f.elements.webhook_secret.value) change.webhook_secret = f.elements.webhook_secret.value.trim();
      return method('razorpay', change).then(() => settingsPayments(box));
    }));
    const mn = m.manual || {};
    cards.push(settingsForm('Bank transfer', null, [
      toggle('enabled', 'Offer bank transfer', mn.enabled ?? true, 'Clients see your instructions; you record the payment when it arrives.'),
      field('Name clients see', h('input', { name: 'name', value: mn.name || 'Bank transfer', maxlength: 60 })),
      field('Instructions', h('textarea', { name: 'instructions', rows: 5, placeholder: 'Bank: …\nAccount holder: …\nIBAN / account number: …\nSWIFT / IFSC: …' }, mn.instructions || ''),
        'Shown with the invoice number to use as the payment reference.')],
    (f) => method('manual', { enabled: f.elements.enabled.checked, name: f.elements.name.value.trim(), instructions: f.elements.instructions.value })));
  } else {
    cards.unshift(banner('info', 'info', 'Built-in billing isn\'t on this server yet', 'Stripe subscriptions and outgoing webhooks work as before.'));
  }
  box.replaceChildren(...cards);
}

function providerHint(title, steps) {
  return h('details', { class: 'advanced hint' }, h('summary', {}, icon('info'), title), h('ol', { class: 'small' }, steps.map((x) => h('li', {}, x))));
}

// stripeKeysForm is Stripe's keys and the older subscription setup (moved
// here from the old Billing tab: prices mapped to plans, metered bandwidth).
function stripeKeysForm(st, builtIn) {
  const prices = h('textarea', { name: 'prices', rows: 3, placeholder: 'price_123 = pro' },
    Object.entries(st.stripe_prices || {}).map(([k, v]) => `${k} = ${v}`).join('\n'));
  return settingsForm('Stripe keys', 'Secrets are write-only: leave a field empty to keep it.', [
    h('div', { class: 'grid' },
      field('Secret key', h('input', { name: 'secret_key', type: 'password', autocomplete: 'off', placeholder: st.stripe_secret_key_set ? 'set (unchanged if empty)' : 'sk_… or rk_…' }),
        'A restricted key needs write access to Checkout Sessions, Customers, Payment Intents and Refunds.'),
      field('Webhook signing secret', h('input', { name: 'webhook_secret', type: 'password', autocomplete: 'off', placeholder: st.stripe_webhook_secret_set ? 'set (unchanged if empty)' : 'whsec_…' }))),
    field('Webhook URL', copyText(st.stripe_webhook_url || '')),
    providerHint('Setting up Stripe', [
      'In Stripe open Developers → API keys and create a secret (or restricted) key; paste it above.',
      'Open Developers → Webhooks → Add endpoint with the URL above' + (builtIn ? ', and the events checkout.session.completed, payment_intent.succeeded and charge.refunded' : '') +
        '. For subscriptions made in Stripe add customer.subscription.created, .updated and .deleted, invoice.paid and invoice.payment_failed too.',
      'Paste the endpoint\'s signing secret (whsec_…) above.']),
    h('details', { class: 'advanced' }, h('summary', {}, 'Stripe subscriptions (the older setup)'),
      h('p', { class: 'muted small' }, 'Subscriptions created in Stripe become accounts: map each Stripe price to a plan. Metered bandwidth reports usage in MB.'),
      h('div', { class: 'grid' },
        field('Prices → plans', prices, 'One per line: price_… = plan'),
        field('Meter event name', h('input', { name: 'meter_event', value: st.stripe_meter_event || '', placeholder: 'wpgenie_bandwidth' }), 'Optional: bandwidth in MB.')))],
  async (f) => {
    const map = {};
    for (const line of f.elements.prices.value.split('\n')) {
      const [k, v] = line.split('=').map((x) => (x || '').trim());
      if (k && v) map[k] = v;
    }
    const body = { meter_event: f.elements.meter_event.value.trim(), prices: map };
    if (f.elements.webhook_secret.value) body.webhook_secret = f.elements.webhook_secret.value.trim();
    if (f.elements.secret_key.value) body.secret_key = f.elements.secret_key.value.trim();
    await api('PUT', '/billing/settings', body);
    f.elements.webhook_secret.value = '';
    f.elements.secret_key.value = '';
    BILLING = null;
    notify('Stripe settings saved');
  }, 'Save Stripe keys');
}

// ---- Outgoing webhooks (moved here from the old Billing tab) ----

const WEBHOOK_EVENTS = ['account.created', 'account.suspended', 'account.unsuspended', 'account.terminated', 'account.payment_failed',
  'plan.changed', 'usage.threshold', 'burst.threshold', 'site.created', 'site.deleted'];

async function settingsWebhooks(box) {
  const [settings, hooks, deliveries] = await Promise.all([api('GET', '/billing/settings').catch(() => ({})), api('GET', '/billing/webhooks'),
    api('GET', '/billing/deliveries?limit=50')]);
  const events = settings.events || WEBHOOK_EVENTS;
  const reload = () => settingsWebhooks(box).catch((e) => box.replaceChildren(loadError(e)));
  const form = h('form', { class: 'webhook-add' },
    h('div', { class: 'with-button' }, h('input', { name: 'url', type: 'url', placeholder: 'https://billing.example.com/wpgenie', required: true, 'aria-label': 'Endpoint URL' }),
      h('button', { type: 'submit' }, icon('plus'), 'Add endpoint')),
    h('fieldset', { class: 'field' }, h('legend', { class: 'field-label' }, 'Events (none ticked: all)'),
      h('div', { class: 'check-list' }, events.map((ev) => h('label', { class: 'check small' }, h('input', { type: 'checkbox', name: 'event', value: ev }), ev)))));
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    try {
      const r = await api('POST', '/billing/webhooks', { url: form.elements.url.value.trim(),
        events: [...form.querySelectorAll('input[name=event]:checked')].map((c) => c.value) });
      showSecret('Webhook signing secret', [r.secret, '', 'Verify X-WPGenie-Signature with it on every delivery.']);
      reload();
    } catch (err) { showError(err); }
  });
  const act = (label, fn, cls) => actionButton(label, async () => { await fn(); reload(); }, { cls });
  box.replaceChildren(
    h('div', { class: 'card' }, h('h2', {}, 'Webhooks'),
      h('p', { class: 'muted small' }, 'Events (accounts, plans, invoices, usage, sites) are posted as JSON, signed with ',
        h('code', {}, 'X-WPGenie-Signature: t=…,v1=HMAC-SHA256(secret, "t.body")'), ', and retried with backoff for about a day and a half.'),
      form,
      btable(['URL', 'Events', 'State', ''], hooks.map((ep) => [h('td', { class: 'wrap' }, ep.url), h('td', { class: 'small' }, ep.events.length ? ep.events.join(', ') : 'all'),
        h('td', {}, bpill(ep.enabled ? 'on' : 'off', ep.enabled ? 'enabled' : 'disabled')),
        h('td', { class: 'row-actions' },
          act('Test', () => api('POST', `/billing/webhooks/${ep.id}/test`).then(() => notify('Test event sent'))),
          act(ep.enabled ? 'Disable' : 'Enable', () => api('PUT', `/billing/webhooks/${ep.id}`, { enabled: !ep.enabled })),
          act('New secret', async () => {
            if (!await ask('Issue a new signing secret? The old one stops working at once.', { ok: 'New secret', danger: true })) return;
            const r = await api('PUT', `/billing/webhooks/${ep.id}`, { rotate_secret: true });
            showSecret('Webhook signing secret', [r.secret]);
          }),
          act('Delete', async () => { if (await ask(`Delete ${ep.url}?`)) await api('DELETE', `/billing/webhooks/${ep.id}`); }, 'ghost danger'))]),
      { empty: h('p', { class: 'muted small' }, 'No endpoints yet.') })),
    h('div', { class: 'card' }, h('h2', {}, 'Deliveries'),
      btable(['Time', 'Event', 'Endpoint', 'State', 'Attempts', 'Last answer', ''], deliveries.map((d) => [fmtTime(d.created_at), d.event, String(d.endpoint_id),
        h('td', {}, status(d.status)), String(d.attempts), h('td', { class: 'wrap small' }, d.last_error || (d.last_status ? String(d.last_status) : '')),
        h('td', {}, d.status === 'delivered' ? null : act('Retry', () => api('POST', `/billing/deliveries/${d.id}/retry`)))]),
      { empty: h('p', { class: 'muted small' }, 'Nothing sent yet.') })));
}
