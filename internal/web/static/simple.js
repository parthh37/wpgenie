'use strict';
// Burst, protection levels and the visitor check: plain choices for site
// owners who shouldn't need to know what a replica or a rate limit is.
// The detailed controls stay under each section's Advanced (app.js).
// Shares api(), h(), fill(), notify(), showError(), load() and fmtNum()
// with the other scripts; the server enforces everything shown here.

const BURST_NAMES = { off: 'off', auto: 'automatic', on: 'on' };
const LEVEL_NAMES = { basic: 'Basic', recommended: 'Recommended', strict: 'Strict' };
const CHECK_NAMES = { auto: 'automatic', under_attack: 'every visitor', standard: 'suspicious visitors only', off: 'off' };

const plural = (n, one, many = one + 's') => `${fmtNum(n)} ${n === 1 ? one : many}`;

// choose wires a group of radio choices: checks the current value, and
// calls pick with the one chosen.
function choose(box, name, value, pick) {
  box.querySelectorAll('input[type=radio]').forEach((r) => {
    r.name = name;
    r.checked = r.value === value;
    r.addEventListener('change', () => { if (r.checked) pick(r.value); });
  });
}

function setChoicesDisabled(box, disabled) {
  box.querySelectorAll('input').forEach((r) => { r.disabled = disabled; });
}

// ---- Burst ----

function renderBurst(el, site) {
  const f = (c) => $('.' + c, el);
  const modes = f('burst-modes'), onRow = f('burst-on'), hours = f('burst-hours'), start = f('burst-start');
  const mode = site.burst_mode || 'off';
  const active = site.status === 'active';
  const save = async (body, msg) => {
    setChoicesDisabled(modes, true);
    start.disabled = true;
    try {
      await api('PUT', `/sites/${site.id}/burst`, body);
      notify(msg);
    } catch (e) { showError(e); }
    await load();
  };
  choose(modes, 'burst-' + site.id, mode, (v) => {
    onRow.hidden = v !== 'on';
    if (v === 'on') { hours.focus(); return; } // started with the button, once they say how long
    save({ mode: v }, v === 'off' ? `Burst off for ${site.primary_domain}` : `Burst is automatic for ${site.primary_domain}`);
  });
  onRow.hidden = mode !== 'on';
  if (mode === 'on') start.textContent = 'Change';
  start.addEventListener('click', () => {
    const n = Number(hours.value);
    save({ mode: 'on', hours: n }, n ? `Burst on for ${plural(n, 'hour')}, then automatic` : 'Burst on until you turn it off');
  });
  setChoicesDisabled(modes, !active);
  start.disabled = !active;
  const shape = f('shape');
  shape.textContent = `· burst ${BURST_NAMES[mode]}` + (site.page_cache ? ' · page cache' : '') + (site.object_cache ? ' · object cache' : '');
  // The status (and the account's minutes) only when the section is open.
  const details = modes.closest('details');
  details.addEventListener('toggle', () => { if (details.open && active) loadBurst(el, site); });
}

async function loadBurst(el, site) {
  let b;
  try { b = await api('GET', `/sites/${site.id}/burst`); } catch (e) { return; } // the rest of the section still works
  const box = $('.burst-status', el);
  const copies = (n) => plural(n, 'copy', 'copies');
  let now;
  if (b.paused) {
    now = h('p', { class: 'st-warning small' }, 'Burst is paused: there are no burst minutes left. Your site stays at its normal size ' +
      'and carries on by itself when minutes are added or the month starts again.');
  } else if (b.bursting) {
    now = h('p', { class: 'st-ok small' }, `Bursting now: ${copies(b.replicas)} running (normally ${b.base}).`);
  } else if (b.mode === 'off') {
    now = h('p', { class: 'muted small' }, `Normal size: ${copies(b.base)}.`);
  } else if (b.mode === 'on') {
    now = h('p', { class: 'muted small' }, 'Waiting for room on the server for extra copies: it is busy right now. ' +
      'No minutes are used meanwhile.');
  } else {
    now = h('p', { class: 'muted small' }, `Normal size now (${copies(b.base)}); up to ${copies(b.max)} when traffic needs them.`);
  }
  const lines = [now];
  if (b.mode === 'on' && b.until) lines.push(h('p', { class: 'muted small' }, `On until ${fmtTime(b.until)}, then automatic.`));
  lines.push(h('p', { class: 'muted small' }, `This site used ${plural(b.minutes, 'burst minute')} this month.`));
  const a = b.account;
  if (a) {
    if (!a.allowed && isTenant()) {
      setChoicesDisabled($('.burst-modes', el), true);
      lines.push(h('p', { class: 'small st-warning' }, 'Burst isn\'t included in your plan. Ask your provider to add it.'));
    } else if (a.unlimited) {
      lines.push(h('p', { class: 'muted small' }, 'Your plan includes unlimited burst minutes.'));
    } else {
      lines.push(burstMeter(a));
    }
  }
  fill(box, lines);
}

