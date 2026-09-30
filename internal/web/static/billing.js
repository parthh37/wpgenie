'use strict';
// Billing: the staff Billing tab (overview, invoices, transactions, orders;
// promotions, tax rules and settings are in billing-admin.js, e-mail in
// emails.js), an account's Billing panel, plan prices, and what the client
// area (clientarea.js) shares with them: money, invoice documents, dialogs
// with forms, the income chart. Shares api(), h(), ask(), toast(), notify(),
// icon(), svgEl(), applyRoute(), routing and ME with app.js and ux.js.
//
// Amounts are integer minor units as the API has them (11800 is 118.00):
// they're only divided to be shown, and multiplied back from inputs.

// ---- Store settings: currency and methods (GET /billing/config) ----

// BILLING is the store's configuration as the client area needs it:
// {currency, company, methods, tax_inclusive, enabled}, or {unavailable}
// on a server without built-in billing. Reloaded when the tab opens.
let BILLING = null;

async function billingConfig(fresh) {
  if (BILLING && !fresh) return BILLING;
  try {
    BILLING = await api('GET', '/billing/config');
  } catch (e) {
    if (e.status !== 404) throw e;
    BILLING = { unavailable: true };
  }
  return BILLING;
}

function currency() {
  const c = (BILLING && BILLING.currency) || {};
  return { code: c.code || 'USD', symbol: c.symbol || '', decimals: Number.isInteger(c.decimals) ? c.decimals : 2 };
}

const companyName = () => (BILLING && ((BILLING.company && BILLING.company.name) || BILLING.company_name)) || '';

// billingMethods is the payment methods clients may choose: [{id, name, description}].
function billingMethods() {
  const m = BILLING && BILLING.methods;
  if (Array.isArray(m)) return m.filter((x) => x.enabled !== false);
  // Also accept {stripe: {enabled, name}, …}.
  return Object.entries(m || {}).filter(([, v]) => v && v.enabled !== false).map(([id, v]) => ({ id, ...v }));
}

// ---- Money, percentages, dates ----

const MONEY_FORMATS = new Map();

function moneyFormat(compact) {
  const c = currency(), key = `${c.code}:${c.decimals}:${compact ? 1 : 0}`;
  if (!MONEY_FORMATS.has(key)) {
    const opts = compact
      ? { style: 'currency', currency: c.code, notation: 'compact', maximumFractionDigits: 1 }
      : { style: 'currency', currency: c.code, minimumFractionDigits: c.decimals, maximumFractionDigits: c.decimals };
    let f;
    // "$", not "US$": the store has one currency, so the short symbol is clear.
    try { f = new Intl.NumberFormat(undefined, { ...opts, currencyDisplay: 'narrowSymbol' }); } catch (e) {
      try { f = new Intl.NumberFormat(undefined, opts); } catch (e2) { // a code Intl doesn't know: the symbol and the number
        f = { format: (v) => `${c.symbol || c.code + ' '}${v.toFixed(compact ? 0 : c.decimals)}`, formatToParts: () => [] };
      }
    }
    MONEY_FORMATS.set(key, f);
  }
  return MONEY_FORMATS.get(key);
}

// money(11800) is "$118.00" in the store's currency; compact: "$1.2K".
const money = (minor, compact) => (minor == null || Number.isNaN(minor) ? '–' : moneyFormat(compact).format(minor / 10 ** currency().decimals));

function currencySymbol() {
  const part = moneyFormat(false).formatToParts(0).find((p) => p.type === 'currency');
  return (part && part.value) || currency().symbol || currency().code;
}

// fromMinor is an amount as an input shows it ("118.00").
const fromMinor = (minor) => (minor == null || minor === '' ? '' : (minor / 10 ** currency().decimals).toFixed(currency().decimals));

// toMinor reads an amount typed by a person ("1,234.50", "1.234,50", "$ 12"):
// minor units, null when empty, NaN when it isn't a number.
function toMinor(text) {
  let s = String(text ?? '').trim().replace(/[\s ']/g, '');
  if (!s) return null;
  if (s.includes(',') && s.includes('.')) {
    s = s.lastIndexOf(',') > s.lastIndexOf('.') ? s.replace(/\./g, '').replace(',', '.') : s.replace(/,/g, '');
  } else {
    s = s.replace(',', '.');
  }
  s = s.replace(/[^\d.-]/g, '');
  const n = s === '' || s === '-' ? NaN : Number(s);
  return Number.isFinite(n) ? Math.round(n * 10 ** currency().decimals) : NaN;
}

// Rates are hundredths of a percent (1800 is 18%).
const fmtRate = (r) => `${Number((r / 100).toFixed(3))}%`;
function toRate(text) {
  const n = Number(String(text ?? '').trim().replace(',', '.').replace('%', ''));
  return String(text ?? '').trim() && Number.isFinite(n) ? Math.round(n * 100) : NaN;
}

// Due dates are days: shown in UTC so midnight isn't the day before.
const fmtDate = (t) => (t ? new Date(t).toLocaleDateString([], { dateStyle: 'medium', timeZone: 'UTC' }) : '–');
const dateInput = (t) => (t ? new Date(t).toISOString().slice(0, 10) : '');
const fromDateInput = (v) => (v ? new Date(v + 'T00:00:00Z').toISOString() : null);
const todayInput = (plusDays = 0) => new Date(Date.now() + plusDays * 864e5).toISOString().slice(0, 10);
// Due dates are whole UTC days (shown in UTC by fmtDate): count calendar
// days, not 24-hour spans, so "due 21 Sept" is "today" all that day.
const utcDay = (d) => Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate());
const daysUntil = (t) => Math.round((utcDay(new Date(t)) - utcDay(new Date())) / 864e5);

function monthLabel(ym, long) {
  const [y, m] = String(ym).split('-').map(Number);
  return new Date(Date.UTC(y, (m || 1) - 1, 1)).toLocaleDateString([], { month: long ? 'long' : 'short', year: long ? 'numeric' : undefined, timeZone: 'UTC' });
}

// ---- Words for the API's values ----

const CYCLES = [
  { id: 'monthly', months: 1, label: 'Monthly', per: 'month' },
  { id: 'quarterly', months: 3, label: 'Every 3 months', per: '3 months' },
  { id: 'semiannually', months: 6, label: 'Every 6 months', per: '6 months' },
  { id: 'annually', months: 12, label: 'Yearly', per: 'year' },
  { id: 'biennially', months: 24, label: 'Every 2 years', per: '2 years' },
  { id: 'triennially', months: 36, label: 'Every 3 years', per: '3 years' },
];
const cycleOf = (id) => CYCLES.find((c) => c.id === id) || { id, months: 1, label: id || '–', per: id || '' };

const METHOD_NAMES = { stripe: 'Card', razorpay: 'Razorpay', manual: 'Bank transfer', bank: 'Bank transfer', cash: 'Cash',
  cheque: 'Cheque', other: 'Other', credit: 'Account credit' };
const methodName = (m) => METHOD_NAMES[m] || m || '–';

const INVOICE_KINDS = { order: 'New order', renewal: 'Renewal', plan_change: 'Plan change', overage: 'Bandwidth overage', manual: 'Invoice',
  burst_topup: 'Burst minutes' };
const INVOICE_STATES = { draft: 'Draft', unpaid: 'Unpaid', overdue: 'Overdue', paid: 'Paid', cancelled: 'Cancelled', refunded: 'Refunded',
  partially_refunded: 'Partly refunded' };

// An unpaid invoice past its due date is overdue (the API derives it).
const invoiceState = (inv) => (inv.status === 'unpaid' && inv.overdue ? 'overdue' : inv.status);
const invoiceTitle = (inv) => inv.number || `Proforma #${inv.id}`;
const bpill = (state, label) => h('span', { class: 'pill bp-' + state }, label || String(state).replace(/_/g, ' '));
const invoicePill = (inv) => bpill(invoiceState(inv), INVOICE_STATES[invoiceState(inv)]);

// dueText says when an unpaid invoice is due: "in 3 days", "5 days late".
function dueText(inv) {
  if (inv.status !== 'unpaid' || !inv.due_at) return '';
  const d = daysUntil(inv.due_at);
  if (d > 1) return `in ${d} days`;
  if (d === 1) return 'tomorrow';
  if (d === 0) return 'today';
  return d === -1 ? '1 day late' : `${-d} days late`;
}

// dueLine is dueText in a sentence: "due in 3 days", "5 days late".
const dueLine = (inv) => (daysUntil(inv.due_at) < 0 ? dueText(inv) : `due ${dueText(inv)}`);

// listOf is a list response, bare or wrapped ({invoices: […], counts}).
const listOf = (res, key) => (Array.isArray(res) ? res : (res && (res[key] || res.items)) || []);

const RANK = { viewer: 0, operator: 1, admin: 2 };
const canStaff = (role) => !!ME && !isTenant() && (RANK[ME.role] ?? -1) >= RANK[role || 'viewer'];

// friendly turns a route the server doesn't have (an older server, or a
// part of billing not installed) into words; other errors are as they are.
function friendly(err) {
  if (err && (err.status === 405 || (err.status === 404 && /^not found$/i.test(err.message || '')))) {
    const e = new Error('This isn\'t available on this server yet (it may need an update).');
    e.status = err.status;
    return e;
  }
  return err;
}

// fieldError is an error a dialog shows next to its field.
function fieldError(name, message) {
  const e = new Error(message);
  e.data = { field: name };
  return e;
}

// ---- Building blocks ----

let UID = 0;
const uid = (p) => `${p}-${++UID}`;

// field is a labelled control with optional help, announced with it (the
// help is outside the label so it isn't part of the control's name).
function field(label, control, help, cls) {
  const input = control.matches('input, select, textarea') ? control : control.querySelector('input, select, textarea');
  const helpEl = help ? h('span', { class: 'field-help', id: uid('help') }, help) : null;
  if (helpEl && input) input.setAttribute('aria-describedby', helpEl.id);
  return h('div', { class: 'field' + (cls ? ' ' + cls : '') }, h('label', {}, h('span', { class: 'field-label' }, label), control), helpEl);
}

// toggle is an on/off switch with its label and optional help.
function toggle(name, label, checked, help) {
  const input = h('input', { type: 'checkbox', class: 'switch', name, checked: !!checked });
  const helpEl = help ? h('span', { class: 'field-help', id: uid('help') }, help) : null;
  if (helpEl) input.setAttribute('aria-describedby', helpEl.id);
  return h('div', { class: 'field toggle-field' }, h('label', { class: 'toggle' }, input, h('span', {}, label)), helpEl);
}

// moneyInput is an amount field with the currency's symbol in front.
function moneyInput(name, minor, attrs = {}) {
  return h('span', { class: 'affix' }, h('span', { class: 'affix-text', 'aria-hidden': 'true' }, currencySymbol()),
    h('input', { name, inputmode: 'decimal', autocomplete: 'off', value: fromMinor(minor), placeholder: fromMinor(0), ...attrs }));
}

function percentInput(name, rate, attrs = {}) {
  return h('span', { class: 'affix suffix' }, h('input', { name, inputmode: 'decimal', autocomplete: 'off',
    value: rate == null ? '' : String(rate / 100), ...attrs }), h('span', { class: 'affix-text', 'aria-hidden': 'true' }, '%'));
}

const options = (pairs, current) => pairs.map(([v, label]) => h('option', { value: v, selected: String(v) === String(current ?? '') }, label));

// choice is a radio as a card: a title and a line of explanation.
function choice(name, value, title, text, checked, extra) {
  return h('label', { class: 'choice' }, h('input', { type: 'radio', name, value, checked: !!checked }),
    h('span', {}, h('strong', {}, title), text ? h('span', { class: 'muted small' }, text) : null, extra || null));
}

function emptyState(ic, title, text, ...actions) {
  return h('div', { class: 'empty b-empty' }, h('span', { class: 'empty-icon' }, icon(ic)), h('h2', {}, title),
    text ? h('p', { class: 'muted' }, text) : null, actions.filter(Boolean).length ? h('div', { class: 'b-empty-actions' }, actions) : null);
}

// loadError replaces a screen that couldn't load. A 404 is a server
// without that part of billing (yet): said plainly, not as a failure.
function loadError(err, retry) {
  const missing = err && err.status === 404;
  const again = retry ? h('button', { type: 'button', class: 'ghost' }, icon('refresh'), 'Try again') : null;
  if (again) again.addEventListener('click', retry);
  return h('div', { class: 'empty b-empty', role: 'alert' }, h('span', { class: 'empty-icon bad' }, icon('alert')),
    h('h2', {}, missing ? 'Not available on this server yet' : 'This didn\'t load'),
    h('p', { class: 'muted' }, missing
      ? 'This server doesn\'t have this part of billing yet (it may need an update). Everything else keeps working.'
      : String((err && err.message) || err)),
    again);
}

// actionButton runs an action, disabled while it runs; failures are toasts
// (a cancelled confirmation is not a failure).
function actionButton(label, run, { cls = 'ghost', ic, title } = {}) {
  const b = h('button', { type: 'button', class: cls, title }, ic ? icon(ic) : null, label);
  b.addEventListener('click', async () => {
    b.disabled = true;
    try { await run(); } catch (e) { showError(friendly(e)); } finally { b.disabled = false; }
  });
  return b;
}

function linkButton(label, run, cls = 'link') {
  const b = h('button', { type: 'button', class: cls }, label);
  b.addEventListener('click', run);
  return b;
}

function copyText(text) {
  const b = h('button', { type: 'button', class: 'ghost' }, icon('copy'), 'Copy');
  b.addEventListener('click', async () => {
    try { await navigator.clipboard.writeText(text); notify('Copied to the clipboard'); }
    catch (e) { showError(new Error('Copying didn\'t work here: select the text and copy it.')); }
  });
  return h('div', { class: 'copy-field' }, h('code', {}, text), b);
}

// banner is a notice across a screen: info, ok, warn or bad.
function banner(tone, ic, title, text, ...actions) {
  return h('div', { class: 'bbanner ' + tone, role: tone === 'bad' ? 'alert' : 'status' }, icon(ic),
    h('div', { class: 'bbanner-text' }, h('strong', {}, title), text ? h('p', {}, text) : null),
    actions.filter(Boolean).length ? h('div', { class: 'bbanner-actions' }, actions) : null);
}

// kpi is a number card; with onClick it's a button that goes somewhere.
function kpi(label, value, sub, { tone, onClick, ic } = {}) {
  const el = h(onClick ? 'button' : 'div', { type: onClick ? 'button' : null, class: 'tile kpi' + (tone ? ' ' + tone : '') },
    h('span', { class: 'tile-k' }, ic ? icon(ic) : null, label), h('span', { class: 'tile-v' }, value), sub ? h('span', { class: 'tile-s' }, sub) : null);
  if (onClick) el.addEventListener('click', onClick);
  return el;
}

// btable is a table with a header, numeric columns aligned right, and rows
// that open something when clicked ({cells, open, cls}); the first cell
// should hold a link so the keyboard gets there too.
function btable(cols, rows, { empty, caption } = {}) {
  if (!rows.length) return empty || h('p', { class: 'muted small' }, 'Nothing here yet.');
  const numeric = cols.map((c) => !!(c && c.num));
  const head = h('thead', {}, h('tr', {}, cols.map((c, i) => h('th', { scope: 'col', class: numeric[i] ? 'num' : null }, c && c.label != null ? c.label : c))));
  const body = h('tbody', {}, rows.map((r) => {
    const cells = Array.isArray(r) ? r : r.cells;
    const cls = [r.cls, r.open ? 'clickable' : ''].filter(Boolean).join(' ');
    const tr = h('tr', { class: cls || null }, cells.map((c, i) => (c instanceof Node && c.tagName === 'TD' ? c : h('td', { class: numeric[i] ? 'num' : null }, c))));
    if (r.open) tr.addEventListener('click', (e) => { if (!e.target.closest('a, button, input, select, label, summary')) r.open(); });
    return tr;
  }));
  return h('div', { class: 'table-wrap' }, h('table', { class: 'btable' }, caption ? h('caption', { class: 'sr-only' }, caption) : null, head, body));
}

// subnav is the row of sections within a tab; links, so they open in a new
// tab too.
function subnav(label, items, current, go, cls = '') {
  const nav = h('nav', { class: 'subnav ' + cls, 'aria-label': label }, items.map((it) => {
    const a = h('a', { href: it.href, class: 'subnav-item', 'aria-current': it.key === current ? 'page' : null },
      it.icon ? icon(it.icon) : null, it.label, it.count ? h('span', { class: 'count' }, it.count) : null);
    a.addEventListener('click', (e) => {
      if (e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
      e.preventDefault();
      go(it.key);
    });
    return a;
  }));
  // On narrow screens the row scrolls sideways: fade the edge that has
  // more, and bring the current section into view.
  const edges = () => {
    nav.classList.toggle('more-left', nav.scrollLeft > 2);
    nav.classList.toggle('more-right', nav.scrollLeft + nav.clientWidth < nav.scrollWidth - 2);
  };
  nav.addEventListener('scroll', edges, { passive: true });
  new ResizeObserver(edges).observe(nav);
  requestAnimationFrame(() => {
    nav.querySelector('[aria-current]')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    edges();
  });
  return nav;
}

// chips is a filter: one pressed at a time, with counts when known.
function chips(label, items, current, pick) {
  return h('div', { class: 'chips', role: 'group', 'aria-label': label }, items.map(([key, text, count]) => {
    const b = h('button', { type: 'button', class: 'fchip', 'aria-pressed': String(key === current) }, text,
      count != null ? h('span', { class: 'count' }, fmtNum(count)) : null);
    b.addEventListener('click', () => pick(key));
    return b;
  }));
}

const debounce = (fn, ms) => {
  let t;
  return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); };
};

