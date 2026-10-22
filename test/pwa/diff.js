// Differential test: the same packet sequences through the native handlePacket
// (TestShimDiff in shimdiff_test.go) and through the shim; resulting state compared.
//   node test/pwa/diff.js gen N SEED > /tmp/sc.json
//   SHIMDIFF_IN=/tmp/sc.json SHIMDIFF_OUT=/tmp/go.json go test -run TestShimDiff .
//   node test/pwa/diff.js cmp /tmp/sc.json /tmp/go.json
const fs = require('fs');
const Y = require('./ygg');
const { makeNode } = require('./harness');
const now = () => Math.floor(Date.now() / 1000);
let seed = 1; const rnd = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
const pick = a => a[Math.floor(rnd() * a.length)];
const sha256hex = b => require('crypto').createHash('sha256').update(b).digest('hex');

// Fixed keys (seeded) so both sides see the same addresses.
function actors() { const k = {}; for (const n of ['ME', 'A', 'B', 'C', 'S']) k[n] = Y.keypair(n); return k; }
const K = actors();
const G1 = '00112233aabbccdd', G2 = '1122334455667788', G3 = 'aaaaaaaaaaaaaaaa';
const postSig = (kp, gid, pid, ts, text) => Buffer.from(Y.signHex(kp, 'ygg-gpost1\n' + gid + '\n' + pid + '\n' + ts + '\n' + sha256hex(Buffer.from(text))), 'hex').toString('base64').replace(/=+$/, '');
const gh = (name, ms) => sha256hex(Buffer.from(name + '\n' + ms.slice().sort().join(','))).slice(0, 8);
const packAddrs = as => Buffer.concat(as.map(a => { const u = new URL('http://[' + a + ']/'); const parts = u.hostname.slice(1, -1); const full = parts.split(':'); const i = full.indexOf(''); const p = i < 0 ? full : full.slice(0, i).concat(Array(8 - full.length + 1).fill('0'), full.slice(i + 1)); const b = Buffer.alloc(16); p.forEach((x, k) => b.writeUInt16BE(parseInt(x || '0', 16), k * 2)); return b; })).toString('base64').replace(/=+$/, '');

