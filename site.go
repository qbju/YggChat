// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Profile site (v0.13.6, opt-in, off by default): one HTML page you write in
// the app, served over plain HTTP on port 80 of your Yggdrasil address
// (http://[200:...]/). Like finger, when it is off there is no listener at
// all. Only GET/HEAD of "/" (and "/index.html") are answered; the page is a
// static file (data/site.html) and the server shares nothing else.
//
// The app can also open other people's pages through its own Yggdrasil node
// (no system Yggdrasil needed). They are shown in a sandboxed iframe with
// scripts, forms and network loads disabled, so a page can't reach the app.
package main

import (
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
)

const sitePort = 80
const siteAltPort = 8080 // VPN mode: the OS can't bind <1024, packets are port-mapped
const siteMax = 256 << 10

type siteState struct {
	applyMu sync.Mutex // serializes siteApply
	mu      sync.Mutex
	dir     string
	on      bool
	html    string
	srv     *http.Server
	err     string
}

func (a *App) siteInit(dataDir string) {
	s := &siteState{dir: dataDir}
	if b, err := os.ReadFile(filepath.Join(dataDir, "site.json")); err == nil {
		var c struct{ On bool }
		_ = json.Unmarshal(b, &c)
		s.on = c.On
	}
	if b, err := os.ReadFile(filepath.Join(dataDir, "site.html")); err == nil && len(b) <= siteMax {
		s.html = string(b)
	}
	a.site = s
	a.siteApply()
}

func (a *App) siteOn() bool {
	if a.site == nil {
		return false
	}
	a.site.mu.Lock()
	defer a.site.mu.Unlock()
	return a.site.on
}

// siteListening reports whether the site is on and its listener is up.
func (a *App) siteListening() bool {
	if a.site == nil {
		return false
	}
	a.site.mu.Lock()
	defer a.site.mu.Unlock()
	return a.site.on && a.site.srv != nil
}

// yggOnlyListener drops connections that don't come from a Yggdrasil address
// (200::/7). Used for listeners on the OS stack (system / VPN mode).
type yggOnlyListener struct{ net.Listener }

func (l yggOnlyListener) Accept() (net.Conn, error) {
	for {
		c, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok && isYggIP(ta.IP) {
			return c, nil
		}
		c.Close()
	}
}

// siteApply starts or stops the listener to match the setting.
func (a *App) siteApply() {
	s := a.site
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.Lock()
	on, srv := s.on, s.srv
	if !on && srv != nil {
		s.srv, s.err = nil, ""
	}
	s.mu.Unlock()
	if !on {
		if srv != nil {
			srv.Close()
		}
		return
	}
	if srv != nil {
		return
	}
	var ln net.Listener
	var err error
	switch {
	case a.vpn:
		ln, err = net.Listen("tcp6", net.JoinHostPort("::", "8080"))
	case a.sys:
		ln, err = net.Listen("tcp6", net.JoinHostPort("::", "80"))
	default:
		ln, err = a.ns.ListenTCP(&net.TCPAddr{IP: net.ParseIP(a.addr), Port: sitePort})
	}
	if err != nil {
		s.mu.Lock()
		s.err = err.Error()
		s.mu.Unlock()
		log.Println("site: listen:", err)
		return
	}
	if a.vpn || a.sys {
		// Bound to :: on the OS stack, so Wi-Fi/LAN (and, in system mode, any
		// public IPv6) clients could reach the page too: Yggdrasil peers only.
		ln = yggOnlyListener{ln}
	}
	srv = &http.Server{Handler: http.HandlerFunc(a.siteServe), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	s.mu.Lock()
	s.srv, s.err = srv, ""
	s.mu.Unlock()
	go func() { _ = srv.Serve(ln) }()
}

func (a *App) siteServe(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Server", "yggchat")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	if r.Method != "GET" && r.Method != "HEAD" {
		http.Error(w, "method not allowed", 405)
		return
	}
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	a.site.mu.Lock()
	page := a.site.html
	a.site.mu.Unlock()
	if strings.TrimSpace(page) == "" {
		page = "<!doctype html><meta charset=utf-8><title>YggChat</title><p>まだ何もありません。"
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	io.WriteString(w, page)
}

// siteFetch loads someone's page through our own node.
func (a *App) siteFetch(addr string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tr := &http.Transport{DialContext: func(ctx context.Context, _, hostport string) (net.Conn, error) {
		if a.sys || a.vpn {
			var d net.Dialer
			return d.DialContext(ctx, "tcp6", hostport)
		}
		return a.ns.DialContext(ctx, "tcp", hostport)
	}, Proxy: nil, DisableKeepAlives: true}
	cl := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://["+addr+"]/", nil)
	req.Header.Set("User-Agent", "YggChat")
	r, err := cl.Do(req)
	if err != nil {
		return "", errors.New("つながりませんでした（相手のサイトがオフか、いまオフライン）")
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		return "", errors.New("サイトがありません（HTTP " + r.Status + "）")
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, siteMax+1))
	if err != nil {
		return "", err
	}
	if len(b) > siteMax {
		return "", errors.New("ページが大きすぎます（256KBまで）")
	}
	return string(b), nil
}

func (a *App) siteHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/site", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On   *bool
			HTML *string
		}
		if !readJSON(w, r, &req) {
			return
		}
		s := a.site
		s.mu.Lock()
		if req.HTML != nil {
			if len(*req.HTML) > siteMax {
				s.mu.Unlock()
				http.Error(w, "ページが大きすぎます（256KBまで）", 400)
				return
			}
			s.html = *req.HTML
			_ = writeAtomic(filepath.Join(s.dir, "site.html"), []byte(s.html))
		}
		if req.On != nil {
			s.on = *req.On
			b, _ := json.Marshal(map[string]bool{"On": s.on})
			_ = writeAtomic(filepath.Join(s.dir, "site.json"), b)
		}
		s.mu.Unlock()
		if req.On != nil {
			a.siteApply()
		}
		s.mu.Lock()
		res := map[string]any{"on": s.on, "html": s.html, "url": "http://[" + a.addr + "]/", "listening": s.srv != nil, "error": s.err}
		s.mu.Unlock()
		json.NewEncoder(w).Encode(res)
	})
	mux.HandleFunc("/api/site/view", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr string }
		if !readJSON(w, r, &req) {
			return
		}
		ad, ok := yggAddr(req.Addr)
		if !ok {
			http.Error(w, "not a Yggdrasil address", 400)
			return
		}
		html, err := a.siteFetch(ad)
		if err != nil {
			http.Error(w, err.Error(), 504)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"addr": ad, "html": html})
	})
}
