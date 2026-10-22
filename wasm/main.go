//go:build js && wasm

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Browser Yggdrasil node for the YggChat PWA.
// Runs yggdrasil-go core in WASM, peers over wss://, and speaks the same
// UDP/7777 JSON datagram protocol as the native node by hand-building IPv6+UDP packets.
package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"syscall/js"
	"time"

	gologme "github.com/gologme/log"
	"github.com/h6e/yggchat/mbox"
	"github.com/h6e/yggchat/move"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/ipv6rwc"
)

const chatPort = 7777

var (
	ygg  *core.Core
	rwc  *ipv6rwc.ReadWriteCloser
	self net.IP
	priv ed25519.PrivateKey
)

func checksum(src, dst net.IP, udp []byte) uint16 {
	var sum uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			sum += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			sum += uint32(b[len(b)-1]) << 8
		}
	}
	add(src)
	add(dst)
	var ph [8]byte
	binary.BigEndian.PutUint32(ph[0:], uint32(len(udp)))
	ph[7] = 17
	add(ph[:])
	add(udp)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	c := ^uint16(sum)
	if c == 0 {
		c = 0xffff
	}
	return c
}

func buildPacket(dst net.IP, payload []byte) []byte {
	ulen := 8 + len(payload)
	p := make([]byte, 40+ulen)
	p[0] = 0x60
	binary.BigEndian.PutUint16(p[4:], uint16(ulen))
	p[6] = 17
	p[7] = 64
	copy(p[8:24], self)
	copy(p[24:40], dst.To16())
	u := p[40:]
	binary.BigEndian.PutUint16(u[0:], chatPort)
	binary.BigEndian.PutUint16(u[2:], chatPort)
	binary.BigEndian.PutUint16(u[4:], uint16(ulen))
	copy(u[8:], payload)
	binary.BigEndian.PutUint16(u[6:], checksum(self, dst.To16(), u))
	return p
}

func readLoop(onPacket js.Value) {
	buf := make([]byte, 65535)
	for {
		n, err := rwc.Read(buf)
		if err != nil {
			return
		}
		p := buf[:n]
		if n < 48 || p[0]>>4 != 6 || p[6] != 17 {
			continue
		}
		u := p[40:]
		if binary.BigEndian.Uint16(u[2:]) != chatPort {
			continue
		}
		ulen := int(binary.BigEndian.Uint16(u[4:]))
		if ulen < 8 || ulen > len(u) {
			continue
		}
		src := net.IP(append([]byte(nil), p[8:24]...)).String()
		onPacket.Invoke(src, string(u[8:ulen]))
	}
}

// yggStart(privKeyHex|"", peers[], onPacket(src, json)) -> {addr, key}
func start(this js.Value, args []js.Value) any {
	cfg := config.GenerateConfig()
	if k := args[0].String(); k != "" {
		if b, err := hex.DecodeString(k); err == nil && len(b) == ed25519.PrivateKeySize {
			cfg.PrivateKey = b
			if err := cfg.GenerateSelfSignedCertificate(); err != nil {
				return js.ValueOf(map[string]any{"error": err.Error()})
			}
		}
	}
	opts := []core.SetupOption{}
	for i := 0; i < args[1].Length(); i++ {
		opts = append(opts, core.Peer{URI: args[1].Index(i).String()})
	}
	l := gologme.New(os.Stderr, "[ygg] ", 0)
	l.EnableLevel("error")
	l.EnableLevel("warn")
	var err error
	ygg, err = core.New(cfg.Certificate, l, opts...)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	rwc = ipv6rwc.NewReadWriteCloser(ygg)
	a := ygg.Address()
	self = net.IP(a[:])
	priv = ed25519.PrivateKey(cfg.PrivateKey)
	go readLoop(args[2])
	return js.ValueOf(map[string]any{"addr": self.String(), "key": hex.EncodeToString(cfg.PrivateKey)})
}

// yggSend(dstAddr, json) -> "" or error
func send(this js.Value, args []js.Value) any {
	dst := net.ParseIP(args[0].String())
	if dst == nil || len(dst) != 16 || dst[0]&0xfe != 0x02 {
		return "not a Yggdrasil address"
	}
	if rwc == nil {
		return "node not ready"
	}
	if _, err := rwc.Write(buildPacket(dst, []byte(args[1].String()))); err != nil {
		return err.Error()
	}
	return ""
}

// yggPeers() -> number of up peers
func peers(this js.Value, args []js.Value) any {
	if ygg == nil {
		return 0
	}
	n := 0
	for _, p := range ygg.GetPeers() {
		if p.Up {
			n++
		}
	}
	return n
}

