// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Inbound firewall for Android VPN mode. With YggChat as the device's
// Yggdrasil, anything listening on the phone (Termux sshd, a dev server...)
// would be reachable from the whole mesh. The user picks:
//
//	closed (default) - nothing from outside, except YggChat chat (UDP 7777)
//	                   and finger while finger is on
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
	finger func() bool
	site   func() bool
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

// inbound reports whether a packet from the mesh may reach the phone.
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
	case 58: // ICMPv6: errors and echo replies pass, echo requests (ping) don't
		return len(p) > 40 && p[40] != 128
	case 6, 17:
	default:
		return false
	}
	proto, sp, dp, ok := pktPorts(p)
	if !ok {
		return false
	}
	if proto == 17 && dp == chatPort {
		return true
	}
	if proto == 6 && dp == fingerAltPort && f.finger != nil && f.finger() {
		return true
	}
	if proto == 6 && dp == siteAltPort && f.site != nil && f.site() {
		return true
	}
	var k flowKey
	k.proto, k.rport, k.lpt = proto, sp, dp
	copy(k.remote[:], p[8:24])
	f.mu.Lock()
	defer f.mu.Unlock()
	if t, ok := f.flows[k]; ok {
		f.flows[k] = time.Now()
		_ = t
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
		fwInst = loadVPNFW(dataDir, func() bool { return a.fing != nil && a.fingerConf().On })
		fwInst.site = a.siteOn
	})
	return fwInst
}
