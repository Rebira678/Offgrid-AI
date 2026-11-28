'use strict';
// Offgrid phone client. No libraries, no inline script (the Content-Security-Policy forbids it), and no
// HTML strings: everything from the node is put on the page as text.
//
// What lives where:
//   IndexedDB  kv      the device key (non-extractable), who we are paired as, the last snapshot, id map
//              queue   what the person did while we could not reach the node (commands, in order)
//              photos  pictures waiting to be uploaded
//   The page shows  VIEW = last snapshot + the queue replayed on top, so an edit appears at once and
//   disappears from "waiting" when the node has taken it.

const ACTION = { 'X-Offgrid-Action': '1' };
const $ = (s) => document.querySelector(s);

// ---------------------------------------------------------------- small helpers
function h(tag, props, ...kids) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(props || {})) {
    if (k === 'class') e.className = v;
    else if (k.startsWith('on')) e.addEventListener(k.slice(2), v);
    else if (v === true) e.setAttribute(k, '');
    else if (v !== false && v != null) e.setAttribute(k, v);
  }
  for (const k of kids.flat()) if (k != null && k !== false) e.append(k instanceof Node ? k : document.createTextNode(String(k)));
  return e;
}
const b64u = (buf) => btoa(String.fromCharCode(...new Uint8Array(buf))).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
const hex = (buf) => [...new Uint8Array(buf)].map((x) => x.toString(16).padStart(2, '0')).join('');
const enc = (s) => new TextEncoder().encode(s);
const uid = () => b64u(crypto.getRandomValues(new Uint8Array(12)));
const ago = (ms) => (ms < 60e3 ? 'just now' : ms < 3600e3 ? Math.round(ms / 60e3) + ' min' : Math.round(ms / 3600e3) + ' h');

// ---------------------------------------------------------------- IndexedDB
const dbp = new Promise((res, rej) => {
  const r = indexedDB.open('offgrid-phone', 1);
  r.onupgradeneeded = () => {
    r.result.createObjectStore('kv');
    r.result.createObjectStore('queue', { keyPath: 'id' });
    r.result.createObjectStore('photos', { keyPath: 'id' });
  };
  r.onsuccess = () => res(r.result);
  r.onerror = () => rej(r.error);
});
async function tx(store, mode, fn) {
  const db = await dbp;
  return new Promise((res, rej) => {
    const t = db.transaction(store, mode);
    const out = fn(t.objectStore(store));
    t.oncomplete = () => res(out.result);
    t.onerror = () => rej(t.error);
    t.onabort = () => rej(t.error);
  });
}
const kvGet = (k) => tx('kv', 'readonly', (s) => s.get(k));
const kvPut = (k, v) => tx('kv', 'readwrite', (s) => s.put(v, k));
const kvDel = (k) => tx('kv', 'readwrite', (s) => s.delete(k));
const all = (store) => tx(store, 'readonly', (s) => s.getAll());
const put = (store, v) => tx(store, 'readwrite', (s) => s.put(v));
const del = (store, k) => tx(store, 'readwrite', (s) => s.delete(k));
const clear = (store) => tx(store, 'readwrite', (s) => s.clear());

// ---------------------------------------------------------------- the device key
// ECDSA P-256, created non-extractable: the private key can sign but can never be read out, even by this page.
async function deviceKey() {
  let k = await kvGet('key');
  if (!k) {
    k = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    await kvPut('key', k);
  }
  return k;
}
async function publicKeyTagged() {
  const k = await deviceKey();
  return 'ecdsa-p256:' + b64u(await crypto.subtle.exportKey('raw', k.publicKey));
}
async function sign(msg) {
  const k = await deviceKey();
  return b64u(await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, k.privateKey, enc(msg)));
}

// ---------------------------------------------------------------- talking to the node
class Offline extends Error {}
async function api(method, path, body, headers) {
  let r;
  try {
    r = await fetch(path, {
      method,
      credentials: 'same-origin',
      headers: { ...(method === 'POST' ? ACTION : {}), ...(body && !(body instanceof Blob) ? { 'Content-Type': 'application/json' } : {}), ...(headers || {}) },
      body: body == null ? undefined : body instanceof Blob ? body : JSON.stringify(body),
    });
  } catch (e) {
    throw new Offline(String(e));
  }
  let data = null;
  try { data = await r.json(); } catch { /* an empty or non-JSON body */ }
  return { status: r.status, data: data || {} };
}

