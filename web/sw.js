// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors
const CACHE = 'yggchat-v14';
const INDEX = './index.html';

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll([INDEX, './manifest.webmanifest', './qrcode.js', './jsqr.min.js'])).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

// Cache-first for the shell; network-first for /api/* (never cached).
self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.startsWith('/api/')) return;
  // Pages: network-first so updates show up immediately; other assets: cache-first.
  if (e.request.mode === 'navigate') {
    e.respondWith(fetch(e.request).then(res => { if (res.ok) { const c = res.clone(); caches.open(CACHE).then(x => x.put(INDEX, c)); } return res; }).catch(() => caches.match(INDEX)));
    return;
  }
  e.respondWith(
    caches.match(e.request, { ignoreSearch: true }).then(hit =>
      hit ||
      fetch(e.request).then(res => {
        // clone only what gets cached: an unread clone (404s) kept the request
        // open, and a worker with open requests never lets the next version take over
        if (res.ok) { const copy = res.clone(); caches.open(CACHE).then(c => c.put(e.request, copy)); }
        return res;
      }) // not reachable and not cached: a network error, not index.html posing as a script
    )
  );
});
