// /api parity: the same requests to the native handlers (TestShimAPIDiff) and to the shim.
//   node test/pwa/apidiff.js gen > /tmp/api.json
//   SHIMAPI_IN=/tmp/api.json SHIMAPI_OUT=/tmp/goapi.json go test -run TestShimAPIDiff .
//   node test/pwa/apidiff.js cmp /tmp/api.json /tmp/goapi.json
const fs = require('fs');
const { K, store, G1, G2 } = require('./diff');
const { makeNode } = require('./harness');
const now = () => Math.floor(Date.now() / 1000);
const A = K.A.addr, B = K.B.addr, S = K.S.addr;
function scenarios() {
  const t = now(), st = () => store(t);
  const R = (ep, body) => ({ ep, body: body === undefined ? null : body });
  return [
    { name: 'state', reqs: [R('state'), R('pow'), R('nostr'), R('finger'), R('lan')] },
    { name: 'send', reqs: [R('send', { To: A, Text: 'hi' }), R('send', { To: A, Text: '' }), R('send', { To: 'nope', Text: 'x' }), R('send', { To: A, Text: 'x'.repeat(13000) }), R('send', { To: 'g:' + G1, Text: 'group' }), R('send', { To: 'g:ffffffffffffffff', Text: 'x' }), R('send', { To: A, Img: 'data:image/png;base64,AAAA' }), R('send', { To: A, Retry: 'm3' }), R('send', { To: A, Retry: 'nope' })] },
    { name: 'contact', reqs: [R('contact', { Addr: S, Nick: 'sam' }), R('contact', { Addr: A, Nick: 'al', TTL: 300 }), R('contact', { Addr: A, TTL: 99999999 }), R('contact', { Addr: 'bad' }), R('contact', { Addr: A, Accept: true }), R('contact/delete', { Addr: S }), R('contact/delete', { Addr: 'g:' + G2 }), R('contact/delete', { Addr: 'zzz' }), R('block', { Addr: B, Block: true }), R('block', { Addr: B, Block: false }), R('block', { Addr: 'x' })] },
    { name: 'msg', reqs: [R('message/edit', { Peer: A, ID: 'm2', Text: 'edited' }), R('message/edit', { Peer: A, ID: 'm1', Text: 'not mine' }), R('message/edit', { Peer: A, ID: 'nope', Text: 'x' }), R('message/edit', { Peer: A, ID: 'm2', Text: '' }), R('message/delete', { Peer: A, ID: 'm1', Everyone: true }), R('message/delete', { Peer: A, ID: 'm1' }), R('message/delete', { Peer: A, ID: 'm2', Everyone: true }), R('message/delete', { Peer: A, ID: 'nope' })] },
    { name: 'profile', reqs: [R('profile', { Nick: 'new', Bio: 'b', Avatar: '😀' }), R('profile', { Nick: 'x'.repeat(100) }), R('nick', { Nick: 'n2' }), R('pow', { Strict: true }), R('nostr', { On: false }), R('nostr', { Relays: ['wss://r.test', 'http://bad'] }), R('finger', { On: true, Plan: 'plan' })] },
    { name: 'group', reqs: [R('group/create', { Name: 'new g', Members: [A, B] }), R('group/create', { Name: '', Members: [] }), R('group/create', { Name: 'x', Members: ['bad'] }), R('group/update', { ID: 'g:' + G1, Name: 'renamed' }), R('group/update', { ID: 'g:' + G1, Add: [S] }), R('group/update', { ID: 'g:' + G1, Remove: [B] }), R('group/update', { ID: 'g:' + G2, Remove: [B] }), R('group/update', { ID: 'g:' + G2, Name: 'member rename' }), R('group/update', { ID: 'g:ffffffffffffffff', Name: 'x' }), R('group/update', { ID: 'g:' + G1, Set: { nf: true, ttl: 300 } }), R('group/update', { ID: 'g:' + G1, Admins: [A] }), R('group/invite', { ID: 'g:' + G1 }), R('group/invite', { ID: 'g:' + G2 }), R('group/approve', { ID: 'g:' + G1, Addr: S, OK: true }), R('group/sync', { ID: 'g:' + G1 }), R('group/join', { Code: 'bad' }), R('group/leave', { ID: 'g:' + G2 }), R('group/leave', { ID: 'g:' + G2 }), R('state')] },
    { name: 'lock', reqs: [R('passcode', { Old: '', New: 'pass1234' }), R('passcode', { Old: 'wrong', New: 'x' }), R('lock', {}), R('state'), R('send', { To: A, Text: 'x' }), R('unlock', { Pass: 'nope' }), R('unlock', { Pass: 'pass1234' }), R('state'), R('passcode', { Old: 'pass1234', New: '' })] },
    { name: 'bad', reqs: [R('send', { To: 5 }), R('contact', { Addr: ['x'] }), R('message/edit', {}), R('group/update', {}), R('group/create', { Members: 'x' }), R('nonexistent', {}), R('send')] },
  ].map(sc => Object.assign({ me: K.ME.addr, store: st() }, sc));
}
// shape of a JSON value: object keys (sorted) with the shape of each; arrays by their first element
function shape(v, d = 0) {
  if (v === null) return 'null';
  if (Array.isArray(v)) return v.length ? '[' + shape(v[0], d + 1) + ']' : '[]';
  if (typeof v === 'object') { if (d > 3) return '{…}'; return '{' + Object.keys(v).sort().map(k => (/^[0-9a-f:]{6,}$|^g:/.test(k) ? '<addr>' : k) + ':' + shape(v[k], d + 1)).filter((x, i, a) => a.indexOf(x) === i).join(',') + '}'; }
  return typeof v;
}
async function cmp(inF, goF) {
  const scs = JSON.parse(fs.readFileSync(inF)), gos = JSON.parse(fs.readFileSync(goF));
  const keep = setInterval(() => {}, 1000); let bad = 0;
  for (let i = 0; i < scs.length; i++) {
    const sc = scs[i];
    const n = makeNode({ kp: K.ME, store: Object.assign({ key: K.ME.priv, peers: ['wss://x.test/'], nostrOn: false }, sc.store) });
    await n.ready();
    for (let j = 0; j < sc.reqs.length; j++) {
      const q = sc.reqs[j], g = gos[i][j];
      const s = await n.api(q.ep, q.body === null ? undefined : q.body); await n.settle(5);
      let gj = null; try { gj = JSON.parse(g.body); } catch {}
      const cls = c => c >= 200 && c < 300 ? 'ok' : c;
      const d = [];
      if (cls(g.status) !== cls(s.status)) d.push('status native=' + g.status + ' shim=' + s.status);
      if (g.status < 300 && s.status < 300) { const a = shape(gj), b = shape(s.json); if (a !== b) { const ta = a.split(/(?<=[,{])/), tb = b.split(/(?<=[,{])/); d.push('shape: only native ' + ta.filter(x => !tb.includes(x)).join(' ') + ' | only shim ' + tb.filter(x => !ta.includes(x)).join(' ')); } }
      if (d.length) { bad++; console.log(sc.name + ' ' + q.ep + ' ' + JSON.stringify(q.body).slice(0, 80) + '\n    ' + d.join('\n    ') + '\n    native body: ' + g.body.slice(0, 160).trim() + '\n    shim body:   ' + s.text.slice(0, 160)); }
    }
  }
  clearInterval(keep); console.log('differences', bad); process.exit(0);
}
if (process.argv[2] === 'gen') process.stdout.write(JSON.stringify(scenarios()));
else if (process.argv[2] === 'cmp') cmp(process.argv[3], process.argv[4]);
