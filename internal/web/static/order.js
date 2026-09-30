'use strict';
// The public order page (/order.html): choose a plan, give your details,
// review the price with taxes and pay. Ordering creates the account, signs
// you in and sends you to pay; the server checks everything again. A page
// of its own (no dashboard scripts), so it has its own small helpers.

// diskText shows a plan's disk as the operator entered it: whole GB when
// it is a multiple of 1024 MB, else MB (5000 MB is not "4.9 GB").
const diskText = (mb) => (mb % 1024 === 0 ? `${(mb / 1024).toLocaleString()} GB` : `${mb.toLocaleString()} MB`);

const $ = (sel, el = document) => el.querySelector(sel);

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === 'class') el.className = v;
    else if (k.startsWith('on')) el.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) el.setAttribute(k, v === true ? '' : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return el;
}

function icon(name) {
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
  svg.setAttribute('class', 'i');
  svg.setAttribute('aria-hidden', 'true');
  const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
  use.setAttribute('href', '#i-' + name);
  svg.append(use);
  return svg;
}

async function api(method, path, body) {
  const res = await fetch('/api/v1' + path, {
    method, credentials: 'same-origin',
    headers: { 'X-Requested-With': 'wpgenie', ...(body ? { 'Content-Type': 'application/json' } : {}) },
    body: body ? JSON.stringify(body) : undefined,
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error((data && data.error) || (res.status === 429 ? 'Too many attempts: please wait a little and try again.' : res.statusText));
    err.status = res.status;
    err.field = data && data.field;
    throw err;
  }
  return data;
}

// ---- Money and plans ----

const S = { catalog: null, step: 0, cycle: 'monthly', plan: null, quote: null, quoting: 0 };

const CYCLES = [
  { id: 'monthly', months: 1, label: 'Monthly', per: 'month', adverb: 'monthly' },
  { id: 'quarterly', months: 3, label: 'Quarterly', per: '3 months', adverb: 'every 3 months' },
  { id: 'semiannually', months: 6, label: '6 months', per: '6 months', adverb: 'every 6 months' },
  { id: 'annually', months: 12, label: 'Yearly', per: 'year', adverb: 'yearly' },
  { id: 'biennially', months: 24, label: '2 years', per: '2 years', adverb: 'every 2 years' },
  { id: 'triennially', months: 36, label: '3 years', per: '3 years', adverb: 'every 3 years' },
];
const cycleOf = (id) => CYCLES.find((c) => c.id === id) || CYCLES[0];

const FEATURE_NAMES = { staging: 'Staging sites', backups: 'Automatic backups', sftp: 'SFTP access', files: 'File manager',
  phpmyadmin: 'Database manager', certificates: 'Your own SSL certificates', cdn: 'CDN integration', smtp: 'E-mail from WordPress',
  burst: 'Extra capacity when traffic spikes' };

let FMT = null;
function money(minor) {
  const c = (S.catalog && S.catalog.currency) || { code: 'USD', decimals: 2 };
  const d = Number.isInteger(c.decimals) ? c.decimals : 2;
  if (!FMT) {
    const opts = { style: 'currency', currency: c.code, minimumFractionDigits: d, maximumFractionDigits: d };
    try { FMT = new Intl.NumberFormat(undefined, { ...opts, currencyDisplay: 'narrowSymbol' }); } catch (e) {
      try { FMT = new Intl.NumberFormat(undefined, opts); } catch (e2) { FMT = { format: (v) => `${c.symbol || c.code + ' '}${v.toFixed(d)}` }; }
    }
  }
  return FMT.format((minor || 0) / 10 ** d);
}
const fmtRate = (r) => `${Number((r / 100).toFixed(3))}%`;

const priceOf = (p, cycle) => (p.prices && p.prices[cycle]) || null;
const plans = () => (S.catalog.plans || []).filter((p) => CYCLES.some((c) => priceOf(p, c.id)));
const offeredCycles = () => CYCLES.filter((c) => plans().some((p) => priceOf(p, c.id)));

// saving is the most a longer cycle saves over paying monthly, in percent.
function saving(cycle) {
  let best = 0;
  for (const p of plans()) {
    const m = priceOf(p, 'monthly'), x = priceOf(p, cycle.id);
    if (m && x && m.price > 0) best = Math.max(best, 1 - x.price / (m.price * cycle.months));
  }
  return Math.round(best * 100);
}

function limitsOf(p) {
  const l = p.limits || p;
  const out = [];
  out.push(l.max_sites ? `${l.max_sites} WordPress site${l.max_sites === 1 ? '' : 's'}` : 'Unlimited sites');
  if (l.disk_mb != null) out.push(l.disk_mb ? `${diskText(l.disk_mb)} storage` : 'Unlimited storage');
  if (l.bandwidth_gb != null) out.push(l.bandwidth_gb ? `${l.bandwidth_gb} GB bandwidth a month` : 'Unmetered bandwidth');
  return out;
}

// ---- Steps ----

const STEPS = ['Plan', 'Your details', 'Review and pay'];
const view = { steps: null, panels: [], heads: [] };

function goStep(i) {
  S.step = i;
  view.steps.replaceChildren(...STEPS.map((label, k) => {
    const cls = k < i ? 'done' : k === i ? 'current' : '';
    const inner = [h('span', { class: 'wiz-n' }, k < i ? icon('check') : String(k + 1)), label];
    // Steps already done can be revisited.
    return h('li', { class: cls, 'aria-current': k === i ? 'step' : null }, k < i ? h('button', { type: 'button', class: 'link', onclick: () => goStep(k) }, inner) : inner);
  }));
  view.panels.forEach((p, k) => { p.hidden = k !== i; });
  if (i === 2) { drawSummary(); refreshQuote(); }
  window.scrollTo(0, 0);
  view.heads[i].focus({ preventScroll: true });
}

function stepHead(title, sub) {
  const h2 = h('h2', { tabindex: '-1', class: 'step-title' }, title);
  view.heads.push(h2);
  return h('div', { class: 'step-head' }, h2, sub ? h('p', { class: 'muted' }, sub) : null);
}

// Step 1: plans as cards, with a switch between billing periods.
function planStep() {
  const cycles = h('div', { class: 'seg cycle-switch', role: 'radiogroup', 'aria-label': 'Billing period' });
  const cards = h('div', { class: 'plan-grid' });
  const drawCycles = () => cycles.replaceChildren(...offeredCycles().map((c) => {
    const save = c.months > 1 ? saving(c) : 0;
    return h('label', {}, h('input', { type: 'radio', name: 'cycle', value: c.id, checked: c.id === S.cycle }),
      h('span', {}, c.label, save > 0 ? h('span', { class: 'save' }, `save ${save}%`) : null));
  }));
  const drawCards = () => cards.replaceChildren(...plans().map((p) => {
    const pr = priceOf(p, S.cycle), c = cycleOf(S.cycle);
    const choose = h('button', { type: 'button', disabled: !pr, 'aria-label': `Choose ${p.name}` }, pr ? 'Choose' : `Not offered ${c.adverb}`);
    choose.addEventListener('click', () => { S.plan = p; S.quote = null; goStep(1); });
    return h('article', { class: 'plan-card' + (S.plan && S.plan.id === p.id ? ' picked' : '') },
      h('h3', {}, p.name),
      p.description ? h('p', { class: 'muted small' }, p.description) : null,
      pr ? h('p', { class: 'price' }, h('span', { class: 'amount' }, money(pr.price)), h('span', { class: 'per' }, ` / ${c.per}`)) : h('p', { class: 'price muted' }, '–'),
      pr && c.months > 1 ? h('p', { class: 'muted small' }, `That's ${money(Math.round(pr.price / c.months))} a month`) : null,
      pr && pr.setup_fee ? h('p', { class: 'muted small' }, `+ ${money(pr.setup_fee)} one-time setup`) : null,
      h('ul', { class: 'plan-features' }, [...limitsOf(p), ...(p.features || []).map((f) => FEATURE_NAMES[f] || f)].map((x) => h('li', {}, icon('check'), x))),
      choose);
  }));
  cycles.addEventListener('change', (e) => { S.cycle = e.target.value; drawCards(); });
  if (!offeredCycles().some((c) => c.id === S.cycle)) S.cycle = offeredCycles()[0].id;
  drawCycles();
  drawCards();
  return h('section', { class: 'step' }, stepHead('Choose your plan', 'Every site gets HTTPS and a firewall in front of WordPress. You can change plan later.'),
    offeredCycles().length > 1 ? cycles : null, cards,
    S.catalog.tax_inclusive ? h('p', { class: 'muted small center' }, 'Prices include tax.') : h('p', { class: 'muted small center' }, 'Taxes, if any, are added at checkout.'));
}

// Step 2: who you are, and the user you'll sign in with.
function detailsStep() {
  const input = (name, label, attrs = {}, help) => {
    const id = 'f-' + name.replace('.', '-');
    const el = h('input', { id, name, ...attrs });
    const helpEl = help ? h('span', { class: 'field-help', id: id + '-help' }, help) : null;
    if (helpEl) el.setAttribute('aria-describedby', helpEl.id);
    const err = h('span', { class: 'field-error', id: id + '-err', hidden: true });
    return h('div', { class: 'field' }, h('label', { for: id }, label), el, helpEl, err);
  };
  const country = h('select', { id: 'f-contact-country', name: 'contact.country', autocomplete: 'country', required: true },
    h('option', { value: '' }, 'Choose…'), countryList().map(([code, name]) => h('option', { value: code, selected: code === guessCountry() }, name)));
  const stateField = input('contact.state', 'State / province', { autocomplete: 'address-level1' });
  const syncState = () => {
    const need = (S.catalog.countries_need_state || STATE_COUNTRIES).includes(country.value);
    const el = $('input', stateField);
    el.required = need;
    $('label', stateField).textContent = need ? 'State / province' : 'State / province (optional)';
  };
  country.addEventListener('change', syncState);

  const pw = h('input', { id: 'f-user-password', name: 'user.password', type: 'password', autocomplete: 'new-password', minlength: 12, required: true,
    'aria-describedby': 'pw-strength' });
  const show = h('button', { type: 'button', class: 'ghost', 'aria-pressed': 'false' }, 'Show');
  show.addEventListener('click', () => {
    const on = pw.type === 'password';
    pw.type = on ? 'text' : 'password';
    show.textContent = on ? 'Hide' : 'Show';
    show.setAttribute('aria-pressed', String(on));
  });
  const meter = h('meter', { min: 0, max: 4, low: 2, high: 3, optimum: 4, value: 0, 'aria-hidden': 'true' });
  const strength = h('span', { id: 'pw-strength', class: 'field-help', 'aria-live': 'polite' }, 'At least 12 characters.');
  pw.addEventListener('input', () => {
    const v = pw.value;
    let score = 0;
    if (v.length >= 12) score++;
    if (v.length >= 16) score++;
    if (/[a-z]/.test(v) && /[A-Z]/.test(v)) score++;
    if (/\d/.test(v) && /[^A-Za-z0-9]/.test(v)) score++;
    meter.value = v.length < 12 ? Math.min(1, score) : score;
    strength.textContent = v.length < 12 ? `${12 - v.length} more character${12 - v.length === 1 ? '' : 's'} needed.`
      : ['Weak: add more characters.', 'Weak: add more characters.', 'Good.', 'Strong.', 'Very strong.'][score];
  });

  const form = h('form', { class: 'step', id: 'details-form', novalidate: false },
    stepHead('Your details', 'For your invoices and your account. We never share them.'),
    h('fieldset', { class: 'card' }, h('legend', {}, 'Billing contact'), h('div', { class: 'grid' },
      input('contact.first_name', 'First name', { autocomplete: 'given-name', required: true }),
      input('contact.last_name', 'Last name', { autocomplete: 'family-name', required: true }),
      input('contact.company', 'Company (optional)', { autocomplete: 'organization' }),
      input('contact.email', 'E-mail', { type: 'email', autocomplete: 'email', required: true }, 'Invoices and receipts go here.'),
      input('contact.phone', 'Phone (optional)', { type: 'tel', autocomplete: 'tel' }),
      input('contact.address1', 'Address', { autocomplete: 'address-line1', required: true }),
      input('contact.address2', 'Address, line 2 (optional)', { autocomplete: 'address-line2' }),
      input('contact.city', 'City', { autocomplete: 'address-level2', required: true }),
      stateField,
      input('contact.postcode', 'Postcode', { autocomplete: 'postal-code' }),
      h('div', { class: 'field' }, h('label', { for: country.id }, 'Country'), country, h('span', { class: 'field-error', id: 'f-contact-country-err', hidden: true })),
      input('contact.tax_id', 'Tax ID (optional)', { autocomplete: 'off' }, 'VAT, GST or a similar number, if you order as a business.'))),
    h('fieldset', { class: 'card' }, h('legend', {}, 'Your login'), h('div', { class: 'grid' },
      input('user.username', 'Username', { autocomplete: 'username', required: true, minlength: 3, maxlength: 64, spellcheck: 'false', autocapitalize: 'none' }),
      h('div', { class: 'field' }, h('label', { for: pw.id }, 'Password'), h('div', { class: 'with-button' }, pw, show), meter, strength,
        h('span', { class: 'field-error', id: 'f-user-password-err', hidden: true })))),
    // Left empty by people; filled by bots (it's off-screen and skipped).
    h('div', { class: 'hp', 'aria-hidden': 'true' }, h('label', {}, 'Website', h('input', { name: 'website', tabindex: '-1', autocomplete: 'off' }))),
    h('div', { class: 'actions' }, h('button', { type: 'button', class: 'ghost', onclick: () => goStep(0) }, icon('back'), 'Back'),
      h('button', { type: 'submit' }, 'Continue')));
  syncState();
  form.addEventListener('submit', (e) => {
    e.preventDefault();
    clearErrors();
    if (form.reportValidity()) goStep(2);
  });
  return form;
}

const val = (name) => { const el = $('#details-form').elements[name]; return el ? el.value.trim() : ''; };

function contact() {
  return Object.fromEntries(['first_name', 'last_name', 'company', 'email', 'phone', 'address1', 'address2', 'city', 'state', 'postcode', 'country', 'tax_id']
    .map((k) => [k, val('contact.' + k)]));
}

// Step 3: the price with taxes, a promotion code, how to pay.
function reviewStep() {
  const summary = h('div', { class: 'order-summary' });
  const promo = h('input', { id: 'f-promo', name: 'promo', autocomplete: 'off', spellcheck: 'false', autocapitalize: 'characters', placeholder: 'e.g. LAUNCH20' });
  const promoMsg = h('span', { class: 'field-help', id: 'promo-msg', 'aria-live': 'polite' });
  promo.setAttribute('aria-describedby', 'promo-msg');
  const apply = h('button', { type: 'button', class: 'ghost' }, icon('tag'), 'Apply');
  apply.addEventListener('click', () => refreshQuote());
  promo.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); refreshQuote(); } });
  const quote = h('div', { class: 'quote-box', 'aria-live': 'polite', id: 'quote' });
  const methods = (S.catalog.methods || []);
  const methodList = h('div', { class: 'choices' }, methods.map((m, i) => h('label', { class: 'choice' },
    h('input', { type: 'radio', name: 'method', value: m.id, checked: i === 0, required: true }),
    h('span', {}, h('strong', {}, m.name), m.description ? h('span', { class: 'muted small' }, m.description) : null))));
  const terms = S.catalog.terms_url
    ? h('span', {}, 'I accept the ', h('a', { href: S.catalog.terms_url, target: '_blank', rel: 'noopener' }, 'terms of service'))
    : h('span', {}, 'I agree to be billed for this plan until I cancel');
  const err = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const submit = h('button', { type: 'submit', class: 'place' }, icon('lock'), 'Place order');
  const form = h('form', { class: 'step', id: 'review-form' },
    stepHead('Review and pay'),
    h('div', { class: 'review-grid' },
      h('div', {},
        h('div', { class: 'card' }, summary),
        h('div', { class: 'card' }, h('h3', {}, 'How would you like to pay?'),
          methods.length ? methodList : h('p', { class: 'muted small' }, 'No payment method is available right now: please contact us.')),
        h('label', { class: 'terms' }, h('input', { type: 'checkbox', name: 'accept_terms', required: true }), terms)),
      h('div', { class: 'card sticky' },
        h('div', { class: 'field' }, h('label', { for: promo.id }, 'Promotion code'), h('div', { class: 'with-button' }, promo, apply), promoMsg),
        quote, err, submit)),
    h('div', { class: 'actions' }, h('button', { type: 'button', class: 'ghost', onclick: () => goStep(1) }, icon('back'), 'Back')));
  form.addEventListener('submit', (e) => { e.preventDefault(); placeOrder(form, submit, err); });
  return form;
}

