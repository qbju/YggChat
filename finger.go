// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// finger over Yggdrasil (RFC 1288, opt-in). When enabled the node answers
// `finger @[addr]` on TCP port 79 of its Yggdrasil address with the nickname
// and a user-written .plan, and answers the same over the chat UDP port
// ({t:"fq"} -> {t:"fr"}) so the PWA and other YggChat nodes can look it up
// without TCP. Nothing else (online state, contacts, keys) is ever disclosed.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const fingerPort = 79
const fingerAltPort = 7979 // VPN mode: the OS can't bind <1024, packets are port-mapped

type fingerCfg struct {
	On   bool   `json:"on"`
	Plan string `json:"plan"`
}

type fingerState struct {
	applyMu sync.Mutex // serializes fingerApply (open/close of the listener)
	mu      sync.Mutex
	path    string
	cfg     fingerCfg
	tcp     string // "" (off: no listener at all) | "listening" | error text
	ln      net.Listener
	waitMu  sync.Mutex
	wait    map[string]chan wire
	sem     chan struct{}
	busy    map[string]int // open connections per peer (fingerPeerKey)
}

// fingerPerPeer caps one peer's share of the 8 slots, so a single node
// holding connections open for the 10 s deadline can't lock everyone else
// out (v0.19.3).
const fingerPerPeer = 2

// fingerPeerKey groups a client by its Yggdrasil node: 200::/8 addresses are
// one per key, 300::/8 hand each key a /64.
func fingerPeerKey(ra net.Addr) string {
	ta, _ := ra.(*net.TCPAddr)
	if ta == nil || len(ta.IP) != 16 {
		return ""
	}
	ip := ta.IP
	if ip[0] == 0x03 {
		return net.IP(append(append([]byte{}, ip[:8]...), make([]byte, 8)...)).String()
	}
	return ip.String()
}

// fingerTake claims a slot for peer k; fingerDone gives it back.
func (f *fingerState) fingerTake(k string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.busy == nil {
		f.busy = map[string]int{}
	}
	if f.busy[k] >= fingerPerPeer {
		return false
	}
	select {
	case f.sem <- struct{}{}:
	default:
		return false
	}
	f.busy[k]++
	return true
}

func (f *fingerState) fingerDone(k string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	<-f.sem
	if f.busy[k]--; f.busy[k] <= 0 {
		delete(f.busy, k)
	}
}

func cleanPlan(s string, max int) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		if r == '\n' || r == '\t' || (r >= 0x20 && r != 0x7f && !(r >= 0x80 && r < 0xa0)) {
			b.WriteRune(r)
		}
	}
	out := b.String()
	for len(out) > max {
		_, n := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-n]
	}
	return out
}

func (a *App) fingerInit(dataDir string) {
	f := &fingerState{path: filepath.Join(dataDir, "finger.json"), wait: map[string]chan wire{}, sem: make(chan struct{}, 8)}
	if b, err := os.ReadFile(f.path); err == nil {
		_ = json.Unmarshal(b, &f.cfg)
	}
	a.fing = f
	a.fingerApply()
}

// fingerApply opens the TCP listener only while finger is on. When off, port
// 79 is simply closed (same RST as any unused port), so nothing reveals that
// this address runs YggChat; UDP fq is ignored too.
func (a *App) fingerApply() {
	// Without this, an "off" racing a slow "on" could leave a listener open
	// while finger is off (the off saw no listener yet).
	a.fing.applyMu.Lock()
	defer a.fing.applyMu.Unlock()
	a.fing.mu.Lock()
	on, ln := a.fing.cfg.On, a.fing.ln
	if !on && ln != nil {
		a.fing.ln, a.fing.tcp = nil, ""
	}
	a.fing.mu.Unlock()
	if !on {
		if ln != nil {
			ln.Close()
		}
		return
	}
	if ln == nil {
		a.fingerListen()
	}
}

// fingerListening reports whether finger is on and its TCP listener is up
// (the VPN firewall and port mapping only let TCP 79 in then).
func (a *App) fingerListening() bool {
	if a.fing == nil {
		return false
	}
	a.fing.mu.Lock()
	defer a.fing.mu.Unlock()
	return a.fing.cfg.On && a.fing.ln != nil
}

func (a *App) fingerConf() fingerCfg {
	a.fing.mu.Lock()
	defer a.fing.mu.Unlock()
	return a.fing.cfg
}

