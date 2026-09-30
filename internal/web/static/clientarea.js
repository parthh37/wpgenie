'use strict';
// The client area: a customer's or reseller's own billing (the Billing tab
// for tenants). What's due and paying it, the plan and changing it, the
// saved card, invoices, payments, billing details and e-mails. Uses
// billing.js's helpers; the server scopes every call to the user's account.

const CLIENT_VIEWS = [['', 'Overview', 'dashboard'], ['invoices', 'Invoices', 'receipt'], ['transactions', 'Payments', 'wallet'],
  ['details', 'Billing details', 'building'], ['emails', 'E-mails', 'mail']];

async function renderClientArea(route) {
  const root = $('#billing-root');
  const [key, , ] = CLIENT_VIEWS.find(([k]) => k === route.view) || CLIENT_VIEWS[0];
  const notices = h('div', { class: 'client-notices' });
  const box = h('div', { class: 'bview', 'aria-busy': 'true' }, h('p', { class: 'muted small b-loading' }, 'Loading…'));
  root.replaceChildren(
    h('div', { class: 'bar' }, h('div', {}, h('h1', {}, 'Billing'), h('p', { class: 'muted small bar-sub' }, 'Your plan, invoices and payments.'))),
    notices,
    subnav('Billing sections', CLIENT_VIEWS.map(([k, label, ic]) => ({ key: k, label, icon: ic, href: '#/billing' + (k ? '/' + k : '') })), key, (k) => billingGo(k)),
    box);
  try {
    await billingConfig();
    const acct = (await api('GET', '/account')).account;
    if (!acct) throw new Error('This user doesn\'t belong to an account.');
    const profile = await api('GET', `/accounts/${acct.id}/billing`).catch((e) => { if (e.status === 404) return null; throw e; });
    const ctx = { acct, profile, reload: () => renderClientArea(BROUTE) };
    notices.replaceChildren(...clientNotices(ctx));
    await ({ '': clientOverview, invoices: clientInvoices, transactions: clientPayments, details: clientDetails, emails: clientEmails })[key](box, ctx, route);
  } catch (e) {
    box.replaceChildren(loadError(e, () => renderClientArea(route)));
  } finally { box.removeAttribute('aria-busy'); }
}

// clientNotices: what needs doing, above every Billing screen.
function clientNotices(ctx) {
  const { acct, profile: p } = ctx;
  const pay = (label = 'Pay now') => actionButton(label, () => payDue(ctx), { cls: '', ic: 'card' });
  const out = [];
  if (acct.status === 'pending') {
    out.push(banner('warn', 'clock', 'Your order is waiting for payment', 'Pay the invoice to activate your account: you can create sites as soon as it\'s paid.', pay('Pay and activate')));
  } else if (acct.status === 'suspended' && acct.suspend_reason === 'billing') {
    out.push(banner('bad', 'lock', 'Your sites are suspended for an unpaid invoice', 'Pay what\'s due and they\'re back online within a minute.', pay()));
  } else if (p && p.overdue) {
    out.push(banner('bad', 'alert', `${money(p.balance_due)} is overdue`, 'Please pay it to keep your sites online.', pay()));
  } else if (p && p.balance_due > 0) {
    out.push(banner('info', 'receipt', `${money(p.balance_due)} to pay`, null, pay()));
  }
  if (p && p.cancel_at) {
    out.push(banner('info', 'calendar', `Your service ends on ${fmtDate(p.cancel_at)}`, 'Nothing more is billed. Changed your mind?',
      actionButton('Keep my service', async () => {
        await api('DELETE', `/accounts/${acct.id}/cancel`);
        notify('Your service continues');
        ctx.reload();
      })));
  }
  return out;
}

// payDue pays the one invoice due, or shows them when there are several.
async function payDue(ctx) {
  const unpaid = listOf(await api('GET', '/invoices?status=unpaid&limit=50'), 'invoices');
  if (unpaid.length === 1) {
    if (await openPay(unpaid[0], ctx)) ctx.reload();
  } else {
    CLIENT_FILTER = 'unpaid';
    billingGo('invoices');
  }
}

