'use strict';
// The smallest service worker that proves the thing that matters: after one visit, the
// page loads with no network at all. Cache-first for the page's own files; the node's
// /ping and /report always go to the network (an offline /ping failing IS the signal).
const CACHE = 'offgrid-spike-v1';
const FILES = ['/', '/app.js', '/app.css', '/manifest.webmanifest'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(FILES.map((f) => new Request(f, { cache: 'reload' })))).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(caches.keys().then((keys) => Promise.all(keys.filter((k) => k !== CACHE).map((k) => caches.delete(k)))).then(() => self.clients.claim()));
});

self.addEventListener('fetch', (e) => {
  const req = e.request;
  const url = new URL(req.url);
  if (req.method !== 'GET' || url.origin !== self.location.origin) return;
  if (url.pathname === '/ping' || url.pathname === '/report') return; // network only
  e.respondWith(
    caches.match(req, { ignoreSearch: true }).then((hit) => hit || fetch(req).catch(() => (req.mode === 'navigate' ? caches.match('/') : Response.error())))
  );
});