// ---- Dialogs with forms ----

// openDialog shows a form in a modal dialog and resolves to what submit()
// returned once it succeeded (true if nothing), or null when cancelled.
// submit(form, ctx) may return false to stay open. Errors show inside the
// dialog, next to their field when the API names one ({field}), rather than
// in a toast behind the backdrop. Focus returns where it was.
function openDialog({ title, intro, body, ok = 'Save', danger, wide, submit, cancel = 'Cancel', noOk, onOpen }) {
  return new Promise((resolve) => {
    const back = document.activeElement;
    const titleId = uid('dlg');
    const err = h('p', { class: 'form-error', role: 'alert', hidden: true });
    const okBtn = noOk ? null : h('button', { type: 'submit', class: danger ? 'danger-solid' : null }, ok);
    const cancelBtn = h('button', { type: 'button', class: 'ghost' }, cancel);
    const form = h('form', {}, h('h2', { id: titleId }, title), intro ? h('p', { class: 'muted small dlg-intro' }, intro) : null,
      h('div', { class: 'dlg-body' }, body), err, h('div', { class: 'actions' }, cancelBtn, okBtn));
    const dlg = h('dialog', { class: 'modal bdialog' + (wide ? ' wide' : ''), 'aria-labelledby': titleId }, form);
    // finish closes it once, however it's closed: a button, Escape, or the
    // form's success (not only on the 'close' event, which a browser may
    // hold back while the page isn't rendering).
    let done = false;
    const finish = (v) => {
      if (done) return;
      done = true;
      if (dlg.open) dlg.close();
      dlg.remove();
      if (back && back.isConnected && typeof back.focus === 'function') back.focus();
      resolve(v);
    };
    const ctx = {
      form, dlg, ok: okBtn, cancel: cancelBtn, body: $('.dlg-body', form),
      close(v) { finish(v === undefined ? true : v); },
      error: showErr,
    };
    function showErr(ex) {
      ex = friendly(ex);
      err.textContent = String((ex && ex.message) || ex);
      err.hidden = false;
      const name = ex && ex.data && ex.data.field;
      const target = name && form.elements[name];
      if (target && typeof target.focus === 'function') {
        target.setAttribute('aria-invalid', 'true');
        target.setAttribute('aria-errormessage', err.id || (err.id = uid('err')));
        target.focus();
      }
    }
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      if (!okBtn || okBtn.disabled) return;
      if (!form.reportValidity()) return;
      err.hidden = true;
      form.querySelectorAll('[aria-invalid]').forEach((x) => x.removeAttribute('aria-invalid'));
      okBtn.disabled = true;
      try {
        const r = submit ? await submit(form, ctx) : true;
        if (r !== false && dlg.open) ctx.close(r);
      } catch (ex) { showErr(ex); } finally { okBtn.disabled = false; }
    });
    cancelBtn.addEventListener('click', () => finish(null));
    dlg.addEventListener('close', () => finish(null));
    document.body.append(dlg);
    dlg.showModal();
    const first = form.querySelector('.dlg-body input:not([type=hidden]):not([type=checkbox]):not([type=radio]):not([readonly]), .dlg-body select, .dlg-body textarea');
    (first || okBtn || cancelBtn).focus();
    if (onOpen) onOpen(ctx);
  });
}

// openFrame shows a page (an e-mail) in a sandboxed frame.
function openFrame(title, url) {
  return openDialog({
    title, wide: true, noOk: true, cancel: 'Close',
    body: h('iframe', { class: 'mail-frame', src: url, title, sandbox: 'allow-popups allow-popups-to-escape-sandbox', referrerpolicy: 'no-referrer' }),
  });
}

// ---- Addresses: #/billing/<view>/<id>[?paid=1] ----