// signIn starts a new session with the device key, no code needed. It only works for a phone that is still enrolled.
async function signIn(device) {
  const c = await api('POST', '/api/challenge');
  if (c.status !== 200) return c;
  return api('POST', '/api/login', { device, nonce: c.data.nonce, sig: await sign('offgrid-phone-login-v1\0' + device + '\0' + c.data.nonce) });
}

// ---------------------------------------------------------------- state
const S = { me: null, snap: null, queue: [], photos: [], idmap: {}, online: navigator.onLine, screen: 'list', open: null, filter: 'open', syncing: false, notices: [], error: '' };

function notice(msg) {
  S.notices.push(msg);
  render();
  setTimeout(() => { S.notices.shift(); render(); }, 15000);
}

async function load() {
  S.me = (await kvGet('paired')) || null;
  S.snap = (await kvGet('snap')) || null;
  S.idmap = (await kvGet('idmap')) || {};
  S.queue = (await all('queue')).sort((a, b) => a.at - b.at);
  S.photos = (await all('photos')).sort((a, b) => a.at - b.at);
}

// view() is what the person sees: the last snapshot with the queued commands applied on top.
function view() {
  if (!S.snap) return { tasks: [], suggestions: [] };
  const tasks = S.snap.tasks.map((t) => ({ ...t, notes_list: [...(t.notes_list || [])], files: [...(t.files || [])] }));
  let sugs = S.snap.suggestions.map((s) => ({ ...s }));
  const find = (id) => tasks.find((t) => t.id === id || t.id === S.idmap[id.slice(1)]);
  for (const c of S.queue) {
    const t = c.task ? find(c.task) : null;
    if (c.op === 'create') tasks.push({ id: '@' + c.id, short_id: '', title: c.title, site: c.site || '', status: 'open', priority: c.priority || 'medium', notes_list: [], files: [], waiting: true });
    else if (c.op === 'status' && t) { t.status = c.status; t.waiting = true; }
    else if (c.op === 'priority' && t) { t.priority = c.priority; t.waiting = true; }
    else if (c.op === 'note' && t) { t.notes_list.push(c.text + ' (waiting)'); t.waiting = true; }
    else if (c.op === 'accept' || c.op === 'dismiss') {
      const s = sugs.find((x) => x.id === c.task);
      sugs = sugs.filter((x) => x.id !== c.task);
      if (s && c.op === 'accept') {
        const st = tasks.find((x) => x.id === s.task);
        if (st) { if (s.status) st.status = s.status; if (s.priority) st.priority = s.priority; st.waiting = true; }
      }
    }
  }
  for (const p of S.photos) {
    const t = find(p.task);
    if (t) { t.files.push({ hash: 'pending-' + p.id, pending: p, name: p.name, image: true }); t.waiting = true; }
  }
  return { tasks, suggestions: sugs };
}

async function enqueue(cmd) {
  cmd.id = uid();
  cmd.at = Date.now();
  await put('queue', cmd);
  S.queue.push(cmd);
  render();
  sync();
}

// ---------------------------------------------------------------- syncing
async function resolveIds() { // commands queued against a temporary task ID get the real one once we know it
  for (const c of S.queue) {
    if (c.task && c.task.startsWith('@') && S.idmap[c.task.slice(1)]) { c.task = S.idmap[c.task.slice(1)]; await put('queue', c); }
  }
  for (const p of S.photos) {
    if (p.task.startsWith('@') && S.idmap[p.task.slice(1)]) { p.task = S.idmap[p.task.slice(1)]; await put('photos', p); }
  }
}

async function ensureSession(fn) {
  let r = await fn();
  if (r.status === 401 && S.me) {
    const l = await signIn(S.me.device);
    if (l.status === 200) r = await fn();
    else if (l.status === 401) { await unpair('This phone is no longer enrolled in the node. Ask for a new pairing code.'); }
  }
  return r;
}

async function refresh() {
  const r = await ensureSession(() => api('GET', '/api/state'));
  if (r.status === 200) {
    S.snap = r.data;
    await kvPut('snap', r.data);
  }
  return r;
}

