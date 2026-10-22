// Regression tests for the PWA shim's packet handling (round-2 fuzz/diff findings).
// node test/pwa/regress.js
const { makeNode } = require('./harness');
const Y = require('./ygg');
const crypto = require('crypto');
const K = { ME: Y.keypair('ME'), A: Y.keypair('A'), B: Y.keypair('B'), S: Y.keypair('S') };
const G1 = '00112233aabbccdd';
const now = () => Math.floor(Date.now() / 1000);
const sha = b => crypto.createHash('sha256').update(b).digest('hex');
let fails = 0, passes = 0;
const ok = (c, name) => { if (c) passes++; else { fails++; console.log('FAIL', name); } };
function store(extra) {
  const t = now(), ME = K.ME.addr, A = K.A.addr, B = K.B.addr;
  return Object.assign({
    key: K.ME.priv, peers: ['wss://x.test/'], nick: 'me', nostrOn: false,
    contacts: { [A]: { addr: A, nick: 'alice', shared: true, hasprof: true } },
    messages: [{ id: 'm1', peer: A, from: A, text: 'hello', ts: t - 100, out: false }],
    groups: { [G1]: { id: G1, name: 'g1', members: [ME, A, B].sort(), owner: ME, v: 1, sv: 1, set: {} } },
    blocked: {},
  }, extra || {});
}
async function node(extra, opts) { const n = makeNode(Object.assign({ kp: K.ME, store: store(extra) }, opts || {})); await n.ready(); return n; }
const wait = ms => new Promise(r => setTimeout(r, ms));
const keep = setInterval(() => {}, 1000);
const postSig = (kp, gid, pid, ts, text) => Buffer.from(Y.signHex(kp, 'ygg-gpost1\n' + gid + '\n' + pid + '\n' + ts + '\n' + sha(Buffer.from(text))), 'hex').toString('base64').replace(/=+$/, '');

