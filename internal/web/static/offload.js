'use strict';

// Uploads offload to S3-compatible storage (GET/PUT /sites/{id}/offload).
// The secret key is write-only: the API never returns it.

Object.assign(JOB_NAMES, { 'offload-sync': 'Uploads offload sync', 'offload-download': 'Uploads copied back from the bucket' });

function renderOffload(el, site) {
  const f = (c) => $('.' + c, el);
  const details = f('offload'), box = f('offload-status');
  const fields = {
    endpoint: f('off-endpoint'), region: f('off-region'), bucket: f('off-bucket'), prefix: f('off-prefix'),
    access_key_id: f('off-key-id'), secret_key: f('off-secret'), public_url: f('off-public'),
    acl: f('off-acl'), local_days: f('off-days'),
  };
  const save = f('off-save'), sync = f('off-sync'), download = f('off-download'), off = f('off-off');
  const active = site.status === 'active';
  [...Object.values(fields), save, sync, download, off].forEach((c) => { c.disabled = !active; });
  fields.prefix.placeholder = `${site.id}/uploads/`;
  let current = { enabled: false };

  const when = (t) => (t ? fmtTime(t) : 'never');
  const show = (st) => {
    current = st;
    const on = st.enabled;
    if (on) {
      for (const k of ['endpoint', 'region', 'bucket', 'prefix', 'public_url', 'acl']) fields[k].value = st[k] || '';
      fields.local_days.value = st.local_days;
      fields.access_key_id.placeholder = st.access_key_id + ' (unchanged if empty)';
      fields.secret_key.placeholder = st.secret_set ? 'unchanged if empty' : 'secret access key';
    }
    sync.hidden = download.hidden = off.hidden = !on;
    download.hidden = !on || !st.removed_local;
    f('offload-summary').textContent = !on
      ? (st.parent_public_url ? '· off (missing uploads come from the live site\'s bucket)' : '· off')
      : `· ${st.bucket}/${st.prefix}` + (st.last_error ? ' · failing' : '') + (st.local_days ? ` · local copies ${st.local_days} days` : '');
    if (!on) {
      fill(box, st.parent_public_url
        ? h('p', { class: 'muted small' }, 'This staging site serves uploads it doesn\'t have from its live site\'s bucket (read-only): ', h('code', {}, st.parent_public_url))
        : null);
      return;
    }
    fill(box,
      table(['', 'Last', ''], [
        ['Copy of new files', when(st.last_incremental), st.syncing ? 'running now' : ''],
        ['Full comparison', when(st.last_full), 'nightly'],
        ['Last copy', st.last_objects ? `${fmtNum(st.last_objects)} files, ${fmtBytes(st.last_bytes)}` : 'nothing new', ''],
        ['In total', `${fmtNum(st.total_objects)} files, ${fmtBytes(st.total_bytes)} copied; ${fmtNum(st.total_deleted)} deleted`,
          st.pending_deletes ? `${fmtNum(st.pending_deletes)} deletes waiting` : ''],
        ['Only in the bucket', st.removed_local ? `${fmtNum(st.removed_local)} uploads, ${fmtBytes(st.removed_bytes)}` : 'none',
          st.last_cleanup ? `local copies last removed ${fmtTime(st.last_cleanup)}` : ''],
      ]),
      h('p', { class: 'muted small' }, 'Uploads missing on this server are served from ', h('code', {}, st.public_url), '.'),
      st.last_error ? h('p', { class: 'st-failed small' }, `Last attempt ${when(st.last_attempt)} failed (retried with backoff): ${st.last_error}`) : null,
    );
  };
  const refresh = async () => {
    try { show(await api('GET', `/sites/${site.id}/offload`)); } catch (e) { showError(e); }
  };
  details.addEventListener('toggle', () => { if (details.open) refresh(); });

  save.addEventListener('click', async () => {
    save.disabled = true;
    save.textContent = 'Checking…';
    try {
      const body = { enabled: true, local_days: Number(fields.local_days.value) || 0 };
      for (const k of ['endpoint', 'region', 'bucket', 'prefix', 'access_key_id', 'secret_key', 'public_url', 'acl']) body[k] = fields[k].value.trim();
      show(await api('PUT', `/sites/${site.id}/offload`, body));
      fields.secret_key.value = fields.access_key_id.value = '';
    } catch (e) { showError(e); }
    finally { save.disabled = false; save.textContent = 'Save'; }
  });
  sync.addEventListener('click', async () => {
    sync.disabled = true;
    try { await startJob('POST', `/sites/${site.id}/offload/sync`, null, refresh); } catch (e) { showError(e); }
    finally { sync.disabled = false; }
  });
  download.addEventListener('click', async () => {
    if (!confirm(`Copy the ${current.removed_local} uploads that only exist in the bucket back to this server?`)) return;
    try { await startJob('POST', `/sites/${site.id}/offload/download`, null, refresh); } catch (e) { showError(e); }
  });
  off.addEventListener('click', async () => {
    let force = false;
    if (current.removed_local) {
      if (!confirm(`${current.removed_local} uploads only exist in the bucket. Turning offload off makes them unavailable on the site ` +
        '(use "Copy back" first to keep them). Turn it off anyway?')) return;
      force = true;
    } else if (!confirm('Stop copying uploads to the bucket? The objects already there stay (delete them from the bucket if they\'re no longer needed).')) return;
    try { show(await api('PUT', `/sites/${site.id}/offload`, { enabled: false, force })); } catch (e) { showError(e); }
  });
}