async function sync() {
  if (S.syncing || !S.me) return;
  S.syncing = true;
  try {
    await resolveIds();
    if (S.queue.length) {
      const sent = S.queue.slice(0, 100);
      const now = Date.now();
      const r = await ensureSession(() => api('POST', '/api/sync', { commands: sent.map((c) => ({ id: c.id, op: c.op, task: c.task, title: c.title, site: c.site, priority: c.priority, status: c.status, text: c.text, was: c.was, age_ms: now - c.at })) }));
      if (r.status === 200) {
        for (const res of r.data.results) {
          if (res.task) S.idmap[res.id] = res.task;
          if (res.outcome === 'rejected') notice('Not applied: ' + (res.message || res.code));
          else if (res.overwrote) notice('Applied, but it replaced a newer change (' + res.overwrote + ')');
          await del('queue', res.id);
        }
        S.queue = S.queue.filter((c) => !r.data.results.some((x) => x.id === c.id));
        await kvPut('idmap', S.idmap);
        await resolveIds();
      } else if (r.status === 413 || r.status === 400) {
        notice('The node refused the queue: ' + (r.data.message || r.status));
      }
    }
    for (const p of [...S.photos]) {
      if (p.task.startsWith('@')) continue; // its task has not reached the node yet
      const r = await ensureSession(() => api('POST', '/api/tasks/' + encodeURIComponent(p.task) + '/photos', p.blob, { 'Content-Type': 'image/jpeg', 'X-Offgrid-Name': encodeURIComponent(p.name), 'X-Offgrid-Sha256': p.sha, 'X-Offgrid-Age-Ms': String(Date.now() - p.at) }));
      if (r.status === 200 || r.status === 201) { await del('photos', p.id); S.photos = S.photos.filter((x) => x.id !== p.id); }
      else if (r.status >= 400 && r.status < 500 && r.status !== 401 && r.status !== 429) {
        notice('Photo not stored: ' + (r.data.message || r.status));
        await del('photos', p.id); S.photos = S.photos.filter((x) => x.id !== p.id);
      } else break;
    }
    S.online = true;
    await refresh();
  } catch (e) {
    if (e instanceof Offline) S.online = false;
    else throw e;
  } finally {
    S.syncing = false;
    render();
  }
}

async function unpair(msg) {
  try { await api('POST', '/api/logout'); } catch { /* offline: the cookie is cleared when we next can */ }
  for (const s of ['queue', 'photos']) await clear(s);
  for (const k of ['paired', 'snap', 'idmap', 'key']) await kvDel(k);
  S.me = null; S.snap = null; S.queue = []; S.photos = []; S.idmap = {}; S.error = msg || '';
  render();
}

// ---------------------------------------------------------------- photos
// Re-drawing the picture to a canvas does two useful things: it shrinks a 4 MB camera file to a few hundred KB,
// and it throws away the EXIF block, which on a phone includes where the picture was taken.
async function prepare(file) {
  const bmp = await createImageBitmap(file, { imageOrientation: 'from-image' });
  const k = Math.min(1, 2048 / Math.max(bmp.width, bmp.height));
  const cv = document.createElement('canvas');
  cv.width = Math.round(bmp.width * k); cv.height = Math.round(bmp.height * k);
  cv.getContext('2d').drawImage(bmp, 0, 0, cv.width, cv.height);
  bmp.close();
  const blob = await new Promise((res) => cv.toBlob(res, 'image/jpeg', 0.85));
  if (!blob) throw new Error('could not read that picture');
  return blob;
}

async function addPhoto(taskId, file) {
  try {
    const blob = await prepare(file);
    const p = { id: uid(), task: taskId, blob, name: (file.name || 'photo').replace(/\.[^.]*$/, '') + '.jpg', at: Date.now(), sha: hex(await crypto.subtle.digest('SHA-256', await blob.arrayBuffer())) };
    await put('photos', p);
    S.photos.push(p);
    render();
    sync();
  } catch (e) {
    notice('Could not add that photo: ' + e.message);
  }
}

// ---------------------------------------------------------------- screens
function pairScreen() {
  const code = h('input', { type: 'text', id: 'code', autocomplete: 'off', autocapitalize: 'characters', spellcheck: 'false', placeholder: 'XXXX-XXXX-XXXX-XXXX-XXXX' });
  const label = h('input', { type: 'text', id: 'label', placeholder: 'Name this phone, e.g. Ana’s phone', maxlength: '64' });
  const msg = h('p', { class: 'err', id: 'pair-error' }, S.error);
  const go = h('button', { class: 'primary', id: 'pair-go', onclick: async () => {
    go.disabled = true; msg.textContent = '';
    try {
      const norm = code.value.toUpperCase().replace(/[-\s]/g, '');
      const lab = label.value.trim() || 'phone';
      const pub = await publicKeyTagged();
      const proof = await sign('offgrid-enroll-v1\0' + norm.slice(0, 8) + '\0' + pub + '\0' + lab);
      const r = await api('POST', '/api/pair', { code: code.value, public_key: pub, label: lab, proof });
      if (r.status === 200) {
        S.me = { device: r.data.device, label: r.data.label };
        await kvPut('paired', S.me);
        S.error = '';
        render();
        sync();
      } else msg.textContent = r.data.message || 'Could not pair (' + r.status + ')';
    } catch (e) {
      msg.textContent = e instanceof Offline ? 'Cannot reach the node. Check you are on the site’s WiFi.' : String(e.message || e);
    } finally { go.disabled = false; }
  } }, 'Pair this phone');
  return h('section', {}, h('h2', {}, 'Pair this phone'),
    h('p', { class: 'muted' }, 'Ask the site admin for a pairing code (offgrid phone pair). It works once and expires.'),
    h('div', { class: 'card' }, code, h('div', { class: 'gap' }), label, h('div', { class: 'gap' }), go, msg));
}