function store(t) {
  const ME = K.ME.addr, A = K.A.addr, B = K.B.addr, C = K.C.addr;
  return {
    nick: 'me', contacts: { [A]: { addr: A, nick: 'alice', shared: true, hasprof: true, pk: K.A.pk } },
    messages: [
      { id: 'm1', peer: A, from: A, nick: 'alice', text: 'hello', ts: t - 100, out: false },
      { id: 'm2', peer: A, from: ME, nick: 'me', text: 'yo', ts: t - 90, out: true, status: 'sent' },
      { id: 'm3', peer: A, from: ME, nick: 'me', text: 'later', ts: t - 80, out: true, status: 'queued' },
      { id: 'p1', peer: 'g:' + G1, from: B, nick: 'bob', text: 'group hi', ts: t - 80, out: false },
    ],
    groups: {
      [G1]: { id: G1, name: 'g1', members: [ME, A, B, C].sort(), owner: ME, admins: [C], v: 3, sv: 2, set: { hi: true }, tok: 'abcdefghijk' },
      [G2]: { id: G2, name: 'g2', members: [ME, A, B].sort(), owner: A, v: 5, sv: 1, set: {} },
    },
  };
}
function genPkt(t) {
  const ME = K.ME.addr, A = K.A.addr, B = K.B.addr, C = K.C.addr, S = K.S.addr;
  const fromN = pick(['A', 'A', 'B', 'C', 'S']), from = K[fromN].addr;
  const ids = ['m1', 'm2', 'm3', 'x1', 'x2', 'p1', 'q1', 'q1g2'];
  const texts = ['hi', 'hi', ' ', '', 'edited!', '\u202eevil', 'x'.repeat(17000), '日本語'];
  const tss = [t, t, t - 50, t + 100, t + 5000, 0, -5, t - 86400 * 20];
  const gid = pick([G1, G1, G2, G2, G3]);
  const ms1 = [ME, A, B, C].sort(), ms2 = [ME, A, B].sort();
  const T = pick(['msg', 'msg', 'msg', 'edit', 'del', 'prof', 'ttl', 'hi', 'ack', 'gmsg', 'gmsg', 'gmsg', 'ghs', 'ghq', 'gi', 'gi', 'gl', 'gd', 'gq', 'gkq', 'gjn', 'pow', 'fh']);
  let w;
  switch (T) {
    case 'msg': w = { t: 'msg', id: pick(ids), nick: pick(['n', '', '\u0000bad']), text: pick(texts), ts: pick(tss), ttl: pick([0, 0, 300, -1, 3600 * 24 * 365]) }; break;
    case 'edit': w = { t: 'edit', id: 'c' + Math.floor(rnd() * 5), ref: pick(ids), text: pick(texts) }; break;
    case 'del': w = { t: 'del', id: 'c' + Math.floor(rnd() * 5), ref: pick(ids) }; break;
    case 'prof': w = { t: 'prof', nick: pick(['Al', '', ' x ', 'a'.repeat(50)]), bio: pick(['', 'bio', 'b'.repeat(300)]), av: pick(['', '😀', 'abcdefghijk']) }; break;
    case 'ttl': w = { t: 'ttl', ttl: pick([0, 300, -5, 99999999]) }; break;
    case 'hi': w = { t: pick(['hi', 'hiack']), pk: pick([K[fromN].pk, K[fromN].pk.toUpperCase(), K.S.pk, '']) }; break;
    case 'ack': w = { t: 'ack', id: pick(ids) }; break;
    case 'pow': w = { t: 'pow', id: pick(ids), n: pick([1, 10, 99]) }; break;
    case 'gmsg': {
      const pid = pick(['q1', 'q2', 'p1']), id = pid + pick(['', 'g1', 'g3']), ts = pick(tss), text = pick(texts.slice(0, 6));
      w = { t: 'msg', id, g: gid, nick: 'gn', text, ts, gh: pick([gh(gid === G1 ? 'g1' : 'g2', gid === G1 ? ms1 : ms2), 'deadbeef', undefined]) };
      if (rnd() < 0.4 && text) { w.sg = postSig(K[fromN], gid, pid, ts, text); w.pk = K[fromN].pk; }
      if (rnd() < 0.1) { w.sg = postSig(K.S, gid, pid, ts, text || 'x'); w.pk = K[fromN].pk; }
      break;
    }
    case 'ghs': {
      const byN = pick(['A', 'B', 'C', 'S', 'ME']), pid = pick(['h1', 'h2', 'p1']), ts = pick(tss), text = pick(texts.slice(0, 6));
      w = { t: 'ghs', g: gid, id: pid, by: K[byN].addr, nick: 'hn', text, ts, ttl: pick([undefined, 300, -1]) };
      if (rnd() < 0.5 && text) { w.sg = postSig(K[byN], gid, pid, ts, text); w.pk = K[byN].pk; }
      break;
    }
    case 'ghq': w = { t: 'ghq', g: gid, ts: pick(tss) }; break;
    case 'gi': {
      const base = gid === G1 ? ms1 : ms2;
      const ms = pick([base, base.filter(x => x !== B), base.concat([S]), [from, ME], base.filter(x => x !== ME), [from]]);
      w = { t: 'gi', g: gid, gn: pick(['g1', 'g2', 'renamed', '']), gm: ms, go: pick([A, ME, from, '', C]), gv: pick([0, 3, 4, 5, 6, 200]), gsv: pick([0, 1, 2, 3]), gad: pick([undefined, [C], [B, C]]), ts: pick([t, 0]), gs: pick([undefined, { hi: true }, { nf: true, ttl: 300 }, { mx: 64, pb: -1 }]) };
      if (rnd() < 0.2) { w.gr = packAddrs([B]); w.k = 'kick'; }
      break;
    }
    case 'gl': w = { t: 'gl', g: gid, ts: pick([t, 0, t - 10]) }; break;
    case 'gd': w = { t: 'gd', g: gid, gv: pick([3, 4, 5, 6, 100]), ga: pick([undefined, packAddrs([S])]), gr: pick([undefined, packAddrs([B]), packAddrs([ME])]), ts: pick([t, 0]), k: pick([undefined, 'kick']) }; break;
    case 'gq': w = { t: 'gq', g: gid }; break;
    case 'gkq': w = { t: 'gkq', g: gid }; break;
    case 'gjn': w = { t: 'gjn', g: gid, text: 'no', k: pick(['pw', 'wait', '']) }; break;
    case 'fh': { // a one-chunk long text: header + chunk
      const body = Buffer.from(pick(['long text body', 'x'.repeat(800)]));
      const id = pick(['f1', 'f2', 'm1']);
      const h = { t: 'fh', id, mime: 'text/plain', size: body.length, n: 1, sha: sha256hex(body), nick: 'f', ts: t };
      if (rnd() < 0.3) { h.g = gid; }
      return [[from, JSON.stringify(h)], [from, JSON.stringify({ t: 'fc', id, i: 0, d: body.toString('base64') })]];
    }
  }
  for (const k of Object.keys(w)) if (w[k] === undefined) delete w[k];
  if (rnd() < 0.08) delete w[pick(Object.keys(w).filter(k => k !== 't').concat(['id']))];
  if (rnd() < 0.12) { const k = pick(Object.keys(w).concat(['ts', 'gm', 'gs'])); w[k] = pick(['5', 5, 1.5, true, null, {}, [], ['x'], { nf: 'yes' }, k === 'ts' ? 2 ** 60 : 2 ** 31 - 1]); }
  let s = JSON.stringify(w);
  if (rnd() < 0.05) s = s.replace('{', pick(['{"__proto__":{"t":"msg"},', '{"_checked":true,', '{"constructor":{"prototype":{"x":1}},', '{"T":"zz",']));
  return [[from, s]];
}
function gen(n, s) {
  seed = s; const t = now(), out = [];
  for (let i = 0; i < n; i++) {
    const pk = []; const L = 1 + Math.floor(rnd() * 8);
    for (let j = 0; j < L; j++) for (const [from, json] of genPkt(t)) pk.push({ from, json });
    out.push({ name: 'sc' + i, me: K.ME.addr, store: store(t), pkts: pk });
  }
  return out;
}

