// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors
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
  // A sender's timestamp as stored: a number, not in the future (non-numbers broke sorting and dates, far-future ones stuck to the bottom and hid unread).
  const inTs = t => { t = Math.trunc(+t); return t > 0 && t <= now() + 600 ? t : now(); }; // +600 s as the native node (r2)

  // ---- persistent state ----
  // Outer record (always readable): key, peers, enc. Inner data (encrypted when
  // a passcode is set): nick, bio, avatar, contacts, messages, blocked.
  // Stored in IndexedDB (images need more room than localStorage's ~5 MB);
  // older versions kept everything in localStorage and are migrated once.
  // The connection is reopened when the browser drops it (Safari: "Connection to
  // Indexed Database server lost" after the app was in the background), so later
  // writes don't all fail silently.
  let idbP = null;
  function openIDB() {
    const p = new Promise((res, rej) => {
      let q; try { q = indexedDB.open('yggchat', 1); } catch (e) { rej(e); return; }
      q.onupgradeneeded = () => q.result.createObjectStore('kv');
      q.onsuccess = () => { const db = q.result; db.onclose = () => { if (idbP === p) idbP = null; }; db.onversionchange = () => { db.close(); if (idbP === p) idbP = null; }; res(db); };
      q.onerror = () => rej(q.error || new Error('indexedDB open failed'));
    });
    p.catch(() => { if (idbP === p) idbP = null; });
    return (idbP = p);
  }
  async function idbDo(fn) {
    try { return await fn(await (idbP || openIDB())); }
    catch (e) { if (e && e.name === 'QuotaExceededError') throw e; idbP = null; return fn(await openIDB()); } // once more on a fresh connection
  }
  const idbGet = k => idbDo(db => new Promise((res, rej) => { const q = db.transaction('kv').objectStore('kv').get(k); q.onsuccess = () => res(q.result); q.onerror = () => rej(q.error); }));
  const idbPut = (k, v) => idbDo(db => new Promise((res, rej) => { const t = db.transaction('kv', 'readwrite'); t.objectStore('kv').put(v, k); t.oncomplete = res; t.onerror = () => rej(t.error); t.onabort = () => rej(t.error || new DOMException('transaction aborted', 'AbortError')); }));
  let raw = {};
  const outer = { key: '', peers: null, enc: null };
  const blank = () => ({ nick: 'anon', bio: '', avatar: '', contacts: {}, messages: [], blocked: {}, nostrOn: true, nostrRelays: [], ctl: [], groups: {}, powStrict: false, joins: {}, gone: {}, early: {}, gdel: {}, gdelh: {}, nrecall: [] });
  let st = blank();
  let locked = false;
  let aesKey = null; // CryptoKey while unlocked with a passcode
  // A member copy of a group post has id "<post>g<i>" (ids are hex, so the last g splits it).
  function postIDf(id) { id = String(id); const i = id.lastIndexOf('g'); return i > 0 ? id.slice(0, i) : id; }
  function samePost(a, b) { return postIDf(a) === postIDf(b); }
  const migrate = () => {
    st.messages = st.messages.filter(m => !(m.hid && m.status === 'sent')); // delivered group copies (kept before v0.19)
    // One post, one message: older versions stored relayed copies of a post under other copy ids (v0.19.2).
    const seen = new Set();
    st.messages = st.messages.filter(m => { if (!isG(m.peer) || m.out || m.sys || !m.from) return true; const k = m.peer + '|' + m.from + '|' + postIDf(m.id); if (seen.has(k)) return false; seen.add(k); return true; });
    for (const m of st.messages) {
      if (m.status !== 'sending' && m.status !== 'failed') continue;
      const gid = m.grp || (isG(m.peer) ? m.peer.slice(2) : ''), g = gid && (st.groups || {})[gid];
      if (g && g.left) { m.status = 'failed'; continue; } // a group we are out of: never retried
      m.status = 'queued';
    }
  };
  function storeError(what, e) {
    const x = new Error('storage ' + what + ': ' + ((e && (e.name || e.message)) || e)); x.store = true; x.cause = e; return x;
  }
  let storeWarnAt = 0;
  function storeWarn(e) { // a write failed: tell the user (at most once a minute) instead of only the console
    console.error('save', e);
    if (Date.now() - storeWarnAt < 60000) return; storeWarnAt = Date.now();
    const full = e && e.name === 'QuotaExceededError';
    try { emit('notice', { k: 'store', text: full ? '端末の保存容量が足りず、保存できませんでした。写真の多い会話を削除するか、空き容量を増やしてください' : '保存に失敗しました（' + ((e && e.name) || 'error') + '）。アプリを開き直してください' }); } catch {}
  }
  async function loadStore() {
    // A store that can't be read is not an empty one: starting fresh here made a
    // new identity, and its first write replaced the real store once IndexedDB
    // answered again. Retry, then stop with an error instead (never overwrite).
    let got, err = null;
    for (let i = 0; i < 3; i++) {
      try { got = await idbGet('state'); err = null; break; } catch (e) { err = e; await new Promise(r => setTimeout(r, 400 * (i + 1))); }
    }
    if (err) throw storeError('read', err);
    if (got != null && (typeof got !== 'object' || Array.isArray(got))) throw storeError('read', new Error('unexpected record'));
    raw = got || null;
    if (!raw) {
      try { raw = JSON.parse(localStorage.getItem(LS) || '{}'); } catch { raw = {}; }
      if (!raw || typeof raw !== 'object') raw = {};
      if (Object.keys(raw).length) { await idbPut('state', raw); localStorage.removeItem(LS); }
    }
    Object.assign(outer, { key: raw.key || '', peers: raw.peers || null, enc: raw.enc || null });
    if (outer.peers && outer.peers.some(p => /workers\.dev|instabox\.tech/.test(p))) outer.peers = null; // old Cloudflare relay / removed entry peer -> defaults
    if (!outer.peers) outer.peers = RECOMMENDED.slice();
    locked = !!outer.enc;
    if (!locked) restoreOutbox(raw);
    if (!locked) { Object.assign(st, { nick: raw.nick || 'anon', bio: raw.bio || '', avatar: raw.avatar || '', contacts: raw.contacts || {}, messages: raw.messages || [], blocked: raw.blocked || {}, domain: raw.domain || undefined, nostrOn: raw.key ? !!raw.nostrOn : true, nostrLegacy: !!raw.nostrLegacy, nostrRelays: raw.nostrRelays || [], ctl: raw.ctl || [], groups: raw.groups || {}, powStrict: !!raw.powStrict, joins: raw.joins || {}, gone: raw.gone || {}, early: raw.early || {}, gdel: raw.gdel || {}, gdelh: raw.gdelh || {}, nrecall: raw.nrecall || [] }); migrate(); }
  }

  // Outbox (v0.19.2): a message we just sent is also written to localStorage at
  // once (synchronously), until the IndexedDB write that holds it is done, so a
  // reload or close right after tapping send can't lose it. With a passcode the
  // entry is encrypted with the store key first (AES-GCM of one message takes
  // well under a millisecond, the full store write much longer) and is merged
  // back after the next unlock.
  const OUTBOX = 'yggchat-outbox', OUTBOX_ENC = 'yggchat-outbox-enc';
  const lsList = k => { try { const o = JSON.parse(localStorage.getItem(k) || '[]'); return Array.isArray(o) ? o : []; } catch { return []; } };
  const lsSet = (k, o) => { try { if (o.length) localStorage.setItem(k, JSON.stringify(o)); else localStorage.removeItem(k); return true; } catch { return false; } };
  const outboxGet = () => lsList(OUTBOX);
  const outboxEncPending = new Set(), outboxDone = new Set();
  function outboxPut(v, cs) {
    if (locked) return;
    outboxDone.delete(v.id);
    if (aesKey) {
      const key = aesKey, salt = outer.enc && outer.enc.salt;
      const put = async e => {
        const iv = crypto.getRandomValues(new Uint8Array(12));
        const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, new TextEncoder().encode(JSON.stringify(e)));
        if (outboxDone.has(v.id)) return true; // already in IndexedDB
        return lsSet(OUTBOX_ENC, lsList(OUTBOX_ENC).concat([{ id: v.id, salt, iv: b64(iv), ct: b64(ct) }]));
      };
      const p = put({ v, cs }).then(ok => ok || put({ v: Object.assign({}, v, { img: undefined, lost: !!v.img || undefined }), cs })).catch(() => {}).finally(() => outboxEncPending.delete(p));
      outboxEncPending.add(p);
      return;
    }
    const o = outboxGet(); o.push({ v, cs });
    if (!lsSet(OUTBOX, o)) lsSet(OUTBOX, outboxGet().concat([{ v: Object.assign({}, v, { img: undefined, lost: !!v.img || undefined }), cs }]));
  }
  function outboxDrop(id) {
    outboxDone.add(id);
    lsSet(OUTBOX, outboxGet().filter(e => !(e && e.v && e.v.id === id)));
    lsSet(OUTBOX_ENC, lsList(OUTBOX_ENC).filter(e => !(e && e.id === id)));
  }
  function mergeOutbox(r, o) {
    let n = 0;
    r.messages = r.messages || [];
    for (const e of o) {
      if (!e || !e.v || !e.v.id || e.v.lost || r.messages.some(m => m.id === e.v.id && m.peer === e.v.peer)) continue;
      r.messages.push(e.v); n++; for (const c of e.cs || []) if (!r.messages.some(m => m.id === c.id && m.peer === c.peer)) r.messages.push(c);
    }
    return n;
  }
  function restoreOutbox(r) {
    const o = outboxGet(); if (!o.length || r.enc) return;
    mergeOutbox(r, o);
    outboxRestored = true;
  }
  let outboxRestored = false;
  // In chunks: spreading a whole encrypted store (100+ KB, e.g. any photo) into
  // fromCharCode threw "Maximum call stack size exceeded", so with a passcode
  // nothing was saved any more and the store on disk stayed unencrypted.
  const b64 = u8 => { u8 = new Uint8Array(u8); let s = ''; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000)); return btoa(s); };
  const unb64 = s => Uint8Array.from(atob(s), c => c.charCodeAt(0));
  async function kdf(pass, salt) {
    const base = await crypto.subtle.importKey('raw', new TextEncoder().encode(pass), 'PBKDF2', false, ['deriveKey']);
    return crypto.subtle.deriveKey({ name: 'PBKDF2', salt, iterations: 310000, hash: 'SHA-256' }, base, { name: 'AES-GCM', length: 256 }, false, ['encrypt', 'decrypt']);
  }
  let saving = Promise.resolve(), saveQueued = false;
  // One pending write at a time, which takes the state as it is when it runs:
  // a burst of changes is one write, and the newest state is never stuck behind
  // a queue of stale ones (a reload/close right after sending lost messages, v0.19.2).
  // Resolves true once the state is in IndexedDB, false if the write failed.
  function save() {
    if (locked) return saving;
    if (saveQueued) return saving;
    saveQueued = true;
    saving = saving.then(async () => {
      saveQueued = false;
      // Locked since this write was queued (lock blanks st and drops the key):
      // writing now would replace the encrypted store with an empty plain one.
      if (locked || (outer.enc && !aesKey)) return false;
      const rec = { key: outer.key, peers: outer.peers };
      if (aesKey) {
        const key = aesKey, salt = outer.enc.salt; // the pair as it is now: a passcode change mid-write can't mix them
        const iv = crypto.getRandomValues(new Uint8Array(12));
        const ct = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, new TextEncoder().encode(JSON.stringify(st)));
        rec.enc = { v: 1, salt, iv: b64(iv), ct: b64(ct) };
        if (key !== aesKey) return false; // passcode changed meanwhile: the write queued after this one stores it
        outer.enc = rec.enc;
      } else Object.assign(rec, st);
      await idbPut('state', rec);
      raw = rec;
      return true;
    }).catch(e => { storeWarn(e); return false; });
    return saving;
  }
  // Write now (the page may be going away): pending saveSoon included.
  function saveNow() { if (saveT) { clearTimeout(saveT); saveT = null; } return save(); }
  let saveT = null;
  const saveSoon = () => { if (!saveT) saveT = setTimeout(() => { saveT = null; save(); }, 500); }; // many group-copy updates at once
  const saveOuter = () => { saving = saving.then(() => { raw = Object.assign({}, raw, { key: outer.key, peers: outer.peers }); return idbPut('state', raw); }).catch(e => console.error('save', e)); };

  let addr = '';
  const subs = new Set();
  const emit = (type, data) => { const s = JSON.stringify({ type, data }); subs.forEach(f => f(s)); };
  const acks = new Map(), ackFrom = new Map(); // id -> resolver, id -> the peer whose ack counts
  const waitAck = (peer, id) => new Promise(r => { acks.set(id, r); ackFrom.set(id, peer); });
  const cleanNick = n => String(n || '').replace(/[\u0000-\u001f\u007f-\u009f\u200b-\u200f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, 32);
  const cleanText = (s, n) => String(s || '').replace(/[\u0000-\u0009\u000b-\u001f\u007f-\u009f\u202a-\u202e\u2066-\u2069]/g, '').trim().slice(0, n);
  const send = (peer, w) => { try { yggSend(peer, JSON.stringify(w)); } catch {} };
  const myPK = () => { try { return yggPub(); } catch { return ''; } };

  function addMessage(m) {
    if (isG(m.peer) && !m.out && !m.sys && m.from && st.messages.some(x => x.peer === m.peer && x.from === m.from && !x.out && samePost(x.id, m.id))) return; // one post, one message (v0.19.2)
    if (!m.out && !m.sys && !isG(m.peer)) insertByTime(m); else st.messages.push(m);
    // Cap the visible history; pending group copies are never dropped (v0.19.1).
    if (st.messages.length > 5000) { let over = st.messages.length - 5000; st.messages = st.messages.filter(x => { if (over > 0 && !x.hid && !(x.out && x.status !== 'sent')) { over--; return false; } return true; }); }
    let c = st.contacts[m.peer];
    if (isG(m.peer)) { /* group conversations have no contact entry */ }
    else if (!c) { c = st.contacts[m.peer] = { addr: m.peer, nick: m.out ? '' : (m.nick || ''), req: !m.out, shared: !!m.out, ttl: m.ttl || 0 }; emit('contact', c); }
    else if (!m.out && m.nick && !c.renamed && c.nick !== m.nick) { c.nick = m.nick; emit('contact', c); }
    save();
    emit('message', m);
  }

  // Incoming 1:1 messages are stored in time order: queued and Nostr-parked ones
  // arrive late and out of order (v0.19.3, as the native node).
  function insertByTime(m) {
    let i = st.messages.length;
    for (let k = 0; i > 0 && k < 2000 && (st.messages[i - 1].ts || 0) > m.ts; k++) i--;
    st.messages.splice(i, 0, m);
  }

  // ---- 1:1 tombstones and early edits (v0.19.3, same rules as msgedit.go) ----
  // st.gone["peer/id"] = until: unsent by a contact, or deleted / expired here; a late
  // Nostr box or a retry is acked and dropped. st.early["peer/id"] = {text, at}: an edit
  // that overtook its message, applied when it lands.
  const TOMBKEEP = 10 * 86400, GONEMAX = 20000, EARLYMAX = 500, EARLYPEER = 50;
  function markGone(peer, id) {
    const t = now(); st.gone = st.gone || {};
    if (Object.keys(st.gone).length >= GONEMAX) {
      for (const k in st.gone) if (st.gone[k] <= t) delete st.gone[k];
      const ks = Object.keys(st.gone);
      if (ks.length >= GONEMAX * 0.9) { ks.sort((a, b) => st.gone[a] - st.gone[b]); for (const k of ks.slice(0, ks.length - Math.floor(GONEMAX * 0.9) + 1)) delete st.gone[k]; }
    }
    st.gone[peer + '/' + id] = t + TOMBKEEP;
    if (st.early) delete st.early[peer + '/' + id];
  }
  // An incoming 1:1 message removed here while a copy of it could still arrive.
  function goneIfRecent(m) { if (!m || m.out || m.sys || m.hid || isG(m.peer) || (m.ts || 0) < now() - TOMBKEEP) return; markGone(m.peer, m.id); }
  const isGoneMsg = (peer, id) => { const t = st.gone && st.gone[peer + '/' + id]; return !!t && t > now(); };
  function addEarly(peer, id, text) {
    const t = now(), k = peer + '/' + id; st.early = st.early || {};
    let n = 0;
    for (const x in st.early) { if (t - (st.early[x].at || 0) > TOMBKEEP) delete st.early[x]; else if (x.startsWith(peer + '/')) n++; }
    if (!(k in st.early) && (n >= EARLYPEER || Object.keys(st.early).length >= EARLYMAX)) return;
    st.early[k] = { text, at: t };
  }
  function takeEarly(peer, id) { const k = peer + '/' + id, e = st.early && st.early[k]; if (!e) return null; delete st.early[k]; return String(e.text || ''); }
  const stillOut = (peer, id) => st.messages.some(x => x.peer === peer && x.id === id);
  // An ack nobody waits for (the peer read our Nostr box and acked directly, or an
  // earlier try got through after we gave up): the message is delivered; a pending
  // edit/unsend is done (v0.19.3).
  function lateAck(from, id) {
    let found = false;
    for (let i = st.messages.length - 1; i >= 0; i--) { const m = st.messages[i]; if (m.id === id && m.peer === from && m.out) { found = m.status === 'queued' || m.status === 'stored'; break; } }
    const done = (st.ctl || []).filter(c => c.peer === from && c.w.id === id);
    if (done.length) { st.ctl = st.ctl.filter(c => !(c.peer === from && c.w.id === id)); save(); nostrRecall(ctlRefs(done)); } // a parked copy of it isn't needed now (v0.19.4)
    if (found) setStatus(from, id, 'sent');
  }
  // Message requests (stranger / not yet accepted) are limited: 20/min per address,
  // 120/min in all. Over the limit no ack, so the sender retries later (v0.19.3).
  const reqT = { all: [], peer: new Map() };
  function strangerOK(from) {
    const t = Date.now(), cut = t - 60000, trim = a => { let i = 0; while (i < a.length && a[i] <= cut) i++; return i ? a.slice(i) : a; };
    reqT.all = trim(reqT.all); const p = trim(reqT.peer.get(from) || []);
    if (p.length >= 20 || reqT.all.length >= 120) { reqT.peer.set(from, p); return false; }
    if (reqT.peer.size > 4096) for (const [k, v] of reqT.peer) if (!trim(v).length) reqT.peer.delete(k);
    p.push(t); reqT.peer.set(from, p); reqT.all.push(t); return true;
  }

  function setStatus(peer, id, status) {
    const i = st.messages.findIndex(x => x.peer === peer && x.id === id), m = i >= 0 ? st.messages[i] : null;
    if (m && m.hid && m.status === 'failed' && status !== 'sent' && gOut(m.grp)) return; // given up when we left: a retry still finishing can't revive it
    if (m) { m.status = status; if (m.hid) { if (status === 'sent') { st.messages.splice(i, 1); const v = st.messages.find(x => x.peer === 'g:' + m.grp && x.id === m.of); if (v) v.dlv = (v.dlv | 0) + 1; } saveSoon(); } else save(); }
    if (m && m.hid) { groupStatus(m.grp, m.of); return; }
    emit('status', { id, peer, status });
  }

  // ---- profile sharing, disappearing timer ----
  const profWire = () => ({ t: 'prof', nick: st.nick, bio: st.bio, av: st.avatar, dom: st.domain || undefined, pk: myPK() });
  function sendProf(p) { const w = profWire(); for (let i = 0; i < 3; i++) setTimeout(() => send(p, w), i * 1500); }
  const profAt = {};
  function maybeProf(p) { const c = st.contacts[p]; if (!c || !c.shared) return; if (Date.now() - (profAt[p] || 0) < 600000) return; profAt[p] = Date.now(); sendProf(p); }
  function setTTL(p, ttl, byMe, at) {
    if (!Number.isSafeInteger(ttl) || ttl < 0 || ttl > MAXTTL) return; // out of range: ignored as native (was clamped: ttl 99999999 set 4 weeks, -5 turned it off), r2
    const c = st.contacts[p]; if (!c || (c.ttl || 0) === ttl) return;
    c.ttl = ttl; c.ttlat = at == null ? now() : at; // when the setting it follows was made (v0.19.2)
    const m = { id: 's' + Date.now().toString(16) + Math.random().toString(16).slice(2, 5), peer: p, sys: 'ttl:' + ttl, ts: now(), out: !!byMe };
    st.messages.push(m); save(); emit('contact', c); emit('message', m);
    if (byMe) for (let i = 0; i < 3; i++) setTimeout(() => send(p, { t: 'ttl', ttl }), i * 1500);
  }
  function learnPK(from, pk) {
    pk = String(pk || '').toLowerCase(); // hex in any case, as native (r2)
    const c = st.contacts[from];
    if (c && pk && c.pk !== pk && /^[0-9a-f]{64}$/.test(pk) && yggAddrOK(from, pk)) { c.pk = pk; save(); }
  }

  // Greet a contact whose full key we lack: hi/hiack carry both keys (needed for Nostr boxes).
  const askPK = p => { const c = st.contacts[p]; if (c && !c.pk && !locked) hello(p); };
  // ---- offline queue ----
  const inflight = new Set();
  const gOut = gid => { const g = st.groups[gid]; return !!(g && g.left); };
  const queuedFor = p => st.messages.filter(m => m.out && m.peer === p && (m.status === 'queued' || m.status === 'stored') && !(m.hid && gOut(m.grp)));
  async function flush(p) { pushInvites(p); for (const m of queuedFor(p)) { if (!(await deliver(m, 4))) return; } await flushCtl(p); }

  // ---- edit / unsend (only our own messages; receiver checks the sender address) ----
  const ctlID = () => 'c' + Date.now().toString(16) + Math.random().toString(16).slice(2, 6);
  const isSent = m => m.status === 'sent' || m.status === 'sending' || !!m.nostr;
  // park: the message went out as a Nostr box, so this goes that way too when the peer can't be reached (v0.19.4).
  function queueCtl(peer, w, park) {
    const old = (st.ctl || []).filter(c => c.peer === peer && c.w.ref === w.ref);
    st.ctl = (st.ctl || []).filter(c => !(c.peer === peer && c.w.ref === w.ref));
    const c = { peer, w }; if (park) c.park = true;
    st.ctl.push(c); save(); nostrRecall(ctlRefs(old)); flushCtl(peer);
  }
  const ctlBusy = new Set();
  async function flushCtl(p) {
    if (ctlBusy.has(p)) return; ctlBusy.add(p);
    try {
      for (const c of (st.ctl || []).filter(x => x.peer === p)) {
        const acked = waitAck(p, c.w.id);
        let ok = false;
        for (let i = 0; i < 6 && !ok; i++) { send(p, c.w); ok = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]); }
        acks.delete(c.w.id);
        if (!ok) { parkCtl(p); return; } // the peer may only come back while we're away
        const done = st.ctl.filter(x => x.peer === p && x.w.id === c.w.id);
        st.ctl = st.ctl.filter(x => !(x.peer === p && x.w.id === c.w.id)); save(); nostrRecall(ctlRefs(done));
      }
    } finally { ctlBusy.delete(p); }
  }
  function onCtl(src, w) {
    const okID = x => /^[0-9A-Za-z_-]{1,40}$/.test(String(x || ''));
    if (!okID(w.id) || !okID(w.ref)) return false;
    const i = st.messages.findIndex(m => m.peer === src && m.id === w.ref && !m.out && m.from === src);
    const t = String(w.text || ''), okText = !!t.trim() && u8len(t) <= MAXRECV;
    let upd = null, del = false;
    if (i >= 0) {
      if (w.t === 'edit') { if (okText) { st.messages[i].text = t; st.messages[i].edited = true; upd = st.messages[i]; } }
      else { st.messages.splice(i, 1); del = true; }
    }
    // The message itself may still be on its way (retry, offline queue, Nostr box):
    // remember the unsend / edit so it applies when it lands. Contacts only, so a
    // stranger can't fill the store (v0.19.3).
    const known = !!st.contacts[src];
    if (known && w.t === 'del') markGone(src, w.ref);
    else if (known && w.t === 'edit' && i < 0 && okText && !isGoneMsg(src, w.ref)) addEarly(src, w.ref, t);
    if (i >= 0) save(); else if (known) saveSoon();
    if (upd) emit('msg_upd', upd); else if (del) emit('msg_del', { peer: src, id: w.ref });
    return true; // ack even if unknown, so the sender stops retrying
  }
  const hello = p => send(p, { t: 'hi', pk: myPK() });
  function helloAll() { if (!locked) for (const p of Object.keys(st.contacts)) hello(p); }

  const handleMsg = (src, w) => w.g ? onGroupMsg(src, w) : Promise.resolve(!!w.text && acceptMsg(src, w, ''));
  function acceptMsg(src, w, img) {
    if ((w.text != null && typeof w.text !== 'string') || (!w.text && !img) || u8len(w.text) > MAXRECV || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return false;
    if (locked || st.blocked[src]) return false;
    if (isGoneMsg(src, String(w.id)) || st.messages.some(m => m.peer === src && m.id === w.id)) return true; // unsent by them, or deleted/expired here (v0.19.3)
    if (!powCheck(src, w)) return false;
    { const c1 = st.contacts[src]; if ((!c1 || c1.req) && !strangerOK(src)) return false; } // request flood: no ack, they retry later
    let ttl = w.ttl | 0; if (ttl < 0 || ttl > MAXTTL) ttl = 0;
    // Only a message newer than the last change moves the timer (late Nostr copies come in any order), v0.19.2.
    const c0 = st.contacts[src], at = Math.min(Math.trunc(+w.ts || 0), now() + 300);
    if (c0 && at >= (c0.ttlat || 0)) { if ((c0.ttl || 0) !== ttl) setTTL(src, ttl, false, at); else if (at > (c0.ttlat || 0)) { c0.ttlat = at; saveSoon(); } }
    const m = { id: w.id, peer: src, from: src, nick: cleanNick(w.nick), text: w.text || '', ts: inTs(w.ts), out: false };
    if (img) m.img = img;
    if (ttl) { m.ttl = ttl; m.exp = now() + ttl; }
    { const e = takeEarly(src, m.id); if (e != null && e.trim()) { m.text = e; m.edited = true; } } // its edit overtook it
    addMessage(m);
    return true;
  }

  // ---- group chat (v0.13.5, provisional; same wire as the native node, see groups.go) ----
  // A group message goes to every member as its own acked "msg" with g (group id)
  // and gh (hash of name+members); the copies are hidden, the "g:<id>" message shows.
  const isG = k => String(k || '').startsWith('g:');
  const gView = g => { const v = { addr: 'g:' + g.id, nick: g.name, avatar: '👥', group: true, members: g.members, req: !!g.req, left: !!g.left, owner: g.owner || '', set: g.set || {}, admins: g.admins || [], max: gMax(g) }; if (g.set && g.set.ttl) v.ttl = g.set.ttl; if (g.pend && g.pend.length) v.pend = g.pend; return v; };
  // ---- group settings (v0.18, see groupset.go) ----
  const GTTLS = [0, 300, 3600, 86400, 604800, 2419200];
  const cleanSet = s => { s = s || {}; const o = {}; if (s.nf) o.nf = true; if (s.ap) o.ap = true; if (s.hi) o.hi = true; if (s.pw) o.pw = true; if (GTTLS.includes(s.ttl | 0) && s.ttl) o.ttl = s.ttl | 0; if (GMAXES.includes(s.mx | 0) && (s.mx | 0) !== GDEF) o.mx = s.mx | 0; if (GPOWS.includes(s.pb | 0) && s.pb) o.pb = s.pb | 0; return o; };
  const ttlText = s => ({ 0: 'オフ', 300: '5分', 3600: '1時間', 86400: '1日', 604800: '1週間', 2419200: '4週間' })[s] || 'オン';
  const onOff = b => b ? 'オン' : 'オフ';
  function setDiff(a, b) {
    a = a || {}; b = b || {}; const o = [];
    if (!!a.nf !== !!b.nf) o.push('写真の送信を' + onOff(!b.nf) + 'にしました');
    if (!!a.ap !== !!b.ap) o.push('参加の承認を' + onOff(b.ap) + 'にしました');
    if (!!a.hi !== !!b.hi) o.push('新メンバーへの履歴共有を' + onOff(b.hi) + 'にしました');
    if ((a.ttl | 0) !== (b.ttl | 0)) o.push('消えるメッセージを「' + ttlText(b.ttl | 0) + '」にしました');
    if (!!a.pw !== !!b.pw) o.push('参加パスワードを' + onOff(b.pw) + 'にしました');
    if (gMax({ set: a }) !== gMax({ set: b })) o.push('人数の上限を' + gMax({ set: b }) + '人にしました');
    if (gPow({ set: a }) !== gPow({ set: b })) o.push(gPow({ set: b }) ? '投稿のPoWを' + gPow({ set: b }) + 'ビットにしました' : '投稿のPoWをなしにしました');
    return o;
  }
  const PWITER = 100000;
  async function pbk(p, salt, len) { const k = await crypto.subtle.importKey('raw', new TextEncoder().encode(p), 'PBKDF2', false, ['deriveBits']); return new Uint8Array(await crypto.subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt, iterations: PWITER }, k, len * 8)); }
  async function hashPass(p) { const salt = crypto.getRandomValues(new Uint8Array(16)); return 'p1:' + hexOf(salt) + ':' + hexOf(await pbk(p, salt, 32)); }
  async function checkPass(h, p) {
    const x = String(h || '').split(':'); if (x.length !== 3 || x[0] !== 'p1' || !p) return false;
    const want = hexBytes(x[2]); const got = await pbk(p, hexBytes(x[1]), want.length);
    let d = got.length ^ want.length; for (let i = 0; i < want.length; i++) d |= got[i] ^ want[i]; return d === 0;
  }
  const pwFails = {}, pwG = { n: 0, at: 0 };
  function pwGlobalOK() { const t = Date.now(); if (t - pwG.at > 60000) { pwG.n = 0; pwG.at = t; } return ++pwG.n <= 20; }
  function pwLimited(a, failed) { const t = Date.now(); let f = pwFails[a]; if (!f || t - f.at > 600000) f = pwFails[a] = { n: 0, at: t }; if (failed) f.n++; return f.n >= 5; }
  function historyOf(gid) {
    const t = now(), out = [];
    for (let i = st.messages.length - 1; i >= 0 && out.length < 30; i--) { const m = st.messages[i]; if (m.peer !== 'g:' + gid || m.hid || m.sys || (m.exp && m.exp <= t) || (!m.text && !m.img)) continue; out.push(m); }
    return out.reverse();
  }
  function sendHistory(to, gid, hist) {
    const t = now(), g = st.groups[gid];
    const ws = [];
    for (const m of hist) {
      let text = m.img ? (m.text ? '📷 ' + m.text : '📷 写真') : m.text;
      if (u8len(text) > 700) { while (u8len(text) > 700) text = text.slice(0, -1); text += '…（省略）'; }
      const w = { t: 'ghs', g: gid, id: postIDf(m.id), by: m.from, nick: m.nick, text, ts: m.ts }; // the post id, not our copy's (v0.19.2)
      { const s = text === m.text ? sigFor(g, m) : null; if (s) { w.sg = s[0]; w.pk = s[1]; } } // the sender's proof, valid only for the whole text (v0.19.3)
      if (m.exp) { const r = m.exp - t - 20; if (r <= 0) continue; w.ttl = r; } // it takes a few seconds to send
      if (w.sg && goLen(w) > DGRAM) { delete w.sg; delete w.pk; } // it will be shortened: the signature no longer fits the text
      while (goLen(w) > DGRAM && w.text.length > 12) w.text = w.text.slice(0, -12 - 6) + '…（省略）';
      if (goLen(w) <= DGRAM) ws.push(w);
    }
    let d = 3000; for (let r = 0; r < 2; r++) { for (const w of ws) { setTimeout(() => send(to, w), d); d += 60; } d += 4000; }
  }
  function admit(g, src, nick, how) {
    const before = g.members;
    g.members = cleanMembers(g.members.concat([src]), addr); g.pend = (g.pend || []).filter(p => p.addr !== src); if (!g.pend.length) delete g.pend;
    if (g.owner === addr) trackGone(g, before);
    noteGone(g, before, 0);
    unban(g, src); // let in again (approved, or a direct add)
    bumpV(g);
    save(); emit('contact', gView(g)); gSys(g, (nick || nameOf(src)) + ' が' + how); gMembers(g, [src], [], false);
    if (g.set && g.set.hi) { const h = historyOf(g.id); if (h.length) sendHistory(src, g.id, h); }
  }
  const histHeard = {}; // gid -> last history answer from a member (keys are only our groups)
  async function onHistory(src, w) {
    const g = st.groups[w.g], by = norm(String(w.by || ''));
    if (g && g.members.includes(src)) histHeard[g.id] = Date.now(); // an answer reached us (syncGroup's third ask)
    // Any member may relay what it has (v0.19.1); copies not from the sender, host or an admin show who relayed them.
    if (!g || g.left || !g.members.includes(src) || !by || !isYgg(by) || by === addr || !w.text || typeof w.text !== 'string' || w.text.length > 2000 || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id)) || (w.ttl | 0) < 0) return;
    // An ordinary member relays only when we asked it (ghq); the host and admins also send history to new members unasked (v0.19.3).
    if (!canMng(g, src) && !histAsked(g.id, src)) return;
    if (!hasM(g, by)) {
      const at = goneAt(g, by);
      if (at && (at < 0 || !(+w.ts > 0) || +w.ts > at)) return; // posted after they left or were removed (v0.19.2)
      if (!at && !canMng(g, src)) return; // never a member as far as we know: only the host/an admin may relay that (v0.19.3)
    }
    const key = 'g:' + g.id, dup = () => isGDel(g.id, by, w.id) || st.messages.some(m => m.peer === key && m.from === by && samePost(m.id, w.id)); // unsent/deleted posts stay gone (v0.19.4)
    if (dup()) return;
    const knownSigner = !!(g.keys && g.keys[by]);
    // The sender's signature: a bad one is a forgery; a good one is its own words, whoever relays it (v0.19.3).
    let signed = false;
    if (w.sg) { const v = await verifyPost(by, w.pk, w.sg, g.id, postIDf(w.id), w.ts, w.text); if (v === false) return; signed = v === true; }
    if (dup() || st.groups[w.g] !== g || g.left) return; // changed while checking
    let ts = Math.trunc(+w.ts || 0); if (ts <= 0 || ts > now() + 300) ts = now();
    const m = { id: postIDf(w.id), peer: key, from: by, nick: cleanNick(w.nick), text: w.text, ts, out: false, hist: true };
    if (signed) { if (ts === w.ts) m.sg = String(w.sg); if (!knownSigner && hasM(g, by)) noteSigner(g.id, by, String(w.pk).toLowerCase()); } // a clamped time breaks the signature (r2)
    // Unsigned relays show who relayed them; the host's and admins' too when the sender is known to sign.
    if (by !== src && !signed && (!canMng(g, src) || knownSigner)) m.via = src;
    // The group's timer applies to relayed posts too, counted from when they were posted (v0.19.3).
    const [exp, live] = histExp(g, ts, w.ttl | 0); if (!live) return;
    if (exp) { m.exp = exp; m.ttl = exp - ts > 0 ? exp - ts : exp - now(); }
    addMessage(m);
  }
  // Catch-up (v0.19.1): whoever of the group is online stands in as its mailbox.
  const histSeen = {};
  function onHistReq(src, w) {
    const g = st.groups[w.g];
    if (g && !g.left && !g.members.includes(src)) goneNotice(src, g); // a removed member catching up: tell it it is out
    else if (g && g.left) leftNotice(src, g); // our heir catching up gets the hand-over (v0.19.3)
    if (!g || g.left || !g.members.includes(src) || !g.members.includes(addr) || src === addr) return;
    const k = src + '/' + g.id, t = now(); if (t - (histSeen[k] || 0) < 20) return; capMap(histSeen); histSeen[k] = t;
    // Without history sharing: only what was said since they were added, whatever "since" they ask for (v0.19.3).
    const hi = !!(g.set && g.set.hi); let since = +w.ts || 0; { const aa = g.gaa && g.gaa[src]; if (!hi && aa > since) since = aa; }
    const h = historyOf(g.id).filter(m => m.from !== src && (hi || (m.ts || 0) >= since) && !(m.from !== addr && !hasM(g, m.from) && goneAt(g, m.from) > 0 && (m.ts || 0) > goneAt(g, m.from))); // never relay what a removed member posted after its removal
    if (h.length) sendHistory(src, g.id, h);
    const ds = gdelWires(g.id); if (ds.length) sendGDels(src, ds); // so a relay from someone who missed an unsend can't bring it back (v0.19.4)
  }
  function syncGroup(gid) {
    const g = st.groups[gid]; if (!g || g.left || g.req || locked) return;
    const key = 'g:' + gid; let since = now(); const f = st.messages.find(m => m.peer === key && m.ts > 0); if (f && f.ts < since) since = f.ts;
    const ask = [];
    const add = x => { if (x && x !== addr && g.members.includes(x) && !ask.includes(x) && ask.length < 8) ask.push(x); };
    add(g.owner); for (const x of g.admins || []) add(x);
    for (const x of g.members.slice().sort(() => Math.random() - 0.5)) add(x);
    // Their answer (ghs) is taken for a while. Asked twice a few seconds apart: members that aren't contacts get no
    // hello first, so the first packet to one often goes nowhere while its route is looked up (answers are rate-limited).
    // No answer at all after both (packets toward a node that just came back can be lost for half a minute): once more
    // after the answerers' 20 s limit (as Go).
    if (ask.length) { const t0 = Date.now(); setTimeout(() => { const g2 = st.groups[gid]; if ((histHeard[gid] || 0) >= t0 || !g2 || g2.left || g2.req || locked) return; for (const x of ask) if (g2.members.includes(x)) { noteHistAsk(gid, x); send(x, { t: 'ghq', g: gid, ts: since }); } }, 24000); }
    for (const x of ask) { noteHistAsk(gid, x); send(x, { t: 'ghq', g: gid, ts: since }); setTimeout(() => { if (st.groups[gid] && !st.groups[gid].left) { noteHistAsk(gid, x); send(x, { t: 'ghq', g: gid, ts: since }); } }, 4000); }
    // History doesn't tell us about membership changes made while we were away: ask the host and admins for the info too (as Go).
    const mgr = [g.owner].concat(g.admins || []).filter(x => x && x !== addr && g.members.includes(x)).slice(0, 4);
    for (const x of mgr) { askInfo(x, gid); setTimeout(() => { if (st.groups[gid] && !st.groups[gid].left) send(x, { t: 'gq', g: gid }); }, 4000); }
  }
  function syncAllGroups() {
    let d = 0; for (const id of Object.keys(st.groups)) { setTimeout(() => syncGroup(id), d); d += 200; }
    // Groups we left lately: our gl again (it may all have been lost while we were cut off), v0.19.3.
    if (!locked) for (const g of Object.values(st.groups)) if (g.left && g.quit && g.qat > 0 && now() - g.qat < LEAVEREPEAT) repeatLeave(g);
  }
  const okGID = id => /^[0-9a-f]{16}$/.test(String(id || ''));
  function cleanMembers(list, must) {
    const out = [];
    for (const x of [must].concat(list || [])) { const a = norm(String(x || '')); if (a && isYgg(a) && !out.includes(a)) out.push(a); }
    return out.slice(0, GCAP).sort();
  }
  const gHash = async g => hexOf(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(g.name + '\n' + g.members.slice().sort().join(',')))).slice(0, 8);
  const hasM = (g, a) => g.members.includes(a);
  const realNick = n => !!n && n !== 'anon'; // 'anon' = never named: system lines use the short address (the UI names it)
  const nameOf = a => a === addr ? 'あなた' : ((st.contacts[a] && realNick(st.contacts[a].nick)) ? st.contacts[a].nick : a.slice(0, 9) + '…');
  // A member in a group's system lines: contact name, else their group-message or join nick, else a short address (the UI names it later).
  function gNameOf(g, a) {
    if (a === addr || (st.contacts[a] && realNick(st.contacts[a].nick))) return nameOf(a);
    for (let i = st.messages.length - 1; i >= 0; i--) { const m = st.messages[i]; if (m.peer === 'g:' + g.id && m.from === a && realNick(m.nick) && !m.hid) return m.nick; }
    const p = (g.pend || []).find(x => x.addr === a && realNick(x.nick)); return p ? p.nick : nameOf(a);
  }
  function gSys(g, text) { addMessage({ id: 's' + Date.now().toString(16) + Math.random().toString(16).slice(2, 5), peer: 'g:' + g.id, from: addr, ts: now(), sys: text, out: true }); }
  const giWire = g => { const w = { t: 'gi', g: g.id, gn: g.name, gm: g.members, go: g.owner || undefined, gv: g.v || undefined, gsv: g.sv || undefined, gad: (g.admins && g.admins.length) ? g.admins : undefined, ts: g.vat || undefined }; if (g.owner) w.gs = cleanSet(g.set); return w; };
  // ---- v0.19: large groups, co-admins, member-list versions (see groupadmin.go) ----
  const GCAP = 1024, GDEF = 24, MAXADM = 10, GIMAX = 1180, PARTN = 40, NOSTRG = 64, DGRAM = 1200;
  const GMAXES = [24, 32, 50, 64, 100, 128, 256, 512, 1024];
  const GPOWS = [-1, 0, 6, 8, 10, 12, 16];
  const gMax = g => { const x = (g.set && g.set.mx) | 0; return x >= GDEF && x <= GCAP ? x : GDEF; };
  const isAdm = (g, a) => (g.admins || []).includes(a);
  const canMng = (g, a) => !!g.owner && (a === g.owner || isAdm(g, a));
  // v0.19.2: same-version collisions are merged by the host; Gone (host only) keeps
  // recently removed/left members so a late or colliding admin list can't bring them back.
  const MAXGONE = 256, MAXSTUBS = 50, COPYTRIES = 3;
  const sameM = (a, b) => a.length === b.length && a.slice().sort().join(',') === b.slice().sort().join(',');
  function trackGone(g, before) {
    const gone = (g.gone || []).slice(); for (const m of before) if (!g.members.includes(m) && !gone.includes(m)) gone.push(m);
    let k = gone.filter(m => !g.members.includes(m)); if (k.length > MAXGONE) k = k.slice(-MAXGONE);
    if (k.length) g.gone = k; else delete g.gone;
  }
  // Every node (local only): when members dropped out, so a post they made
  // before is still taken (late copy or relay) and a later one is acked and dropped (v0.19.2).
  const GONEKEEP = 14 * 86400;
  function noteGone(g, before, at) {
    const t = now(); if (!(at > 0) || at > t) at = t;
    const ga = Object.assign({}, g.gat || {});
    for (const m of before) if (!g.members.includes(m) && (!(m in ga) || at < ga[m])) ga[m] = at;
    for (const m in ga) if (g.members.includes(m) || t - ga[m] > GONEKEEP) delete ga[m];
    let ks = Object.keys(ga); if (ks.length > MAXGONE) { ks.sort((a, b) => ga[b] - ga[a]); for (const m of ks.slice(MAXGONE)) delete ga[m]; ks = ks.slice(0, MAXGONE); }
    if (ks.length) g.gat = ga; else delete g.gat;
    noteAdded(g, before, 0); // and when the new ones came (history without sharing, v0.19.3)
    if (g.ban) { g.ban = g.ban.filter(m => !g.members.includes(m)); if (!g.ban.length) delete g.ban; } // added back by any manager: no longer barred
  }
  const goneAt = (g, a) => (g.gat && a in g.gat) ? g.gat[a] : (g.owner && (g.gone || []).includes(a)) ? -1 : 0;
  // ---- v0.19.3 group hardening (same rules as groupsguard.go / groupssig.go) ----
  const LATEGRACE = 7 * 86400, LATEMAX = 30, MAXVJUMP = 64, MAXINVITES = 50, MAXINVITESPER = 3, HISTASK = 120, MAXBAN = 256, PENDKEEP = 3 * 86400, LEAVEREPEAT = 3 * 86400;
  const bumpV = g => { g.v = (g.v | 0) + 1; g.vat = now(); }; // a new member-list version (host/admin side), with when
  // The time of a member-list change as the manager who made it says ("ts" on gi/gd), when plausible; else 0 (= now).
  const changeTime = ts => { const t = now(); ts = Math.trunc(+ts || 0); return ts <= 0 || ts > t + 300 || t - ts > GONEKEEP ? 0 : Math.min(ts, t); };
  // A former member's post made before it dropped out is taken only for a while after that, and only so many (per run).
  const lateN = {};
  const lateOK = (g, from, ts) => { const at = goneAt(g, from); ts = +ts || 0; return at > 0 && ts > 0 && ts <= at && now() - at <= LATEGRACE; };
  function lateTake(gid, from) { const k = gid + '|' + from; if ((lateN[k] | 0) >= LATEMAX) return false; if (Object.keys(lateN).length > 4096) for (const x in lateN) delete lateN[x]; lateN[k] = (lateN[k] | 0) + 1; return true; }
  // When members not in before joined (local; history without sharing starts there for them).
  function noteAdded(g, before, at) {
    const t = now(); if (!(at > 0) || at > t) at = t;
    const aa = Object.assign({}, g.gaa || {});
    if (before.length) for (const m of g.members) if (!before.includes(m)) aa[m] = at;
    for (const m in aa) if (!g.members.includes(m)) delete aa[m];
    if (Object.keys(aa).length) g.gaa = aa; else delete g.gaa;
  }
  // Lower the drop-out times of members of before that left to the per-member times the manager gave ("gt").
  function noteGoneEach(g, before, gt) {
    if (!gt || typeof gt !== 'object') return;
    for (const m of Object.keys(gt).slice(0, 64)) {
      const t = changeTime(gt[m]); if (t <= 0 || g.members.includes(m) || !before.includes(m)) continue;
      g.gat = g.gat || {}; if (!(m in g.gat) || t < g.gat[m]) g.gat[m] = t;
    }
  }
  // What the host/an admin puts in its info as "gt": when the most recent (≤8) drop-outs happened.
  function goneTimes(g) {
    if (!g.gat || !g.owner) return null; const t = now();
    const es = Object.entries(g.gat).filter(([m, x]) => !g.members.includes(m) && x > 0 && t - x <= GONEKEEP).sort((a, b) => b[1] - a[1]).slice(0, 8);
    return es.length ? Object.fromEntries(es) : null;
  }
  // Drop the posts members of before (no longer in g) made after they dropped out: they reached us while we
  // hadn't heard of the removal. Returns the msg_del events to send after saving.
  function purgeGone(g, before) {
    const gone = {}; let any = false;
    for (const m of before) { const t = g.gat && g.gat[m]; if (!g.members.includes(m) && m !== addr && t > 0) { gone[m] = t; any = true; } }
    if (!any) return [];
    const key = 'g:' + g.id, del = [];
    st.messages = st.messages.filter(m => { if (m.peer === key && !m.out && !m.sys && m.from in gone && (m.ts || 0) > gone[m.from]) { del.push({ peer: key, id: m.id }); return false; } return true; });
    return del;
  }
  const emitDels = ds => { for (const d of ds) emit('msg_del', d); };
  // Relayed history ("ghs") from an ordinary member is taken only when we asked it a moment ago.
  const histAskAt = {};
  function noteHistAsk(gid, p) { if (Object.keys(histAskAt).length > 4096) for (const x in histAskAt) delete histAskAt[x]; histAskAt[gid + '|' + p] = now(); }
  const histAsked = (gid, p) => now() - (histAskAt[gid + '|' + p] || 0) <= HISTASK;
  // A relayed post's expiry: its own timer, and the group's counted from when it was posted. [exp, live]
  function histExp(g, ts, ttl) {
    const t = now(); let exp = ttl > 0 ? t + ttl : 0; const gt = (g.set && g.set.ttl) | 0;
    if (gt > 0) { const e = ts > 0 ? ts + gt : t + gt; if (!exp || e < exp) exp = e; }
    return [exp, !exp || exp > t];
  }
  // Unanswered invitations from people we haven't accepted: 50 in all, 3 from one of them.
  function inviteRoom(from) { let all = 0, by = 0; for (const x of Object.values(st.groups)) if (x.req && !x.left && !x.tomb) { all++; if (x.by === from) by++; } return all < MAXINVITES && by < MAXINVITESPER; }
  // Bans (host/admin, local): removed by a manager; their link joins are refused. A direct add or approval lets them in again.
  const banned = (g, a) => (g.ban || []).includes(a);
  function addBan(g, as) { const b = (g.ban || []).slice(); for (const m of as) if (!b.includes(m)) b.push(m); g.ban = b.slice(-MAXBAN); if (!g.ban.length) delete g.ban; }
  function unban(g, a) { if (g.ban) { g.ban = g.ban.filter(m => m !== a); if (!g.ban.length) delete g.ban; } }
  // A leaving host hands the group to the co-admin we heard from last (most likely online), else the first.
  function pickHeir(g) {
    const ad = g.admins || []; if (!ad.length) return '';
    for (let i = st.messages.length - 1; i >= 0; i--) { const m = st.messages[i]; if (m.peer === 'g:' + g.id && !m.out && ad.includes(m.from)) return m.from; }
    return ad.slice().sort()[0];
  }
  // Our hand-over (the info naming the heir as host, from us) and then our leave, again to an heir that missed them; once a minute.
  const handAt = {};
  function handNotice(heir, g) {
    if (!g.hand || !g.quit || g.owner !== heir) return;
    const k = g.id + '|' + heir; if (Date.now() - (handAt[k] || 0) < 60000) return;
    if (Object.keys(handAt).length > 1024) for (const x in handAt) delete handAt[x];
    handAt[k] = Date.now();
    const full = Object.assign({}, g, { members: cleanMembers(g.members, addr) }); // the hand-over lists us; the gl then takes us out
    for (const p of infoPackets(full)) send(heir, p);
    setTimeout(() => send(heir, { t: 'gl', g: g.id, ts: now() }), 3000);
  }
  // A packet about a group we left: the heir gets our hand-over again, anyone else our leave.
  function leftNotice(peer, g) { if (!g || !g.quit) return; if (g.hand && peer === g.owner) { handNotice(peer, g); return; } leaveNoticeAt(peer, g.id, g.qat); }
  // Our gl again for a group we left lately (it may all have been lost while we were cut off). Posts from before
  // the leave that never got out get another push first; members on v0.19.2+ still take them by their time.
  async function repeatLeave(g) {
    const cs = st.messages.filter(m => m.hid && m.out && m.grp === g.id && m.status !== 'sent' && m.ts > 0 && m.ts <= g.qat);
    if (cs.length) { for (const m of cs) if (m.status === 'failed') m.status = 'queued'; saveSoon(); await flushBeforeLeave(g.id, 8000); failLeftCopies(g.id); }
    for (const m of g.members) { if (m === addr) continue; if (g.hand && m === g.owner) { handNotice(m, g); continue; } send(m, { t: 'gl', g: g.id, ts: g.qat }); }
  }
  // ---- signed group text posts (groupssig.go): sg = Ed25519(key, "ygg-gpost1\n"+gid+"\n"+postID+"\n"+ts+"\n"+hex(sha256(text))), base64 without padding ----
  const postSigMsg = (gid, pid, ts, text) => 'ygg-gpost1\n' + gid + '\n' + pid + '\n' + Math.trunc(Number(ts) || 0) + '\n' + hexOf(sha256s(new TextEncoder().encode(String(text))));
  function signPost(gid, pid, ts, text) { if (!text) return ''; try { const h = yggSign(postSigMsg(gid, pid, ts, text)); return h ? b64(hexBytes(h)).replace(/=+$/, '') : ''; } catch { return ''; } }
  // true / false (a forgery) / null (this browser can't check: treated as unsigned)
  async function verifyPost(by, pk, sg, gid, pid, ts, text) {
    pk = String(pk || '').toLowerCase(); sg = String(sg || '');
    if (!/^[0-9a-f]{64}$/.test(pk) || !/^[A-Za-z0-9+/]{86}$/.test(sg)) return false;
    const msg = postSigMsg(gid, pid, ts, text);
    if (typeof yggVerify === 'function') { try { return !!yggVerify(by, pk, msg, sg); } catch { return null; } }
    try { // an older ygg.wasm: WebCrypto Ed25519 where the browser has it
      if (!yggAddrOK(by, pk)) return false;
      const k = await crypto.subtle.importKey('raw', hexBytes(pk), { name: 'Ed25519' }, false, ['verify']);
      return await crypto.subtle.verify({ name: 'Ed25519' }, k, unb64(sg + '=='), new TextEncoder().encode(msg));
    } catch { return null; }
  }
  // Whether from's latest direct post in gid was signed (pk = its key) or not (''). Members only.
  function noteSigner(gid, from, pk) {
    const g = st.groups[gid]; if (!g || !hasM(g, from) || ((g.keys || {})[from] || '') === pk) return;
    const k = Object.assign({}, g.keys || {}); if (pk) k[from] = pk; else delete k[from];
    for (const m in k) if (!hasM(g, m)) delete k[m];
    if (Object.keys(k).length) g.keys = k; else delete g.keys;
    saveSoon();
  }
  // ---- group unsend / moderation (v0.19.4, groupunsend.go) ----
  // {t:'del', id, g, ref: post id, by: author, dby: deleter (not the author), pk: deleter's key, sg, ts}
  // sg = Ed25519(deleter's key, "ygg-gdel1\n"+gid+"\n"+postID+"\n"+author+"\n"+dby), dby '' for the author.
  // st.gdel["gid/author/postID"] = {x: until, at, d, pk, sg}; without sg it is local only (never passed on).
  const GDELMAX = 5000, GDELSYNC = 64;
  const gdelKey = (gid, by, pid) => gid + '/' + by + '/' + postIDf(String(pid));
  const gdelMsg = (gid, pid, by, dby) => 'ygg-gdel1\n' + gid + '\n' + postIDf(String(pid)) + '\n' + by + '\n' + (dby || '');
  // A short hash of every deleted post is kept 90 days (bounded) after the full tombstone (10 days) goes,
  // so an old peer relaying the post can't bring it back (v0.19.4, as groupunsend.go).
  const GDELHKEEP = 90 * 86400, GDELHMAX = 10000;
  const gdelHash = k => hexOf(sha256s(new TextEncoder().encode('ygg-gdelh\n' + k))).slice(0, 24);
  function markGDelH(k, n) {
    st.gdelh = st.gdelh || {};
    if (Object.keys(st.gdelh).length >= GDELHMAX) {
      for (const x in st.gdelh) if (st.gdelh[x] <= n) delete st.gdelh[x];
      const ks = Object.keys(st.gdelh); if (ks.length >= GDELHMAX) { ks.sort((a, b) => st.gdelh[a] - st.gdelh[b]); for (const x of ks.slice(0, ks.length - Math.floor(GDELHMAX * 0.9))) delete st.gdelh[x]; }
    }
    st.gdelh[gdelHash(k)] = n + GDELHKEEP;
  }
  const isGDel = (gid, by, pid) => { const k = gdelKey(gid, by, pid), t = st.gdel && st.gdel[k]; if (t && t.x > now()) return true; const h = st.gdelh && st.gdelh[gdelHash(k)]; return !!h && h > now(); };
  const mayDel = (g, deleter, author) => deleter === author || (canMng(g, deleter) && (deleter === g.owner || !canMng(g, author)));
  function markGDel(gid, by, pid, t) {
    const n = now(), k = gdelKey(gid, by, pid); st.gdel = st.gdel || {};
    const o = st.gdel[k]; if (o && o.x > n && (o.sg || !t.sg)) return; // a signed one replaces a local one
    if (Object.keys(st.gdel).length >= GDELMAX) {
      for (const x in st.gdel) if (st.gdel[x].x <= n) delete st.gdel[x];
      const ks = Object.keys(st.gdel); if (ks.length >= GDELMAX * 0.9) { ks.sort((a, b) => st.gdel[a].x - st.gdel[b].x); for (const x of ks.slice(0, ks.length - Math.floor(GDELMAX * 0.9) + 1)) delete st.gdel[x]; }
    }
    const e = { x: n + TOMBKEEP, at: t.at || n }; if (t.d) e.d = t.d; if (t.sg) { e.sg = t.sg; e.pk = t.pk; }
    st.gdel[k] = e; markGDelH(k, n);
  }
  async function verifySig(who, pk, sg, msg) { // true / false (forged) / null (can't check here)
    pk = String(pk || '').toLowerCase(); sg = String(sg || '');
    if (!/^[0-9a-f]{64}$/.test(pk) || !/^[A-Za-z0-9+/]{86}$/.test(sg)) return false;
    if (typeof yggVerify === 'function') { try { return !!yggVerify(who, pk, msg, sg); } catch { return null; } }
    try { if (!yggAddrOK(who, pk)) return false; const k = await crypto.subtle.importKey('raw', hexBytes(pk), { name: 'Ed25519' }, false, ['verify']); return await crypto.subtle.verify({ name: 'Ed25519' }, k, unb64(sg + '=='), new TextEncoder().encode(msg)); } catch { return null; }
  }
  function signDel(gid, pid, by) { try { const h = yggSign(gdelMsg(gid, pid, by, by === addr ? '' : addr)); return h ? [b64(hexBytes(h)).replace(/=+$/, ''), myPK()] : ['', '']; } catch { return ['', '']; } }
  // Removes author's post (and, for our own, the copies not delivered yet). Returns whether it was shown.
  // Also returns the Nostr boxes of the dropped copies (to recall) and the members whose copy went out as a box (v0.19.4).
  function dropGroupPost(gid, by, pid) {
    const key = 'g:' + gid, p = postIDf(String(pid)); let found = false; const refs = [], parked = new Set();
    st.messages = st.messages.filter(m => {
      if (m.peer === key && m.from === by && !m.sys && samePost(m.id, p)) { found = true; return false; }
      if (by === addr && m.hid && m.grp === gid && m.of === p) { if (m.nev) refs.push(...m.nev); if (m.nostr) parked.add(m.peer); return false; } // never goes out now
      return true;
    });
    return { found, refs, parked };
  }
  function groupUnsend(gid, m) {
    const g = st.groups[gid];
    if (!g || g.left) throw new Error('退出したグループでは自分の端末からだけ削除できます');
    if (!mayDel(g, addr, m.from)) throw new Error('他の人のメッセージを全員から削除できるのは、ホスト（と、一般メンバーの投稿なら管理者）だけです');
    const pid = postIDf(m.id), by = m.from, dby = by === addr ? '' : addr, [sg, pk] = signDel(gid, pid, by), t = now();
    markGDel(gid, by, pid, { at: t, d: dby, sg, pk });
    const dr = dropGroupPost(gid, by, pid);
    const base = ctlID(), to = g.members.filter(x => x !== addr);
    const goneC = (st.ctl || []).filter(c => (c.w.g && c.w.ts > 0 && c.w.ts < t - TOMBKEEP) || (c.w.g === gid && c.w.ref === pid && to.includes(c.peer)));
    st.ctl = (st.ctl || []).filter(c => !goneC.includes(c));
    // A member whose copy went out as a Nostr box gets the unsend that way too when it can't be reached (v0.19.4).
    to.forEach((p, i) => { const w = { t: 'del', id: base + '-' + i, g: gid, ref: pid, by, ts: t }; if (dby) w.dby = dby; if (sg) { w.sg = sg; w.pk = pk; } const c = { peer: p, w }; if (dr.parked.has(p)) c.park = true; st.ctl.push(c); });
    save(); emit('msg_del', { peer: 'g:' + gid, id: pid });
    nostrRecall(dr.refs.concat(ctlRefs(goneC)));
    let busy = 0; const q = to.slice(); const next = () => { while (busy < 48 && q.length) { busy++; flushCtl(q.shift()).finally(() => { busy--; next(); }); } }; next();
  }
  // An incoming group unsend (direct or relayed); true = ack (anything well-formed, so the sender stops).
  async function onGroupDel(src, w) {
    const okID = x => /^[0-9A-Za-z_-]{1,40}$/.test(String(x || ''));
    if (!okGID(w.g) || !okID(w.id) || !okID(w.ref)) return false;
    const by = norm(String(w.by || '')); if (!by || !isYgg(by)) return false;
    let deleter = by; if (w.dby) { deleter = norm(String(w.dby)); if (!deleter || !isYgg(deleter)) return false; }
    if (w.sg) { if (await verifySig(deleter, w.pk, w.sg, gdelMsg(w.g, w.ref, by, w.dby ? deleter : '')) !== true) return true; } // forged (or can't check): ignored
    else if (src !== deleter) return true; // unsigned: only straight from the deleter
    const g = st.groups[w.g];
    if (locked || !g || g.tomb || (src !== deleter && !hasM(g, src)) || !mayDel(g, deleter, by)) return true;
    const t = { d: w.dby ? deleter : '' }; if (w.sg) { t.sg = String(w.sg); t.pk = String(w.pk).toLowerCase(); }
    if (+w.ts > 0 && +w.ts <= now() + 600) t.at = Math.trunc(+w.ts);
    markGDel(w.g, by, w.ref, t);
    const { found, refs } = dropGroupPost(w.g, by, w.ref);
    if (found || refs.length) save(); else saveSoon();
    if (found) emit('msg_del', { peer: 'g:' + w.g, id: postIDf(String(w.ref)) });
    nostrRecall(refs); // moderation of our own post: our boxes of it go too
    return true;
  }
  // Signed tombstones of gid for a catch-up answer, newest first.
  function gdelWires(gid) {
    const n = now(), out = [];
    for (const k in st.gdel || {}) {
      const t = st.gdel[k]; if (!t.sg || t.x <= n || !k.startsWith(gid + '/')) continue;
      const rest = k.slice(gid.length + 1), i = rest.lastIndexOf('/'); if (i <= 0) continue;
      const w = { t: 'del', g: gid, by: rest.slice(0, i), ref: rest.slice(i + 1), pk: t.pk, sg: t.sg, ts: t.at }; if (t.d) w.dby = t.d;
      out.push(w);
    }
    return out.sort((a, b) => b.ts - a.ts).slice(0, GDELSYNC);
  }
  function sendGDels(to, ws) { let d = 2000; for (let r = 0; r < 2; r++) { ws.forEach((w, i) => { setTimeout(() => send(to, Object.assign({}, w, { id: ctlID() + '-' + i })), d); d += 40; }); d += 4000; } }
  // The signature and key to put on a relay of m (null when unknown).
  function sigFor(g, m) { if (!m.sg || m.img) return null; if (m.from === addr) return [m.sg, myPK()]; const k = g && g.keys && g.keys[m.from]; return k ? [m.sg, k] : null; }
  // A relayed version of a post (history from someone else, maybe altered) is replaced by the sender's own copy.
  async function replaceRelayed(key, from, w, img) {
    const ok = w.sg && !img ? await verifyPost(from, w.pk, w.sg, w.g, postIDf(w.id), w.ts, w.text) : false;
    let m = null; for (let i = st.messages.length - 1; i >= 0; i--) { const x = st.messages[i]; if (x.peer === key && x.from === from && !x.out && samePost(x.id, w.id)) { m = x; break; } }
    if (!m) return;
    m.text = String(w.text || ''); delete m.via; delete m.hist; delete m.sg;
    if (img) m.img = img; // the sender's own copy of a photo the relay only named
    const n = cleanNick(w.nick); if (n) m.nick = n;
    if (+w.ts > 0 && +w.ts <= now() + 300) m.ts = Math.trunc(+w.ts);
    if (ok === true && m.ts === w.ts) m.sg = String(w.sg); // only for the time it was signed with (r2)
    save();
    emit('msg_upd', Object.assign({ id: m.id, peer: m.peer, from: m.from, nick: m.nick, text: m.text, ts: m.ts, via: '', hist: false }, img ? { img } : {})); // spelled out: the UI merges it
    if (ok === true) noteSigner(w.g, from, String(w.pk).toLowerCase());
  }
  // The host tells a recently removed member that still posts or asks (its list without them), at most every 15 s.
  const goneAt_ = {};
  function goneNotice(peer, g) {
    if (g.owner !== addr || g.left || g.members.includes(peer) || !goneAt(g, peer)) return;
    const k = g.id + '|' + peer; if (Date.now() - (goneAt_[k] || 0) < 15000) return;
    if (Object.keys(goneAt_).length > 4096) for (const x in goneAt_) delete goneAt_[x];
    goneAt_[k] = Date.now();
    infoPackets(g).forEach((p, i) => { if (i === 0) { const x = Object.assign({}, p, { gr: packAddrs([peer]) }); if (goLen(x) <= DGRAM) p = x; } send(peer, p); }); // "gr": it is them who are out, so a list at the version they hold from an admin isn't taken as a collision (v0.19.3)
  }
  // Our undelivered copies of posts to a group we are out of: failed, never retried.
  function failLeftCopies(gid) {
    const ofs = new Set();
    for (const m of st.messages) if (m.hid && m.out && m.grp === gid && m.status !== 'sent' && m.status !== 'failed' && !m.nostr) { m.status = 'failed'; ofs.add(m.of); } // parked on Nostr: stays "stored" (may still be fetched)
    if (ofs.size) { save(); for (const of of ofs) groupStatus(gid, of); }
  }
  const leavingG = new Set();
  async function flushBeforeLeave(gid, wait) {
    const cs = st.messages.filter(m => m.hid && m.out && m.grp === gid && m.status !== 'sent' && m.status !== 'failed');
    if (!cs.length) return;
    leavingG.add(gid);
    try {
      await Promise.race([Promise.all(cs.map(async c => { for (let i = 0; i < 20 && inflight.has(c.id); i++) await new Promise(r => setTimeout(r, 250)); const x = st.messages.find(m => m.hid && m.peer === c.peer && m.id === c.id); if (x && x.status !== 'sent' && x.status !== 'failed') await deliver(x, 2); })), new Promise(r => setTimeout(r, wait))]);
    } finally { leavingG.delete(gid); }
  }
  function mergeAdds(g, list) { const out = []; for (const m of list) if (!g.members.includes(m) && !(g.gone || []).includes(m) && !out.includes(m) && g.members.length + out.length < gMax(g)) out.push(m); return out; }
  // Re-send gl to someone who still treats us as a member of a group we left, at most every 5 min.
  const glAt = {};
  // Twice a few seconds apart and at most once a minute: the first packets after a peer comes back often go nowhere (v0.19.3).
  function leaveNoticeAt(peer, gid, ts) { const k = gid + '|' + peer; if (Date.now() - (glAt[k] || 0) < 60000) return; if (Object.keys(glAt).length > 4096) for (const x in glAt) delete glAt[x]; glAt[k] = Date.now(); const w = { t: 'gl', g: gid, ts: ts || undefined }; send(peer, w); setTimeout(() => send(peer, w), 3000); }
  const cleanAdmins = (list, g) => [...new Set((list || []).map(x => norm(String(x || ''))).filter(a => a && isYgg(a) && a !== g.owner && g.members.includes(a)))].sort().slice(0, MAXADM);
  const jlen = w => u8len(JSON.stringify(w));
  function addrBytes(a) {
    const full = norm(a).split(':'), i = full.indexOf(''), p = i < 0 ? full : full.slice(0, i).concat(Array(8 - full.length + 1).fill('0'), full.slice(i + 1));
    const b = new Uint8Array(16); p.forEach((x, k) => { const v = parseInt(x || '0', 16); b[k * 2] = v >> 8; b[k * 2 + 1] = v & 255; }); return b;
  }
  const bytesAddr = b => { const p = []; for (let k = 0; k < 8; k++) p.push(((b[k * 2] << 8) | b[k * 2 + 1]).toString(16)); return norm(p.join(':')); };
  const packAddrs = as => { const b = new Uint8Array(16 * as.length); as.forEach((a, i) => b.set(addrBytes(a), i * 16)); return b64(b).replace(/=+$/, ''); };
  function unpackAddrs(s) {
    let b; try { b = unb64(String(s || '')); } catch { return null; }
    if (b.length % 16 || b.length > 16 * PARTN) return null;
    const out = []; for (let i = 0; i < b.length; i += 16) { const a = bytesAddr(b.subarray(i, i + 16)); if (a && isYgg(a)) out.push(a); } return out;
  }
  const chunkAddrs = as => { const o = []; for (let i = 0; i < as.length; i += PARTN) o.push(as.slice(i, i + PARTN)); return o.length ? o : [[]]; };
  const gHashS = (name, ms) => hexOf(sha256s(new TextEncoder().encode(name + '\n' + ms.slice().sort().join(',')))).slice(0, 8);
  function infoPackets(g) {
    const w = giWire(g);
    // The host/an admin says when the members it lists as gone dropped out ("gt", v0.19.3), when it still fits.
    const gt = canMng(g, addr) ? goneTimes(g) : null;
    if (goLen(w) <= GIMAX) { if (gt) { const x = Object.assign({}, w, { gt }); if (goLen(x) <= GIMAX) return [x]; } return [w]; }
    delete w.gm; w.gc = g.members.length; w.gh = gHashS(g.name, g.members);
    if (gt) { w.gt = gt; if (goLen(w) > DGRAM) delete w.gt; }
    const parts = chunkAddrs(g.members);
    return [w].concat(parts.map((p, i) => ({ t: 'gp', g: g.id, gv: g.v || undefined, gh: w.gh, i: i || undefined, n: parts.length, gmb: packAddrs(p) })));
  }
  function sendPackets(to, ps) {
    to = to.filter(m => m !== addr); if (!to.length || !ps.length) return;
    for (let r = 0; r < 3; r++) setTimeout(async () => { let n = 0; for (const m of to) for (const p of ps) { send(m, p); if (++n % 64 === 0) await sleep(20); } }, r * 2000);
  }
  // Group info that may not have arrived (as groupinv.go): the info sent on create/add is plain datagrams, and a
  // member that just came online often misses all three rounds. Sent again at about 8/28/58/118 s and on its hello until
  // any packet about the group comes from it. In memory, at most a week.
  const invAt = {}, invSent = {};
  function noteInvited(gid, ms) {
    if (Object.keys(invAt).length > 4096) for (const x in invAt) { delete invAt[x]; delete invSent[x]; }
    ms = (ms || []).filter(m => m && m !== addr); if (!ms.length) return;
    for (const m of ms) { const k = gid + '|' + m; if (!invAt[k]) invAt[k] = Date.now(); invSent[k] = Date.now(); }
    const gaps = [8000, 20000, 30000, 60000];
    let i = 0; const step = () => { if (i >= gaps.length) return; setTimeout(() => { let left = 0; for (const m of ms) if (pushInvite(gid, m)) left++; i++; if (left) step(); }, gaps[i]); };
    step();
  }
  function invHeard(gid, peer) { const k = gid + '|' + peer; if (invAt[k]) { delete invAt[k]; delete invSent[k]; } }
  function pushInvite(gid, peer) {
    const k = gid + '|' + peer, at = invAt[k];
    if (at && Date.now() - at > 7 * 86400000) { invHeard(gid, peer); return false; }
    if (!at) return false;
    if (Date.now() - (invSent[k] || 0) < 8000) return true;
    invSent[k] = Date.now();
    const g = st.groups[gid];
    if (!g || g.left || locked || !canMng(g, addr) || !g.members.includes(peer)) { invHeard(gid, peer); return false; }
    for (const p of infoPackets(g)) send(peer, p);
    return true;
  }
  function pushInvites(peer) { for (const k of Object.keys(invAt)) if (k.endsWith('|' + peer)) pushInvite(k.slice(0, k.length - peer.length - 1), peer); }
  function gInfo(g, extra) {
    const ps = infoPackets(g);
    if (ps.length === 1) { sendPackets(g.members.concat(extra || []), ps); return; }
    sendPackets(g.members, ps.slice(0, 1)); sendPackets(extra || [], ps);
  }
  // kick: removed by a manager (not a leave), so other managers refuse their link joins too. The classic gi
  // also carries what was removed ("gr"), so a host merging two admins' same-version changes keeps both (v0.19.3).
  function gMembers(g, added, removed, kick) {
    let ps = infoPackets(g);
    if (ps.length === 1) {
      if (removed.length) { const x = Object.assign({}, ps[0], { gr: packAddrs(removed) }); if (kick) x.k = 'kick'; if (goLen(x) <= DGRAM) ps = [x]; }
      sendPackets(g.members.concat(removed), ps); noteInvited(g.id, added); return;
    }
    const old = g.members.filter(m => !added.includes(m)), gh = gHashS(g.name, g.members), ds = [];
    for (const c of chunkAddrs(added)) if (c.length) ds.push({ t: 'gd', g: g.id, gv: g.v, ga: packAddrs(c), gh, ts: g.vat || undefined });
    for (const c of chunkAddrs(removed)) if (c.length) ds.push({ t: 'gd', g: g.id, gv: g.v, gr: packAddrs(c), gh, ts: g.vat || undefined, k: kick ? 'kick' : undefined });
    sendPackets(old.concat(removed), ds); sendPackets(added, ps); noteInvited(g.id, added);
  }
  const qAt = {}, rAt = {}, pbufs = {};
  const capMap = o => { if (Object.keys(o).length > 4096) for (const x in o) delete o[x]; }; // strangers can make keys up (v0.19.3)
  function askInfo(peer, gid) { const k = gid + '|' + peer; if (Date.now() - (qAt[k] || 0) < 15000) return; capMap(qAt); qAt[k] = Date.now(); send(peer, { t: 'gq', g: gid }); }
  function answerInfo(peer, g) { const k = g.id + '|' + peer; if (Date.now() - (rAt[k] || 0) < 10000) return; capMap(rAt); rAt[k] = Date.now(); for (const p of infoPackets(g)) send(peer, p); }
  function pbuf(src, gid, v, gh) {
    const k = gid + '|' + src; let b = pbufs[k];
    if (!b || b.v !== (v | 0) || b.gh !== gh || Date.now() - b.at > 60000) {
      b = { v: v | 0, gh, n: 0, parts: {}, hdr: null, at: Date.now() };
      // Drop stale buffers and keep their number bounded: anyone can send parts for made-up group ids (v0.19.3).
      let mine = 0, oldK = '', oldAt = 0, oldMine = '', oldMineAt = 0;
      for (const [kk, x] of Object.entries(pbufs)) {
        if (Date.now() - x.at > 120000) { delete pbufs[kk]; continue; }
        if (!oldK || x.at < oldAt) { oldK = kk; oldAt = x.at; }
        if (kk.endsWith('|' + src)) { mine++; if (!oldMine || x.at < oldMineAt) { oldMine = kk; oldMineAt = x.at; } }
      }
      if (mine >= 4 && oldMine) delete pbufs[oldMine]; else if (Object.keys(pbufs).length >= 64 && oldK) delete pbufs[oldK];
      pbufs[k] = b;
    }
    return b;
  }
  function partsDone(src, b) {
    if (!b.hdr || !b.n || Object.keys(b.parts).length < b.n) return null;
    let ms = []; for (let i = 0; i < b.n; i++) ms = ms.concat(b.parts[i]);
    const w = Object.assign({}, b.hdr);
    if (ms.length !== (w.gc | 0) || gHashS(cleanNick(w.gn) || 'グループ', ms) !== w.gh) return null;
    w.gm = ms; delete w.gc;
    for (const [k, x] of Object.entries(pbufs)) if (x === b) delete pbufs[k];
    return w;
  }
  function onInfoHeader(src, w) {
    const gc = w.gc | 0; if (gc <= 0 || gc > GCAP || !w.gh) return;
    const g = st.groups[w.g];
    if (g && gHashS(cleanNick(w.gn) || 'グループ', g.members) === w.gh) { onGroupCtl(src, Object.assign({}, w, { gm: g.members.slice(), gc: undefined })); return; }
    const b = pbuf(src, w.g, w.gv, w.gh), first = !b.hdr; b.hdr = w;
    const full = partsDone(src, b); if (full) { onGroupCtl(src, full); return; }
    if (!first) return; // a repeat: the check below is already scheduled
    setTimeout(() => { if (pbufs[w.g + '|' + src] === b) askInfo(src, w.g); }, 5000);
  }
  function onInfoPart(src, w) {
    const n = w.n | 0, i = w.i | 0;
    if (!okGID(w.g) || n <= 0 || n > Math.ceil(GCAP / PARTN) || i < 0 || i >= n) return;
    const as = unpackAddrs(w.gmb); if (!as) return;
    const b = pbuf(src, w.g, w.gv, w.gh); if (!b.n) b.n = n; if (b.n !== n) return;
    b.parts[i] = as;
    const full = partsDone(src, b); if (full) onGroupCtl(src, full);
  }
  function memberSys(g, by, added, removed) {
    const who = gNameOf(g, by);
    const line = (list, verb) => { const o = list.filter(m => m !== addr && m !== by); if (o.length > 5) { gSys(g, who + ' が ' + o.length + '人を' + verb + 'ました'); return; } for (const m of o) gSys(g, who + ' が ' + gNameOf(g, m) + ' を' + verb + 'ました'); };
    line(added, '追加し'); line(removed, '外し');
  }
  function adminSys(g, by, old, now_) {
    const who = gNameOf(g, by);
    for (const x of now_) if (!(old || []).includes(x)) gSys(g, who + ' が ' + gNameOf(g, x) + ' を副管理者にしました');
    for (const x of old || []) if (!now_.includes(x) && g.members.includes(x)) gSys(g, who + ' が ' + gNameOf(g, x) + ' を副管理者から外しました');
  }
  function onDelta(src, w) {
    const g = st.groups[w.g];
    // A host that left (no heir) is still the owner on file but no longer runs the group (v0.19.3).
    if (!g || g.left || !canMng(g, src) || !hasM(g, src) || !g.members.includes(addr)) return;
    const host = g.owner === addr, gv = w.gv | 0;
    // An admin far ahead of us: the host's list decides, so an admin can't push the version out of reach (v0.19.3).
    if (!host && src !== g.owner && gv > (g.v | 0) + MAXVJUMP) { askInfo(g.owner, g.id); return; }
    // An admin's delta the host gets late or from an old base is merged into a new version:
    // its additions (not ones removed since), its removals only at the same version (v0.19.2).
    const merge = host && gv <= (g.v | 0);
    if (gv < (g.v | 0) && !merge) return;
    let add = unpackAddrs(w.ga) || [], rm = unpackAddrs(w.gr) || [];
    if (merge) { add = mergeAdds(g, add); if (gv < (g.v | 0)) rm = []; }
    const byHost = src === g.owner;
    const added = [], removed = [];
    let ms = g.members.slice();
    for (const x of add) if (!ms.includes(x) && ms.length < GCAP) { ms.push(x); added.push(x); }
    ms = ms.filter(m => { const d = rm.includes(m) && (byHost || (m !== g.owner && !isAdm(g, m))); if (d) removed.push(m); return !d; });
    if (!added.length && !removed.length && gv <= (g.v | 0)) return; // a repeat
    const out = removed.includes(addr);
    if (!byHost && ms.length > gMax(g)) { if (!host) askInfo(g.owner, g.id); return; } // an admin can't take the group over its limit (v0.19.3)
    // Same version, different change: the host merges (the host removing us is final even then, v0.19.3).
    if (!host && gv === (g.v | 0) && !(byHost && out)) { if (src === g.owner) answerInfo(g.owner, g); else askInfo(g.owner, g.id); return; }
    const before = g.members;
    g.members = ms.sort(); g.admins = cleanAdmins(g.admins, g);
    let at = 0;
    if (merge) bumpV(g); else { g.v = gv; at = changeTime(w.ts); if (at) g.vat = at; else delete g.vat; } // 0 = unknown (an older sender): never pass on a stale time
    if (host) trackGone(g, before);
    noteGone(g, before, at);
    const dels = purgeGone(g, before); // what they posted after the removal reached us late (v0.19.3)
    const kick = w.k === 'kick' || (merge && removed.length > 0);
    if (kick && canMng(g, addr)) addBan(g, removed);
    if (g.pend) { g.pend = g.pend.filter(p => !added.includes(p.addr)); if (!g.pend.length) delete g.pend; }
    if (out) g.left = true;
    save(); emit('contact', gView(g)); emitDels(dels);
    if (out) { gSys(g, 'グループから外されました'); failLeftCopies(g.id); return; }
    memberSys(g, src, added, removed);
    if (merge) { gMembers(g, added, removed, kick); return; } // everyone (the admin too) gets the merged version
    if (g.owner === addr && !byHost && (added.length || removed.length)) { const ps = infoPackets(g); if (ps.length === 1) sendPackets(g.members.concat(removed), ps); }
    const v = w.gv | 0, gh = w.gh;
    setTimeout(() => { const x = st.groups[w.g]; if (x && !x.left && (x.v | 0) === v && gHashS(x.name, x.members) !== gh) askInfo(src, w.g); }, 6000);
  }
  function sendKey(g, to) { sendPackets(to, [g.pwh ? { t: 'gk', g: g.id, pw: g.pwh } : { t: 'gk', g: g.id, k: 'none' }]); }
  function onKey(src, w) {
    const g = st.groups[w.g];
    if (!g || g.left || !g.owner || src !== g.owner || !isAdm(g, addr) || String(w.pw || '').length > 200) return;
    if (!w.pw || w.k === 'none') delete g.pwh; else g.pwh = String(w.pw);
    save();
  }
  // Limit how many member copies are being delivered at once. A copy gets a few direct
  // tries, then waits in the queue (hello/flush retry it), so offline members don't hog the slots (v0.19.2).
  let gBusy = 0; const gWait = [];
  async function deliverCopy(c) {
    if (gBusy >= 48) await new Promise(r => gWait.push(r));
    gBusy++;
    try { return await deliver(c, COPYTRIES); } finally { gBusy--; const n = gWait.shift(); if (n) n(); }
  }
  function dropCopy(peer, id) {
    const i = st.messages.findIndex(m => m.hid && m.peer === peer && m.id === id); if (i < 0) return;
    const m = st.messages[i]; st.messages.splice(i, 1); saveSoon(); groupStatus(m.grp, m.of);
  }

  // ---- v0.19: light proof of work on posts (see pow.go) ----
  const POWDEF = 3, POWREQ = 8, POWMAX = 20;
  const gPow = g => { const p = (g && g.set && g.set.pb) | 0; return p < 0 ? 0 : p === 0 ? POWDEF : p; };
  const K256 = new Uint32Array([0x428a2f98, 0x71374491, 0xb5c0fbcf, 0xe9b5dba5, 0x3956c25b, 0x59f111f1, 0x923f82a4, 0xab1c5ed5, 0xd807aa98, 0x12835b01, 0x243185be, 0x550c7dc3, 0x72be5d74, 0x80deb1fe, 0x9bdc06a7, 0xc19bf174, 0xe49b69c1, 0xefbe4786, 0x0fc19dc6, 0x240ca1cc, 0x2de92c6f, 0x4a7484aa, 0x5cb0a9dc, 0x76f988da, 0x983e5152, 0xa831c66d, 0xb00327c8, 0xbf597fc7, 0xc6e00bf3, 0xd5a79147, 0x06ca6351, 0x14292967, 0x27b70a85, 0x2e1b2138, 0x4d2c6dfc, 0x53380d13, 0x650a7354, 0x766a0abb, 0x81c2c92e, 0x92722c85, 0xa2bfe8a1, 0xa81a664b, 0xc24b8b70, 0xc76c51a3, 0xd192e819, 0xd6990624, 0xf40e3585, 0x106aa070, 0x19a4c116, 0x1e376c08, 0x2748774c, 0x34b0bcb5, 0x391c0cb3, 0x4ed8aa4a, 0x5b9cca4f, 0x682e6ff3, 0x748f82ee, 0x78a5636f, 0x84c87814, 0x8cc70208, 0x90befffa, 0xa4506ceb, 0xbef9a3f7, 0xc67178f2]);
  const W256 = new Uint32Array(64);
  function sha256s(msg) { // synchronous SHA-256 (WebCrypto is async-only; PoW needs many small hashes)
    const l = msg.length, nb = ((l + 9 + 63) >> 6) << 6, p = new Uint8Array(nb); p.set(msg); p[l] = 0x80;
    const bits = l * 8; p[nb - 4] = bits >>> 24; p[nb - 3] = bits >>> 16; p[nb - 2] = bits >>> 8; p[nb - 1] = bits; p[nb - 5] = Math.floor(l / 0x20000000);
    let h0 = 0x6a09e667, h1 = 0xbb67ae85, h2 = 0x3c6ef372, h3 = 0xa54ff53a, h4 = 0x510e527f, h5 = 0x9b05688c, h6 = 0x1f83d9ab, h7 = 0x5be0cd19;
    const W = W256;
    for (let o = 0; o < nb; o += 64) {
      for (let i = 0; i < 16; i++) W[i] = (p[o + i * 4] << 24) | (p[o + i * 4 + 1] << 16) | (p[o + i * 4 + 2] << 8) | p[o + i * 4 + 3];
      for (let i = 16; i < 64; i++) { const a = W[i - 15], b = W[i - 2]; W[i] = (((a >>> 7) | (a << 25)) ^ ((a >>> 18) | (a << 14)) ^ (a >>> 3)) + W[i - 16] + (((b >>> 17) | (b << 15)) ^ ((b >>> 19) | (b << 13)) ^ (b >>> 10)) + W[i - 7]; }
      let a = h0, b = h1, c = h2, d = h3, e = h4, f = h5, g = h6, h = h7;
      for (let i = 0; i < 64; i++) {
        const t1 = (h + (((e >>> 6) | (e << 26)) ^ ((e >>> 11) | (e << 21)) ^ ((e >>> 25) | (e << 7))) + ((e & f) ^ (~e & g)) + K256[i] + W[i]) | 0;
        const t2 = ((((a >>> 2) | (a << 30)) ^ ((a >>> 13) | (a << 19)) ^ ((a >>> 22) | (a << 10))) + ((a & b) ^ (a & c) ^ (b & c))) | 0;
        h = g; g = f; f = e; e = (d + t1) | 0; d = c; c = b; b = a; a = (t1 + t2) | 0;
      }
      h0 = (h0 + a) | 0; h1 = (h1 + b) | 0; h2 = (h2 + c) | 0; h3 = (h3 + d) | 0; h4 = (h4 + e) | 0; h5 = (h5 + f) | 0; h6 = (h6 + g) | 0; h7 = (h7 + h) | 0;
    }
    const out = new Uint8Array(32); [h0, h1, h2, h3, h4, h5, h6, h7].forEach((x, i) => { out[i * 4] = x >>> 24; out[i * 4 + 1] = x >>> 16; out[i * 4 + 2] = x >>> 8; out[i * 4 + 3] = x; });
    return out;
  }
  const zbits = h => { let n = 0; for (const b of h) { if (!b) { n += 8; continue; } return n + Math.clz32(b) - 24; } return n; };
  const powPrefix = (scope, id, ts, text, sh) => new TextEncoder().encode('ygg-pow1\n' + scope + '\n' + id + '\n' + Math.trunc(Number(ts) || 0) + '\n' + hexOf(sha256s(new TextEncoder().encode(String(text || '') + '\0' + (sh || '')))) + '\n');
  function powBits(scope, id, ts, text, sh, nonce) {
    nonce = String(nonce || ''); if (!nonce || nonce.length > 20) return -1;
    const p = powPrefix(scope, id, ts, text, sh), n = new TextEncoder().encode(nonce), b = new Uint8Array(p.length + n.length); b.set(p); b.set(n, p.length);
    return zbits(sha256s(b));
  }
  function powSolve(scope, id, ts, text, sh, need) {
    if (need <= 0) return '';
    const p = powPrefix(scope, id, ts, text, sh), b = new Uint8Array(p.length + 20); b.set(p);
    for (let n = 0; ; n++) {
      const s = n.toString(36); for (let i = 0; i < s.length; i++) b[p.length + i] = s.charCodeAt(i);
      if (zbits(sha256s(b.subarray(0, p.length + s.length))) >= need) return s;
    }
  }
  const postID = id => { const i = String(id).lastIndexOf('g'); return i > 0 ? String(id).slice(0, i) : String(id); };
  const powScope = (from, w, to) => w.g ? from + '>g:' + w.g : from + '>' + to;
  const powNeed = {};
  function onPowNeed(src, w) { const n = w.n | 0; if (n <= 0 || n > POWMAX || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return; const k = src + '/' + w.id; if (Object.keys(powNeed).length > 4096) for (const x in powNeed) delete powNeed[x]; if (n > (powNeed[k] | 0)) powNeed[k] = n; }
  function sendBits(m) {
    let need = POWDEF;
    if (m.grp) need = gPow(st.groups[m.grp]);
    else { const c = st.contacts[m.peer]; if (!c || !c.hasprof) need = POWREQ; }
    return Math.max(need, powNeed[m.peer + '/' + m.id] | 0);
  }
  function withPow(m, w, sh) {
    const need = sendBits(m); if (need <= 0) return;
    const scope = powScope(addr, w, m.peer), id = m.grp ? postID(m.id) : m.id, text = m.textFile ? '' : m.text;
    if (m.pn && powBits(scope, id, m.ts, text, sh, m.pn) >= need) { w.pn = m.pn; return; }
    w.pn = powSolve(scope, id, m.ts, text, sh, need);
    const x = st.messages.find(y => y.peer === m.peer && y.id === m.id); if (x) { x.pn = w.pn; saveSoon(); }
  }
  function recvBits(src, w) {
    if (w.g) {
      // An unknown group, a placeholder or an unanswered invitation from someone we haven't
      // accepted is a message request: request work, PowStrict applies (v0.19.2).
      const g = st.groups[w.g], c = st.contacts[src], acc = !!(c && !c.req);
      if (!g) return acc ? [POWDEF, true] : [POWREQ, !st.powStrict];
      const pb = (g.set && g.set.pb) | 0;
      if ((g.stub || g.req) && !acc) return [Math.max(gPow(g), POWREQ), !st.powStrict && !pb];
      return [gPow(g), !pb];
    }
    const c = st.contacts[src]; if (c && !c.req) return [POWDEF, true];
    return [POWREQ, !st.powStrict];
  }
  function powCheck(src, w) {
    if (w._checked) return true;
    const [need, legacyOK] = recvBits(src, w); if (need <= 0) return true;
    if (!w.pn && legacyOK) return true; // pre-v0.19 sender
    if (powBits(powScope(src, w, addr), w.g ? postID(w.id) : w.id, w.ts, w.text, w.sha, w.pn) >= need) return true;
    send(src, { t: 'pow', id: w.id, n: need }); return false;
  }
  const joinPow = (from, gid, tok) => powSolve(from + '>j:' + gid, tok, 0, '', '', POWREQ);
  const joinPowOK = (from, gid, tok, nonce) => powBits(from + '>j:' + gid, tok, 0, '', '', nonce) >= POWREQ;

  function groupStatus(gid, id) {
    const cs = st.messages.filter(m => m.hid && m.grp === gid && m.of === id);
    const n = cs.length, sent = cs.filter(m => m.status === 'sent').length, sending = cs.filter(m => m.status === 'sending').length, stored = cs.filter(m => m.status === 'stored').length, failed = cs.filter(m => m.status === 'failed').length;
    const v0 = st.messages.find(m => m.peer === 'g:' + gid && m.id === id);
    const s = (!n || sent || (v0 && v0.dlv)) ? 'sent' : sending ? 'sending' : stored ? 'stored' : failed === n ? 'failed' : 'queued'; // delivered to anyone = sent (v0.19.1); failed = we left first
    const v = st.messages.find(m => m.peer === 'g:' + gid && m.id === id);
    if (v && (v.status !== s || v.dlv)) { const ch = v.status !== s; v.status = s; if (ch) saveSoon(); emit('status', { id, peer: v.peer, status: s, dlv: v.dlv | 0 }); }
  }
  function msgPktBare(m) {
    const w = { t: 'msg', id: m.id, nick: m.nick, text: m.text, ts: m.ts, ttl: m.ttl || undefined };
    if (m.grp) { w.g = m.grp; const g = st.groups[m.grp]; if (g) w.gh = gHashS(g.name, g.members); if (m.sg) { w.sg = m.sg; w.pk = myPK(); } }
    return w;
  }
  async function msgPkt(m) { const w = msgPktBare(m); withPow(m, w, ''); return w; }
  // Go's encoder escapes <>& as \u003c etc.: count like it so both sides agree on sizes.
  const goLen = w => jlen(w) + 5 * (JSON.stringify(w).match(/[<>&\u2028\u2029]/g) || []).length;
  const longText = m => !m.img && !m.gimg && goLen(msgPktBare(m)) > DGRAM - 40;
  const MAXSEND = 12288, MAXRECV = 16384, TEXTMIME = 'text/plain';
  function sendGroup(gid, text, img) {
    const g = st.groups[gid];
    if (!g || g.left) return null;
    if (img && g.set && g.set.nf) return null;
    if (g.req) { g.req = false; emit('contact', gView(g)); }
    const id = Date.now().toString(16) + Math.random().toString(16).slice(2, 6), ts = now();
    const ttl = (g.set && g.set.ttl) | 0, tx = ttl ? { ttl, exp: ts + ttl } : {};
    const v = Object.assign({ id, peer: 'g:' + gid, from: addr, nick: st.nick, text, ts, out: true, status: 'sending' }, tx);
    if (img) v.img = img;
    else { const sg = signPost(gid, id, ts, text); if (sg) v.sg = sg; } // relays can then prove it is ours (v0.19.3)
    // The photo stays on the visible message only (gimg on the copies).
    const cs = g.members.map((a, i) => [a, i]).filter(([a]) => a !== addr).map(([a, i]) => Object.assign({ id: id + 'g' + i, peer: a, from: addr, nick: st.nick, text, ts, out: true, status: 'sending', grp: gid, of: id, hid: true, gimg: img ? true : undefined, sg: v.sg }, tx));
    if (!cs.length) v.status = 'sent';
    if (cs.length) v.tot = cs.length; // recipients at send time (for 届いた n/m)
    const need = gPow(g);
    if (need && cs.length) {
      // One nonce for the post, shared by every member copy. A long text goes as a file
      // (text '', sha of the UTF-8 text), so solve it the way that is checked (v0.19.2);
      // copy ids differ in length, so a text right at the limit may need both.
      const scope = addr + '>g:' + gid, pns = {};
      let sh = ''; if (img) { const pi = parseImg(img); if (pi) sh = hexOf(sha256s(pi.bytes)); }
      for (const c of cs) {
        const long = !img && longText(c);
        if (!(long in pns)) pns[long] = long ? powSolve(scope, id, ts, '', hexOf(sha256s(new TextEncoder().encode(text))), need) : powSolve(scope, id, ts, text, sh, need);
        c.pn = pns[long];
      }
    }
    st.messages.push(...cs);
    addMessage(v);
    for (const c of cs) deliverCopy(c);
    return v;
  }
  async function onGroupMsg(src, w, img) {
    if (!okGID(w.g) || (w.text != null && typeof w.text !== 'string') || (!w.text && !img) || u8len(w.text) > MAXRECV || !/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) return false;
    if (locked || st.blocked[src]) return false;
    let g = st.groups[w.g], ask = false;
    const known = !!g;
    if (!g) {
      // A group we've never heard of is a message request: request PoW first, and only so many placeholders (v0.19.2).
      if (Object.values(st.groups).filter(x => x.stub && !x.left).length >= MAXSTUBS || !powCheck(src, w)) return false;
      const c = st.contacts[src];
      g = st.groups[w.g] = { id: w.g, name: 'グループ', members: cleanMembers([src], addr), req: !(c && c.shared && !c.req), stub: true };
      ask = true; save(); emit('contact', gView(g));
    } else if (g.left) { leftNotice(src, g); return true; } // we're out: ack and drop (and re-tell a missed gl / hand-over)
    let late = false;
    if (!g.left && !hasM(g, src)) {
      const at = goneAt(g, src);
      if (!at) { askInfo(src, w.g); if (g.owner && g.owner !== src && g.owner !== addr) askInfo(g.owner, w.g); return false; }
      // A member who left or was removed: a post made before that still counts (only for a while and only so many,
      // v0.19.3); a later one is acked and dropped so they stop retrying, and the host tells them they are out.
      if (!lateOK(g, src, w.ts)) { goneNotice(src, g); return true; }
      late = true;
    }
    else if (known && w.gh && w.gh !== gHashS(g.name, g.members)) ask = true;
    if (ask) { askInfo(src, w.g); if (g.owner && g.owner !== src && g.owner !== addr) askInfo(g.owner, w.g); }
    const key = 'g:' + w.g;
    if (isGDel(w.g, src, w.id)) return true; // unsent (or deleted here): a late copy must not bring it back (v0.19.4)
    const ex = st.messages.find(m => m.peer === key && m.from === src && samePost(m.id, w.id));
    if (late && !ex && !lateTake(w.g, src)) return true; // too many late posts from a former member
    const nf = !!(g.set && g.set.nf);
    // We hold it as someone's relay: the sender's own copy is the real one (v0.19.3).
    // A relayed photo is only a "📷" line, so the photo itself replaces it too.
    if (ex && !ex.out && (ex.via || ex.hist) && (ex.text !== w.text || ex.via || (img && !ex.img)) && !(img && nf && !w.text)) { if (!powCheck(src, w)) return false; await replaceRelayed(key, src, w, nf ? '' : img); return true; }
    if (ex) return true;
    if (!powCheck(src, w)) return false;
    if (img && nf) { if (!w.text) return true; img = ''; } // photos off: ack and drop
    let ttl = w.ttl | 0; const gt = (g.set && g.set.ttl) | 0;
    if (gt && (ttl <= 0 || ttl > gt)) ttl = gt; if (ttl < 0 || ttl > MAXTTL) ttl = 0;
    const m = { id: postIDf(w.id), peer: key, from: src, nick: cleanNick(w.nick), text: w.text || '', ts: inTs(w.ts), out: false }; // stored under the post id
    if (img) m.img = img;
    if (ttl) { m.ttl = ttl; m.exp = now() + ttl; }
    if (!img) {
      // A signed text post: keep the signature to pass on with relays; the sender is known to sign while its posts are signed (v0.19.3).
      // (a clamped time no longer matches what was signed: relaying it with the signature made others drop it as forged, r2)
      let pk = ''; if (w.sg && await verifyPost(src, w.pk, w.sg, w.g, postIDf(w.id), w.ts, w.text) === true) { if (m.ts === w.ts) m.sg = String(w.sg); pk = String(w.pk).toLowerCase(); }
      if (st.messages.some(x => x.peer === key && x.from === src && samePost(x.id, w.id))) return true; // landed while checking
      noteSigner(w.g, src, pk);
    }
    addMessage(m);
    return true;
  }
  // ---- group invite links (v0.17, see invite.go): base64url(host 16 B | gid 8 B | token 8 B) ----
  // Our pending invite-link joins, saved with an expiry (v0.19.2): st.joins[gid] = {host, exp}.
  const JOINTTL = 600, JOINAPPROVE = 86400;
  const joinHost = gid => { const j = (st.joins || {})[gid]; if (!j) return ''; if (j.exp < now()) { delete st.joins[gid]; return ''; } return j.host; };
  function setJoin(gid, host, ttl) { st.joins = st.joins || {}; const t = now(); for (const k in st.joins) if (st.joins[k].exp < t) delete st.joins[k]; st.joins[gid] = { host, exp: t + ttl, wait: ttl > JOINTTL || undefined }; save(); emit('joins', joinsView()); }
  function delJoin(gid) { if (st.joins && st.joins[gid]) { delete st.joins[gid]; save(); emit('joins', joinsView()); } }
  // Our join attempts per group: an older attempt's retries stop, and "password needed" can't
  // answer a request that carries the password (it was for an older one); else it dropped the
  // new join and the approval arrived as a plain invitation.
  const joinTries = {};
  const JOINPWNEEDED = 'このグループに入るにはパスワードが必要です';
  // Pending joins for the UI (same shape as Go's /api/state "joins").
  const joinsView = () => Object.entries(st.joins || {}).filter(([, j]) => j.exp >= now()).sort().map(([gid, j]) => { const v = { addr: 'g:' + gid, host: j.host, exp: j.exp, wait: !!j.wait }; const c = st.contacts[j.host]; if (c && c.nick) v.hostnick = c.nick; return v; });
  const b64u = b => btoa(String.fromCharCode(...b)).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  const unb64u = s => Uint8Array.from(atob(String(s).replace(/-/g, '+').replace(/_/g, '/')), c => c.charCodeAt(0));
  const hexBytes = h => Uint8Array.from(String(h).match(/../g) || [], x => parseInt(x, 16));
  function encodeJoin(host, gid, tok) {
    const full = norm(host).split(':'), i = full.indexOf(''), groups = i < 0 ? full : full.slice(0, i).concat(Array(8 - full.length + 1).fill('0'), full.slice(i + 1));
    const b = new Uint8Array(32);
    groups.forEach((x, k) => { const v = parseInt(x || '0', 16); b[k * 2] = v >> 8; b[k * 2 + 1] = v & 255; });
    b.set(hexBytes(gid), 16); b.set(hexBytes(tok), 24);
    return b64u(b);
  }
  function decodeJoin(code) {
    let b; try { b = unb64u(code); } catch { b = null; }
    if (!b || b.length !== 32) throw new Error('招待リンクが壊れています');
    const parts = []; for (let k = 0; k < 8; k++) parts.push(((b[k * 2] << 8) | b[k * 2 + 1]).toString(16));
    const host = norm(parts.join(':'));
    if (!host || !isYgg(host)) throw new Error('招待リンクのアドレスが不正です');
    return { host, gid: hexOf(b.slice(16, 24)), tok: hexOf(b.slice(24)) };
  }
  async function onJoin(src, w) {
    const g = st.groups[w.g];
    if (!g || g.left || !canMng(g, addr) || !g.tok || w.tok !== g.tok || st.blocked[src]) { send(src, { t: 'gjn', g: w.g, text: 'この招待リンクは使えません（リセットされたか、グループがなくなりました）' }); return; }
    if (hasM(g, src)) { answerInfo(src, g); return; }
    // removed by the host or an admin: only a direct add (or approval of a request made before) brings them back (v0.19.3)
    if (banned(g, src)) { send(src, { t: 'gjn', g: w.g, text: 'このグループからは外されているため、リンクからは参加できません' }); return; }
    if (g.members.length >= gMax(g)) { send(src, { t: 'gjn', g: w.g, text: 'グループが満員です（最大' + gMax(g) + '人）' }); return; }
    const nick = cleanNick(w.nick);
    if ((w.pn || st.powStrict) && !joinPowOK(src, w.g, String(w.tok || ''), w.pn)) { send(src, { t: 'gjn', g: w.g, text: '参加にはYggChat v0.19.0以上が必要です（PoW）' }); return; }
    if (g.set && g.set.pw && !g.pwh) { send(src, { t: 'gjn', g: w.g, text: 'いまは参加を受け付けられません。少ししてからもう一度開いてね' }); send(g.owner, { t: 'gkq', g: g.id }); return; }
    if (g.pwh) {
      if (pwLimited(src, false)) { send(src, { t: 'gjn', g: w.g, text: 'パスワードを何度も間違えたので、しばらく参加できません' }); return; }
      if (w.pw && !pwGlobalOK()) { send(src, { t: 'gjn', g: w.g, text: 'いま参加が混み合っています。少ししてからもう一度開いてね' }); return; }
      if (!await checkPass(g.pwh, String(w.pw || ''))) { if (w.pw) pwLimited(src, true); send(src, { t: 'gjn', g: w.g, k: 'pw', text: w.pw ? 'パスワードが違います' : JOINPWNEEDED }); return; }
    }
    if (g.set && g.set.ap) {
      const fresh = !(g.pend || []).some(p => p.addr === src);
      if (fresh && (g.pend || []).length >= 20) { g.pend = g.pend.filter(p => now() - (p.ts || 0) < PENDKEEP); if (!g.pend.length) delete g.pend; } // requests nobody answered for days make room (v0.19.3)
      if (fresh) { if ((g.pend || []).length >= 20) { send(src, { t: 'gjn', g: w.g, text: '参加申請がいっぱいです。しばらくしてからもう一度開いてね' }); return; } (g.pend = g.pend || []).push({ addr: src, nick, ts: now() }); save(); }
      send(src, { t: 'gjn', g: w.g, k: 'wait', text: 'ホストの承認待ちです。承認されると自動で参加します' });
      if (fresh) { emit('contact', gView(g)); gSys(g, (nick || nameOf(src)) + ' が参加を申請しています'); emit('notice', { text: '「' + g.name + '」に参加申請があります' }); }
      return;
    }
    admit(g, src, nick, '招待リンクから参加しました');
  }
  function onGroupCtl(src, w) {
    if (!okGID(w.g) || locked) return;
    let g = st.groups[w.g];
    if (w.t === 'gj') { onJoin(src, w); return; }
    if (w.t === 'gjn') { if (w.k === 'pw' && w.text === JOINPWNEEDED && joinTries[w.g] && joinTries[w.g].pw) return; if (joinHost(w.g) === src) { joinTries[w.g] = { n: ((joinTries[w.g] || {}).n || 0) + 1, pw: (joinTries[w.g] || {}).pw }; if (w.k === 'wait') setJoin(w.g, src, JOINAPPROVE); else delJoin(w.g); emit('notice', { text: cleanText(w.text, 200), k: w.k || '', g: w.g }); } return; }
    if (w.t === 'ghs') { onHistory(src, w); return; }
    if (w.t === 'ghq') { onHistReq(src, w); return; }
    if (w.t === 'gd') { onDelta(src, w); return; }
    if (w.t === 'gk') { onKey(src, w); return; }
    if (w.t === 'gkq') { if (g && !g.left && g.owner === addr && isAdm(g, src)) sendKey(g, [src]); return; }
    if (w.t === 'gp') { onInfoPart(src, w); return; }
    if (w.t === 'gq') { if (g && !g.left && hasM(g, src) && hasM(g, addr)) answerInfo(src, g); else if (g && !g.left && !hasM(g, src)) goneNotice(src, g); else if (g && g.left) leftNotice(src, g); return; }
    if (w.t === 'gl') {
      if (!g || g.left || !hasM(g, src)) return;
      const before = g.members, host = g.owner === addr;
      g.members = g.members.filter(a => a !== src); g.admins = cleanAdmins(g.admins, g);
      noteGone(g, before, +w.ts || 0); // gl carries when they left (v0.19.2): their posts from before still count
      if (host) { bumpV(g); trackGone(g, before); } // the host makes a leave a new version, so members who missed the gl follow (v0.19.2)
      save(); emit('contact', gView(g)); gSys(g, gNameOf(g, src) + ' が退出しました');
      if (host) gMembers(g, [], [src], false);
      return;
    }
    if (w.t === 'gi') {
      if ((!Array.isArray(w.gm) || !w.gm.length) && (w.gc | 0) > 0) { onInfoHeader(src, w); return; } // split list (v0.19)
      if (!Array.isArray(w.gm) || !w.gm.length) return;
      const stubReq = g ? !!g.req : false;
      if (g && g.stub && !g.left) g = null; // a placeholder made from a message: take this as the first info
      // An unanswered invitation while we wait for the host whose link we opened: anyone who saw the group id may
      // have sent it first, maybe without the host in it; the host's own info starts the group afresh (v0.19.3).
      if (g && g.req && !g.left && joinHost(w.g) === src) g = null;
      const wasKnown = !!g;
      const gv = w.gv | 0;
      // The host's settings/name/admins carry their own version (gsv): a late repeat of older info must not undo a change.
      const staleSet = !!(g && g.owner && (w.gsv | 0) < (g.sv | 0));
      let ms = cleanMembers(w.gm, src), inG = ms.includes(addr), rejoined = false;
      if (g && !g.left && !hasM(g, src)) return;
      if (g && g.left) {
        // We left ourselves: a list with us is from someone who missed our gl, not an invitation (v0.19.2).
        if (g.quit) { if (inG) leftNotice(src, g); return; }
        // Only the host (or an admin) adding us back with a newer list brings us back.
        if (!inG || !canMng(g, src) || (gv && gv <= (g.v | 0))) return;
        g.left = false; g.req = true; delete g.tomb; rejoined = true;
      }
      const byHost = !!(g && g.owner && src === g.owner);
      let collide = false, hint = false, stale = false, resync = false, askHost = false, merged = [], unmerged = [], adopted = !g; // adopted: the list came from the host/an admin and replaces ours
      const gr = unpackAddrs(w.gr) || [];
      if (g && g.owner) {
        // Same version, different list: two changes from the same base. Keep ours; the host merges (v0.19.2).
        // (Unless the host's list says it removed us ("gr"): its merge keeps that, v0.19.3.)
        if (gv && gv === (g.v | 0) && g.owner !== addr && !sameM(ms, g.members) && !(byHost && !inG && gr.includes(addr))) { ms = g.members.slice(); inG = true; collide = true; }
        else if (byHost && (!gv || gv >= (g.v | 0))) { adopted = !sameM(ms, g.members); /* the host's list */ }
        // An admin's list far ahead of ours or over the limit: an admin can't brick the versions or overfill the group (v0.19.3).
        else if (!byHost && isAdm(g, src) && gv > (g.v | 0) && (gv > (g.v | 0) + MAXVJUMP || ms.length > gMax(g)) && g.owner !== addr) { ms = g.members.slice(); inG = true; askHost = true; }
        else if (!byHost && isAdm(g, src) && gv > (g.v | 0) && gv <= (g.v | 0) + MAXVJUMP && ms.length <= gMax(g)) { for (const x of [g.owner].concat(g.admins || [])) if (g.members.includes(x) && !ms.includes(x)) ms.push(x); ms.sort(); inG = ms.includes(addr); adopted = true; }
        else { // only the host (or an admin) changes members
          if (g.owner === addr && gv && !sameM(ms, g.members)) {
            if (isAdm(g, src)) {
              merged = mergeAdds(g, ms);
              // its removals too, when it worked from the same base (two admins removing at once, v0.19.3)
              if (gv === (g.v | 0)) for (const x of gr) if (hasM(g, x) && x !== g.owner && !isAdm(g, x) && x !== addr && !ms.includes(x)) unmerged.push(x);
              if (!merged.length && !unmerged.length) { resync = gv === (g.v | 0); stale = gv < (g.v | 0); }
            } else if (gv >= (g.v | 0)) hint = true; else stale = true;
          }
          ms = g.members.slice(); inG = true;
          if (merged.length || unmerged.length) ms = cleanMembers(g.members.filter(m => !unmerged.includes(m)).concat(merged), addr);
        }
      }
      let known = !!g, viaLink = false;
      const fresh = !g;
      if (!g) {
        if (!inG) return; const c = st.contacts[src];
        const prev = st.groups[w.g];
        // a stranger flooding us with invitations (v0.19.3)
        if (!(c && c.shared && !c.req) && joinHost(w.g) !== src && !(prev && prev.stub) && !inviteRoom(src)) return;
        g = st.groups[w.g] = { id: w.g, name: '', members: [], req: prev && prev.stub ? stubReq : !(c && c.shared && !c.req), by: src };
        const o = norm(String(w.go || '')); if (o && isYgg(o) && ms.includes(o)) { g.owner = o; g.admins = cleanAdmins(w.gad, { owner: o, members: ms }); }
        if (joinHost(w.g) === src && canMng(g, src)) { g.req = false; viaLink = true; } // we asked through this host's/admin's link
      }
      if (joinHost(w.g) === src && g.owner && canMng(g, src) && inG) delJoin(w.g);
      const old = { name: g.name, members: g.members.slice(), admins: (g.admins || []).slice(), set: g.set, v: g.v | 0 }, wasAdm = isAdm(g, addr);
      known = known && !!old.name;
      if (!staleSet) g.name = cleanNick(w.gn) || 'グループ';
      // The manager who made this list says when; a member removed then can't have posted after it (v0.19.3).
      const at = adopted && wasKnown ? changeTime(w.ts) : 0;
      noteGone(Object.assign(g, { members: ms }), old.members, at); if (!inG) g.left = true;
      if (adopted && wasKnown) noteGoneEach(g, old.members, w.gt);
      const dels = purgeGone(g, old.members);
      if (gv > (g.v | 0) && (byHost || old.admins.includes(src) || !known) && !askHost) { g.v = gv; const t = changeTime(w.ts); if (t) g.vat = t; else delete g.vat; }
      if (merged.length || unmerged.length || resync) { g.v = old.v; bumpV(g); } // the host re-issues the merged list as a new version
      if (w.k === 'kick' && canMng({ owner: g.owner, admins: old.admins }, src) && canMng(g, addr)) for (const x of gr) if (!hasM(g, x)) addBan(g, [x]); // removed by a manager: no coming back by link
      if (unmerged.length) addBan(g, unmerged);
      if (g.owner === addr) trackGone(g, old.members);
      let diffs = [], handed = '', claimed = false;
      // The host left (it is no longer a member) and we missed its hand-over: the admin it named says so itself (v0.19.3).
      if (known && g.owner && src !== g.owner && !hasM(g, g.owner) && old.admins.includes(src) && hasM(g, src) && (w.gsv | 0) > (g.sv | 0) && norm(String(w.go || '')) === src) { g.owner = src; claimed = true; }
      if (g.owner && src === g.owner && !staleSet) {
        if ((w.gsv | 0) > (g.sv | 0)) g.sv = w.gsv | 0;
        const o = norm(String(w.go || '')); if (o && isYgg(o) && o !== g.owner && g.members.includes(o)) { g.owner = o; handed = o; } // the host handed the group over
        if (w.gs && typeof w.gs === 'object') { const ns = cleanSet(w.gs); if (known) diffs = setDiff(g.set, ns); g.set = ns; }
        g.admins = cleanAdmins(w.gad, g);
      } else g.admins = cleanAdmins(g.admins, g);
      if ((wasAdm || !isAdm(g, addr)) && !isAdm(g, addr) && g.owner !== addr) { delete g.tok; delete g.pend; delete g.pwh; }
      if (g.pend) { g.pend = g.pend.filter(p => !g.members.includes(p.addr)); if (!g.pend.length) delete g.pend; }
      if (!g.admins.length) delete g.admins;
      save(); emit('contact', gView(g)); emitDels(dels);
      if (askHost) askInfo(g.owner, g.id);
      if (isAdm(g, addr) && g.set && g.set.pw && !g.pwh) send(g.owner, { t: 'gkq', g: g.id });
      if (collide) { if (src === g.owner) answerInfo(g.owner, g); else askInfo(g.owner, g.id); }
      else if (hint) for (const x of g.admins || []) askInfo(x, g.id);
      else if (stale) answerInfo(src, g);
      else if (resync) gInfo(g, [src]);
      else if (merged.length || unmerged.length) { gMembers(g, merged, unmerged, unmerged.length > 0); if (g.set && g.set.hi) { const h = historyOf(g.id); if (h.length) for (const x of merged) sendHistory(x, g.id, h); } }
      if (fresh && inG && viaLink) gSys(g, 'あなたが参加しました'); // the UI puts history before this line
      else if (fresh && inG) gSys(g, gNameOf(g, src) + ' があなたを追加しました');
      else if (rejoined) gSys(g, gNameOf(g, src) + ' があなたをグループに戻しました');
      else if (!inG) { gSys(g, 'グループから外されました'); failLeftCopies(g.id); }
      else if (known && old.name !== g.name && old.name !== 'グループ') gSys(g, gNameOf(g, src) + ' がグループ名を「' + g.name + '」にしました');
      if (inG) for (const t of diffs) gSys(g, gNameOf(g, src) + ' が' + t);
      if (handed) gSys(g, gNameOf(g, src) + ' が ' + gNameOf(g, handed) + ' をホストにしました');
      if (claimed) gSys(g, gNameOf(g, src) + ' がホストになりました');
      if (known && inG) { memberSys(g, src, ms.filter(m => !old.members.includes(m) && m !== addr), old.members.filter(m => !ms.includes(m) && m !== src)); adminSys(g, src, old.admins, g.admins || []); }
    }
  }

  // finger (opt-in): answer {t:"fq"} with nick + .plan; collect {t:"fr"} replies.
  let finger = { on: false, plan: '' };
  idbGet('finger').then(v => { if (v) finger = { on: !!v.on, plan: String(v.plan || '') }; }).catch(() => {});
  const fingerWait = new Map();
  const cleanPlan = (t, max) => { let x = String(t || '').replace(/\r\n/g, '\n').replace(/[\u0000-\u0008\u000b-\u001f\u007f-\u009f]/g, ''); while (new TextEncoder().encode(x).length > max) x = x.slice(0, -1); return x; };
  function fingerQuery(peer) {
    return new Promise((res, rej) => {
      let n = 0;
      const iv = setInterval(() => { if (++n > 6) { clearInterval(iv); fingerWait.delete(peer); rej(new Error('応答がなかったよ（相手のfingerがオフか、いまつながっていない）')); return; } send(peer, { t: 'fq' }); }, 2000);
      fingerWait.set(peer, w => { clearInterval(iv); fingerWait.delete(peer); res(w); });
      send(peer, { t: 'fq' });
    });
  }

  // A datagram as the native node decodes it (main.go type wire): only the known fields, each of its
  // Go type, or the whole packet is dropped like a failed json.Unmarshal. Before this, a wrong type
  // ("ts":"x", "gm":{...}) or a key like "__proto__" / "_checked" reached the handlers as is (r2).
  const WSTR = ['t', 'id', 'nick', 'text', 'bio', 'av', 'dom', 'pk', 'mime', 'sha', 'd', 'ref', 'g', 'gn', 'go', 'gh', 'tok', 'pw', 'k', 'by', 'gmb', 'ga', 'gr', 'pn', 'sg', 'dby', 'ma'];
  const WINT = ['ts', 'ttl', 'size', 'n', 'i', 'gv', 'gsv', 'gc'];
  const wellFormed = s => typeof s.toWellFormed === 'function' ? s.toWellFormed() : s; // lone surrogates as Go reads them (U+FFFD)
  const isInt = v => typeof v === 'number' && Number.isInteger(v) && Math.abs(v) < 2 ** 63; // Go int64 (big ones lose precision here, but aren't dropped)
  function wireOf(json) {
    let o; try { o = JSON.parse(json); } catch { return null; }
    if (o === null || typeof o !== 'object' || Array.isArray(o)) return null;
    const w = {}; // absent fields are Go's zero values: a missing id is "" (String(undefined) passed the id checks as "undefined")
    for (const k of WSTR) { const v = o[k]; if (v == null) { w[k] = ''; continue; } if (typeof v !== 'string') return null; w[k] = wellFormed(v); }
    // Go int fields as on a 32-bit build (ts is int64): the handlers use |0, which wrapped bigger ones (2^32+5 -> 5).
    for (const k of WINT) { const v = o[k]; if (v == null) { w[k] = 0; continue; } if (!isInt(v) || (k !== 'ts' && (v | 0) !== v)) return null; w[k] = v; }
    for (const k of ['gm', 'gad']) {
      const v = o[k]; if (v == null) continue;
      if (!Array.isArray(v) || v.some(x => x != null && typeof x !== 'string')) return null;
      w[k] = v.map(x => x == null ? '' : wellFormed(x));
    }
    if (o.gs != null) {
      const v = o.gs; if (typeof v !== 'object' || Array.isArray(v)) return null;
      const s = {};
      for (const k of ['nf', 'ap', 'hi', 'pw']) { if (v[k] == null) continue; if (typeof v[k] !== 'boolean') return null; if (v[k]) s[k] = true; }
      for (const k of ['ttl', 'mx', 'pb']) { if (v[k] == null) continue; if (!isInt(v[k]) || (v[k] | 0) !== v[k]) return null; if (v[k]) s[k] = v[k]; }
      w.gs = s;
    }
    if (o.gt != null) {
      const v = o.gt; if (typeof v !== 'object' || Array.isArray(v)) return null;
      const m = {};
      for (const k of Object.keys(v)) { if (v[k] == null) continue; if (!isInt(v[k])) return null; if (k !== '__proto__') m[wellFormed(k)] = v[k]; }
      w.gt = m;
    }
    return w;
  }
  // Called synchronously by the wasm read loop: an exception here used to panic the Go side and stop
  // the node until a reload (one {"t":...} packet or the text "null" was enough), r2.
  function onPacket(src, json) {
    try { onPacket_(src, json); } catch (e) { console.error('packet', e); }
  }
  function onPacket_(src, json) {
    const w = wireOf(json); if (!w) return;
    if (w.t === 'fq' || w.t === 'fr') {
      if (st.blocked && st.blocked[src]) return;
      if (w.t === 'fr') { const f = fingerWait.get(src); if (f) f(w); return; }
      if (finger.on) send(src, { t: 'fr', nick: locked ? 'anon' : (st.nick || 'anon'), text: cleanPlan(finger.plan, 700) });
      return;
    }
    if (w.t === 'mh' || w.t === 'mc') { if (!(st.blocked && st.blocked[src])) onMoveIn(src, w); return; }
    if (w.t === 'mha' || w.t === 'mca' || w.t === 'mok' || w.t === 'mng') { onMoveAck(src, w); return; }
    if (w.t === 'fca') { const g = chunkAcks.get(w.id); if (g && ackFrom.get(w.id) === src) g.add(w.i | 0); return; }
    if (w.t === 'ack') {
      const f = acks.get(w.id);
      if (f && ackFrom.get(w.id) === src) { acks.delete(w.id); ackFrom.delete(w.id); f(); }
      else if (!f && !locked && /^[0-9A-Za-z_-]{1,40}$/.test(String(w.id))) lateAck(src, String(w.id));
      askPK(src); return;
    }
    if (w.t === 'pow') { onPowNeed(src, w); return; }
    if (locked || st.blocked[src]) return; // stay silent: the sender keeps it queued
    if (w.pk) learnPK(src, String(w.pk));
    if (w.g) invHeard(String(w.g), src); // it has the group
    if (w.t === 'hi' || w.t === 'hiack') { if (w.t === 'hi') send(src, { t: 'hiack', pk: myPK() }); maybeProf(src); flush(src); return; }
    if (w.t === 'prof') {
      const c = st.contacts[src]; if (!c) return;
      const n = cleanNick(w.nick); if (n && !c.renamed) c.nick = n;
      c.bio = cleanText(w.bio, 140); c.avatar = cleanText(w.av, 8); c.hasprof = true;
      const dom = cleanDomain(w.dom);
      const recheck = dom && (dom !== c.dom || Date.now() / 1000 - (c.domat || 0) > 12 * 3600);
      if (dom !== (c.dom || '')) { c.dom = dom || undefined; c.domok = false; c.domat = 0; }
      save(); emit('contact', c);
      if (recheck) resolveYgg(dom).then(r => { const x = st.contacts[src]; if (!x || x.dom !== dom || r.err === 'net') return; x.domok = r.addr === src; x.domat = Math.floor(Date.now() / 1000); save(); emit('contact', x); });
      return;
    }
    if (w.t === 'ttl') { setTTL(src, w.ttl || 0, false); return; }
    if (w.t === 'fh') { onFileHeader(src, w); return; }
    if (w.t === 'fc') { onFileChunk(src, w); return; }
    if ((w.t === 'edit' || w.t === 'del') && w.g) { // group unsend (v0.19.4); group posts aren't edited remotely
      if (w.t === 'del') onGroupDel(src, w).then(ok => { if (ok) send(src, { t: 'ack', id: w.id }); }); else if (/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id || ''))) send(src, { t: 'ack', id: w.id });
      return;
    }
    if ((w.t === 'edit' || w.t === 'del') && onCtl(src, w)) { send(src, { t: 'ack', id: w.id }); return; }
    if (['gq', 'gi', 'gl', 'gj', 'gjn', 'ghs', 'ghq', 'gd', 'gp', 'gk', 'gkq'].includes(w.t)) { onGroupCtl(src, w); return; }
    if (w.t === 'msg') handleMsg(src, w).then(async okm => { if (okm) { await saveNow(); send(src, { t: 'ack', id: w.id }); askPK(src); if (queuedFor(src).length) flush(src); } }).catch(e => console.error('msg', e)); // ack once stored (v0.19.2)
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
  // Decompression bombs (v0.19.3, as imgdims.go): a 600 KB PNG/GIF/WebP can claim
  // 50000x50000 px. Only the header is read; at most 8192 px a side and ~25 MP.
  function imgDims(mime, b) {
    const be16 = i => (b[i] << 8) | b[i + 1], le16 = i => b[i] | (b[i + 1] << 8), be32 = i => ((b[i] << 24) >>> 0) + (b[i + 1] << 16) + (b[i + 2] << 8) + b[i + 3];
    if (mime === 'image/png') { if (b.length < 24 || b[0] !== 0x89 || b[1] !== 0x50 || String.fromCharCode(...b.subarray(12, 16)) !== 'IHDR') return null; return [be32(16), be32(20)]; }
    if (mime === 'image/gif') { if (b.length < 10 || !/^GIF8[79]a$/.test(String.fromCharCode(...b.subarray(0, 6)))) return null; return [le16(6), le16(8)]; }
    if (mime === 'image/webp') {
      if (b.length < 30 || String.fromCharCode(...b.subarray(0, 4)) !== 'RIFF' || String.fromCharCode(...b.subarray(8, 12)) !== 'WEBP') return null;
      const k = String.fromCharCode(...b.subarray(12, 16));
      if (k === 'VP8X') return [(b[24] | (b[25] << 8) | (b[26] << 16)) + 1, (b[27] | (b[28] << 8) | (b[29] << 16)) + 1];
      if (k === 'VP8 ') { if (b[23] !== 0x9d || b[24] !== 0x01 || b[25] !== 0x2a) return null; return [le16(26) & 0x3fff, le16(28) & 0x3fff]; }
      if (k === 'VP8L') { if (b[20] !== 0x2f) return null; const v = (b[21] | (b[22] << 8) | (b[23] << 16) | (b[24] << 24)) >>> 0; return [(v & 0x3fff) + 1, ((v >>> 14) & 0x3fff) + 1]; }
      return null;
    }
    if (mime === 'image/jpeg') {
      if (b.length < 4 || b[0] !== 0xff || b[1] !== 0xd8) return null;
      let i = 2;
      while (i + 3 < b.length) {
        if (b[i] !== 0xff) return null;
        const mk = b[i + 1];
        if (mk === 0xff) { i++; continue; } // fill byte
        if (mk === 0xd8 || mk === 0x01 || (mk >= 0xd0 && mk <= 0xd7)) { i += 2; continue; } // no length
        if (mk === 0xd9 || mk === 0xda) return null; // image data before any frame header
        const len = be16(i + 2); if (len < 2) return null;
        if (mk >= 0xc0 && mk <= 0xcf && mk !== 0xc4 && mk !== 0xc8 && mk !== 0xcc) { if (i + 9 > b.length) return null; return [be16(i + 7), be16(i + 5)]; }
        i += 2 + len;
      }
      return null;
    }
    return null;
  }
  const imgDimsOK = (mime, b) => { const d = imgDims(mime, b); return !!d && d[0] > 0 && d[1] > 0 && d[0] <= 8192 && d[1] <= 8192 && d[0] * d[1] <= 24 * 1048576; };
  const IMGDIMS = '画像の縦横が大きすぎるか、読み取れません（1辺8192px・約2500万画素まで）';
  const chunkAcks = new Map();
  const inFiles = new Map();

  // ---------- account move (v0.15; same wire + bundle as moveacct.go) ----------
  const MV_MAX = 64 * 1024 * 1024;
  const sleep = ms => new Promise(r => setTimeout(r, ms));
  const b64big = u8 => { let s = ''; for (let i = 0; i < u8.length; i += 0x8000) s += String.fromCharCode.apply(null, u8.subarray(i, i + 0x8000)); return btoa(s); };
  const mvIn = { on: false, id: '', from: '', hdr: null, chunks: null, got: 0, at: 0, done: false, busy: false, key: null, auth: false };
  const mvOut = { id: '', acks: null, res: null, peer: '', key: null };
  // Transfer proof (v0.19.4, same as move.AuthKey/HeaderMAC/ReplyMAC): the code's X25519 public key never
  // travels over the mesh, so HMAC keyed by SHA-256("yggchat-move-auth1\n"+rpk) proves a header comes from
  // whoever holds the code. A proven header takes the receive slot from an unproven one (a squatter).
  function mvCodeKey(code) {
    let c = String(code || '').trim(); const i = c.indexOf('ygm1:'); if (i >= 0) c = c.slice(i);
    const j = c.lastIndexOf(':'); if (!c.startsWith('ygm1:') || j < 5) return null;
    try { const b = atob(c.slice(j + 1).trim().replace(/-/g, '+').replace(/_/g, '/')); if (b.length !== 32) return null; return Uint8Array.from(b, x => x.charCodeAt(0)); } catch { return null; }
  }
  async function mvAuthKey(rpk) { const pre = new TextEncoder().encode('yggchat-move-auth1\n'), u = new Uint8Array(pre.length + rpk.length); u.set(pre); u.set(rpk, pre.length); return crypto.subtle.importKey('raw', await crypto.subtle.digest('SHA-256', u), { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']); }
  async function mvMAC(key, ...parts) { return hexOf(await crypto.subtle.sign('HMAC', key, new TextEncoder().encode(parts.join('\n')))).slice(0, 32); }
  const mvHdrMAC = (key, w) => mvMAC(key, 'mh', String(w.id), String(w.size), String(w.n), String(w.sha));
  const mvMACOK = (want, got) => { got = String(got || ''); if (got.length !== 32 || want.length !== 32) return false; let d = 0; for (let i = 0; i < 32; i++) d |= want.charCodeAt(i) ^ got.charCodeAt(i); return d === 0; };
  function moveBundle(history) {
    const s = { nick: st.nick, bio: st.bio || undefined, avatar: st.avatar || undefined, domain: st.domain || undefined, contacts: st.contacts, blocked: st.blocked, groups: st.groups, nostr_on: !!st.nostrOn, nostr_relays: st.nostrRelays && st.nostrRelays.length ? st.nostrRelays : undefined, nostr_legacy: !!st.nostrLegacy, pow_strict: !!st.powStrict || undefined };
    if (history) { s.messages = st.messages; s.ctl = st.ctl; }
    return JSON.stringify({ v: 1, key: outer.key, store: s });
  }
  async function importBundle(txt) {
    let b; try { b = JSON.parse(txt) } catch { throw new Error('データが壊れています') }
    if (!b || b.v !== 1) throw new Error('対応していない移行データです（アプリを更新してください）');
    if (!/^[0-9a-f]{128}$/.test(String(b.key))) throw new Error('鍵が壊れています');
    const s = b.store || {};
    st = Object.assign(blank(), { nick: s.nick || 'anon', bio: s.bio || '', avatar: s.avatar || '', domain: s.domain || undefined, contacts: s.contacts || {}, messages: s.messages || [], blocked: s.blocked || {}, groups: s.groups || {}, ctl: s.ctl || [], nostrOn: !!s.nostr_on, nostrLegacy: !!s.nostr_legacy, nostrRelays: s.nostr_relays || [], powStrict: !!s.pow_strict });
    migrate(); outer.key = b.key; save(); saveOuter(); await saving;
    setTimeout(() => location.reload(), 700);
  }
  async function onMoveIn(src, w) {
    if (!mvIn.on) return;
    if (mvIn.done) { if (w.id === mvIn.id) send(src, { t: 'mok', id: w.id }); return; }
    if (w.t === 'mh') {
      const size = w.size, n = w.n;
      if (!/^[0-9a-f]{8,40}$/.test(String(w.id)) || !/^[0-9a-f]{64}$/.test(String(w.sha)) || !Number.isInteger(size) || !Number.isInteger(n) || size < 33 || size > MV_MAX || n !== Math.ceil(size / CHUNK)) return;
      // A proven header (v0.19.4 sender holding the code); a wrong proof is dropped, none = older sender.
      const key = mvIn.key, auth = w.ma !== undefined && w.ma !== '';
      if (auth && (!key || !mvMACOK(await mvHdrMAC(key, w), w.ma))) return;
      if (!mvIn.on || mvIn.done || key !== mvIn.key) return; // the screen changed while hashing
      const reply = { t: 'mha', id: w.id }; if (auth) reply.ma = await mvMAC(key, 'mha', w.id);
      if (w.id === mvIn.id && src === mvIn.from) { if (auth === mvIn.auth) send(src, reply); return; }
      if (w.id === mvIn.id) return;
      if (mvIn.id && Date.now() - mvIn.at < 60000) {
        // Taken: only a proven sender displaces an unproven one; the same sender may restart (not downgrade).
        const takeover = auth && !mvIn.auth, restart = src === mvIn.from && (auth || !mvIn.auth);
        if (!takeover && !restart) return; // another transfer is running
        if (takeover && src !== mvIn.from) send(mvIn.from, { t: 'mng', id: mvIn.id, text: '別の送信元が受け取り側を使っています' });
      }
      Object.assign(mvIn, { id: w.id, from: src, hdr: w, chunks: new Array(n), got: 0, at: Date.now(), busy: false, auth });
      send(src, reply);
      emit('move', { role: 'recv', phase: 'recv', got: 0, n, from: src });
      return;
    }
    if (w.id !== mvIn.id || src !== mvIn.from || !mvIn.chunks) return;
    const i = w.i | 0; if (i < 0 || i >= mvIn.chunks.length) return;
    if (!mvIn.chunks[i]) {
      let b; try { b = unb64(String(w.d || '')) } catch { return }
      if (!b.length || b.length > CHUNK) return;
      mvIn.chunks[i] = b; mvIn.got++; mvIn.at = Date.now();
      if (mvIn.got % 64 === 0) emit('move', { role: 'recv', phase: 'recv', got: mvIn.got, n: mvIn.chunks.length, from: src });
    }
    send(src, { t: 'mca', id: w.id, i });
    if (mvIn.got < mvIn.chunks.length || mvIn.busy) return;
    mvIn.busy = true;
    const id = w.id, n = mvIn.chunks.length;
    const fail = msg => { send(src, { t: 'mng', id, text: msg }); emit('move', { role: 'recv', phase: 'error', err: msg }); if (mvIn.id === id) Object.assign(mvIn, { id: '', chunks: null, busy: false, auth: false }); };
    const all = new Uint8Array(mvIn.hdr.size); let off = 0;
    for (const c of mvIn.chunks) { if (off + c.length > all.length) return fail('データが壊れていました'); all.set(c, off); off += c.length; }
    if (off !== all.length || await sha(all) !== mvIn.hdr.sha) return fail('データが壊れていました');
    const r = yggMoveOpen(b64big(all)); if (r.error) return fail(r.error);
    emit('move', { role: 'recv', phase: 'import', got: n, n });
    try { await importBundle(r.plain); } catch (e) { return fail(e.message); }
    mvIn.done = true; mvIn.chunks = null;
    for (let k = 0; k < 3; k++) send(src, { t: 'mok', id });
  }
  async function onMoveAck(src, w) {
    if (!w.id || w.id !== mvOut.id || src !== mvOut.peer) return; // only the receiver in the code
    if (w.t === 'mha') { if (w.ma !== undefined && w.ma !== '' && !mvMACOK(await mvMAC(mvOut.key, 'mha', String(w.id)), w.ma)) return; if (w.id === mvOut.id) mvOut.hello = true; }
    else if (w.t === 'mca') { mvOut.hello = true; mvOut.acks.add(w.i | 0); }
    else if (mvOut.res) mvOut.res(w.t === 'mok' ? '' : '受け取り側: ' + (cleanText(w.text, 200) || '失敗しました'));
  }
  async function moveSend(peer, rpk, blob) {
    const id = hexOf(crypto.getRandomValues(new Uint8Array(8))), n = Math.ceil(blob.length / CHUNK);
    const key = await mvAuthKey(rpk);
    const hdr = { t: 'mh', id, size: blob.length, n, sha: await sha(blob) };
    hdr.ma = await mvHdrMAC(key, hdr); // older receivers ignore it
    let resolve; const res = new Promise(r => resolve = r);
    Object.assign(mvOut, { id, acks: new Set(), res: resolve, hello: false, peer, key });
    const finish = err => { if (mvOut.id === id) Object.assign(mvOut, { id: '', acks: null, res: null }); emit('move', err ? { role: 'send', phase: 'error', err } : { role: 'send', phase: 'done', got: n, n }); };
    // Header only until the receiver answers it (mha; v0.15.0 never does, so
    // chunks start after 4 s anyway), then a window that grows from 32.
    let last = -1, lastAt = Date.now(), win = 32;
    const start = Date.now();
    for (;;) {
      if (mvOut.id !== id) return finish('キャンセルしました');
      const got = mvOut.acks.size;
      if (got !== last) { if (last >= 0 && win < 256) win *= 2; last = got; lastAt = Date.now(); emit('move', { role: 'send', phase: 'send', got, n }); }
      else if (Date.now() - lastAt > 90000) return finish('相手に届きません。新しい端末で受け取り画面が開いているか確認してください');
      send(peer, hdr); await sleep(30);
      let k = 0;
      if (mvOut.hello || Date.now() - start >= 4000) for (let i = 0; i < n && k < win; i++) if (!mvOut.acks.has(i)) { send(peer, { t: 'mc', id, i, d: b64(blob.subarray(i * CHUNK, (i + 1) * CHUNK)) }); if (++k % 8 === 0) await sleep(4); }
      const r = await Promise.race([res, sleep(700).then(() => null)]);
      if (r !== null) return finish(r);
    }
  }
  async function deliverImg(m, tries) {
    let p;
    if (m.img) { p = parseImg(m.img); if (!p) { setStatus(m.peer, m.id, 'failed'); return false; } }
    else p = { mime: TEXTMIME, bytes: new TextEncoder().encode(m.text || '') }; // a long text goes chunked (v0.19)
    const isText = p.mime === TEXTMIME, mm = isText ? Object.assign({}, m, { textFile: true }) : m;
    const n = Math.ceil(p.bytes.length / CHUNK);
    const hdr = { t: 'fh', id: m.id, nick: m.nick, text: isText ? undefined : (m.text || undefined), ts: m.ts, ttl: m.ttl || undefined, mime: p.mime, size: p.bytes.length, n, sha: await sha(p.bytes) };
    if (m.grp) { const gw = msgPktBare(m); hdr.g = gw.g; hdr.gh = gw.gh; if (gw.sg) { hdr.sg = gw.sg; hdr.pk = gw.pk; } }
    withPow(mm, hdr, hdr.sha); let asked = powNeed[m.peer + '/' + m.id] | 0;
    const got = new Set(); chunkAcks.set(m.id, got);
    const acked = waitAck(m.peer, m.id);
    let done = false; acked.then(() => done = true);
    try {
      let idle = 0, last = -1;
      while (idle < tries) {
        if (last >= 0 && !m.hid && !stillOut(m.peer, m.id)) { acks.delete(m.id); ackFrom.delete(m.id); return true; } // deleted meanwhile
        if ((powNeed[m.peer + '/' + m.id] | 0) > asked) { asked = powNeed[m.peer + '/' + m.id] | 0; mm.pn = ''; withPow(mm, hdr, hdr.sha); }
        send(m.peer, hdr);
        if (got.size === last) idle++; else { idle = 0; last = got.size; }
        await new Promise(r => setTimeout(r, 30));
        for (let i = 0; i < n && !done; i++) if (!got.has(i)) { send(m.peer, { t: 'fc', id: m.id, i, d: b64(p.bytes.subarray(i * CHUNK, (i + 1) * CHUNK)) }); if (i % 8 === 7) await new Promise(r => setTimeout(r, 4)); }
        const ok = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (ok) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id); ackFrom.delete(m.id);
      setStatus(m.peer, m.id, 'queued'); // images don't go through the mailbox
      if (isText) nostrPark(m); // a long text can (one Nostr event holds it)
      probeSoon(m.peer);
      return false;
    } finally { chunkAcks.delete(m.id); }
  }
  // hasGroupFile mirrors the native node: we already hold src's post (member
  // copies carry "<post>g<i>" ids, the store the bare post id), so the photo or
  // long text isn't downloaded again; a relay without the photo / full text
  // still takes the sender's own copy.
  function hasGroupFile(gid, src, id, mime) {
    const key = 'g:' + gid;
    for (let i = st.messages.length - 1; i >= 0; i--) {
      const m = st.messages[i];
      if (m.peer !== key || m.from !== src || !samePost(m.id, id)) continue;
      return !(!m.out && (m.hist || m.via) && (mime === TEXTMIME || !m.img));
    }
    return false;
  }
  function onFileHeader(src, w) {
    const n = w.n | 0, size = w.size | 0;
    const isText = w.mime === TEXTMIME;
    if (!/^[0-9A-Za-z_-]{1,40}$/.test(String(w.id)) || !(MIMES.has(w.mime) || isText) || n < 1 || n > MAXN || size < 1 || size > (isText ? MAXRECV : IMGMAX) || Math.ceil(size / CHUNK) !== n || !/^[0-9a-f]{64}$/.test(String(w.sha)) || u8len(w.text) > CAPMAX || (isText && w.text) || u8len(w.nick) > 120) return;
    if ((!w.g && isGoneMsg(src, String(w.id))) || st.messages.some(m => m.peer === src && m.id === w.id) || (w.g && okGID(w.g) && hasGroupFile(w.g, src, w.id, w.mime)) || (w.g && okGID(w.g) && isGDel(w.g, src, w.id)) || (w.g && !w.text && !isText && st.groups[w.g] && st.groups[w.g].set && st.groups[w.g].set.nf)) { send(src, { t: 'ack', id: w.id }); return; }
    if (w.g && st.groups[w.g] && st.groups[w.g].left && !locked && !st.blocked[src]) { send(src, { t: 'ack', id: w.id }); leftNotice(src, st.groups[w.g]); return; } // we're out (v0.19.2)
    if (locked || st.blocked[src] || !powCheck(src, w)) return;
    const t = Date.now();
    for (const [k, f] of inFiles) if (t - f.at > 180000) inFiles.delete(k);
    const key = src + '/' + w.id;
    if (inFiles.has(key) || inFiles.size >= 32) return;
    // One peer holds at most 4 slots and all strangers together 12, so nobody can keep
    // every slot busy and block photos and long texts from everyone else (v0.19.3).
    const c = st.contacts[src], stranger = !c || !!c.req;
    let np = 0, ns = 0; for (const [k, f] of inFiles) { if (k.startsWith(src + '/')) np++; if (f.stranger) ns++; }
    if (np >= 4 || (stranger && ns >= 12)) return;
    inFiles.set(key, { hdr: w, chunks: new Array(n), got: 0, at: t, stranger });
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
    const h = Object.assign({}, f.hdr, { _checked: true }); // the header's proof of work was checked
    let img = '';
    if (h.mime === TEXTMIME) { try { h.text = new TextDecoder('utf-8', { fatal: true }).decode(all); } catch { return; } }
    else if (!imgDimsOK(h.mime, all)) { send(src, { t: 'ack', id: w.id }); return; } // a decompression bomb would crash the UI: dropped, acked so it stops (v0.19.3)
    else { let s = ''; for (let j = 0; j < all.length; j += 0x8000) s += String.fromCharCode(...all.subarray(j, j + 0x8000)); img = 'data:' + h.mime + ';base64,' + btoa(s); }
    if (h.g) { if (await onGroupMsg(src, h, img)) { await saveNow(); send(src, { t: 'ack', id: w.id }); } return; }
    if (acceptMsg(src, h, img)) { await saveNow(); send(src, { t: 'ack', id: w.id }); }
  }

  // After a miss, greet the peer at 3, 5 and 10 s so the message goes out as
  // soon as a route exists instead of waiting for the 20 s queue loop.
  const probing = new Set();
  async function probeSoon(peer) {
    if (probing.has(peer)) return;
    probing.add(peer);
    try {
      for (const d of [3000, 5000, 10000]) {
        await new Promise(r => setTimeout(r, d));
        if (!queuedFor(peer).length) return;
        hello(peer);
      }
    } finally { probing.delete(peer); }
  }
  async function deliver(m, tries = 8) {
    if (m.hid && gOut(m.grp) && !leavingG.has(m.grp)) { if (!m.nostr) setStatus(m.peer, m.id, 'failed'); return true; } // we left (or were removed): not retried
    if (inflight.has(m.id)) return false;
    inflight.add(m.id);
    if (m.status !== 'sending') setStatus(m.peer, m.id, 'sending');
    if (m.gimg && !m.img) {
      const v = st.messages.find(x => x.peer === 'g:' + m.grp && x.id === m.of);
      if (v && v.img) m = Object.assign({}, m, { img: v.img });
      else { inflight.delete(m.id); dropCopy(m.peer, m.id); return true; } // the photo was deleted here before this copy went out
    }
    if (m.img || longText(m)) { try { return await deliverImg(m, tries); } finally { inflight.delete(m.id); } }
    let pkt = JSON.stringify(await msgPkt(m)), asked = powNeed[m.peer + '/' + m.id] | 0;
    const acked = waitAck(m.peer, m.id);
    try {
      for (let i = 0; i < tries; i++) {
        if (i > 0 && !m.hid && !stillOut(m.peer, m.id)) { acks.delete(m.id); ackFrom.delete(m.id); return true; } // deleted meanwhile: don't keep sending it
        if ((powNeed[m.peer + '/' + m.id] | 0) > asked) { asked = powNeed[m.peer + '/' + m.id] | 0; pkt = JSON.stringify(await msgPkt(Object.assign({}, m, { pn: '' }))); } // the receiver wants more work
        yggSend(m.peer, pkt);
        const got = await Promise.race([acked.then(() => true), new Promise(r => setTimeout(() => r(false), 2500))]);
        if (got) { setStatus(m.peer, m.id, 'sent'); return true; }
      }
      acks.delete(m.id); ackFrom.delete(m.id);
      setStatus(m.peer, m.id, m.nostr ? 'stored' : 'queued');
      if (!m.nostr) nostrPark(m);
      probeSoon(m.peer);
      return false;
    } finally { inflight.delete(m.id); }
  }

  // ---- optional offline delivery over public Nostr relays (deprecated, opt-in) ----
  // Undeliverable text is sealed to the peer's key (only they can open it; the
  // sender is inside the box) and posted as a Nostr event signed by a
  // throw-away key, tagged with a pairwise tag that rotates daily (v0.13), a
  // back-dated created_at, a blurred 7-day NIP-40 expiry and padding. The
  // recipient pulls it, opens it, acks directly and publishes the NIP-09
  // deletion. Relays still see IP addresses and which tags one fetch asks for.
  const nDefaults = () => { try { return JSON.parse(yggNostrDefaults()); } catch { return []; } };
  const nRelays = () => (!locked && st.nostrOn) ? (st.nostrRelays && st.nostrRelays.length ? st.nostrRelays.slice() : nDefaults()) : null;
  const nostrState = () => ({ on: !!st.nostrOn, legacy: !!st.nostrLegacy, relays: st.nostrRelays && st.nostrRelays.length ? st.nostrRelays : nDefaults(), defaults: nDefaults() });
  function relayTalk(url, first, onMsg, ms) {
    return new Promise(res => {
      let ws, done = false;
      const fin = v => { if (done) return; done = true; clearTimeout(t); try { ws && ws.close(); } catch {} res(v); };
      const t = setTimeout(() => fin(undefined), ms);
      try { ws = new WebSocket(url); } catch { return fin(undefined); }
      ws.onopen = () => ws.send(first);
      ws.onmessage = e => { let m; try { m = JSON.parse(e.data); } catch { return; } if (!Array.isArray(m)) return; const v = onMsg(m); if (v !== undefined) fin(v); };
      ws.onerror = () => fin(undefined);
      ws.onclose = () => fin(undefined);
    });
  }
  const relayPublish = (url, ev) => { const id = JSON.parse(ev).id; return relayTalk(url, '["EVENT",' + ev + ']', m => (m[0] === 'OK' && m[1] === id) ? m[2] === true : undefined, 15000).then(v => !!v); };
  const nostrPublish = (rs, ev) => Promise.all(rs.map(r => relayPublish(r, ev))).then(a => a.some(Boolean));
  function relayFetch(url, tags, since) {
    const out = [], reqs = [];
    for (let i = 0; i < tags.length; i += 100) reqs.push(JSON.stringify(['REQ', 'yc' + reqs.length, { kinds: [1059], '#p': tags.slice(i, i + 100), since, limit: 500 }]));
    let left = reqs.length;
    return new Promise(res => {
      let ws, done = false;
      const fin = () => { if (done) return; done = true; clearTimeout(t); try { ws && ws.close(); } catch {} res(out); };
      const t = setTimeout(fin, 25000);
      try { ws = new WebSocket(url); } catch { return fin(); }
      ws.onopen = () => reqs.forEach(r => ws.send(r));
      ws.onmessage = e => { if (done) return; let m; try { m = JSON.parse(e.data); } catch { return; }
        if (!Array.isArray(m)) return;
        if (m[0] === 'EVENT' && m[2] && typeof m[2] === 'object') { out.push(m[2]); if (out.length >= 2000) fin(); } // a relay streaming junk can't fill our memory (as nostr_net.go maxFetchEvents, r2)
        else if (m[0] === 'EOSE' || m[0] === 'CLOSED') { try { ws.send(JSON.stringify(['CLOSE', m[1]])); } catch {} if (--left <= 0) fin(); } };
      ws.onerror = fin; ws.onclose = fin;
    });
  }
  const nSeen = new Set();
  let nBusy = false;
  async function nostrFetch() {
    const rs = nRelays(); if (!rs || nBusy) return;
    nBusy = true;
    try {
      let q; try { q = JSON.parse(yggNostrTags(JSON.stringify(Object.values(st.contacts).map(c => c.pk).filter(Boolean)), !!st.nostrLegacy)); } catch { return; }
      if (!q || !Array.isArray(q.tags) || !q.tags.length) return;
      const evs = new Map();
      for (const l of await Promise.all(rs.map(r => relayFetch(r, q.tags, q.since)))) for (const e of l) if (e && e.id) evs.set(e.id, e);
      const ps = [];
      for (const [id, e] of evs) {
        if (nSeen.has(id)) continue;
        const r = yggNostrOpen(JSON.stringify(e)); if (r.error) continue;
        const w = wireOf(r.w); if (!w || (w.t !== 'msg' && w.t !== 'del' && w.t !== 'edit')) continue; // checked like a datagram (r2); unsends / edits are parked too since v0.19.4
        learnPK(r.from, r.pk);
        ps.push({ id, r, w });
      }
      // Oldest first (the message's own time; created_at is blurred), so per-chat state ends at the newest (v0.19.2).
      // Unsends / edits first: they leave a tombstone / early edit, so a copy of the post in the same round can't show (v0.19.4).
      ps.sort((a, b) => ((a.w.t === 'msg') - (b.w.t === 'msg')) || ((+a.w.ts || 0) - (+b.w.ts || 0)) || (String(a.w.id) < String(b.w.id) ? -1 : String(a.w.id) > String(b.w.id) ? 1 : 0));
      for (const { id, r, w } of ps) if (await handleParked(r.from, w)) { nSeen.add(id); send(r.from, { t: 'ack', id: w.id }); if (r.del) nostrPublish(rs, r.del); }
    } finally { nBusy = false; }
  }
  // A packet read from a box (the sender is proven by the box, like a datagram's source). true = ack.
  async function handleParked(from, w) {
    if (w.t === 'msg') return handleMsg(from, w);
    if (st.blocked[from]) return false;
    if (w.g) return w.t === 'del' ? onGroupDel(from, w) : /^[0-9A-Za-z_-]{1,40}$/.test(String(w.id || ''));
    return onCtl(from, w);
  }
  // Seal w for peer and park it; returns the boxes made [{i: event id, k: throw-away key, r: relays, x: expiry}]
  // so they can be recalled (unsend), or [] when nothing was accepted (v0.19.4).
  async function nostrPutWire(peer, w) {
    const rs = nRelays(); if (!rs) return [];
    const c = st.contacts[peer]; if (!c || !c.pk) return [];
    const pkt = JSON.stringify(w), refs = [];
    for (const leg of st.nostrLegacy ? [false, true] : [false]) {
      if (typeof yggNostrWrapK === 'function') {
        let b; try { b = JSON.parse(yggNostrWrapK(c.pk, pkt, leg) || 'null'); } catch { b = null; }
        if (b && b.ev && await nostrPublish(rs, b.ev)) refs.push({ i: b.id, k: b.nk, r: rs.slice(), x: now() + 8 * 86400 });
      } else { // ygg.wasm before v0.19.4: parked, but can't be recalled
        const ev = yggNostrWrap(c.pk, pkt, leg);
        if (ev && await nostrPublish(rs, ev)) refs.push({ i: '', k: '', r: [], x: 0 });
      }
    }
    return refs;
  }
  async function nostrPark(m) {
    const rs = nRelays(); if (!rs || m.img || m.nostr) return;
    if (m.grp && st.groups[m.grp] && st.groups[m.grp].members.length > NOSTRG) return; // don't flood relays for a big group
    const c = st.contacts[m.peer]; if (!c || !c.pk) return;
    { const x0 = st.messages.find(y => y.peer === m.peer && y.id === m.id); if (!x0 || x0.status !== 'queued') return; } // delivered, deleted or unsent meanwhile
    const refs = await nostrPutWire(m.peer, await msgPkt(m));
    if (!refs.length) return;
    const real = refs.filter(r => r.i);
    const x = st.messages.find(y => y.peer === m.peer && y.id === m.id);
    if (!x) { nostrRecall(real); return; } // unsent, deleted or (a group copy) delivered while we were parking it
    if (real.length) { x.nev = (x.nev || []).concat(real); save(); } // to recall the boxes on unsend
    if (x.status === 'queued') { x.nostr = true; setStatus(m.peer, m.id, 'stored'); }
  }
  // ---- recalling our boxes (v0.19.4, nostrrecall.go): NIP-09 deletions signed by each box's key, kept in
  // st.nrecall until every relay took them (or the box expired), so an offline recall still happens later.
  const NRECALLMAX = 2000;
  const ctlRefs = cs => cs.reduce((a, c) => a.concat(c.nev || []), []);
  function nostrRecall(refs) {
    refs = (refs || []).filter(r => r && r.i && r.k && r.r && r.r.length && r.x > now());
    if (!refs.length) return;
    const n = now();
    st.nrecall = (st.nrecall || []).filter(r => r.x > n && r.r && r.r.length).concat(refs.map(r => Object.assign({}, r, { r: r.r.slice() }))).slice(-NRECALLMAX);
    save(); recallAgain = true; runRecall();
  }
  function relayPublishMany(url, evs) { // evs: Map id -> event JSON; resolves the Set of ids the relay took
    const got = new Set(); let left = evs.size;
    if (!left) return Promise.resolve(got);
    return new Promise(res => {
      let ws, done = false;
      const fin = () => { if (done) return; done = true; clearTimeout(t); try { ws && ws.close(); } catch {} res(got); };
      const t = setTimeout(fin, 20000);
      try { ws = new WebSocket(url); } catch { return fin(); }
      ws.onopen = () => { for (const ev of evs.values()) ws.send('["EVENT",' + ev + ']'); };
      ws.onmessage = e => { let m; try { m = JSON.parse(e.data); } catch { return; } if (!Array.isArray(m) || m[0] !== 'OK' || !evs.has(m[1])) return; if (m[2] === true) got.add(m[1]); if (--left <= 0) fin(); };
      ws.onerror = fin; ws.onclose = fin;
    });
  }
  let recallBusy = false, recallAgain = false;
  async function runRecall() {
    if (recallBusy || locked) return;
    recallBusy = true;
    try {
      do {
        recallAgain = false;
        const todo = (st.nrecall || []).slice(); if (!todo.length) break;
        const byRelay = new Map(), boxOf = new Map(), bad = new Set();
        for (const r of todo) {
          let d = ''; try { d = typeof yggNostrDel === 'function' ? yggNostrDel(r.k, r.i) : ''; } catch {}
          let id = ''; try { id = d ? JSON.parse(d).id : ''; } catch {}
          if (!id) { if (typeof yggNostrDel === 'function') bad.add(r.i); continue; }
          boxOf.set(id, r.i);
          for (const u of r.r) { if (!byRelay.has(u)) byRelay.set(u, new Map()); byRelay.get(u).set(id, d); }
        }
        const done = new Map();
        await Promise.all([...byRelay].map(async ([u, evs]) => { const g = await relayPublishMany(u, evs); done.set(u, new Set([...g].map(x => boxOf.get(x)))); }));
        if (locked) break;
        const n = now();
        st.nrecall = (st.nrecall || []).filter(r => !bad.has(r.i) && r.x > n).map(r => Object.assign(r, { r: r.r.filter(u => !(done.get(u) && done.get(u).has(r.i))) })).filter(r => r.r.length);
        save();
      } while (recallAgain);
    } finally { recallBusy = false; }
  }
  // Park the pending control packets for p whose message went out as a box (not parked yet).
  const ctlParking = new Set();
  async function parkCtl(p) {
    if (typeof yggNostrWrapK !== 'function' || ctlParking.has(p)) return; // (an older ygg.wasm can't recall: don't park)
    ctlParking.add(p);
    try {
      for (const c of (st.ctl || []).filter(x => x.peer === p && x.park && !(x.nev && x.nev.length))) {
        const refs = (await nostrPutWire(p, c.w)).filter(r => r.i);
        if (!refs.length) continue;
        const x = (st.ctl || []).find(y => y.peer === p && y.w.id === c.w.id);
        if (x) { x.nev = (x.nev || []).concat(refs); save(); } else nostrRecall(refs); // acked meanwhile
      }
    } finally { ctlParking.delete(p); }
  }
  const cleanRelays = l => [...new Set((Array.isArray(l) ? l : []).map(r => String(r).trim()).filter(r => { try { const u = new URL(r); return u.protocol === 'wss:' && !!u.host && r.length <= 200; } catch { return false; } }))].slice(0, 8);

  // ---- disappearing sweep ----
  setInterval(() => {
    if (locked) return;
    const t = now(), gone = st.messages.filter(m => m.exp && m.exp <= t);
    if (!gone.length) return;
    for (const m of gone) goneIfRecent(m); // a late copy must not bring it back (v0.19.3)
    st.messages = st.messages.filter(m => !(m.exp && m.exp <= t)); save();
    for (const m of gone) emit('msg_del', { peer: m.peer, id: m.id });
  }, 2000);

  // One tab runs the node: two tabs with the same key fight over the address
  // and each keep their own copy of the history (v0.19.1).
  function tabCover(text, btn, onClick, sub) {
    const d = document.createElement('div');
    // the page's theme colours (light/dark, green accent)
    d.style.cssText = 'position:fixed;inset:0;z-index:99999;display:flex;flex-direction:column;gap:12px;align-items:center;justify-content:center;background:var(--bg,#121212);color:var(--fg,#e0e0e0);font:16px system-ui,sans-serif;text-align:center;padding:24px';
    const p = document.createElement('div'); p.textContent = text; p.style.fontWeight = '600'; d.appendChild(p);
    if (sub) { const q = document.createElement('div'); q.textContent = sub; q.style.cssText = 'font-size:13px;color:var(--dim,#a0a0a0)'; d.appendChild(q); }
    const b = document.createElement('button'); b.textContent = btn;
    b.style.cssText = 'margin-top:6px;padding:10px 22px;border-radius:999px;border:0;background:var(--accent,#2ecc71);color:#111;font-size:15px;font-weight:600';
    b.onclick = () => { b.disabled = true; onClick(); }; d.appendChild(b);
    const put = () => document.body.appendChild(d);
    document.body ? put() : addEventListener('DOMContentLoaded', put, { once: true });
    return d;
  }
  async function tabLock() {
    if (!(navigator.locks && navigator.locks.request) || typeof BroadcastChannel === 'undefined') return;
    const bc = new BroadcastChannel('yggchat-tab');
    const hold = () => new Promise(got => {
      navigator.locks.request('yggchat-node', () => { got(); return new Promise(() => {}); });
    });
    const take = () => new Promise(got => {
      navigator.locks.request('yggchat-node', { ifAvailable: true }, l => { if (!l) { got(false); return; } got(true); return new Promise(() => {}); });
    });
    // Write pending state before going passive: the other tab reads the store as
    // soon as our lock is released, and a write cut off by the reload was lost.
    bc.onmessage = async e => { if (e.data === 'take' && holding) { holding = false; try { sessionStorage.setItem('yggpassive', '1'); } catch {} await Promise.race([saveNow(), new Promise(r => setTimeout(r, 3000))]); location.reload(); } };
    let holding = false, passive = false;
    try { passive = sessionStorage.getItem('yggpassive') === '1'; sessionStorage.removeItem('yggpassive'); } catch {}
    if (!passive && await take()) { holding = true; return; }
    await new Promise(go => {
      const cover = tabCover('YggChatは別のタブで開いています', 'このタブで使う', async () => { bc.postMessage('take'); await hold(); cover.remove(); go(); }, 'もう一方のタブは停止します');
    });
    holding = true;
  }

  // A wasm cached half-way (e.g. fetched while a deploy was in progress) never
  // parses: fetch it fresh from the network and replace only that cache entry.
  // Offline, or on a failure that isn't the file's (out of memory), the caches
  // stay as they are; wiping them all used to leave an offline PWA with nothing.
  async function loadWasm(go) {
    let first;
    try { return await WebAssembly.instantiateStreaming(fetch('ygg.wasm'), go.importObject); } catch (e) { first = e; }
    try { // a server/worker that sends the wrong MIME type: same bytes, non-streaming
      const r = await fetch('ygg.wasm');
      if (r.ok && !/text\/html/i.test(r.headers.get('content-type') || '')) return await WebAssembly.instantiate(await r.arrayBuffer(), go.importObject);
    } catch (e) { if (e instanceof RangeError) throw e; }
    if (navigator.onLine === false) throw new Error('オフラインのためアプリ本体(ygg.wasm)を読み込めません: ' + (first && first.message));
    // Drop just the bad copy first (an older service worker answers cache-first,
    // query string ignored), remembering where it was to put the good one back.
    const had = [];
    try { for (const k of await caches.keys()) if (await (await caches.open(k)).delete('ygg.wasm', { ignoreSearch: true })) had.push(k); } catch {}
    let buf;
    try {
      const r = await fetch('ygg.wasm?fresh=' + Date.now(), { cache: 'reload' });
      if (!r.ok) throw new Error('HTTP ' + r.status);
      buf = await r.arrayBuffer();
    } catch (e) { throw new Error('ygg.wasm を取得できません: ' + e.message); }
    const res = await WebAssembly.instantiate(buf, go.importObject);
    try {
      for (const k of had) await (await caches.open(k)).put('ygg.wasm', new Response(buf, { headers: { 'Content-Type': 'application/wasm' } }));
      for (const k of await caches.keys()) { const c = await caches.open(k); for (const q of await c.keys()) if (/[?&]fresh=/.test(q.url)) await c.delete(q); }
    } catch {}
    try { navigator.serviceWorker && navigator.serviceWorker.getRegistration().then(r => r && r.update()); } catch {}
    return res;
  }
  // Save what is pending now (the page is about to reload for an update).
  window.yggSaveNow = () => saveNow();
  const ready = (async () => {
    await tabLock();
    await loadStore();
    const go = new Go();
    const wasmReady = new Promise(r => addEventListener('yggready', r, { once: true }));
    // A wasm cached half-way (e.g. fetched while a deploy was in progress) never
    // parses; drop the caches and fetch it fresh from the network once.
    const res = await loadWasm(go);
    go.run(res.instance);
    await wasmReady;
    const r = yggStart(outer.key, outer.peers, onPacket);
    if (r.error) throw new Error(r.error);
    // Write a freshly made key before anything else, so a quick reload right
    // after creating an ID doesn't throw it away; ask the browser not to evict it.
    const fresh = r.key !== outer.key;
    addr = r.addr; outer.key = r.key; saveOuter();
    if (fresh) await saving;
    try { navigator.storage && navigator.storage.persist && navigator.storage.persist(); } catch {}
    if (outboxRestored && !locked) { outboxRestored = false; save().then(ok => { if (ok) try { localStorage.removeItem(OUTBOX); } catch {} }); } // sends caught by the outbox are in IndexedDB now
    let last = -1;
    setInterval(() => {
      const n = yggPeers();
      if (n !== last) { if (n > 0 && last <= 0) { setTimeout(helloAll, 1500); setTimeout(syncAllGroups, 6000); } last = n; emit('peers', { up: n }); }
    }, 3000);
    setInterval(() => { if (yggPeers() > 0 && !locked) syncAllGroups(); }, 600000);
    // Probe peers we still owe messages to, plus everyone now and then so they can flush to us.
    setInterval(() => { if (yggPeers() > 0 && !locked) for (const p of new Set([...st.messages.filter(m => m.out && (m.status === 'queued' || m.status === 'stored')).map(m => m.peer), ...(st.ctl || []).map(c => c.peer)])) hello(p); }, 20000);
    setInterval(() => { if (yggPeers() > 0) helloAll(); }, 120000);
    addEventListener('online', () => setTimeout(() => { helloAll(); nostrFetch(); }, 3000));
    document.addEventListener('visibilitychange', () => { if (document.hidden) { saveNow(); return; } if (yggPeers() > 0) helloAll(); nostrFetch(); });
    addEventListener('pagehide', () => saveNow()); // write what is pending before the page goes away (v0.19.2)
    setTimeout(nostrFetch, 5000);
    setInterval(() => { runRecall(); nostrFetch(); }, 180000);
    setTimeout(runRecall, 6000);
  })();

  // The node could not start: say why (the UI alone only shows "offline" forever).
  ready.catch(e => {
    console.error('yggchat start', e);
    if (e && e.store) tabCover('保存データを読み込めませんでした', '再読み込み', () => location.reload(), 'データは消していません。ほかのタブを閉じてから再読み込みしてください。直らないときはブラウザの空き容量を確認してください（' + (e.cause && e.cause.name || '') + '）');
    else tabCover('YggChatを起動できませんでした', '再読み込み', () => location.reload(), String((e && e.message) || e).slice(0, 300));
  });
  const norm = a => { try { return new URL('http://[' + a.trim() + ']/').hostname.slice(1, -1) } catch { return null } };
  const isYgg = a => !!a && /^0?[23][0-9a-f]{0,2}:/.test(a);
  const cleanDomain = d => { d = String(d || '').trim().toLowerCase().replace(/\.$/, ''); return (d.length <= 253 && /^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9-]{2,63}$/.test(d)) ? d : ''; };
  // First Yggdrasil AAAA of a domain over DNS-over-HTTPS -> {addr} | {err:'none'|'net'}
  async function resolveYgg(name) {
    for (const s of ['https://cloudflare-dns.com/dns-query', 'https://dns.google/resolve']) {
      let j; try { const r = await fetch(s + '?type=AAAA&name=' + encodeURIComponent(name), { headers: { accept: 'application/dns-json' }, referrerPolicy: 'no-referrer', credentials: 'omit' }); j = await r.json(); } catch { continue; }
      const a = (j.Answer || []).find(x => x.type === 28 && /^[23][0-9a-f]{2}:/i.test(x.data || ''));
      const n = a && norm(a.data);
      return n ? { addr: n } : { err: 'none' };
    }
    return { err: 'net' };
  }
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
        return ok({ addr, pubkey: myPK(), nick: st.nick, bio: st.bio, avatar: st.avatar, domain: st.domain || '', contacts: Object.assign({}, st.contacts, Object.fromEntries(Object.values(st.groups || {}).filter(g => !g.tomb).map(g => ['g:' + g.id, gView(g)]))), messages: st.messages.filter(m => !m.hid), peers: yggPeers(), platform: 'web', locked: false, haspass: !!aesKey, blocked: Object.keys(st.blocked), nostr: nostrState(), powstrict: !!st.powStrict, joins: joinsView() });
      case 'unlock': {
        if (!locked) return ok({});
        try {
          const k = await kdf(String(body.Pass || ''), unb64(outer.enc.salt));
          const plain = await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(outer.enc.iv) }, k, unb64(outer.enc.ct));
          st = Object.assign(blank(), JSON.parse(new TextDecoder().decode(plain)));
          aesKey = k; locked = false;
          { const o = lsList(OUTBOX_ENC); if (o.length) { const got = []; for (const e of o) { if (!e || e.salt !== outer.enc.salt) continue; try { got.push(JSON.parse(new TextDecoder().decode(await crypto.subtle.decrypt({ name: 'AES-GCM', iv: unb64(e.iv) }, k, unb64(e.ct))))); } catch {} } mergeOutbox(st, got); } }
          migrate();
          if (lsList(OUTBOX_ENC).length) { const o = lsList(OUTBOX_ENC); save().then(ok => { if (ok) lsSet(OUTBOX_ENC, lsList(OUTBOX_ENC).filter(e => !o.some(x => x.id === e.id))); }); }
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
        if (!(await save())) return bad('パスコードを保存できませんでした（端末の保存領域のエラー）', 500); // the store on disk would not match what the user was told
        return ok({});
      }
      case 'lock': {
        if (!aesKey) return bad('パスコードが未設定です');
        // Every write queued until now goes out first (a save queued while the
        // last one ran used to land after the lock, with blank state).
        let wrote = false;
        for (let s0 = save(); ; s0 = saving) { wrote = await s0; if (s0 === saving && !saveQueued) break; }
        if (!wrote) return bad('保存できなかったのでロックしませんでした（ロックすると未保存の内容が消えます）', 500);
        if (outboxEncPending.size) await Promise.allSettled([...outboxEncPending]);
        locked = true; aesKey = null; st = blank();
        emit('locked', { locked: true });
        return ok({});
      }
      case 'send': {
        if (isG(body.To)) {
          const gid = String(body.To).slice(2);
          if (body.Retry) {
            const v = st.messages.find(x => x.peer === body.To && x.id === body.Retry && x.out); if (!v) return bad('no such message', 404);
            if (gOut(gid)) return bad('退出したグループには再送できません');
            const cs = st.messages.filter(m => m.hid && m.grp === gid && m.of === v.id && m.status !== 'sent');
            if (!cs.length) { groupStatus(gid, v.id); return ok(v); }
            for (const c of cs) { setStatus(c.peer, c.id, 'sending'); deliverCopy(c); }
            return ok(v);
          }
          const t = String(body.Text || ''); if ((!t && !body.Img) || u8len(t) > MAXSEND) return bad('message empty or too long');
          if (body.Img && goLen(t) - 2 > CAPMAX) return bad('写真の説明が長すぎます（日本語でおよそ260文字まで）');
          if (body.Img && !parseImg(body.Img)) return bad('unsupported or too large image (max 600KB)');
          if (body.Img) { const pi = parseImg(body.Img); if (!imgDimsOK(pi.mime, pi.bytes)) return bad(IMGDIMS); }
          const gg = st.groups[gid]; if (body.Img && gg && gg.set && gg.set.nf) return bad('このグループでは写真を送れません（ホストの設定）');
          const v = sendGroup(gid, t, body.Img || ''); if (!v) return bad('このグループには送れません');
          outboxPut(v, st.messages.filter(m => m.hid && m.grp === gid && m.of === v.id));
          if (await saveNow()) outboxDrop(v.id); return ok(v); // stored before the UI shows it (v0.19.2)
        }
        if (body.Retry) {
          const m = st.messages.find(x => x.peer === norm(body.To || '') && x.id === body.Retry && x.out);
          if (!m) return bad('no such message', 404);
          setStatus(m.peer, m.id, 'sending'); deliver(m); return ok(m);
        }
        const dst = norm(body.To || '');
        if (!isYgg(dst)) return bad('not a Yggdrasil address: ' + body.To);
        if ((!body.Text && !body.Img) || u8len(body.Text) > MAXSEND) return bad('message empty or too long');
        if (body.Img && !parseImg(body.Img)) return bad('unsupported or too large image (max 600KB)');
        if (body.Img) { const pi = parseImg(body.Img); if (!imgDimsOK(pi.mime, pi.bytes)) return bad(IMGDIMS); }
        if (body.Img && goLen(String(body.Text || '')) - 2 > CAPMAX) return bad('写真の説明が長すぎます（日本語でおよそ260文字まで）');
        const c = st.contacts[dst];
        const share = !c || !c.shared;
        if (c && (c.req || !c.shared)) { c.req = false; c.shared = true; emit('contact', c); }
        const ttl = (c && c.ttl) || 0;
        const m = { id: Date.now().toString(16) + Math.random().toString(16).slice(2, 6), peer: dst, from: addr, nick: st.nick, text: body.Text || '', ts: now(), out: true, status: 'sending' };
        if (body.Img) m.img = body.Img;
        if (ttl) { m.ttl = ttl; m.exp = m.ts + ttl; }
        addMessage(m); outboxPut(m, []); if (share) sendProf(dst);
        deliver(m).then(ok => { if (ok && queuedFor(dst).length) flush(dst); }); // older queued ones follow at once (v0.19.3)
        if (await saveNow()) outboxDrop(m.id); return ok(m); // stored before the UI shows it (v0.19.2)
      }
      case 'group/sync': { syncGroup(String(body.ID || '')); return ok({}); }
      case 'group/create': {
        const ms = cleanMembers(body.Members, addr); if (ms.length < 2) return bad('メンバーを1人以上選んでください');
        const id = hexOf(crypto.getRandomValues(new Uint8Array(8)));
        const g = st.groups[id] = { id, name: cleanNick(body.Name) || 'グループ', members: ms, owner: addr, v: 1, vat: now(), set: { hi: true } };
        { const mx = GMAXES.find(c => c >= ms.length); if (mx && mx > GDEF) g.set.mx = mx; }
        save(); emit('contact', gView(g)); gSys(g, 'グループ「' + g.name + '」を作りました'); gInfo(g); noteInvited(id, ms); return ok(gView(g));
      }
      case 'group/update': {
        const g = st.groups[body.ID]; if (!g || g.left) return bad('no such group');
        const host = !!g.owner && g.owner === addr;
        const rmReq = Array.isArray(body.Remove) && body.Remove.length, addReq = Array.isArray(body.Add) && body.Add.length;
        // Permission checks first, so a refused request changes nothing.
        if ((addReq || rmReq) && g.owner && !canMng(g, addr)) return bad('メンバーを追加・削除できるのはホストと副管理者だけです', 403);
        if ((body.Set || body.Pass != null || body.Admins) && !host) return bad('設定を変えられるのはホストだけです', 403);
        if (rmReq && !g.owner) return bad('このグループにはホストがいないので外せません（v0.16.0より前に作ったグループ）');
        if (body.Pass != null) { const p = String(body.Pass); if (p && ([...p].length < 4 || [...p].length > 64)) return bad('パスワードは4〜64文字にしてください'); }
        const ns = body.Set ? cleanSet(body.Set) : null;
        if (ns && gMax({ set: ns }) < g.members.length) return bad('今のメンバー（' + g.members.length + '人）より少ない上限にはできません');
        const added = [...new Set((body.Add || []).map(x => norm(String(x || ''))).filter(a => a && isYgg(a) && !g.members.includes(a)))];
        if (g.members.length + added.length > gMax(ns ? { set: ns } : g)) return bad('このグループの上限は' + gMax(ns ? { set: ns } : g) + '人です（グループ設定で変えられます）');
        const rm = rmReq ? body.Remove.map(x => norm(String(x || ''))).filter(x => x && x !== addr) : [];
        if (!host && rm.some(x => x === g.owner || isAdm(g, x))) return bad('ホストと副管理者は外せません', 403);
        if (body.Admins && body.Admins.length > MAXADM) return bad('副管理者は' + MAXADM + '人までです');
        const old = { name: g.name, members: g.members.slice(), admins: (g.admins || []).slice() };
        const oldSet = Object.assign({}, g.set || {}), hadPw = !!g.pwh;
        if (body.Name != null && cleanNick(body.Name)) g.name = cleanNick(body.Name);
        let pwChanged = false;
        if (body.Pass != null) { const p = String(body.Pass); if (p) g.pwh = await hashPass(p); else delete g.pwh; pwChanged = true; }
        if (ns) g.set = ns;
        if (ns || pwChanged) g.set = cleanSet(Object.assign({}, g.set || {}, { pw: !!g.pwh }));
        g.members = cleanMembers(g.members.concat(added), addr);
        const removed = g.members.filter(m => rm.includes(m)); g.members = g.members.filter(m => !rm.includes(m));
        if (body.Admins) g.admins = cleanAdmins(body.Admins, g);
        g.admins = cleanAdmins(g.admins, g); if (!g.admins.length) delete g.admins;
        if (added.length || removed.length) {
          bumpV(g); if (host) trackGone(g, old.members); noteGone(g, old.members, 0);
          addBan(g, removed); for (const m of added) unban(g, m); // removed by us: their link joins are refused (v0.19.3)
        }
        if (host && (ns || pwChanged || body.Admins || g.name !== old.name)) g.sv = (g.sv | 0) + 1; // a late repeat of the old info can't undo this
        if (g.pend) { g.pend = g.pend.filter(p => !added.includes(p.addr)); if (!g.pend.length) delete g.pend; }
        g.req = false;
        save(); emit('contact', gView(g));
        if (g.name !== old.name) gSys(g, 'グループ名を「' + g.name + '」にしました');
        for (const t of setDiff(oldSet, g.set || {})) gSys(g, 'あなたが' + t);
        if (body.Pass && hadPw) gSys(g, 'あなたが参加パスワードを変更しました');
        memberSys(g, addr, added, removed); adminSys(g, addr, old.admins, g.admins || []);
        if (added.length || removed.length) gMembers(g, added, removed, removed.length > 0); // the removed hear it too, so they know they're out
        if (added.length && g.set && g.set.hi) { const h = historyOf(g.id); if (h.length) for (const x of added) sendHistory(x, g.id, h); }
        if (g.name !== old.name || ns || pwChanged || body.Admins) gInfo(g);
        if (host) { const na = (g.admins || []).filter(x => !old.admins.includes(x) || pwChanged); if (na.length) sendKey(g, na); }
        return ok(gView(g));
      }
      case 'group/invite': {
        const g = st.groups[body.ID]; if (!g || g.left || !canMng(g, addr)) return bad('招待リンクを作れるのはホストと副管理者だけです', 403);
        if (!g.tok || body.Reset) { g.tok = hexOf(crypto.getRandomValues(new Uint8Array(8))); save(); }
        const code = encodeJoin(addr, g.id, g.tok);
        return ok({ code, link: 'https://yggchat.surge.sh/#j=' + code });
      }
      case 'group/join': {
        if (body.Cancel) { let gid = String(body.ID || '').replace(/^g:/, ''); if (body.Code) { try { gid = decodeJoin(String(body.Code)).gid; } catch (e) {} } delJoin(gid); return ok({}); }
        let j; try { j = decodeJoin(String(body.Code || '')); } catch (e) { return bad(e.message); }
        if (j.host === addr) return bad('自分のグループの招待リンクです');
        const g0 = st.groups[j.gid];
        if (g0 && !g0.left && !g0.req && hasM(g0, addr)) return ok({ already: true, addr: 'g:' + j.gid });
        // Joining again starts fresh; so does an unanswered invitation (anyone who knows the group id can make one,
        // and its list could leave out the host so the host's answer is refused), v0.19.3.
        if (g0 && (g0.left || g0.req)) { delete st.groups[j.gid]; save(); }
        setJoin(j.gid, j.host, JOINTTL);
        const jw = { t: 'gj', g: j.gid, tok: j.tok, nick: st.nick, pw: body.Pw ? String(body.Pw) : undefined, pn: joinPow(addr, j.gid, j.tok) };
        hello(j.host); // the first packets to a new peer often go nowhere while its route is looked up
        const jt = joinTries[j.gid] = { n: ((joinTries[j.gid] || {}).n || 0) + 1, pw: !!body.Pw };
        let wait = 0; for (const d of [1, 2, 2, 3, 4, 5, 6]) { wait += d; setTimeout(() => { if (joinHost(j.gid) && joinTries[j.gid] === jt) send(j.host, jw); }, wait * 1000); }
        return ok({ addr: 'g:' + j.gid });
      }
      case 'group/approve': {
        const g = st.groups[body.ID]; if (!g || g.left || !canMng(g, addr)) return bad('承認できるのはホストと副管理者だけです', 403);
        const a = norm(String(body.Addr || '')), p = (g.pend || []).find(x => x.addr === a); if (!p) return bad('その申請はありません');
        if (!body.OK) { g.pend = g.pend.filter(x => x.addr !== a); if (!g.pend.length) delete g.pend; save(); emit('contact', gView(g)); send(a, { t: 'gjn', g: g.id, text: '参加は承認されませんでした' }); return ok(gView(g)); }
        if (g.members.length >= gMax(g)) return bad('グループが満員です');
        admit(g, a, p.nick, '参加しました（承認）'); return ok(gView(g));
      }
      case 'group/leave': {
        const g = st.groups[body.ID]; if (!g) return bad('no such group');
        const was = g.left;
        // A leaving host hands the group to the first co-admin, so it can still be run.
        // (the co-admin we heard from last, most likely online; re-sent if it missed it, v0.19.3)
        let heir = '';
        if (!was && g.owner === addr && (g.admins || []).length) { heir = pickHeir(g); g.owner = heir; g.admins = cleanAdmins(g.admins, g); if (!g.admins.length) delete g.admins; bumpV(g); g.sv = (g.sv | 0) + 1; g.hand = true; }
        const full = Object.assign({}, g, { members: g.members.slice() }); // the list as it was, for the hand-over and our gl
        if (!was) { // we are out: our own list no longer shows us (v0.19.3)
          g.members = g.members.filter(m => m !== addr); g.admins = cleanAdmins(g.admins, g); if (!g.admins.length) delete g.admins;
          g.qat = now(); // our gl is repeated on reconnects for a while
        }
        g.left = true; g.req = false; g.quit = true; save(); emit('contact', gView(g));
        if (heir) { gInfo(full); gSys(g, nameOf(heir) + ' をホストにしました'); }
        if (!was) {
          // Posts still on their way get one last push before the gl (a pre-v0.19.2 member drops
          // them once it has it); what is left is failed. gl says when we left (v0.19.2).
          const at = g.qat, ms = full.members.slice();
          flushBeforeLeave(g.id, 8000).then(() => { failLeftCopies(g.id); const d = heir ? 5000 : 0; for (let i = 0; i < 3; i++) setTimeout(() => { for (const m of ms) if (m !== addr) send(m, { t: 'gl', g: g.id, ts: at }); }, d + i * 2000); });
          gSys(g, 'グループを退出しました');
        }
        return ok(gView(g));
      }
      case 'contact': {
        if (isG(body.Addr)) {
          const g = st.groups[String(body.Addr).slice(2)]; if (!g) return bad('no such group');
          if (body.Nick != null && cleanNick(body.Nick)) g.name = cleanNick(body.Nick);
          if (body.Accept) g.req = false;
          save(); emit('contact', gView(g)); return ok(gView(g));
        }
        const a = norm(body.Addr || ''); if (!isYgg(a)) return bad('not a Yggdrasil address');
        const exists = !!st.contacts[a];
        const c = st.contacts[a] || (st.contacts[a] = { addr: a, nick: '' });
        if (body.Nick != null) { c.nick = cleanNick(body.Nick); c.renamed = exists; }
        const share = body.Accept && (c.req || !c.shared);
        if (body.Accept) { c.req = false; c.shared = true; }
        delete st.blocked[a];
        save(); emit('contact', c);
        if (share) sendProf(a);
        if (body.TTL != null) setTTL(a, body.TTL, true); // out of range: ignored, as native
        return ok(c);
      }
      case 'contact/delete': {
        if (isG(body.Addr)) {
          const gid = String(body.Addr).slice(2), g = st.groups[gid];
          // A group we left stays as a hidden tombstone, so a late copy doesn't bring it back (v0.19.2).
          if (g && g.left) { g.tomb = true; delete g.tok; delete g.pwh; delete g.pend; } else delete st.groups[gid];
          st.messages = st.messages.filter(m => m.peer !== body.Addr && m.grp !== gid); save(); emit('contact_del', { addr: body.Addr }); return ok({});
        }
        const a = norm(body.Addr || ''); if (!a) return bad('bad address');
        delete st.contacts[a]; st.messages = st.messages.filter(m => { if (m.peer !== a || m.hid) return true; goneIfRecent(m); return false; }); save(); // pending group copies to them still count (v0.19.2)
        emit('contact_del', { addr: a }); return ok({});
      }
      case 'block': {
        const a = norm(body.Addr || ''); if (!a) return bad('bad address');
        if (body.Block) { st.blocked[a] = true; delete st.contacts[a]; st.messages = st.messages.filter(m => { if (m.peer !== a || m.hid) return true; goneIfRecent(m); return false; }); emit('contact_del', { addr: a }); }
        else delete st.blocked[a];
        save(); return ok({});
      }
      case 'nick': st.nick = cleanNick(body.Nick) || 'anon'; save(); for (const p in st.contacts) if (st.contacts[p].shared) sendProf(p); return ok({});
      case 'profile': {
        if (body.Domain != null) {
          const d = cleanDomain(body.Domain);
          if (!d && String(body.Domain).trim()) return bad('ドメイン名ではありません');
          if (d) { const r = await resolveYgg(d); if (!r.addr) return bad(r.err === 'net' ? 'DNSに問い合わせできませんでした' : d + ' にはYggdrasilアドレス（200::/7）のAAAAレコードがありません'); if (r.addr !== addr) return bad(d + ' のAAAAは ' + r.addr + ' で、あなたのアドレスではありません'); }
          st.domain = d || undefined;
        }
        st.nick = cleanNick(body.Nick) || 'anon'; st.bio = cleanText(body.Bio, 140); st.avatar = cleanText(body.Avatar, 8);
        save(); for (const p in st.contacts) if (st.contacts[p].shared) sendProf(p); return ok({});
      }
      case 'move/recv': {
        if (body.Stop) { if (!mvIn.done) Object.assign(mvIn, { on: false, id: '', chunks: null, key: null, auth: false }); return ok({}); }
        const code = yggMoveNew(addr); if (!code) return bad('failed', 500);
        const rpk = mvCodeKey(code); if (!rpk) return bad('failed', 500);
        Object.assign(mvIn, { on: true, id: '', chunks: null, done: false, busy: false, key: await mvAuthKey(rpk), auth: false });
        return ok({ code });
      }
      case 'move/send': {
        if (body.Cancel) { mvOut.id = ''; return ok({}); }
        if (locked) return bad('ロックを解除してください');
        if (mvOut.id) return bad('送信中です');
        const r = yggMoveSeal(String(body.Code || ''), moveBundle(!!body.History));
        if (r.error) return bad(r.error);
        if (r.addr === addr) return bad('これはこの端末自身のコードです。新しい端末に出ているコードを入れてください');
        const rpk = mvCodeKey(body.Code); if (!rpk) return bad('移行コードが壊れています');
        const blob = unb64(r.blob); moveSend(r.addr, rpk, blob);
        return ok({ size: blob.length });
      }
      case 'move/export': {
        if (locked) return bad('ロックを解除してください');
        const r = yggMoveSealPass(String(body.Pass || ''), moveBundle(!!body.History));
        return r.error ? bad(r.error) : ok({ data: r.data });
      }
      case 'move/import': {
        const r = yggMoveOpenPass(String(body.Pass || ''), String(body.Data || ''));
        if (r.error) return bad(r.error);
        try { await importBundle(r.plain); } catch (e) { return bad(e.message); }
        return ok({});
      }
      case 'identity/reset': {
        if (!body.Confirm) return bad('confirm required');
        // Everything tied to the old address goes (chats, groups, pending edits, the verified domain).
        outer.key = ''; st.contacts = {}; st.messages = []; st.groups = {}; st.ctl = []; st.blocked = {}; st.joins = {}; st.gone = {}; st.early = {}; st.gdel = {}; st.gdelh = {}; delete st.domain; save(); saveOuter(); await saving;
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
      case 'resolve': {
        const name = cleanDomain(body.Name);
        if (!name) return bad('ドメイン名ではありません');
        const r = await resolveYgg(name);
        if (r.addr) return ok({ addr: r.addr, name });
        return r.err === 'net' ? bad('DNSに問い合わせできませんでした', 502) : bad(name + ' にはYggdrasilアドレス（200::/7）のAAAAレコードがありません', 404);
      }
      case 'nostr': {
        const was = !!st.nostrOn;
        if (body.On != null) st.nostrOn = !!body.On;
        if (body.Legacy != null) st.nostrLegacy = !!body.Legacy;
        if (body.Reset) st.nostrRelays = [];
        else if (body.Relays) st.nostrRelays = cleanRelays(body.Relays);
        save();
        if (st.nostrOn && !was) { nostrFetch(); for (const m of st.messages.filter(x => x.out && x.status === 'queued' && !x.nostr)) nostrPark(m); }
        return ok(nostrState());
      }
      case 'pow': {
        if (body.Strict != null) { st.powStrict = !!body.Strict; save(); }
        return ok({ strict: !!st.powStrict });
      }
      case 'message/edit': {
        const m = st.messages.find(x => x.peer === body.Peer && x.id === body.ID && x.out && !x.sys);
        const t = String(body.Text || '');
        if (!m || m.img) return bad('編集できるのは自分の文章メッセージだけです', 400);
        if (!t.trim() || u8len(t) > 8000) return bad('bad request', 400);
        if (goLen({ t: 'edit', id: ctlID(), ref: m.id, text: t }) > DGRAM) return bad('長すぎて編集できません（日本語でおよそ350文字まで）。送り直してね', 400);
        // A "queued" message may have reached them already (lost ack), v0.19.3; group posts are edited here only.
        m.text = t; const remote = !isG(m.peer) && m.status !== 'failed'; if (remote) m.edited = true; save();
        if (remote) queueCtl(m.peer, { t: 'edit', id: ctlID(), ref: m.id, text: t }, !!m.nostr);
        emit('msg_upd', m);
        return ok(m);
      }
      case 'message/delete': {
        const i = st.messages.findIndex(x => x.peer === body.Peer && x.id === body.ID);
        if (i < 0) return bad('not found', 404);
        const m = st.messages[i];
        if (isG(m.peer) && body.Everyone) { // group unsend / moderation (v0.19.4)
          if (m.sys || m.hid) return bad('bad request', 400);
          try { groupUnsend(m.peer.slice(2), m); } catch (e) { return bad(e.message, 400); }
          return ok({});
        }
        if (body.Everyone && !m.out) return bad('相手のメッセージは自分の端末からだけ削除できます', 400);
        st.messages.splice(i, 1);
        if (!m.out && !m.sys && !isG(m.peer)) markGone(m.peer, m.id); // a late copy must not bring it back
        if (!m.out && !m.sys && isG(m.peer) && m.from) markGDel(m.peer.slice(2), m.from, m.id, {}); // nor a catch-up relay (local only), v0.19.4
        const oldC = (st.ctl || []).filter(c => c.peer === m.peer && c.w.ref === m.id);
        st.ctl = (st.ctl || []).filter(c => !(c.peer === m.peer && c.w.ref === m.id));
        save();
        if (body.Everyone && m.out) nostrRecall((m.nev || []).concat(ctlRefs(oldC))); // unsend: our boxes of it are recalled (v0.19.4)
        // "queued" may still have reached them (lost ack, retry, Nostr box): everything but a failed send gets the unsend (v0.19.3).
        if (body.Everyone && !m.sys && m.status !== 'failed' && !isG(m.peer)) queueCtl(m.peer, { t: 'del', id: ctlID(), ref: m.id }, !!m.nostr);
        emit('msg_del', { peer: m.peer, id: m.id });
        return ok({});
      }
      case 'lan':
        return ok({ supported: false, on: false, nearby: [] });
      case 'finger':
        if (body) { finger = { on: !!body.On, plan: cleanPlan(body.Plan, 1500) }; await idbPut('finger', finger); }
        return ok({ on: finger.on, plan: finger.plan, tcp: 'web', addr });
      case 'finger/query': {
        const a = String(body.Addr || '').trim().replace(/^\[|\]$/g, '');
        if (!/^[0-9a-f:]+$/i.test(a) || !/^[23][0-9a-f]{2}:/i.test(a)) return bad('not a Yggdrasil address (200::/7)', 400);
        if (body.User) return bad('ブラウザ版はTCPのfingerができないので、YggChat同士の問い合わせだけです', 400);
        try { const w = await fingerQuery(a); return ok({ addr: a, via: 'yggchat', nick: cleanNick(w.nick), plan: cleanPlan(w.text, 700) }); }
        catch (e) { return bad(e.message, 504); }
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
  // Test hook (test/pwa/harness.js defines __yggTest before loading; never set in a page).
  if (typeof __yggTest === 'object' && __yggTest) Object.assign(__yggTest, { st: () => st, ready, onPacket, nostrFetch, wireOf, nostrPark, parkCtl, runRecall });
})();