function billingRoute() {
  const [path, qs] = location.hash.replace(/^#\/?/, '').split('?');
  const parts = path.split('/').map((x) => { try { return decodeURIComponent(x); } catch (e) { return ''; } });
  if (parts[0] !== 'billing') return { view: '', id: '', sub: '', query: new URLSearchParams() };
  return { view: parts[1] || '', id: parts[2] || '', sub: parts[3] || '', query: new URLSearchParams(qs || '') };
}

// BROUTE is the Billing tab's current address.
let BROUTE = billingRoute();

// billingGo opens a Billing address from anywhere (the Accounts tab too).
function billingGo(view, id, replace) {
  const hash = '#/billing' + (view ? `/${view}` + (id != null && id !== '' ? `/${encodeURIComponent(id)}` : '') : '');
  if (location.hash !== hash) history[replace ? 'replaceState' : 'pushState'](null, '', hash);
  applyRoute();
}

const renderBilling = () => (isTenant() ? renderClientArea(BROUTE) : renderStaffBilling(BROUTE));

// Opening the tab by its address (a link, back, a payment page sending the
// client back) shows that address; clicking it in the navigation shows the
// start (ux.js then sets the address to #/billing).
loaders.billing = () => {
  BROUTE = routing ? billingRoute() : { view: '', id: '', sub: '', query: new URLSearchParams() };
  if (!routing) BILLING = null; // opened afresh: the settings may have changed
  return renderBilling();
};

function invoiceLink(inv, text) {
  const a = h('a', { href: `#/billing/invoices/${inv.id}`, class: 'inv-link' }, text || invoiceTitle(inv));
  a.addEventListener('click', (e) => {
    if (e.metaKey || e.ctrlKey || e.shiftKey || e.button) return;
    e.preventDefault();
    billingGo('invoices', inv.id);
  });
  return a;
}

function backLink(label, view) {
  const a = h('a', { href: '#/billing' + (view ? '/' + view : ''), class: 'back-link' }, icon('back'), label);
  a.addEventListener('click', (e) => { e.preventDefault(); billingGo(view); });
  return a;
}

// openAccount shows an account's detail in the Accounts tab.
function openAccount(id) {
  openTab('accounts');
  showAccountDetail(id).then(() => $('#account-detail').scrollIntoView({ behavior: 'smooth', block: 'start' })).catch(showError);
}

// ---- The staff Billing tab ----

const STAFF_VIEWS = [
  { key: '', label: 'Overview', icon: 'dashboard', render: staffOverview },
  { key: 'invoices', label: 'Invoices', icon: 'receipt', render: staffInvoices },
  { key: 'transactions', label: 'Transactions', icon: 'wallet', render: staffTransactions },
  { key: 'orders', label: 'Orders', icon: 'cart', render: staffOrders, role: 'operator' },
  { key: 'promotions', label: 'Promotions', icon: 'percent', render: (box) => staffPromotions(box), role: 'admin' },
  { key: 'taxes', label: 'Tax rules', icon: 'landmark', render: (box) => staffTaxes(box), role: 'admin' },
  { key: 'email', label: 'E-mail', icon: 'mail-cog', render: (box, r) => staffEmail(box, r), role: 'operator' },
  { key: 'settings', label: 'Settings', icon: 'sliders', render: (box, r) => staffSettings(box, r), role: 'admin' },
];

async function renderStaffBilling(route) {
  const root = $('#billing-root');
  const views = STAFF_VIEWS.filter((v) => canStaff(v.role));
  const view = views.find((v) => v.key === route.view) || views[0];
  const box = h('div', { class: 'bview', 'aria-busy': 'true' }, h('p', { class: 'muted small b-loading' }, 'Loading…'));
  const create = canStaff('admin') ? actionButton('New invoice', () => openInvoiceEditor({}), { cls: '', ic: 'plus' }) : null;
  root.replaceChildren(
    h('div', { class: 'bar' }, h('div', {}, h('h1', {}, 'Billing'),
      h('p', { class: 'muted small bar-sub' }, 'Invoices, payments, and the reminders that collect them.')), h('div', { class: 'bar-actions' }, create)),
    subnav('Billing sections', views.map((v) => ({ key: v.key, label: v.label, icon: v.icon, href: '#/billing' + (v.key ? '/' + v.key : '') })),
      view.key, (k) => billingGo(k)),
    box);
  try {
    await billingConfig();
    await view.render(box, route);
  } catch (e) {
    box.replaceChildren(loadError(e, () => renderBilling()));
  } finally { box.removeAttribute('aria-busy'); }
}

// ---- Overview ----

async function staffOverview(box) {
  if (BILLING.unavailable) {
    // An older server: what it has (Stripe subscriptions, webhooks) is in Settings.
    box.replaceChildren(emptyState('receipt', 'Built-in billing isn\'t on this server yet',
      'Invoices, payments and the order page come with an update. Stripe subscriptions and outgoing webhooks work as before.',
      canStaff('admin') ? actionButton('Stripe settings', () => billingGo('settings', 'payments'), { cls: '' }) : null,
      canStaff('admin') ? actionButton('Webhooks', () => billingGo('settings', 'webhooks')) : null));
    return;
  }
  const o = await api('GET', '/billing/overview');
  if (o.currency) BILLING.currency = o.currency;
  const admin = canStaff('admin');
  const month = o.income_this_month || 0, last = o.income_last_month || 0;
  let change = null;
  if (last > 0) {
    const pct = Math.round(((month - last) / last) * 100);
    change = h('span', { class: 'trend ' + (pct >= 0 ? 'up' : 'down') }, icon(pct >= 0 ? 'trend-up' : 'trend-down'),
      `${pct >= 0 ? '+' : ''}${pct}%`, h('span', { class: 'muted' }, ` vs ${money(last)} last month`));
  } else {
    change = h('span', {}, last === 0 && month === 0 ? 'Nothing received yet' : 'Nothing last month');
  }
  const setup = BILLING.enabled === false
    ? banner('info', 'info', 'Built-in billing is off', 'Turn it on to send invoices, take card and bank payments and run reminders ' +
      'for the accounts you bill. Your order page opens with it.', admin ? actionButton('Set up billing', () => billingGo('settings'), { cls: '' }) : null)
    : null;

  const overdue = listOf(o.overdue_invoices);
  const recent = listOf(o.recent_payments);
  const upcoming = listOf(o.upcoming_renewals);
  box.replaceChildren(
    setup || '',
    h('div', { class: 'kpis' },
      kpi('Monthly recurring revenue', money(o.mrr), `${fmtNum(o.billed_accounts || 0)} billed account${o.billed_accounts === 1 ? '' : 's'}`, { ic: 'trend-up' }),
      kpi('Income this month', money(month), change, { ic: 'wallet' }),
      kpi('Outstanding', money(o.outstanding), `${fmtNum(o.unpaid_count || 0)} unpaid invoice${o.unpaid_count === 1 ? '' : 's'}`,
        { ic: 'receipt', onClick: () => { INVOICE_FILTER.status = 'unpaid'; billingGo('invoices'); } }),
      kpi('Overdue', money(o.overdue_total), `${fmtNum(o.overdue_count || 0)} invoice${o.overdue_count === 1 ? '' : 's'} past due`,
        { ic: 'alert', tone: o.overdue_count ? 'bad' : '', onClick: () => { INVOICE_FILTER.status = 'overdue'; billingGo('invoices'); } }),
      kpi('Pending orders', fmtNum(o.pending_orders || 0), 'waiting for payment or approval',
        { ic: 'cart', tone: o.pending_orders ? 'warn' : '', onClick: canStaff('operator') ? () => billingGo('orders') : null }),
      kpi('Client credit', money(o.credit_total), 'held on accounts', { ic: 'tag' })),
    h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('h2', {}, 'Income, last 12 months'),
      h('span', { class: 'muted small' }, 'payments received, less refunds')),
    barChart(listOf(o.income_by_month), { title: 'Income by month, last 12 months' })),
    h('div', { class: 'bgrid' },
      h('div', { class: 'card' }, h('h2', {}, 'Overdue invoices'), btable(['Invoice', 'Late', { label: 'Balance', num: true }, ''],
        overdue.map((inv) => ({
          open: () => billingGo('invoices', inv.id),
          cells: [h('td', {}, invoiceLink(inv), h('span', { class: 'sub-line' }, inv.account_name || `#${inv.account_id}`)),
            h('td', { class: 'st-failed' }, dueText(inv) || 'late'), money(inv.balance),
            h('td', { class: 'row-actions' }, admin ? actionButton('Remind', () => remindInvoice(inv), { ic: 'send' }) : null)],
        })), { empty: h('p', { class: 'muted small ok-line' }, icon('check'), 'Nothing overdue. Well done.') })),
      h('div', { class: 'card' }, h('h2', {}, 'Recent payments'), btable(['Client', 'Method', { label: 'Amount', num: true }],
        recent.map((p) => ({
          open: p.invoice_id ? () => billingGo('invoices', p.invoice_id) : null,
          cells: [h('td', {}, p.account_name || (p.account_id ? `#${p.account_id}` : '–'), h('span', { class: 'sub-line' }, fmtDate(p.at))),
            methodName(p.gateway || p.method), money(p.amount)],
        })), { empty: h('p', { class: 'muted small' }, 'Payments show up here as they come in.') })),
      h('div', { class: 'card' }, h('h2', {}, 'Upcoming renewals'), btable(['Client', 'Due', { label: 'Amount', num: true }],
        upcoming.map((r) => [h('td', {}, r.account_name || `#${r.account_id}`, h('span', { class: 'sub-line' },
          [r.plan_name || r.plan_id || '', r.cycle ? cycleOf(r.cycle).label.toLowerCase() : ''].filter(Boolean).join(' · '))),
        fmtDate(r.next_due_at || r.due_at), money(r.amount ?? r.recurring_amount)]),
        { empty: h('p', { class: 'muted small' }, 'No renewals in the coming weeks.') }))),
  );
}

// barChart draws amounts by month as bars from a zero baseline, the latest
// month in full colour; the numbers are in its description and in a table
// under it, and each bar shows its value under the pointer.
function barChart(points, { title }) {
  const W = 640, H = 230, L = 64, R = 8, T = 14, B = 30;
  const pw = W - L - R, ph = H - T - B;
  const most = Math.max(0, ...points.map((p) => p.amount || 0));
  // A "nice" step (1, 2, 2.5 or 5 × 10ⁿ) for four gridlines.
  const raw = Math.max(most, 10 ** currency().decimals) / 4;
  const e = 10 ** Math.floor(Math.log10(raw)), m = raw / e;
  const step = (m <= 1 ? 1 : m <= 2 ? 2 : m <= 2.5 ? 2.5 : m <= 5 ? 5 : 10) * e, max = step * 4;
  const id = uid('chart');
  const svg = svgEl('svg', { viewBox: `0 0 ${W} ${H}`, class: 'bchart', role: 'img', 'aria-labelledby': `${id}-t ${id}-d` });
  const t = svgEl('title', { id: id + '-t' });
  t.textContent = title;
  const d = svgEl('desc', { id: id + '-d' });
  d.textContent = points.length ? points.map((p) => `${monthLabel(p.month, true)}: ${money(p.amount)}`).join('; ') : 'No data yet.';
  svg.append(t, d);
  for (let i = 0; i <= 4; i++) {
    const y = T + ph - (i / 4) * ph;
    svg.append(svgEl('line', { x1: L, x2: W - R, y1: y, y2: y, class: i ? 'gridline' : 'baseline' }));
    const lab = svgEl('text', { x: L - 10, y: y + 4, class: 'ylab', 'text-anchor': 'end' });
    lab.textContent = money(step * i, true);
    svg.append(lab);
  }
  const wrap = h('div', { class: 'bchart-wrap' });
  const tip = h('div', { class: 'bchart-tip', hidden: true });
  const slot = pw / Math.max(1, points.length), bw = Math.min(34, slot * 0.6);
  points.forEach((p, i) => {
    const x = L + slot * i + (slot - bw) / 2, bh = max ? ((p.amount || 0) / max) * ph : 0;
    if (bh > 0) {
      const hh = Math.max(bh, 2), y = T + ph - hh, r = Math.min(4, bw / 2, hh);
      svg.append(svgEl('path', {
        class: 'bar' + (i === points.length - 1 ? ' current' : ''),
        d: `M${x} ${y + hh}V${y + r}Q${x} ${y} ${x + r} ${y}H${x + bw - r}Q${x + bw} ${y} ${x + bw} ${y + r}V${y + hh}Z`,
      }));
    }
    const xl = svgEl('text', { x: x + bw / 2, y: H - 9, class: 'xlab' + (i % 2 ? ' odd' : ''), 'text-anchor': 'middle' });
    xl.textContent = monthLabel(p.month);
    const hit = svgEl('rect', { x: L + slot * i, y: T, width: slot, height: ph, class: 'hit' });
    hit.addEventListener('pointerenter', () => {
      tip.textContent = `${monthLabel(p.month, true)} · ${money(p.amount)}`;
      tip.hidden = false;
      const box = wrap.getBoundingClientRect(), r = hit.getBoundingClientRect();
      // CSSOM, not a style attribute: the panel's CSP allows this.
      tip.style.left = `${Math.max(0, Math.min(box.width - tip.offsetWidth, r.left - box.left + r.width / 2 - tip.offsetWidth / 2))}px`;
      tip.style.top = `${Math.max(0, (T + ph - bh) * (box.height / H) - 34)}px`;
    });
    hit.addEventListener('pointerleave', () => { tip.hidden = true; });
    svg.append(xl, hit);
  });
  wrap.append(svg, tip);
  const tbl = h('details', { class: 'chart-table' }, h('summary', {}, 'Show the numbers'),
    btable(['Month', { label: 'Income', num: true }], points.map((p) => [monthLabel(p.month, true), money(p.amount)])));
  return h('div', {}, wrap, points.length && !most ? h('p', { class: 'muted small' }, 'No income recorded yet: bars appear as payments come in.') : null, tbl);
}

