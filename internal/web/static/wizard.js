'use strict';

// ---- Domain wizards: connect a domain before it's used ----
//
// A domain whose DNS doesn't point here gets no certificate from Let's
// Encrypt, and a site redirecting to it sends every visitor to a TLS error.
// Creating a site and adding a domain both go through a check first
// (GET /dns-check, GET /sites/{id}/dns-check), which re-runs while DNS
// propagates.

const DNS_STATES = {
  ok: { icon: 'check', cls: 'ok', title: 'Connected' },
  proxied: { icon: 'shield', cls: 'ok', title: 'Behind Cloudflare' },
  partly: { icon: 'alert', cls: 'warn', title: 'Partly connected' },
  unknown: { icon: 'alert', cls: 'warn', title: 'Couldn\'t verify' },
  elsewhere: { icon: 'alert', cls: 'bad', title: 'Points to another server' },
  missing: { icon: 'alert', cls: 'bad', title: 'Not connected yet' },
};
const DNS_RECHECK_MS = 10000;
const dnsReady = (r) => !!r && (r.status === 'ok' || r.status === 'proxied');

// cleanDomain accepts what people paste: a URL, a trailing dot or slash.
const cleanDomain = (v) => v.trim().toLowerCase().replace(/^[a-z]+:\/\//, '').replace(/[/?#].*$/, '').replace(/\.$/, '');

// recordName is what goes in a DNS provider's Name field, assuming the zone
// is the last two labels: "@" for the zone itself.
function recordName(domain) {
  const labels = domain.split('.');
  return labels.length <= 2 ? '@' : labels.slice(0, -2).join('.');
}

function dnsRecordTable(r) {
  return table(['Type', 'Name', 'Value'], r.expected.map((a) => {
    const v6 = a.includes(':');
    return [v6 ? 'AAAA' : 'A', h('td', {}, h('code', {}, recordName(r.domain)), h('div', { class: 'muted small' }, r.domain)),
      h('td', {}, h('code', {}, a), v6 ? h('span', { class: 'muted small' }, ' (optional)') : null)];
  }));
}

// dnsPanel shows a domain's DNS check. run(domain) checks it (again), and
// keeps re-checking every few seconds until it points here; stop() ends
// that. onchange(result) follows every result.
function dnsPanel(check, { many = false } = {}) {
  const el = h('div', { class: 'dns-check', 'aria-live': 'polite' });
  let timer = null, seq = 0, domain = '';
  const panel = { el, result: null, onchange: null };
  panel.stop = () => { clearTimeout(timer); timer = null; seq++; };
  panel.run = async (d, quiet) => {
    panel.stop();
    const my = seq;
    if (d !== domain) panel.result = null;
    domain = d;
    if (!quiet) fill(el, h('p', { class: 'muted small dns-busy' }, `Looking up ${d}…`));
    let r;
    try {
      r = await check(d);
    } catch (e) {
      if (my === seq) fill(el, h('p', { class: 'st-failed small' }, e.message));
      panel.result = null;
      if (panel.onchange) panel.onchange(null);
      return null;
    }
    if (my !== seq) return panel.result; // superseded (or closed)
    panel.result = r;
    render(r);
    if (!dnsReady(r)) timer = setTimeout(() => panel.run(d, true), DNS_RECHECK_MS);
    if (panel.onchange) panel.onchange(r);
    return r;
  };
  const again = h('button', { type: 'button', class: 'ghost' }, 'Check again');
  again.addEventListener('click', () => panel.run(domain));
  function render(r) {
    const st = DNS_STATES[r.status] || DNS_STATES.unknown;
    const parts = [h('div', { class: `dns-status dns-${st.cls}` }, icon(st.icon),
      h('div', {}, h('strong', {}, st.title), h('p', {}, r.message)))];
    if (r.addresses.length) {
      parts.push(h('p', { class: 'muted small' }, `${r.domain} resolves to `, h('code', {}, r.addresses.join(', '))));
    }
    if (!dnsReady(r) && r.expected.length) {
      parts.push(h('p', { class: 'small' }, many
        ? 'At your DNS provider, point it at one of these servers (the site is created on the one it points to):'
        : `At your DNS provider, create ${r.expected.length > 1 ? 'these records' : 'this record'}` +
          (r.addresses.length ? ' (and remove the others):' : ':')));
      parts.push(dnsRecordTable(r));
    }
    if (!dnsReady(r)) {
      parts.push(h('div', { class: 'dns-foot' },
        h('p', { class: 'muted small' }, 'DNS changes usually show up within minutes. Checking again every 10 seconds…'), again));
    }
    fill(el, parts);
  }
  return panel;
}

// openWizard shows a step-by-step dialog and resolves to true once the
// last step's next() succeeded, false if it was cancelled. A step is
// {label, el, enter?(), leave?(), next?() → false to stay, button?() →
// {label, danger}}; wiz.update() refreshes the buttons after a step's state
// changed, wiz.go(i) jumps to a step.
function openWizard({ title, steps, start = 0 }) {
  return new Promise((resolve) => {
    const dlg = h('dialog', { class: 'modal wizard', 'aria-label': title });
    const crumbs = steps.map((s, i) => h('li', {}, h('span', { class: 'wiz-n' }, String(i + 1)), s.label));
    const body = h('div', { class: 'wiz-body' });
    const back = h('button', { type: 'button', class: 'ghost' }, 'Back');
    const cancel = h('button', { type: 'button', class: 'ghost' }, 'Cancel');
    const next = h('button', { type: 'submit' }, 'Next');
    // A form, so Enter in a field moves on.
    const form = h('form', { novalidate: true }, h('h2', {}, title), h('ol', { class: 'wiz-steps' }, crumbs), body,
      h('div', { class: 'actions' }, cancel, back, next));
    dlg.append(form);
    let i = -1, busy = false, done = false;
    const wiz = {
      update() {
        const s = steps[i], b = (s.button && s.button()) || {};
        next.textContent = b.label || (i === steps.length - 1 ? 'Finish' : 'Next');
        next.classList.toggle('danger-solid', !!b.danger);
        next.disabled = busy || !!b.disabled;
        back.hidden = i === 0;
        // A disabled button can't take the focus: give it once it's usable,
        // so Enter works on a step without fields.
        const a = document.activeElement;
        if (!next.disabled && (!a || a === document.body || a === dlg)) next.focus();
      },
      async go(n) {
        if (i >= 0 && steps[i].leave) steps[i].leave();
        i = n;
        crumbs.forEach((c, k) => { c.className = k < i ? 'done' : k === i ? 'current' : ''; });
        body.replaceChildren(steps[i].el);
        wiz.update();
        const first = steps[i].el.querySelector('input:not([type=radio]):not([type=checkbox]), select');
        if (first) first.focus(); else next.focus();
        if (steps[i].enter) await steps[i].enter();
      },
    };
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      if (busy || next.disabled) return;
      busy = true;
      wiz.update();
      let ok;
      try { ok = steps[i].next ? await steps[i].next() : true; } catch (err) { showError(err); ok = false; }
      busy = false;
      if (ok === false) { wiz.update(); return; }
      if (i === steps.length - 1) { done = true; dlg.close(); return; }
      await wiz.go(i + 1);
    });
    back.addEventListener('click', () => wiz.go(i - 1));
    cancel.addEventListener('click', () => dlg.close());
    dlg.addEventListener('close', () => {
      if (i >= 0 && steps[i].leave) steps[i].leave();
      dlg.remove();
      resolve(done);
    });
    for (const s of steps) s.wiz = wiz;
    document.body.append(dlg);
    dlg.showModal();
    wiz.go(start);
  });
}