func peerURL(this js.Value, args []js.Value) (*url.URL, string) {
	if ygg == nil {
		return nil, "node not ready"
	}
	u, err := url.Parse(args[0].String())
	if err != nil || u.Host == "" {
		return nil, "bad peer URI"
	}
	return u, ""
}

// yggAddPeer(uri) / yggRemovePeer(uri) -> "" or error
func addPeer(this js.Value, args []js.Value) any {
	u, e := peerURL(this, args)
	if e != "" {
		return e
	}
	if err := ygg.AddPeer(u, ""); err != nil {
		return err.Error()
	}
	return ""
}

func removePeer(this js.Value, args []js.Value) any {
	u, e := peerURL(this, args)
	if e != "" {
		return e
	}
	if err := ygg.RemovePeer(u, ""); err != nil {
		return err.Error()
	}
	return ""
}

// yggPeerList() -> [{uri, up, latency_ms, error}]
func peerList(this js.Value, args []js.Value) any {
	out := []any{}
	if ygg == nil {
		return js.ValueOf(out)
	}
	for _, p := range ygg.GetPeers() {
		e := ""
		if p.LastError != nil && !p.Up {
			e = p.LastError.Error()
		}
		out = append(out, map[string]any{"uri": p.URI, "up": p.Up, "inbound": p.Inbound, "latency_ms": p.Latency.Milliseconds(), "error": e})
	}
	return js.ValueOf(out)
}

// Mailbox crypto (see mbox): yggPub() -> hex, yggSign(msg) -> hex,
// yggSeal(toPkHex, wireJSON) -> base64 | "", yggOpen(b64) -> {from, pk, w} | {error},
// yggAddrOK(addr, pkHex) -> bool, yggTag(pkHex) -> tag.
func pub(this js.Value, args []js.Value) any {
	return hex.EncodeToString(priv.Public().(ed25519.PublicKey))
}
func sign(this js.Value, args []js.Value) any {
	return hex.EncodeToString(ed25519.Sign(priv, []byte(args[0].String())))
}
// yggVerify(addr, pkHex, msg, sigB64) -> bool: sigB64 (base64, no padding)
// is pk's Ed25519 signature of msg and pk belongs to addr (signed group
// posts, groupssig.go; v0.19.3).
func verify(this js.Value, args []js.Value) any {
	if len(args) < 4 {
		return false
	}
	pk, err := hex.DecodeString(args[1].String())
	if err != nil || len(pk) != ed25519.PublicKeySize || !mbox.AddrOK(args[0].String(), pk) {
		return false
	}
	sig, err := base64.RawStdEncoding.DecodeString(args[3].String())
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pk), []byte(args[2].String()), sig)
}
func sealBox(this js.Value, args []js.Value) any {
	pk, err := hex.DecodeString(args[0].String())
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return ""
	}
	b, err := mbox.Seal(pk, priv, []byte(args[1].String()))
	if err != nil {
		return ""
	}
	return b
}
func openBox(this js.Value, args []js.Value) any {
	from, pk, w, err := mbox.Open(args[0].String(), priv)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(map[string]any{"from": from, "pk": hex.EncodeToString(pk), "w": string(w)})
}
func addrOK(this js.Value, args []js.Value) any {
	pk, err := hex.DecodeString(args[1].String())
	return err == nil && mbox.AddrOK(args[0].String(), pk)
}
func tag(this js.Value, args []js.Value) any {
	pk, _ := hex.DecodeString(args[0].String())
	return mbox.Tag(pk)
}

// yggNostrWrap(toPkHex, wireJSON) -> event JSON | "", yggNostrOpen(evJSON) -> {from, pk, w, del} | {error},
// yggNostrTag() -> our recipient tag, yggNostrDefaults() -> default relays (JSON).
func nostrWrap(this js.Value, args []js.Value) any {
	pk, err := hex.DecodeString(args[0].String())
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return ""
	}
	wrap := mbox.NostrWrap
	if len(args) > 2 && args[2].Truthy() {
		wrap = mbox.NostrWrapLegacy
	}
	ev, err := wrap(pk, priv, []byte(args[1].String()))
	if err != nil {
		return ""
	}
	return ev
}
func nostrOpen(this js.Value, args []js.Value) any {
	from, pk, w, del, err := mbox.NostrOpen(args[0].String(), priv)
	if err != nil {
		return js.ValueOf(map[string]any{"error": err.Error()})
	}
	return js.ValueOf(map[string]any{"from": from, "pk": hex.EncodeToString(pk), "w": string(w), "del": del})
}
func nostrTag(this js.Value, args []js.Value) any {
	return mbox.NostrTag(priv.Public().(ed25519.PublicKey))
}

