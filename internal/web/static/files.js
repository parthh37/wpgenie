'use strict';

// File manager (/sites/{id}/files…): browse a site's files, upload (drop
// them on the list), edit text files, download files and folders (as zip),
// rename, copy, change permissions, extract zip archives and delete.
// Paths go in the query string, so the audit log names them. Shares api(),
// h(), fill(), ask(), askText(), notify() and showError() with the rest.

const FILE_DIR = new Map(); // site ID -> the folder shown
const TEXT_FILE = /(^|\/)(\.htaccess|\.user\.ini|\.env[^/]*|robots\.txt|readme|license|changelog)$|\.(php\d?|phtml|inc|js|mjs|cjs|jsx|ts|tsx|json|map|css|scss|sass|less|html?|xml|svg|txt|md|markdown|ini|conf|cfg|log|csv|tsv|ya?ml|twig|po|pot|sql|sh|lock|vue|htm|tpl|mustache|hbs)$/i;
const IMAGE_FILE = /\.(png|jpe?g|gif|webp|avif|ico|bmp)$/i;
const ZIP_FILE = /\.zip$/i;

const qs = (params) => new URLSearchParams(Object.entries(params).filter(([, v]) => v != null && v !== false)).toString();
const filesURL = (site, sub, params) => `/api/v1/sites/${encodeURIComponent(site.id)}/files${sub}?` + qs(params);
const joinPath = (dir, name) => (dir === '/' ? '' : dir) + '/' + name;
const parentDir = (p) => p.replace(/\/[^/]*$/, '') || '/';
const baseName = (p) => p.slice(p.lastIndexOf('/') + 1);
const canWrite = () => !(ME && ME.role === 'viewer');

// resolvePath: what someone typed for a new name, relative to dir unless
// it starts with "/".
const resolvePath = (dir, typed) => (typed.startsWith('/') ? typed : joinPath(dir, typed));

// putFile sends a file's bytes (PUT, raw body) with upload progress, which
// fetch() can't report.
function putFile(site, path, body, params = {}, onProgress) {
  return new Promise((resolve, reject) => {
    const x = new XMLHttpRequest();
    x.open('PUT', filesURL(site, '/content', { path, ...params }));
    x.setRequestHeader('X-Requested-With', 'wpgenie');
    x.setRequestHeader('Content-Type', 'application/octet-stream');
    if (onProgress) x.upload.onprogress = (e) => { if (e.lengthComputable) onProgress(e.loaded / e.total); };
    x.onload = () => {
      let data = {};
      try { data = JSON.parse(x.responseText); } catch { /* not JSON */ }
      if (x.status >= 200 && x.status < 300) return resolve(data);
      if (x.status === 401) showSignIn();
      const err = new Error(data.error || x.statusText || `HTTP ${x.status}`);
      err.status = x.status;
      reject(err);
    };
    x.onerror = () => reject(new Error(`Uploading ${baseName(path)} failed: the connection dropped`));
    x.send(body);
  });
}

// download starts a browser download (a folder comes as a zip archive).
function download(site, path) {
  const a = h('a', { href: filesURL(site, '/download', { path }), download: '' });
  document.body.append(a);
  a.click();
  a.remove();
}

function renderFiles(el, site) {
  const d = $('.files', el);
  if (!['active', 'suspended'].includes(site.status)) { d.hidden = true; return; }
  $('.files-summary', el).textContent = FILE_DIR.has(site.id) ? `· ${FILE_DIR.get(site.id)}` : '';
  d.addEventListener('toggle', () => { if (d.open) showFiles(el, site).catch(showError); });
}