(async () => {
  const A = K.A.addr, S = K.S.addr, B = K.B.addr;
  { // 1. a packet that made a handler throw ended the wasm program (Invoke panics): nothing may escape onPacket
    const n = await node();
    for (const p of ['null', '[]', '1', '"x"', 'true', '{"t":"ghq","g":"__proto__"}', '{"t":"gi","g":"' + G1 + '","gm":{"length":1}}', '{"t":"prof","nick":{"toString":1}}', '{"t":"ttl","ttl":{"valueOf":{}}}', '{"t":"msg","id":{"toString":1},"text":"x"}'])
      ok(n.pkt(A, p) === null, 'no throw: ' + p);
    await n.settle(10);
    ok(!n.logged.length, 'nothing thrown inside: ' + JSON.stringify(n.logged.map(String)).slice(0, 300));
    n.pkt(A, { t: 'msg', id: 'after', text: 'still alive', ts: now() }); await wait(20); await n.settle(10);
    ok(n.st.messages.some(m => m.id === 'after'), 'node keeps working after hostile packets');
  }
  { // 2. "_checked" from the wire skipped the proof of work
    const n = await node({ powStrict: true });
    n.pkt(S, { t: 'msg', id: 'spam1', text: 'buy', ts: now(), _checked: true }); await wait(20); await n.settle(10);
    ok(!n.st.messages.some(m => m.id === 'spam1'), '_checked does not bypass PoW');
    ok(n.sent.some(x => x.peer === S && x.w.t === 'pow'), 'stranger is asked for PoW');
  }
  { // 3. "__proto__" in a file header became the prototype of the copied header (h.g, h._checked inherited)
    const n = await node();
    const body = Buffer.from('a long text'), id = 'lt1';
    n.pkt(A, '{"__proto__":{"g":"' + G1 + '","_checked":true},"t":"fh","id":"' + id + '","mime":"text/plain","size":' + body.length + ',"n":1,"sha":"' + sha(body) + '","ts":' + now() + '}');
    n.pkt(A, { t: 'fc', id, i: 0, d: body.toString('base64') }); await wait(50); await n.settle(20);
    const m = n.st.messages.find(x => x.id === id);
    ok(m && m.peer === A, '__proto__ in fh ignored: long text stays 1:1');
    ok(n.run('({}).g') === undefined && ({}).g === undefined, 'no prototype pollution');
  }
  { // 4. wrong field types drop the packet like Go's json.Unmarshal
    const n = await node();
    n.pkt(A, { t: 'msg', id: 'wt1', text: 'x', ts: '12' }); n.pkt(A, { t: 'msg', id: 'wt2', text: ['x'] }); n.pkt(A, { t: 'msg', id: 'wt3', text: 'x', ttl: 1.5 });
    n.pkt(A, { t: 'msg', id: 'wt4', text: 'ok', ts: 2 ** 60 }); // Go int64 takes it
    n.pkt(A, { t: 'msg', id: 'wt5', text: 'x', ttl: 2 ** 32 + 300 }); // an int field past int32 (|0 made it 300)
    await wait(20); await n.settle(10);
    ok(!n.st.messages.some(m => /^wt[1235]$/.test(m.id)), 'wrong types / int32 overflow dropped');
    ok(n.st.messages.some(m => m.id === 'wt4'), 'big int64 ts accepted (clamped)');
  }
  { // 5. out-of-range ttl is ignored (was clamped: 99999999 -> 4 weeks, -5 -> off)
    const n = await node();
    n.st.contacts[A].ttl = 300;
    n.pkt(A, { t: 'ttl', ttl: 99999999 }); n.pkt(A, { t: 'ttl', ttl: -5 }); await n.settle(5);
    ok(n.st.contacts[A].ttl === 300, 'ttl out of range ignored, got ' + n.st.contacts[A].ttl);
    n.pkt(A, { t: 'ttl', ttl: 3600 }); await n.settle(5);
    ok(n.st.contacts[A].ttl === 3600, 'valid ttl applied');
  }
  { // 6. an upper-case key is learned (Go decodes hex in any case)
    const n = await node();
    n.pkt(A, { t: 'hiack', pk: K.A.pk.toUpperCase() }); await n.settle(5);
    ok(n.st.contacts[A].pk === K.A.pk, 'upper-case pk learned');
    n.pkt(A, { t: 'hiack', pk: K.S.pk }); await n.settle(5);
    ok(n.st.contacts[A].pk === K.A.pk, 'wrong pk refused');
  }
  { // 7. a signed post whose time was clamped keeps no signature (relaying it made others drop it as forged)
    const n = await node();
    const ts = now() + 5000, sg = postSig(K.A, G1, 'fut1', ts, 'hello');
    n.pkt(A, { t: 'msg', id: 'fut1g1', g: G1, text: 'hello', ts, sg, pk: K.A.pk }); await wait(20); await n.settle(10);
    const m = n.st.messages.find(x => x.peer === 'g:' + G1 && x.id === 'fut1');
    ok(m && !m.sg, 'clamped post stored without sg');
    ok(n.st.groups[G1].keys && n.st.groups[G1].keys[A] === K.A.pk, 'sender still known as a signer');
    const ts2 = now() - 10, sg2 = postSig(K.A, G1, 'ok1', ts2, 'fine');
    n.pkt(A, { t: 'msg', id: 'ok1g1', g: G1, text: 'fine', ts: ts2, sg: sg2, pk: K.A.pk }); await wait(20); await n.settle(10);
    ok((n.st.messages.find(x => x.id === 'ok1') || {}).sg === sg2, 'normal signed post keeps sg');
  }
  { // 8. Nostr: a relay streaming junk is capped; malformed / replayed / oversized / wrong-type boxes
    let opened = 0;
    class WS { constructor() { setTimeout(() => this.onopen && this.onopen(), 0); }
      send(r) { const q = JSON.parse(r); if (q[0] !== 'REQ') return; setTimeout(() => { for (let i = 0; i < 5000; i++) this.onmessage({ data: JSON.stringify(['EVENT', q[1], { id: 'e' + i }]) }); this.onmessage({ data: 'null' }); this.onmessage({ data: JSON.stringify(['EOSE', q[1]]) }); }, 0); }
      close() {} }
    const n = await node({ nostrOn: true, contacts: { [A]: { addr: A, nick: 'alice', shared: true, hasprof: true, pk: K.A.pk } } }, { WebSocket: WS, nostrOpen: () => { opened++; return { error: 'x' }; } });
    await n.T.nostrFetch();
    ok(opened > 0 && opened <= 2000, 'relay events capped at 2000, opened ' + opened);
    const boxes = { b1: 'not json', b2: 'null', b3: '{"t":"msg","id":"nb3","text":"hi","ts":"1"}', b4: '{"t":"gi","g":"' + G1 + '","gm":["' + A + '"]}', b5: JSON.stringify({ t: 'msg', id: 'nb5', text: 'x'.repeat(17000), ts: now() }), b6: JSON.stringify({ t: 'msg', id: 'nb6', text: 'from the future', ts: now() + 86400 * 365 }), b7: JSON.stringify({ t: 'msg', id: 'nb6', text: 'replay', ts: now() }) };
    class WS2 { constructor() { setTimeout(() => this.onopen && this.onopen(), 0); }
      send(r) { const q = JSON.parse(r); if (q[0] !== 'REQ') return; setTimeout(() => { for (const id of Object.keys(boxes)) this.onmessage({ data: JSON.stringify(['EVENT', q[1], { id, kind: 1059 }]) }); this.onmessage({ data: JSON.stringify(['EOSE', q[1]]) }); }, 0); }
      close() {} }
    const n2 = await node({ nostrOn: true, contacts: { [A]: { addr: A, nick: 'alice', shared: true, hasprof: true, pk: K.A.pk } } }, { WebSocket: WS2, nostrOpen: ev => { const e = JSON.parse(ev); return { from: A, pk: K.A.pk, w: boxes[e.id], del: '' }; } });
    await n2.T.nostrFetch(); await n2.settle(10);
    ok(!n2.logged.length, 'no errors on malformed boxes: ' + n2.logged.map(String).join(' ').slice(0, 200));
    const ids = n2.st.messages.map(m => m.id);
    ok(!ids.includes('nb3') && !ids.includes('nb5'), 'wrong-type and oversized boxes dropped');
    const f = n2.st.messages.filter(m => m.id === 'nb6');
    ok(f.length === 1 && f[0].ts <= now() + 600, 'future box clamped, replay stored once');
    ok(!Object.keys(n2.st.groups).some(g => g !== G1) && n2.st.groups[G1].members.length === 3, 'non-msg box ignored');
    await n2.T.nostrFetch(); await n2.settle(10);
    ok(n2.st.messages.filter(m => m.id === 'nb6').length === 1, 'second fetch: no duplicate');
  }
  { // 9. a msg without an id was stored with id undefined (String(undefined) passed the id check)
    const n = await node();
    n.pkt(A, { t: 'msg', text: 'no id', ts: now() }); n.pkt(A, { t: 'msg', id: '', text: 'empty id', ts: now() });
    await wait(20); await n.settle(10);
    ok(!n.st.messages.some(m => m.text === 'no id' || m.text === 'empty id'), 'msg without id dropped');
    ok(!n.sent.some(x => x.w.t === 'ack' && !x.w.id), 'no ack without id');
  }

  { // 10. v0.19.4: unsent posts parked on Nostr are recalled (NIP-09 by the box key), the unsend is parked too,
    // and a receiver reading the boxes never shows the post, also from a relay that ignores deletions.
    for (const ignoreDel of [false, true]) {
      const relay = new Map(); let dels = 0;
      class RWS { constructor() { setTimeout(() => this.onopen && this.onopen(), 0); }
        send(r) { const q = JSON.parse(r);
          if (q[0] === 'EVENT') { const e = q[1]; if (e.kind === 5) { dels++; if (!ignoreDel) for (const t of e.tags) if (t[0] === 'e' && relay.has(t[1]) && relay.get(t[1]).pubkey === e.pubkey) relay.delete(t[1]); } else relay.set(e.id, e);
            setTimeout(() => this.onmessage({ data: JSON.stringify(['OK', e.id, true, '']) }), 0); }
          if (q[0] === 'REQ') setTimeout(() => { for (const e of relay.values()) this.onmessage({ data: JSON.stringify(['EVENT', q[1], e]) }); this.onmessage({ data: JSON.stringify(['EOSE', q[1]]) }); }, 0); }
        close() {} }
      const kpOf = a => Object.values(K).find(k => k.addr === a);
      const fakeW = me => ({
        yggNostrWrapK: (pk, pkt) => { const nk = crypto.randomBytes(32).toString('hex'), id = crypto.randomBytes(32).toString('hex');
          return JSON.stringify({ ev: JSON.stringify({ id, pubkey: 'P' + nk, kind: 1059, tags: [['p', 't1']], content: JSON.stringify({ from: me, pk: kpOf(me).pk, w: pkt }) }), id, nk }); },
        yggNostrDel: (nk, id) => JSON.stringify({ id: sha(nk + id), pubkey: 'P' + nk, kind: 5, tags: [['e', id]] }),
        yggNostrOpen: ev => { const c = JSON.parse(JSON.parse(ev).content); return { from: c.from, pk: c.pk, w: c.w, del: '' }; },
      });
      const ME = K.ME.addr;
      const n = await node({ nostrOn: true, contacts: { [A]: { addr: A, nick: 'alice', shared: true, hasprof: true, pk: K.A.pk } } }, { WebSocket: RWS, wasm: fakeW });
      const m = { id: '18f00000000000b1', peer: A, from: ME, nick: 'me', text: 'secret', ts: now(), out: true, status: 'queued' };
      n.st.messages.push(m);
      await n.T.nostrPark(m); await n.settle(10);
      const mm = n.st.messages.find(x => x.id === m.id);
      ok(relay.size === 1 && mm && mm.nostr && mm.nev && mm.nev.length === 1, 'pwa: parked box recorded ' + ignoreDel);
      const r = await n.api('message/delete', { Peer: A, ID: m.id, Everyone: true });
      await wait(30); await n.settle(10);
      ok(r.status === 200 && dels >= 1 && (ignoreDel || relay.size === 0), 'pwa: box recalled on unsend ' + ignoreDel);
      const c = n.st.ctl.find(x => x.peer === A && x.w.t === 'del');
      ok(c && c.park, 'pwa: unsend marked to park');
      await n.T.parkCtl(A); await n.settle(5);
      ok(c.nev && c.nev.length === 1, 'pwa: unsend parked');
      // a group post: the parked copy for A is recalled, the unsend parked for A only
      const vis = await n.api('send', { To: 'g:' + G1, Text: 'group oops' });
      await wait(20);
      const cp = n.st.messages.find(x => x.hid && x.peer === A && x.of === vis.json.id);
      if (cp) { cp.status = 'queued'; await n.T.nostrPark(cp); }
      const before = relay.size;
      await n.api('message/delete', { Peer: 'g:' + G1, ID: vis.json.id, Everyone: true }); await wait(30); await n.settle(10);
      ok(cp && cp.nev && (ignoreDel || relay.size === before - 1), 'pwa: group copy recalled ' + ignoreDel);
      const gc = n.st.ctl.filter(x => x.w.g === G1);
      ok(gc.length === 2 && gc.every(x => !!x.park === (x.peer === A)), 'pwa: group unsend parked for the parked member only');
      await n.T.parkCtl(A); await n.settle(5);
      // the receiver (A) fetches: post never shows, tombstones set, unsend acked
      const ra = makeNode({ kp: K.A, store: { key: K.A.priv, peers: ['wss://x.test/'], nick: 'a', nostrOn: true, contacts: { [ME]: { addr: ME, nick: 'me', shared: true, hasprof: true, pk: K.ME.pk } }, messages: [], groups: { [G1]: { id: G1, name: 'g1', members: [ME, A, B].sort(), owner: ME, v: 1, sv: 1, set: {} } }, blocked: {} }, WebSocket: RWS, wasm: fakeW });
      await ra.ready();
      await ra.T.nostrFetch(); await ra.settle(20); await wait(30);
      ok(!ra.st.messages.some(x => x.id === m.id), 'pwa receiver: unsent 1:1 not shown ' + ignoreDel);
      ok(!!ra.st.gone[ME + '/' + m.id], 'pwa receiver: 1:1 unsend from box applied');
      ok(!ra.st.messages.some(x => x.peer === 'g:' + G1 && x.text === 'group oops'), 'pwa receiver: unsent group post not shown ' + ignoreDel);
      ok(ra.sent.some(x => x.peer === ME && x.w.t === 'ack' && x.w.id === c.w.id), 'pwa receiver: parked unsend acked');
      // the author hears the ack: queue drains, the parked unsend is recalled
      n.pkt(A, { t: 'ack', id: c.w.id }); await wait(30); await n.settle(10);
      ok(!n.st.ctl.some(x => x.w.id === c.w.id) && (ignoreDel || ![...relay.values()].some(e => e.content.includes(c.w.id))), 'pwa: acked unsend drained and recalled');
      ok(!(n.st.nrecall || []).length, 'pwa: recall queue empty');
    }
    // the 90-day hash list outlives the 10-day tombstone (B is a co-admin, so its history relay is taken)
    const n = await node({ groups: { [G1]: { id: G1, name: 'g1', members: [K.ME.addr, A, B].sort(), owner: K.ME.addr, admins: [B], v: 1, sv: 1, set: {} } } });
    const pid = '18f00000000000c1', ts = now() - 100, text = 'old';
    n.pkt(A, { t: 'msg', id: pid + 'g1', g: G1, text, ts, sg: postSig(K.A, G1, pid, ts, text), pk: K.A.pk }); await wait(20); await n.settle(10);
    const sg = Buffer.from(Y.signHex(K.A, 'ygg-gdel1\n' + G1 + '\n' + pid + '\n' + A + '\n'), 'hex').toString('base64').replace(/=+$/, '');
    n.pkt(A, { t: 'del', id: 'cdel1', g: G1, ref: pid, by: A, pk: K.A.pk, sg, ts: now() }); await wait(20); await n.settle(10);
    ok(!n.st.messages.some(x => x.text === text) && Object.keys(n.st.gdelh || {}).length === 1, 'pwa: unsend leaves a hash');
    const pid2 = '18f00000000000c2';
    n.pkt(B, { t: 'ghs', g: G1, id: pid2, by: A, text: 'control', ts, sg: postSig(K.A, G1, pid2, ts, 'control'), pk: K.A.pk }); await wait(20); await n.settle(10);
    ok(n.st.messages.some(x => x.text === 'control'), 'pwa: control relay is taken');
    for (const k in n.st.gdel) n.st.gdel[k].x = now() - 1;
    n.pkt(B, { t: 'ghs', g: G1, id: pid, by: A, text, ts, sg: postSig(K.A, G1, pid, ts, text), pk: K.A.pk }); await wait(20); await n.settle(10);
    ok(!n.st.messages.some(x => x.text === text), 'pwa: relay after the tombstone expired does not resurrect');
  }
  clearInterval(keep);
  console.log(passes + ' passed, ' + fails + ' failed');
  process.exit(fails ? 1 : 0);
})().catch(e => { console.log('CRASH', e); process.exit(2); });
