// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// YggChat: serverless P2P chat over Yggdrasil.
// Every node gets a global, stable IPv6 (200::/7) derived from its key,
// so peers dial each other directly. No NAT traversal, no server.
// Yggdrasil runs embedded in userspace (yggstack netstack): no TUN, no root.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	gologme "github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggstack/src/netstack"
)

//go:embed web
var webFS embed.FS

const chatPort = 7777

// Recommended peers: Japan/Asia first, then well-known EU nodes as backbone.
var defaultPeers = []string{
	"tls://yg-tyo.magicum.net:32333",
	"tls://133.18.201.69:54232",
	"tls://yg-sin.magicum.net:23900",
	"tls://ygg.mkg20001.io:443",
}

type Message struct {
	ID   string `json:"id"`
	Peer string `json:"peer"` // remote ygg address (conversation key)
	From string `json:"from"` // sender ygg address
	Nick string `json:"nick"`
	Text string `json:"text"`
	TS   int64  `json:"ts"`
	Out  bool   `json:"out"`
	// Status of outgoing messages: sending | sent | queued (auto-resent when the peer is reachable) | stored (legacy v0.5/v0.6 mailbox; treated as queued) | failed.
	Status string `json:"status,omitempty"`
	TTL    int    `json:"ttl,omitempty"`   // disappearing timer (seconds)
	Exp    int64  `json:"exp,omitempty"`   // unix time when this message is deleted
	Sys    string `json:"sys,omitempty"`   // system line, e.g. "ttl:300"
	Img    string `json:"img,omitempty"`   // data:image/...;base64,... (compressed by the UI)
	Nostr  bool   `json:"nostr,omitempty"` // also parked on Nostr relays (opt-in offline delivery)
	Edited bool   `json:"edited,omitempty"`
	// Per-member copy of a group message (not shown; the "g:<id>" message is).
	Grp string `json:"grp,omitempty"` // group id
	Of  string `json:"of,omitempty"`  // id of the visible group message
	Hid bool   `json:"hid,omitempty"`
	// GImg: a hidden group copy whose photo is on the visible message (Of).
	GImg bool `json:"gimg,omitempty"`
	Hist bool `json:"hist,omitempty"` // relayed by the group host as history for a new member (v0.18)
	Via  string `json:"via,omitempty"` // history relayed by another member, not the sender (v0.19.1)
	Dlv  int    `json:"dlv,omitempty"` // group copies delivered so far (v0.19.1)
	Tot  int    `json:"tot,omitempty"` // group copies made when sent (recipients then)
	Pn   string `json:"pn,omitempty"`  // proof-of-work nonce (pow.go)
}

type Contact struct {
	Addr    string `json:"addr"`
	Nick    string `json:"nick"`
	Renamed bool   `json:"renamed,omitempty"` // nick set locally; ignore peer's
	Bio     string `json:"bio,omitempty"`
	Avatar  string `json:"avatar,omitempty"`
	HasProf bool   `json:"hasprof,omitempty"`
	TTL     int    `json:"ttl,omitempty"`
	Req     bool   `json:"req,omitempty"`    // message request not yet accepted
	Shared  bool   `json:"shared,omitempty"` // we shared our profile with them
	PK      string `json:"pk,omitempty"`     // full Ed25519 key (verified against addr)
	Dom     string `json:"dom,omitempty"`    // domain the peer claims in its profile
	DomOK   bool   `json:"domok,omitempty"`  // its AAAA is this peer's address (checked by us)
	DomAt   int64  `json:"domat,omitempty"`  // last check
	TTLAt   int64  `json:"ttlat,omitempty"`  // time of the message/setting the current TTL came from (local)
}

type Store struct {
	mu       sync.Mutex
	path     string
	locked   bool
	key      []byte
	salt     []byte
	env      *envelope
	dirty    bool
	saveT    *time.Timer
	Nick     string             `json:"nick"`
	Bio      string             `json:"bio,omitempty"`
	Avatar   string             `json:"avatar,omitempty"`
	Domain   string             `json:"domain,omitempty"` // our verified domain (v0.14)
	Contacts map[string]Contact `json:"contacts"`
	Messages []Message          `json:"messages"`
	Blocked  map[string]bool    `json:"blocked,omitempty"`
	// Park undeliverable messages on public Nostr relays (on by default for new installs since v0.13.6).
	NostrOn     bool     `json:"nostr_on,omitempty"`
	NostrRelays []string `json:"nostr_relays,omitempty"`
	// Also send/receive pre-v0.13 boxes with the static tag (until v0.24).
	NostrLegacy bool `json:"nostr_legacy,omitempty"`
	// Edits / unsends of our messages not yet acked by the peer.
	Ctl []ctlItem `json:"ctl,omitempty"`
	// Group chats (groups.go), keyed by group id.
	Groups map[string]Group `json:"groups,omitempty"`
	// Refuse message requests and invite-link joins without proof of work (v0.19, opt-in).
	PowStrict bool `json:"pow_strict,omitempty"`
	// Our pending invite-link joins: group id -> host, until they expire (invite.go).
	Joins map[string]joinWait `json:"joins,omitempty"`
}

func (s *Store) save() {
	s.dirty = false
	if s.locked {
		return
	}
	if b, err := s.encode(); err == nil {
		_ = writeAtomic(s.path, b)
	}
}

// saveSoon coalesces many saves into one within half a second. Caller holds s.mu.
func (s *Store) saveSoon() {
	s.dirty = true
	if s.saveT != nil {
		return
	}
	s.saveT = time.AfterFunc(500*time.Millisecond, func() {
		s.mu.Lock()
		s.saveT = nil
		if s.dirty {
			s.save()
		}
		s.mu.Unlock()
	})
}

// writeAtomic writes via a 0600 temp file + fsync + rename so a crash never
// leaves a half-written file behind.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(path, 0600)
	return os.Rename(tmp, path)
}

var msgIDRe = regexp.MustCompile(`^[0-9A-Za-z_-]{1,40}$`)

