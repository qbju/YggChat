// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors
const CACHE = 'yggchat-pwa-v39';
const INDEX = './index.html';

self.addEventListener('install', e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll([INDEX, './manifest.webmanifest', './wasm_exec.js', './shim.js', './qrcode.js', './twemoji.min.js', './ygg.wasm', './THIRD_PARTY_NOTICES.txt', './LICENSE.txt'].map(u => new Request(u, { cache: 'reload' })))).then(() => self.skipWaiting()));
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
  if (e.request.method !== 'GET' || url.origin !== location.origin || url.pathname.includes('/api/') || url.pathname.includes('/dl/')) return;
  // Pages: network-first so updates show up immediately; other assets: cache-first.
  if (e.request.mode === 'navigate') {
    e.respondWith(fetch(e.request).then(res => { const c = res.clone(); if (res.ok) caches.open(CACHE).then(x => x.put(INDEX, c)); return res; }).catch(() => caches.match(INDEX)));
    return;
  }
  e.respondWith(
    caches.open(CACHE).then(c => c.match(e.request, { ignoreSearch: true })).then(hit =>
      hit ||
      fetch(e.request).then(res => {
        const copy = res.clone();
        if (res.ok) caches.open(CACHE).then(c => c.put(e.request, copy));
        return res;
      }).catch(() => caches.match(INDEX))
    )
  );
});