// ---- Invoices ----

const INVOICE_FILTER = { status: '', q: '', from: '', to: '' };
const INVOICE_CHIPS = [['', 'All'], ['unpaid', 'Unpaid'], ['overdue', 'Overdue'], ['paid', 'Paid'], ['draft', 'Drafts'],
  ['cancelled', 'Cancelled'], ['refunded', 'Refunded']];

function invoiceQuery(f, extra = {}) {
  const q = new URLSearchParams();
  if (f.status === 'overdue') q.set('overdue', '1');
  else if (f.status) q.set('status', f.status);
  for (const k of ['q', 'from', 'to']) if (f[k]) q.set(k, f[k]);
  for (const [k, v] of Object.entries(extra)) if (v != null && v !== '') q.set(k, v);
  return q.toString();
}

async function staffInvoices(box, route) {
  if (route.id) return staffInvoice(box, route.id, route.query);
  const f = INVOICE_FILTER, admin = canStaff('admin');
  const results = h('div', { 'aria-live': 'polite' });
  const counts = {};
  const chipBox = h('div');
  const drawChips = () => chipBox.replaceChildren(chips('Show invoices', INVOICE_CHIPS.map(([k, label]) => [k, label, counts[k]]), f.status,
    (k) => { f.status = k; drawChips(); load(true); }));
  const search = h('input', { type: 'search', placeholder: 'Number or client', 'aria-label': 'Search invoices', value: f.q });
  const from = h('input', { type: 'date', 'aria-label': 'Issued from', value: f.from });
  const to = h('input', { type: 'date', 'aria-label': 'Issued until', value: f.to });
  const csv = admin ? h('a', { class: 'button ghost', download: '' }, icon('download'), 'Export CSV') : null;
  const syncCSV = () => { if (csv) csv.href = '/api/v1/invoices.csv?' + new URLSearchParams(Object.entries({ from: f.from, to: f.to }).filter(([, v]) => v)); };
  search.addEventListener('input', debounce(() => { f.q = search.value.trim(); load(true); }, 300));
  for (const [el, k] of [[from, 'from'], [to, 'to']]) el.addEventListener('change', () => { f[k] = el.value; syncCSV(); load(true); });
  syncCSV();

  let items = [], more = false;
  async function load(reset) {
    if (reset) items = [];
    results.setAttribute('aria-busy', 'true');
    try {
      const res = await api('GET', '/invoices?' + invoiceQuery(f, { limit: 50, before: reset ? '' : (items.length ? items[items.length - 1].id : '') }));
      const list = listOf(res, 'invoices');
      if (res && res.counts) { Object.assign(counts, res.counts); drawChips(); }
      items = items.concat(list);
      more = list.length === 50;
      draw();
    } catch (e) {
      results.replaceChildren(loadError(e, () => load(true)));
    } finally { results.removeAttribute('aria-busy'); }
  }
  function draw() {
    const filtered = f.status || f.q || f.from || f.to;
    const empty = filtered
      ? emptyState('search', 'No invoices match', 'Try another filter, or clear the search and dates.')
      : emptyState('receipt', 'No invoices yet', 'Invoices are created automatically a few days before each renewal of the clients you bill, ' +
        'and when someone orders from your order page. You can also write one by hand.',
      admin ? actionButton('New invoice', () => openInvoiceEditor({}), { cls: '', ic: 'plus' }) : null);
    const loadMore = more ? h('div', { class: 'actions center-actions' }, actionButton('Load more', () => load(false))) : null;
    results.replaceChildren(btable(['Invoice', 'Client', 'Issued', 'Due', { label: 'Total', num: true }, { label: 'Balance', num: true }, 'Status'],
      items.map((inv) => ({
        cls: invoiceState(inv) === 'overdue' ? 'row-overdue' : '',
        open: () => billingGo('invoices', inv.id),
        cells: [invoiceLink(inv), inv.account_name || `#${inv.account_id}`, fmtDate(inv.issued_at),
          h('td', {}, fmtDate(inv.due_at), dueText(inv) ? h('span', { class: 'sub-line' }, dueText(inv)) : null),
          money(inv.total), money(inv.balance), h('td', {}, invoicePill(inv))],
      })), { empty, caption: 'Invoices' }), loadMore || '');
  }

  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'toolbar' }, chipBox, h('div', { class: 'toolbar-fields' },
      h('label', { class: 'filter' }, icon('search'), search), from, h('span', { class: 'muted small' }, 'to'), to, csv)),
    results));
  drawChips();
  // Counts for the chips the list doesn't give: from the overview.
  api('GET', '/billing/overview').then((o) => {
    if (counts.unpaid == null && o.unpaid_count != null) counts.unpaid = o.unpaid_count;
    if (counts.overdue == null && o.overdue_count != null) counts.overdue = o.overdue_count;
    drawChips();
  }).catch(() => {});
  await load(true);
}

// invoiceDocument shows an invoice like the paper one: who from, who to,
// the lines, the taxes and totals, and a stamp saying where it stands.
function invoiceDocument(inv) {
  const st = invoiceState(inv);
  const stampText = { paid: 'Paid', overdue: 'Overdue', cancelled: 'Cancelled', draft: 'Draft', refunded: 'Refunded', partially_refunded: 'Part refunded' }[st];
  const addr = inv.billing_address || {};
  const company = (BILLING && BILLING.company) || {};
  const totals = [['Subtotal', money(inv.subtotal)]];
  if (inv.discount) totals.push([discountLabel(inv.items), '−' + money(inv.discount)]);
  for (const t of inv.tax_lines || []) totals.push([`${t.name} ${fmtRate(t.rate)}`, money(t.amount)]);
  totals.push(['Total', money(inv.total), 'total']);
  if (inv.credit_applied) totals.push(['Credit applied', '−' + money(inv.credit_applied)]);
  if (inv.amount_paid) totals.push(['Paid', '−' + money(inv.amount_paid)]);
  if (inv.amount_refunded) totals.push(['Refunded', money(inv.amount_refunded)]);
  if (inv.status !== 'cancelled' && inv.status !== 'draft') totals.push(['Balance due', money(inv.balance), 'grand']);
  const meta = [['Issued', fmtDate(inv.issued_at)], ['Due', fmtDate(inv.due_at)]];
  // period_end is the next period's start: show the last day covered, as
  // the lines, e-mails and the printed invoice do.
  if (inv.period_start && inv.period_end) {
    const last = Math.max(new Date(inv.period_start).getTime(), new Date(inv.period_end).getTime() - 864e5);
    meta.push(['Period', `${fmtDate(inv.period_start)} – ${fmtDate(last)}`]);
  }
  if (inv.paid_at) meta.push(['Paid', fmtDate(inv.paid_at)]);
  return h('article', { class: 'inv-doc', 'aria-label': `Invoice ${invoiceTitle(inv)}` },
    h('header', { class: 'inv-head' },
      h('div', {}, h('div', { class: 'inv-company' }, companyName() || 'Your provider'),
        company.address ? h('div', { class: 'inv-lines muted' }, company.address) : null,
        company.tax_id ? h('div', { class: 'muted small' }, `Tax ID ${company.tax_id}`) : null),
      h('div', { class: 'inv-heading' }, h('div', { class: 'inv-word' }, inv.number ? 'Invoice' : 'Proforma invoice'),
        h('div', { class: 'inv-no' }, invoiceTitle(inv)))),
    h('div', { class: 'inv-parties' },
      h('div', {}, h('h3', {}, 'Bill to'), h('div', { class: 'inv-lines' }, [addr.name || inv.account_name, addr.company, ...(addr.lines || []),
        addr.country ? countryName(addr.country) : ''].filter(Boolean).join('\n')),
      addr.tax_id ? h('div', { class: 'muted small' }, `Tax ID ${addr.tax_id}`) : null),
      h('dl', { class: 'inv-meta' }, meta.map(([k, v]) => [h('dt', {}, k), h('dd', {}, v)]).flat())),
    h('div', { class: 'table-wrap' }, h('table', { class: 'inv-items' },
      h('thead', {}, h('tr', {}, h('th', { scope: 'col' }, 'Description'), h('th', { scope: 'col', class: 'num' }, 'Qty'),
        h('th', { scope: 'col', class: 'num' }, 'Unit price'), h('th', { scope: 'col', class: 'num' }, 'Amount'))),
      h('tbody', {}, (inv.items || []).filter(notDiscount).map((it) => h('tr', { class: 'item-' + (it.kind || 'custom') },
        h('td', {}, it.description, it.taxable === false && (inv.tax_lines || []).length ? h('span', { class: 'sub-line' }, 'not taxed') : null),
        h('td', { class: 'num' }, String(it.quantity ?? 1)), h('td', { class: 'num' }, money(it.unit_price)), h('td', { class: 'num' }, money(it.amount))))))),
    // The stamp sits in the space beside the totals.
    h('div', { class: 'inv-foot' }, stampText ? h('div', { class: 'inv-stamp s-' + st }, stampText) : h('span'),
      h('dl', { class: 'inv-totals' }, totals.map(([k, v, cls]) => h('div', { class: cls || null }, h('dt', {}, k), h('dd', {}, v))))),
    BILLING && BILLING.tax_inclusive && (inv.tax_lines || []).length ? h('p', { class: 'muted small' }, 'Prices include tax.') : null,
    inv.notes ? h('div', { class: 'inv-notes' }, h('h3', {}, 'Notes'), h('p', {}, inv.notes)) : null);
}

