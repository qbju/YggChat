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
	mu     sync.Mutex
	path   string
	cfg    fingerCfg
	tcp    string // "" | "listening" | error text
	waitMu sync.Mutex
	wait   map[string]chan wire
	sem    chan struct{}
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
	go a.fingerListen()
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
	a.fing.mu.Lock()
	if err != nil {
		a.fing.tcp = err.Error()
	} else {
		a.fing.tcp = "listening"
	}
	a.fing.mu.Unlock()
	if err != nil {
		log.Println("finger: listen:", err)
		return
	}
	for {
		c, err := ln.Accept()
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		select {
		case a.fing.sem <- struct{}{}:
			go func() { a.fingerServe(c); <-a.fing.sem }()
		default:
			c.Close()
		}
	}
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
	line, _ := bufio.NewReader(io.LimitReader(c, 256)).ReadString('\n')
	q := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "/W"))
	if strings.Contains(q, "@") {
		io.WriteString(c, "finger: forwarding refused\r\n")
		return
	}
	io.WriteString(c, strings.ReplaceAll(a.fingerText(cf), "\n", "\r\n"))
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