// dnsStep is the wizard step that checks a domain (domain() says which)
// and won't go on without a confirmation while it doesn't point here.
function dnsStep(check, domain, { many, finish, warn, after } = {}) {
  const panel = dnsPanel(check, { many });
  const step = {
    label: 'Connect DNS',
    el: h('div', {}, panel.el),
    panel,
    enter: () => panel.run(domain()),
    leave: () => panel.stop(),
    button() {
      if (!panel.result) return { label: finish || 'Next', disabled: true };
      return dnsReady(panel.result) ? { label: finish || 'Next' } : { label: (finish || 'Continue') + ' anyway', danger: true };
    },
    async next() {
      if (!dnsReady(panel.result) && !await ask(`${panel.result.message}\n${warn || 'Its HTTPS certificate is issued once the domain points here; ' +
        'until then visitors get a certificate error.'}\nContinue anyway?`,
      { title: `${domain()} isn't connected yet`, ok: 'Continue anyway', danger: true })) return false;
      return after ? after() : true;
    },
  };
  panel.onchange = () => step.wiz && step.wiz.update();
  return step;
}

// ---- New site ----

function openNewSiteWizard() {
  const domain = h('input', { name: 'domain', placeholder: 'example.com', autocomplete: 'off', spellcheck: 'false', required: true });
  const pickNode = isAdmin() && typeof clustered === 'function' && clustered();
  const node = pickNode ? h('select', {}, h('option', { value: '' }, 'Automatic (the server with the most room)'),
    NODES.filter((n) => n.status === 'active' && n.up).map((n) => h('option', { value: n.id }, `${n.name} (${n.id})`))) : null;
  const name = h('input', { placeholder: 'My Blog' });
  const email = h('input', { type: 'email', required: true, value: (ME && ME.email) || '' });
  const user = h('input', { placeholder: 'random if empty', autocomplete: 'off' });
  const err = h('p', { class: 'st-failed small', hidden: true });
  const target = () => (node ? node.value : '');

  const dns = dnsStep((d) => api('GET', `/dns-check?domain=${encodeURIComponent(d)}${target() ? '&node=' + encodeURIComponent(target()) : ''}`),
    () => cleanDomain(domain.value), { many: pickNode });
  // The www twin, for information: it can redirect here once the site exists.
  const twinNote = h('p', { class: 'muted small dns-twin' });
  dns.el.append(twinNote);
  const enterDNS = dns.enter;
  dns.enter = async () => {
    twinNote.textContent = '';
    const d = cleanDomain(domain.value);
    const r = await enterDNS();
    // Automatic placement: the site goes where the domain points.
    if (node && !node.value && r && r.server) node.dataset.auto = r.server;
    if (d.startsWith('www.') || d.split('.').length > 2) return;
    try {
      const t = await api('GET', `/dns-check?domain=www.${encodeURIComponent(d)}${target() ? '&node=' + encodeURIComponent(target()) : ''}`);
      twinNote.textContent = dnsReady(t)
        ? `www.${d} points here too: add it under Domains once the site is live to redirect it.`
        : `Tip: www.${d} isn't set up. To have it redirect here later, point it here too (a CNAME to ${d} works).`;
    } catch { /* only a hint */ }
  };

  return openWizard({
    title: 'Create a WordPress site',
    steps: [
      {
        label: 'Domain',
        el: h('div', {},
          h('label', {}, 'Domain', domain),
          node ? h('label', { class: 'wiz-gap' }, 'Server', node) : null,
          h('p', { class: 'muted small' }, 'The site\'s address. Next, WPGenie checks its DNS points to this server, so the HTTPS certificate can be issued.')),
        next() {
          const d = cleanDomain(domain.value);
          if (!/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(d)) { showError(new Error('Enter a domain like example.com')); return false; }
          domain.value = d;
          if (node) delete node.dataset.auto;
          return true;
        },
      },
      dns,
      {
        label: 'Details',
        el: h('div', { class: 'grid' },
          h('label', {}, 'Site title', name),
          h('label', {}, 'Admin e-mail', email),
          h('label', {}, 'Admin username ', h('span', { class: 'muted' }, '(optional)'), user), err),
        button: () => ({ label: 'Create site' }),
        async next() {
          err.hidden = true;
          if (!email.checkValidity()) { err.textContent = 'Enter the admin\'s e-mail address.'; err.hidden = false; email.focus(); return false; }
          const body = { domain: domain.value, name: name.value.trim(), admin_email: email.value.trim(), admin_user: user.value.trim() };
          if (node) body.node = node.value || node.dataset.auto || '';
          // Provisioning runs as a job (progress in the jobs tray); the
          // credentials come with it when it's done.
          const res = await api('POST', '/sites', body);
          notify(`Creating ${res.site.primary_domain}: progress is in the jobs panel`);
          followJob(res.job_id, async (v) => {
            if (v.secret) showCredentials(res.site, v.secret, res.job_id);
            else if (v.job.status === 'failed') showError(new Error(`Creating ${res.site.primary_domain} failed: ${v.job.error}`));
            await load();
          });
          await load();
          return true;
        },
      },
    ],
  });
}