function drawSummary() {
  const p = S.plan, c = cycleOf(S.cycle), ct = contact();
  const change = (label, step) => h('button', { type: 'button', class: 'link', onclick: () => goStep(step) }, label);
  $('.order-summary').replaceChildren(
    h('div', { class: 'sum-row' }, h('div', {}, h('span', { class: 'muted small' }, 'Plan'), h('strong', {}, `${p.name} · ${c.label.toLowerCase()}`)), change('Change', 0)),
    h('div', { class: 'sum-row' }, h('div', {}, h('span', { class: 'muted small' }, 'Billed to'),
      h('strong', {}, [ct.first_name, ct.last_name].join(' ') + (ct.company ? `, ${ct.company}` : '')),
      h('span', { class: 'muted small' }, [ct.email, ct.city, countryName(ct.country)].filter(Boolean).join(' · '))), change('Edit', 1)));
}

async function refreshQuote() {
  const n = ++S.quoting;
  const box = $('#quote'), msg = $('#promo-msg'), promo = $('#f-promo').value.trim();
  box.setAttribute('aria-busy', 'true');
  try {
    const ct = contact();
    const q = await api('POST', '/store/quote', { plan_id: S.plan.id, cycle: S.cycle, promo, country: ct.country, state: ct.state, tax_id: ct.tax_id });
    if (n !== S.quoting) return;
    S.quote = q;
    // A promotion is an item and the discount total: shown once, under
    // the subtotal, named after it.
    const items = q.items || [], promos = items.filter((it) => it.kind === 'discount');
    const rows = items.filter((it) => it.kind !== 'discount').map((it) => [it.description, money(it.amount)]);
    if ((q.tax_lines || []).length || q.discount) rows.push(['Subtotal', money(q.subtotal)]);
    if (q.discount) rows.push([promos.map((it) => it.description).join(', ') || 'Discount', '−' + money(q.discount)]);
    for (const t of q.tax_lines || []) rows.push([`${t.name} ${fmtRate(t.rate)}`, money(t.amount)]);
    box.replaceChildren(h('dl', { class: 'quote-lines' }, rows.map(([k, v]) => h('div', {}, h('dt', {}, k), h('dd', {}, v))),
      h('div', { class: 'grand' }, h('dt', {}, 'Due today'), h('dd', {}, money(q.total)))),
    q.recurring_total != null ? h('p', { class: 'muted small' }, `Then ${money(q.recurring_total)} ${cycleOf(S.cycle).adverb}, until you cancel.`) : null);
    msg.textContent = q.promo && promo ? q.promo.message || (q.promo.valid ? 'Code applied.' : 'This code doesn\'t apply.') : '';
    msg.className = 'field-help ' + (q.promo && promo ? (q.promo.valid ? 'ok' : 'bad') : '');
  } catch (e) {
    if (n !== S.quoting) return;
    box.replaceChildren(h('p', { class: 'form-error' }, e.message));
  } finally { box.removeAttribute('aria-busy'); }
}

