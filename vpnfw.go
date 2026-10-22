// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Inbound firewall for Android VPN mode. With YggChat as the device's
// Yggdrasil, anything listening on the phone (Termux sshd, a dev server...)
// would be reachable from the whole mesh. The user picks:
//
//	closed (default) - nothing from outside, except YggChat chat (UDP 7777),
//	                   finger (TCP 79) and the profile site (TCP 80) while
//	                   their listeners are up
//	list             - those plus the listed TCP/UDP ports
//	all              - everything (like the official app)
//
// Replies to connections the phone opened itself always pass (tiny conntrack).
// Blocked packets are dropped silently, so the phone looks like an empty host.
package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

type vpnFWCfg struct {
	Mode  string `json:"mode"` // closed | list | all
	Ports []int  `json:"ports,omitempty"`
}

type flowKey struct {
	proto      byte
	remote     [16]byte
	rport, lpt uint16
}

type vpnFW struct {
	mu     sync.Mutex
	path   string
	cfg    vpnFWCfg
	open   map[uint16]bool
	flows  map[flowKey]time.Time
	finger func() bool // our finger listener is up (TCP 79, mapped to 7979)
	site   func() bool // our profile site listener is up (TCP 80, mapped to 8080)
}

func loadVPNFW(dataDir string, finger func() bool) *vpnFW {
	f := &vpnFW{path: filepath.Join(dataDir, "vpn-in.json"), flows: map[flowKey]time.Time{}, finger: finger}
	if b, err := os.ReadFile(f.path); err == nil {
		_ = json.Unmarshal(b, &f.cfg)
	}
	f.setCfg(f.cfg)
	go func() {
		for range time.Tick(time.Minute) {
			f.mu.Lock()
			for k, t := range f.flows {
				if time.Since(t) > 30*time.Minute {
					delete(f.flows, k)
				}
			}
			f.mu.Unlock()
		}
	}()
	return f
}

func (f *vpnFW) setCfg(c vpnFWCfg) {
	if c.Mode != "list" && c.Mode != "all" {
		c.Mode = "closed"
	}
	open := map[uint16]bool{}
	var ports []int
	for _, p := range c.Ports {
		if p > 0 && p < 65536 && !open[uint16(p)] && len(ports) < 64 {
			open[uint16(p)] = true
			ports = append(ports, p)
		}
	}
	sort.Ints(ports)
	c.Ports = ports
	f.mu.Lock()
	f.cfg, f.open = c, open
	f.mu.Unlock()
}

func pktPorts(p []byte) (proto byte, sport, dport uint16, ok bool) {
	if len(p) < 44 || p[0]>>4 != 6 {
		return 0, 0, 0, false
	}
	proto = p[6]
	if proto != 6 && proto != 17 {
		return proto, 0, 0, true
	}
	return proto, uint16(p[40])<<8 | uint16(p[41]), uint16(p[42])<<8 | uint16(p[43]), true
}

// outbound remembers flows the phone starts, so their replies get in.
func (f *vpnFW) outbound(p []byte) {
	proto, sp, dp, ok := pktPorts(p)
	if !ok || (proto != 6 && proto != 17) {
		return
	}
	var k flowKey
	k.proto, k.rport, k.lpt = proto, dp, sp
	copy(k.remote[:], p[24:40])
	f.mu.Lock()
	f.flows[k] = time.Now()
	f.mu.Unlock()
}

// inbound reports whether a packet from the mesh may reach the phone. It sees
// the packet as it arrived, before the 79->7979 / 80->8080 port mapping, so
// only finger/site traffic sent to the real ports gets the finger/site
// exception; a packet aimed straight at 7979 or 8080 is treated like any other
// port (otherwise another app on 8080, say a dev server, would be exposed
// whenever the profile site is on, or when our own listener failed to bind).
func (f *vpnFW) inbound(p []byte) bool {
	f.mu.Lock()
	mode := f.cfg.Mode
	f.mu.Unlock()
	if mode == "all" {
		return true
	}
	if len(p) < 40 || p[0]>>4 != 6 {
		return false
	}
	switch p[6] {
	case 58:
		// ICMPv6: errors (types 0-127, so PMTU/unreachable work) and echo
		// replies pass; echo requests (ping) and other informational messages
		// (node information queries, MLD, ...) don't.
		return len(p) > 40 && (p[40] < 128 || p[40] == 129)
	case 6, 17:
	default:
		// Extension headers (fragments included) are dropped: chat datagrams
		// always fit one unfragmented packet.
		return false
	}
	proto, sp, dp, ok := pktPorts(p)
	if !ok {
		return false
	}
	if proto == 17 && dp == chatPort {
		return true
	}
	if proto == 6 && dp == fingerPort && f.finger != nil && f.finger() {
		return true
	}
	if proto == 6 && dp == sitePort && f.site != nil && f.site() {
		return true
	}
	var k flowKey
	k.proto, k.rport, k.lpt = proto, sp, dp
	copy(k.remote[:], p[8:24])
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.flows[k]; ok {
		f.flows[k] = time.Now()
		return true
	}
	if mode == "list" && f.open[dp] {
		return true
	}
	return false
}

func (a *App) vpnFWHandlers(mux *http.ServeMux, dataDir string) {
	fw := a.vpnFW(dataDir)
	mux.HandleFunc("/api/vpn/in", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var c vpnFWCfg
			if !readJSON(w, r, &c) {
				return
			}
			fw.setCfg(c)
			fw.mu.Lock()
			b, _ := json.Marshal(fw.cfg)
			fw.mu.Unlock()
			if err := writeAtomic(fw.path, b); err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
		}
		fw.mu.Lock()
		res := map[string]any{"mode": fw.cfg.Mode, "ports": fw.cfg.Ports, "vpn": a.vpn}
		fw.mu.Unlock()
		if a.vpnS != nil {
			a.vpnS.mu.Lock()
			res["dropped"] = a.vpnS.drop
			a.vpnS.mu.Unlock()
		}
		if p, _ := res["ports"].([]int); len(p) == 0 {
			res["ports"] = []int{}
		}
		json.NewEncoder(w).Encode(res)
	})
}

var fwOnce sync.Once
var fwInst *vpnFW

func (a *App) vpnFW(dataDir string) *vpnFW {
	fwOnce.Do(func() {
		fwInst = loadVPNFW(dataDir, a.fingerListening)
		fwInst.site = a.siteListening
	})
	return fwInst
}
