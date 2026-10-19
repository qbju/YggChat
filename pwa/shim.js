// YggChat PWA shim: runs a Yggdrasil node in WASM and emulates the native
// daemon's /api/* (see API.md) so the same UI works in the browser.
(() => {
  const RELAY = 'https://yggws.i2p.workers.dev';
  const r = u => 'wss://yggws.i2p.workers.dev/r?u=' + encodeURIComponent(u);
  // Recommended: Hong Kong + Germany + Armenia peers from the official public-peers list, via relay.
  const RECOMMENDED = [
    r('ws://ygg5.mk16.de:1340?key=0000009611ae5391dc0aceea9f3fa6a0dc1279f4306059339e84bfb8b74d2f9b'),
    r('wss://ygg-evn-1.wgos.org:443'),
    r('ws://ygg1.mk16.de:1340?key=0000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a'),
  ];
  const LS = 'yggchat_pwa_v1';
  const st = Object.assign({ key: '', nick: 'anon', contacts: {}, messages: [], peers: null }, JSON.parse(localStorage.getItem(LS) || '{}'));
  // Old builds stored the legacy relay aliases; move them to the recommended set.
  if (st.peers && st.peers.some(p => /workers\.dev\/(evn|dnct)$/.test(p))) st.peers = null;
  if (!st.peers) st.peers = RECOMMENDED.slice();
  for (const m of st.messages) if (m.status === 'sending' || m.status === 'failed') m.status = 'queued';
  const save = () => localStorage.setItem(LS, JSON.stringify(st));
  let addr = '';
  const subs = new Set();
  const emit = (type, data) => { const s = JSON.stringify({ type, data }); subs.forEach(f => f(s)); };
  const acks = new Map();

  function addMessage(m) {
    st.messages.push(m);
    if (st.messages.length > 5000) st.messages.splice(0, st.messages.length - 5000);
    const c = st.contacts[m.peer];
    if (!c) { st.contacts[m.peer] = { addr: m.peer, nick: m.nick }; emit('contact', st.contacts[m.peer]); }
    else if (!m.out && m.nick && c.nick !== m.nick) { c.nick = m.nick; emit('contact', c); }
    save();
    emit('message', m);
  }

  function setStatus(peer, id, status) {
    const m = st.messages.find(x => x.peer === peer && x.id === id);
    if (m) { m.status = status; save(); }
    emit('status', { id, peer, status });
  }

  // Offline queue: undelivered messages stay "queued" and are re-sent automatically
  // whenever the peer shows signs of life (any packet from it, or a hi/hiack probe).
  const inflight = new Set();
  const queuedFor = p => st.messages.filter(m => m.out && m.peer === p && m.status === 'queued');
  async function flush(p) {
    for (const m of queuedFor(p)) { if (!(await deliver(m, 4))) break; }
  }
  const hello = p => yggSend(p, JSON.stringify({ t: 'hi' }));
  function helloAll() { for (const p of Object.keys(st.contacts)) hello(p); }

  function onPacket(src, json) {
    let w; try { w = JSON.parse(json) } catch { return }
    if (w.t === 'hi') { yggSend(src, JSON.stringify({ t: 'hiack' })); flush(src); return; }
    if (w.t === 'hiack') { flush(src); return; }
    if (w.t === 'ack') { const f = acks.get(w.id); if (f) { acks.delete(w.id); f(); } return; }
    if (w.t !== 'msg' || !w.text || w.text.length > 8000 || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return;
    w.nick = String(w.nick || '').replace(/[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, 32);
    yggSend(src, JSON.stringify({ t: 'ack', id: w.id }));
    if (queuedFor(src).length) flush(src);
    if (st.messages.some(m => m.peer === src && m.id === w.id)) return;
    addMessage({ id: w.id, peer: src, from: src, nick: w.nick || '', text: w.text, ts: w.ts || Math.floor(Date.now() / 1000), out: false });
  }

  const ready = (async () => {
    const go = new Go();
    const wasmReady = new Promise(r => addEventListener('yggready', r, { once: true }));
    const res = await WebAssembly.instantiateStreaming(fetch('ygg.wasm'), go.importObject);
    go.run(res.instance);
    await wasmReady;
    const r = yggStart(st.key, st.peers, onPacket);
    if (r.error) throw new Error(r.error);
    addr = r.addr; st.key = r.key; save();
    let last = -1;
    setInterval(() => {
      const n = yggPeers();
      if (n !== last) { if (n > 0 && last <= 0) setTimeout(helloAll, 1500); last = n; emit('peers', { up: n }); }
    }, 3000);
    // Probe peers we still owe messages to, plus everyone now and then so they can flush to us.
    setInterval(() => { if (yggPeers() > 0) for (const p of new Set(st.messages.filter(m => m.out && m.status === 'queued').map(m => m.peer))) hello(p); }, 20000);
    setInterval(() => { if (yggPeers() > 0) helloAll(); }, 120000);
    addEventListener('online', () => setTimeout(helloAll, 3000));
    document.addEventListener('visibilitychange', () => { if (!document.hidden && yggPeers() > 0) helloAll(); });
  })();

  const norm = a => { try { return new URL('http://[' + a.trim() + ']/').hostname.slice(1, -1) } catch { return null } };
  const isYgg = a => !!a && /^0?[23][0-9a-f]{0,2}:/.test(a);
  const ok = d => new Response(JSON.stringify(d), { status: 200, headers: { 'Content-Type': 'application/json' } });
  const bad = (msg, code = 400) => new Response(msg, { status: code });

  async function deliver(m, tries = 8) {
    if (inflight.has(m.id)) return false;
    inflight.add(m.id);
    if (m.status !== 'sending') setStatus(m.peer, m.id, 'sending');
    const pkt = JSON.stringify({ t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts });
    const acked = new Promise(r => acks.set(m.id, r));
    try {
      for (let i = 0; i < tries; i++) {
        yggSend(m.peer, pkt);
        const got = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (got) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id);
      setStatus(m.peer, m.id, 'queued');
      return false;
    } finally { inflight.delete(m.id); }
  }

  function peerList() {
    const live = yggPeerList();
    const seen = new Set(live.map(p => p.uri));
    const saved = new Set(st.peers);
    return live.map(p => ({ ...p, saved: saved.has(p.uri) }))
      .concat(st.peers.filter(p => !seen.has(p)).map(uri => ({ uri, up: false, saved: true })));
  }

  async function api(ep, body) {
    switch (ep) {
      case 'state': return ok({ addr, pubkey: '', nick: st.nick, contacts: st.contacts, messages: st.messages, peers: yggPeers(), platform: 'web' });
      case 'send': {
        if (body.Retry) {
          const m = st.messages.find(x => x.peer === norm(body.To || '') && x.id === body.Retry && x.out);
          if (!m) return bad('no such message', 404);
          setStatus(m.peer, m.id, 'sending'); deliver(m); return ok(m);
        }
        const dst = norm(body.To || '');
        if (!isYgg(dst)) return bad('not a Yggdrasil address: ' + body.To);
        if (!body.Text || body.Text.length > 8000) return bad('message empty or too long');
        const m = { id: Date.now().toString(16) + Math.random().toString(16).slice(2, 6), peer: dst, from: addr, nick: st.nick, text: body.Text, ts: Math.floor(Date.now() / 1000), out: true, status: 'sending' };
        addMessage(m); deliver(m); return ok(m);
      }
      case 'contact': {
        const a = norm(body.Addr || ''); if (!isYgg(a)) return bad('not a Yggdrasil address');
        const c = { addr: a, nick: body.Nick || '' }; st.contacts[a] = c; save(); emit('contact', c); return ok(c);
      }
      case 'contact/delete': {
        const a = norm(body.Addr || ''); if (!a) return bad('bad address');
        delete st.contacts[a]; st.messages = st.messages.filter(m => m.peer !== a); save(); emit('contact_del', { addr: a }); return ok({});
      }
      case 'nick': st.nick = (body.Nick || '').trim() || 'anon'; save(); return ok({});
      case 'peers': {
        if (body.Reset) { for (const p of st.peers) if (!RECOMMENDED.includes(p)) yggRemovePeer(p); for (const p of RECOMMENDED) if (!st.peers.includes(p)) yggAddPeer(p); st.peers = RECOMMENDED.slice(); }
        else if (body.Add) {
          let u = body.Add.trim();
          if (/^wss?:\/\//.test(u) && !u.startsWith('wss://yggws.')) u = r(u); // browsers need the relay
          if (!/^wss:\/\//.test(u)) return bad('browsers can only use ws/wss peers');
          if (!st.peers.includes(u)) { const e = yggAddPeer(u); if (e) return bad(e); st.peers.push(u); }
        } else if (body.Remove) { yggRemovePeer(body.Remove); st.peers = st.peers.filter(p => p !== body.Remove); }
        save();
        return ok({ peers: peerList(), recommended: RECOMMENDED, up: yggPeers() });
      }
      case 'peers/public': {
        const x = await fetch(RELAY + '/list').then(r => r.json());
        return ok({ peers: x.peers.map(p => ({ uri: p.relay, label: p.uri, region: p.region, continent: p.continent, asia: p.continent === 'asia' })) });
      }
    }
    return bad('not found', 404);
  }

  const realFetch = fetch.bind(window);
  window.fetch = async (input, init = {}) => {
    const u = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (u.origin !== location.origin || !u.pathname.includes('/api/')) return realFetch(input, init);
    try { await ready } catch (e) { return bad('node failed: ' + e.message, 500) }
    let body = {};
    try { body = init.body ? JSON.parse(init.body) : {} } catch { return bad('bad json') }
    try { return await api(u.pathname.split('/api/')[1], body) } catch (e) { return bad(String(e.message || e), 500) }
  };

  const RealES = window.EventSource;
  window.EventSource = function (url, opts) {
    if (!String(url).includes('/api/events')) return new RealES(url, opts);
    const es = { readyState: 1, onopen: null, onmessage: null, onerror: null, addEventListener() {}, close() { subs.delete(f) } };
    const f = s => es.onmessage && es.onmessage({ data: s });
    subs.add(f);
    setTimeout(() => es.onopen && es.onopen({}), 0);
    return es;
  };
})();
