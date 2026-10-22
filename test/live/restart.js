#!/usr/bin/env node
// Live regression (r3): a node R restarts with the same key while long-running
// peers X, Y (this build) and O (v0.19.2 binary) keep sending to it.
// Measures how fast datagrams flow again both ways after R is back:
//   gi     R creates a group right away: seconds until each peer has it
//   queued the posts/DMs each peer queued for R while it was away reach R
// Usage: node test/live/restart.js -bin <ygm> -old <v0.19.2 binary> [-dir scratch]
//   [-port 19200] [-down 90] [-every 15] [-rounds 2] [-limit 30]
//   [-lag ms]     X, Y, O reach the hub through a proxy delaying every byte
//   [-churn 1]    a node Z keeps joining/leaving (hub ports reused)
//   [-freeze 1]   random SIGSTOP/SIGCONT of X/Y/O/HUB (an overloaded host)
//   [-dbg 0|1]    YGGCHAT_YGGDBG on the nodes (default 1)
// Uses ports P0..P0+42. Exit code 0 = PASS (gi within -limit s and queued
// within -limit+30 s for every peer and round), 1 = FAIL.
// Without -lag: before the r3 fix gi took 4-8 s (the first gi burst after a
// restart was always lost); after it 1.5-2 s. With -lag 1200 (≈5 s RTT,
// app-level acks time out, so "queued" is slow regardless) gi went from
// 22-110 s to 7-10 s for current peers.
const { spawn } = require('child_process');
const fs = require('fs'), http = require('http');
const arg = (k, d) => { const i = process.argv.indexOf('-' + k); return i > 0 ? process.argv[i + 1] : d; };
const HOME = process.env.HOME;
const BIN = arg('bin', HOME + '/jobs/r3-ygg/ygm'), OLD = arg('old', HOME + '/jobs/r3-ygg/ygt.0192');
const DIR = arg('dir', HOME + '/jobs/r3-ygg/rx'), P0 = +arg('port', 19200), DOWN = +arg('down', 90), ROUNDS = +arg('rounds', 2);
const LIMIT = +arg('limit', 30), DBG = arg('dbg', '1');
const HUB_TCP = P0 + 10;
const sleep = ms => new Promise(r => setTimeout(r, ms));
const T0 = Date.now(), t = () => ((Date.now() - T0) / 1000).toFixed(1);
const log = (...a) => console.log(t(), ...a);
const N = {}; let np = P0 + 21;
function def(name, old) { N[name] = { name, old, D: DIR + '/' + name, port: name === 'HUB' ? P0 + 20 : np++ }; }
def('HUB'); def('R'); def('X'); def('Y'); def('O', true); def('Z');
const CHURN = arg('churn', '0') === '1';
// -lag ms: X, Y and O reach the hub through a proxy that delays every byte (an overloaded or far path)
const LAG = +arg('lag', 0);
function lagProxy(port, ms) {
  const net = require('net');
  net.createServer(c => {
    const u = net.connect(HUB_TCP, '127.0.0.1');
    const pipe = (a, b) => { a.on('data', d => setTimeout(() => { if (!b.destroyed) b.write(d); }, ms)); a.on('close', () => setTimeout(() => b.destroy(), ms)); a.on('error', () => {}); };
    pipe(c, u); pipe(u, c);
  }).listen(port, '127.0.0.1');
}
if (LAG) { let p = P0 + 40; for (const k of ['X', 'Y', 'O']) { N[k].lag = p; lagProxy(p, LAG); p++; } }
const alive = p => { try { process.kill(p, 0); return true; } catch { return false; } };
function start(k) {
  const n = N[k]; if (n.pid && alive(n.pid)) return;
  if (!fs.existsSync(n.D)) { fs.mkdirSync(n.D, { recursive: true }); fs.writeFileSync(n.D + '/chat.json', JSON.stringify({ nick: k })); }
  const args = ['-data', n.D, '-ui', '127.0.0.1:' + n.port, '-nowindow'];
  if (k === 'HUB') args.push('-listen', `tcp://127.0.0.1:${HUB_TCP}`, '-peers', 'tcp://127.0.0.1:' + (P0 + 9));
  else args.push('-peers', 'tcp://127.0.0.1:' + (n.lag ? n.lag : HUB_TCP));
  const out = fs.openSync(n.D + '.log', 'a');
  fs.writeSync(out, `\n==== ${new Date().toISOString()} start\n`);
  const p = spawn(n.old ? OLD : BIN, args, { stdio: ['ignore', out, out], detached: true, env: Object.assign({}, process.env, { YGGCHAT_NO_OPEN: '1', YGGCHAT_YGGDBG: DBG === '1' ? '1' : '' }) });
  p.unref(); n.pid = p.pid; fs.writeFileSync(n.D + '.pid', String(p.pid)); log('start', k, p.pid);
}
function kill(k) { const n = N[k]; if (n.pid && alive(n.pid)) process.kill(n.pid, 'SIGKILL'); n.pid = 0; log('kill', k); }
const key = n => fs.readFileSync(n.D + '/ui-key', 'utf8').trim();
function api(k, p, body) {
  const n = N[k]; let kk; try { kk = key(n); } catch { return Promise.resolve({ _err: 'nokey' }); }
  return new Promise(res => {
    const data = body === undefined ? null : Buffer.from(JSON.stringify(body));
    const req = http.request({ host: '127.0.0.1', port: n.port, path: '/api/' + p, method: data ? 'POST' : 'GET', timeout: 20000,
      headers: Object.assign({ 'X-YggChat-Key': kk }, data ? { 'Content-Type': 'application/json', 'Content-Length': data.length } : {}) }, r => {
      const ch = []; r.on('data', d => ch.push(d)); r.on('end', () => { const s = Buffer.concat(ch).toString();
        if (r.statusCode >= 300) return res({ _err: r.statusCode + ' ' + s.slice(0, 200) }); try { res(JSON.parse(s)); } catch { res({ _err: 'json' }); } });
    });
    req.on('error', e => res({ _err: e.message })); req.on('timeout', () => { req.destroy(); res({ _err: 'timeout' }); });
    if (data) req.write(data); req.end();
  });
}
const st = k => api(k, 'state');
async function until(f, s, step = 500) { const end = Date.now() + s * 1000; while (Date.now() < end) { try { if (await f()) return true; } catch {} await sleep(step); } return false; }
async function up(k) { start(k); const ok = await until(async () => { const s = await st(k); return s && !s._err && (k === 'HUB' || s.peers > 0); }, 60); N[k].addr = (await st(k)).addr; log('up', k, ok, N[k].addr); }
async function befriend(a, b) { await api(a, 'contact', { Addr: N[b].addr, Nick: b, Accept: true }); await api(b, 'contact', { Addr: N[a].addr, Nick: a, Accept: true }); }
async function waitGroup(members, gkey, n, s) {
  const t0 = Date.now(), when = {};
  await until(async () => { let all = true; for (const m of members) { if (when[m]) continue; const c = ((await st(m)).contacts || {})[gkey];
    if (c && (c.members || []).length === n) when[m] = +((Date.now() - t0) / 1000).toFixed(1); else all = false; } return all; }, s);
  return when;
}
const has = async (k, gkey, text) => ((await st(k)).messages || []).some(m => m.peer === gkey && m.text === text);
async function main() {
  fs.rmSync(DIR, { recursive: true, force: true }); fs.mkdirSync(DIR, { recursive: true });
  await up('HUB'); for (const k of ['R', 'X', 'Y', 'O']) await up(k);
  for (const [a, b] of [['R', 'X'], ['R', 'Y'], ['R', 'O'], ['X', 'O'], ['X', 'Y'], ['Y', 'O']]) await befriend(a, b);
  const peers = ['X', 'Y', 'O'];
  const g0 = await api('R', 'group/create', { Name: 'g0', Members: peers.map(m => N[m].addr) });
  const w0 = await waitGroup(peers, g0.addr, 4, 60); log('g0 info', JSON.stringify(w0));
  for (const m of peers) await api(m, 'contact', { Addr: g0.addr, Accept: true });
  if (CHURN) (async () => { for (;;) { start('Z'); await sleep(4000 + Math.random() * 4000); kill('Z'); await sleep(1000 + Math.random() * 3000); } })(); // a node that keeps coming and going (PWA reloads): hub ports get reused
  if (arg('freeze', '0') === '1') (async () => { for (;;) { await sleep(3000 + Math.random() * 5000); const v = ['X', 'Y', 'O', 'HUB'][Math.floor(Math.random() * 4)]; const p = N[v].pid; if (!p) continue; try { process.kill(p, 'SIGSTOP'); await sleep(500 + Math.random() * 3000); process.kill(p, 'SIGCONT'); } catch {} } })(); // CPU starvation like an overloaded host
  const res = []; let fail = false;
  for (let r = 1; r <= ROUNDS; r++) {
    kill('R');
    // peers keep sending to the offline R (queued copies, 1:1, hellos)
    const texts = [];
    const end = Date.now() + DOWN * 1000; let i = 0;
    while (Date.now() < end) {
      for (const m of peers) { const tx = `r${r} ${m} post ${i}`; await api(m, 'send', { To: g0.addr, Text: tx }); texts.push([m, tx]); await api(m, 'send', { To: N.R.addr, Text: `r${r} ${m} dm ${i}` }); }
      i++; await sleep(+arg('every', 15) * 1000);
    }
    log('round', r, 'R down', DOWN, 's; sent', texts.length, 'posts');
    await up('R');
    const tUp = Date.now();
    // gi: R creates a group right away
    const g = await api('R', 'group/create', { Name: 'g' + r, Members: peers.map(m => N[m].addr) });
    const w = await waitGroup(peers, g.addr, 4, 150);
    log('round', r, 'gi arrival (s)', JSON.stringify(w));
    // posts queued at the peers reach R
    const got = {};
    await until(async () => {
      for (const m of peers) if (got[m] === undefined) { const mine = texts.filter(x => x[0] === m); let ok = true; for (const [, tx] of mine) if (!(await has('R', g0.addr, tx))) { ok = false; break; } if (ok) got[m] = +((Date.now() - tUp) / 1000).toFixed(1); }
      return peers.every(m => got[m] !== undefined); }, 150, 1000);
    log('round', r, 'queued posts at R (s since up)', JSON.stringify(got));
    for (const m of peers) {
      const gi = w[m], qp = got[m];
      const ok = gi !== undefined && gi <= LIMIT && qp !== undefined && qp <= LIMIT + 30;
      if (!ok) fail = true;
      res.push(`round ${r} ${m}${N[m].old ? '(v0.19.2)' : ''}: gi ${gi === undefined ? '>150' : gi}s, queued→R ${qp === undefined ? '>150' : qp}s ${ok ? 'PASS' : 'FAIL'}`);
    }
    for (const m of peers) await api(m, 'contact', { Addr: g.addr, Accept: true });
  }
  console.log('\n' + res.join('\n') + '\n' + (fail ? 'FAIL' : 'PASS'));
  return fail;
}
let code = 1;
main().then(f => { code = f ? 1 : 0; }).catch(e => { console.log('ERR', e); }).finally(() => { for (const k in N) if (N[k].pid && alive(N[k].pid)) process.kill(N[k].pid, 'SIGKILL'); process.exit(code); });