async function showFiles(el, site, dir = FILE_DIR.get(site.id) || '/') {
  const body = $('.files-body', el);
  let list;
  try {
    list = await api('GET', `/sites/${site.id}/files?` + qs({ path: dir }));
  } catch (e) {
    // A plan without the file manager: say so here rather than as an error.
    if (e.status === 403) { fill(body, h('p', { class: 'muted small' }, e.message)); return; }
    // The folder went away (deleted, renamed elsewhere): back to the top.
    if (e.status === 404 && dir !== '/') { FILE_DIR.delete(site.id); return showFiles(el, site, '/'); }
    throw e;
  }
  dir = list.path;
  FILE_DIR.set(site.id, dir);
  $('.files-summary', el).textContent = dir === '/' ? '' : `· ${dir}`;
  const go = (p) => showFiles(el, site, p).catch(showError);
  const reload = () => go(dir);
  const writable = canWrite() && list.writable && site.status === 'active';

  // Breadcrumbs: / › wp-content › themes
  const parts = dir.split('/').filter(Boolean);
  const crumbs = h('nav', { class: 'crumbs', 'aria-label': 'Folder' },
    h('button', { type: 'button', class: 'link', onclick: () => go('/') }, site.primary_domain),
    parts.flatMap((p, i) => [h('span', { class: 'muted' }, ' / '),
      i === parts.length - 1 ? h('strong', {}, p)
        : h('button', { type: 'button', class: 'link', onclick: () => go('/' + parts.slice(0, i + 1).join('/')) }, p)]));

  const run = (fn) => async () => { try { await fn(); } catch (e) { showError(e); } };
  const tool = (label, fn, opts = {}) => h('button', { type: 'button', class: 'ghost', hidden: opts.hidden, onclick: run(fn) }, label);

  const picker = h('input', { type: 'file', multiple: true, hidden: true });
  picker.addEventListener('change', () => { const fs = [...picker.files]; picker.value = ''; uploadAll(fs); });
  const progress = h('div', { class: 'file-uploads' });

  // uploadAll uploads files into this folder one after the other, asking
  // once before replacing any that exist.
  async function uploadAll(files) {
    if (!files.length) return;
    const names = new Set(list.entries.map((x) => x.name));
    const clash = files.filter((f) => names.has(f.name));
    let replace = false;
    if (clash.length) {
      const shown = clash.slice(0, 5).map((f) => f.name).join(', ') + (clash.length > 5 ? ` and ${clash.length - 5} more` : '');
      const question = clash.length === 1 ? `Replace ${clash[0].name}? It already exists in ${dir}.`
        : `Replace ${clash.length} files that already exist? ${shown}.`;
      replace = await ask(`${question} ${files.length > clash.length ? 'Cancel uploads only the new ones.' : ''}`,
        { ok: 'Replace', danger: true });
    }
    let ok = 0;
    for (const f of files) {
      if (names.has(f.name) && !replace) continue;
      const bar = h('progress', { max: 1, value: 0 });
      const row = h('div', { class: 'file-upload' }, h('span', { class: 'small' }, f.name, h('span', { class: 'muted' }, ` · ${fmtBytes(f.size)}`)), bar);
      progress.append(row);
      try {
        await putFile(site, joinPath(dir, f.name), f, { overwrite: names.has(f.name) ? 1 : null }, (v) => { bar.value = v; });
        ok++;
      } catch (e) { showError(e); }
      row.remove();
    }
    if (ok) notify(`Uploaded ${ok === 1 ? files.find((f) => !names.has(f.name) || replace).name : ok + ' files'} to ${dir}`);
    await reload();
  }

  const newFile = tool('New file', async () => {
    const name = await askText(`In ${dir}. A path like inc/x.php creates it in that (existing) folder.`,
      { title: 'New file', label: 'Name', placeholder: 'example.php', ok: 'Create' });
    if (!name) return;
    const path = resolvePath(dir, name);
    await putFile(site, path, new Blob([]));
    await reload();
    await editFile(site, path, reload);
  }, { hidden: !writable });
  const newFolder = tool('New folder', async () => {
    const name = await askText(`In ${dir}.`, { title: 'New folder', label: 'Name', ok: 'Create' });
    if (!name) return;
    await api('POST', `/sites/${site.id}/files/folder?` + qs({ path: resolvePath(dir, name) }));
    await reload();
  }, { hidden: !writable });
  const upload = tool('Upload', async () => picker.click(), { hidden: !writable });
  const zipAll = tool('Download folder', async () => download(site, dir));
  const refresh = tool('Refresh', reload);

  const rows = list.entries.map((x) => fileRow(site, dir, x, writable, { go, reload }));
  const drop = h('div', { class: 'file-drop' + (writable ? '' : ' locked') },
    rows.length ? h('table', { class: 'files-table' },
      h('tr', {}, ['Name', 'Size', 'Modified', 'Permissions', ''].map((t) => h('th', {}, t))), rows)
      : h('p', { class: 'muted small' }, 'This folder is empty.' + (writable ? ' Drop files here to upload them.' : '')));
  if (writable) {
    // Dropped files upload into the folder shown. (Folders: zip them, upload
    // the archive and extract it.)
    drop.addEventListener('dragover', (e) => { if ([...e.dataTransfer.types].includes('Files')) { e.preventDefault(); drop.classList.add('over'); } });
    drop.addEventListener('dragleave', (e) => { if (!drop.contains(e.relatedTarget)) drop.classList.remove('over'); });
    drop.addEventListener('drop', (e) => {
      e.preventDefault();
      drop.classList.remove('over');
      const items = [...e.dataTransfer.items || []];
      if (items.some((i) => i.webkitGetAsEntry && i.webkitGetAsEntry()?.isDirectory)) {
        showError(new Error('Folders can\'t be dropped: zip the folder, upload the archive, then choose Extract.'));
      }
      uploadAll([...e.dataTransfer.files].filter((f, i) => !(items[i]?.webkitGetAsEntry?.()?.isDirectory)));
    });
  }

  fill(body,
    h('div', { class: 'files-bar' }, crumbs,
      h('div', { class: 'actions' }, dir !== '/' ? tool('Up', async () => go(parentDir(dir))) : null, refresh, zipAll, newFolder, newFile, upload)),
    picker, progress, drop,
    list.truncated ? h('p', { class: 'muted small' }, `Only the first ${list.entries.length} entries are shown; use SFTP for folders this large.`) : null,
    h('p', { class: 'muted small' }, !canWrite() ? 'Read-only: your role can look, not change.'
      : site.status !== 'active' ? 'Read-only while the site is suspended.'
        : !list.writable ? 'This folder belongs to WPGenie, not the site: it can\'t be changed here.'
          : 'Files you add belong to the site, as WordPress\'s own do. wp-config.php is kept outside this folder, ' +
            'where neither the site nor this file manager can change it. Uploads up to 1 GB; larger files over SFTP.'));
}