function paymentsTable(inv) {
  return btable(['Date', 'Method', 'Reference', { label: 'Amount', num: true }], (inv.payments || []).map((p) => [
    fmtDate(p.at), methodName(p.gateway), h('td', { class: 'wrap' }, p.reference || '–'),
    h('td', { class: 'num' }, money(p.amount), p.refunded ? h('span', { class: 'sub-line' }, `${money(p.refunded)} refunded`) : null)]),
  { empty: h('p', { class: 'muted small' }, 'No payments yet.') });
}

async function staffInvoice(box, id) {
  const inv = await api('GET', `/invoices/${encodeURIComponent(id)}`);
  const admin = canStaff('admin');
  // E-mails about it: the account's invoice messages naming its number.
  const mails = (await api('GET', `/accounts/${inv.account_id}/emails?limit=100`).catch(() => []))
    .filter((m) => (m.template || '').startsWith('invoice.') && (!inv.number || (m.subject || '').includes(inv.number)));
  const reload = () => staffInvoice(box, id).catch((e) => box.replaceChildren(loadError(e, reload)));
  const st = inv.status;
  const acts = [h('a', { class: 'button ghost', href: `/api/v1/invoices/${inv.id}/print`, target: '_blank', rel: 'noopener' }, icon('printer'), 'Print / PDF')];
  if (admin) {
    if (st === 'draft') {
      acts.push(actionButton('Edit', async () => { if (await openInvoiceEditor({ invoice: inv })) reload(); }, { ic: 'pencil' }));
      acts.push(actionButton('Issue', async () => {
        if (!await ask(`Issue ${invoiceTitle(inv)}? It gets its number and is e-mailed to the client with a link to pay.`, { ok: 'Issue invoice' })) return;
        await api('POST', `/invoices/${inv.id}/issue`);
        notify('Invoice issued');
        reload();
      }, { cls: '', ic: 'send' }));
    }
    if (st === 'unpaid') {
      acts.push(actionButton('Record payment', async () => { if (await recordPayment(inv)) reload(); }, { cls: '', ic: 'banknote' }));
      acts.push(actionButton('Send reminder', () => remindInvoice(inv), { ic: 'send' }));
      acts.push(actionButton('Apply credit', async () => { if (await applyCredit(inv)) reload(); }, { ic: 'tag' }));
      acts.push(actionButton('Edit', async () => { if (await editUnpaid(inv)) reload(); }, { ic: 'pencil' }));
    }
    if ((inv.payments || []).some((p) => p.amount > (p.refunded || 0))) {
      acts.push(actionButton('Refund', async () => { if (await refundInvoice(inv)) reload(); }, { ic: 'refresh' }));
    }
    if (st === 'draft' || st === 'unpaid') {
      acts.push(actionButton('Cancel invoice', async () => {
        // A cancelled renewal waives its period (the server moves the next
        // due date past it), so billing carries on with the next one.
        const what = inv.kind === 'renewal'
          ? 'Cancelling a renewal invoice waives that period: the client isn\'t billed for it, and their next due date moves to the end of the period. Billing continues with the next period.'
          : 'The client no longer needs to pay it.';
        if (!await ask(`Cancel ${invoiceTitle(inv)}? ${what} Payments already made stay recorded.`,
          { ok: 'Cancel invoice', danger: true })) return;
        await api('POST', `/invoices/${inv.id}/cancel`);
        notify('Invoice cancelled');
        reload();
      }, { cls: 'ghost danger', ic: 'ban' }));
    }
  }
  const client = canStaff('viewer') ? linkButton(inv.account_name || `Account #${inv.account_id}`, () => openAccount(inv.account_id)) : inv.account_name;
  box.replaceChildren(
    backLink('All invoices', 'invoices'),
    h('div', { class: 'inv-top' },
      h('div', {}, h('h2', { class: 'inv-title' }, invoiceTitle(inv), invoicePill(inv)),
        h('p', { class: 'muted small' }, client, ` · ${INVOICE_KINDS[inv.kind] || 'Invoice'}`, dueText(inv) ? ` · ${dueLine(inv)}` : '')),
      h('div', { class: 'actions inv-actions' }, acts)),
    h('div', { class: 'inv-layout' }, invoiceDocument(inv),
      h('aside', { class: 'inv-side' },
        h('div', { class: 'card' }, h('h2', {}, 'Payments'), paymentsTable(inv)),
        h('div', { class: 'card' }, h('h2', {}, 'E-mails about it'), btable(['Sent', 'Subject', ''], mails.map((m) => [
          fmtTime(m.sent_at || m.created_at), m.subject,
          h('td', {}, linkButton('View', () => openFrame(m.subject, `/api/v1/accounts/${inv.account_id}/emails/${m.id}/html`)))]),
        { empty: h('p', { class: 'muted small' }, 'None yet.') })))));
}

async function remindInvoice(inv) {
  if (!await ask(`Send a reminder for ${invoiceTitle(inv)} now? The client gets the reminder e-mail with a link to pay ${money(inv.balance)}.`,
    { ok: 'Send reminder' })) return;
  await api('POST', `/invoices/${inv.id}/remind`);
  notify(`Reminder sent for ${invoiceTitle(inv)}`);
}

// liveHint is a line under a form that follows what's typed.
const liveHint = () => h('p', { class: 'live-hint', 'aria-live': 'polite' });

function recordPayment(inv) {
  const hint = liveHint();
  const amount = moneyInput('amount', inv.balance, { required: true });
  const sync = () => {
    const a = toMinor(amount.querySelector('input').value);
    hint.textContent = a == null || Number.isNaN(a) || a <= 0 ? 'Enter the amount you received.'
      : a === inv.balance ? 'This pays the invoice in full.'
        : a < inv.balance ? `${money(inv.balance - a)} will still be due.` : `${money(a - inv.balance)} more than due goes to the client's credit.`;
  };
  amount.addEventListener('input', sync);
  sync();
  return openDialog({
    title: `Record a payment for ${invoiceTitle(inv)}`, ok: 'Record payment',
    intro: 'For money received outside the panel: a bank transfer, cash or a cheque. Card and Razorpay payments are recorded by themselves.',
    body: [h('div', { class: 'grid' },
      field('Amount', amount),
      field('Method', h('select', { name: 'method' }, options([['bank', 'Bank transfer'], ['cash', 'Cash'], ['cheque', 'Cheque'], ['other', 'Other']], 'bank'))),
      field('Received on', h('input', { type: 'date', name: 'at', value: todayInput(), required: true })),
      field('Reference', h('input', { name: 'reference', placeholder: 'e.g. the bank\'s transaction ID' }))),
    field('Note', h('input', { name: 'note', placeholder: 'Only staff see it' })), hint],
    async submit(f) {
      const a = toMinor(f.elements.amount.value);
      if (a == null || Number.isNaN(a) || a <= 0) throw fieldError('amount', 'Enter an amount above zero.');
      await api('POST', `/invoices/${inv.id}/payments`, { amount: a, method: f.elements.method.value, reference: f.elements.reference.value.trim(),
        at: fromDateInput(f.elements.at.value), note: f.elements.note.value.trim() });
      notify(`Payment of ${money(a)} recorded`);
    },
  });
}

