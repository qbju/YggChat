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
	"fmt"
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
	ads, err := resolveYggAll(name)
	if err != nil {
		return "", err
	}
	return ads[0], nil
}

// resolveYggFor is resolveYgg for checking a claim: when any of the name's
// Yggdrasil AAAA records is want, it returns want. A domain may list several
// Yggdrasil hosts, and DoH answers come in any order, so comparing only the
// first record made verification fail (or flap) for all but one of them.
func resolveYggFor(name, want string) (string, error) {
	ads, err := resolveYggAll(name)
	if err != nil {
		return "", err
	}
	for _, ad := range ads {
		if ad == want {
			return ad, nil
		}
	}
	return ads[0], nil
}

// noAAAAError: the name resolved, but has no Yggdrasil AAAA record (as
// opposed to the lookup itself failing).
type noAAAAError struct{ name string }

func (e noAAAAError) Error() string {
	return e.name + " にはYggdrasilアドレス（200::/7）のAAAAレコードがありません"
}

func noAAAA(name string) error { return noAAAAError{name} }

func isNoAAAA(err error) bool { var e noAAAAError; return errors.As(err, &e) }

// resolveYggAll returns every Yggdrasil address among name's AAAA records
// (at least one, else an error).
func resolveYggAll(name string) ([]string, error) {
	name = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
	if len(name) > 253 || !domainRE.MatchString(name) {
		return nil, errors.New("ドメイン名ではありません")
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
		if r.StatusCode != 200 {
			r.Body.Close()
			lastErr = errors.New("DoH: " + r.Status)
			continue
		}
		err = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&res)
		r.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		// 0 = NOERROR, 3 = NXDOMAIN are real answers; SERVFAIL, REFUSED and
		// the like say nothing about the name, so ask the next server instead
		// of reporting "no AAAA" (which also un-verified a contact's domain).
		if res.Status != 0 && res.Status != 3 {
			lastErr = fmt.Errorf("DNS rcode %d", res.Status)
			continue
		}
		var out []string
		for _, a := range res.Answer {
			if ip := net.ParseIP(a.Data); a.Type == 28 && isYgg6(ip) {
				out = append(out, ip.String())
			}
		}
		if len(out) == 0 {
			return nil, noAAAA(name)
		}
		return out, nil
	}
	if !socks.On {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if ips, err := net.DefaultResolver.LookupIPAddr(ctx, name); err == nil {
			var out []string
			for _, ip := range ips {
				if isYgg6(ip.IP) {
					out = append(out, ip.IP.String())
				}
			}
			if len(out) == 0 {
				return nil, noAAAA(name)
			}
			return out, nil
		}
	}
	if lastErr == nil {
		lastErr = errors.New("DNSに問い合わせできませんでした")
	}
	return nil, lastErr
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