// cleanNick drops control/bidi characters and caps the length of a peer-supplied nick.
func cleanNick(n string) string {
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, n)
	n = strings.TrimSpace(n)
	if r := []rune(n); len(r) > 32 {
		n = string(r[:32])
	}
	return n
}

// localOnly rejects requests that could come from other web pages or DNS
// rebinding: Host must be loopback, a present Origin must match it, and
// POST bodies must be application/json (which forces a CORS preflight).
func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// checkUIAddr refuses to serve the local API anywhere but loopback: it has no
// authentication, so exposing it (e.g. -ui 0.0.0.0:7780) would hand the node,
// keys and history to anyone who can reach the port.
func checkUIAddr(addr string) error {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-ui %q: %v", addr, err)
	}
	if !isLoopbackHost(h) {
		return fmt.Errorf("-ui %q: the UI/API has no authentication and may only listen on loopback (127.0.0.1, [::1], localhost)", addr)
	}
	return nil
}

func localOnly(uiAddr string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The peer address is what really proves the request is local; the
		// Host header alone can be forged by anyone who can reach the port.
		if rh, _, err := net.SplitHostPort(r.RemoteAddr); err != nil || !isLoopbackHost(rh) {
			http.Error(w, "forbidden", 403)
			return
		}
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
			http.Error(w, "forbidden host", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "forbidden origin", 403)
					return
				}
			}
			if r.Method != "GET" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "content-type must be application/json", 415)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "forbidden", 403)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

type App struct {
	core     *core.Core
	ns       *netstack.YggdrasilNetstack
	sys      bool // using the device's own Yggdrasil (VPN/TUN) instead of the embedded node
	vpn      bool // Android: we ARE the device's Yggdrasil (VpnService TUN pumped into core)
	dataDir  string
	vpnS     *vpnState
	addr     string
	store    *Store
	subMu    sync.Mutex
	subs     map[chan []byte]struct{}
	udp      net.PacketConn
	ackMu    sync.Mutex
	acks     map[string]chan struct{}
	ackFrom  map[string]string // id -> the peer whose ack counts (ackMu)
	pwMu     sync.Mutex
	pwFails  map[string]*pwFail // wrong group passwords per address (groupset.go)
	flMu     sync.Mutex
	inflight map[string]bool
	probing  map[string]bool // peers with a quick re-probe running (flMu)
	ctlBusy  map[string]bool // peers with flushCtl running (flMu)
	fsOnce   sync.Once
	fs       *fileState
	seenMu   sync.Mutex
	seen     map[string]bool
	profAt   map[string]time.Time
	cfg      *config.NodeConfig
	cfgPath  string
	fing     *fingerState
	site     *siteState
	lan      *lanState
}

func (a *App) broadcast(kind string, v any) {
	b, _ := json.Marshal(map[string]any{"type": kind, "data": v})
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for c := range a.subs {
		select {
		case c <- b:
		default:
		}
	}
}

func (a *App) addMessage(m Message) {
	a.store.mu.Lock()
	if isGroupKey(m.Peer) && !m.Out && m.Sys == "" && m.From != "" {
		// One post, one message: member copies and relays carry different ids (v0.19.2).
		for i := len(a.store.Messages) - 1; i >= 0; i-- {
			if x := a.store.Messages[i]; x.Peer == m.Peer && x.From == m.From && !x.Out && samePost(x.ID, m.ID) {
				a.store.mu.Unlock()
				return
			}
		}
	}
	a.store.Messages = append(a.store.Messages, m)
	if isGroupKey(m.Peer) {
		// group conversations have no contact entry
	} else if _, ok := a.store.Contacts[m.Peer]; !ok {
		c := Contact{Addr: m.Peer, Req: !m.Out, Shared: m.Out, TTL: m.TTL}
		if !m.Out {
			c.Nick = m.Nick
		}
		a.store.Contacts[m.Peer] = c
		defer a.broadcast("contact", c)
	} else if c := a.store.Contacts[m.Peer]; !m.Out && m.Nick != "" && !c.Renamed && c.Nick != m.Nick {
		c.Nick = m.Nick
		a.store.Contacts[m.Peer] = c
		defer a.broadcast("contact", c)
	}
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("message", m)
}

// Wire format (shared with the browser/WASM node): one JSON datagram per UDP
// packet on port 7777. {"t":"msg","id","nick","text","ts"} -> {"t":"ack","id"}
type wire struct {
	T    string `json:"t"`
	ID   string `json:"id,omitempty"`
	Nick string `json:"nick,omitempty"`
	Text string `json:"text,omitempty"`
	TS   int64  `json:"ts,omitempty"`
	TTL  int    `json:"ttl,omitempty"`
	Bio  string `json:"bio,omitempty"`
	Av   string `json:"av,omitempty"`
	Dom  string `json:"dom,omitempty"`  // claimed domain (prof); verified by its AAAA
	PK   string `json:"pk,omitempty"`   // sender's full Ed25519 key (hi/hiack/prof)
	Mime string `json:"mime,omitempty"` // image header (fh)
	Size int    `json:"size,omitempty"`
	N    int    `json:"n,omitempty"`
	Sha  string `json:"sha,omitempty"`
	I    int    `json:"i,omitempty"`   // chunk index (fc/fca)
	D    string `json:"d,omitempty"`   // chunk data, base64
	Ref  string `json:"ref,omitempty"` // edit/del: id of the message it changes
	// Group chat (groups.go)
	G   string   `json:"g,omitempty"`   // group id
	GN  string   `json:"gn,omitempty"`  // group name (gi)
	GM  []string `json:"gm,omitempty"`  // members (gi)
	GO  string   `json:"go,omitempty"`  // host (gi, v0.16+)
	GH  string   `json:"gh,omitempty"`  // hash of name+members (msg)
	Tok string   `json:"tok,omitempty"` // invite-link token (gj, v0.17)
	// v0.18 group settings
	GS *GroupSet `json:"gs,omitempty"` // host's settings (gi)
	Pw string    `json:"pw,omitempty"` // join password (gj)
	K  string    `json:"k,omitempty"`  // gjn kind: "pw" (password needed/wrong), "wait" (approval pending)
	By string    `json:"by,omitempty"` // ghs: original sender of a history message
	// v0.19 large groups / co-admins (groupadmin.go)
	GV  int      `json:"gv,omitempty"`  // member-list version
	GAD []string `json:"gad,omitempty"` // co-admins (gi)
	GSV int      `json:"gsv,omitempty"` // version of name/settings/admins (gi)
	GC  int      `json:"gc,omitempty"`  // member count of a split gi
	GMB string   `json:"gmb,omitempty"` // packed members (gp)
	GA  string   `json:"ga,omitempty"`  // packed members added (gd)
	GR  string   `json:"gr,omitempty"`  // packed members removed (gd)
	PN  string   `json:"pn,omitempty"`  // proof-of-work nonce (msg, fh, gj)
	GT  map[string]int64 `json:"gt,omitempty"` // gi: when recently removed members dropped out (v0.19.3)

	checked bool // local only: proof of work already verified (file header)
}