// ---- Overview ----

async function clientOverview(box, ctx) {
  const { acct, profile: p } = ctx;
  const [invoices, burst] = await Promise.all([api('GET', '/invoices?limit=5').then((r) => listOf(r, 'invoices')).catch(() => []),
    api('GET', `/accounts/${acct.id}/burst`).catch(() => null)]);
  if (!p) {
    box.replaceChildren(emptyState('receipt', 'Billing isn\'t set up here', 'Your provider hasn\'t turned on billing in the panel. Contact them about invoices and payments.'));
    return;
  }
  const billed = p.mode === 'invoice';
  const plan = acct.plan || { name: acct.plan_id };
  const nextAmount = p.upcoming ? p.upcoming.amount : p.recurring_amount;
  const nextDate = p.upcoming ? p.upcoming.period_start : p.next_due_at;

  const planCard = h('div', { class: 'card plan-card' },
    h('div', { class: 'card-head' }, h('div', {}, h('span', { class: 'muted small' }, 'Your plan'), h('h2', { class: 'plan-name' }, plan.name)),
      billed && acct.status === 'active' && !p.cancel_at
        ? actionButton('Change plan', async () => {
          const r = await openPlanChange({ accountId: acct.id, planId: acct.plan_id, cycle: p.cycle, tenant: true });
          if (r && r.invoice) billingGo('invoices', r.invoice.id);
          else if (r) ctx.reload();
        }, { ic: 'layers' }) : null),
    billed && p.recurring_amount != null ? h('p', { class: 'plan-price' }, money(p.recurring_amount), h('span', { class: 'muted small' }, ` / ${cycleOf(p.cycle).per}`)) : null,
    h('ul', { class: 'plan-limits' }, planLimits(plan).map((x) => h('li', {}, icon('check'), x))),
    billed && p.next_due_at ? h('p', { class: 'muted small' }, p.cancel_at ? `Ends ${fmtDate(p.cancel_at)}.` : `Renews ${fmtDate(p.next_due_at)}.`) : null,
    !billed ? h('p', { class: 'muted small' }, p.mode === 'whmcs' ? 'Billed through your provider\'s billing system.'
      : p.mode === 'stripe_subscription' ? 'Billed by a Stripe subscription.' : 'Not billed through this panel.') : null);

  const cardText = p.card ? `${cap(p.card.brand || 'card')} •••• ${p.card.last4}` : 'No card saved';
  const auto = h('input', { type: 'checkbox', class: 'switch', checked: !!p.auto_pay, disabled: !p.card, 'aria-describedby': 'autopay-help' });
  auto.addEventListener('change', async () => {
    auto.disabled = true;
    try {
      await api('PUT', `/accounts/${acct.id}/billing/auto-pay`, { auto_pay: auto.checked });
      notify(auto.checked ? 'Invoices are paid with your card automatically' : 'Automatic payment is off');
    } catch (e) { showError(e); auto.checked = !auto.checked; } finally { auto.disabled = !p.card; }
  });
  const payCard = h('div', { class: 'card' },
    h('span', { class: 'muted small' }, 'Payment method'),
    h('div', { class: 'saved-card' }, icon('card'), h('div', {}, h('strong', {}, cardText),
      p.card ? h('span', { class: 'muted small' }, `Expires ${String(p.card.exp_month).padStart(2, '0')}/${String(p.card.exp_year).slice(-2)}`) : null)),
    h('label', { class: 'toggle' }, auto, h('span', {}, 'Pay invoices automatically')),
    h('p', { class: 'field-help', id: 'autopay-help' }, p.card
      ? 'Your card is charged on each due date; you get a receipt by e-mail.'
      : 'To save a card, pay an invoice by card and tick "Save my card".'),
    p.card ? actionButton('Remove card', async () => {
      if (!await ask('Remove your saved card? Automatic payment turns off; you can pay each invoice yourself.', { ok: 'Remove card', danger: true })) return;
      await api('DELETE', `/accounts/${acct.id}/billing/card`);
      notify('Card removed');
      ctx.reload();
    }, { cls: 'ghost danger' }) : null);

  box.replaceChildren(
    h('div', { class: 'kpis' },
      kpi('Balance due', money(p.balance_due || 0), p.overdue ? 'overdue' : p.balance_due ? 'to pay' : 'nothing to pay', {
        ic: 'receipt', tone: p.overdue ? 'bad' : p.balance_due ? 'warn' : 'ok', onClick: p.balance_due ? () => payDue(ctx) : null,
      }),
      kpi('Credit', money(p.credit || 0), p.credit ? 'used for your next invoices' : 'none', { ic: 'tag' }),
      billed && nextDate ? kpi('Next payment', money(nextAmount), `on ${fmtDate(nextDate)}`, { ic: 'calendar' }) : null),
    h('div', { class: 'bgrid two' }, planCard, billed ? payCard : null, burstCard(ctx, burst)),
    h('div', { class: 'card' }, h('div', { class: 'card-head' }, h('h2', {}, 'Recent invoices'), linkButton('All invoices', () => billingGo('invoices'))),
      clientInvoiceTable(invoices, ctx)),
    billed && acct.status === 'active' && !p.cancel_at
      ? h('p', { class: 'small cancel-line' }, linkButton('Cancel my service…', async () => {
        if (await openCancel({ accountId: acct.id, nextDue: p.next_due_at })) ctx.reload();
      })) : null);
}