function clearErrors() {
  document.querySelectorAll('.field-error').forEach((x) => { x.hidden = true; x.textContent = ''; });
  document.querySelectorAll('[aria-invalid]').forEach((x) => { x.removeAttribute('aria-invalid'); x.removeAttribute('aria-errormessage'); });
}

// showFieldError puts the server's complaint next to its field, on its step.
function showFieldError(field, message) {
  const el = document.querySelector(`[name="${CSS.escape(field)}"]`);
  const err = el && el.id && document.getElementById(el.id + '-err');
  if (el) el.setAttribute('aria-invalid', 'true');
  // Fields without a place for a message (the promotion code) use the
  // form's own.
  if (!err) return false;
  const step = view.panels.findIndex((p) => p.contains(el));
  if (step >= 0 && step !== S.step) goStep(step);
  err.textContent = message;
  err.hidden = false;
  el.setAttribute('aria-errormessage', err.id);
  el.focus();
  return true;
}

async function placeOrder(form, btn, err) {
  err.hidden = true;
  clearErrors();
  if (!form.reportValidity()) return;
  btn.disabled = true;
  btn.lastChild.textContent = 'Placing your order…';
  try {
    const r = await api('POST', '/store/orders', {
      plan_id: S.plan.id, cycle: S.cycle, promo: $('#f-promo').value.trim(), method: form.elements.method ? form.elements.method.value : '',
      accept_terms: form.elements.accept_terms.checked, contact: contact(),
      user: { username: val('user.username'), password: $('#details-form').elements['user.password'].value },
      website: $('#details-form').elements.website.value,
    });
    const next = r.next || {};
    if (next.redirect_url) {
      const url = new URL(next.redirect_url, location.href);
      if (url.protocol !== 'https:' && url.origin !== location.origin) throw new Error('The payment page address looks wrong: please contact us.');
      btn.lastChild.textContent = 'Taking you to the payment page…';
      location.assign(url.href);
      return;
    }
    if (next.instructions != null) { done(r, next); return; }
    location.assign(`/#/billing/invoices/${encodeURIComponent(r.invoice_id)}` + (next.paid ? '?paid=1' : ''));
  } catch (e) {
    if (!(e.field && showFieldError(e.field, e.message))) {
      err.textContent = e.message;
      err.hidden = false;
    }
    btn.disabled = false;
    btn.lastChild.textContent = 'Place order';
  }
}

