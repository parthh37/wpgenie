'use strict';

// Servers: the panel's own server and the nodes paired with it (GET
// /nodes); per site, where it lives, moving it and spreading its replicas.
// Everything here hides itself on a single server. Shares api(), h(),
// table(), fill(), showError(), followJob() and fmtTime() with app.js,
// panels.js and environments.js.

Object.assign(JOB_NAMES, { migrate: 'Moving a site to another server', drain: 'Moving every site off a server' });

let NODES = []; // the panel's own server first
const clustered = () => NODES.length > 1;
const nodeName = (id) => (NODES.find((n) => n.id === (id || 'local')) || { name: id }).name;
const nodeInfo = (n) => { try { return typeof n.info === 'string' ? JSON.parse(n.info) : (n.info || {}); } catch { return {}; } };

// loadNodes refreshes the list the site cards and the create form use.
async function loadNodes() {
  try { NODES = await api('GET', '/nodes'); } catch { NODES = []; }
  const label = $('#new-site-node');
  label.hidden = !clustered();
  const sel = $('select', label);
  sel.replaceChildren(h('option', { value: '' }, 'Automatic (the server with the most room)'),
    ...NODES.filter((n) => n.status === 'active' && n.up).map((n) => h('option', { value: n.id }, `${n.name} (${n.id})`)));
}

function nodeState(n) {
  if (!n.up) return h('span', { class: 'st-failed', title: n.last_error || '' }, 'unreachable');
  return h('span', { class: n.status === 'active' ? 'st-ok' : 'st-running' }, n.status);
}

async function loadServers() {
  await loadNodes();
  const admin = isAdmin();
  const panelVersion = nodeInfo(NODES.find((n) => n.local) || {}).version;
  const rows = NODES.map((n) => {
    const i = nodeInfo(n);
    const mem = i.mem_total_mb ? `${fmtMem(i.committed_mb || 0)} of ${fmtMem(i.mem_total_mb)}` : '–';
    const disk = i.disk_total_gb ? `${i.disk_free_gb.toFixed(0)} of ${i.disk_total_gb.toFixed(0)} GB` : '–';
    const actions = h('td', { class: 'actions' });
    if (admin && !n.local) {
      const act = (label, fn, cls = 'ghost') => {
        const b = h('button', { class: cls }, label);
        b.addEventListener('click', async () => {
          b.disabled = true;
          try { await fn(); await loadServers(); } catch (e) { showError(e); b.disabled = false; }
        });
        actions.append(b);
      };
      if (n.status === 'active') {
        act('Drain', async () => {
          if (!await ask(`Move every site off ${n.name}, one after the other? New sites won't be placed on it.`)) return;
          const r = await api('POST', `/nodes/${n.id}/drain`);
          followJob(r.job_id, () => { load(); loadServers(); });
        });
      } else {
        act('Activate', () => api('PUT', `/nodes/${n.id}`, { status: 'active' }));
      }
      if (i.version && panelVersion && i.version !== panelVersion) {
        act('Update', () => api('POST', `/nodes/${n.id}/update`));
      }
      act('Remove', async () => {
        const force = n.sites > 0;
        if (force && !await ask(`${n.sites} site(s) still live on ${n.name}. Removing it leaves them running there, out of the panel's reach. Remove anyway?`)) return;
        if (!force && !await ask(`Remove ${n.name} from the cluster?`)) return;
        await api('DELETE', `/nodes/${n.id}` + (force ? '?force=1' : ''));
      }, 'ghost danger');
    }
    return [h('td', {}, h('strong', {}, n.name), h('div', { class: 'muted small' }, n.local ? 'the panel' : n.id)),
      n.local ? '–' : h('td', { class: 'wrap' }, n.address, n.public_ip ? h('div', { class: 'muted small' }, `DNS: ${n.public_ip}`) : null),
      h('td', {}, nodeState(n)), String(n.sites), mem, disk, i.version || '–',
      n.local || !n.cert_not_after ? '–' : fmtTime(n.cert_not_after), actions];
  });
  $('#servers').replaceChildren(table(['Server', 'Address', 'State', 'Sites', 'Memory promised', 'Disk free', 'Version',
    'Certificate until', ''], rows));
}
loaders.servers = loadServers;

