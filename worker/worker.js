// yggws: WebSocket relay so browser (WASM) Yggdrasil nodes can peer with the
// public ws/wss peers from the official github.com/yggdrasil-network/public-peers list.
// Public peers reject cross-origin browser handshakes (403), so the relay drops Origin.
// The Yggdrasil link is end-to-end encrypted between the browser node and the peer.
//
//   GET /list            -> {peers:[{uri, relay, region}]}  (relayable ws/wss peers)
//   WS  /r?u=<peer uri>  -> relayed link (only peers on the official list)
//   WS  /evn, /dnct      -> legacy aliases
const ZIP = 'https://codeload.github.com/yggdrasil-network/public-peers/zip/refs/heads/master';
const LEGACY = { evn: 'wss://ygg-evn-1.wgos.org:443', dnct: 'wss://donotclickthis.link:443/api/v2/socket' };
const TTL = 6 * 3600;

async function inflate(buf, method) {
  if (method === 0) return buf;
  const ds = new DecompressionStream('deflate-raw');
  const out = new Response(new Blob([buf]).stream().pipeThrough(ds));
  return new Uint8Array(await out.arrayBuffer());
}

async function parseZip(ab) {
  const b = new Uint8Array(ab), dv = new DataView(ab), files = [];
  let eocd = b.length - 22;
  while (eocd >= 0 && dv.getUint32(eocd, true) !== 0x06054b50) eocd--;
  if (eocd < 0) throw new Error('bad zip');
  let p = dv.getUint32(eocd + 16, true);
  const n = dv.getUint16(eocd + 10, true);
  const td = new TextDecoder();
  for (let i = 0; i < n; i++) {
    const method = dv.getUint16(p + 10, true), csize = dv.getUint32(p + 20, true);
    const nl = dv.getUint16(p + 28, true), el = dv.getUint16(p + 30, true), cl = dv.getUint16(p + 32, true);
    const off = dv.getUint32(p + 42, true);
    const name = td.decode(b.subarray(p + 46, p + 46 + nl));
    p += 46 + nl + el + cl;
    if (!name.endsWith('.md') || name.split('/').length !== 3) continue;
    const lnl = dv.getUint16(off + 26, true), lel = dv.getUint16(off + 28, true);
    const data = b.slice(off + 30 + lnl + lel, off + 30 + lnl + lel + csize);
    files.push({ name, text: td.decode(await inflate(data, method)) });
  }
  return files;
}

const isIP = h => /^[\d.]+$/.test(h) || h.startsWith('[');

async function peerList(ctx) {
  const cache = caches.default, key = new Request('https://yggws.cache/list-v1');
  const hit = await cache.match(key);
  if (hit) return hit.json();
  const r = await fetch(ZIP);
  const files = await parseZip(await r.arrayBuffer());
  const peers = [];
  for (const f of files) {
    const [, continent, file] = f.name.split('/');
    for (const m of f.text.matchAll(/`(wss?:\/\/[^`\s]+)`/g)) {
      const u = new URL(m[1]);
      if (isIP(u.hostname)) continue; // Workers cannot fetch bare IPs
      peers.push({ uri: m[1], region: file.replace('.md', ''), continent });
    }
  }
  const body = { peers, updated: Date.now() };
  ctx.waitUntil(cache.put(key, new Response(JSON.stringify(body), { headers: { 'cache-control': 'max-age=' + TTL } })));
  return body;
}

function relay(req, peerUri) {
  const u = new URL(peerUri);
  u.protocol = u.protocol === 'wss:' ? 'https:' : 'http:';
  u.searchParams.delete('key'); // the key pin is checked by the Ygg node, not the transport
  const h = new Headers();
  for (const n of ['Upgrade', 'Connection', 'Sec-WebSocket-Key', 'Sec-WebSocket-Version', 'Sec-WebSocket-Protocol']) {
    const v = req.headers.get(n); if (v) h.set(n, v);
  }
  return fetch(u.toString(), { headers: h });
}

const cors = { 'access-control-allow-origin': '*', 'content-type': 'application/json' };

export default {
  async fetch(req, env, ctx) {
    const url = new URL(req.url);
    const path = url.pathname.replace(/^\/+|\/+$/g, '');
    const ws = req.headers.get('Upgrade') === 'websocket';
    if (path === 'list') {
      const l = await peerList(ctx);
      const base = url.origin + '/r?u=';
      return new Response(JSON.stringify({ updated: l.updated, peers: l.peers.map(p => ({ ...p, relay: base.replace('https:', 'wss:') + encodeURIComponent(p.uri) })) }), { headers: cors });
    }
    if (ws && LEGACY[path]) return relay(req, LEGACY[path]);
    if (ws && path === 'r') {
      const target = url.searchParams.get('u') || '';
      const l = await peerList(ctx);
      if (!l.peers.some(p => p.uri === target)) return new Response('peer not on the official public list', { status: 403 });
      return relay(req, target);
    }
    return new Response('YggChat Yggdrasil WebSocket relay.\nGET /list, WS /r?u=<peer uri from github.com/yggdrasil-network/public-peers>\n', { headers: { 'content-type': 'text/plain' } });
  },
};