// ---- Adding a domain to a site ----

// openAddDomainWizard adds a domain to a site, serving it or redirecting to
// the primary domain, once its DNS is checked. preset: {domain, redirect}
// starts at the check.
function openAddDomainWizard(site, preset) {
  const domain = h('input', { placeholder: 'example.org', autocomplete: 'off', spellcheck: 'false', value: (preset && preset.domain) || '' });
  const mode = (value, text, hint, checked) => h('label', { class: 'check wiz-choice' },
    h('input', { type: 'radio', name: 'mode', value, checked }), h('span', {}, h('strong', {}, text), h('span', { class: 'muted small' }, hint)));
  const redirect = !preset || preset.redirect !== false;
  const modes = h('div', { class: 'wiz-choices', role: 'radiogroup', 'aria-label': 'What the domain does' },
    mode('redirect', `Redirect to ${site.primary_domain}`, 'For www, old names and typos: visitors land on the primary domain (301, path kept).', redirect),
    mode('serve', 'Serve the site on it too', 'The site answers on both names. Search engines prefer one: redirecting is usually better.', !redirect));
  const isRedirect = () => $('input[name=mode]:checked', modes).value === 'redirect';
  const dns = dnsStep((d) => api('GET', `/sites/${site.id}/dns-check?domain=${encodeURIComponent(d)}`), () => cleanDomain(domain.value), {
    finish: 'Add domain',
    warn: 'Caddy keeps trying to get its certificate; once DNS points here it works. Until then, visitors to it get a certificate error.',
    async after() {
      const d = cleanDomain(domain.value), redir = isRedirect();
      await api('POST', `/sites/${site.id}/domains`, { domain: d, redirect: redir });
      notify(`${d} ${redir ? 'now redirects to ' + site.primary_domain : 'now serves the site'}`);
      await load();
      return true;
    },
  });
  return openWizard({
    title: `Add a domain to ${site.primary_domain}`,
    start: preset ? 1 : 0,
    steps: [
      {
        label: 'Domain',
        el: h('div', {}, h('label', {}, 'Domain', domain), modes),
        next() {
          const d = cleanDomain(domain.value);
          if (!/^[a-z0-9-]+(\.[a-z0-9-]+)+$/.test(d)) { showError(new Error('Enter a domain like example.org')); return false; }
          domain.value = d;
          return true;
        },
      },
      dns,
    ],
  });
}

// checkBeforePrimary: making a domain primary redirects every visitor to
// it, so it must point here. Resolves to whether to go ahead.
async function checkBeforePrimary(site, d) {
  let r = null;
  try { r = await api('GET', `/sites/${site.id}/dns-check?domain=${encodeURIComponent(d)}`); } catch (e) { showError(e); }
  const change = `Links in the database are rewritten to it and ${site.primary_domain} redirects to it.`;
  if (dnsReady(r)) return ask(`Make ${d} the primary domain? ${change}`);
  return ask(`${r ? r.message : 'Its DNS couldn\'t be checked.'}\nEvery visitor to ${site.primary_domain} would be redirected to ${d} and get a certificate error until it points here. ` +
    `${change}\nMake it primary anyway?`, { title: `${d} isn't connected`, ok: 'Make primary anyway', danger: true });
}
