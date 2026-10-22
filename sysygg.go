// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// System Yggdrasil mode: instead of the embedded node, use a Yggdrasil that is
// already running on the device as a VPN/TUN (yggdrasil-android, the desktop
// yggdrasil service). The chat identity is then that node's address; YggChat
// holds no Yggdrasil key, so the full public key (and Nostr boxes) is unavailable.
package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

func sysModeFile(dataDir string) string { return filepath.Join(dataDir, "system-ygg") }

func sysModeOn(dataDir string) bool {
	if os.Getenv("YGGCHAT_SYSTEM") == "1" {
		return true
	}
	_, err := os.Stat(sysModeFile(dataDir))
	return err == nil
}

// detectSysAddr asks the kernel which source address it would use to reach
// 200::/7. That works without listing interfaces (Android 11+ forbids that).
func detectSysAddr() string {
	c, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: net.ParseIP("200::1"), Port: chatPort})
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if len(ip) == 16 && (ip[0]&0xfe) == 0x02 {
		return ip.String()
	}
	return ""
}

func (a *App) myAddr() string { return a.addr }

// sysUp: the device's Yggdrasil currently routes 200::/7 (system mode).
var sysUp atomic.Bool

// watchSys follows the system node (its VPN may start later or stop).
// a.addr is read everywhere without a lock, so it is never rewritten while
// the node runs: when the system node comes up with an address other than
// the one we started with (it wasn't up yet, or its key changed), we restart
// and start again with that one (v0.19.3; it was a data race before).
func (a *App) watchSys() {
	sysUp.Store(a.addr != "")
	pending := ""
	for {
		time.Sleep(5 * time.Second)
		ad := detectSysAddr()
		up := ad != "" && ad == a.addr
		if up != sysUp.Load() {
			sysUp.Store(up)
			n := 0
			if up {
				n = 1
				go a.helloAll()
			}
			a.broadcast("peers", map[string]any{"up": n, "total": 1})
		}
		switch {
		case ad == "" || ad == a.addr:
			pending = ""
		case ad != pending:
			pending = ad // seen once: restart only if it holds on the next look
		default:
			log.Printf("system Yggdrasil address is now %s (was %q): restarting", ad, a.addr)
			restartSelf()
		}
	}
}

func (a *App) pub() []byte {
	if a.sys || a.core == nil { // nil core: unit-test Apps (a late probeSoon hello panicked the test binary)
		return nil
	}
	return a.core.PublicKey()
}

func (a *App) systemHandlers(mux *http.ServeMux, dataDir string) {
	mux.HandleFunc("/api/system", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct{ On bool }
			if !readJSON(w, r, &req) {
				return
			}
			if req.On {
				_ = os.Remove(vpnModeFile(dataDir))
				if err := writeAtomic(sysModeFile(dataDir), []byte("1\n")); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else {
				_ = os.Remove(sysModeFile(dataDir))
			}
			json.NewEncoder(w).Encode(map[string]any{"on": req.On, "restart": true})
			go func() {
				time.Sleep(300 * time.Millisecond)
				restartSelf()
			}()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"on": a.sys, "detected": detectSysAddr()})
	})
}
