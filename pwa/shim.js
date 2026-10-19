// YggChat PWA shim: runs a Yggdrasil node in WASM and emulates the native
// daemon's /api/* (see API.md) so the same UI works in the browser.
// The node, keys and history live only in this browser; the relay only
// forwards encrypted Yggdrasil link traffic and sealed mailbox boxes.
(() => {
  const RELAY = 'https://yggws.i2p.workers.dev';
  const MB = RELAY + '/mb/';
  const r = u => 'wss://yggws.i2p.workers.dev/r?u=' + encodeURIComponent(u);
  // Recommended: Hong Kong + Germany + Armenia peers from the official public-peers list, via relay.
  const RECOMMENDED = [
    r('ws://ygg5.mk16.de:1340?key=0000009611ae5391dc0aceea9f3fa6a0dc1279f4306059339e84bfb8b74d2f9b'),
    r('wss://ygg-evn-1.wgos.org:443'),
    r('ws://ygg1.mk16.de:1340?key=0000000087ee9949eeab56bd430ee8f324cad55abf3993ed9b9be63ce693e18a'),
  ];
  const LS = 'yggchat_pwa_v1';
  const MAXTTL = 4 * 7 * 86400;
  const now = () => Math.floor(Date.now() / 1000);

  // ---- persistent state ----
  // Outer record (always readable): key, peers, enc. Inner data (encrypted when
  // a passcode is set): nick, bio, avatar, contacts, messages, blocked.
  const raw = JSON.parse(localStorage.getItem(LS) || '{}');
  const outer = { key: raw.key || '', peers: raw.peers || null, enc: raw.enc || null };
  if (outer.peers && outer.peers.some(p => /workers\.dev\/(evn|dnct)$/.test(p))) outer.peers = null;
  if (!outer.peers) outer.peers = RECOMMENDED.slice();
  const blank = () => ({ nick: 'anon', bio: '', avatar: '', contacts: {}, messages: [], blocked: {} });
  let st = blank();
  let locked = !!outer.enc;
  let aesKey = null; // CryptoKey while unlocked with a passcode
  if (!locked) Object.assign(st, { nick: raw.nick || 'anon', bio: raw.bio || '', avatar: raw.avatar || '', contacts: raw.contacts || {}, messages: raw.messages || [], blocked: raw.blocked || {} });
  const migrate = () => { for (const m of st.messages) if (m.status === 'sending' || m.status === 'failed') m.status = 'queued'; };
  if (!locked) migrate();

  const b64 = u8 => btoa(String.fromCharCode(...new Uint8Array(u8)));
  const unb64 = s => Uint8Array.from(atob(s), c => c.charCodeAt(0));
  async function kdf(pass, salt) {
    const base = await crypto.subtle.importKey('raw', new TextEncoder().encode(pass), 'PBKDF2', false, ['deriveKey']);
    return crypto.subtle.deriveKey({ name: 'PBKDF2', salt, iterations: 310000, hash: 'SHA-256' }, base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  }
  let saving = Promise.resolve();
  function save() {
    if (locked) return;
    saving = saving.then(async () => {
      const rec = { key: outer.key, peers: outer.peers };
      if (aesKey) {
        const iv = crypto.getRandomValues(new Uint8Array(12));
        const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, aesKey, new TextEncoder().encode(JSON.stringify(st)));
        rec.enc = { v: 1, salt: outer.enc.salt, iv: b64(iv), ct: b64(ct) };
        outer.enc = rec.enc;
      } else Object.assign(rec, st);
      localStorage.setItem(LS, JSON.stringify(rec));
    }).catch(e => console.error('save', e));
  }
  const saveOuter = () => { const rec = JSON.parse(localStorage.getItem(LS) || '{}'); rec.key = outer.key; rec.peers = outer.peers; localStorage.setItem(LS, JSON.stringify(rec)); };

  let addr = '';
  const subs = new Set();
  const emit = (type, data) => { const s = JSON.stringify({ type, data }); subs.forEach(f => f(s)); };
  const acks = new Map();
  const cleanNick = n => String(n || '').replace(/[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, 32);
  const cleanText = (s, n) => String(s || '').replace(/[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, n);
  const send = (peer, w) => { try { yggSend(peer, JSON.stringify(w)); } catch {} };
  const myPK = () => { try { return yggPub(); } catch { return ''; } };

  function addMessage(m) {
    st.messages.push(m);
    if (st.messages.length > 5000) st.messages.splice(0, st.messages.length - 5000);
    let c = st.contacts[m.peer];
    if (!c) { c = st.contacts[m.peer] = { addr: m.peer, nick: m.out ? '' : (m.nick || ''), req: !m.out, shared: !!m.out, ttl: m.ttl || 0 }; emit('contact', c); }
    else if (!m.out && m.nick && !c.renamed && c.nick !== m.nick) { c.nick = m.nick; emit('contact', c); }
    save();
    emit('message', m);
  }

  function setStatus(peer, id, status) {
    const m = st.messages.find(x => x.peer === peer && x.id === id);
    if (m) { m.status = status; save(); }
    emit('status', { id, peer, status });
  }

  // ---- profile sharing, disappearing timer ----
  const profWire = () => ({ t: 'prof', nick: st.nick, bio: st.bio, av: st.avatar, pk: myPK() });
  function sendProf(p) { const w = profWire(); for (let i = 0; i < 3; i++) setTimeout(() => send(p, w), i * 1500); }
  const profAt = {};
  function maybeProf(p) { const c = st.contacts[p]; if (!c || !c.shared) return; if (Date.now() - (profAt[p] || 0) < 600000) return; profAt[p] = Date.now(); sendProf(p); }
  function setTTL(p, ttl, byMe) {
    ttl = Math.max(0, Math.min(MAXTTL, ttl | 0));
    const c = st.contacts[p]; if (!c || (c.ttl || 0) === ttl) return;
    c.ttl = ttl;
    const m = { id: 's' + Date.now().toString(16) + Math.random().toString(16).slice(2, 5), peer: p, sys: 'ttl:' + ttl, ts: now(), out: !!byMe };
    st.messages.push(m); save(); emit('contact', c); emit('message', m);
    if (byMe) for (let i = 0; i < 3; i++) setTimeout(() => send(p, { t: 'ttl', ttl }), i * 1500);
  }
  function learnPK(from, pk) {
    const c = st.contacts[from];
    if (c && pk && c.pk !== pk && /^[0-9a-f]{64}$/.test(pk) && yggAddrOK(from, pk)) { c.pk = pk; save(); }
  }

  // ---- offline queue ----
  const inflight = new Set();
  const queuedFor = p => st.messages.filter(m => m.out && m.peer === p && (m.status === 'queued' || m.status === 'stored'));
  async function flush(p) { for (const m of queuedFor(p)) { if (!(await deliver(m, 4))) break; } }
  const hello = p => send(p, { t: 'hi', pk: myPK() });
  function helloAll() { if (!locked) for (const p of Object.keys(st.contacts)) hello(p); }

  function handleMsg(src, w) {
    if (!w.text || w.text.length > 8000 || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return false;
    if (locked || st.blocked[src]) return false;
    if (st.messages.some(m => m.peer === src && m.id === w.id)) return true;
    let ttl = w.ttl | 0; if (ttl < 0 || ttl > MAXTTL) ttl = 0;
    if (st.contacts[src] && (st.contacts[src].ttl || 0) !== ttl) setTTL(src, ttl, false);
    const m = { id: w.id, peer: src, from: src, nick: cleanNick(w.nick), text: w.text, ts: w.ts || now(), out: false };
    if (ttl) { m.ttl = ttl; m.exp = now() + ttl; }
    addMessage(m);
    return true;
  }

  function onPacket(src, json) {
    let w; try { w = JSON.parse(json) } catch { return }
    if (w.t === 'ack') { const f = acks.get(w.id); if (f) { acks.delete(w.id); f(); } return; }
    if (locked || st.blocked[src]) return; // stay silent: the sender keeps it queued
    if (w.pk) learnPK(src, String(w.pk));
    if (w.t === 'hi' || w.t === 'hiack') { if (w.t === 'hi') send(src, { t: 'hiack', pk: myPK() }); maybeProf(src); flush(src); return; }
    if (w.t === 'prof') {
      const c = st.contacts[src]; if (!c) return;
      const n = cleanNick(w.nick); if (n && !c.renamed) c.nick = n;
      c.bio = cleanText(w.bio, 140); c.avatar = cleanText(w.av, 8); c.hasprof = true; save(); emit('contact', c); return;
    }
    if (w.t === 'ttl') { setTTL(src, w.ttl | 0, false); return; }
    if (w.t === 'msg' && handleMsg(src, w)) { send(src, { t: 'ack', id: w.id }); if (queuedFor(src).length) flush(src); }
  }

  // ---- mailbox ----
  async function mbPost(op, body) {
    const res = await fetch(MB + op, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) });
    if (!res.ok) throw new Error('mailbox ' + op + ' ' + res.status);
    return res.json();
  }
  function mbAuth(op) { const pk = myPK(), tag = yggTag(pk), ts = now(); return { pk, ts, sig: yggSign('yggchat-mb-' + op + ':' + tag + ':' + ts) }; }
  async function mbPut(m) {
    const c = st.contacts[m.peer]; if (!c || !c.pk) return false;
    const box = yggSeal(c.pk, JSON.stringify({ t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts, ttl: m.ttl || undefined }));
    if (!box) return false;
    try { await mbPost('put', { to: yggTag(c.pk), box }); return true; } catch { return false; }
  }
  let fetching = false;
  async function mbFetch() {
    if (locked || fetching || !addr) return; fetching = true;
    try {
      const res = await mbPost('get', mbAuth('get'));
      const keys = [];
      for (const bx of res.boxes || []) {
        keys.push(bx.k);
        const o = yggOpen(bx.box); if (o.error) continue;
        let w; try { w = JSON.parse(o.w) } catch { continue }
        if (w.t !== 'msg') continue;
        learnPK(o.from, o.pk);
        if (handleMsg(o.from, w)) send(o.from, { t: 'ack', id: w.id });
      }
      if (keys.length) await mbPost('del', { ...mbAuth('del'), keys });
    } catch (e) { console.warn(e) } finally { fetching = false; }
  }

  async function deliver(m, tries = 12) {
    if (inflight.has(m.id)) return false;
    inflight.add(m.id);
    if (m.status !== 'sending') setStatus(m.peer, m.id, 'sending');
    const pkt = JSON.stringify({ t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts, ttl: m.ttl || undefined });
    const acked = new Promise(r => acks.set(m.id, r));
    const wasStored = m.status === 'stored';
    try {
      for (let i = 0; i < tries; i++) {
        yggSend(m.peer, pkt);
        const got = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (got) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id);
      setStatus(m.peer, m.id, (wasStored || await mbPut(m)) ? 'stored' : 'queued');
      return false;
    } finally { inflight.delete(m.id); }
  }

  // ---- disappearing sweep ----
  setInterval(() => {
    if (locked) return;
    const t = now(), gone = st.messages.filter(m => m.exp && m.exp <= t);
    if (!gone.length) return;
    st.messages = st.messages.filter(m => !(m.exp && m.exp <= t)); save();
    for (const m of gone) emit('msg_del', { peer: m.peer, id: m.id });
  }, 2000);

  const ready = (async () => {
    const go = new Go();
    const wasmReady = new Promise(r => addEventListener('yggready', r, { once: true }));
    const res = await WebAssembly.instantiateStreaming(fetch('ygg.wasm'), go.importObject);
    go.run(res.instance);
    await wasmReady;
    const r = yggStart(outer.key, outer.peers, onPacket);
    if (r.error) throw new Error(r.error);
    addr = r.addr; outer.key = r.key; saveOuter();
    let last = -1;
    setInterval(() => {
      const n = yggPeers();
      if (n !== last) { if (n > 0 && last <= 0) setTimeout(helloAll, 1500); last = n; emit('peers', { up: n }); }
    }, 3000);
    // Probe peers we still owe messages to, plus everyone now and then so they can flush to us.
    setInterval(() => { if (yggPeers() > 0 && !locked) for (const p of new Set(st.messages.filter(m => m.out && (m.status === 'queued' || m.status === 'stored')).map(m => m.peer))) hello(p); }, 20000);
    setInterval(() => { if (yggPeers() > 0) helloAll(); }, 120000);
    setInterval(mbFetch, 300000);
    setTimeout(mbFetch, 2000);
    addEventListener('online', () => setTimeout(() => { helloAll(); mbFetch(); }, 3000));
    document.addEventListener('visibilitychange', () => { if (!document.hidden) { if (yggPeers() > 0) helloAll(); mbFetch(); } });
  })();

  const norm = a => { try { return new URL('http://[' + a.trim() + ']/').hostname.slice(1, -1) } catch { return null } };
  const isYgg = a => !!a && /^0?[23][0-9a-f]{0,2}:/.test(a);
  const ok = d => new Response(JSON.stringify(d), { status: 200, headers: { 'Content-Type': 'application/json' } });
  const bad = (msg, code = 400) => new Response(msg, { status: code });

  function peerList() {
    const live = yggPeerList();
    const seen = new Set(live.map(p => p.uri));
    const saved = new Set(outer.peers);
    return live.map(p => ({ ...p, saved: saved.has(p.uri) }))
      .concat(outer.peers.filter(p => !seen.has(p)).map(uri => ({ uri, up: false, saved: true })));
  }

  const OPEN_WHEN_LOCKED = new Set(['state', 'unlock', 'peers', 'peers/public']);
  async function api(ep, body) {
    if (locked && !OPEN_WHEN_LOCKED.has(ep)) return bad('locked', 423);
    switch (ep) {
      case 'state':
        if (locked) return ok({ locked: true, addr, peers: yggPeers(), platform: 'web' });
        return ok({ addr, pubkey: myPK(), nick: st.nick, bio: st.bio, avatar: st.avatar, contacts: st.contacts, messages: st.messages, peers: yggPeers(), platform: 'web', locked: false, haspass: !!aesKey, blocked: Object.keys(st.blocked) });
      case 'unlock': {
        if (!locked) return ok({});
        try {
          const k = await kdf(String(body.Pass || ''), unb64(outer.enc.salt));
          const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(outer.enc.iv) }, k, unb64(outer.enc.ct));
          st = Object.assign(blank(), JSON.parse(new TextDecoder().decode(plain)));
          aesKey = k; locked = false; migrate();
          setTimeout(() => { helloAll(); mbFetch(); }, 500);
          return ok({});
        } catch { await new Promise(r => setTimeout(r, 700)); return bad('パスコードが違います', 403); }
      }
      case 'passcode': {
        if (aesKey) {
          try { const k = await kdf(String(body.Old || ''), unb64(outer.enc.salt)); await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(outer.enc.iv) }, k, unb64(outer.enc.ct)); }
          catch { return bad('現在のパスコードが違います'); }
        }
        const nw = String(body.New || '');
        if (!nw) { aesKey = null; outer.enc = null; }
        else {
          if ([...nw].length < 4) return bad('パスコードは4文字以上にしてください');
          const salt = crypto.getRandomValues(new Uint8Array(16));
          aesKey = await kdf(nw, salt); outer.enc = { salt: b64(salt) };
        }
        save(); await saving; return ok({});
      }
      case 'lock': {
        if (!aesKey) return bad('パスコードが未設定です');
        save(); await saving;
        locked = true; aesKey = null; st = blank();
        emit('locked', { locked: true });
        return ok({});
      }
      case 'send': {
        if (body.Retry) {
          const m = st.messages.find(x => x.peer === norm(body.To || '') && x.id === body.Retry && x.out);
          if (!m) return bad('no such message', 404);
          setStatus(m.peer, m.id, 'sending'); deliver(m); return ok(m);
        }
        const dst = norm(body.To || '');
        if (!isYgg(dst)) return bad('not a Yggdrasil address: ' + body.To);
        if (!body.Text || body.Text.length > 8000) return bad('message empty or too long');
        const c = st.contacts[dst];
        const share = !c || !c.shared;
        if (c && (c.req || !c.shared)) { c.req = false; c.shared = true; emit('contact', c); }
        const ttl = (c && c.ttl) || 0;
        const m = { id: Date.now().toString(16) + Math.random().toString(16).slice(2, 6), peer: dst, from: addr, nick: st.nick, text: body.Text, ts: now(), out: true, status: 'sending' };
        if (ttl) { m.ttl = ttl; m.exp = m.ts + ttl; }
        addMessage(m); if (share) sendProf(dst); deliver(m); return ok(m);
      }
      case 'contact': {
        const a = norm(body.Addr || ''); if (!isYgg(a)) return bad('not a Yggdrasil address');
        const exists = !!st.contacts[a];
        const c = st.contacts[a] || (st.contacts[a] = { addr: a, nick: '' });
        if (body.Nick != null) { c.nick = cleanNick(body.Nick); c.renamed = exists; }
        const share = body.Accept && (c.req || !c.shared);
        if (body.Accept) { c.req = false; c.shared = true; }
        delete st.blocked[a];
        save(); emit('contact', c);
        if (share) sendProf(a);
        if (body.TTL != null) setTTL(a, body.TTL, true);
        return ok(c);
      }
      case 'contact/delete': {
        const a = norm(body.Addr || ''); if (!a) return bad('bad address');
        delete st.contacts[a]; st.messages = st.messages.filter(m => m.peer !== a); save(); emit('contact_del', { addr: a }); return ok({});
      }
      case 'block': {
        const a = norm(body.Addr || ''); if (!a) return bad('bad address');
        if (body.Block) { st.blocked[a] = true; delete st.contacts[a]; st.messages = st.messages.filter(m => m.peer !== a); emit('contact_del', { addr: a }); }
        else delete st.blocked[a];
        save(); return ok({});
      }
      case 'nick': st.nick = cleanNick(body.Nick) || 'anon'; save(); for (const p in st.contacts) if (st.contacts[p].shared) sendProf(p); return ok({});
      case 'profile': {
        st.nick = cleanNick(body.Nick) || 'anon'; st.bio = cleanText(body.Bio, 140); st.avatar = cleanText(body.Avatar, 8);
        save(); for (const p in st.contacts) if (st.contacts[p].shared) sendProf(p); return ok({});
      }
      case 'identity/reset': {
        if (!body.Confirm) return bad('confirm required');
        outer.key = ''; st.contacts = {}; st.messages = []; save(); await saving; saveOuter();
        setTimeout(() => location.reload(), 300);
        return ok({});
      }
      case 'peers': {
        if (body.Reset) { for (const p of outer.peers) if (!RECOMMENDED.includes(p)) yggRemovePeer(p); for (const p of RECOMMENDED) if (!outer.peers.includes(p)) yggAddPeer(p); outer.peers = RECOMMENDED.slice(); }
        else if (body.Add) {
          let u = body.Add.trim();
          if (/^wss?:\/\//.test(u) && !u.startsWith('wss://yggws.')) u = r(u); // browsers need the relay
          if (!/^wss:\/\//.test(u)) return bad('browsers can only use ws/wss peers');
          if (!outer.peers.includes(u)) { const e = yggAddPeer(u); if (e) return bad(e); outer.peers.push(u); }
        } else if (body.Remove) { yggRemovePeer(body.Remove); outer.peers = outer.peers.filter(p => p !== body.Remove); }
        saveOuter();
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