// burstMeter shows what an account has left this month.
function burstMeter(a) {
  const monthly = Math.max(0, a.included - a.used);
  let text = `${fmtNum(monthly)} of ${fmtNum(a.included)} monthly minutes left`;
  if (a.credit) text += ` · ${plural(a.credit, 'extra minute')}`;
  return h('div', { class: 'usage-row' }, h('span', { class: 'small' }, 'Burst minutes'),
    h('meter', { min: 0, max: a.included, value: monthly, low: a.included * 0.2, high: a.included * 0.5, optimum: a.included }),
    h('span', { class: 'small muted' }, text));
}

// ---- Protection level and visitor check ----

let PROTECTION_LEVELS = null; // the server's protection levels, fetched once

function protectionLevels() {
  PROTECTION_LEVELS ??= api('GET', '/security/levels').catch((e) => { PROTECTION_LEVELS = null; throw e; });
  return PROTECTION_LEVELS;
}

// levelOf is the level a site's settings match, or "custom".
function levelOf(site, levels) {
  const l = levels.find((l) => l.waf === site.waf && l.body_waf === site.body_waf && l.reputation === site.reputation &&
    l.rate_rps === site.rate_rps && l.rate_burst === site.rate_burst && l.login_per_min === site.login_per_min &&
    l.challenge_bits === site.challenge_bits);
  return l ? l.id : 'custom';
}

function renderProtection(el, site) {
  const f = (c) => $('.' + c, el);
  const levels = f('sec-levels'), check = f('sec-check'), ai = f('sec-ai'), xmlrpc = f('xmlrpc');
  const active = site.status === 'active';
  // Every change keeps the rest as it is: the server only touches what is
  // sent, and the mode and AI setting are read live (the overview changes
  // them without re-rendering this section).
  const put = async (body, msg) => {
    [levels, check].forEach((b) => setChoicesDisabled(b, true));
    try {
      await api('PUT', `/sites/${site.id}/shield`, { mode: $('.mode', el).value, block_ai_bots: $('.ai', el).checked, ...body });
      notify(msg);
    } catch (e) { showError(e); }
    await load();
  };
  f('sec-off').hidden = site.shield_mode !== 'off';
  choose(check, 'check-' + site.id, site.shield_mode, (v) => put({ mode: v }, `Visitor check on ${site.primary_domain}: ${CHECK_NAMES[v]}`));
  ai.checked = site.block_ai_bots;
  ai.addEventListener('change', () => put({ block_ai_bots: ai.checked }, ai.checked ? 'AI crawlers blocked' : 'AI crawlers allowed'));
  xmlrpc.checked = site.xmlrpc;
  xmlrpc.addEventListener('change', () => put({ xmlrpc: xmlrpc.checked },
    xmlrpc.checked ? 'Jetpack and the WordPress app can connect' : 'XML-RPC blocked'));
  protectionLevels().then((all) => {
    const cur = levelOf(site, all);
    f('sec-custom').hidden = cur !== 'custom';
    choose(levels, 'level-' + site.id, cur, (v) => put({ level: v }, `${LEVEL_NAMES[v]} protection on ${site.primary_domain}`));
    const parts = [cur === 'custom' ? 'custom settings' : LEVEL_NAMES[cur], `visitor check ${CHECK_NAMES[site.shield_mode] || site.shield_mode}`];
    f('sec-summary').textContent = '· ' + parts.join(' · ');
    setChoicesDisabled(levels, !active);
  }).catch(showError);
  setChoicesDisabled(check, !active);
  ai.disabled = xmlrpc.disabled = !active;
}

// ---- Automatic Under attack ----

async function loadAttack(el, site) {
  let a;
  try { a = await api('GET', `/sites/${site.id}/attack`); } catch (e) { return; }
  const was = el.dataset.attack;
  el.dataset.attack = a.active ? '1' : '';
  if (a.active) $('.shield-chip', el).textContent = 'Under attack';
  else if (typeof shieldChip === 'function') shieldChip(el, $('.mode', el).value);
  if (was !== el.dataset.attack && typeof renderAttention === 'function') renderAttention();
  el.querySelectorAll('.attack-banner').forEach((b) => {
    b.hidden = !a.active;
    if (!a.active) { b.replaceChildren(); return; }
    const over = h('button', { class: 'ghost' }, 'It\'s over');
    over.addEventListener('click', async () => {
      over.disabled = true;
      try { await api('DELETE', `/sites/${site.id}/attack`); notify('Visitors are no longer all checked'); } catch (e) { showError(e); }
      loadAttack(el, site);
    });
    fill(b, h('p', {}, h('strong', {}, 'Under attack'), ` since ${fmtTime(a.attack.since)}: ${a.attack.reason}. ` +
      'Every visitor is checked automatically, and your site stays online. This ends by itself once the attack stops.'),
    ME && ME.role !== 'viewer' ? over : null);
  });
}
