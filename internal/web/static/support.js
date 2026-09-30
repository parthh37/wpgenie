'use strict';
// Support tickets: the list (#/support), a ticket as a conversation with
// its reply box (#/support/<id>), the new-ticket form (#/support/new) and
// staff's departments, canned replies and settings (#/support/manage).
// Shares $, h(), api(), icon(), fill(), ask(), askText(), notify(),
// showError(), fmtBytes(), fmtTime(), ME, isTenant(), isAdmin() and loaders
// with the rest. Everything here is also enforced by the server: hiding a
// control is only a convenience, and customers never receive internal
// notes to hide.

loaders.support = loadSupport;

const SUP = {
  seq: 0,          // the view being shown; older loads drop their result
  summary: null,   // GET /support/summary: badge count, attachment limits
  filter: null,    // the list's chip
  q: '', dept: '', prio: '', assigned: '',
  poll: null,      // refreshes the open ticket
};

// Staff and resellers see the ticket's state; customers what it means for them.
const PROVIDER_STATUS = { open: 'New', customer_reply: 'Customer replied', in_progress: 'In progress', on_hold: 'On hold', answered: 'Answered', closed: 'Closed' };
const CUSTOMER_STATUS = { open: 'Waiting for support', customer_reply: 'Waiting for support', in_progress: 'In progress', on_hold: 'On hold', answered: 'Support replied', closed: 'Closed' };
const PRIORITIES = [['low', 'Low', 'A question, no rush'], ['medium', 'Normal', 'Something isn\'t working right'],
  ['high', 'High', 'A site is affected'], ['urgent', 'Urgent', 'A site is down']];
const PRIO_LABEL = Object.fromEntries(PRIORITIES.map(([k, l]) => [k, l]));

const statusLabel = (t) => (t.you === 'customer' ? CUSTOMER_STATUS : PROVIDER_STATUS)[t.status] || t.status;
// A customer waiting for support is no alarm to the customer.
const statusPill = (t) => h('span', { class: `pill tk-st tks-${t.you === 'customer' && t.status === 'customer_reply' ? 'open' : t.status}` }, statusLabel(t));
const prioPill = (p) => h('span', { class: `pill tk-prio tkp-${p}` }, PRIO_LABEL[p] || p);
const canAct = () => ME && ME.role !== 'viewer';
const isOperator = () => ME && (ME.role === 'operator' || ME.role === 'admin');

// ---- Small helpers ----

const RTF = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });

// ago: "just now", "5 minutes ago", "yesterday"…
function ago(t) {
  const s = (new Date(t).getTime() - Date.now()) / 1000, a = Math.abs(s);
  if (a < 60) return 'just now';
  for (const [limit, unit, div] of [[3600, 'minute', 60], [86400, 'hour', 3600], [604800, 'day', 86400],
    [2629800, 'week', 604800], [31557600, 'month', 2629800], [Infinity, 'year', 31557600]]) {
    if (a < limit) return RTF.format(Math.round(s / div), unit);
  }
  return fmtTime(t);
}
const timeEl = (t) => h('time', { datetime: t, title: fmtTime(t) }, ago(t));

function duration(secs) {
  if (secs < 0) return '–';
  if (secs < 60) return 'under a minute';
  if (secs < 3600) return `${Math.max(1, Math.round(secs / 60))} min`;
  if (secs < 86400) return `${(secs / 3600).toFixed(secs < 36000 ? 1 : 0)} h`;
  return `${(secs / 86400).toFixed(1)} days`;
}

