// YggChat PWA shim: runs a Yggdrasil node in WASM and emulates the native
// daemon's /api/* (see API.md) so the same UI works in the browser.
// The node, keys and history live only in this browser; it peers directly
// with public wss Yggdrasil peers that accept this page's Origin (no relay).
(() => {
  // No third-party relay: browsers connect straight to Yggdrasil peers whose
  // WebSocket listener allows this page's Origin (yggdrasil-go "?origin=").
  // As of 2026-10-19 the only public peer found that accepts this page's Origin.
  const RECOMMENDED = ['wss://ygg.being.pet/ygg'];
  const LABELS = { [RECOMMENDED[0]]: 'ygg.being.pet（公開・ブラウザ可）' };
  const LS = 'yggchat_pwa_v1';
  const MAXTTL = 4 * 7 * 86400;
  const now = () => Math.floor(Date.now() / 1000);

  // ---- persistent state ----
  // Outer record (always readable): key, peers, enc. Inner data (encrypted when
  // a passcode is set): nick, bio, avatar, contacts, messages, blocked.
  // Stored in IndexedDB (images need more room than localStorage's ~5 MB);
  // older versions kept everything in localStorage and are migrated once.
  const idb = new Promise((res, rej) => {
    const q = indexedDB.open('yggchat', 1);
    q.onupgradeneeded = () => q.result.createObjectStore('kv');
    q.onsuccess = () => res(q.result); q.onerror = () => rej(q.error);
  });
  const idbGet = k => idb.then(db => new Promise((res, rej) => { const q = db.transaction('kv').objectStore('kv').get(k); q.onsuccess = () => res(q.result); q.onerror = () => rej(q.error); }));
  const idbPut = (k, v) => idb.then(db => new Promise((res, rej) => { const t = db.transaction('kv', 'readwrite'); t.objectStore('kv').put(v, k); t.oncomplete = res; t.onerror = () => rej(t.error); }));
  let raw = {};
  const outer = { key: '', peers: null, enc: null };
  const blank = () => ({ nick: 'anon', bio: '', avatar: '', contacts: {}, messages: [], blocked: {}, nostrOn: false, nostrRelays: [] });
  let st = blank();
  let locked = false;
  let aesKey = null; // CryptoKey while unlocked with a passcode
  const migrate = () => { for (const m of st.messages) if (m.status === 'sending' || m.status === 'failed') m.status = 'queued'; };
  async function loadStore() {
    raw = (await idbGet('state').catch(() => null)) || null;
    if (!raw) {
      raw = JSON.parse(localStorage.getItem(LS) || '{}');
      if (Object.keys(raw).length) { await idbPut('state', raw); localStorage.removeItem(LS); }
    }
    Object.assign(outer, { key: raw.key || '', peers: raw.peers || null, enc: raw.enc || null });
    if (outer.peers && outer.peers.some(p => /workers\.dev|instabox\.tech/.test(p))) outer.peers = null; // old Cloudflare relay / removed entry peer -> defaults
    if (!outer.peers) outer.peers = RECOMMENDED.slice();
    locked = !!outer.enc;
    if (!locked) { Object.assign(st, { nick: raw.nick || 'anon', bio: raw.bio || '', avatar: raw.avatar || '', contacts: raw.contacts || {}, messages: raw.messages || [], blocked: raw.blocked || {}, nostrOn: !!raw.nostrOn, nostrRelays: raw.nostrRelays || [] }); migrate(); }
  }

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
      raw = rec;
      await idbPut('state', rec);
    }).catch(e => console.error('save', e));
  }
  const saveOuter = () => { saving = saving.then(() => { raw = Object.assign({}, raw, { key: outer.key, peers: outer.peers }); return idbPut('state', raw); }).catch(e => console.error('save', e)); };

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

  // Greet a contact whose full key we lack: hi/hiack carry both keys (needed for Nostr boxes).
  const askPK = p => { const c = st.contacts[p]; if (c && !c.pk && !locked) hello(p); };
  // ---- offline queue ----
  const inflight = new Set();
  const queuedFor = p => st.messages.filter(m => m.out && m.peer === p && (m.status === 'queued' || m.status === 'stored'));
  async function flush(p) { for (const m of queuedFor(p)) { if (!(await deliver(m, 4))) break; } }
  const hello = p => send(p, { t: 'hi', pk: myPK() });
  function helloAll() { if (!locked) for (const p of Object.keys(st.contacts)) hello(p); }

  const handleMsg = (src, w) => !!w.text && acceptMsg(src, w, '');
  function acceptMsg(src, w, img) {
    if ((!w.text && !img) || String(w.text || '').length > 8000 || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return false;
    if (locked || st.blocked[src]) return false;
    if (st.messages.some(m => m.peer === src && m.id === w.id)) return true;
    let ttl = w.ttl | 0; if (ttl < 0 || ttl > MAXTTL) ttl = 0;
    if (st.contacts[src] && (st.contacts[src].ttl || 0) !== ttl) setTTL(src, ttl, false);
    const m = { id: w.id, peer: src, from: src, nick: cleanNick(w.nick), text: w.text || '', ts: w.ts || now(), out: false };
    if (img) m.img = img;
    if (ttl) { m.ttl = ttl; m.exp = now() + ttl; }
    addMessage(m);
    return true;
  }

  function onPacket(src, json) {
    let w; try { w = JSON.parse(json) } catch { return }
    if (w.t === 'fca') { const g = chunkAcks.get(w.id); if (g) g.add(w.i | 0); return; }
    if (w.t === 'ack') { const f = acks.get(w.id); if (f) { acks.delete(w.id); f(); } askPK(src); return; }
    if (locked || st.blocked[src]) return; // stay silent: the sender keeps it queued
    if (w.pk) learnPK(src, String(w.pk));
    if (w.t === 'hi' || w.t === 'hiack') { if (w.t === 'hi') send(src, { t: 'hiack', pk: myPK() }); maybeProf(src); flush(src); return; }
    if (w.t === 'prof') {
      const c = st.contacts[src]; if (!c) return;
      const n = cleanNick(w.nick); if (n && !c.renamed) c.nick = n;
      c.bio = cleanText(w.bio, 140); c.avatar = cleanText(w.av, 8); c.hasprof = true; save(); emit('contact', c); return;
    }
    if (w.t === 'ttl') { setTTL(src, w.ttl | 0, false); return; }
    if (w.t === 'fh') { onFileHeader(src, w); return; }
    if (w.t === 'fc') { onFileChunk(src, w); return; }
    if (w.t === 'msg' && handleMsg(src, w)) { send(src, { t: 'ack', id: w.id }); askPK(src); if (queuedFor(src).length) flush(src); }
  }

  // ---- images: header + 12 KiB chunks, per-chunk acks, SHA-256 check ----
  // Each datagram must fit a 1280-byte IPv6 packet (no fragmentation here).
  const CHUNK = 864, IMGMAX = 600 * 1024, MAXN = Math.ceil(IMGMAX / CHUNK), CAPMAX = 800; // caption bytes
  const u8len = t => new TextEncoder().encode(String(t || '')).length;
  const MIMES = new Set(['image/jpeg', 'image/png', 'image/webp', 'image/gif']);
  const hexOf = b => [...new Uint8Array(b)].map(x => x.toString(16).padStart(2, '0')).join('');
  const sha = async u8 => hexOf(await crypto.subtle.digest('SHA-256', u8));
  function parseImg(u) {
    const m = /^data:(image\/(?:jpeg|png|webp|gif));base64,([A-Za-z0-9+/=]+)$/.exec(String(u || ''));
    if (!m) return null;
    const bytes = unb64(m[2]);
    return bytes.length && bytes.length <= IMGMAX ? { mime: m[1], bytes } : null;
  }
  const chunkAcks = new Map();
  const inFiles = new Map();
  async function deliverImg(m, tries) {
    const p = parseImg(m.img); if (!p) { setStatus(m.peer, m.id, 'failed'); return false; }
    const n = Math.ceil(p.bytes.length / CHUNK);
    const hdr = { t: 'fh', id: m.id, nick: m.nick, text: m.text || undefined, ts: m.ts, ttl: m.ttl || undefined, mime: p.mime, size: p.bytes.length, n, sha: await sha(p.bytes) };
    const got = new Set(); chunkAcks.set(m.id, got);
    const acked = new Promise(r => acks.set(m.id, r));
    let done = false; acked.then(() => done = true);
    try {
      let idle = 0, last = -1;
      while (idle < tries) {
        send(m.peer, hdr);
        if (got.size === last) idle++; else { idle = 0; last = got.size; }
        await new Promise(r => setTimeout(r, 30));
        for (let i = 0; i < n && !done; i++) if (!got.has(i)) { send(m.peer, { t: 'fc', id: m.id, i, d: b64(p.bytes.subarray(i * CHUNK, (i + 1) * CHUNK)) }); if (i % 8 === 7) await new Promise(r => setTimeout(r, 4)); }
        const ok = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (ok) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id);
      setStatus(m.peer, m.id, 'queued'); // images don't go through the mailbox
      return false;
    } finally { chunkAcks.delete(m.id); }
  }
  function onFileHeader(src, w) {
    const n = w.n | 0, size = w.size | 0;
    if (!/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id)) || !MIMES.has(w.mime) || n < 1 || n > MAXN || size < 1 || size > IMGMAX || Math.ceil(size / CHUNK) !== n || !/^[0-9a-f]{64}$/.test(String(w.sha)) || u8len(w.text) > CAPMAX || u8len(w.nick) > 120) return;
    if (st.messages.some(m => m.peer === src && m.id === w.id)) { send(src, { t: 'ack', id: w.id }); return; }
    const t = Date.now();
    for (const [k, f] of inFiles) if (t - f.at > 180000) inFiles.delete(k);
    const key = src + '/' + w.id;
    if (inFiles.has(key) || inFiles.size >= 16) return;
    inFiles.set(key, { hdr: w, chunks: new Array(n), got: 0, at: t });
  }
  async function onFileChunk(src, w) {
    const key = src + '/' + w.id, f = inFiles.get(key), i = w.i | 0;
    if (!f || i < 0 || i >= f.hdr.n) return;
    if (!f.chunks[i]) {
      let b; try { b = unb64(String(w.d || '')) } catch { return }
      if (!b.length || b.length > CHUNK) return;
      f.chunks[i] = b; f.got++; f.at = Date.now();
    }
    send(src, { t: 'fca', id: w.id, i });
    if (f.got !== f.hdr.n || f.done) return;
    f.done = true; inFiles.delete(key);
    const all = new Uint8Array(f.hdr.size); let off = 0;
    for (const c of f.chunks) { if (off + c.length > all.length) return; all.set(c, off); off += c.length; }
    if (off !== f.hdr.size || await sha(all) !== f.hdr.sha) return;
    let s = ''; for (let j = 0; j < all.length; j += 0x8000) s += String.fromCharCode(...all.subarray(j, j + 0x8000));
    if (acceptMsg(src, f.hdr, 'data:' + f.hdr.mime + ';base64,' + btoa(s))) send(src, { t: 'ack', id: w.id });
  }

  async function deliver(m, tries = 12) {
    if (inflight.has(m.id)) return false;
    inflight.add(m.id);
    if (m.status !== 'sending') setStatus(m.peer, m.id, 'sending');
    if (m.img) { try { return await deliverImg(m, tries); } finally { inflight.delete(m.id); } }
    const pkt = JSON.stringify({ t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts, ttl: m.ttl || undefined });
    const acked = new Promise(r => acks.set(m.id, r));
    try {
      for (let i = 0; i < tries; i++) {
        yggSend(m.peer, pkt);
        const got = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (got) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id);
      setStatus(m.peer, m.id, m.nostr ? 'stored' : 'queued');
      if (!m.nostr) nostrPark(m);
      return false;
    } finally { inflight.delete(m.id); }
  }

  // ---- optional offline delivery over public Nostr relays (deprecated, opt-in) ----
  // Undeliverable text is sealed to the peer's key (only they can open it; the
  // sender is inside the box) and posted as a Nostr event signed by a
  // throw-away key, tagged with a hash of the peer's key and a 7-day NIP-40
  // expiry. The recipient pulls it, opens it, acks directly and publishes the
  // NIP-09 deletion. Relays still see the tag, timing and IP addresses.
  const nDefaults = () => { try { return JSON.parse(yggNostrDefaults()); } catch { return []; } };
  const nRelays = () => (!locked && st.nostrOn) ? (st.nostrRelays && st.nostrRelays.length ? st.nostrRelays.slice() : nDefaults()) : null;
  const nostrState = () => ({ on: !!st.nostrOn, relays: st.nostrRelays && st.nostrRelays.length ? st.nostrRelays : nDefaults(), defaults: nDefaults() });
  function relayTalk(url, first, onMsg, ms) {
    return new Promise(res => {
      let ws, done = false;
      const fin = v => { if (done) return; done = true; clearTimeout(t); try { ws && ws.close(); } catch {} res(v); };
      const t = setTimeout(() => fin(undefined), ms);
      try { ws = new WebSocket(url); } catch { return fin(undefined); }
      ws.onopen = () => ws.send(first);
      ws.onmessage = e => { let m; try { m = JSON.parse(e.data); } catch { return; } const v = onMsg(m); if (v !== undefined) fin(v); };
      ws.onerror = () => fin(undefined);
      ws.onclose = () => fin(undefined);
    });
  }
  const relayPublish = (url, ev) => { const id = JSON.parse(ev).id; return relayTalk(url, '["EVENT",' + ev + ']', m => (m[0] === 'OK' && m[1] === id) ? m[2] === true : undefined, 15000).then(v => !!v); };
  const nostrPublish = (rs, ev) => Promise.all(rs.map(r => relayPublish(r, ev))).then(a => a.some(Boolean));
  function relayFetch(url, tag) {
    const out = [];
    const req = JSON.stringify(['REQ', 'yc', { kinds: [1059], '#p': [tag], since: now() - 7 * 86400, limit: 200 }]);
    return relayTalk(url, req, m => { if (m[0] === 'EVENT' && m[2]) out.push(m[2]); else if (m[0] === 'EOSE' || m[0] === 'CLOSED') return true; }, 20000).then(() => out);
  }
  const nSeen = new Set();
  let nBusy = false;
  async function nostrFetch() {
    const rs = nRelays(); if (!rs || nBusy) return;
    nBusy = true;
    try {
      const tag = yggNostrTag();
      const evs = new Map();
      for (const l of await Promise.all(rs.map(r => relayFetch(r, tag)))) for (const e of l) if (e && e.id) evs.set(e.id, e);
      for (const [id, e] of evs) {
        if (nSeen.has(id)) continue;
        const r = yggNostrOpen(JSON.stringify(e)); if (r.error) continue;
        let w; try { w = JSON.parse(r.w); } catch { continue; }
        if (w.t !== 'msg') continue;
        learnPK(r.from, r.pk);
        if (handleMsg(r.from, w)) { nSeen.add(id); send(r.from, { t: 'ack', id: w.id }); if (r.del) nostrPublish(rs, r.del); }
      }
    } finally { nBusy = false; }
  }
  async function nostrPark(m) {
    const rs = nRelays(); if (!rs || m.img || m.nostr) return;
    const c = st.contacts[m.peer]; if (!c || !c.pk) return;
    const ev = yggNostrWrap(c.pk, JSON.stringify({ t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts, ttl: m.ttl || undefined }));
    if (!ev || !(await nostrPublish(rs, ev))) return;
    const x = st.messages.find(y => y.peer === m.peer && y.id === m.id);
    if (x && x.status === 'queued') { x.nostr = true; setStatus(m.peer, m.id, 'stored'); }
  }
  const cleanRelays = l => [...new Set((Array.isArray(l) ? l : []).map(r => String(r).trim()).filter(r => { try { const u = new URL(r); return u.protocol === 'wss:' && !!u.host && r.length <= 200; } catch { return false; } }))].slice(0, 8);

  // ---- disappearing sweep ----
  setInterval(() => {
    if (locked) return;
    const t = now(), gone = st.messages.filter(m => m.exp && m.exp <= t);
    if (!gone.length) return;
    st.messages = st.messages.filter(m => !(m.exp && m.exp <= t)); save();
    for (const m of gone) emit('msg_del', { peer: m.peer, id: m.id });
  }, 2000);

  const ready = (async () => {
    await loadStore();
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
    addEventListener('online', () => setTimeout(() => { helloAll(); nostrFetch(); }, 3000));
    document.addEventListener('visibilitychange', () => { if (!document.hidden) { if (yggPeers() > 0) helloAll(); nostrFetch(); } });
    setTimeout(nostrFetch, 5000);
    setInterval(nostrFetch, 180000);
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
        return ok({ addr, pubkey: myPK(), nick: st.nick, bio: st.bio, avatar: st.avatar, contacts: st.contacts, messages: st.messages, peers: yggPeers(), platform: 'web', locked: false, haspass: !!aesKey, blocked: Object.keys(st.blocked), nostr: nostrState() });
      case 'unlock': {
        if (!locked) return ok({});
        try {
          const k = await kdf(String(body.Pass || ''), unb64(outer.enc.salt));
          const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(outer.enc.iv) }, k, unb64(outer.enc.ct));
          st = Object.assign(blank(), JSON.parse(new TextDecoder().decode(plain)));
          aesKey = k; locked = false; migrate();
          setTimeout(() => { helloAll(); nostrFetch(); }, 500);
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
        if ((!body.Text && !body.Img) || String(body.Text || '').length > 8000) return bad('message empty or too long');
        if (body.Img && !parseImg(body.Img)) return bad('unsupported or too large image (max 600KB)');
        if (body.Img && u8len(body.Text) > CAPMAX) return bad('写真に添える文章が長すぎます（約260文字まで）');
        const c = st.contacts[dst];
        const share = !c || !c.shared;
        if (c && (c.req || !c.shared)) { c.req = false; c.shared = true; emit('contact', c); }
        const ttl = (c && c.ttl) || 0;
        const m = { id: Date.now().toString(16) + Math.random().toString(16).slice(2, 6), peer: dst, from: addr, nick: st.nick, text: body.Text || '', ts: now(), out: true, status: 'sending' };
        if (body.Img) m.img = body.Img;
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
        outer.key = ''; st.contacts = {}; st.messages = []; save(); saveOuter(); await saving;
        setTimeout(() => location.reload(), 300);
        return ok({});
      }
      case 'peers': {
        if (body.Reset) { for (const p of outer.peers) if (!RECOMMENDED.includes(p)) yggRemovePeer(p); for (const p of RECOMMENDED) if (!outer.peers.includes(p)) yggAddPeer(p); outer.peers = RECOMMENDED.slice(); }
        else if (body.Add) {
          let u = body.Add.trim();
          if (!/^wss:\/\//.test(u)) return bad('ブラウザ版はwss://のピアだけ使えます（ピア側でこのページのOriginを許可している必要があります）');
          if (!outer.peers.includes(u)) { const e = yggAddPeer(u); if (e) return bad(e); outer.peers.push(u); }
        } else if (body.Remove) { yggRemovePeer(body.Remove); outer.peers = outer.peers.filter(p => p !== body.Remove); }
        saveOuter();
        return ok({ peers: peerList(), recommended: RECOMMENDED, up: yggPeers() });
      }
      case 'nostr': {
        const was = !!st.nostrOn;
        if (body.On != null) st.nostrOn = !!body.On;
        if (body.Reset) st.nostrRelays = [];
        else if (body.Relays) st.nostrRelays = cleanRelays(body.Relays);
        save();
        if (st.nostrOn && !was) { nostrFetch(); for (const m of st.messages.filter(x => x.out && x.status === 'queued' && !x.nostr)) nostrPark(m); }
        return ok(nostrState());
      }
      case 'peers/public':
        return ok({ peers: RECOMMENDED.map(u => ({ uri: u, label: LABELS[u] || u, region: '', continent: '', asia: false })) });
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