// fileRow is one entry: its name opens it (a folder, the editor, an image
// preview or a download) and a menu holds everything else.
function fileRow(site, dir, x, writable, { go, reload }) {
  const path = joinPath(dir, x.name);
  const isDir = x.type === 'dir', isFile = x.type === 'file';
  const editable = isFile && TEXT_FILE.test(x.name);
  const changeable = writable && x.owned;
  const open = async () => {
    if (isDir) return go(path);
    if (x.type === 'link') return go(path).catch(() => {}); // a link to a folder opens it
    if (editable) return editFile(site, path, reload);
    if (IMAGE_FILE.test(x.name)) { window.open(filesURL(site, '/download', { path, inline: 1 }), '_blank', 'noopener'); return; }
    download(site, path);
  };

  const acts = [
    editable && ['edit', changeable && parseInt(x.mode, 8) & 0o200 ? 'Edit' : 'View'],
    (isFile || isDir) && ['download', isDir ? 'Download as zip' : 'Download'],
    changeable && ['rename', 'Rename / move'],
    writable && (isFile || isDir) && ['copy', 'Copy'],
    changeable && (isFile || isDir) && ['mode', 'Permissions'],
    writable && isFile && ZIP_FILE.test(x.name) && ['extract', 'Extract here'],
    changeable && ['delete', 'Delete'],
  ].filter(Boolean);
  const menu = h('select', { class: 'file-menu', 'aria-label': `Actions for ${x.name}` },
    h('option', { value: '' }, 'Actions…'), acts.map(([v, label]) => h('option', { value: v }, label)));
  menu.addEventListener('change', async () => {
    const act = menu.value;
    menu.value = '';
    try { await fileAction(site, dir, x, act, { open, reload }); } catch (e) { showError(e); }
  });

  const glyph = isDir ? 'folder' : x.type === 'link' ? 'external' : ZIP_FILE.test(x.name) ? 'archive' : 'file';
  const name = h('button', { type: 'button', class: 'link file-name', title: path }, icon(glyph), x.name);
  name.addEventListener('click', () => open().catch(showError));
  return h('tr', {},
    h('td', {}, name, x.type === 'link' ? h('span', { class: 'muted small' }, ` → ${x.target}`) : null,
      x.owned ? null : h('span', { class: 'badge', title: 'Managed by WPGenie: read-only' }, 'WPGenie')),
    h('td', { class: 'small' }, isDir ? '–' : fmtBytes(x.size)),
    h('td', { class: 'small muted' }, fmtTime(x.modified)),
    h('td', { class: 'small' }, h('code', {}, x.mode)),
    h('td', {}, menu));
}