// done: an order to pay by bank transfer.
function done(r, next) {
  const app = $('#order-app');
  const copy = h('button', { type: 'button', class: 'ghost' }, icon('copy'), 'Copy');
  copy.addEventListener('click', () => navigator.clipboard.writeText(next.reference || '').then(() => { copy.lastChild.textContent = 'Copied'; }).catch(() => {}));
  const title = h('h2', { tabindex: '-1' }, 'Thank you! Your order is in.');
  app.replaceChildren(h('section', { class: 'card order-done' }, h('span', { class: 'done-icon' }, icon('check')), title,
    h('p', {}, `Please transfer ${S.quote ? money(S.quote.total) : 'the amount due'} using these details. Your account is activated as soon as the payment arrives.`),
    h('pre', { class: 'instructions' }, next.instructions),
    next.reference ? h('div', { class: 'field' }, h('span', { class: 'muted small' }, 'Payment reference (please quote it)'),
      h('div', { class: 'copy-field' }, h('code', {}, next.reference), copy)) : null,
    h('p', { class: 'muted small' }, 'We\'ve e-mailed you these details too. You\'re signed in: your invoice and account are in your dashboard.'),
    h('a', { class: 'button', href: `/#/billing/invoices/${encodeURIComponent(r.invoice_id)}` }, 'Go to your dashboard')));
  title.focus();
}