func (a *App) serveChat() {
	var pc net.PacketConn
	var err error
	if a.sys || a.vpn {
		pc, err = net.ListenUDP("udp6", &net.UDPAddr{Port: chatPort})
	} else {
		pc, err = a.ns.ListenUDP(&net.UDPAddr{IP: net.ParseIP(a.addr), Port: chatPort})
	}
	if err != nil {
		log.Fatal("listen: ", err)
	}
	a.udp = pc
	buf := make([]byte, 65535)
	for {
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		var w wire
		if json.Unmarshal(buf[:n], &w) != nil {
			continue
		}
		// The source IP is cryptographically bound to the sender's key by Yggdrasil.
		rip := raddr.(*net.UDPAddr).IP
		if len(rip) != 16 || (rip[0]&0xfe) != 0x02 {
			continue // only Yggdrasil sources (system mode listens on the OS stack)
		}
		from := rip.String()
		if w.T == "fca" {
			a.onFileAck(from, w)
			continue
		}
		if w.T == "pow" {
			a.onPowNeed(from, w)
			continue
		}
		if w.T == "ack" {
			a.ackMu.Lock()
			if ch, ok := a.acks[w.ID]; ok && a.ackFrom[w.ID] == from {
				close(ch)
				delete(a.acks, w.ID)
				delete(a.ackFrom, w.ID)
			}
			a.ackMu.Unlock()
			a.askPK(from)
			continue
		}
		a.store.mu.Lock()
		locked, blocked := a.store.locked, a.store.Blocked[from]
		a.store.mu.Unlock()
		if blocked {
			continue
		}
		switch w.T {
		case "mh", "mc":
			a.onMoveIn(from, w)
			continue
		case "mha", "mca", "mok", "mng":
			a.onMoveAck(from, w)
			continue
		case "fq", "fr":
			a.onFingerUDP(from, w)
			continue
		case "nq", "nr":
			a.onLANUDP(from, w)
			continue
		}
		// While locked we can't store anything: stay silent so the sender keeps it queued.
		if locked {
			continue
		}
		if w.PK != "" {
			a.learnPK(from, w.PK)
		}
		if isGroupCtl(w.T) {
			a.onGroupCtl(from, w)
			continue
		}
		switch w.T {
		case "hi", "hiack":
			if w.T == "hi" {
				a.sendWire(from, wire{T: "hiack", PK: hex.EncodeToString(a.pub())})
			}
			a.maybeProf(from)
			go a.flush(from)
		case "fh":
			a.onFileHeader(from, w)
		case "fc":
			a.onFileChunk(from, w)
		case "prof", "ttl":
			a.handleCtl(from, w)
		case "edit", "del":
			if a.onCtl(from, w) {
				a.sendWire(from, wire{T: "ack", ID: w.ID})
			}
		case "msg":
			if a.handleMsg(from, w) {
				a.sendWire(from, wire{T: "ack", ID: w.ID})
				a.askPK(from)
				go a.flush(from)
			}
		}
	}
}

// isGroupCtl reports whether t is a group control packet (onGroupCtl). "ghq"
// (catch-up request) was missing before v0.19.2, so native nodes never
// answered it; it is only safe together with the post-id dedupe.
func isGroupCtl(t string) bool {
	switch t {
	case "gq", "gi", "gl", "gj", "gjn", "ghs", "ghq", "gd", "gp", "gk", "gkq":
		return true
	}
	return false
}

// handleMsg validates and stores an incoming message (direct or from the
// mailbox). Returns true when it should be acked.
func (a *App) handleMsg(from string, w wire) bool {
	if w.G != "" {
		return a.onGroupMsg(from, w, "")
	}
	if w.Text == "" {
		return false
	}
	return a.acceptMsg(from, w, "")
}

func (a *App) acceptMsg(from string, w wire, img string) bool {
	if (w.Text == "" && img == "") || len(w.Text) > maxRecvText || !msgIDRe.MatchString(w.ID) {
		return false
	}
	a.store.mu.Lock()
	if a.store.locked || a.store.Blocked[from] {
		a.store.mu.Unlock()
		return false
	}
	dup := false
	for i := len(a.store.Messages) - 1; i >= 0 && !dup; i-- {
		dup = a.store.Messages[i].Peer == from && a.store.Messages[i].ID == w.ID
	}
	c, known := a.store.Contacts[from]
	a.store.mu.Unlock()
	key := from + "/" + w.ID
	a.seenMu.Lock()
	dup = dup || a.seen[key]
	a.seen[key] = true
	a.seenMu.Unlock()
	if dup {
		return true
	}
	if w.TTL < 0 || w.TTL > 4*7*86400 {
		w.TTL = 0
	}
	if !a.powCheck(from, w) {
		a.seenMu.Lock()
		delete(a.seen, key) // let the recomputed one in
		a.seenMu.Unlock()
		return false
	}
	// Only a message newer than the last change moves the timer (late Nostr
	// copies arrive in any order), v0.19.2.
	if at := min(w.TS, time.Now().Unix()+300); known && at >= c.TTLAt {
		if c.TTL != w.TTL {
			a.setTTLAt(from, w.TTL, false, at)
		} else {
			a.store.mu.Lock()
			if x, ok := a.store.Contacts[from]; ok && at > x.TTLAt {
				x.TTLAt = at
				a.store.Contacts[from] = x
				a.store.saveSoon()
			}
			a.store.mu.Unlock()
		}
	}
	m := Message{ID: w.ID, Peer: from, From: from, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, TTL: w.TTL, Img: img}
	if w.TTL > 0 {
		m.Exp = time.Now().Unix() + int64(w.TTL)
	}
	a.addMessage(m)
	return true
}

