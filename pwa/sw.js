// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors
const CACHE = 'yggchat-pwa-v47';
const INDEX = './index.html';
const ASSETS = [INDEX, './manifest.webmanifest', './wasm_exec.js', './shim.js', './qrcode.js', './jsqr.min.js', './twemoji.min.js', './ygg.wasm', './THIRD_PARTY_NOTICES.txt', './LICENSE.txt'];

// All files of a version are fetched into its own cache before it takes over
// (an install that fails half-way is dropped and retried by the browser).
self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(ASSETS.map(u => new Request(u, { cache: 'reload' })))).then(() => self.skipWaiting()));
});

self.addEventListener('activate', e => {
  e.waitUntil(
    caches.keys()
      .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
      .then(() => self.clients.claim())
  );
});

const scopePath = () => new URL(self.registration.scope).pathname;

// The page and its scripts always come from the same cache: index.html from the
// network next to shim.js/ygg.wasm from the old cache (and that index then
// cached for good) left PWAs half-updated whenever the new version's install
// was slow or failed. A new version shows once its worker has everything and
// takes over (the page reloads on controllerchange).
self.addEventListener('fetch', e => {
  const url = new URL(e.request.url);
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.includes('/api/') || url.pathname.includes('/dl/')) return;
  if (/[?&]fresh=/.test(url.search)) return; // the page re-fetching a broken cached file: network only
  if (e.request.mode === 'navigate') {
    const p = url.pathname, root = scopePath();
    if (p === root || p === root + 'index.html') {
      e.respondWith(caches.open(CACHE).then(c => c.match(INDEX)).then(hit => hit || fetch(e.request).then(res => {
        if (res.ok) { const copy = res.clone(); caches.open(CACHE).then(c => c.put(INDEX, copy)); }
        return res;
      })));
      return;
    }
    e.respondWith(fetch(e.request).catch(() => caches.open(CACHE).then(c => c.match(e.request, { ignoreSearch: true })).then(hit => hit || caches.match(INDEX))));
    return;
  }
  e.respondWith(
    caches.open(CACHE).then(c => c.match(e.request, { ignoreSearch: true })).then(hit =>
      hit ||
      fetch(e.request).then(res => {
        // clone only what gets cached: an unread clone (404s) kept the request
        // open, and a worker with open requests never lets the next version take over
        if (res.ok) { const copy = res.clone(); caches.open(CACHE).then(c => c.put(e.request, copy)); }
        return res;
      }) // offline and not cached: a network error, not index.html posing as a script or wasm
    )
  );
});