async function fileAction(site, dir, x, act, { open, reload }) {
  const path = joinPath(dir, x.name);
  const isDir = x.type === 'dir';
  const q = (sub, params) => `/sites/${site.id}/files${sub}?` + qs(params);
  switch (act) {
    case 'edit': return open();
    case 'download': return download(site, path);
    case 'rename': {
      const to = await askText(`Type a new name, or a path to move it: other/folder/${x.name} (from here) or /wp-content/… (from the top).`,
        { title: `Rename ${x.name}`, label: 'New name', value: x.name, ok: 'Rename' });
      if (!to || to === x.name) return;
      await api('POST', q('/move', { path, to: resolvePath(dir, to) }));
      notify(`Moved to ${resolvePath(dir, to)}`);
      return reload();
    }
    case 'copy': {
      const dot = isDir ? -1 : x.name.lastIndexOf('.');
      const suggestion = dot > 0 ? `${x.name.slice(0, dot)}-copy${x.name.slice(dot)}` : `${x.name}-copy`;
      const to = await askText('Links inside aren\'t copied; nothing existing is replaced.',
        { title: `Copy ${x.name}`, label: 'Name of the copy', value: suggestion, ok: 'Copy' });
      if (!to) return;
      const r = await api('POST', q('/copy', { path, to: resolvePath(dir, to) }));
      notify(isDir ? `Copied ${r.files} file(s) in ${r.folders} folder(s)` : `Copied to ${resolvePath(dir, to)}`);
      return reload();
    }
    case 'mode': {
      const mode = await askText(isDir ? 'Folders are usually 755: the site keeps full access (7), others may enter and list (5).'
        : 'Files are usually 644 (the site writes, everyone reads); 600 or 640 keeps a file from other users, 444 locks it.',
      { title: `Permissions of ${x.name}`, label: 'Octal permissions', value: x.mode.replace(/^0/, ''), ok: 'Change' });
      if (!mode) return;
      await api('PUT', q('/mode', { path, mode }));
      return reload();
    }
    case 'extract': {
      const params = { path, to: dir };
      try {
        const r = await api('POST', q('/extract', params));
        notify(`Extracted ${r.files} file(s)` + (r.skipped ? `; ${r.skipped} link(s) or special entries skipped` : ''));
      } catch (e) {
        if (e.status !== 409 || !/already exist/.test(e.message)) throw e;
        const which = e.message.replace(/^conflict: /, '').replace(/ already exist.*$/, '');
        if (!await ask(`Replace existing files? ${which} already exist; the archive's versions would replace them.`, { ok: 'Replace', danger: true })) return;
        const r = await api('POST', q('/extract', { ...params, overwrite: 1 }));
        notify(`Extracted ${r.files} file(s), replacing existing ones`);
      }
      return reload();
    }
    case 'delete': {
      const what = isDir ? `${path} and everything in it` : path;
      if (!await ask(`Delete ${x.name}? This permanently deletes ${what}. Backups, if any, still have it.`)) return;
      await api('DELETE', q('', { path }));
      notify(`Deleted ${path}`);
      return reload();
    }
  }
}