// maybeProf re-shares our profile with accepted contacts (at most every 10 min).
func (a *App) maybeProf(peer string) {
	a.store.mu.Lock()
	c, ok := a.store.Contacts[peer]
	a.store.mu.Unlock()
	if !ok || !c.Shared {
		return
	}
	a.seenMu.Lock()
	last := a.profAt[peer]
	if time.Since(last) < 10*time.Minute {
		a.seenMu.Unlock()
		return
	}
	a.profAt[peer] = time.Now()
	a.seenMu.Unlock()
	a.sendProf(peer)
}

func (a *App) setStatus(peer, id, status string) {
	a.store.mu.Lock()
	grp, of := "", ""
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := &a.store.Messages[i]; m.ID == id && m.Peer == peer {
			if m.Hid && m.Status == "failed" && status != "sent" && a.groupOutLocked(m.Grp) {
				a.store.mu.Unlock() // given up when we left the group: a retry still finishing can't revive it
				return
			}
			m.Status = status
			if m.Hid {
				grp, of = m.Grp, m.Of
				if status == "sent" {
					// A delivered group copy is no longer needed (the visible
					// message's status counts the copies still pending).
					a.store.Messages = append(a.store.Messages[:i], a.store.Messages[i+1:]...)
					for j := len(a.store.Messages) - 1; j >= 0; j-- {
						if v := &a.store.Messages[j]; v.ID == of && v.Peer == gKey(grp) {
							v.Dlv++
							break
						}
					}
				}
			}
			break
		}
	}
	if grp != "" {
		a.store.saveSoon() // a big group changes many copies at once
	} else {
		a.store.save()
	}
	a.store.mu.Unlock()
	if grp != "" {
		a.groupStatus(grp, of)
		return
	}
	a.broadcast("status", map[string]string{"id": id, "peer": peer, "status": status})
}

// send stores the message as "sending" and delivers it in the background.
func (a *App) send(to, text, img string) (Message, error) {
	if isGroupKey(to) {
		return a.sendGroup(strings.TrimPrefix(to, "g:"), text, img)
	}
	ip := net.ParseIP(to)
	if ip == nil || len(ip) != 16 || (ip[0]&0xfe) != 0x02 {
		return Message{}, fmt.Errorf("not a Yggdrasil address: %s", to)
	}
	if (text == "" && img == "") || len(text) > maxSendText {
		return Message{}, fmt.Errorf("message empty or too long")
	}
	if img != "" {
		if _, _, err := parseImg(img); err != nil {
			return Message{}, err
		}
		if escLen(text) > capMax {
			return Message{}, fmt.Errorf("写真の説明が長すぎます（日本語でおよそ260文字まで）")
		}
	}
	peer := ip.String()
	a.store.mu.Lock()
	if a.store.locked {
		a.store.mu.Unlock()
		return Message{}, errLocked
	}
	nick := a.store.Nick
	c, ok := a.store.Contacts[peer]
	share := !ok || !c.Shared
	if ok && (c.Req || !c.Shared) {
		// Replying is accepting the request.
		c.Req, c.Shared = false, true
		a.store.Contacts[peer] = c
	}
	a.store.mu.Unlock()
	if ok && share {
		a.broadcast("contact", c)
	}
	m := Message{ID: fmt.Sprintf("%x", time.Now().UnixNano()), Peer: peer, From: a.addr, Nick: nick, Text: text, TS: time.Now().Unix(), Out: true, Status: "sending", TTL: c.TTL, Img: img}
	if c.TTL > 0 {
		m.Exp = m.TS + int64(c.TTL)
	}
	a.addMessage(m)
	if share {
		a.sendProf(peer)
	}
	go a.deliver(m)
	return m, nil
}

func (a *App) retry(peer, id string) (Message, error) {
	a.store.mu.Lock()
	var found *Message
	for i := range a.store.Messages {
		if a.store.Messages[i].ID == id && a.store.Messages[i].Peer == peer && a.store.Messages[i].Out {
			c := a.store.Messages[i]
			found = &c
		}
	}
	a.store.mu.Unlock()
	if found == nil {
		return Message{}, fmt.Errorf("no such message")
	}
	a.setStatus(peer, id, "sending")
	found.Status = "sending"
	go a.deliver(*found)
	return *found, nil
}

func (a *App) deliver(m Message) { a.deliverN(m, 8) }