const cap = (s) => s.charAt(0).toUpperCase() + s.slice(1);

function clientInvoiceTable(list, ctx) {
  return btable(['Invoice', 'Date', 'Due', { label: 'Total', num: true }, 'Status', ''], list.map((inv) => ({
    cls: invoiceState(inv) === 'overdue' ? 'row-overdue' : '',
    open: () => billingGo('invoices', inv.id),
    cells: [invoiceLink(inv), fmtDate(inv.issued_at), h('td', {}, fmtDate(inv.due_at), dueText(inv) ? h('span', { class: 'sub-line' }, dueText(inv)) : null),
      money(inv.total), h('td', {}, invoicePill(inv)),
      h('td', { class: 'row-actions' }, inv.status === 'unpaid'
        ? actionButton(`Pay ${money(inv.balance)}`, async () => { if (await openPay(inv, ctx)) ctx.reload(); }, { cls: '' }) : null)],
  })), { caption: 'Invoices', empty: emptyState('receipt', 'No invoices yet', 'Your invoices appear here as soon as they\'re issued, and we e-mail each one.') });
}

// ---- Invoices ----

let CLIENT_FILTER = '';

async function clientInvoices(box, ctx, route) {
  if (route.id) return clientInvoice(box, ctx, route.id, route.query);
  const list = listOf(await api('GET', '/invoices?limit=100' + (CLIENT_FILTER ? '&status=' + CLIENT_FILTER : '')), 'invoices');
  box.replaceChildren(h('div', { class: 'card' },
    h('div', { class: 'toolbar' }, chips('Show invoices', [['', 'All'], ['unpaid', 'To pay'], ['paid', 'Paid']], CLIENT_FILTER,
      (k) => { CLIENT_FILTER = k; clientInvoices(box, ctx, route).catch((e) => box.replaceChildren(loadError(e))); })),
    clientInvoiceTable(list, ctx)));
}