// yggNostrTags(pksJSON) -> JSON {tags:[...], since:N}: every tag a box for us may carry now.
func nostrTags(this js.Value, args []js.Value) any {
	var hexes []string
	_ = json.Unmarshal([]byte(args[0].String()), &hexes)
	var pks [][]byte
	for _, h := range hexes {
		if pk, err := hex.DecodeString(h); err == nil && len(pk) == ed25519.PublicKeySize {
			pks = append(pks, pk)
		}
	}
	now := time.Now().Unix()
	b, _ := json.Marshal(map[string]any{"tags": mbox.NostrTags(priv, pks, now, len(args) > 1 && args[1].Truthy()), "since": mbox.NostrSince(now)})
	return string(b)
}
func nostrDefaults(this js.Value, args []js.Value) any {
	b, _ := json.Marshal(mbox.DefaultRelays)
	return string(b)
}

func main() {
	js.Global().Set("yggPub", js.FuncOf(pub))
	js.Global().Set("yggSign", js.FuncOf(sign))
	js.Global().Set("yggVerify", js.FuncOf(verify))
	js.Global().Set("yggSeal", js.FuncOf(sealBox))
	js.Global().Set("yggOpen", js.FuncOf(openBox))
	js.Global().Set("yggAddrOK", js.FuncOf(addrOK))
	js.Global().Set("yggTag", js.FuncOf(tag))
	js.Global().Set("yggNostrWrap", js.FuncOf(nostrWrap))
	js.Global().Set("yggNostrOpen", js.FuncOf(nostrOpen))
	js.Global().Set("yggNostrTag", js.FuncOf(nostrTag))
	js.Global().Set("yggNostrTags", js.FuncOf(nostrTags))
	js.Global().Set("yggNostrDefaults", js.FuncOf(nostrDefaults))
	js.Global().Set("yggAddPeer", js.FuncOf(addPeer))
	js.Global().Set("yggRemovePeer", js.FuncOf(removePeer))
	js.Global().Set("yggPeerList", js.FuncOf(peerList))
	js.Global().Set("yggStart", js.FuncOf(start))
	js.Global().Set("yggMoveNew", js.FuncOf(moveNew))
	js.Global().Set("yggMoveSeal", js.FuncOf(moveSeal))
	js.Global().Set("yggMoveOpen", js.FuncOf(moveOpen))
	js.Global().Set("yggMoveSealPass", js.FuncOf(moveSealPass))
	js.Global().Set("yggMoveOpenPass", js.FuncOf(moveOpenPass))
	js.Global().Set("yggSend", js.FuncOf(send))
	js.Global().Set("yggPeers", js.FuncOf(peers))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("yggready"))
	select {}
}

// ---------- account move (see move/move.go) ----------

var mvPriv *ecdh.PrivateKey

func jsErr(err error) any { return js.ValueOf(map[string]any{"error": err.Error()}) }

// yggMoveNew(addr) -> code (keeps the throwaway key for yggMoveOpen)
func moveNew(this js.Value, args []js.Value) any {
	k, code, err := move.NewReceiver(args[0].String())
	if err != nil {
		return ""
	}
	mvPriv = k
	return code
}

// yggMoveSeal(code, bundleJSON) -> {addr, blob(base64)} | {error}
func moveSeal(this js.Value, args []js.Value) any {
	addr, rpk, err := move.ParseCode(args[0].String())
	if err != nil {
		return jsErr(err)
	}
	b, err := move.Seal(rpk, []byte(args[1].String()))
	if err != nil {
		return jsErr(err)
	}
	return js.ValueOf(map[string]any{"addr": addr, "blob": base64.StdEncoding.EncodeToString(b)})
}

// yggMoveOpen(blobBase64) -> {plain} | {error}
func moveOpen(this js.Value, args []js.Value) any {
	if mvPriv == nil {
		return js.ValueOf(map[string]any{"error": "受け取り待ちではありません"})
	}
	b, err := base64.StdEncoding.DecodeString(args[0].String())
	if err == nil {
		b, err = move.Open(mvPriv, b)
	}
	if err != nil {
		return jsErr(err)
	}
	return js.ValueOf(map[string]any{"plain": string(b)})
}

func moveSealPass(this js.Value, args []js.Value) any {
	b, err := move.SealPass(args[0].String(), []byte(args[1].String()))
	if err != nil {
		return jsErr(err)
	}
	return js.ValueOf(map[string]any{"data": base64.StdEncoding.EncodeToString(b)})
}

func moveOpenPass(this js.Value, args []js.Value) any {
	b, err := base64.StdEncoding.DecodeString(args[1].String())
	if err == nil {
		b, err = move.OpenPass(args[0].String(), b)
	}
	if err != nil {
		return jsErr(err)
	}
	return js.ValueOf(map[string]any{"plain": string(b)})
}