function taskCard(t) {
  const statusTag = t.status === 'done' ? 'done-status' : t.status === 'in_progress' ? 'waiting' : '';
  const photoCount = (t.files || []).length;
  return h('button', { class: 'task' + (t.status === 'done' ? ' done' : ''), 'data-task': t.id, onclick: () => { S.screen = 'task'; S.open = t.id; render(); } },
    h('div', { class: 't' }, t.title),
    h('div', { class: 's' },
      t.priority === 'high' ? h('span', { class: 'tag high' }, 'high') : null,
      statusTag ? h('span', { class: 'tag ' + statusTag }, t.status.replace('_', ' ')) : h('span', { class: 'tag' }, t.status.replace('_', ' ')),
      t.waiting ? h('span', { class: 'tag waiting' }, 'syncing…') : null,
      photoCount ? h('span', { class: 'tag' }, photoCount + ' photo' + (photoCount > 1 ? 's' : '')) : null));
}

function listScreen() {
  const v = view();
  const title = h('input', { type: 'text', id: 'new-title', placeholder: 'Add a task…', maxlength: '200', enterkeyhint: 'done' });
  const add = () => { const x = title.value.trim(); if (x) { enqueue({ op: 'create', title: x, priority: 'medium' }); title.value = ''; } };
  title.addEventListener('keydown', (e) => { if (e.key === 'Enter') add(); });
  const shown = v.tasks.filter((t) => S.filter === 'all' || t.status !== 'done');
  const out = [h('div', { class: 'row' }, h('div', { class: 'grow' }, title), h('button', { class: 'primary', id: 'add', onclick: add }, 'Add'))];
  if (v.suggestions.length) {
    out.push(h('h2', {}, 'Suggested'));
    for (const s of v.suggestions) {
      out.push(h('div', { class: 'card suggestion', 'data-sug': s.id },
        h('div', { class: 't' }, s.task_name),
        h('div', { class: 'muted' }, [s.status && 'status → ' + s.status.replace('_', ' '), s.priority && 'priority → ' + s.priority, s.note].filter(Boolean).join(' · ')),
        s.stale ? h('div', { class: 'err' }, 'The task changed since; accepting would be refused.') : null,
        h('div', { class: 'row' },
          h('button', { class: 'primary', onclick: () => enqueue({ op: 'accept', task: s.id }) }, 'Accept'),
          h('button', { onclick: () => enqueue({ op: 'dismiss', task: s.id }) }, 'Dismiss'))));
    }
  }
  out.push(h('h2', {}, 'Tasks'),
    h('div', { class: 'seg' },
      h('button', { 'aria-pressed': String(S.filter === 'open'), onclick: () => { S.filter = 'open'; render(); } }, 'Open'),
      h('button', { 'aria-pressed': String(S.filter === 'all'), onclick: () => { S.filter = 'all'; render(); } }, 'All')));
  if (!shown.length) out.push(h('p', { class: 'muted' }, S.snap ? 'Nothing here.' : 'No data yet. Connect to the node once to load the list.'));
  for (const t of shown) out.push(taskCard(t));
  if (S.snap) out.push(h('p', { class: 'muted' }, 'Last read ' + ago(Date.now() - Date.parse(S.snap.now)) + (S.snap.tasks_total > S.snap.tasks.length ? ' · showing ' + S.snap.tasks.length + ' of ' + S.snap.tasks_total : '')));
  out.push(h('div', { class: 'gap' }), h('button', { class: 'danger', onclick: () => { if (confirm('Forget this phone? Waiting changes that have not reached the node are lost.')) unpair(); } }, 'Forget this phone'));
  return h('div', { class: 'container' }, h('section', {}, out));
}