function refundInvoice(inv) {
  const pays = (inv.payments || []).filter((p) => p.amount > (p.refunded || 0));
  const left = (p) => p.amount - (p.refunded || 0);
  const pick = h('select', { name: 'payment_id' }, pays.map((p) => h('option', { value: p.id },
    `${fmtDate(p.at)} · ${methodName(p.gateway)} · ${money(left(p))} refundable`)));
  const amount = moneyInput('amount', left(pays[0]), { required: true });
  const where = h('div', { class: 'choices' });
  const hint = liveHint();
  const sync = () => {
    const p = pays.find((x) => String(x.id) === pick.value) || pays[0];
    const online = ['stripe', 'razorpay'].includes(p.gateway);
    const was = where.querySelector('input:checked');
    where.replaceChildren(
      online ? choice('to', 'gateway', `Back to the ${methodName(p.gateway).toLowerCase()}`, 'The money goes back the way it came; it takes a few days to arrive.', !was || was.value === 'gateway') : null,
      choice('to', 'credit', 'As account credit', 'Kept on the account and used for its next invoices.', !online || (was && was.value === 'credit')));
    const a = toMinor(amount.querySelector('input').value);
    hint.textContent = a > left(p) ? `At most ${money(left(p))} of this payment can be refunded.` : '';
  };
  pick.addEventListener('change', () => { amount.querySelector('input').value = fromMinor(left(pays.find((x) => String(x.id) === pick.value))); sync(); });
  amount.addEventListener('input', sync);
  sync();
  return openDialog({
    title: `Refund ${invoiceTitle(inv)}`, ok: 'Refund', danger: true,
    body: [field('Payment', pick), field('Amount', amount), h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Refund to'), where), hint],
    async submit(f) {
      const a = toMinor(f.elements.amount.value);
      if (a == null || Number.isNaN(a) || a <= 0) throw fieldError('amount', 'Enter an amount above zero.');
      await api('POST', `/invoices/${inv.id}/refund`, { payment_id: Number(pick.value), amount: a, to: f.elements.to.value });
      notify(`${money(a)} refunded`);
    },
  });
}

function applyCredit(inv) {
  return openDialog({
    title: `Apply credit to ${invoiceTitle(inv)}`, ok: 'Apply credit',
    intro: 'Pays the invoice from the credit held on the account (refunds, goodwill, over-payments).',
    body: [field('Amount', moneyInput('amount', null, { placeholder: 'as much as is available' }), `Leave empty to use up to ${money(inv.balance)}.`)],
    async submit(f) {
      const a = toMinor(f.elements.amount.value);
      if (Number.isNaN(a) || (a != null && a <= 0)) throw fieldError('amount', 'Enter an amount above zero, or leave it empty.');
      await api('POST', `/invoices/${inv.id}/apply-credit`, { amount: a });
      notify('Credit applied');
    },
  });
}

function editUnpaid(inv) {
  return openDialog({
    title: `Edit ${invoiceTitle(inv)}`, intro: 'An issued invoice keeps its lines; its due date and notes can change.',
    body: [field('Due date', h('input', { type: 'date', name: 'due_at', value: dateInput(inv.due_at), required: true })),
      field('Notes', h('textarea', { name: 'notes', rows: 3 }, inv.notes || ''), 'Printed on the invoice.')],
    async submit(f) {
      await api('PUT', `/invoices/${inv.id}`, { due_at: fromDateInput(f.elements.due_at.value), notes: f.elements.notes.value });
      notify('Invoice updated');
    },
  });
}

// openInvoiceEditor writes a new invoice (or a draft's lines) with a total
// that follows the lines; tax is added from the tax rules when it's saved.
async function openInvoiceEditor({ accountId, invoice }) {
  await billingConfig();
  let pick = null;
  if (!invoice) {
    const accounts = (await api('GET', '/accounts')).filter((a) => !a.parent_id && a.status !== 'terminated')
      .sort((a, b) => a.name.localeCompare(b.name));
    if (!accounts.length) { showError(new Error('There are no accounts to invoice yet: create one under Accounts.')); return null; }
    pick = h('select', { name: 'account_id', required: true }, h('option', { value: '' }, 'Choose a client…'),
      accounts.map((a) => h('option', { value: a.id, selected: String(a.id) === String(accountId || '') }, `${a.name} (#${a.id})`)));
  }
  const lines = h('div', { class: 'lines', role: 'list' });
  const total = h('div', { class: 'live-total', 'aria-live': 'polite' });
  let n = 0;
  const recalc = () => {
    let sum = 0;
    for (const row of lines.children) {
      const q = Number(row.querySelector('[data-k=quantity]').value || 0), p = toMinor(row.querySelector('[data-k=unit_price]').value);
      const amt = Number.isNaN(p) || p == null ? null : Math.round(q * p);
      row.querySelector('.line-amount').textContent = amt == null ? '–' : money(amt);
      sum += amt || 0;
    }
    total.replaceChildren(h('span', {}, 'Subtotal'), h('strong', {}, money(sum)),
      h('span', { class: 'muted small' }, BILLING && BILLING.tax_inclusive ? 'Prices include tax.' : 'Tax is added from your tax rules when it\'s saved.'));
  };
  const addLine = (it = {}) => {
    const i = ++n;
    const del = h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Remove line ${i}` }, icon('trash'));
    const row = h('div', { class: 'line', role: 'listitem' },
      h('input', { 'data-k': 'description', 'aria-label': `Line ${i}: description`, placeholder: 'Description', value: it.description || '', maxlength: 200 }),
      h('input', { 'data-k': 'quantity', type: 'number', min: 1, step: 1, 'aria-label': `Line ${i}: quantity`, value: it.quantity || 1 }),
      h('span', { class: 'affix' }, h('span', { class: 'affix-text', 'aria-hidden': 'true' }, currencySymbol()),
        h('input', { 'data-k': 'unit_price', inputmode: 'decimal', 'aria-label': `Line ${i}: unit price`, placeholder: fromMinor(0), value: fromMinor(it.unit_price) })),
      h('span', { class: 'line-amount num', 'aria-label': `Line ${i}: amount` }, '–'),
      h('label', { class: 'line-tax', title: 'Tax applies to this line' }, h('input', { type: 'checkbox', 'data-k': 'taxable', checked: it.taxable !== false }), 'Taxed'),
      del);
    del.addEventListener('click', () => { if (lines.children.length > 1) { row.remove(); recalc(); } });
    row.addEventListener('input', recalc);
    lines.append(row);
    recalc();
    return row;
  };
  (invoice ? (invoice.items || []).filter((it) => !['discount', 'credit'].includes(it.kind)) : [{}]).forEach(addLine);
  if (!lines.children.length) addLine();
  const add = h('button', { type: 'button', class: 'ghost' }, icon('plus'), 'Add a line');
  add.addEventListener('click', () => addLine().querySelector('input').focus());
  const mode = invoice ? null : h('div', { class: 'choices' },
    choice('mode', 'issue', 'Issue it now', 'It gets its number and the client can pay it.', true),
    choice('mode', 'draft', 'Save as a draft', 'Nobody sees it until you issue it.'));
  const email = invoice ? null : toggle('send_email', 'E-mail it to the client', true, 'With a link to pay online.');
  if (mode) mode.addEventListener('change', () => {
    const draft = mode.querySelector('input:checked').value === 'draft';
    email.querySelector('input').disabled = draft;
  });
  return openDialog({
    title: invoice ? `Edit ${invoiceTitle(invoice)}` : 'New invoice', wide: true, ok: invoice ? 'Save draft' : 'Create invoice',
    body: [
      pick ? field('Client', pick) : h('p', { class: 'small' }, 'For ', h('strong', {}, invoice.account_name || `account #${invoice.account_id}`)),
      h('div', { class: 'lines-head', 'aria-hidden': 'true' }, h('span', {}, 'Description'), h('span', {}, 'Qty'), h('span', {}, 'Unit price'),
        h('span', { class: 'num' }, 'Amount'), h('span', {}, '')),
      lines, h('div', { class: 'lines-foot' }, add, total),
      h('div', { class: 'grid' },
        field('Due date', h('input', { type: 'date', name: 'due_at', required: true, value: invoice ? dateInput(invoice.due_at) : todayInput(7) })),
        field('Notes', h('textarea', { name: 'notes', rows: 2, placeholder: 'Printed on the invoice' }, invoice ? invoice.notes || '' : ''))),
      mode, email],
    async submit(f) {
      const items = [...lines.children].map((row) => ({
        description: row.querySelector('[data-k=description]').value.trim(),
        quantity: Math.max(1, Math.trunc(Number(row.querySelector('[data-k=quantity]').value || 1))),
        unit_price: toMinor(row.querySelector('[data-k=unit_price]').value),
        taxable: row.querySelector('[data-k=taxable]').checked,
      })).filter((it) => it.description || it.unit_price != null);
      if (!items.length) throw new Error('Add at least one line.');
      const bad = items.findIndex((it) => !it.description || it.unit_price == null || Number.isNaN(it.unit_price));
      if (bad >= 0) {
        lines.children[bad].querySelector(items[bad].description ? '[data-k=unit_price]' : '[data-k=description]').focus();
        throw new Error(`Line ${bad + 1} needs a description and a price.`);
      }
      const body = { items, due_at: fromDateInput(f.elements.due_at.value), notes: f.elements.notes.value.trim() };
      if (invoice) {
        await api('PUT', `/invoices/${invoice.id}`, body);
        notify('Draft saved');
        return true;
      }
      const draft = f.elements.mode.value === 'draft';
      const created = await api('POST', '/invoices', { account_id: Number(f.elements.account_id.value), ...body, draft,
        send_email: !draft && f.elements.send_email.checked });
      const inv = created.invoice || created;
      notify(draft ? 'Draft saved' : `Invoice ${invoiceTitle(inv)} created`);
      if (inv && inv.id) billingGo('invoices', inv.id);
      return inv;
    },
  });
}

// ---- Transactions ----

const TX_FILTER = { method: '', from: '', to: '' };

async function staffTransactions(box) {
  const f = TX_FILTER, admin = canStaff('admin');
  const method = h('select', { 'aria-label': 'Method' }, options([['', 'Every method'], ['stripe', 'Card (Stripe)'], ['razorpay', 'Razorpay'],
    ['bank', 'Bank transfer'], ['cash', 'Cash'], ['cheque', 'Cheque'], ['other', 'Other'], ['credit', 'Account credit']], f.method));
  const from = h('input', { type: 'date', 'aria-label': 'From', value: f.from });
  const to = h('input', { type: 'date', 'aria-label': 'Until', value: f.to });
  const csv = admin ? h('a', { class: 'button ghost', download: '' }, icon('download'), 'Export CSV') : null;
  const results = h('div', { 'aria-live': 'polite' });
  const syncCSV = () => { if (csv) csv.href = '/api/v1/transactions.csv?' + new URLSearchParams(Object.entries({ from: f.from, to: f.to }).filter(([, v]) => v)); };
  let items = [], more = false;
  const load = async (reset) => {
    if (reset) items = [];
    results.setAttribute('aria-busy', 'true');
    try {
      const q = new URLSearchParams(Object.entries({ ...f, limit: 50, before: reset || !items.length ? '' : items[items.length - 1].id }).filter(([, v]) => v !== ''));
      const list = listOf(await api('GET', '/transactions?' + q), 'transactions');
      items = items.concat(list);
      more = list.length === 50;
      results.replaceChildren(btable(['Date', 'Client', 'Invoice', 'Method', 'Reference', { label: 'Amount', num: true }, { label: 'Fee', num: true }],
        items.map((t) => ({
          open: t.invoice_id ? () => billingGo('invoices', t.invoice_id) : null,
          cells: [fmtDate(t.at), t.account_name || (t.account_id ? `#${t.account_id}` : '–'),
            t.invoice_id ? invoiceLink({ id: t.invoice_id, number: t.invoice_number }) : '–', methodName(t.gateway || t.method),
            h('td', { class: 'wrap' }, t.reference || '–'),
            h('td', { class: 'num' }, money(t.amount), t.refunded ? h('span', { class: 'sub-line' }, `${money(t.refunded)} refunded`) : null),
            t.fee ? money(t.fee) : '–'],
        })), {
        empty: emptyState('wallet', f.method || f.from || f.to ? 'No payments match' : 'No payments yet',
          'Every payment lands here: cards and Razorpay by themselves, bank transfers and cash when you record them on the invoice.'),
        caption: 'Transactions',
      }), more ? h('div', { class: 'actions center-actions' }, actionButton('Load more', () => load(false))) : '');
    } catch (e) {
      results.replaceChildren(loadError(e, () => load(true)));
    } finally { results.removeAttribute('aria-busy'); }
  };
  method.addEventListener('change', () => { f.method = method.value; load(true); });
  for (const [el, k] of [[from, 'from'], [to, 'to']]) el.addEventListener('change', () => { f[k] = el.value; syncCSV(); load(true); });
  syncCSV();
  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'toolbar' }, h('div', { class: 'toolbar-fields' }, method, from, h('span', { class: 'muted small' }, 'to'), to, csv)), results));
  await load(true);
}

// ---- Orders ----

let ORDER_STATUS = 'pending';

async function staffOrders(box) {
  const admin = canStaff('admin');
  const list = listOf(await api('GET', `/orders?status=${ORDER_STATUS}`), 'orders');
  const accept = (o) => actionButton('Accept', async () => {
    const unpaid = o.invoice_status && o.invoice_status !== 'paid';
    if (!await ask(`Activate ${o.account_name || 'this account'} now? ` + (unpaid ? 'Its invoice isn\'t paid yet; it stays open for the client to pay.' : 'The client can create sites straight away.'),
      { ok: 'Activate account' })) return;
    await api('POST', `/orders/${o.id}/accept`);
    notify('Order accepted');
    staffOrders(box).catch((e) => box.replaceChildren(loadError(e)));
  }, { cls: '', ic: 'check' });
  const cancel = (o) => actionButton('Cancel', async () => {
    if (!await ask(`Cancel the order from ${o.account_name || 'this client'}? Its invoice is cancelled and the pending account is closed.`,
      { ok: 'Cancel order', danger: true })) return;
    await api('POST', `/orders/${o.id}/cancel`);
    notify('Order cancelled');
    staffOrders(box).catch((e) => box.replaceChildren(loadError(e)));
  }, { cls: 'ghost danger' });
  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'toolbar' }, chips('Show orders', [['pending', 'Waiting'], ['active', 'Activated'], ['cancelled', 'Cancelled']], ORDER_STATUS,
      (k) => { ORDER_STATUS = k; staffOrders(box).catch((e) => box.replaceChildren(loadError(e))); }),
    h('a', { class: 'button ghost', href: '/order.html', target: '_blank', rel: 'noopener' }, icon('external'), 'Your order page')),
    btable(['Placed', 'Client', 'Plan', { label: 'Total', num: true }, 'Invoice', 'From', ''], list.map((o) => [
      fmtTime(o.created_at),
      h('td', {}, linkButton(o.account_name || `#${o.account_id}`, () => openAccount(o.account_id)), o.email ? h('span', { class: 'sub-line' }, o.email) : null),
      `${o.plan_name || o.plan_id} · ${cycleOf(o.cycle).label.toLowerCase()}`, money(o.total),
      h('td', {}, o.invoice_id ? invoiceLink({ id: o.invoice_id, number: o.invoice_number }) : '–', ' ',
        o.invoice_status ? bpill(o.invoice_status, INVOICE_STATES[o.invoice_status]) : null),
      h('td', { class: 'nowrap small' }, o.ip || '–'),
      h('td', { class: 'row-actions' }, admin && ORDER_STATUS === 'pending' ? [accept(o), cancel(o)] : null)]), {
      caption: 'Orders',
      empty: emptyState('cart', ORDER_STATUS === 'pending' ? 'No orders waiting' : 'Nothing here',
        ORDER_STATUS === 'pending' ? 'New orders from your order page wait here until they\'re paid' +
          (BILLING.orders_need_approval ? ' and you approve them.' : '. Paid orders activate by themselves.') : 'Orders move here once they\'re activated or cancelled.'),
    })));
}