// deliverN sends m up to tries times; on no ack the message goes back to the
// offline queue. Returns true when acked.
func (a *App) deliverN(m Message, tries int) bool {
	a.flMu.Lock()
	if a.inflight[m.ID] {
		a.flMu.Unlock()
		return false
	}
	a.inflight[m.ID] = true
	a.flMu.Unlock()
	defer func() { a.flMu.Lock(); delete(a.inflight, m.ID); a.flMu.Unlock() }()
	if m.Hid {
		a.store.mu.Lock()
		out := a.groupOutLocked(m.Grp)
		a.store.mu.Unlock()
		if out && !isLeaving(m.Grp) {
			if !m.Nostr {
				a.setStatus(m.Peer, m.ID, "failed") // we left (or were removed): not retried
			}
			return true
		}
	}
	if m.Status != "sending" {
		a.setStatus(m.Peer, m.ID, "sending")
	}
	if m.GImg && m.Img == "" {
		if m.Img = a.groupImg(m.Grp, m.Of); m.Img == "" {
			// The photo was deleted (or expired) here before this copy went out.
			a.dropCopy(m.Peer, m.ID)
			return true
		}
	}
	if m.Img != "" || a.longText(m) {
		var ok bool
		if m.Img != "" {
			ok = a.deliverImg(m, tries)
		} else {
			ok = a.deliverFile(m, textMime, []byte(m.Text), tries)
		}
		if !ok {
			go a.probeSoon(m.Peer)
		}
		return ok
	}
	b, _ := json.Marshal(a.msgWire(m))
	asked := powAsked(m.Peer, m.ID)
	ch := a.waitAck(m.Peer, m.ID)
	defer a.doneAck(m.ID, ch)
	for a.udp == nil {
		time.Sleep(200 * time.Millisecond)
	}
	dst := &net.UDPAddr{IP: net.ParseIP(m.Peer), Port: chatPort}
	// First packet to a new key triggers a key lookup, so retry a few times.
	for i := 0; i < tries; i++ {
		if x := powAsked(m.Peer, m.ID); x > asked {
			asked = x // the receiver wants more work
			m.Pn = ""
			b, _ = json.Marshal(a.msgWire(m))
		}
		a.udpWrite(b, dst)
		select {
		case <-ch:
			a.setStatus(m.Peer, m.ID, "sent")
			return true
		case <-time.After(2500 * time.Millisecond):
		}
	}
	if m.Nostr {
		a.setStatus(m.Peer, m.ID, "stored")
	} else {
		a.setStatus(m.Peer, m.ID, "queued")
		go a.nostrPark(m)
	}
	go a.probeSoon(m.Peer)
	return false
}

// probeSoon greets a peer we just failed to reach a few times in quick
// succession (3, 5, 10 s), so a message gets through as soon as the route
// is found instead of waiting for the 20 s queue loop. Its hiack flushes.
func (a *App) probeSoon(peer string) {
	a.flMu.Lock()
	if a.probing == nil {
		a.probing = map[string]bool{}
	}
	if a.probing[peer] {
		a.flMu.Unlock()
		return
	}
	a.probing[peer] = true
	a.flMu.Unlock()
	defer func() { a.flMu.Lock(); delete(a.probing, peer); a.flMu.Unlock() }()
	for _, d := range []time.Duration{3, 5, 10} {
		time.Sleep(d * time.Second)
		if len(a.queued(peer)) == 0 {
			return
		}
		a.hello(peer)
	}
}

// nostrPark parks a message the peer did not ack on Nostr relays (if enabled).
func (a *App) nostrPark(m Message) {
	if !a.nostrPut(m) {
		return
	}
	a.store.mu.Lock()
	st, grp, of := "", "", ""
	for i := range a.store.Messages {
		x := &a.store.Messages[i]
		if x.Peer == m.Peer && x.ID == m.ID && x.Status == "queued" {
			x.Nostr, x.Status, st = true, "stored", "stored"
			if x.Hid {
				grp, of = x.Grp, x.Of
			}
			a.store.save()
		}
	}
	a.store.mu.Unlock()
	if grp != "" {
		a.groupStatus(grp, of) // the visible group message shows the combined status
		return
	}
	if st != "" {
		a.broadcast("status", map[string]string{"id": m.ID, "peer": m.Peer, "status": st})
	}
}

func (a *App) queued(peer string) []Message {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []Message
	for _, m := range a.store.Messages {
		if m.Out && (m.Status == "queued" || m.Status == "stored") && !isGroupKey(m.Peer) && (peer == "" || m.Peer == peer) && !(m.Hid && a.groupOutLocked(m.Grp)) {
			out = append(out, m)
		}
	}
	return out
}

// flush re-sends everything queued for peer, oldest first; stops at the first miss.
func (a *App) flush(peer string) {
	for _, m := range a.queued(peer) {
		if !a.deliverN(m, 4) {
			return
		}
	}
	a.flushCtl(peer)
}

func (a *App) hello(peer string) {
	if a.udp == nil {
		return
	}
	a.sendWire(peer, wire{T: "hi", PK: hex.EncodeToString(a.pub())})
}

func (a *App) helloAll() {
	a.store.mu.Lock()
	var ps []string
	for p := range a.store.Contacts {
		ps = append(ps, p)
	}
	a.store.mu.Unlock()
	for _, p := range ps {
		a.hello(p)
	}
}

type peerView struct {
	URI     string `json:"uri"`
	Up      bool   `json:"up"`
	Inbound bool   `json:"inbound"`
	Latency int64  `json:"latency_ms"`
	Error   string `json:"error,omitempty"`
	Saved   bool   `json:"saved"`
}

func (a *App) peerList() []peerView {
	if a.sys {
		v := peerView{URI: "system://（端末のYggdrasilアプリ）", Up: a.addr != ""}
		if a.addr == "" {
			v.Error = "端末のYggdrasilが見つかりません（VPNがオフ?）"
		}
		return []peerView{v}
	}
	saved := map[string]bool{}
	for _, p := range a.cfg.Peers {
		saved[p] = true
	}
	seen := map[string]bool{}
	out := []peerView{}
	for _, p := range a.core.GetPeers() {
		v := peerView{URI: p.URI, Up: p.Up, Inbound: p.Inbound, Latency: p.Latency.Milliseconds(), Saved: saved[p.URI]}
		if p.LastError != nil && !p.Up {
			v.Error = p.LastError.Error()
		}
		seen[p.URI] = true
		out = append(out, v)
	}
	for _, p := range a.cfg.Peers {
		if !seen[p] {
			out = append(out, peerView{URI: p, Saved: true})
		}
	}
	return out
}

func (a *App) upPeers() int {
	if a.sys {
		if a.addr != "" {
			return 1
		}
		return 0
	}
	n := 0
	for _, p := range a.core.GetPeers() {
		if p.Up {
			n++
		}
	}
	return n
}

func (a *App) saveCfg() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	_ = writeAtomic(a.cfgPath, b)
}