// The add-server form (scripts load after app.js has started: wire it now).
(function wireServers() {
  const f = $('#add-server');
  f.addEventListener('submit', async (e) => {
    e.preventDefault();
    const btn = $('button[type=submit]', f);
    btn.disabled = true;
    try {
      const body = Object.fromEntries(new FormData(f));
      await api('POST', '/nodes', body);
      f.reset();
      await loadServers();
    } catch (err) { showError(err); }
    finally { btn.disabled = false; }
  });
})();

// renderCluster fills a site card's Server section.
function renderCluster(el, site) {
  const box = $('.cluster', el);
  box.hidden = !clustered();
  if (!clustered()) return;
  const here = site.node || 'local';
  $('.cluster-summary', el).textContent = `· ${nodeName(here)}` +
    (site.spread_nodes && site.spread_nodes.length ? ` + ${site.spread_nodes.map(nodeName).join(', ')}` : '');
  const body = $('.cluster-body', el);
  const admin = isAdmin();
  const others = NODES.filter((n) => n.id !== here && n.status === 'active' && n.up);
  const node = NODES.find((n) => n.id === here);
  const parts = [h('p', { class: 'small' }, `Lives on ${nodeName(here)}`,
    node && node.public_ip ? ` — its domains should point at ${node.public_ip}.` : '.')];

  const moveBox = h('div');
  parts.push(moveBox);
  if (admin && site.status === 'active' && !site.parent_id) {
    const target = h('select', {}, others.map((n) => h('option', { value: n.id }, `${n.name} (${n.id})`)));
    const go = h('button', { class: 'ghost' }, 'Move');
    go.disabled = !others.length;
    go.addEventListener('click', async () => {
      if (!await ask(`Move ${site.primary_domain} to ${nodeName(target.value)}? Files and database are copied while the site runs; ` +
        'it shows a maintenance page only during the final copy. Its old server then passes visitors on until DNS points at the new one.')) return;
      go.disabled = true;
      try {
        const r = await api('POST', `/sites/${site.id}/migrate`, { node: target.value });
        followJob(r.job_id, () => load());
      } catch (e) { showError(e); go.disabled = false; }
    });
    parts.push(h('div', { class: 'row' }, h('label', {}, 'Move to ', target), go));

    // Spread: replicas on other servers too (needs uploads offload).
    const checks = others.map((n) => h('label', { class: 'check small' },
      h('input', { type: 'checkbox', value: n.id, checked: (site.spread_nodes || []).includes(n.id) }), ` ${n.name}`));
    const save = h('button', { class: 'ghost' }, 'Save');
    save.addEventListener('click', async () => {
      save.disabled = true;
      try {
        const nodes = checks.map((c) => $('input', c)).filter((i) => i.checked).map((i) => i.value);
        await api('PUT', `/sites/${site.id}/spread`, { nodes });
        await load();
      } catch (e) { showError(e); save.disabled = false; }
    });
    parts.push(h('h3', {}, 'Replicas on other servers'),
      h('p', { class: 'muted small' }, 'Page views are also served by replicas on the servers ticked here (the site\'s ',
        h('em', {}, 'replicas'), ' are shared out); wp-admin, sign-ins and every change still run on its own server. ',
        'Needs uploads offload.'),
      checks.length ? h('div', { class: 'row' }, checks, save) : h('p', { class: 'muted small' }, 'No other server is available.'));
  }
  fill(body, parts);

  box.addEventListener('toggle', async () => {
    if (!box.open) return;
    try {
      const { move } = await api('GET', `/sites/${site.id}/move`);
      if (!move) { fill(moveBox); return; }
      const finish = h('button', { class: 'ghost' }, 'Finish move');
      finish.hidden = !admin;
      finish.addEventListener('click', async () => {
        if (!await ask(`Delete the old copy on ${nodeName(move.from)} now? Do this once the domains point at ${move.point_dns_to || 'the new server'}.`)) return;
        try { await api('POST', `/sites/${site.id}/move/finish`); await load(); } catch (e) { showError(e); }
      });
      fill(moveBox, h('p', { class: 'small' }, `Moved from ${nodeName(move.from)} ${fmtTime(move.at)}. `,
        move.point_dns_to ? `Point its domains at ${move.point_dns_to}; ` : '',
        'until then the old server passes visitors on (for a week at most).'), finish);
    } catch (e) { showError(e); }
  }, { once: true });
}
