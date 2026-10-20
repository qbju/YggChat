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
  On web, `uri` is a wss peer that accepts the page Origin (no relay since v0.7); show `label` when present.
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
Mailbox: removed in v0.7 (no server-side store-and-forward; offline messages stay queued on the sender).

## v0.6 images
POST /api/send {To, Text?, Img: "data:image/(jpeg|png|webp|gif);base64,..."} (decoded ≤ 600 KB; UI compresses to ≤ ~400 KB JPEG). Message.img holds the data URL.
Wire: {t:"fh", id, nick, text?, ts, ttl?, mime, size, n, sha} then {t:"fc", id, i, d} (864-byte raw chunks so every datagram fits the 1280-byte IPv6 MTU unfragmented — the PWA can't do IPv6 fragments; caption ≤800 UTF-8 bytes so the header fits too; image ≤600 KB) acked by {t:"fca", id, i}; after all chunks + SHA-256 match the receiver stores it and sends {t:"ack", id}. Sender rounds: header + unacked chunks every 2.5 s; gives up after `tries` rounds without progress → queued (no mailbox for images).


## Groups (v0.13.5, beta)
- Groups appear in `/api/state` `contacts` under the key `g:<16 hex id>` as `{addr, nick, avatar:"👥", group:true, members:[addr...], req, left}`; their messages have `peer: "g:<id>"` and `from` = the sender. Per-member delivery copies are hidden.
- POST /api/send {To:"g:<id>", Text} (text only) / {To:"g:<id>", Retry:id}
- POST /api/group/create {Name, Members:[addr]} -> group view
- POST /api/group/update {ID, Name?, Add?:[addr]} -> group view (pushed to all members)
- POST /api/group/leave {ID} -> group view (members are told)
- POST /api/contact {Addr:"g:<id>", Nick?, Accept?} renames locally / accepts an invitation; /api/contact/delete {Addr:"g:<id>"} deletes it locally.
- Wire: msg + {g, gh}; {t:"gq",g}; {t:"gi",g,gn,gm}; {t:"gl",g}. See groups.go.

## POST /api/socks (v0.13.5, native, beta)
- {} -> {on, addr, default, available}; {On?, Addr?} saves data/socks.json and restarts the node when it changed (`restart: true`). Also in /api/state as `socks`.

## POST /api/nostr  (v0.8, optional / deprecated)
Body: `{"On": bool?, "Relays": ["wss://..."]?, "Reset": bool?}` → `{"on", "relays", "defaults"}`. Also returned as `nostr` in `/api/state`.
When on, an outgoing text message that is not acked becomes status `stored` after it is parked on the relays:
sealed box (mbox.Seal of `{"w": wire, "nk": throwaway-secp256k1-key}`) as the content of a kind-1059 event signed by the throw-away key,
tags `["p", sha256("yggchat-nostr:"+hex(ed25519 pk))]`, `["expiration", now+7d]`. The recipient polls `#p` every 3 min,
opens, acks directly over Ygg and publishes the NIP-09 deletion signed with `nk`. Images are never parked. Direct resend continues.
Defaults: wss://nos.lol, wss://relay.primal.net, wss://nostr.mom (relay.damus.io accepts but does not return kind 1059 to unauthenticated readers).

## /api/system (native only)
GET → {"on":bool,"detected":"<200::/7 source address the OS would use, or empty>"}
POST {"On":bool} → writes/removes <data>/system-ygg and restarts the node.
In system mode /api/state has "system":true, "pubkey":"" and the address of the device's own Yggdrasil;
peers lists one pseudo-peer "system://…"; /api/peers POST and identity reset return errors; Nostr is inactive.

## /api/vpn (Android)
GET → {"on":bool,"tun":bool,"addr":"…","mtu":1280,"in":n,"out":n,"werr":n,"err":"…"}
POST {"On":bool} → writes/removes <data>/vpn-mode and restarts (Android only; clears system-ygg).
In VPN mode the node runs core + ipv6rwc without netstack, listens for a TUN fd on abstract unix socket
@yggchat-tun (SCM_RIGHTS, same uid only, replies 1 byte), pumps TUN<->core, and chats over the OS UDP stack.
The Java VpnService (YggVpn) polls /api/vpn every 3 s, establishes addr/7 + route 200::/7, and re-sends the fd when tun=false.

## /api/lan (v0.10, native with embedded node)
- GET -> {supported, on, pass, err, ifaces:[name], nearby:[{addr, nick?, up, iface?, ms?, contact?}]}
- POST {On: bool, Pass?: string} -> same. Starts/stops Yggdrasil multicast (all up+running+multicast, non-p2p interfaces; Beacon+Listen; optional password) live, no restart.
- Wire (UDP 7777, only between direct link-local peers): {t:"nq"} -> {t:"nr", nick}. Event {type:"lan", data:{nearby}}.
- PWA: GET returns {supported:false}.

## /api/finger (v0.10)
- GET -> {on, plan, tcp: "listening"|error|"web", addr}; POST {On, Plan(<=1500 bytes)} -> same. Stored in data/finger.json (outside the passcode envelope: it is public).
- When on: TCP 79 on our Ygg address (netstack / OS; Android VPN mode maps 79<->7979 in the TUN pump), and UDP {t:"fq"} -> {t:"fr", nick, text(<=700 bytes)}.
- POST /api/finger/query {Addr, User?} -> {addr, via:"yggchat"|"finger", nick?, plan?, raw?}; 504 when nobody answers. UDP fq and TCP finger race; User set = TCP only.

## v0.11
- POST /api/message/edit {Peer, ID, Text} -> Message (own text messages). Event {type:"msg_upd", data: Message}. Message.edited.
- POST /api/message/delete {Peer, ID, Everyone} -> {}. Event msg_del. Everyone only for own messages.
- Wire: {t:"edit", id:<ctl id>, ref:<msg id>, text} / {t:"del", id, ref} -> {t:"ack", id}. Receiver applies only if the message came from the same address. Pending ctl kept in store (Ctl) and flushed with the offline queue.
- GET/POST /api/vpn/in {Mode: closed|list|all, Ports:[int]} -> {mode, ports, vpn, dropped}. Android VPN mode inbound filter (data/vpn-in.json); chat UDP 7777, finger (when on) and replies to outbound flows always pass.
- GET /twemoji/<code>.svg — bundled Twemoji (jdecked v17.0.3, CC-BY 4.0).
