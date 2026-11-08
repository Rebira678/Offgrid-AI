'use strict';
// Phone check for Offgrid. Everything runs in the browser; the only network calls are to the
// node that served this page (/ping, /report). Each check records {name, ok, detail}.

const results = [];
const $ = (id) => document.getElementById(id);
const DB = 'offgrid-spike';

function rec(name, ok, detail) {
  const i = results.findIndex((r) => r.name === name);
  const r = { name, ok, detail: String(detail) };
  if (i >= 0) results[i] = r; else results.push(r);
  render();
}

function render() {
  const ul = $('checks');
  ul.textContent = '';
  for (const r of results) {
    const li = document.createElement('li');
    const tag = document.createElement('span');
    const cls = r.ok === true ? 'ok' : r.ok === false ? 'bad' : 'warn';
    tag.className = 'tag ' + cls;
    tag.textContent = r.ok === true ? 'PASS' : r.ok === false ? 'FAIL' : 'NOTE';
    const name = document.createElement('span');
    name.className = 'name';
    name.textContent = ' ' + r.name;
    const d = document.createElement('span');
    d.className = 'detail';
    d.textContent = r.detail;
    li.append(tag, name, d);
    ul.append(li);
  }
  const bad = results.filter((r) => r.ok === false).length;
  const v = $('verdict');
  if (bad) { v.className = 'verdict bad'; v.textContent = bad + ' check(s) failed. This phone cannot run Offgrid the planned way yet.'; }
  else if (results.length) { v.className = 'verdict ok'; v.textContent = 'Every check so far passed. Now do the offline test below.'; }
  $('json').textContent = JSON.stringify(report(), null, 2);
}

function report() {
  return {
    page: 'offgrid-phone-check/1',
    ua: navigator.userAgent,
    platform: (navigator.userAgentData && navigator.userAgentData.platform) || navigator.platform || '',
    url: location.origin,
    secure_context: window.isSecureContext,
    online_now: navigator.onLine,
    phone_time: new Date().toISOString(),
    results,
    offline_loads: offlineLoads,
  };
}

// ---- small IndexedDB helper -------------------------------------------------
function idb() {
  return new Promise((resolve, reject) => {
    const q = indexedDB.open(DB, 1);
    q.onupgradeneeded = () => { q.result.createObjectStore('kv'); };
    q.onsuccess = () => resolve(q.result);
    q.onerror = () => reject(q.error);
  });
}
async function kvPut(k, v) {
  const db = await idb();
  return new Promise((res, rej) => { const t = db.transaction('kv', 'readwrite'); t.objectStore('kv').put(v, k); t.oncomplete = () => { db.close(); res(); }; t.onerror = () => rej(t.error); });
}
async function kvGet(k) {
  const db = await idb();
  return new Promise((res, rej) => { const r = db.transaction('kv').objectStore('kv').get(k); r.onsuccess = () => { db.close(); res(r.result); }; r.onerror = () => rej(r.error); });
}

let offlineLoads = [];
const hex = (buf) => Array.from(new Uint8Array(buf)).map((b) => b.toString(16).padStart(2, '0')).join('');

// ---- the checks -------------------------------------------------------------
async function checkSecure() {
  rec('Secure context (HTTPS trusted)', window.isSecureContext, window.isSecureContext
    ? 'The browser trusts this address. Service workers and WebCrypto are allowed.'
    : 'Not a secure context: the certificate authority is not installed or not trusted, so offline pages and device keys cannot work.');
  rec('Address uses https', location.protocol === 'https:', location.href);
}

async function checkWorker(online) {
  if (!('serviceWorker' in navigator)) { rec('Service worker', false, 'Not available in this browser (is the page a secure context?).'); return; }
  try {
    // Offline, do not try to fetch /sw.js again: ask for the registration that already exists.
    const reg = online ? await navigator.serviceWorker.register('/sw.js', { scope: '/' }) : await navigator.serviceWorker.getRegistration('/');
    if (!reg) throw new Error('no service worker is registered: visit this page once while the node is reachable');
    await navigator.serviceWorker.ready;
    const controlled = !!navigator.serviceWorker.controller;
    rec('Service worker registered', true, 'scope ' + reg.scope + (controlled ? '; controlling this page' : '; will control the page after the next reload'));
    const hit = await caches.match('/app.js');
    rec('Page files cached for offline', !!hit, hit ? 'app.js is in the phone\'s cache storage' : 'not cached yet (reload once and check again)');
  } catch (e) {
    rec('Service worker registered', false, e && e.message ? e.message : e);
  }
}

async function checkStorage() {
  try {
    await kvPut('probe', { at: Date.now(), text: 'offgrid' });
    const v = await kvGet('probe');
    rec('IndexedDB write and read', !!v && v.text === 'offgrid', 'the offline queue and local copy will live here');
  } catch (e) { rec('IndexedDB write and read', false, e && e.message ? e.message : e); }
  try {
    const persisted = navigator.storage && navigator.storage.persist ? await navigator.storage.persist() : null;
    const est = navigator.storage && navigator.storage.estimate ? await navigator.storage.estimate() : null;
    const q = est ? (' quota ' + Math.round(est.quota / 1048576) + ' MiB, used ' + Math.round(est.usage / 1048576) + ' MiB') : '';
    // A refusal is a caveat, not a failure: the browser decides, and installing to the home screen usually changes it.
    rec('Storage kept when the phone is low on space', persisted === true ? true : null, (persisted ? 'persistent' : 'the browser may clear it under storage pressure; install to home screen helps') + ';' + q);
  } catch (e) { rec('Storage kept when the phone is low on space', null, e && e.message ? e.message : e); }
}