// ---- An account's Billing panel (Accounts tab) ----

const BILLING_MODES = {
  none: ['Not billed here', 'No invoices from WPGenie (free, or billed some other way).'],
  invoice: ['Invoices', 'WPGenie invoices it every period and collects payment.'],
  stripe_subscription: ['Stripe subscription', 'Billed by a subscription in Stripe (the older setup).'],
  whmcs: ['WHMCS', 'Billed by your WHMCS; WPGenie follows what it says.'],
};

// accountBilling is the Billing panel of an account's detail, filled in
// once its data is in. A reseller's customers are billed by the reseller.
function accountBilling(a) {
  const box = h('section', { class: 'acct-billing', 'aria-label': 'Billing' }, h('h2', {}, 'Billing'), h('p', { class: 'muted small' }, 'Loading…'));
  if (a.parent_id) {
    box.replaceChildren(h('h2', {}, 'Billing'), h('p', { class: 'muted small' }, `Billed by its reseller${a.parent_name ? ' (' + a.parent_name + ')' : ''}, not by you.`));
    return box;
  }
  fillAccountBilling(box, a).catch((e) => box.replaceChildren(h('h2', {}, 'Billing'),
    h('p', { class: 'muted small' }, e.status === 404 ? 'Billing details aren\'t available on this server yet.' : String(e.message))));
  return box;
}

async function fillAccountBilling(box, a) {
  await billingConfig();
  const [p, invoices, credit] = await Promise.all([api('GET', `/accounts/${a.id}/billing`),
    api('GET', `/invoices?account=${a.id}&limit=10`).then((r) => listOf(r, 'invoices')).catch(() => []),
    api('GET', `/accounts/${a.id}/credit`).catch(() => [])]);
  const admin = canStaff('admin');
  const reload = () => fillAccountBilling(box, a).catch(showError);
  const card = p.card ? `${p.card.brand ? p.card.brand[0].toUpperCase() + p.card.brand.slice(1) : 'Card'} •••• ${p.card.last4}, expires ${p.card.exp_month}/${String(p.card.exp_year).slice(-2)}` : 'none saved';
  const rows = [
    ['Billed by', (BILLING_MODES[p.mode] || [p.mode])[0]],
    ['Price', p.recurring_amount != null ? `${money(p.recurring_amount)} / ${cycleOf(p.cycle).per}` + (p.price_override != null ? ' (custom price)' : '') : '–'],
    ['Next due', p.next_due_at ? fmtDate(p.next_due_at) : '–'],
    ['Balance due', h('span', { class: p.overdue ? 'st-failed' : '' }, money(p.balance_due || 0), p.overdue ? ' · overdue' : '')],
    ['Credit', money(p.credit || 0)],
    ['Card', card + (p.auto_pay ? ' · pays automatically' : '')],
    ['Tax', p.tax_exempt ? 'exempt' : 'charged by the tax rules'],
  ];
  if (p.cancel_at) rows.push(['Cancellation', h('span', { class: 'st-warning' }, `ends ${fmtDate(p.cancel_at)}${p.cancel_reason ? ' — ' + p.cancel_reason : ''}`)]);
  const acts = [];
  if (admin) {
    acts.push(actionButton('Edit billing', async () => { if (await editBillingProfile(a, p)) reload(); }, { ic: 'pencil' }));
    acts.push(actionButton('New invoice', () => openInvoiceEditor({ accountId: a.id }), { ic: 'plus' }));
    acts.push(actionButton('Add credit', async () => { if (await addCredit(a)) reload(); }, { ic: 'tag' }));
    acts.push(actionButton('Change plan', async () => {
      if (await openPlanChange({ accountId: a.id, planId: a.plan_id, cycle: p.cycle })) { await loadAccounts(); await showAccountDetail(a.id); }
    }, { ic: 'layers' }));
    acts.push(actionButton('Billing contact', async () => { if (await editContact(a.id, p.contact)) reload(); }, { ic: 'users' }));
    acts.push(p.cancel_at
      ? actionButton('Withdraw cancellation', async () => { await api('DELETE', `/accounts/${a.id}/cancel`); notify('Cancellation withdrawn'); reload(); })
      : actionButton('Cancel service…', async () => { if (await openCancel({ accountId: a.id, name: a.name, admin: true, nextDue: p.next_due_at })) reload(); }, { cls: 'ghost danger' }));
  }
  box.replaceChildren(h('div', { class: 'site-head' }, h('h2', {}, 'Billing'), p.overdue ? bpill('overdue', 'Overdue') : null),
    h('dl', { class: 'kv' }, rows.map(([k, v]) => [h('dt', {}, k), h('dd', {}, v)]).flat()),
    acts.length ? h('div', { class: 'actions acct-billing-actions' }, acts) : null,
    h('h3', {}, 'Invoices'),
    btable(['Invoice', 'Issued', { label: 'Total', num: true }, 'Status'], invoices.map((inv) => ({
      open: () => billingGo('invoices', inv.id),
      cells: [invoiceLink(inv), fmtDate(inv.issued_at), money(inv.total), h('td', {}, invoicePill(inv))],
    })), { empty: h('p', { class: 'muted small' }, 'No invoices yet.') }),
    listOf(credit).length ? h('details', { class: 'advanced' }, h('summary', {}, 'Credit history'),
      btable(['Date', 'Description', { label: 'Amount', num: true }, { label: 'Balance', num: true }], listOf(credit).map((c) => [
        fmtDate(c.at), c.description + (c.by ? ` (${c.by})` : ''), money(c.amount), money(c.balance)]))) : null);
}

function editBillingProfile(a, p) {
  const modes = h('div', { class: 'choices' }, Object.entries(BILLING_MODES).map(([k, [t, text]]) => choice('mode', k, t, text, k === (p.mode || 'none'))));
  return openDialog({
    title: `Billing for ${a.name}`, wide: true,
    body: [h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'How it\'s billed'), modes),
      h('div', { class: 'grid' },
        field('Billing period', h('select', { name: 'cycle' }, options(CYCLES.map((c) => [c.id, c.label]), p.cycle || 'monthly'))),
        field('Custom price', moneyInput('price_override', p.price_override, { placeholder: 'the plan\'s price' }), 'Per period; empty uses the plan\'s price.'),
        field('Next due date', h('input', { type: 'date', name: 'next_due_at', value: dateInput(p.next_due_at) }), 'Later renewals keep this day of the month.')),
      toggle('tax_exempt', 'Tax exempt', p.tax_exempt, 'No tax on its invoices (e.g. a registered business abroad).'),
      toggle('auto_pay', 'Charge the saved card automatically', p.auto_pay, p.card ? 'On the due date.' : 'Needs a saved card: the client saves one when paying by card.')],
    async submit(f) {
      const price = toMinor(f.elements.price_override.value);
      if (Number.isNaN(price)) throw fieldError('price_override', 'Enter an amount, or leave it empty.');
      await api('PUT', `/accounts/${a.id}/billing`, { mode: f.elements.mode.value, cycle: f.elements.cycle.value, price_override: price,
        next_due_at: fromDateInput(f.elements.next_due_at.value), tax_exempt: f.elements.tax_exempt.checked, auto_pay: f.elements.auto_pay.checked });
      notify('Billing saved');
    },
  });
}

function addCredit(a) {
  return openDialog({
    title: `Add credit to ${a.name}`, ok: 'Add credit',
    intro: 'Credit pays the account\'s next invoices. A negative amount takes credit away.',
    body: [field('Amount', moneyInput('amount', null, { required: true })),
      field('Description', h('input', { name: 'description', required: true, placeholder: 'e.g. Goodwill for the outage on 3 May', maxlength: 200 }), 'The client sees it.')],
    async submit(f) {
      const amt = toMinor(f.elements.amount.value);
      if (!amt || Number.isNaN(amt)) throw fieldError('amount', 'Enter an amount (negative to remove credit).');
      await api('POST', `/accounts/${a.id}/credit`, { amount: amt, description: f.elements.description.value.trim() });
      notify(`${money(amt)} credit ${amt > 0 ? 'added' : 'removed'}`);
    },
  });
}

// ---- Billing contact (staff and the client area) ----

const CONTACT_KEYS = ['first_name', 'last_name', 'company', 'email', 'phone', 'address1', 'address2', 'city', 'state', 'postcode', 'country', 'tax_id'];

function contactFields(c = {}) {
  const country = h('select', { name: 'country', autocomplete: 'country', required: true }, h('option', { value: '' }, 'Choose…'),
    countryList().map(([code, name]) => h('option', { value: code, selected: code === (c.country || guessCountry()) }, name)));
  const state = h('input', { name: 'state', autocomplete: 'address-level1', value: c.state || '' });
  const stateLabel = h('span', {}, 'State / province');
  const sync = () => {
    state.required = STATE_COUNTRIES.includes(country.value);
    stateLabel.textContent = state.required ? 'State / province' : 'State / province (optional)';
  };
  country.addEventListener('change', sync);
  sync();
  const inp = (name, auto, attrs = {}) => h('input', { name, autocomplete: auto, value: c[name] || '', ...attrs });
  return h('div', { class: 'grid contact-grid' },
    field('First name', inp('first_name', 'given-name', { required: true })),
    field('Last name', inp('last_name', 'family-name', { required: true })),
    field('Company (optional)', inp('company', 'organization')),
    field('E-mail for invoices', inp('email', 'email', { type: 'email', required: true })),
    field('Phone (optional)', inp('phone', 'tel', { type: 'tel' })),
    field('Address', inp('address1', 'address-line1', { required: true })),
    field('Address, line 2 (optional)', inp('address2', 'address-line2')),
    field('City', inp('city', 'address-level2', { required: true })),
    field(stateLabel, state),
    field('Postcode', inp('postcode', 'postal-code')),
    field('Country', country),
    field('Tax ID (optional)', inp('tax_id', 'off'), 'VAT, GST or a similar number, if you\'re a registered business.'));
}

const readContact = (form) => Object.fromEntries(CONTACT_KEYS.map((k) => [k, ((form.elements[k] && form.elements[k].value) || '').trim()]));

function editContact(accountId, contact) {
  return openDialog({
    title: 'Billing contact', wide: true, intro: 'The name and address on invoices, and where they\'re e-mailed.',
    body: [contactFields(contact || {})],
    async submit(f) {
      await api('PUT', `/accounts/${accountId}/billing/contact`, readContact(f));
      notify('Billing contact saved');
    },
  });
}

// ---- Changing plan, cancelling (staff and the client area) ----

const planPrice = (p, cycle) => (p.prices && p.prices[cycle]) || null;
const planCycles = (p) => CYCLES.filter((c) => planPrice(p, c.id));