function closed(message) {
  $('#order-app').replaceChildren(h('section', { class: 'card order-done' }, h('span', { class: 'done-icon muted' }, icon('alert')),
    h('h2', {}, 'Ordering isn\'t open right now'), h('p', { class: 'muted' }, message),
    h('a', { class: 'button ghost', href: '/' }, 'Sign in')));
}

// ---- Start ----

function syncTheme() {
  const light = document.documentElement.dataset.theme === 'light';
  const b = $('#theme-toggle');
  b.setAttribute('aria-label', light ? 'Switch to dark theme' : 'Switch to light theme');
  b.title = b.getAttribute('aria-label');
}

document.addEventListener('DOMContentLoaded', async () => {
  syncTheme();
  $('#theme-toggle').addEventListener('click', () => {
    const next = document.documentElement.dataset.theme === 'light' ? 'dark' : 'light';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('wpgenie_theme', next); } catch (e) { /* this page only */ }
    syncTheme();
  });
  try {
    S.catalog = await api('GET', '/store/catalog');
  } catch (e) {
    closed(e.status === 404 ? 'This server doesn\'t take orders online. Please contact us to order.' : `The plans didn't load (${e.message}). Please try again in a moment.`);
    return;
  }
  const company = (S.catalog.company && S.catalog.company.name) || '';
  if (company) {
    $('#order-company').textContent = company;
    document.title = `Order hosting · ${company}`;
  }
  const logo = S.catalog.company && S.catalog.company.logo_url;
  // The page's CSP allows images from this server only.
  if (logo && new URL(logo, location.href).origin === location.origin) {
    $('#order-brand .logo').replaceChildren(h('img', { src: logo, alt: '' }));
    $('#order-brand .logo').classList.add('custom');
  }
  if (!S.catalog.enabled || !plans().length) {
    closed('No plans are for sale at the moment. Please check back soon, or contact us.');
    return;
  }
  // A plan and period can be linked to: /order.html?plan=pro&cycle=annually.
  const q = new URLSearchParams(location.search);
  if (q.get('cycle') && CYCLES.some((c) => c.id === q.get('cycle'))) S.cycle = q.get('cycle');
  view.steps = h('ol', { class: 'wiz-steps order-steps', 'aria-label': 'Steps' });
  view.panels = [planStep(), detailsStep(), reviewStep()];
  $('#order-app').replaceChildren(h('h1', { class: 'center order-title' }, 'Get your WordPress hosting'), view.steps, ...view.panels);
  const pick = q.get('plan') && plans().find((p) => p.id === q.get('plan') && priceOf(p, S.cycle));
  if (pick) S.plan = pick;
  goStep(pick ? 1 : 0);
});