// linkify turns the URLs of a message into links; the rest stays text.
function linkify(text) {
  const out = [];
  const re = /https?:\/\/[^\s<>"']+[^\s<>"'.,;:!?)]/g;
  let last = 0, m;
  while ((m = re.exec(text))) {
    out.push(text.slice(last, m.index), h('a', { href: m[0], target: '_blank', rel: 'noopener noreferrer nofollow' }, m[0]));
    last = m.index + m[0].length;
  }
  out.push(text.slice(last));
  return out;
}

const loading = (what) => h('p', { class: 'muted tk-loading', role: 'status' }, `Loading ${what}…`);

function supportSub() {
  const parts = location.hash.split('?')[0].replace(/^#\/?/, '').split('/');
  return parts[0] === 'support' ? decodeURIComponent(parts[1] || '') : '';
}

const go = (sub) => { location.hash = '#/support' + (sub ? '/' + sub : ''); };

// sendForm posts JSON, or multipart (data + files) when there are files,
// with upload progress (fetch() can't report it).
function sendForm(method, path, data, files, onProgress) {
  if (!files.length) return api(method, path, data);
  return new Promise((resolve, reject) => {
    const fd = new FormData();
    fd.append('data', JSON.stringify(data));
    for (const f of files) fd.append('files', f, f.name);
    const x = new XMLHttpRequest();
    x.open(method, '/api/v1' + path);
    x.setRequestHeader('X-Requested-With', 'wpgenie');
    if (onProgress) x.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
    x.onload = () => {
      let body = {};
      try { body = JSON.parse(x.responseText); } catch { /* not JSON */ }
      if (x.status >= 200 && x.status < 300) return resolve(body);
      if (x.status === 401) showSignIn();
      const err = new Error(body.error || x.statusText || `HTTP ${x.status}`);
      err.status = x.status;
      reject(err);
    };
    x.onerror = () => reject(new Error('The connection dropped while sending: nothing was saved, try again'));
    x.send(fd);
  });
}

// ---- The badge in the navigation ----

async function refreshSupportBadge() {
  if (!ME) return null;
  const btn = $('.tab[data-tab="support"]'), badge = $('#support-badge');
  let s;
  try { s = await api('GET', '/support/summary'); } catch (e) {
    if (e.status === 400) btn.hidden = true; // no help desk on this server
    return null;
  }
  SUP.summary = s;
  // Customers of a panel without support (and no old tickets) don't need the page.
  btn.hidden = isTenant() && !s.enabled && s.total === 0;
  badge.hidden = !s.awaiting;
  badge.replaceChildren(String(s.awaiting), h('span', { class: 'sr-only' }, ` ${s.awaiting === 1 ? 'ticket needs' : 'tickets need'} your reply`));
  return s;
}

// Signed in: count at once, then every minute.
{
  let authed = false;
  new MutationObserver(() => {
    const now = document.body.classList.contains('authed');
    if (now && !authed) { SUP.summary = null; refreshSupportBadge(); }
    authed = now;
  }).observe(document.body, { attributes: true, attributeFilter: ['class'] });
}
setInterval(() => { if (ME && !document.hidden) refreshSupportBadge(); }, 60000);

// ---- Loading a view ----

async function loadSupport() {
  // After the navigation's click has set its own address (ux.js).
  await new Promise((r) => setTimeout(r));
  clearInterval(SUP.poll);
  const seq = ++SUP.seq;
  const box = $('#support');
  const sub = supportSub();
  if (!SUP.summary) await refreshSupportBadge();
  if (sub === 'new') return showNewTicket(box, seq);
  if (sub === 'manage' && !isTenant()) return showManage(box, seq);
  if (/^\d+$/.test(sub)) return showTicket(box, Number(sub), seq);
  return showTicketList(box, seq);
}

const stale = (seq) => seq !== SUP.seq;

// ---- The list ----

const CHIPS = [
  ['awaiting', 'Needs your reply', { awaiting: '1' }],
  ['active', 'Open', { status: 'open,customer_reply,in_progress,on_hold,answered' }],
  ['on_hold', 'On hold', { status: 'on_hold' }, true],
  ['closed', 'Closed', { status: 'closed' }],
  ['all', 'All', {}],
];
const PAGE = 50;

async function showTicketList(box, seq) {
  const staff = !isTenant(), provider = staff || ME.role === 'reseller';
  const sum = await refreshSupportBadge() || SUP.summary || { enabled: true, awaiting: 0, total: 0 };
  const [depts, ov, agents] = await Promise.all([
    staff ? api('GET', '/support/departments') : null,
    staff ? api('GET', '/support/overview').catch(() => null) : null,
    staff ? api('GET', '/support/agents').catch(() => []) : null,
  ]);
  if (stale(seq)) return;
  if (SUP.filter == null) SUP.filter = sum.awaiting ? 'awaiting' : 'active';

  const newBtn = h('a', { class: 'button', href: '#/support/new' }, icon('plus'), staff ? 'Open a ticket for a customer' : 'New ticket');
  newBtn.hidden = !canAct() || (!staff && !sum.enabled);
  const manage = staff && isOperator() ? h('a', { class: 'button ghost', href: '#/support/manage' }, icon('sliders'), 'Manage') : null;
  const head = h('div', { class: 'bar' },
    h('div', {}, h('h1', {}, 'Support'),
      h('p', { class: 'muted small bar-sub' }, staff ? 'Your customers\' questions and problems, in one place.'
        : provider ? 'Your own tickets, and your customers\' tickets to answer.'
          : 'Ask us anything about your sites, e-mail or account. We\'ll e-mail you when we reply.')),
    h('div', { class: 'bar-actions' }, manage, newBtn));

  const tiles = ov ? h('div', { class: 'fleet tk-kpis' },
    tile('Needs a reply', ov.awaiting, ov.awaiting ? 'waiting for your team' : 'all caught up', ov.awaiting ? 'warn' : 'ok'),
    tile('Open', ['open', 'customer_reply', 'in_progress', 'on_hold', 'answered'].reduce((n, k) => n + (ov.by_status[k] || 0), 0),
      `${ov.by_status.on_hold || 0} on hold`),
    tile('Unassigned', ov.unassigned, 'open tickets nobody owns'),
    tile('First reply', duration(ov.avg_first_response_seconds), `on average · ${ov.opened_30d} opened in 30 days`)) : null;

  const chips = h('div', { class: 'tk-chips', role: 'group', 'aria-label': 'Show tickets' }, CHIPS
    .filter(([, , , providerOnly]) => !providerOnly || provider)
    .map(([key, label]) => {
      const b = h('button', { type: 'button', class: 'tk-chip', 'aria-pressed': String(SUP.filter === key) }, label,
        key === 'awaiting' && sum.awaiting ? h('span', { class: 'nav-count' }, String(sum.awaiting)) : null);
      b.addEventListener('click', () => { SUP.filter = key; renderList(); chips.querySelectorAll('.tk-chip').forEach((c) => c.setAttribute('aria-pressed', String(c === b))); });
      return b;
    }));
  const search = h('input', { type: 'search', placeholder: 'Search tickets',
    'aria-label': 'Search tickets', value: SUP.q, autocomplete: 'off' });
  let timer;
  search.addEventListener('input', () => { clearTimeout(timer); timer = setTimeout(() => { SUP.q = search.value.trim(); renderList(); }, 300); });
  const selects = staff ? [
    select('Department', SUP.dept, [['', 'All departments'], ...depts.map((d) => [String(d.id), d.name])], (v) => { SUP.dept = v; renderList(); }),
    select('Priority', SUP.prio, [['', 'Any priority'], ...PRIORITIES.map(([k, l]) => [k, l])], (v) => { SUP.prio = v; renderList(); }),
    select('Assigned to', SUP.assigned, [['', 'Anyone'], ['me', 'Me'], ['none', 'Nobody'], ...agents.map((a) => [String(a.id), a.username])],
      (v) => { SUP.assigned = v; renderList(); }),
  ] : [];
  const tools = h('div', { class: 'tk-tools' }, chips,
    h('div', { class: 'tk-filters' }, h('label', { class: 'filter' }, icon('search'), search), ...selects));
  const list = h('div', { class: 'tk-list-box', 'aria-live': 'polite', 'aria-busy': 'true' });
  fill(box, head, tiles, !staff && !sum.enabled ? h('div', { class: 'card tk-banner' }, icon('alert'),
    h('p', {}, 'New tickets are turned off at the moment. You can still read and reply to the ones you have.')) : null,
  h('div', { class: 'card tk-card' }, tools, list));

  async function renderList(before) {
    const mySeq = SUP.seq;
    const q = new URLSearchParams({ ...CHIPS.find(([k]) => k === SUP.filter)[2], limit: String(PAGE) });
    if (SUP.q) q.set('q', SUP.q);
    if (SUP.dept) q.set('department', SUP.dept);
    if (SUP.prio) q.set('priority', SUP.prio);
    if (SUP.assigned) q.set('assigned', SUP.assigned);
    if (before) q.set('before', String(before));
    list.setAttribute('aria-busy', 'true');
    let items;
    try { items = await api('GET', '/tickets?' + q); } catch (e) { showError(e); return; } finally { list.setAttribute('aria-busy', 'false'); }
    if (mySeq !== SUP.seq) return;
    const ul = before ? list.querySelector('.tk-list') : h('ul', { class: 'tk-list' });
    list.querySelector('.tk-more')?.remove();
    ul.append(...items.map((t) => ticketRow(t, provider)));
    if (!before) fill(list, items.length ? ul : emptyList(staff, provider, sum));
    if (items.length === PAGE) {
      const more = h('button', { type: 'button', class: 'ghost tk-more' }, 'Show older tickets');
      more.addEventListener('click', () => { more.disabled = true; renderList(items[items.length - 1].id); });
      list.append(more);
    }
  }
  await renderList();
}

function tile(label, value, sub, tone) {
  return h('div', { class: 'tile' + (tone ? ' tk-tile-' + tone : '') }, h('span', { class: 'tile-k' }, label),
    h('span', { class: 'tile-v' }, typeof value === 'number' ? fmtNum(value) : value), h('span', { class: 'tile-s' }, sub));
}

function select(label, value, options, onChange) {
  const s = h('select', { 'aria-label': label }, options.map(([v, l]) => h('option', { value: v, selected: v === value }, l)));
  s.addEventListener('change', () => onChange(s.value));
  return s;
}

function ticketRow(t, provider) {
  const meta = [t.mask, provider && t.you !== 'customer' ? t.account_name : null, t.department,
    t.assigned_to ? `→ ${t.assigned_to}` : null].filter(Boolean).join(' · ');
  return h('li', {}, h('a', { class: 'tk-row' + (t.awaiting ? ' awaiting' : '') + (t.status === 'closed' ? ' closed' : ''), href: `#/support/${t.id}` },
    h('div', { class: 'tk-row-main' },
      h('div', { class: 'tk-row-top' }, h('span', { class: 'tk-subject' }, t.subject),
        t.awaiting ? h('span', { class: 'tk-await' }, 'Needs your reply') : null,
        t.you === 'handler' && t.escalated ? h('span', { class: 'badge' }, 'Escalated') : null,
        t.you === 'staff' && t.handler === 'reseller' ? h('span', { class: 'badge', title: 'The customer\'s reseller answers it unless they escalate it' }, 'Reseller') : null),
      t.preview ? h('p', { class: 'tk-preview' }, t.preview) : null,
      h('p', { class: 'tk-meta' }, meta)),
    h('div', { class: 'tk-row-side' }, h('div', { class: 'tk-pills' }, statusPill(t), t.priority !== 'medium' ? prioPill(t.priority) : null),
      h('span', { class: 'small muted' }, timeEl(t.updated_at)))));
}

function emptyList(staff, provider, sum) {
  const filtered = SUP.q || SUP.dept || SUP.prio || SUP.assigned || !['all', 'active'].includes(SUP.filter);
  if (filtered) {
    const all = h('button', { type: 'button', class: 'ghost' }, 'Show all tickets');
    all.addEventListener('click', () => { Object.assign(SUP, { filter: 'all', q: '', dept: '', prio: '', assigned: '' }); loadSupport(); });
    return h('div', { class: 'empty tk-empty' }, h('span', { class: 'empty-icon' }, icon(SUP.filter === 'awaiting' ? 'check' : 'search')),
      h('h2', {}, SUP.filter === 'awaiting' && !SUP.q ? 'All caught up' : 'No tickets match'),
      h('p', { class: 'muted' }, SUP.filter === 'awaiting' && !SUP.q ? 'Nothing is waiting for your reply right now.' : 'Try another filter or search.'), all);
  }
  if (staff || provider) {
    return h('div', { class: 'empty tk-empty' }, h('span', { class: 'empty-icon' }, icon('inbox')), h('h2', {}, 'No tickets yet'),
      h('p', { class: 'muted' }, staff ? 'When customers ask for help, their tickets show up here, and whoever you set up in Manage gets an e-mail.'
        : 'When your customers ask for help, their tickets show up here for you to answer.'),
      staff ? null : h('a', { class: 'button', href: '#/support/new' }, icon('plus'), 'Open a ticket of your own'));
  }
  return h('div', { class: 'empty tk-empty' }, h('span', { class: 'empty-icon' }, icon('life-buoy')), h('h2', {}, 'Need a hand?'),
    h('p', { class: 'muted' }, sum.enabled ? 'Open a ticket and tell us what\'s going on. You\'ll get an e-mail when we reply, and the whole conversation stays here.'
      : 'You have no tickets.'),
    sum.enabled ? h('a', { class: 'button', href: '#/support/new' }, icon('plus'), 'Open your first ticket') : null);
}

// ---- One ticket ----

async function showTicket(box, id, seq) {
  box.replaceChildren(loading('the ticket'));
  let th;
  try { th = await api('GET', `/tickets/${id}`); } catch (e) {
    if (stale(seq)) return;
    if (e.status !== 404) throw e;
    fill(box, h('div', { class: 'empty tk-empty' }, h('span', { class: 'empty-icon' }, icon('inbox')), h('h2', {}, 'Ticket not found'),
      h('p', { class: 'muted' }, 'It may have been opened by another account, or the link is wrong.'),
      h('a', { class: 'button ghost', href: '#/support' }, icon('back'), 'All tickets')));
    return;
  }
  const t = th.ticket, provider = t.you !== 'customer', staff = t.you === 'staff';
  const [depts, agents, canned] = await Promise.all([
    provider ? api('GET', '/support/departments').catch(() => []) : [],
    staff ? api('GET', '/support/agents').catch(() => []) : [],
    staff && isOperator() ? api('GET', '/support/canned').catch(() => []) : [],
  ]);
  if (stale(seq)) return;
  const view = { th, depts, agents, canned };

  const title = h('h1', { tabindex: '-1' }, t.subject);
  const header = h('div', { class: 'tk-head' });
  const convo = h('ol', { class: 'tk-convo', role: 'log', 'aria-label': 'Conversation', 'aria-live': 'polite' });
  const side = h('aside', { class: 'tk-side', 'aria-label': 'Ticket details' });
  const update = (next) => {
    view.th = next;
    renderHeader(header, next);
    renderConvo(convo, next);
    renderSide(side, view, update);
    if (next.warning) showError(new Error(next.warning));
  };
  const reply = canAct() ? composer(view, update) : h('p', { class: 'muted small' }, 'Read-only: your role can look, not reply.');
  fill(box,
    h('a', { class: 'button ghost tk-back', href: '#/support' }, icon('back'), 'All tickets'),
    h('div', { class: 'tk-title' }, title, header),
    h('div', { class: 'tk-layout' }, h('div', { class: 'tk-main' }, convo, reply), side));
  update(th);
  title.focus({ preventScroll: true });
  refreshSupportBadge();

  // New messages show up while the ticket is open.
  SUP.poll = setInterval(async () => {
    if (stale(seq) || $('#support').closest('.panel').hidden) { clearInterval(SUP.poll); return; }
    if (document.hidden) return;
    try {
      const next = await api('GET', `/tickets/${id}`);
      const cur = view.th;
      if (!stale(seq) && (next.messages.length !== cur.messages.length || next.ticket.status !== cur.ticket.status ||
        next.ticket.updated_at !== cur.ticket.updated_at)) update(next);
    } catch (e) { /* the next try */ }
  }, 30000);
}

function renderHeader(el, th) {
  const t = th.ticket;
  fill(el, h('span', { class: 'chip' }, t.mask), statusPill(t), prioPill(t.priority),
    h('span', { class: 'small muted' }, 'Opened ', timeEl(t.created_at), t.opened_by ? ` by ${t.opened_by}` : ''));
}

// renderConvo shows the messages; new ones are appended, so a screen
// reader announces only them.
function renderConvo(el, th) {
  const t = th.ticket, shown = [...el.children].map((li) => li.dataset.id);
  const same = shown.length <= th.messages.length && shown.every((id, i) => id === String(th.messages[i].id));
  const add = th.messages.slice(same ? shown.length : 0).map((m) => {
    const li = messageEl(m, t);
    li.dataset.id = String(m.id);
    return li;
  });
  if (same) el.append(...add); else fill(el, add);
}

function messageEl(m, t) {
  if (m.side === 'system') {
    return h('li', { class: 'tk-sys' + (m.internal ? ' internal' : '') }, icon(m.internal ? 'lock' : 'check'),
      h('span', {}, m.body.split('\n\n')[0]), m.body.includes('\n\n') ? h('q', {}, m.body.split('\n\n').slice(1).join(' ')) : null,
      h('span', { class: 'muted' }, timeEl(m.at)));
  }
  const mine = t.you === 'customer' ? m.side === 'customer' : m.side !== 'customer';
  const role = m.side === 'customer' ? (t.you === 'customer' ? '' : t.account_name)
    : t.you === 'customer' ? 'Support' : m.side === 'handler' ? 'Reseller' : 'Staff';
  const noteFor = t.you === 'handler' ? 'Only you and your provider\'s staff see this' : 'Customers never see this';
  return h('li', { class: `tk-msg ${mine ? 'mine' : 'theirs'}` + (m.internal ? ' internal' : '') + (m.staff ? ' from-staff' : '') },
    h('span', { class: 'avatar', 'aria-hidden': 'true' }, (m.author || '?').slice(0, 1).toUpperCase()),
    h('div', { class: 'tk-bubble' },
      h('div', { class: 'tk-msg-head' }, h('strong', {}, m.author || 'Someone'), role ? h('span', { class: 'muted' }, role) : null,
        m.internal ? h('span', { class: 'tk-note-tag', title: noteFor }, icon('lock'), 'Internal note') : null,
        h('span', { class: 'muted tk-when' }, timeEl(m.at))),
      m.body ? h('div', { class: 'tk-body' }, linkify(m.body)) : null,
      attachmentsEl(t, m.attachments)));
}

function attachmentsEl(t, files) {
  if (!files || !files.length) return null;
  return h('ul', { class: 'tk-files', 'aria-label': 'Attachments' }, files.map((f) => {
    const url = `/api/v1/tickets/${t.id}/attachments/${f.id}`;
    if (f.image) {
      return h('li', {}, h('a', { class: 'tk-thumb', href: url + '?inline=1', target: '_blank', rel: 'noopener', title: `${f.name} · ${fmtBytes(f.size)}` },
        h('img', { src: url + '?inline=1', alt: f.name, loading: 'lazy' })));
    }
    return h('li', {}, h('a', { class: 'tk-file', href: url, download: '' }, icon('paperclip'), h('span', { class: 'tk-file-name' }, f.name),
      h('span', { class: 'muted small' }, fmtBytes(f.size))));
  }));
}

// renderSide: the details, and the controls of whoever may change them.
function renderSide(el, view, update) {
  const { th, depts, agents } = view;
  const t = th.ticket, provider = t.you !== 'customer', act = canAct();
  const change = (body) => async () => {
    try { update(await api('PUT', `/tickets/${t.id}`, body)); refreshSupportBadge(); } catch (e) { showError(e); update(view.th); }
  };
  const field = (label, control) => [h('dt', {}, label), h('dd', {}, control)];
  const pick = (label, value, options, key, cast = (v) => v) => {
    if (!provider || !act) return options.find(([v]) => String(v) === String(value))?.[1] || '–';
    const s = h('select', { 'aria-label': label }, options.map(([v, l]) => h('option', { value: String(v), selected: String(v) === String(value) }, l)));
    s.addEventListener('change', () => change({ [key]: cast(s.value) })());
    return s;
  };
  const rows = [
    ...field('Status', provider && act
      ? pick('Status', t.status, Object.entries(PROVIDER_STATUS), 'status')
      : statusPill(t)),
    ...field('Priority', provider && act ? pick('Priority', t.priority, PRIORITIES, 'priority') : prioPill(t.priority)),
    ...field('Department', provider && act
      ? pick('Department', t.department_id, (depts.some((d) => d.id === t.department_id) ? depts : [...depts, { id: t.department_id, name: t.department }])
        .map((d) => [d.id, d.name + (d.hidden ? ' (hidden)' : '')]), 'department_id', Number)
      : t.department),
  ];
  if (t.you === 'staff') {
    rows.push(...field('Assigned to', act ? pick('Assigned to', t.assigned_user_id || 0,
      [[0, 'Nobody'], ...agents.map((a) => [a.id, a.username + (a.id === ME.id ? ' (you)' : '')])], 'assigned_user_id', Number)
      : t.assigned_to || 'Nobody'));
  }
  if (provider) rows.push(...field('Customer', `${t.account_name || 'Account'} (#${t.account_id})`));
  if (t.site_id) {
    rows.push(...field('Site', h('a', { href: `#/sites/${encodeURIComponent(t.site_id)}` }, t.site_domain || SITES.get(t.site_id)?.primary_domain || t.site_id)));
  }
  rows.push(...field('Last reply', timeEl(t.last_reply_at)));
  if (provider && t.first_response_at) rows.push(...field('First reply', duration((new Date(t.first_response_at) - new Date(t.created_at)) / 1000) + ' after opening'));
  if (t.closed_at) rows.push(...field('Closed', timeEl(t.closed_at)));

  let handling = null;
  if (t.you === 'handler') {
    handling = t.handler === 'reseller' ? 'Your customer\'s ticket: you answer it. Escalate it if your provider needs to step in.'
      : t.escalated ? 'You escalated this ticket: your provider\'s staff handle it now. You can still follow it and reply.'
        : 'Your provider\'s staff opened this ticket and handle it. You can follow it and reply.';
  } else if (t.you === 'staff' && t.handler === 'reseller') {
    handling = `A reseller's customer: the reseller (account #${t.handler_account_id}) answers it unless they escalate it.`;
  } else if (t.you === 'staff' && t.escalated) {
    handling = 'Escalated to you by the customer\'s reseller.';
  }

  const actions = [];
  if (act && t.you === 'customer') {
    const closed = t.status === 'closed';
    const b = h('button', { type: 'button', class: 'ghost' }, icon(closed ? 'back' : 'check'), closed ? 'Reopen ticket' : 'Close ticket');
    b.addEventListener('click', async () => {
      if (!closed && !await ask('Close this ticket? You can reopen it any time by replying.', { title: 'Close this ticket?', ok: 'Close ticket', danger: false })) return;
      await change({ status: closed ? 'open' : 'closed' })();
      notify(closed ? 'Ticket reopened' : 'Ticket closed. Glad we could help!');
    });
    actions.push(b);
  }
  if (act && th.can_escalate) {
    const b = h('button', { type: 'button', class: 'ghost' }, icon('arrow-up'), 'Escalate to provider');
    b.addEventListener('click', async () => {
      const reason = await askText('Your provider\'s staff take over and get an e-mail. You stay on the ticket and can still reply. ' +
        'Your customer isn\'t told.', { title: 'Escalate this ticket?', label: 'What do they need to know? (optional)', ok: 'Escalate' });
      if (reason === null) return;
      try { update(await api('POST', `/tickets/${t.id}/escalate`, { reason })); notify('Escalated: your provider has been notified'); } catch (e) { showError(e); }
    });
    actions.push(b);
  }
  fill(el, h('div', { class: 'card' }, h('h2', {}, 'Details'), h('dl', { class: 'kv tk-kv' }, rows),
    handling ? h('p', { class: 'small muted tk-handling' }, handling) : null,
    actions.length ? h('div', { class: 'tk-side-actions' }, actions) : null));
}

// ---- The reply box ----

// fileChecker checks picked files against the limits before uploading
// (the server checks again).
function fileChecker(limits) {
  const exts = new Set((limits.extensions || []).map((e) => e.toLowerCase()));
  return (f, count) => {
    const ext = f.name.includes('.') ? f.name.split('.').pop().toLowerCase() : '';
    if (!limits.max_files) return 'Attachments are turned off.';
    if (count >= limits.max_files) return `At most ${limits.max_files} files per message.`;
    if (!exts.has(ext)) return `${f.name}: this type of file can't be attached (allowed: ${[...exts].join(', ')}).`;
    if (f.size > limits.max_file_mb * 1048576) return `${f.name} is larger than ${limits.max_file_mb} MB.`;
    if (!f.size) return `${f.name} is empty.`;
    return '';
  };
}

// attachBox: the pending files, a button to add more, and drag & drop on
// target. files() returns them; reset() empties it.
function attachBox(target, limits) {
  let files = [];
  const check = fileChecker(limits);
  const list = h('ul', { class: 'tk-pending', 'aria-label': 'Files to attach' });
  const picker = h('input', { type: 'file', multiple: true, hidden: true, accept: (limits.extensions || []).map((e) => '.' + e).join(',') });
  const add = (picked) => {
    for (const f of picked) {
      const problem = check(f, files.length);
      if (problem) { showError(new Error(problem)); continue; }
      files.push(f);
    }
    render();
  };
  const render = () => fill(list, files.map((f, i) => {
    const rm = h('button', { type: 'button', class: 'icon-btn', 'aria-label': `Remove ${f.name}` }, icon('x'));
    rm.addEventListener('click', () => { files.splice(i, 1); render(); picker.focus(); });
    return h('li', {}, icon(/^image\//.test(f.type) ? 'image' : 'paperclip'), h('span', { class: 'tk-file-name' }, f.name),
      h('span', { class: 'muted small' }, fmtBytes(f.size)), rm);
  }));
  picker.addEventListener('change', () => { add([...picker.files]); picker.value = ''; });
  const button = h('button', { type: 'button', class: 'ghost' }, icon('paperclip'), 'Attach files');
  button.addEventListener('click', () => picker.click());
  button.hidden = !limits.max_files;
  const touch = matchMedia('(pointer: coarse)').matches;
  const hint = h('span', { class: 'small muted tk-drop-hint' }, limits.max_files
    ? `${touch ? '' : 'or drop them here · '}up to ${limits.max_files} × ${limits.max_file_mb} MB` : '');
  if (limits.max_files) {
    target.addEventListener('dragover', (e) => { if ([...e.dataTransfer.types].includes('Files')) { e.preventDefault(); target.classList.add('dragging'); } });
    target.addEventListener('dragleave', (e) => { if (!target.contains(e.relatedTarget)) target.classList.remove('dragging'); });
    target.addEventListener('drop', (e) => {
      if (![...e.dataTransfer.types].includes('Files')) return;
      e.preventDefault();
      target.classList.remove('dragging');
      add([...e.dataTransfer.files]);
    });
    // Pasting a screenshot attaches it.
    target.addEventListener('paste', (e) => {
      const pasted = [...(e.clipboardData?.files || [])];
      if (!pasted.length) return;
      e.preventDefault();
      add(pasted.map((f, i) => (f.name && f.name !== 'image.png' ? f : new File([f], `screenshot-${Date.now()}${i ? '-' + i : ''}.png`, { type: f.type }))));
    });
  }
  return { el: h('div', { class: 'tk-attach' }, h('div', { class: 'tk-attach-bar' }, button, hint), picker, list),
    files: () => files, reset: () => { files = []; render(); } };
}

function composer(view, update) {
  const t0 = view.th.ticket, id = t0.id, provider = t0.you !== 'customer';
  const limits = (SUP.summary && SUP.summary.limits) || { max_files: 0, max_file_mb: 0, extensions: [] };
  const draftKey = `wpgenie_ticket_draft_${id}`;
  const problem = h('p', { class: 'tk-problem small', role: 'alert', hidden: true });
  const text = h('textarea', { id: 'tk-reply-text', rows: 5, maxlength: 20000 });
  try { text.value = sessionStorage.getItem(draftKey) || ''; } catch { /* no storage */ }
  text.addEventListener('input', () => {
    problem.hidden = true;
    try { sessionStorage.setItem(draftKey, text.value); } catch { /* no storage */ }
  });
  const label = h('label', { for: 'tk-reply-text', class: 'tk-reply-label' });
  const note = provider ? h('input', { type: 'checkbox' }) : null;
  const send = h('button', { type: 'submit' }, icon('send'));
  const sendClose = provider ? h('button', { type: 'button', class: 'ghost' }, icon('check'), 'Reply & close') : null;
  const progress = h('progress', { max: 1, value: 0, hidden: true, 'aria-label': 'Uploading' });
  const form = h('form', { class: 'tk-reply card', 'aria-label': 'Reply' });
  const attach = attachBox(form, limits);

  const canned = view.canned && view.canned.length ? h('select', { 'aria-label': 'Insert a canned reply' },
    h('option', { value: '' }, 'Insert a canned reply…'), view.canned.map((c) => h('option', { value: String(c.id) }, c.title))) : null;
  if (canned) {
    canned.addEventListener('change', () => {
      const c = view.canned.find((x) => String(x.id) === canned.value);
      canned.value = '';
      if (!c) return;
      const at = text.selectionStart ?? text.value.length;
      const before = text.value.slice(0, at), after = text.value.slice(text.selectionEnd ?? at);
      const sep = before && !before.endsWith('\n') ? '\n\n' : '';
      text.value = before + sep + c.body + after;
      text.focus();
      text.selectionStart = text.selectionEnd = (before + sep + c.body).length;
      text.dispatchEvent(new Event('input'));
    });
  }

  const sync = () => {
    const t = view.th.ticket, internal = note && note.checked;
    form.classList.toggle('internal', !!internal);
    label.textContent = internal ? 'Internal note' : provider ? 'Reply to the customer' : 'Your reply';
    text.placeholder = internal ? (t.you === 'handler' ? 'Only you and your provider\'s staff will see this.' : 'Only staff will see this: customers never do.')
      : t.status === 'closed' && !provider ? 'This ticket is closed. Write here to reopen it.'
        : provider ? 'Write your reply… (Ctrl+Enter sends)' : 'Add more details or answer our questions… (Ctrl+Enter sends)';
    send.lastChild?.nodeType === Node.TEXT_NODE && send.lastChild.remove();
    send.append(internal ? 'Add note' : 'Send reply');
    if (sendClose) sendClose.hidden = internal || t.status === 'closed';
  };
  if (note) note.addEventListener('change', sync);

  async function submit(status) {
    const body = text.value.trim(), files = attach.files();
    problem.hidden = true;
    if (!body && !files.length) {
      problem.textContent = 'Write a message (or attach a file) first.';
      problem.hidden = false;
      text.focus();
      return;
    }
    const internal = !!(note && note.checked);
    const buttons = [send, sendClose].filter(Boolean);
    buttons.forEach((b) => { b.disabled = true; });
    progress.hidden = !files.length;
    progress.value = 0;
    try {
      const th = await sendForm('POST', `/tickets/${id}/replies`, { body, internal, ...(status ? { status } : {}) }, files,
        (v) => { progress.value = v; });
      text.value = '';
      try { sessionStorage.removeItem(draftKey); } catch { /* no storage */ }
      attach.reset();
      if (note) note.checked = false;
      update(th);
      sync();
      notify(internal ? 'Note added' : status === 'closed' ? 'Reply sent and ticket closed' : provider ? 'Reply sent: the customer gets an e-mail' : 'Reply sent. We\'ll e-mail you when we answer.');
      refreshSupportBadge();
      const convo = $('.tk-convo');
      convo?.lastElementChild?.scrollIntoView({ block: 'nearest', behavior: 'smooth' });
    } catch (e) {
      problem.textContent = e.message;
      problem.hidden = false;
    } finally {
      buttons.forEach((b) => { b.disabled = false; });
      progress.hidden = true;
    }
  }
  form.addEventListener('submit', (e) => { e.preventDefault(); submit(); });
  if (sendClose) sendClose.addEventListener('click', () => submit('closed'));
  text.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); submit(); }
  });

  fill(form, label, text,
    h('div', { class: 'tk-drop-overlay', 'aria-hidden': 'true' }, icon('paperclip'), 'Drop files to attach them'),
    attach.el, problem, progress,
    h('div', { class: 'tk-reply-bar' },
      h('div', { class: 'tk-reply-tools' }, note ? h('label', { class: 'check tk-note-toggle' }, note, icon('lock'), 'Internal note') : null, canned),
      h('div', { class: 'actions' }, sendClose, send)));
  sync();
  return form;
}