async function checkCrypto() {
  try {
    if (!(window.crypto && crypto.subtle)) throw new Error('WebCrypto is not available (needs a secure context)');
    const pair = await crypto.subtle.generateKey({ name: 'ECDSA', namedCurve: 'P-256' }, false, ['sign', 'verify']);
    const msg = new TextEncoder().encode('offgrid device key test');
    const sig = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, pair.privateKey, msg);
    const ok = await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, pair.publicKey, sig, msg); // (algorithm, key, signature, data)
    rec('Device key: ECDSA P-256 sign and verify', ok && sig.byteLength === 64, 'signature ' + sig.byteLength + ' bytes (raw r||s)');
    let extractable = true;
    try { await crypto.subtle.exportKey('pkcs8', pair.privateKey); } catch (_) { extractable = false; }
    rec('Private key cannot be read out', !extractable, extractable ? 'the browser exported a key that was created non-extractable' : 'non-extractable: page code and a stolen backup cannot copy it');
    // The key must survive a restart: store the CryptoKey object itself, read it back, sign with it.
    await kvPut('devicekey', pair);
    const back = await kvGet('devicekey');
    const sig2 = await crypto.subtle.sign({ name: 'ECDSA', hash: 'SHA-256' }, back.privateKey, msg);
    const ok2 = await crypto.subtle.verify({ name: 'ECDSA', hash: 'SHA-256' }, back.publicKey, sig2, msg);
    rec('Device key survives storage in IndexedDB', ok2, 'a stored key signed after being read back');
    const raw = await crypto.subtle.exportKey('raw', pair.publicKey);
    rec('Device ID can be derived from the public key', raw.byteLength === 65, 'SHA-256 of the public key: ' + hex(await crypto.subtle.digest('SHA-256', raw)).slice(0, 16) + '…');
  } catch (e) {
    rec('Device key: ECDSA P-256 sign and verify', false, e && e.message ? e.message : e);
  }
}

async function checkNode() {
  const t0 = Date.now();
  try {
    const ctl = new AbortController();
    const timer = setTimeout(() => ctl.abort(), 4000);
    const r = await fetch('/ping', { cache: 'no-store', signal: ctl.signal });
    clearTimeout(timer);
    const j = await r.json();
    const t1 = Date.now();
    const skew = Math.round((((t0 + t1) / 2) - Date.parse(j.now)) / 1000);
    rec('Node reachable', true, 'answered in ' + (t1 - t0) + ' ms');
    // Sync is refused when clocks differ by more than 5 minutes, so the phone's clock matters.
    rec('Phone clock matches the node', Math.abs(skew) <= 300 ? true : false, 'the phone is ' + (skew >= 0 ? skew + ' s ahead of' : -skew + ' s behind') + ' the node (limit 300 s)');
    return true;
  } catch (e) {
    return false;
  }
}

// An offline load is the proof: this script is running, so the service worker served it from the phone.
async function noteOfflineLoad() {
  const entry = { at: new Date().toISOString(), controlled: !!navigator.serviceWorker && !!navigator.serviceWorker.controller, sent: false };
  const list = ((await kvGet('offline_loads')) || []).concat(entry);
  await kvPut('offline_loads', list.slice(-20));
  offlineLoads = list.slice(-20);
  $('offline').textContent = '';
  const b = document.createElement('strong');
  b.textContent = 'OFFLINE LOAD: ';
  $('offline').append(b, document.createTextNode(entry.controlled
    ? 'this page loaded with no answer from the node, served by the service worker from the phone\'s storage. That is the result we need.'
    : 'the page loaded but was NOT served by a service worker. Offline use would fail.'));
  rec('Page loads with the node unreachable', entry.controlled, 'recorded ' + entry.at);
}

async function send(auto) {
  const state = $('sendstate');
  try {
    const r = await fetch('/report', { method: 'POST', headers: { 'Content-Type': 'application/json', 'X-Offgrid-Spike': '1' }, body: JSON.stringify(report()) });
    if (!r.ok) throw new Error('node answered ' + r.status);
    state.textContent = (auto ? 'Sent automatically' : 'Sent') + ' at ' + new Date().toLocaleTimeString() + '. The node has your result.';
    const list = ((await kvGet('offline_loads')) || []).map((x) => Object.assign(x, { sent: true }));
    await kvPut('offline_loads', list);
    return true;
  } catch (e) {
    state.textContent = 'Could not reach the node (' + (e && e.message ? e.message : e) + '). Use "Copy as text" instead, or reconnect and press the button again.';
    return false;
  }
}

async function main() {
  $('send').addEventListener('click', () => send(false));
  $('copy').addEventListener('click', async () => {
    const text = JSON.stringify(report(), null, 2);
    try { await navigator.clipboard.writeText(text); $('sendstate').textContent = 'Copied.'; }
    catch (_) { const r = document.createRange(); r.selectNodeContents($('json')); const s = getSelection(); s.removeAllRanges(); s.addRange(r); $('sendstate').textContent = 'Select the text below and copy it.'; }
  });
  render();
  try { offlineLoads = (await kvGet('offline_loads')) || []; } catch (_) { offlineLoads = []; }
  await checkSecure();
  const online = await checkNode();
  if (!online) await noteOfflineLoad();
  await checkWorker(online);
  await checkStorage();
  await checkCrypto();
  if (online) await send(true);
}
main();