func (a *App) fingerListen() {
	var ln net.Listener
	var err error
	switch {
	case a.vpn:
		// The TUN pump rewrites port 79 <-> 7979 for our address.
		ln, err = net.Listen("tcp6", net.JoinHostPort("::", "7979"))
	case a.sys:
		ln, err = net.Listen("tcp6", net.JoinHostPort("::", "79"))
	default:
		ln, err = a.ns.ListenTCP(&net.TCPAddr{IP: net.ParseIP(a.addr), Port: fingerPort})
	}
	if err == nil && (a.vpn || a.sys) {
		ln = yggOnlyListener{ln} // the OS listener also sees LAN/Internet clients
	}
	a.fing.mu.Lock()
	if err != nil {
		a.fing.tcp = err.Error()
	} else {
		a.fing.tcp, a.fing.ln = "listening", ln
	}
	a.fing.mu.Unlock()
	if err != nil {
		log.Println("finger: listen:", err)
		return
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				a.fing.mu.Lock()
				closed := a.fing.ln != ln
				a.fing.mu.Unlock()
				if closed {
					return
				}
				time.Sleep(200 * time.Millisecond)
				continue
			}
			k := fingerPeerKey(c.RemoteAddr())
			if !a.fing.fingerTake(k) {
				c.Close()
				continue
			}
			go func() { a.fingerServe(c); a.fing.fingerDone(k) }()
		}
	}()
}

func isYggIP(ip net.IP) bool { return len(ip) == 16 && ip[0]&0xfe == 0x02 }

func (a *App) fingerServe(c net.Conn) {
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	ta, _ := c.RemoteAddr().(*net.TCPAddr)
	if ta == nil || !isYggIP(ta.IP) {
		return // OS-stack listeners also see non-Yggdrasil traffic
	}
	cf := a.fingerConf()
	if !cf.On {
		return
	}
	a.store.mu.Lock()
	blocked := a.store.Blocked[ta.IP.String()]
	a.store.mu.Unlock()
	if blocked {
		return
	}
	line, _ := bufio.NewReader(io.LimitReader(c, fingerMaxLine)).ReadString('\n')
	if _, fwd := fingerQuery(line); fwd {
		io.WriteString(c, "finger: forwarding refused\r\n")
		return
	}
	io.WriteString(c, strings.ReplaceAll(a.fingerText(cf), "\n", "\r\n"))
}

const fingerMaxLine = 256

// fingerQuery parses an RFC 1288 query line ({Q1} "[/W] [user]" or {Q2}
// "[/W] [user]@host..."). Every user gets the same card, so only whether it
// asks us to forward (contains "@", refused per RFC 1288 2.5.5) matters.
func fingerQuery(line string) (user string, forward bool) {
	q := strings.TrimSpace(line)
	if strings.HasPrefix(q, "/W") || strings.HasPrefix(q, "/w") {
		q = strings.TrimSpace(q[2:])
	}
	if strings.Contains(q, "@") {
		return "", true
	}
	return q, false
}

func (a *App) fingerNick() string {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.locked || a.store.Nick == "" {
		return "anon"
	}
	return a.store.Nick
}