function taskScreen() {
  const t = view().tasks.find((x) => x.id === S.open);
  if (!t) { S.screen = 'list'; return listScreen(); }
  const seg = (items, cur, op, field) => h('div', { class: 'seg' }, items.map(([val, text]) =>
    h('button', { 'aria-pressed': String(cur === val), 'data-set': val, onclick: () => cur !== val && enqueue({ op, task: t.id, [field]: val, was: cur }) }, text)));
  const note = h('textarea', { id: 'note', rows: '3', placeholder: 'Add a note…', maxlength: '1000' });
  const file = h('input', { type: 'file', accept: 'image/*', id: 'photo', hidden: true, onchange: (e) => { const f = e.target.files[0]; e.target.value = ''; if (f) addPhoto(t.id, f); } });
  const imgs = (t.files || []).map((f) => f.pending
    ? h('img', { src: URL.createObjectURL(f.pending.blob), alt: 'waiting to upload' })
    : (S.online ? h('img', { src: '/api/tasks/' + encodeURIComponent(t.id) + '/attachments/' + encodeURIComponent(f.hash), alt: f.name || 'photo', loading: 'lazy' }) : null));
  return h('div', { class: 'container' }, h('section', {},
    h('p', {}, h('button', { id: 'back', onclick: () => { S.screen = 'list'; render(); } }, '‹ Tasks')),
    h('div', { class: 'card' },
      h('div', { class: 't' }, t.title),
      t.site ? h('div', { class: 'muted' }, t.site) : null,
      t.waiting ? h('span', { class: 'tag waiting' }, 'syncing…') : null),
    h('h2', {}, 'Status'), seg([['open', 'Open'], ['in_progress', 'In progress'], ['done', 'Done']], t.status, 'status', 'status'),
    h('h2', {}, 'Priority'), seg([['low', 'Low'], ['medium', 'Medium'], ['high', 'High']], t.priority, 'priority', 'priority'),
    h('h2', {}, 'Photos'),
    imgs.length ? h('div', { class: 'photos' }, imgs) : null,
    file,
    h('button', { id: 'add-photo', onclick: () => file.click() }, 'Add a photo'),
    !S.online && (t.files || []).some((f) => !f.pending) ? h('p', { class: 'muted' }, 'Photos show when connected.') : null,
    h('h2', {}, 'Notes'),
    (t.notes_list || []).length ? h('ul', { class: 'notes' }, (t.notes_list || []).map((n) => h('li', {}, n.replace(/\s*\[c:[^\]]+\]/g, '')))) : null,
    note, h('div', { class: 'gap' }),
    h('button', { class: 'primary', id: 'add-note', onclick: () => { const x = note.value.trim(); if (x) { enqueue({ op: 'note', task: t.id, text: x }); note.value = ''; } } }, 'Add note')));
}

function render() {
  const pill = $('#pill');
  const waiting = S.queue.length + S.photos.length;
  pill.className = 'pill ' + (S.online ? 'online' : 'offline');
  pill.textContent = (S.online ? 'online' : 'offline') + (waiting ? ' · ' + waiting + ' waiting' : '');
  const main = $('#main');
  // A redraw (a sync finishing, the 30-second timer) must not wipe what the person is typing.
  const typed = {};
  for (const id of ['new-title', 'note', 'code', 'label']) { const e = document.getElementById(id); if (e) typed[id] = e.value; }
  const focused = document.activeElement && document.activeElement.id;
  main.replaceChildren(!S.me ? pairScreen() : S.screen === 'task' ? taskScreen() : listScreen());
  for (const [id, v] of Object.entries(typed)) { const e = document.getElementById(id); if (e && v) e.value = v; }
  if (focused && typed[focused] !== undefined) { const e = document.getElementById(focused); if (e) e.focus(); }
  $('#notices').replaceChildren(...S.notices.map((n) => h('div', { class: 'notice' }, n)));
}

// ---------------------------------------------------------------- start
window.addEventListener('online', () => { S.online = true; render(); sync(); });
window.addEventListener('offline', () => { S.online = false; render(); });
document.addEventListener('visibilitychange', () => { if (!document.hidden) sync(); });
setInterval(() => { if (!document.hidden && S.me) sync(); }, 30000);

(async () => {
  if ('serviceWorker' in navigator) navigator.serviceWorker.register('/sw.js').catch(() => {});
  try {
    await load();
  } catch (e) {
    // No IndexedDB (a private tab, a browser that blocks it): there is nowhere to keep the device key or the queue,
    // so the offline app cannot work here. Say so instead of showing a blank page.
    $('#main').replaceChildren(h('p', { class: 'err', id: 'no-storage' }, 'This browser will not let the app keep data on the phone, so it cannot work offline here. Open it in a normal (not private) tab, or in Chrome.'));
    return;
  }
  render();
  if (S.me) sync();
})();