// ---- normalise and compare ----
const B = x => !!x;
function norm(st) {
  const o = { contacts: {}, messages: [], groups: {}, gone: Object.keys(st.gone || {}).sort(), early: Object.keys(st.early || {}).sort() };
  for (const [k, c] of Object.entries(st.contacts || {})) o.contacts[k] = { nick: c.nick || '', req: B(c.req), shared: B(c.shared), ttl: c.ttl | 0, bio: c.bio || '', avatar: c.avatar || '', hasprof: B(c.hasprof), pk: c.pk || '' };
  for (const m of st.messages || []) { if (m.hid) continue; o.messages.push([m.peer, m.sys ? 'sys' : m.id, m.from || '', m.text || '', B(m.out), B(m.edited), m.via || '', B(m.hist), m.sys || '', (m.ttl | 0) > 0, B(m.sg), m.out ? (m.status || '') : '', m.nick || ''].join(' ¦ ')); }
  for (const [k, g] of Object.entries(st.groups || {})) o.groups[k] = { name: g.name, members: (g.members || []).slice().sort(), owner: g.owner || '', admins: (g.admins || []).slice().sort(), v: g.v || 0, sv: g.sv || 0, left: B(g.left), req: B(g.req), stub: B(g.stub), quit: B(g.quit), set: JSON.stringify(Object.fromEntries(Object.entries(g.set || {}).filter(([, v]) => v).sort())), ban: (g.ban || []).slice().sort(), keys: Object.keys(g.keys || {}).sort(), gone: (g.gone || []).slice().sort() };
  return o;
}
const name = a => { for (const [n, x] of Object.entries(K)) if (x.addr === a) return n; return a; };
const pretty = s => String(s).replace(/[0-9a-f]{1,4}(:[0-9a-f]{0,4}){3,7}/g, m => { const n = name(m); return n === m ? m : n; });
function cmpVal(path, a, b, out) {
  if (JSON.stringify(a) === JSON.stringify(b)) return;
  if (Array.isArray(a) && Array.isArray(b) && path.endsWith('messages') && JSON.stringify(a.slice().sort()) === JSON.stringify(b.slice().sort())) return;
  if (Array.isArray(a) && Array.isArray(b) && path.endsWith('messages')) {
    const sa = new Set(a), sb = new Set(b);
    for (const x of a) if (!sb.has(x)) out.push(path + ' only native: ' + pretty(x).slice(0, 220));
    for (const x of b) if (!sa.has(x)) out.push(path + ' only shim:   ' + pretty(x).slice(0, 220));
    // order alone is not compared: sys lines take the clock of each run
    return;
  }
  if (a && b && typeof a === 'object' && typeof b === 'object' && !Array.isArray(a)) { for (const k of new Set([...Object.keys(a), ...Object.keys(b)])) cmpVal(path + '.' + pretty(k), a[k], b[k], out); return; }
  out.push(path + ': native=' + pretty(JSON.stringify(a)).slice(0, 200) + ' shim=' + pretty(JSON.stringify(b)).slice(0, 200));
}
async function cmp(scFile, goFile) {
  const scs = JSON.parse(fs.readFileSync(scFile)), gos = JSON.parse(fs.readFileSync(goFile));
  const keep = setInterval(() => {}, 1000);
  const kinds = new Map(); let bad = 0;
  for (let i = 0; i < scs.length; i++) {
    const sc = scs[i], go = gos[i];
    const n = makeNode({ kp: K.ME, store: Object.assign({ key: K.ME.priv, peers: ['wss://x.test/'], nostrOn: false }, sc.store) });
    await n.ready();
    for (const p of sc.pkts) { n.pkt(p.from, p.json); await n.settle(4); await new Promise(r => setTimeout(r, 4)); await n.settle(4); }
    await new Promise(r => setTimeout(r, 30)); await n.settle(10);
    const a = norm(go.store), b = norm(n.st), d = [];
    for (const k of ['contacts', 'messages', 'groups', 'gone', 'early']) cmpVal(k, a[k], b[k], d);
    const ackSet = l => new Set(l.filter(x => ['ack', 'pow', 'gjn'].includes(x[1])).map(x => pretty(x[0]) + ' ' + x[1] + ' ' + x[2]));
    const ga = ackSet(go.sent || []), sa = ackSet(n.sent.map(x => [x.peer, x.w.t, x.w.id || '']));
    for (const x of ga) if (!sa.has(x)) d.push('sent only native: ' + x);
    for (const x of sa) if (!ga.has(x)) d.push('sent only shim: ' + x);
    if (d.length) {
      bad++;
      const key = d.map(x => x.replace(/: native=.*/, '')).join(' / ').slice(0, 200);
      if (!kinds.has(key)) { kinds.set(key, 1); console.log('\n=== ' + sc.name + '\n  pkts: ' + sc.pkts.map(p => name(p.from) + ' ' + pretty(p.json).slice(0, 260)).join('\n        ') + '\n  diff: ' + d.join('\n        ')); }
      else kinds.set(key, kinds.get(key) + 1);
    }
  }
  clearInterval(keep);
  console.log('\nscenarios', scs.length, 'differing', bad, 'kinds', kinds.size);
  process.exit(bad ? 1 : 0);
}
if (process.argv[2] === 'gen') process.stdout.write(JSON.stringify(gen(+process.argv[3] || 50, +process.argv[4] || 1)));
else if (process.argv[2] === 'cmp') cmp(process.argv[3], process.argv[4]);