async function clientInvoice(box, ctx, id, query) {
  let inv = await api('GET', `/invoices/${encodeURIComponent(id)}`);
  const returned = query && query.get('paid') === '1';
  const thanks = h('div', { 'aria-live': 'polite' });
  const draw = () => {
    const acts = [];
    if (inv.status === 'unpaid') {
      acts.push(actionButton(`Pay ${money(inv.balance)}`, async () => { if (await openPay(inv, ctx)) ctx.reload(); }, { cls: '', ic: 'card' }));
      if (ctx.profile && ctx.profile.credit > 0) {
        acts.push(actionButton(`Use my credit (${money(ctx.profile.credit)})`, async () => {
          if (!await ask(`Pay ${invoiceTitle(inv)} from your credit? Up to ${money(Math.min(ctx.profile.credit, inv.balance))} is used.`, { ok: 'Use credit' })) return;
          await api('POST', `/invoices/${inv.id}/apply-credit`, { amount: null });
          notify('Credit applied');
          ctx.reload();
        }, { ic: 'tag' }));
      }
    }
    acts.push(h('a', { class: 'button ghost', href: `/api/v1/invoices/${inv.id}/print`, target: '_blank', rel: 'noopener' }, icon('printer'), 'Print / PDF'));
    box.replaceChildren(backLink('All invoices', 'invoices'), thanks,
      h('div', { class: 'inv-top' }, h('div', {}, h('h2', { class: 'inv-title' }, invoiceTitle(inv), invoicePill(inv)),
        h('p', { class: 'muted small' }, INVOICE_KINDS[inv.kind] || 'Invoice', dueText(inv) ? ` · ${dueLine(inv)}` : '')),
      h('div', { class: 'actions inv-actions' }, acts)),
      h('div', { class: 'inv-layout' }, invoiceDocument(inv),
        h('aside', { class: 'inv-side' }, h('div', { class: 'card' }, h('h2', {}, 'Payments'), paymentsTable(inv)))));
  };
  draw();
  if (!returned) return;
  // Back from the payment page: the payment is confirmed to us by the
  // provider, usually within seconds. Wait for it here.
  const say = (tone, ic, title, text) => thanks.replaceChildren(banner(tone, ic, title, text));
  if (inv.status === 'paid') { say('ok', 'check', 'Payment received, thank you!', 'A receipt is on its way to your inbox.'); return; }
  say('info', 'clock', 'Thank you! Confirming your payment…', 'This usually takes a few seconds.');
  thanks.firstChild.classList.add('busy');
  for (let i = 0; i < 30 && thanks.isConnected; i++) {
    await new Promise((r) => setTimeout(r, 2000));
    try { inv = await api('GET', `/invoices/${encodeURIComponent(id)}`); } catch (e) { continue; }
    if (inv.status === 'paid') {
      history.replaceState(null, '', `#/billing/invoices/${inv.id}`);
      // Everything changes: banners, balance, perhaps the account's status.
      await renderClientArea({ view: 'invoices', id: String(inv.id), query: new URLSearchParams() });
      const top = $('#billing-root .bview');
      if (top) top.prepend(banner('ok', 'check', 'Payment received, thank you!', 'A receipt is on its way to your inbox.'));
      return;
    }
  }
  if (thanks.isConnected) {
    say('warn', 'clock', 'We haven\'t heard back from the payment provider yet', 'If you completed the payment, it will show up here shortly and ' +
      'you\'ll get a receipt by e-mail. There\'s no need to pay again.');
  }
}

