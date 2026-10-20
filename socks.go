// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Optional SOCKS5 proxy (v0.13.5, provisional), meant for Tor (Orbot on
// Android: 127.0.0.1:9050, Tor Browser: 127.0.0.1:9150).
//
// When on, every connection YggChat itself makes to the Internet goes through
// the proxy: Yggdrasil peers (tcp:// becomes socks://, tls:// becomes
// sockstls://), Nostr relays and the public peer list. It fails closed: peers
// that cannot go through SOCKS (quic://, ws://, wss://) are not used, and when
// the proxy is down nothing connects directly instead. Host names are passed
// to the proxy unresolved. Not covered: incoming listeners you configured
// yourself, LAN chat (local network only) and system/VPN Yggdrasil modes.
// Stored in data/socks.json (not secret); changing it restarts the node.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/proxy"
)

type socksCfg struct {
	On   bool   `json:"on"`
	Addr string `json:"addr"`
}

var socks socksCfg // loaded once at start; changes restart the process

const socksDefault = "127.0.0.1:9050"

func socksPath(dir string) string { return filepath.Join(dir, "socks.json") }

func loadSocks(dir string) {
	b, err := os.ReadFile(socksPath(dir))
	if err == nil {
		_ = json.Unmarshal(b, &socks)
	}
	if socks.Addr == "" {
		socks.Addr = socksDefault
	}
}

func validSocksAddr(s string) bool {
	h, p, err := net.SplitHostPort(s)
	return err == nil && h != "" && p != "" && len(s) <= 100 && !strings.ContainsAny(s, "/?#@ ")
}

// peerViaSocks rewrites a peer URI to go through the proxy, or reports false
// when it cannot (those peers are skipped while SOCKS is on).
func peerViaSocks(p string) (string, bool) {
	u, err := url.Parse(p)
	if err != nil || u.Host == "" {
		return "", false
	}
	scheme := ""
	switch u.Scheme {
	case "tcp":
		scheme = "socks"
	case "tls":
		scheme = "sockstls"
	case "socks", "sockstls":
		return p, true // already explicit
	default:
		return "", false
	}
	out := scheme + "://" + socks.Addr + "/" + u.Host
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	return out, true
}

// effectivePeers is what the node really dials for the configured list.
func effectivePeers(list []string) []string {
	if !socks.On {
		return list
	}
	out := []string{}
	for _, p := range list {
		if q, ok := peerViaSocks(p); ok {
			out = append(out, q)
		}
	}
	return out
}

// netDial is the dialer for YggChat's own Internet connections.
func netDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if !socks.On {
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}
	d, err := proxy.SOCKS5("tcp", socks.Addr, nil, &net.Dialer{Timeout: 20 * time.Second})
	if err != nil {
		return nil, err
	}
	if cd, ok := d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return d.Dial(network, addr)
}

// netHTTP is an HTTP client that honours the proxy setting.
func netHTTP(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, Transport: &http.Transport{DialContext: netDial, Proxy: nil, ForceAttemptHTTP2: true, TLSHandshakeTimeout: 20 * time.Second}}
}

func (a *App) socksState() map[string]any {
	return map[string]any{"on": socks.On, "addr": socks.Addr, "default": socksDefault, "available": !a.sys && !a.vpn}
}

func (a *App) socksHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/socks", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On   *bool
			Addr *string
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.On == nil && req.Addr == nil {
			json.NewEncoder(w).Encode(a.socksState())
			return
		}
		if a.sys || a.vpn {
			http.Error(w, "端末のYggdrasil／VPNモードでは使えません", 400)
			return
		}
		n := socks
		if req.Addr != nil {
			ad := strings.TrimSpace(*req.Addr)
			if ad == "" {
				ad = socksDefault
			}
			if !validSocksAddr(ad) {
				http.Error(w, "ホスト:ポート の形で入力してください（例 127.0.0.1:9050）", 400)
				return
			}
			n.Addr = ad
		}
		if req.On != nil {
			n.On = *req.On
		}
		b, _ := json.Marshal(n)
		if err := writeAtomic(socksPath(a.dataDir), b); err != nil {
			http.Error(w, fmt.Sprint("save: ", err), 500)
			return
		}
		changed := n != socks
		socks = n
		res := a.socksState()
		res["restart"] = changed
		json.NewEncoder(w).Encode(res)
		if changed {
			go func() { time.Sleep(500 * time.Millisecond); restartSelf() }()
		}
	})
}