func (a *App) fingerText(cf fingerCfg) string {
	var b strings.Builder
	b.WriteString("Login: " + a.fingerNick() + "\t\t\t(YggChat)\n")
	b.WriteString("Address: " + a.myAddr() + "\n")
	if cf.Plan == "" {
		b.WriteString("No Plan.\n")
	} else {
		b.WriteString("Plan:\n" + cf.Plan)
		if !strings.HasSuffix(cf.Plan, "\n") {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// onFingerUDP handles {t:"fq"} (answer) and {t:"fr"} (a reply we asked for).
func (a *App) onFingerUDP(from string, w wire) {
	if w.T == "fr" {
		a.fing.waitMu.Lock()
		if ch, ok := a.fing.wait[from]; ok {
			select {
			case ch <- w:
			default:
			}
		}
		a.fing.waitMu.Unlock()
		return
	}
	cf := a.fingerConf()
	if !cf.On {
		return
	}
	// Each datagram must fit the 1280-byte IPv6 MTU unfragmented.
	a.sendWire(from, wire{T: "fr", Nick: a.fingerNick(), Text: cleanPlan(cf.Plan, 700)})
}

type fingerResult struct {
	Addr string `json:"addr"`
	Via  string `json:"via"` // yggchat | finger
	Nick string `json:"nick,omitempty"`
	Plan string `json:"plan,omitempty"`
	Raw  string `json:"raw,omitempty"`
}

// fingerUDP asks a YggChat node for its finger card.
func (a *App) fingerUDP(ctx context.Context, addr string) (wire, error) {
	ch := make(chan wire, 1)
	a.fing.waitMu.Lock()
	a.fing.wait[addr] = ch
	a.fing.waitMu.Unlock()
	defer func() {
		a.fing.waitMu.Lock()
		if a.fing.wait[addr] == ch {
			delete(a.fing.wait, addr)
		}
		a.fing.waitMu.Unlock()
	}()
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	a.sendWire(addr, wire{T: "fq"})
	for {
		select {
		case w := <-ch:
			return w, nil
		case <-t.C:
			a.sendWire(addr, wire{T: "fq"})
		case <-ctx.Done():
			return wire{}, ctx.Err()
		}
	}
}

// fingerTCP runs a classic finger query against any Yggdrasil host.
func (a *App) fingerTCP(ctx context.Context, addr, user string) (string, error) {
	ta := &net.TCPAddr{IP: net.ParseIP(addr), Port: fingerPort}
	var c net.Conn
	var err error
	if a.sys || a.vpn {
		var d net.Dialer
		c, err = d.DialContext(ctx, "tcp6", ta.String())
	} else {
		c, err = a.ns.DialContext(ctx, "tcp", ta.String())
	}
	if err != nil {
		return "", err
	}
	defer c.Close()
	dl, _ := ctx.Deadline()
	_ = c.SetDeadline(dl)
	if _, err = io.WriteString(c, user+"\r\n"); err != nil {
		return "", err
	}
	b, err := io.ReadAll(io.LimitReader(c, 16<<10))
	if len(b) == 0 {
		if err == nil {
			err = errors.New("empty reply (finger off?)")
		}
		return "", err
	}
	return cleanPlan(string(b), 16<<10), nil
}

func (a *App) fingerLookup(addr, user string) (fingerResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	res := fingerResult{Addr: addr}
	type r struct {
		w   *wire
		raw string
		err error
	}
	out := make(chan r, 2)
	if user == "" {
		go func() { w, err := a.fingerUDP(ctx, addr); out <- r{w: &w, err: err} }()
	} else {
		out <- r{err: errors.New("skip")}
	}
	go func() { s, err := a.fingerTCP(ctx, addr, user); out <- r{raw: s, err: err} }()
	var lastErr error
	for i := 0; i < 2; i++ {
		x := <-out
		if x.err != nil {
			if x.err.Error() != "skip" {
				lastErr = x.err
			}
			continue
		}
		if x.w != nil {
			res.Via, res.Nick, res.Plan = "yggchat", cleanNick(x.w.Nick), cleanPlan(x.w.Text, 700)
			return res, nil
		}
		res.Via, res.Raw = "finger", x.raw
		return res, nil
	}
	if lastErr == nil {
		lastErr = errors.New("no answer")
	}
	return res, lastErr
}

func (a *App) fingerHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/finger", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct {
				On   bool
				Plan string
			}
			if !readJSON(w, r, &req) {
				return
			}
			a.fing.mu.Lock()
			a.fing.cfg = fingerCfg{On: req.On, Plan: cleanPlan(req.Plan, 1500)}
			b, _ := json.Marshal(a.fing.cfg)
			err := writeAtomic(a.fing.path, b)
			a.fing.mu.Unlock()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			a.fingerApply()
		}
		a.fing.mu.Lock()
		res := map[string]any{"on": a.fing.cfg.On, "plan": a.fing.cfg.Plan, "tcp": a.fing.tcp, "addr": a.myAddr()}
		a.fing.mu.Unlock()
		json.NewEncoder(w).Encode(res)
	})
	// POST {"Addr":"200:…","User":""} — look up any Yggdrasil host.
	mux.HandleFunc("/api/finger/query", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr, User string }
		if !readJSON(w, r, &req) {
			return
		}
		ip := net.ParseIP(strings.Trim(strings.TrimSpace(req.Addr), "[]"))
		if !isYggIP(ip) {
			http.Error(w, "not a Yggdrasil address (200::/7)", 400)
			return
		}
		user := strings.TrimSpace(req.User)
		if len(user) > 64 || strings.ContainsAny(user, "@\r\n") {
			http.Error(w, "bad user", 400)
			return
		}
		res, err := a.fingerLookup(ip.String(), user)
		if err != nil {
			http.Error(w, "応答がなかったよ（相手のfingerがオフか、いまつながっていない）: "+err.Error(), 504)
			return
		}
		json.NewEncoder(w).Encode(res)
	})
}
