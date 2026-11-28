'use strict';
// The service worker keeps the app's own files so the app opens with the node unreachable. It never
// touches /api/: tasks, photos and the queue live in the page's IndexedDB, behind the session.
const CACHE = 'offgrid-phone-shell-v1';
const SHELL = ['/', '/app.js', '/style.css', '/manifest.webmanifest', '/icon.svg', '/icon-192.png'];

self.addEventListener('install', (e) => {
  e.waitUntil(caches.open(CACHE).then((c) => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener('activate', (e) => {
  e.waitUntil(
    caches.keys()
      .then((ks) => Promise.all(ks.filter((k) => k !== CACHE).map((k) => caches.delete(k))))
      .then(() => self.clients.claim()),
  );
});

self.addEventListener('fetch', (e) => {
  const r = e.request;
  const u = new URL(r.url);
  if (r.method !== 'GET' || u.origin !== self.location.origin || u.pathname.startsWith('/api/')) return;
  e.respondWith(
    caches.open(CACHE).then(async (c) => {
      const hit = await c.match(r.mode === 'navigate' ? '/' : r, { ignoreSearch: true });
      // Serve what we have at once and refresh it for next time; with no network, what we have is all there is.
      const fresh = fetch(r).then((res) => {
        if (res.ok && SHELL.includes(u.pathname)) c.put(r.mode === 'navigate' ? '/' : r, res.clone());
        return res;
      });
      if (hit) { fresh.catch(() => {}); return hit; }
      return fresh;
    }),
  );
});
