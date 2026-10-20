// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Add a contact by domain name (v0.13.8): if a domain's AAAA record points to
// a Yggdrasil address (200::/7), that address is the contact. Lookups go over
// DNS-over-HTTPS through the same dialer as everything else, so with SOCKS on
// they go through Tor too (no fallback to the system resolver then).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var domainRE = regexp.MustCompile(`^(?i:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+(?i:[a-z0-9-]{2,63})$`)

var dohServers = []string{"https://cloudflare-dns.com/dns-query", "https://dns.google/resolve"}

func isYgg6(ip net.IP) bool {
	ip = ip.To16()
	return ip != nil && ip.To4() == nil && ip[0]&0xfe == 0x02
}

// resolveYgg returns the first Yggdrasil address among name's AAAA records.
func resolveYgg(name string) (string, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if len(name) > 253 || !domainRE.MatchString(name) {
		return "", errors.New("ドメイン名ではありません")
	}
	cl := netHTTP(10 * time.Second)
	var lastErr error
	for _, s := range dohServers {
		req, _ := http.NewRequest("GET", s+"?type=AAAA&name="+url.QueryEscape(name), nil)
		req.Header.Set("Accept", "application/dns-json")
		r, err := cl.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		var res struct {
			Status int
			Answer []struct {
				Type int    `json:"type"`
				Data string `json:"data"`
			}
		}
		err = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&res)
		r.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		for _, a := range res.Answer {
			if ip := net.ParseIP(a.Data); a.Type == 28 && isYgg6(ip) {
				return ip.String(), nil
			}
		}
		return "", errors.New(name + " にはYggdrasilアドレス（200::/7）のAAAAレコードがありません")
	}
	if !socks.On {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if ips, err := net.DefaultResolver.LookupIPAddr(ctx, name); err == nil {
			for _, ip := range ips {
				if isYgg6(ip.IP) {
					return ip.IP.String(), nil
				}
			}
			return "", errors.New(name + " にはYggdrasilアドレス（200::/7）のAAAAレコードがありません")
		}
	}
	if lastErr == nil {
		lastErr = errors.New("DNSに問い合わせできませんでした")
	}
	return "", lastErr
}

func (a *App) resolveHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/resolve", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name string }
		if !readJSON(w, r, &req) {
			return
		}
		ad, err := resolveYgg(req.Name)
		if err != nil {
			http.Error(w, err.Error(), 404)
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"addr": ad, "name": strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Name), "."))})
	})
}