// ---- A new ticket ----

async function showNewTicket(box, seq) {
  const staff = !isTenant();
  const sum = SUP.summary || await refreshSupportBadge();
  const [depts, sites, accounts] = await Promise.all([api('GET', '/support/departments'), api('GET', '/sites').catch(() => []),
    staff ? api('GET', '/accounts').catch(() => []) : null]);
  if (stale(seq)) return;
  const back = h('a', { class: 'button ghost tk-back', href: '#/support' }, icon('back'), 'All tickets');
  if (!staff && sum && !sum.enabled) {
    fill(box, back, h('div', { class: 'empty tk-empty' }, h('span', { class: 'empty-icon' }, icon('life-buoy')), h('h2', {}, 'Tickets are turned off'),
      h('p', { class: 'muted' }, 'New tickets can\'t be opened at the moment. Please contact your provider another way.')));
    return;
  }
  const visible = depts.filter((d) => !d.hidden);
  const form = h('form', { class: 'card tk-new', novalidate: true });
  const problem = h('p', { class: 'tk-problem', role: 'alert', hidden: true });
  const account = staff ? h('select', { name: 'account_id', required: true },
    h('option', { value: '' }, 'Choose a customer…'), accounts.map((a) => h('option', { value: String(a.id) }, `${a.name} (#${a.id})`))) : null;
  const siteSel = h('select', { name: 'site_id' });
  const fillSites = () => {
    const acct = account ? Number(account.value) : null;
    const mine = sites.filter((s) => !s.parent_id && (!staff || (acct && s.account_id === acct)));
    fill(siteSel, h('option', { value: '' }, mine.length ? 'Not about one site' : 'No sites to choose from'),
      mine.map((s) => h('option', { value: s.id }, s.primary_domain)));
  };
  fillSites();
  if (account) account.addEventListener('change', fillSites);
  // A ticket opened from a site's page is about that site.
  const fromSite = new URLSearchParams(location.hash.split('?')[1] || '').get('site');
  if (fromSite) siteSel.value = fromSite;

  const deptChoices = h('div', { class: 'choices tk-depts', role: 'radiogroup', 'aria-label': 'Department' },
    (staff ? depts : visible).map((d, i) => h('label', { class: 'choice' },
      h('input', { type: 'radio', name: 'department_id', value: String(d.id), checked: i === 0 }),
      h('span', {}, h('strong', {}, d.name + (d.hidden ? ' (hidden)' : '')), d.description ? h('span', { class: 'muted small' }, d.description) : null))));
  const prioChoices = h('div', { class: 'choices tk-prios', role: 'radiogroup', 'aria-label': 'How urgent is it?' },
    PRIORITIES.map(([k, l, hint]) => h('label', { class: 'choice' }, h('input', { type: 'radio', name: 'priority', value: k, checked: k === 'medium' }),
      h('span', {}, h('strong', {}, l), h('span', { class: 'muted small' }, hint)))));
  const subject = h('input', { name: 'subject', required: true, maxlength: 200, autocomplete: 'off',
    placeholder: 'e.g. My contact form stopped sending e-mail' });
  const body = h('textarea', { name: 'body', required: true, rows: 7, maxlength: 20000,
    placeholder: staff ? 'What should the customer know?' : 'What happened? Include the page address, what you did and what you expected. Screenshots help.' });
  const attach = attachBox(form, (sum && sum.limits) || { max_files: 0, extensions: [] });
  const progress = h('progress', { max: 1, value: 0, hidden: true, 'aria-label': 'Uploading' });
  const submit = h('button', { type: 'submit' }, icon('send'), staff ? 'Open ticket' : 'Send to support');

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const f = new FormData(form);
    const say = (msg, el) => { problem.textContent = msg; problem.hidden = false; if (el) el.focus(); };
    problem.hidden = true;
    if (staff && !account.value) return say('Choose the customer this ticket is for.', account);
    if (!subject.value.trim()) return say('Add a short subject: what is it about?', subject);
    if (!body.value.trim()) return say('Describe what you need help with.', body);
    submit.disabled = true;
    progress.hidden = !attach.files().length;
    try {
      const th = await sendForm('POST', '/tickets', {
        account_id: staff ? Number(account.value) : 0, department_id: Number(f.get('department_id') || 0), site_id: siteSel.value,
        subject: subject.value, body: body.value, priority: f.get('priority') || 'medium',
      }, attach.files(), (v) => { progress.value = v; });
      notify(staff ? `Ticket ${th.ticket.mask} opened; the customer gets an e-mail` : `Ticket ${th.ticket.mask} sent. We'll e-mail you when we reply.`);
      if (th.warning) showError(new Error(th.warning));
      refreshSupportBadge();
      go(String(th.ticket.id));
    } catch (err) { say(err.message); } finally { submit.disabled = false; progress.hidden = true; }
  });

  fill(form,
    staff ? h('label', {}, 'Customer', account) : null,
    h('fieldset', { class: 'tk-fieldset' }, h('legend', {}, 'What is it about?'), deptChoices),
    h('div', { class: 'grid' }, h('label', {}, 'Subject', subject), h('label', {}, 'Related site ', h('span', { class: 'muted' }, '(optional)'), siteSel)),
    h('fieldset', { class: 'tk-fieldset' }, h('legend', {}, 'How urgent is it?'), prioChoices),
    h('label', {}, 'Message', body),
    h('div', { class: 'tk-drop-overlay', 'aria-hidden': 'true' }, icon('paperclip'), 'Drop files to attach them'),
    attach.el, problem, progress,
    h('div', { class: 'actions' }, h('a', { class: 'button ghost', href: '#/support' }, 'Cancel'), submit));
  fill(box, back, h('div', { class: 'bar' }, h('div', {}, h('h1', {}, staff ? 'Open a ticket for a customer' : 'New ticket'),
    h('p', { class: 'muted small bar-sub' }, staff ? 'The customer gets an e-mail with your message and can reply from their panel.'
      : 'Tell us what\'s going on. You\'ll get an e-mail when we reply.'))), form);
  (staff ? account : subject).focus();
}