func (a *App) setPeers(list []string) error {
	if a.sys {
		return fmt.Errorf("端末のYggdrasilを使用中です。ピアはYggdrasilアプリ側で設定してください")
	}
	want := map[string]bool{}
	for _, p := range list {
		u, err := url.Parse(p)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("bad peer URI: %s", p)
		}
		want[p] = true
	}
	have := map[string]bool{}
	for _, p := range effectivePeers(a.cfg.Peers) {
		have[p] = true
	}
	eff := map[string]bool{}
	for _, p := range effectivePeers(list) {
		eff[p] = true
	}
	want = eff
	for p := range have {
		if !want[p] {
			u, _ := url.Parse(p)
			_ = a.core.RemovePeer(u, "")
		}
	}
	for p := range want {
		if !have[p] {
			u, _ := url.Parse(p)
			_ = a.core.AddPeer(u, "")
		}
	}
	a.cfg.Peers = list
	a.saveCfg()
	return nil
}

var peerURIRe = regexp.MustCompile("`((?:tcp|tls|quic|ws|wss)://[^`\\s]+)`")

// publicPeers builds the list from the official github.com/yggdrasil-network/public-peers
// repo (one zip download), annotated with live status from the community crawler when reachable.
// Asia first, then by region; unreachable/flaky peers last.
func publicPeers() ([]map[string]any, error) {
	cl := netHTTP(20 * time.Second)
	r, err := cl.Get("https://codeload.github.com/yggdrasil-network/public-peers/zip/refs/heads/master")
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	// Optional live status (crawler keyed by country file name).
	type st struct {
		Up     bool   `json:"up"`
		States string `json:"states"`
	}
	status := map[string]st{}
	if r2, err := netHTTP(8 * time.Second).Get("https://publicpeers.neilalexander.dev/publicnodes.json"); err == nil {
		var d map[string]map[string]st
		if json.NewDecoder(r2.Body).Decode(&d) == nil {
			for _, m := range d {
				for u, v := range m {
					status[u] = v
				}
			}
		}
		r2.Body.Close()
	}
	out := []map[string]any{}
	for _, f := range zr.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) != 3 || !strings.HasSuffix(parts[2], ".md") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		md, _ := io.ReadAll(rc)
		rc.Close()
		continent, country := parts[1], strings.TrimSuffix(parts[2], ".md")
		for _, m := range peerURIRe.FindAllStringSubmatch(string(md), -1) {
			uri := m[1]
			e := map[string]any{"uri": uri, "continent": continent, "region": country, "asia": continent == "asia"}
			if v, ok := status[uri]; ok {
				e["up"] = v.Up
				if len(v.States) > 0 {
					e["reliability"] = float64(strings.Count(v.States, "*")) / float64(len(v.States))
				}
			}
			out = append(out, e)
		}
	}
	score := func(e map[string]any) int {
		if up, ok := e["up"].(bool); ok && !up {
			return 2
		}
		if rel, ok := e["reliability"].(float64); ok && rel < 0.9 {
			return 1
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i]["asia"] != out[j]["asia"] {
			return out[i]["asia"].(bool)
		}
		if si, sj := score(out[i]), score(out[j]); si != sj {
			return si < sj
		}
		if out[i]["region"] != out[j]["region"] {
			return out[i]["region"].(string) < out[j]["region"].(string)
		}
		return out[i]["uri"].(string) < out[j]["uri"].(string)
	})
	return out, nil
}

