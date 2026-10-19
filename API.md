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

## v0.5
State adds: bio, avatar, locked, haspass, blocked[]. When locked, state is {locked:true, addr, peers, platform} and other endpoints return 423 except unlock/events/peers.
Message adds: ttl, exp (unix delete time), sys ("ttl:<sec>" system line), status "stored" (legacy, removed in v0.7; treated as queued).
Contact adds: bio, avatar, hasprof, ttl, req (message request), shared (we sent our profile), renamed, pk (verified Ed25519 key hex).
POST /api/profile {Nick,Bio,Avatar} -> shares with contacts where shared.
POST /api/contact {Addr, Nick?, TTL?, Accept?}  (merge; Accept shares profile; TTL syncs to peer)
POST /api/block {Addr, Block}; POST /api/passcode {Old, New("" removes)}; POST /api/lock {}; POST /api/unlock {Pass}
POST /api/identity/reset {Confirm:true} -> new key, contacts/history wiped, node restarts.
Events add: msg_del {peer,id}, locked.
Wire adds: msg.ttl; {t:"prof",nick,bio,av,pk}; {t:"ttl",ttl}; hi/hiack carry pk. Receivers verify pk against the source address.
Mailbox (worker yggws): POST /mb/put {to:tag, box}; /mb/get and /mb/del {pk, ts, sig=Ed25519("yggchat-mb-<op>:<tag>:<ts>"), keys?}. tag = hex(sha256("yggchat-mb:"+pkHex))[:32]. Box = base64(nacl sealed box to X25519(pk) of {pk, w, sig=Ed25519(recipientPK||w)}). 7-day expiry, 200 boxes per tag.

## v0.6 images
POST /api/send {To, Text?, Img: "data:image/(jpeg|png|webp|gif);base64,..."} (decoded ≤ 600 KB; UI compresses to ≤ ~400 KB JPEG). Message.img holds the data URL.
Wire: {t:"fh", id, nick, text?, ts, ttl?, mime, size, n, sha} then {t:"fc", id, i, d} (864-byte raw chunks so every datagram fits the 1280-byte IPv6 MTU unfragmented — the PWA can't do IPv6 fragments; caption ≤800 UTF-8 bytes so the header fits too; image ≤600 KB) acked by {t:"fca", id, i}; after all chunks + SHA-256 match the receiver stores it and sends {t:"ack", id}. Sender rounds: header + unacked chunks every 2.5 s; gives up after `tries` rounds without progress → queued (no mailbox for images).