// editFile opens a text file in the editor. Saves carry the version that
// was read: if the file changed in between (WordPress, SFTP, someone else
// here), the save stops and asks before overwriting.
async function editFile(site, path, onSaved) {
  const t = await api('GET', `/sites/${site.id}/files/content?` + qs({ path }));
  const dlg = $('#editor'), ta = $('textarea', dlg), save = $('.editor-save', dlg), state = $('.editor-state', dlg);
  // A text box's value always has \n line endings: files written with \r\n
  // get theirs back on save.
  const crlf = t.content.includes('\r\n');
  const readOnly = !t.writable || !canWrite() || site.status !== 'active';
  let version = t.version, saved = t.content.replace(/\r\n/g, '\n');
  $('#editor-title').textContent = path;
  ta.value = saved;
  ta.readOnly = readOnly;
  save.hidden = readOnly;
  const dirty = () => ta.value !== saved;
  const show = () => {
    state.textContent = readOnly ? (t.writable ? 'read-only' : `read-only (${t.mode})`) : dirty() ? 'unsaved changes' : 'saved';
  };
  show();

  const doSave = async (overwrite) => {
    save.disabled = true;
    state.textContent = 'saving…';
    const text = ta.value;
    try {
      const body = crlf ? text.replace(/\n/g, '\r\n') : text;
      const r = await putFile(site, path, new Blob([body]), overwrite ? { overwrite: 1 } : { version });
      version = r.version;
      saved = text;
      show();
      if (onSaved) onSaved().catch(() => {});
    } catch (e) {
      show();
      if (e.status === 409 && !overwrite) {
        if (await ask(`Overwrite the newer version? ${path} changed since you opened it (WordPress, SFTP or someone else). ` +
          'Saving replaces those changes with yours; Cancel keeps editing (copy your changes out, then reopen the file).',
        { ok: 'Overwrite', danger: true })) return doSave(true);
        return;
      }
      showError(e);
    } finally { save.disabled = false; }
  };

  const ctl = new AbortController();
  const on = (target, ev, fn) => target.addEventListener(ev, fn, { signal: ctl.signal });
  on(save, 'click', () => doSave(false));
  on(ta, 'input', show);
  on(ta, 'keydown', (e) => {
    if ((e.ctrlKey || e.metaKey) && e.key.toLowerCase() === 's') {
      e.preventDefault();
      if (!readOnly && !save.disabled) doSave(false);
    } else if (e.key === 'Tab' && !e.shiftKey && !e.ctrlKey && !e.metaKey && !e.altKey && !readOnly) {
      e.preventDefault();
      ta.setRangeText('\t', ta.selectionStart, ta.selectionEnd, 'end');
      show();
    }
  });
  // Closing (Close, Escape) with unsaved changes asks first.
  const closing = async (e) => {
    if (!dirty() || readOnly) return;
    e.preventDefault();
    if (await ask(`Discard unsaved changes? Your edits to ${baseName(path)} aren't saved.`, { ok: 'Discard', danger: true })) {
      saved = ta.value;
      dlg.close();
    }
  };
  on(dlg, 'cancel', closing);
  on($('button[value=close]', dlg), 'click', closing);
  on(dlg, 'close', () => { ctl.abort(); ta.value = ''; });
  dlg.showModal();
  ta.focus();
  ta.setSelectionRange(0, 0);
  ta.scrollTop = 0;
}
