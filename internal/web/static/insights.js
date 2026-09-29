'use strict';

// Per-site performance insights: PHP response times, page cache hits, slow
// URLs and PHP errors (GET /sites/{id}/insights).

const fmtMS = (ms) => (ms >= 1000 ? `${(ms / 1000).toFixed(ms >= 10000 ? 0 : 1)} s` : `${Math.round(ms)} ms`);
const LEVELS = { 'fatal error': 'failed', 'parse error': 'failed', warning: 'warning', 'recoverable fatal error': 'failed' };

function renderInsights(el, site) {
  const f = (c) => $('.' + c, el);
  const details = f('insights'), body = f('insights-body'), hours = f('insights-hours');
  const refresh = f('insights-refresh'), clear = f('insights-clear');
  if (site.status !== 'active') { details.hidden = true; return; }
  const load = async () => {
    body.replaceChildren(h('p', { class: 'muted small' }, 'Loading…'));
    try { showInsights(el, await api('GET', `/sites/${site.id}/insights?hours=${hours.value}`)); }
    catch (e) { body.replaceChildren(); showError(e); }
  };
  details.addEventListener('toggle', () => { if (details.open) load(); });
  hours.addEventListener('change', load);
  refresh.addEventListener('click', load);
  clear.addEventListener('click', async () => {
    if (!await ask('Forget this site\'s PHP errors? New ones are collected from now on.')) return;
    try { await api('DELETE', `/sites/${site.id}/insights/errors`); await load(); } catch (e) { showError(e); }
  });
}

function showInsights(el, ins) {
  const p = ins.perf, t = p.totals;
  const pages = t.cache_hits + t.cache_misses;
  const hitRate = pages ? Math.round((100 * t.cache_hits) / pages) : null;
  const fatal = ins.php_errors.filter((e) => LEVELS[e.level] === 'failed').reduce((n, e) => n + e.count, 0);
  $('.insights-summary', el).textContent = t.php_requests
    ? `· p95 ${fmtMS(p.p95_ms)}` + (hitRate != null ? ` · ${hitRate}% cached` : '') + (fatal ? ` · ${fmtNum(fatal)} fatal errors` : '')
    : (fatal ? `· ${fmtNum(fatal)} fatal errors` : '');

  const stat = (label, value, title) => h('div', { title: title || '' }, h('dt', {}, label), h('dd', {}, value));
  const stats = h('dl', { class: 'stats' },
    stat('PHP responses', fmtNum(t.php_requests), 'Requests PHP answered: cache hits and static files are not counted'),
    stat('Median', t.php_requests ? fmtMS(p.p50_ms) : '–'),
    stat('95%', t.php_requests ? fmtMS(p.p95_ms) : '–', '95% of PHP responses were at least this fast'),
    stat('99%', t.php_requests ? fmtMS(p.p99_ms) : '–'),
    stat('Over 1 s', fmtNum(t.slow)),
    stat('Page cache hits', hitRate != null ? `${hitRate}%` : '–', `${fmtNum(t.cache_hits)} pages served from the cache, ${fmtNum(t.cache_misses)} rendered by PHP to be stored`));

  // Response time distribution: one meter per bucket.
  const total = p.histogram.reduce((a, b) => a + b, 0);
  const label = (i) => (i === 0 ? `≤ ${fmtMS(p.buckets[0])}` : i < p.buckets.length ? `${fmtMS(p.buckets[i - 1])} – ${fmtMS(p.buckets[i])}` : `> ${fmtMS(p.buckets[i - 1])}`);
  const hist = total ? h('table', { class: 'hist' }, p.histogram.map((n, i) => (n ? h('tr', {},
    h('td', {}, label(i)),
    h('td', {}, h('meter', { min: 0, max: total, value: n })),
    h('td', { class: 'muted' }, `${fmtNum(n)} (${((100 * n) / total).toFixed(n * 1000 < total ? 1 : 0)}%)`)) : null))) : null;

  const live = ins.live;
  const now = live ? h('p', { class: 'muted small' }, `Now: CPU ${live.percent}%`,
    live.workers_percent != null ? ` · PHP workers ${live.workers_percent}% busy${live.queued ? `, ${live.queued} waiting` : ''}` : '',
    live.p95_ms != null ? ` · last minute p95 ${fmtMS(live.p95_ms)}` : '', ` · ${live.replicas} replica(s)`) : null;

  const slow = table(['Slow URL', 'Count', 'Average', 'Slowest', 'Status', 'Last'], ins.slow_requests.map((s) => [
    h('td', { class: 'wrap' }, `${s.method} ${s.path}`), fmtNum(s.count), fmtMS(s.avg_ms), fmtMS(s.max_ms),
    h('td', { class: s.last_status >= 500 ? 'st-failed' : '' }, String(s.last_status)), fmtTime(s.last_seen),
  ]));
  const errors = table(['PHP error', 'Whose', 'Count', 'Last'], ins.php_errors.map((e) => [
    h('td', {}, h('span', { class: 'st-' + (LEVELS[e.level] || 'unknown') }, e.level), ' ', e.message,
      e.file ? h('div', { class: 'wrap muted' }, `${e.file}:${e.line}`) : null),
    e.source.replace(':', ': '), fmtNum(e.count), fmtTime(e.last_seen),
  ]));

  // fill(), not replaceChildren: now and hist may be null.
  fill($('.insights-body', el),
    stats, now, hist,
    h('h3', { class: 'small' }, 'Slowest URLs (over 1 s, most total time first)'), slow,
    h('h3', { class: 'small' }, 'PHP errors (most frequent first)'), errors,
    h('p', { class: 'muted small' }, 'Response times come from the web server\'s log and include waiting for a PHP worker. ' +
      'PHP errors are read from the site\'s own error log, grouped by message and place; the plugin or theme is taken from the file, or from the stack trace for errors inside WordPress.'),
  );
}