// openPay: choose how to pay, then go to the payment page, or see the bank
// details. Resolves truthy when something changed (paid, or credit used).
async function openPay(inv, ctx) {
  await billingConfig();
  const methods = billingMethods();
  const credit = (ctx.profile && ctx.profile.credit) || 0;
  if (!methods.length && !credit) {
    showError(new Error('No payment methods are set up yet. Please contact us to pay this invoice.'));
    return null;
  }
  const list = h('div', { class: 'choices pay-methods' }, methods.map((m, i) => choice('method', m.id, m.name || methodName(m.id),
    m.description || { stripe: 'Pay securely on Stripe\'s page.', razorpay: 'UPI, cards, netbanking and wallets.', manual: 'We show you our bank details.' }[m.id] || '',
    i === 0)));
  const save = toggle('save_card', 'Save my card for automatic payments', !(ctx.profile && ctx.profile.card), 'Your card details stay with Stripe.');
  const useCredit = credit > 0 ? toggle('use_credit', `Use my credit first (${money(credit)} available)`, true) : null;
  const total = h('p', { class: 'live-total pay-total', 'aria-live': 'polite' });
  let form = null;
  const sync = () => {
    if (!form) return;
    const m = form.elements.method ? form.elements.method.value : '';
    save.hidden = m !== 'stripe';
    const fromCredit = useCredit && form.elements.use_credit.checked ? Math.min(credit, inv.balance) : 0;
    const due = inv.balance - fromCredit;
    total.replaceChildren(h('span', {}, due > 0 ? 'You pay' : 'Paid from your credit'), h('strong', {}, money(due > 0 ? due : fromCredit)));
    if (form.elements.method) for (const r of form.querySelectorAll('input[name=method]')) r.disabled = due <= 0;
  };
  let paid = false;
  const res = await openDialog({
    title: `Pay ${invoiceTitle(inv)}`, ok: 'Continue',
    body: [methods.length ? h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'How would you like to pay?'), list) : null, save, useCredit, total],
    onOpen(c) { form = c.form; form.addEventListener('change', sync); sync(); },
    async submit(f, c) {
      if (useCredit && f.elements.use_credit.checked) {
        await api('POST', `/invoices/${inv.id}/apply-credit`, { amount: null });
        paid = true;
        inv = await api('GET', `/invoices/${inv.id}`);
        if (inv.status === 'paid' || inv.balance <= 0) { notify('Paid from your credit, thank you!'); return true; }
        f.elements.use_credit.checked = false;
        useCredit.hidden = true;
      }
      const method = f.elements.method && f.elements.method.value;
      if (!method) throw new Error('Choose how to pay.');
      const r = await api('POST', `/invoices/${inv.id}/pay`, { method, save_card: method === 'stripe' && f.elements.save_card.checked });
      return payNext(r, c, inv.balance);
    },
  });
  return res || paid;
}

// payNext follows the answer to a payment (an invoice's, or a pack's):
// off to the payment page, bank details shown in the dialog, or paid.
// Returns what the dialog's submit should (false: stay open).
function payNext(r, c, amount) {
  if (r.redirect_url) {
    const url = new URL(r.redirect_url, location.href);
    if (url.protocol !== 'https:' && url.origin !== location.origin) throw new Error('The payment page address looks wrong; please contact us.');
    c.ok.disabled = true;
    c.body.replaceChildren(h('p', { class: 'small' }, 'Taking you to the payment page…'));
    location.assign(url.href);
    return false;
  }
  if (r.paid) { notify('Paid, thank you!'); return true; }
  if (r.instructions != null) {
    // Bank transfer: the details, and the reference to quote.
    c.body.replaceChildren(
      h('p', {}, `Please transfer ${money(amount)} using these details:`),
      h('pre', { class: 'instructions' }, r.instructions),
      r.reference ? field('Payment reference', copyText(r.reference), 'Quote it so we can match your payment.') : null,
      h('p', { class: 'muted small' }, 'We\'ll mark the invoice paid and e-mail a receipt when the money arrives, usually within 1–2 working days.'));
    c.ok.hidden = true;
    c.cancel.textContent = 'Done';
    c.cancel.focus();
    return false;
  }
  return true;
}

// ---- Burst minute packs ----

const burstPacks = () => ((BILLING && BILLING.burst_packs) || []).filter((p) => p.minutes > 0);
// Packs are sold to accounts billed here, when there are any to sell.
const canBuyBurst = (profile) => !!profile && profile.mode === 'invoice' && burstPacks().length > 0;

