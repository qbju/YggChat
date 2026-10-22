// Loads pwa/shim.js in a Node vm with a fake wasm node, IndexedDB and DOM, so
// packet handlers and the /api emulation can be tested without a browser.
// Test-only hook: the shim exposes internals on globalThis.__yggTest when that
// object exists before it runs (never in a real page).
'use strict';
const fs = require('fs'), path = require('path'), vm = require('vm'), nodeCrypto = require('crypto');
const SRC = fs.readFileSync(process.env.SHIM || path.join(__dirname, '..', '..', 'pwa', 'shim.js'), 'utf8');

const Y = require('./ygg');
function makeNode(opts = {}) {
  const kp = opts.kp || null; // real key: real address, signatures and key checks
  const addr = kp ? kp.addr : (opts.addr || '200:1111:2222:3333:4444:5555:6666:7777');
  const sent = [], events = [], errors = [], logged = [];
  let onPacket = null;
  const timers = new Set();
  const wrapT = (f, kind) => (fn, ms, ...a) => { const t = f(() => { if (kind === 't') timers.delete(t); try { fn(...a); } catch (e) { errors.push(e); } }, ms); if (t && t.unref) t.unref(); timers.add(t); return t; };
  const kv = new Map();
  const idbReq = (fn) => { const q = {}; setImmediate(() => { try { q.result = fn(); q.onsuccess && q.onsuccess(); } catch (e) { q.error = e; q.onerror && q.onerror(); } }); return q; };
  const db = {
    transaction() {
      const t = { objectStore: () => ({ get: k => idbReq(() => structuredClone(kv.get(k))), put: (v, k) => { kv.set(k, structuredClone(v)); setImmediate(() => t.oncomplete && t.oncomplete()); } }) };
      return t;
    },
  };
  const indexedDB = { open() { const q = {}; setImmediate(() => { q.result = db; q.onsuccess && q.onsuccess(); }); return q; } };
  if (opts.store) kv.set('state', opts.store);
  const et = new EventTarget();
  const ls = new Map();
  const ctx = {
    console: opts.quiet === false ? console : { log() {}, error: (...a) => logged.push(a), warn() {} },
    setTimeout: wrapT(setTimeout, 't'), setInterval: wrapT(setInterval, 'i'), clearTimeout, clearInterval, setImmediate, queueMicrotask,
    TextEncoder, TextDecoder, URL, Response, Request, Headers, structuredClone, crypto: nodeCrypto.webcrypto, atob, btoa, DOMException, AbortController,
    indexedDB,
    localStorage: { getItem: k => ls.has(k) ? ls.get(k) : null, setItem: (k, v) => ls.set(k, String(v)), removeItem: k => ls.delete(k) },
    sessionStorage: { getItem: () => null, setItem() {}, removeItem() {} },
    navigator: { onLine: true },
    location: { href: 'https://yggchat.test/', origin: 'https://yggchat.test', reload() {} },
    document: { body: null, hidden: false, createElement: () => ({ style: {}, appendChild() {}, remove() {} }), addEventListener() {} },
    addEventListener: (t, f, o) => et.addEventListener(t, f, o),
    dispatchEvent: e => et.dispatchEvent(e),
    Event,
    fetch: async u => { if (String(u).includes('ygg.wasm')) return new Response(''); throw new Error('no network in tests'); },
    EventSource: function () {},
    WebSocket: opts.WebSocket || function () { throw new Error('no network in tests'); },
    WebAssembly: { instantiateStreaming: async () => ({ instance: {} }), instantiate: async () => ({ instance: {} }) },
    Go: class { constructor() { this.importObject = {}; } run() { setImmediate(() => et.dispatchEvent(new Event('yggready'))); } },
    __yggTest: {},
    // fake wasm exports
    yggStart: (key, peers, cb) => { onPacket = cb; return { addr, key: key || (kp ? kp.priv : 'k'.repeat(128)) }; },
    yggSend: (peer, json) => { if (typeof json !== 'string') throw new Error('yggSend: not a string'); sent.push({ peer, w: JSON.parse(json), len: Buffer.byteLength(json) }); },
    yggPub: () => kp ? kp.pk : (opts.pk || 'ab'.repeat(32)),
    yggSign: msg => kp ? Y.signHex(kp, msg) : 'cd'.repeat(64),
    yggVerify: opts.verify || (kp ? Y.verify : ((by, pk, msg, sg) => sg.startsWith('GOOD'))),
    yggAddrOK: kp ? Y.addrOK : () => true,
    yggPeers: () => 1,
    yggPeerList: () => [],
    yggAddPeer: () => ({}), yggRemovePeer: () => ({}),
    yggNostrDefaults: () => '["wss://relay.test"]',
    yggNostrTags: () => JSON.stringify({ tags: ['t1'], since: 0 }),
    yggNostrOpen: opts.nostrOpen || (() => ({ error: 'no' })),
    yggNostrWrap: () => '',
  };
  if (opts.wasm) Object.assign(ctx, opts.wasm(addr)); // extra / replaced fake wasm exports
  ctx.window = ctx; ctx.globalThis = ctx; ctx.self = ctx;
  vm.createContext(ctx);
  vm.runInContext(SRC, ctx, { filename: 'shim.js' });
  const T = ctx.__yggTest;
  const settle = async (n = 5) => { const k = setInterval(() => {}, 1000); for (let i = 0; i < n; i++) await new Promise(r => setImmediate(r)); clearInterval(k); };
  const node = {
    addr, sent, events, errors, logged, kv, ctx, T,
    async ready() { const k = setInterval(() => {}, 1000); try { await T.ready; await settle(); } finally { clearInterval(k); } },
    get st() { return T.st(); },
    // deliver a raw datagram the way wasm does (synchronous callback); returns the thrown error, if any
    pkt(src, w) { const s = typeof w === 'string' ? w : JSON.stringify(w); try { onPacket(src, s); return null; } catch (e) { errors.push(e); return e; } },
    async api(ep, body) { const r = await ctx.fetch('https://yggchat.test/api/' + ep, body === undefined ? {} : { method: 'POST', body: JSON.stringify(body) }); const t = await r.text(); let j = null; try { j = JSON.parse(t); } catch {} return { status: r.status, text: t, json: j }; },
    settle, run: code => vm.runInContext(code, ctx),
  };
  const es = new ctx.EventSource("/api/events"); es.onmessage = e => events.push(JSON.parse(e.data));
  return node;
}
module.exports = { makeNode };