// planLimits is a plan's headline limits in plain words.
function planLimits(p) {
  const n = (v, unit) => (v ? `${fmtNum(v)} ${unit}` : `Unlimited ${unit}`);
  return [n(p.max_sites, p.max_sites === 1 ? 'site' : 'sites'), p.disk_mb ? `${p.disk_mb % 1024 === 0 ? `${fmtNum(p.disk_mb / 1024)} GB` : `${fmtNum(p.disk_mb)} MB`} disk` : 'Unlimited disk',
    p.bandwidth_gb ? `${fmtNum(p.bandwidth_gb)} GB bandwidth / month` : 'Unlimited bandwidth'];
}

// A promotion is an item with a negative amount and also the "discount"
// total: show it once, as the line under the subtotal, named after it.
const notDiscount = (it) => it.kind !== 'discount';
const discountLabel = (items) => (items || []).filter((it) => !notDiscount(it)).map((it) => it.description).join(', ') || 'Discount';

// quoteLines shows a quote (plan change or order): lines, subtotal,
// discount, taxes, total.
function quoteLines(q, { totalLabel = 'Due now' } = {}) {
  const rows = [];
  for (const it of (q.items || []).filter(notDiscount)) rows.push([it.description, money(it.amount)]);
  if (!(q.items || []).length) {
    if (q.credit) rows.push(['Credit for the unused time', '−' + money(q.credit)]);
    if (q.charge != null) rows.push(['New plan', money(q.charge)]);
  }
  if (q.subtotal != null && ((q.tax_lines || []).length || q.discount)) rows.push(['Subtotal', money(q.subtotal)]);
  if (q.discount) rows.push([discountLabel(q.items), '−' + money(q.discount)]);
  for (const t of q.tax_lines || []) rows.push([`${t.name} ${fmtRate(t.rate)}`, money(t.amount)]);
  return h('dl', { class: 'quote-lines' }, rows.map(([k, v]) => h('div', {}, h('dt', {}, k), h('dd', {}, v))),
    h('div', { class: 'grand' }, h('dt', {}, totalLabel), h('dd', {}, money(Math.max(0, q.total || 0)))));
}

// openPlanChange compares plans and quotes the switch as the choice
// changes. Resolves to the API's answer (an invoice to pay, or applied).
async function openPlanChange({ accountId, planId, cycle, tenant }) {
  await billingConfig();
  const plans = (await api('GET', '/plans')).filter((p) => planCycles(p).length && (p.id === planId || !tenant || p.public !== false));
  if (!plans.some((p) => p.id !== planId)) {
    showError(new Error(tenant ? 'There are no other plans to switch to right now. Contact us if you need more.' : 'No other plan has prices: add them under Plans.'));
    return null;
  }
  const state = { plan: planId, cycle: cycle || 'monthly' };
  const cards = h('div', { class: 'plan-pick', role: 'radiogroup', 'aria-label': 'Plans' });
  const cycleSel = h('select', { name: 'cycle' });
  const quote = h('div', { class: 'quote', 'aria-live': 'polite' });
  let ctx = null, seq = 0;
  const drawCards = () => cards.replaceChildren(...plans.map((p) => {
    const pr = planPrice(p, state.cycle) || planPrice(p, planCycles(p)[0].id);
    const c = planPrice(p, state.cycle) ? cycleOf(state.cycle) : planCycles(p)[0];
    return h('label', { class: 'choice plan-choice' }, h('input', { type: 'radio', name: 'plan_id', value: p.id, checked: p.id === state.plan }),
      h('span', {}, h('strong', {}, p.name, p.id === planId ? h('span', { class: 'badge' }, 'current') : null),
        h('span', { class: 'plan-price' }, money(pr.price), h('span', { class: 'muted small' }, ` / ${c.per}`)),
        p.description ? h('span', { class: 'muted small' }, p.description) : null,
        h('span', { class: 'muted small' }, planLimits(p).join(' · '))));
  }));
  const drawCycles = () => {
    const p = plans.find((x) => x.id === state.plan);
    const avail = planCycles(p);
    if (!avail.some((c) => c.id === state.cycle)) state.cycle = avail[0].id;
    cycleSel.replaceChildren(...options(avail.map((c) => [c.id, `${c.label} · ${money(planPrice(p, c.id).price)}`]), state.cycle));
  };
  const refresh = debounce(async () => {
    const n = ++seq;
    if (state.plan === planId && state.cycle === (cycle || 'monthly')) {
      quote.replaceChildren(h('p', { class: 'muted small' }, 'Choose another plan or billing period to see what changes.'));
      if (ctx) ctx.ok.disabled = true;
      return;
    }
    quote.replaceChildren(h('p', { class: 'muted small' }, 'Working out the price…'));
    try {
      const q = await api('POST', `/accounts/${accountId}/plan-change/quote`, { plan_id: state.plan, cycle: state.cycle });
      if (n !== seq) return;
      quote.replaceChildren(quoteLines(q),
        h('p', { class: 'muted small' }, (q.total || 0) <= 0
          ? `Nothing to pay now${q.total < 0 ? `: ${money(-q.total)} goes to the account's credit` : ''}. The plan changes straight away.`
          : 'The new plan starts as soon as this is paid.',
        q.new_next_due_at ? ` Next renewal ${fmtDate(q.new_next_due_at)}.` : ''));
      if (ctx) ctx.ok.disabled = false;
    } catch (e) {
      if (n !== seq) return;
      quote.replaceChildren(h('p', { class: 'form-error' }, e.status === 404 ? 'Plan changes aren\'t available on this server yet.' : e.message));
      if (ctx) ctx.ok.disabled = true;
    }
  }, 250);
  cards.addEventListener('change', (e) => { state.plan = e.target.value; drawCycles(); drawCards(); cards.querySelector('input:checked').focus(); refresh(); });
  cycleSel.addEventListener('change', () => { state.cycle = cycleSel.value; drawCards(); refresh(); });
  drawCycles();
  drawCards();
  return openDialog({
    title: 'Change plan', wide: true, ok: 'Change plan',
    intro: 'What\'s left of the current period counts towards the new plan.',
    body: [cards, field('Billing period', cycleSel), quote],
    onOpen(c) { ctx = c; refresh(); },
    async submit() {
      const res = await api('POST', `/accounts/${accountId}/plan-change`, { plan_id: state.plan, cycle: state.cycle });
      if (res && res.invoice) notify(`Invoice ${invoiceTitle(res.invoice)} created: the plan changes once it's paid`);
      else notify('Plan changed');
      return res || true;
    },
  });
}

function openCancel({ accountId, name, admin, nextDue }) {
  const when = admin ? h('div', { class: 'choices' },
    choice('when', 'end_of_period', 'At the end of the period', nextDue ? `On ${fmtDate(nextDue)}; nothing more is billed.` : 'Nothing more is billed.', true),
    choice('when', 'immediately', 'Right away', 'The account is closed now; unpaid invoices are cancelled.')) : null;
  return openDialog({
    title: admin ? `Cancel ${name}'s service?` : 'Cancel your service?', ok: 'Cancel service', danger: true, cancel: 'Keep it',
    intro: admin ? null : `Your sites keep working until ${nextDue ? fmtDate(nextDue) : 'the end of the period you paid for'}; then the account is closed ` +
      'and nothing more is billed. You can change your mind until then.',
    body: [when ? h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'When'), when) : null,
      field(admin ? 'Reason (optional)' : 'Why are you leaving? (optional)', h('textarea', { name: 'reason', rows: 3, maxlength: 1000 }))],
    async submit(f) {
      await api('POST', `/accounts/${accountId}/cancel`, { when: admin ? f.elements.when.value : 'end_of_period', reason: f.elements.reason.value.trim() });
      notify('Cancellation scheduled');
    },
  });
}

// ---- Plans: prices per billing period (the Plans form in accounts.js) ----

// PLAN_PRICING: the server's plans have prices (older servers reject the
// fields, so the form only sends them when it does).
let PLAN_PRICING = false;

function renderPriceEditor(el, plan = {}) {
  const prices = plan.prices || {};
  const rows = CYCLES.map((c) => {
    const pr = prices[c.id];
    const offer = h('input', { type: 'checkbox', name: `offer_${c.id}`, checked: !!pr, 'aria-label': `Offer ${c.label.toLowerCase()}` });
    const price = h('input', { name: `price_${c.id}`, inputmode: 'decimal', value: pr ? fromMinor(pr.price) : '', 'aria-label': `${c.label} price`, placeholder: fromMinor(0) });
    const setup = h('input', { name: `setup_${c.id}`, inputmode: 'decimal', value: pr ? fromMinor(pr.setup_fee) : '', 'aria-label': `${c.label} setup fee`, placeholder: fromMinor(0) });
    const per = h('td', { class: 'num muted small' });
    const sync = () => {
      price.disabled = setup.disabled = !offer.checked;
      const v = toMinor(price.value);
      per.textContent = offer.checked && v && c.months > 1 ? `${money(Math.round(v / c.months))} / month` : '';
    };
    offer.addEventListener('change', () => { sync(); if (offer.checked) price.focus(); });
    price.addEventListener('input', sync);
    sync();
    const sym = () => h('span', { class: 'affix-text', 'aria-hidden': 'true' }, currencySymbol());
    return h('tr', {}, h('td', {}, h('label', { class: 'check' }, offer, c.label)), h('td', {}, h('span', { class: 'affix' }, sym(), price)),
      h('td', {}, h('span', { class: 'affix' }, sym(), setup)), per);
  });
  el.replaceChildren(h('table', { class: 'price-table' }, h('thead', {}, h('tr', {}, h('th', { scope: 'col' }, 'Billing period'),
    h('th', { scope: 'col' }, 'Price'), h('th', { scope: 'col' }, 'Setup fee'), h('th', { scope: 'col', class: 'num' }, 'Works out at'))), h('tbody', {}, rows)));
}

// readPriceEditor is the prices the form offers ({} when none: free).
function readPriceEditor(form) {
  const out = {};
  for (const c of CYCLES) {
    if (!form.elements[`offer_${c.id}`] || !form.elements[`offer_${c.id}`].checked) continue;
    const price = toMinor(form.elements[`price_${c.id}`].value), setup = toMinor(form.elements[`setup_${c.id}`].value);
    if (price == null || Number.isNaN(price) || price < 0) throw new Error(`Enter the ${c.label.toLowerCase()} price, or untick it.`);
    if (Number.isNaN(setup) || setup < 0) throw new Error(`The ${c.label.toLowerCase()} setup fee isn't an amount.`);
    out[c.id] = { price, setup_fee: setup || 0 };
  }
  return out;
}

// planPriceText is a plan's price for the Plans table.
function planPriceText(p) {
  const cs = planCycles(p);
  if (!cs.length) return h('span', { class: 'muted' }, 'free');
  const first = cs[0];
  return h('span', {}, `${money(planPrice(p, first.id).price)} / ${first.per}`, cs.length > 1 ? h('span', { class: 'sub-line' }, `${cs.length} billing periods`) : null);
}

// ---- The sign-in screen: a link to the order page when the store is open ----

document.addEventListener('DOMContentLoaded', () => {
  const link = $('#order-link');
  if (!link) return;
  fetch('/api/v1/store/catalog', { credentials: 'same-origin', headers: { 'X-Requested-With': 'wpgenie' } })
    .then((r) => (r.ok ? r.json() : null))
    .then((c) => { link.hidden = !(c && c.enabled && (c.plans || []).length); })
    .catch(() => {});
});