// ---- Staff: departments, canned replies, settings ----

// editDialog shows a form in a dialog; save(values) runs on submit, and the
// dialog stays open with the error if it throws.
function editDialog({ title, fields, ok = 'Save', save }) {
  return new Promise((resolve) => {
    const problem = h('p', { class: 'tk-problem small', role: 'alert', hidden: true });
    const inputs = fields.map((f) => {
      const attrs = { name: f.name, required: !!f.required, placeholder: f.placeholder || '', maxlength: f.max || null };
      if (f.type === 'textarea') return [f, h('textarea', { ...attrs, rows: f.rows || 6 }, f.value || '')];
      if (f.type === 'checkbox') return [f, h('input', { type: 'checkbox', name: f.name, checked: !!f.value })];
      return [f, h('input', { ...attrs, type: f.type || 'text', value: f.value ?? '' })];
    });
    const cancel = h('button', { type: 'button', class: 'ghost' }, 'Cancel');
    const submit = h('button', { type: 'submit' }, ok);
    const form = h('form', {}, h('h2', {}, title), inputs.map(([f, el]) => (f.type === 'checkbox'
      ? h('label', { class: 'check small' }, el, f.label)
      : h('label', { class: 'tk-field' }, f.label, f.hint ? h('span', { class: 'muted' }, ` ${f.hint}`) : null, el))),
    problem, h('div', { class: 'actions' }, cancel, submit));
    const dlg = h('dialog', { class: 'modal tk-dialog', 'aria-label': title }, form);
    let saved = null;
    cancel.addEventListener('click', () => dlg.close());
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const values = Object.fromEntries(inputs.map(([f, el]) => [f.name, f.type === 'checkbox' ? el.checked : f.type === 'number' ? Number(el.value || 0) : el.value]));
      submit.disabled = true;
      problem.hidden = true;
      try { saved = await save(values); dlg.close(); } catch (err) { problem.textContent = err.message; problem.hidden = false; } finally { submit.disabled = false; }
    });
    dlg.addEventListener('close', () => { dlg.remove(); resolve(saved); });
    document.body.append(dlg);
    dlg.showModal();
    inputs[0][1].focus();
  });
}