func (a *App) httpHandlers(mux *http.ServeMux) {
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/twemoji/", twemojiHandler)
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		peers := a.upPeers()
		w.Header().Set("Content-Type", "application/json")
		if a.store.locked {
			json.NewEncoder(w).Encode(map[string]any{"locked": true, "addr": a.addr, "peers": peers, "platform": "native"})
			return
		}
		blocked := []string{}
		for b := range a.store.Blocked {
			blocked = append(blocked, b)
		}
		sort.Strings(blocked)
		json.NewEncoder(w).Encode(map[string]any{
			"addr": a.addr, "pubkey": hex.EncodeToString(a.pub()), "system": a.sys, "vpn": a.vpn,
			"nick": a.store.Nick, "bio": a.store.Bio, "avatar": a.store.Avatar, "domain": a.store.Domain,
			"contacts": a.contactsViewLocked(), "messages": a.visibleMessagesLocked(), "peers": peers, "platform": "native",
			"locked": false, "haspass": a.store.key != nil, "blocked": blocked,
			"nostr": a.nostrStateLocked(), "socks": a.socksState(), "powstrict": a.store.PowStrict,
			"joins": a.joinsViewLocked(),
		})
	})
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ To, Text, Retry, Img string }
		if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 2<<20)).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		var m Message
		var err error
		if req.Retry != "" && isGroupKey(strings.TrimSpace(req.To)) {
			m, err = a.retryGroup(strings.TrimPrefix(strings.TrimSpace(req.To), "g:"), req.Retry)
		} else if req.Retry != "" {
			ip := net.ParseIP(strings.TrimSpace(req.To))
			if ip == nil {
				http.Error(w, "bad address", 400)
				return
			}
			m, err = a.retry(ip.String(), req.Retry)
		} else {
			m, err = a.send(strings.TrimSpace(req.To), req.Text, req.Img)
		}
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/api/contact", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Addr   string
			Nick   *string
			TTL    *int
			Accept bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		if k := strings.TrimSpace(req.Addr); isGroupKey(k) {
			a.store.mu.Lock()
			g, ok := a.store.Groups[strings.TrimPrefix(k, "g:")]
			if !ok {
				a.store.mu.Unlock()
				http.Error(w, "no such group", 400)
				return
			}
			if req.Nick != nil && cleanNick(*req.Nick) != "" {
				g.Name = cleanNick(*req.Nick) // local rename only
			}
			if req.Accept {
				g.Req = false
			}
			a.store.Groups[g.ID] = g
			a.store.save()
			a.store.mu.Unlock()
			a.broadcast("contact", groupView(g))
			json.NewEncoder(w).Encode(groupView(g))
			return
		}
		ip := net.ParseIP(strings.TrimSpace(req.Addr))
		if ip == nil || len(ip) != 16 || (ip[0]&0xfe) != 0x02 {
			http.Error(w, "not a Yggdrasil address", 400)
			return
		}
		addr := ip.String()
		a.store.mu.Lock()
		c, ok := a.store.Contacts[addr]
		if !ok {
			c = Contact{Addr: addr} // added by us: not a request
		}
		if req.Nick != nil {
			c.Nick = cleanNick(*req.Nick)
			c.Renamed = ok
		}
		share := req.Accept && (c.Req || !c.Shared)
		if req.Accept {
			c.Req, c.Shared = false, true
		}
		delete(a.store.Blocked, addr)
		a.store.Contacts[addr] = c
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", c)
		if share {
			a.sendProf(addr)
		}
		if req.TTL != nil {
			a.setTTL(addr, *req.TTL, true)
		}
		a.store.mu.Lock()
		c = a.store.Contacts[addr]
		a.store.mu.Unlock()
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("/api/contact/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		if isGroupKey(req.Addr) {
			a.store.mu.Lock()
			a.deleteGroupLocked(strings.TrimPrefix(req.Addr, "g:"))
			a.store.save()
			a.store.mu.Unlock()
			a.broadcast("contact_del", map[string]string{"addr": req.Addr})
			w.Write([]byte("{}"))
			return
		}
		if net.ParseIP(req.Addr) == nil {
			http.Error(w, "bad request", 400)
			return
		}
		addr := net.ParseIP(req.Addr).String()
		a.store.mu.Lock()
		delete(a.store.Contacts, addr)
		kept := a.store.Messages[:0]
		for _, m := range a.store.Messages {
			if m.Peer != addr || m.Hid { // pending group copies to them still count for the group (v0.19.2)
				kept = append(kept, m)
			}
		}
		a.store.Messages = kept
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact_del", map[string]string{"addr": addr})
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct {
				Add, Remove string
				Reset       bool
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			list := append([]string{}, a.cfg.Peers...)
			switch {
			case req.Reset:
				list = append([]string{}, defaultPeers...)
			case req.Add != "":
				add := strings.TrimSpace(req.Add)
				dup := false
				for _, p := range list {
					dup = dup || p == add
				}
				if !dup {
					list = append(list, add)
				}
			case req.Remove != "":
				n := list[:0]
				for _, p := range list {
					if p != req.Remove {
						n = append(n, p)
					}
				}
				list = n
			}
			if err := a.setPeers(list); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"peers": a.peerList(), "recommended": defaultPeers, "up": a.upPeers()})
	})
	mux.HandleFunc("/api/peers/public", func(w http.ResponseWriter, r *http.Request) {
		list, err := publicPeers()
		if err != nil {
			http.Error(w, "public peer list unavailable: "+err.Error(), 502)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"peers": list})
	})
	mux.HandleFunc("/api/nick", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Nick string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		a.store.mu.Lock()
		a.store.Nick = cleanNick(req.Nick)
		a.store.save()
		a.store.mu.Unlock()
		a.profAll()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no stream", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan []byte, 512)
		a.subMu.Lock()
		a.subs[ch] = struct{}{}
		a.subMu.Unlock()
		defer func() { a.subMu.Lock(); delete(a.subs, ch); a.subMu.Unlock() }()
		// Send the headers now: the UI resyncs on "open", which otherwise waits for the first event.
		fmt.Fprint(w, ": ok\n\n")
		fl.Flush()
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case b := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-tick.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
}

func loadConfig(path string) *config.NodeConfig {
	cfg := config.GenerateConfig()
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if _, err := cfg.ReadFrom(f); err != nil {
			log.Fatal("config: ", err)
		}
		return cfg
	}
	cfg.AdminListen = "none"
	cfg.Peers = defaultPeers
	cfg.Listen = []string{}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = writeAtomic(path, b)
	return cfg
}