// openBuyBurst sells a pack of burst minutes: choose one and how to pay,
// then pay like an invoice (the purchase is one). Resolves truthy once
// something changed.
async function openBuyBurst(accountId, profile) {
  await billingConfig();
  const packs = burstPacks().slice().sort((a, b) => a.minutes - b.minutes);
  if (!packs.length) { showError(new Error('No burst minute packs are for sale right now.')); return null; }
  const credit = (profile && profile.credit) || 0;
  const list = h('div', { class: 'choices pack-pick' }, packs.map((p, i) => choice('pack', p.id, `${fmtNum(p.minutes)} minutes`,
    `${money(p.price)} · ${money(Math.round(p.price / p.minutes * 60))} an hour of extra capacity`, i === 0)));
  const methods = billingMethods();
  const pay = h('div', { class: 'choices' });
  let form = null;
  const sync = () => {
    if (!form) return;
    const pack = packs.find((p) => p.id === form.elements.pack.value) || packs[0];
    const was = form.elements.method ? form.elements.method.value : '';
    const opts = [...(credit >= pack.price ? [{ id: 'credit', name: 'My credit', description: `${money(credit)} available` }] : []), ...methods];
    const pick = opts.some((m) => m.id === was) ? was : (opts[0] || {}).id;
    pay.replaceChildren(...opts.map((m) => choice('method', m.id, m.name || methodName(m.id), m.description || '', m.id === pick)));
  };
  return openDialog({
    title: 'Buy burst minutes', ok: 'Buy', wide: true,
    intro: 'Bought minutes never expire: they\'re used once the month\'s included minutes run out, and your sites keep their extra capacity under load.',
    body: [h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Pack'), list),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Pay with'), pay)],
    onOpen(c) { form = c.form; list.addEventListener('change', sync); sync(); },
    async submit(f, c) {
      const pack = packs.find((p) => p.id === f.elements.pack.value);
      const method = f.elements.method && f.elements.method.value;
      if (!method) throw new Error('No payment method is set up yet: please contact us.');
      const r = await api('POST', `/accounts/${accountId}/burst/buy`, { pack: pack.id, method });
      const next = r.next || {};
      if (next.paid || method === 'credit') {
        notify(`${fmtNum(pack.minutes)} burst minutes added, thank you!`);
        return true;
      }
      return payNext(next, c, (r.invoice && r.invoice.balance) ?? pack.price);
    },
  });
}

// burstBuyButton is a "Buy more minutes" button for the site screens' burst
// status: filled in once it's known the account may buy packs.
function burstBuyButton() {
  const slot = h('span', { class: 'burst-buy' });
  if (!isTenant()) return slot;
  (async () => {
    await billingConfig();
    const a = (await api('GET', '/account')).account;
    const p = await api('GET', `/accounts/${a.id}/billing`);
    if (!canBuyBurst(p)) return;
    slot.replaceChildren(actionButton('Buy more minutes', async () => {
      if (await openBuyBurst(a.id, p)) notify('Burst resumes within a minute.');
    }, { ic: 'zap' }));
  })().catch(() => {});
  return slot;
}

// burstCard is the Burst minutes card of the client area's overview.
function burstCard(ctx, b) {
  if (!b || !b.allowed) return null;
  const left = Math.max(0, (b.included || 0) - (b.used || 0));
  const buy = canBuyBurst(ctx.profile) ? actionButton('Buy minutes', async () => {
    if (await openBuyBurst(ctx.acct.id, ctx.profile)) ctx.reload();
  }, { ic: 'zap' }) : null;
  return h('div', { class: 'card burst-card' },
    h('div', { class: 'card-head' }, h('div', {}, h('span', { class: 'muted small' }, 'Burst minutes'),
      h('h2', { class: 'plan-name' }, b.unlimited ? 'Unlimited' : `${fmtNum(b.remaining ?? left + (b.credit || 0))} left`)), buy),
    b.unlimited ? h('p', { class: 'muted small' }, 'Your plan includes unlimited burst minutes.') : [
      h('meter', { min: 0, max: b.included || 1, value: left, low: (b.included || 1) * 0.2, high: (b.included || 1) * 0.5, optimum: b.included || 1,
        'aria-label': 'Monthly burst minutes left' }),
      h('p', { class: 'muted small' }, `${fmtNum(left)} of ${fmtNum(b.included || 0)} this month` +
        (b.credit ? ` · ${fmtNum(b.credit)} bought minutes that never expire` : '') + '.'),
      !b.remaining && !buy ? h('p', { class: 'small st-warning' }, 'Out of burst minutes: sites stay at their normal size until the month starts again.') : null]);
}

