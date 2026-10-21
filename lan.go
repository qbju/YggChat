// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// LAN chat (opt-in): Yggdrasil multicast discovery on the local network.
// Nodes on the same Wi-Fi / Ethernet / tethering find each other and peer
// directly over link-local addresses, so chat keeps working with no Internet
// and no public peers. An optional LAN password (Yggdrasil's multicast
// password) limits peering to nodes that know it. Nearby nodes swap only their
// nickname ({t:"nq"} -> {t:"nr"}), and only with direct link-local peers.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggdrasil-go/src/multicast"
)

type lanCfg struct {
	On   bool   `json:"on"`
	Pass string `json:"pass,omitempty"`
}

type lanState struct {
	mu    sync.Mutex
	path  string
	cfg   lanCfg
	mc    *multicast.Multicast
	err   string
	nicks map[string]string // addr -> nick from {t:"nr"}
	asked map[string]time.Time
	log   core.Logger
}

func (a *App) lanInit(dataDir string, logger core.Logger) {
	l := &lanState{path: filepath.Join(dataDir, "lan.json"), nicks: map[string]string{}, asked: map[string]time.Time{}, log: logger}
	if b, err := os.ReadFile(l.path); err == nil {
		_ = json.Unmarshal(b, &l.cfg)
	}
	a.lan = l
	if a.core == nil {
		return // system mode: the device's Yggdrasil does its own multicast
	}
	if l.cfg.On {
		a.lanApply()
	}
	go func() {
		for range time.Tick(5 * time.Second) {
			a.lanAsk()
		}
	}()
}

// lanApply (re)starts or stops the multicast module to match the config.
func (a *App) lanApply() {
	l := a.lan
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.mc != nil {
		_ = l.mc.Stop()
		l.mc = nil
	}
	l.err = ""
	if !l.cfg.On || a.core == nil {
		return
	}
	mc, err := multicast.New(a.core, l.log, multicast.MulticastInterface{
		Regex:    regexp.MustCompile(".*"),
		Beacon:   true,
		Listen:   true,
		Password: l.cfg.Pass,
	})
	if err != nil {
		l.err = err.Error()
		return
	}
	l.mc = mc
}

// cleanLANPass trims the LAN password to Yggdrasil's 64-byte limit (it is a
// BLAKE2b key) without cutting a character in half: a split UTF-8 sequence was
// saved to lan.json as U+FFFD, so after a restart the password silently
// differed from the one other devices typed and LAN peering stopped.
func cleanLANPass(s string) string {
	s = strings.ToValidUTF8(strings.TrimSpace(s), "")
	for len(s) > 64 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}

func isLANPeer(p core.PeerInfo) bool {
	return strings.Contains(p.URI, "[fe80") || strings.Contains(strings.ToLower(p.URI), "%25")
}

type nearby struct {
	Addr    string `json:"addr"`
	Nick    string `json:"nick,omitempty"`
	Up      bool   `json:"up"`
	Iface   string `json:"iface,omitempty"`
	Latency int64  `json:"ms,omitempty"`
	Contact bool   `json:"contact,omitempty"`
}

func (a *App) lanNearby() []nearby {
	if a.core == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []nearby
	for _, p := range a.core.GetPeers() {
		if !isLANPeer(p) || len(p.Key) == 0 {
			continue
		}
		addr := net.IP(address.AddrForKey(p.Key)[:]).String()
		if seen[addr] {
			continue
		}
		seen[addr] = true
		n := nearby{Addr: addr, Up: p.Up, Latency: p.Latency.Milliseconds()}
		if i := strings.Index(p.URI, "%25"); i >= 0 {
			rest := p.URI[i+3:]
			if j := strings.IndexByte(rest, ']'); j >= 0 {
				n.Iface = rest[:j]
			}
		}
		a.lan.mu.Lock()
		n.Nick = a.lan.nicks[addr]
		a.lan.mu.Unlock()
		a.store.mu.Lock()
		if c, ok := a.store.Contacts[addr]; ok {
			n.Contact = true
			if c.Nick != "" {
				n.Nick = c.Nick
			}
		}
		a.store.mu.Unlock()
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Addr < out[j].Addr })
	return out
}

func (a *App) isLANNeighbour(addr string) bool {
	for _, n := range a.lanNearby() {
		if n.Addr == addr && n.Up {
			return true
		}
	}
	return false
}

// lanAsk asks LAN neighbours for their nickname.
func (a *App) lanAsk() {
	if !a.lanOn() {
		return
	}
	changed := false
	for _, n := range a.lanNearby() {
		if !n.Up {
			continue
		}
		a.lan.mu.Lock()
		// The first datagrams to a new node can be lost while Yggdrasil finds the
		// path, so keep asking every few seconds until we get a name.
		since := time.Since(a.lan.asked[n.Addr])
		due := since > 4*time.Second && (a.lan.nicks[n.Addr] == "" || since > 5*time.Minute)
		if due {
			a.lan.asked[n.Addr] = time.Now()
		}
		a.lan.mu.Unlock()
		if due {
			a.sendWire(n.Addr, wire{T: "nq"})
			changed = true
		}
	}
	if changed {
		go func() { time.Sleep(2 * time.Second); a.broadcast("lan", map[string]any{"nearby": a.lanNearby()}) }()
	}
}

func (a *App) lanOn() bool {
	if a.lan == nil {
		return false
	}
	a.lan.mu.Lock()
	defer a.lan.mu.Unlock()
	return a.lan.cfg.On && a.lan.mc != nil
}

// onLANUDP: nickname exchange, only between direct LAN neighbours.
func (a *App) onLANUDP(from string, w wire) {
	if !a.lanOn() || !a.isLANNeighbour(from) {
		return
	}
	switch w.T {
	case "nq":
		a.sendWire(from, wire{T: "nr", Nick: a.fingerNick()})
	case "nr":
		if n := cleanNick(w.Nick); n != "" {
			a.lan.mu.Lock()
			old := a.lan.nicks[from]
			a.lan.nicks[from] = n
			a.lan.mu.Unlock()
			if old != n {
				a.broadcast("lan", map[string]any{"nearby": a.lanNearby()})
			}
		}
	}
}

func (a *App) lanHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/lan", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct {
				On   bool
				Pass *string
			}
			if !readJSON(w, r, &req) {
				return
			}
			if a.core == nil {
				http.Error(w, "システムYggdrasilモードでは端末側のYggdrasilの設定を使ってね", 400)
				return
			}
			a.lan.mu.Lock()
			a.lan.cfg.On = req.On
			if req.Pass != nil {
				a.lan.cfg.Pass = cleanLANPass(*req.Pass)
			}
			b, _ := json.Marshal(a.lan.cfg)
			err := writeAtomic(a.lan.path, b)
			a.lan.mu.Unlock()
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			a.lanApply()
		}
		a.lan.mu.Lock()
		res := map[string]any{"supported": a.core != nil, "on": a.lan.cfg.On, "pass": a.lan.cfg.Pass, "err": a.lan.err}
		var ifs []string
		if a.lan.mc != nil {
			for name := range a.lan.mc.Interfaces() {
				ifs = append(ifs, name)
			}
		}
		a.lan.mu.Unlock()
		sort.Strings(ifs)
		res["ifaces"] = ifs
		res["nearby"] = a.lanNearby()
		json.NewEncoder(w).Encode(res)
	})
}
