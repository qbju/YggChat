# YggChat local API (same in native daemon and browser PWA shim)

All paths are relative: call `fetch('api/state')` style is NOT needed; use absolute `/api/...` (the PWA shim intercepts any URL containing `/api/`).
POST bodies are JSON. Errors: non-2xx with plain-text message.

## GET /api/state
{ addr: "200:…" (own Yggdrasil address), nick, platform: "native"|"web", peers: <int connected peers>,
  contacts: { "<addr>": {addr, nick} },
  messages: [ {id, peer, from, nick, text, ts (unix sec), out (bool), status: "sending"|"sent"|"queued"|"failed"|undefined for incoming} ] }
Conversation key = message.peer (remote address).

## POST /api/send  {To, Text}  -> message (status "sending", appears immediately)
## POST /api/send  {To, Retry: "<message id>"}  -> re-sends a queued or failed outgoing message now
Delivery result arrives as a `status` event.

## POST /api/contact {Addr, Nick} -> contact (add or rename)
## POST /api/contact/delete {Addr} -> {}  (removes contact and its messages)
## POST /api/nick {Nick} -> {}

## GET  /api/peers -> { up: <int>, recommended: [uri], peers: [ {uri, up, inbound, latency_ms, error?, saved} ] }
## POST /api/peers {Add: uri} | {Remove: uri} | {Reset: true} -> same shape as GET
## GET  /api/peers/public -> { peers: [ {uri, region, continent?, asia, up?, reliability?, label?} ] }
  Built from the official github.com/yggdrasil-network/public-peers list. Asia first.
  On web, `uri` is a relay URL; show `label` (the original peer URI) when present.
  On native, show uri; many entries (~370): needs a search box + region filter.
  Web can only use ws/wss peers; native can use tcp/tls/quic/ws/wss.

## Events: new EventSource('/api/events'), onmessage data = JSON {type, data}
- message: a message object (incoming or our own outgoing)
- status:  {id, peer, status}
- contact: {addr, nick}
- contact_del: {addr}
- peers:   {up: <int>}

## Offline queue (v0.4)
Undelivered outgoing messages become "queued" and are re-sent automatically.
Wire additions on UDP/7777: {t:"hi"} -> reply {t:"hiack"}; receiving hi, hiack, or msg from a peer flushes everything queued for it.
Nodes greet all contacts when their first peer comes up and every 2 min, and probe peers with queued messages every 20 s. Older clients ignore hi.