// ---- Payments, details, e-mails ----

async function clientPayments(box) {
  const list = listOf(await api('GET', '/transactions?limit=100'), 'transactions');
  box.replaceChildren(h('div', { class: 'card' }, btable(['Date', 'Invoice', 'Method', 'Reference', { label: 'Amount', num: true }], list.map((t) => [
    fmtDate(t.at), t.invoice_id ? invoiceLink({ id: t.invoice_id, number: t.invoice_number }) : '–', methodName(t.gateway || t.method),
    h('td', { class: 'wrap' }, t.reference || '–'),
    h('td', { class: 'num' }, money(t.amount), t.refunded ? h('span', { class: 'sub-line' }, `${money(t.refunded)} refunded`) : null)]),
  { caption: 'Payments', empty: emptyState('wallet', 'No payments yet', 'Your payments and refunds show up here.') })));
}

async function clientDetails(box, ctx) {
  const { acct, profile: p } = ctx;
  if (!p) { box.replaceChildren(emptyState('building', 'Billing isn\'t set up here', 'There are no billing details to keep.')); return; }
  box.replaceChildren(settingsForm('Billing details', 'The name and address on your invoices, and where we send them. Taxes depend on your country.',
    contactFields(p.contact || {}), async (f) => {
      await api('PUT', `/accounts/${acct.id}/billing/contact`, readContact(f));
      notify('Billing details saved');
    }, 'Save details'));
}

async function clientEmails(box, ctx) {
  const { acct } = ctx;
  const list = await api('GET', `/accounts/${acct.id}/emails?limit=100`);
  box.replaceChildren(h('div', { class: 'card' }, h('p', { class: 'muted small' }, 'Messages we\'ve sent you: invoices, receipts and reminders.'),
    btable(['Sent', 'Subject', ''], list.map((m) => ({
      open: () => openFrame(m.subject, `/api/v1/accounts/${acct.id}/emails/${m.id}/html`),
      cells: [fmtTime(m.sent_at || m.created_at), h('td', {}, linkButton(m.subject, () => openFrame(m.subject, `/api/v1/accounts/${acct.id}/emails/${m.id}/html`))),
        h('td', {}, m.status === 'sent' ? '' : bpill('mail-' + m.status, m.status === 'failed' ? 'not delivered' : 'sending'))],
    })), { caption: 'E-mails', empty: emptyState('mail', 'No e-mails yet', 'Invoices and receipts we e-mail you also appear here.') })));
}

// ---- After sign-in (app.js calls this) ----

// billingSignedIn shows tenants the Billing tab when there's billing for
// them, and takes an account that must pay first (a new order, or
// suspended for an unpaid invoice) straight to it.
async function billingSignedIn() {
  document.body.classList.remove('billing-off');
  if (!isTenant()) return;
  try {
    const [cfg, acct] = await Promise.all([billingConfig(true), api('GET', '/account')]);
    const a = acct.account;
    const mustPay = a && (a.status === 'pending' || (a.status === 'suspended' && a.suspend_reason === 'billing'));
    // A reseller's customers are billed by the reseller, not here.
    const off = !a || a.parent_id || cfg.unavailable || (cfg.enabled === false && !mustPay);
    document.body.classList.toggle('billing-off', !!off);
    const here = location.hash.replace(/^#\/?/, '');
    if (off && here.startsWith('billing')) { history.replaceState(null, '', '#/sites'); applyRoute(); }
    else if (!off && mustPay && (!here || here === 'sites')) { history.replaceState(null, '', '#/billing'); applyRoute(); }
  } catch (e) {
    document.body.classList.add('billing-off');
  }
}
