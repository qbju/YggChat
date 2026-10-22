// Random + structured hostile packets against the shim's handlers.
// node test/pwa/fuzz.js [iterations] [seed]
const { makeNode } = require('./harness');
const F = require('./fixtures');
const ITER = +process.argv[2] || 3000; let seed = +process.argv[3] || 1;
const rnd = () => { seed = (seed * 1103515245 + 12345) & 0x7fffffff; return seed / 0x7fffffff; };
const pick = a => a[Math.floor(rnd() * a.length)];
const rejections = []; process.on('unhandledRejection', e => rejections.push(e));
const TYPES = ['fq', 'fr', 'mh', 'mc', 'mha', 'mca', 'mok', 'mng', 'fca', 'ack', 'pow', 'hi', 'hiack', 'prof', 'ttl', 'fh', 'fc', 'edit', 'del', 'gq', 'gi', 'gl', 'gj', 'gjn', 'ghs', 'ghq', 'gd', 'gp', 'gk', 'gkq', 'msg', 'nq', 'zz'];
const STRF = ['t', 'id', 'nick', 'text', 'bio', 'av', 'dom', 'pk', 'mime', 'sha', 'd', 'ref', 'g', 'gn', 'go', 'gh', 'tok', 'pw', 'k', 'by', 'gmb', 'ga', 'gr', 'pn', 'sg'];
const INTF = ['ts', 'ttl', 'size', 'n', 'i', 'gv', 'gsv', 'gc'];
const big = n => 'x'.repeat(n);
const ADDRS = [F.ME, F.A, F.B, F.C, F.S, '200::', '3ff:ffff::1', '::1', 'fe80::1', '201:aaaa:aaaa:aaaa:aaaa:aaaa:aaaa:aaab'];
const HOSTILE_S = ['', ' ', '__proto__', 'constructor', 'prototype', 'toString', 'hasOwnProperty', 'valueOf', 'g:' + F.G1, F.G1, F.G2, '0000000000000000', 'm1', 'm2', 'p1', 'p1g0', 'g', 'gg', big(2000), big(70000), '\u202e\u0000\ud800', 'image/png', 'text/plain', 'a'.repeat(64), 'GOOD' + 'A'.repeat(82), 'A'.repeat(86), 'kick', 'pw', 'wait', 'none', ...ADDRS, '{"t":"msg"}', 'AAAA', '!!!'];
const HOSTILE_N = [0, -1, 1, 2, 7, 31, 64, 999, 1e6, 2 ** 31, -(2 ** 31), 2 ** 53 - 1, -(2 ** 53 - 1), F.now(), F.now() + 86400 * 365, F.now() - 10];
const val = k => STRF.includes(k) ? pick(HOSTILE_S) : INTF.includes(k) ? pick(HOSTILE_N) : null;
function packet(typed) {
  const w = { t: pick(TYPES) };
  const nk = 1 + Math.floor(rnd() * 8);
  for (let i = 0; i < nk; i++) { const k = pick(STRF.concat(INTF, ['gm', 'gad', 'gs', 'gt', '__proto__', 'constructor', '_checked'])); w[k] = val(k); }
  if (rnd() < 0.7) w.g = pick([F.G1, F.G2, F.G1, '__proto__', 'constructor', 'ffffffffffffffff']);
  if (rnd() < 0.5) w.id = pick(['m1', 'p1', 'p1g1', 'zz' + Math.floor(rnd() * 99), '__proto__', 'constructor']);
  if (rnd() < 0.4) w.gm = Array.from({ length: Math.floor(rnd() * 6) }, () => pick(ADDRS.concat(['__proto__', ''])));
  if (rnd() < 0.1) w.gm = Array.from({ length: 3000 }, (_, i) => '205::' + i.toString(16));
  if (rnd() < 0.2) w.gad = Array.from({ length: Math.floor(rnd() * 4) }, () => pick(ADDRS));
  if (rnd() < 0.2) w.gs = { nf: rnd() < .5, ap: rnd() < .5, hi: rnd() < .5, pw: rnd() < .5, ttl: pick(HOSTILE_N), mx: pick(HOSTILE_N), pb: pick(HOSTILE_N) };
  if (rnd() < 0.1) w.gt = Object.fromEntries(ADDRS.map(a => [a, pick(HOSTILE_N)]));
  if (!typed) { // wrong types too
    for (const k of Object.keys(w)) if (rnd() < 0.2) w[k] = pick([null, true, {}, [], [[[[[]]]]], { __proto__: { x: 1 } }, 1.5, '1', -0, [1, 2], { toString: 1 }, { valueOf: {}, toString: {} }, [{ toString: 1 }]]);
  }
  let s = JSON.stringify(w);
  if (rnd() < 0.05) s = s.replace('"t"', '"__proto__":{"polluted":1},"t"');
  if (rnd() < 0.02) s = pick(['null', '[]', '1', '"x"', 'true', '{', '[' .repeat(5000) + ']'.repeat(5000), '{"t":"msg","id":"a","text":"b","constructor":{"prototype":{"polluted":1}}}']);
  return s;
}
function check(n, label) {
  const st = n.st, probs = [];
  if (n.run('({}).polluted') !== undefined || ({}).polluted !== undefined) probs.push('prototype polluted');
  let js; try { js = JSON.stringify(st); } catch (e) { probs.push('state not serialisable ' + e.message); }
  if (!Array.isArray(st.messages)) probs.push('messages not array');
  else for (const m of st.messages) { if (typeof m.id !== 'string' || typeof m.peer !== 'string') { probs.push('bad message ' + JSON.stringify(m).slice(0, 200)); break; } if (m.text != null && typeof m.text !== 'string') { probs.push('non-string text'); break; } }
  for (const [k, c] of Object.entries(st.contacts)) if (!/^[23][0-9a-f]{0,2}:[0-9a-f:]*$/.test(k) || !c || typeof c !== 'object') probs.push('bad contact key ' + k.slice(0, 50));
  for (const [k, g] of Object.entries(st.groups)) {
    if (!/^[0-9a-f]{16}$/.test(k)) probs.push('bad group key ' + k.slice(0, 40));
    if (!g || !Array.isArray(g.members) || g.members.some(x => typeof x !== 'string')) probs.push('bad members ' + k);
    if (g && g.admins && (!Array.isArray(g.admins))) probs.push('bad admins');
    if (g && typeof g.name !== 'string') probs.push('bad name ' + k);
  }
  { const okP = o => { const p = Object.getPrototypeOf(o); return p === Object.prototype || p === n.run('Object.prototype'); }; if (!okP(st.groups) || !okP(st.contacts) || Object.values(st.groups).some(g => g && !okP(g))) probs.push('store map prototype changed'); }
  if (js && js.length > 3e6) probs.push('state huge ' + js.length);
  const pk = n.logged.filter(a => a[0] === 'packet' || a[0] === 'msg');
  if (pk.length) probs.push('handler threw: ' + pk.map(a => String(a[1] && a[1].stack || a[1]).split('\n').slice(0, 3).join(' | ')).slice(0, 3).join(' || '));
  if (n.errors.length) probs.push('sync throw (kills wasm): ' + n.errors.map(e => String(e && e.stack || e).split('\n').slice(0, 2).join(' | ')).slice(0, 2).join(' || '));
  n.logged.length = 0; n.errors.length = 0;
  return probs;
}
(async () => { try {
  const found = new Map(); const keep = setInterval(() => {}, 1000);
  let n = null, cnt = 0, lastPkts = [];
  for (let it = 0; it < ITER; it++) {
    if (!n || cnt >= 150) { n = makeNode({ addr: F.ME, store: F.store() }); await n.ready(); cnt = 0; lastPkts = []; }
    const typed = rnd() < 0.75, s = packet(typed), src = rnd() < 0.15 ? '2' + Math.floor(rnd() * 255).toString(16).padStart(2, '0') + ':' + Math.floor(rnd() * 65535).toString(16) + '::' + it.toString(16) : pick([F.A, F.B, F.C, F.S, F.A, F.B]);
    lastPkts.push(src + ' ' + s.slice(0, 300)); if (lastPkts.length > 4) lastPkts.shift();
    if (process.env.V) console.log(it, src, s.slice(0, 200)); const t0 = Date.now(); n.pkt(src, s); cnt++; if (Date.now() - t0 > 300) console.log('SLOW', Date.now() - t0, 'ms', s.slice(0, 300));
    if (it % 5 === 0) await n.settle(3);
    const probs = check(n);
    if (rejections.length) probs.push('unhandled rejection: ' + rejections.splice(0).map(e => String(e && e.stack || e).split('\n').slice(0, 3).join(' | ')).join(' || '));
    for (const p of probs) { const key = p.slice(0, 160); if (!found.has(key)) { found.set(key, lastPkts.slice()); console.log('PROBLEM', p.slice(0, 600), '\n  after:', lastPkts.slice(-2).join('\n         ').slice(0, 900)); } }
    if (probs.length) n = null; // fresh node after a problem
  }
  console.log('done', ITER, 'problems', found.size);
  process.exit(found.size ? 1 : 0); } catch (e) { console.log('FUZZER CRASH', e); process.exit(2); }
})();