func main() {
	if runtime.GOOS == "android" {
		// Pure-Go resolver has no /etc/resolv.conf on Android: use public DNS.
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		}}
	}
	home, _ := os.UserHomeDir()
	dataDir := flag.String("data", filepath.Join(home, ".yggchat"), "data directory (keys, contacts, history)")
	uiAddr := flag.String("ui", "127.0.0.1:7780", "local web UI address")
	peers := flag.String("peers", "", "comma-separated Yggdrasil peer URIs (overrides config)")
	listen := flag.String("listen", "", "comma-separated Yggdrasil listen URIs, e.g. tls://0.0.0.0:12345")
	cliMode := flag.Bool("cli", false, "terminal chat (attaches to a node already running on -ui, or starts one)")
	noWindow := flag.Bool("nowindow", false, "Windows: don't open the app window (use a browser on -ui instead)")
	licenses := flag.Bool("licenses", false, "print the license (GPL-3.0-or-later) and third-party notices, then exit")
	flag.Parse()
	if *licenses {
		for _, f := range []string{"web/THIRD_PARTY_NOTICES.txt", "web/LICENSE.txt"} {
			b, _ := webFS.ReadFile(f)
			os.Stdout.Write(b)
			fmt.Println()
		}
		return
	}
	if err := checkUIAddr(*uiAddr); err != nil {
		log.Fatal(err)
	}
	_ = os.MkdirAll(*dataDir, 0700)
	if k, err := loadUIKey(*dataDir); err != nil {
		log.Fatal("ui key: ", err)
	} else {
		uiKey = k
	}
	if *cliMode && apiUp(*uiAddr) {
		runCLI(*uiAddr)
		return
	}
	wantWindow := !*cliMode && !*noWindow && runtime.GOOS == "windows"
	if wantWindow && apiUp(*uiAddr) {
		// Already running (e.g. double-clicked twice): just show it.
		openWindow(uiURL(*uiAddr), *dataDir)
		return
	}
	_ = os.MkdirAll(*dataDir, 0700)
	_ = os.Chmod(*dataDir, 0700)
	for _, f := range []string{"node.json", "chat.json"} {
		_ = os.Chmod(filepath.Join(*dataDir, f), 0600)
	}

	cfg := loadConfig(filepath.Join(*dataDir, "node.json"))
	if *peers != "" {
		cfg.Peers = strings.Split(*peers, ",")
	}
	if *listen != "" {
		cfg.Listen = strings.Split(*listen, ",")
	}

	var logOut io.Writer = os.Stderr
	if *cliMode {
		// Keep the terminal for the chat; node logs go to a file.
		if f, err := os.OpenFile(filepath.Join(*dataDir, "node.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600); err == nil {
			logOut = f
			log.SetOutput(f)
		}
		go func() {
			for !apiUp(*uiAddr) {
				time.Sleep(300 * time.Millisecond)
			}
			runCLI(*uiAddr)
			os.Exit(0)
		}()
	}
	sys := sysModeOn(*dataDir)
	vpn := !sys && vpnModeOn(*dataDir)
	logger := gologme.New(logOut, "[ygg] ", gologme.LstdFlags)
	logger.EnableLevel("error")
	logger.EnableLevel("warn")
	opts := []core.SetupOption{}
	for _, l := range cfg.Listen {
		opts = append(opts, core.ListenAddress(l))
	}
	loadSocks(*dataDir)
	for _, p := range effectivePeers(cfg.Peers) {
		opts = append(opts, core.Peer{URI: p})
	}
	var c *core.Core
	var ns *netstack.YggdrasilNetstack
	if !sys {
		var err error
		if c, err = core.New(cfg.Certificate, logger, opts...); err != nil {
			log.Fatal(err)
		}
		if !vpn {
			if ns, err = netstack.CreateYggdrasilNetstack(c); err != nil {
				log.Fatal(err)
			}
		}
	}

	st := &Store{path: filepath.Join(*dataDir, "chat.json")}
	_, statErr := os.Stat(st.path)
	fresh := os.IsNotExist(statErr)
	st.load()
	if fresh {
		// v0.13.7: new installs start with Nostr 置き配 on (hardened in v0.13.0).
		// Existing users keep whatever they chose.
		st.NostrOn = true
	}
	if !st.locked {
		st.fix()
	} else {
		st.Contacts, st.Blocked = map[string]Contact{}, map[string]bool{}
	}

	a := &App{core: c, ns: ns, sys: sys, vpn: vpn, store: st, subs: map[chan []byte]struct{}{}, acks: map[string]chan struct{}{}, inflight: map[string]bool{}, seen: map[string]bool{}, profAt: map[string]time.Time{}, cfg: cfg, cfgPath: filepath.Join(*dataDir, "node.json"), dataDir: *dataDir}
	// Messages left "sending" by a previous run never got an ack.
	st.requeueAfterRestart()
	go func() {
		last := -1
		for range time.Tick(3 * time.Second) {
			if n := a.upPeers(); n != last {
				if n > 0 && last <= 0 {
					go func() { time.Sleep(1500 * time.Millisecond); a.helloAll() }()
					go func() { time.Sleep(6 * time.Second); a.syncAllGroups() }()
				}
				last = n
				a.broadcast("peers", map[string]int{"up": n})
			}
		}
	}()
	if sys {
		a.addr = detectSysAddr()
		go a.watchSys()
	} else {
		a.addr = c.Address().String()
		if vpn {
			a.vpnFW(*dataDir)
			a.startVPN()
		}
	}
	a.fingerInit(*dataDir)
	a.siteInit(*dataDir)
	a.lanInit(*dataDir, logger)
	go a.serveChat()
	go a.sweep()
	go a.nostrLoop()
	// Offline queue: probe peers we owe messages to every 20 s, and greet every
	// contact every 2 min so they can flush messages queued for us.
	go func() {
		tick := 0
		for range time.Tick(20 * time.Second) {
			tick++
			if a.upPeers() == 0 {
				continue
			}
			if tick%30 == 0 {
				go a.syncAllGroups() // catch up on groups every 10 min
			}
			if tick%6 == 0 {
				a.helloAll()
				continue
			}
			seen := map[string]bool{}
			for _, m := range a.queued("") {
				if !seen[m.Peer] {
					seen[m.Peer] = true
					a.hello(m.Peer)
				}
			}
			a.store.mu.Lock()
			var cp []string
			for _, c := range a.store.Ctl {
				cp = append(cp, c.Peer)
			}
			a.store.mu.Unlock()
			for _, p := range cp {
				if !seen[p] {
					seen[p] = true
					a.hello(p)
				}
			}
		}
	}()

	mux := http.NewServeMux()
	a.httpHandlers(mux)
	a.featureHandlers(mux)
	a.moveHandlers(mux)
	a.nostrHandlers(mux)
	a.groupHandlers(mux)
	a.inviteHandlers(mux)
	a.groupSetHandlers(mux)
	a.powHandlers(mux)
	a.socksHandlers(mux)
	a.siteHandlers(mux)
	a.resolveHandlers(mux)
	a.systemHandlers(mux, *dataDir)
	a.vpnHandlers(mux, *dataDir)
	a.fingerHandlers(mux)
	a.msgEditHandlers(mux)
	a.vpnFWHandlers(mux, *dataDir)
	a.lanHandlers(mux)
	if !*cliMode {
		fmt.Printf("YggChat\n  your address: %s\n  open UI:      %s\n", a.addr, uiURL(*uiAddr))
	}
	ln, err := listenRetry(*uiAddr)
	if err != nil {
		log.Fatal(err)
	}
	handler := localOnly(*uiAddr, keyGuard(*uiAddr, a.lockGuard(mux)))
	if wantWindow {
		go func() { log.Fatal(http.Serve(ln, handler)) }()
		if openWindow(uiURL(*uiAddr), *dataDir) {
			return // window closed: quit
		}
		select {}
	}
	log.Fatal(http.Serve(ln, handler))
}

// lockGuard: while the history is locked only unlock/state/events/peers work.
func (a *App) lockGuard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if strings.HasPrefix(p, "/api/") && a.store.isLocked() {
			switch p {
			case "/api/state", "/api/events", "/api/unlock", "/api/peers", "/api/peers/public":
			default:
				http.Error(w, "locked", 423)
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}