async function showManage(box, seq) {
  const admin = isAdmin();
  const [canned, depts, settings] = await Promise.all([
    isOperator() ? api('GET', '/support/canned') : [],
    api('GET', '/support/departments'),
    admin ? api('GET', '/support/settings') : null,
  ]);
  if (stale(seq)) return;
  const reload = () => { if (!stale(seq)) loadSupport(); };
  const parts = [
    h('a', { class: 'button ghost tk-back', href: '#/support' }, icon('back'), 'All tickets'),
    h('div', { class: 'bar' }, h('div', {}, h('h1', {}, 'Manage support'),
      h('p', { class: 'muted small bar-sub' }, 'Saved answers for your team, where tickets go, and how the help desk behaves.'))),
  ];

  // Canned replies.
  const cannedForm = (c) => editDialog({
    title: c ? 'Edit canned reply' : 'New canned reply', ok: c ? 'Save' : 'Add',
    fields: [{ name: 'title', label: 'Title', value: c?.title, required: true, max: 100, placeholder: 'e.g. Cleared the cache' },
      { name: 'body', label: 'Text', type: 'textarea', value: c?.body, required: true, rows: 8, placeholder: 'Hi, thanks for reaching out…' }],
    save: (v) => api(c ? 'PUT' : 'POST', c ? `/support/canned/${c.id}` : '/support/canned', v),
  });
  const addCanned = h('button', { type: 'button' }, icon('plus'), 'New canned reply');
  addCanned.addEventListener('click', async () => { if (await cannedForm()) reload(); });
  parts.push(h('section', { class: 'card', 'aria-labelledby': 'tk-canned-h' },
    h('div', { class: 'site-head' }, h('h2', { id: 'tk-canned-h' }, 'Canned replies'), addCanned),
    h('p', { class: 'muted small' }, 'Answers you give often. In a ticket, pick one from "Insert a canned reply" and edit it before sending.'),
    canned.length ? h('ul', { class: 'tk-canned' }, canned.map((c) => {
      const edit = h('button', { type: 'button', class: 'ghost' }, 'Edit');
      edit.addEventListener('click', async () => { if (await cannedForm(c)) reload(); });
      const del = h('button', { type: 'button', class: 'ghost danger' }, 'Delete');
      del.addEventListener('click', async () => {
        if (!await ask(`Delete the canned reply "${c.title}"?`)) return;
        try { await api('DELETE', `/support/canned/${c.id}`); reload(); } catch (e) { showError(e); }
      });
      return h('li', {}, h('div', {}, h('strong', {}, c.title), h('p', { class: 'muted small' }, c.body.length > 160 ? c.body.slice(0, 160) + '…' : c.body)),
        h('div', { class: 'actions' }, edit, del));
    })) : h('p', { class: 'muted small' }, 'None yet.')));

  if (admin) {
    const deptForm = (d) => editDialog({
      title: d ? `Edit ${d.name}` : 'New department', ok: d ? 'Save' : 'Add',
      fields: [{ name: 'name', label: 'Name', value: d?.name, required: true, max: 60, placeholder: 'e.g. Billing' },
        { name: 'description', label: 'Description', hint: '(customers see it when they choose)', value: d?.description, max: 300 },
        { name: 'notify_email', label: 'Also notify', hint: '(an e-mail address, optional)', type: 'email', value: d?.notify_email },
        { name: 'sort', label: 'Order', hint: '(lower comes first)', type: 'number', value: d?.sort ?? 0 },
        { name: 'hidden', label: 'Hidden from customers (staff can still move tickets here)', type: 'checkbox', value: d?.hidden }],
      save: (v) => api(d ? 'PUT' : 'POST', d ? `/support/departments/${d.id}` : '/support/departments', v),
    });
    const addDept = h('button', { type: 'button' }, icon('plus'), 'New department');
    addDept.addEventListener('click', async () => { if (await deptForm()) reload(); });
    parts.push(h('section', { class: 'card', 'aria-labelledby': 'tk-dept-h' },
      h('div', { class: 'site-head' }, h('h2', { id: 'tk-dept-h' }, 'Departments'), addDept),
      h('p', { class: 'muted small' }, 'Customers choose one when they open a ticket. New tickets of a department also go to its address.'),
      table(['Name', 'Also notify', 'Open tickets', 'Visible', ''], depts.map((d) => {
        const edit = h('button', { type: 'button', class: 'ghost' }, 'Edit');
        edit.addEventListener('click', async () => { if (await deptForm(d)) reload(); });
        const del = h('button', { type: 'button', class: 'ghost danger' }, 'Delete');
        del.addEventListener('click', async () => {
          if (!await ask(`Delete the department ${d.name}? Departments with tickets can only be hidden.`)) return;
          try { await api('DELETE', `/support/departments/${d.id}`); reload(); } catch (e) { showError(e); }
        });
        return [h('td', {}, h('strong', {}, d.name), d.description ? h('div', { class: 'muted small' }, d.description) : null),
          d.notify_email || '–', String(d.active), d.hidden ? 'Hidden' : 'Yes', h('td', {}, h('div', { class: 'actions' }, edit, del))];
      }))));

    const f = (name, attrs = {}) => h('input', { name, ...attrs });
    const form = h('form', { class: 'tk-settings' },
      h('label', { class: 'check small' }, f('enabled', { type: 'checkbox', checked: settings.enabled }), 'Customers can open new tickets'),
      h('div', { class: 'grid' },
        h('label', {}, 'Send new tickets and replies to ', h('span', { class: 'muted' }, '(comma-separated)'),
          f('notify_emails', { value: settings.notify_emails.join(', '), placeholder: 'support@yourcompany.com' })),
        h('label', {}, 'Close answered tickets after ', h('span', { class: 'muted' }, '(days without a reply; 0 = never)'),
          f('auto_close_days', { type: 'number', min: 0, max: 365, value: settings.auto_close_days }))),
      h('details', { class: 'advanced' }, h('summary', {}, 'Advanced: attachments and e-mail'),
        h('div', { class: 'grid' },
          h('label', {}, 'Files per message ', h('span', { class: 'muted' }, '(0 = no attachments)'), f('max_files', { type: 'number', min: 0, max: 10, value: settings.max_files })),
          h('label', {}, 'Largest file (MB)', f('max_file_mb', { type: 'number', min: 1, max: 25, value: settings.max_file_mb })),
          h('label', {}, 'Allowed file types', f('extensions', { value: settings.extensions.join(', ') })),
          h('label', {}, 'Reply-To address ', h('span', { class: 'muted' }, '(optional)'), f('reply_to', { type: 'email', value: settings.reply_to || '' })))),
      h('div', { class: 'actions' }, h('button', { type: 'submit' }, 'Save settings')));
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const list = (v) => v.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);
      try {
        await api('PUT', '/support/settings', {
          enabled: form.enabled.checked, notify_emails: list(form.notify_emails.value), auto_close_days: Number(form.auto_close_days.value || 0),
          max_files: Number(form.max_files.value || 0), max_file_mb: Number(form.max_file_mb.value || 1), extensions: list(form.extensions.value),
          reply_to: form.reply_to.value.trim(),
        });
        notify('Support settings saved');
        SUP.summary = null;
        refreshSupportBadge();
      } catch (err) { showError(err); }
    });
    parts.push(h('section', { class: 'card', 'aria-labelledby': 'tk-settings-h' }, h('h2', { id: 'tk-settings-h' }, 'Settings'),
      h('p', { class: 'muted small' }, 'Customers get e-mail at their account\'s address; replies to that e-mail aren\'t read yet, so they answer from the panel. ' +
        'Tickets of a reseller\'s customers go to the reseller until they escalate them.'), form));
  }
  fill(box, parts);
}
