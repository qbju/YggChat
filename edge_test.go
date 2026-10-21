// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// Regression: in system/VPN mode the profile site listened on [::]:80/8080 of
// the OS stack and answered anyone (Wi-Fi neighbours, public IPv6), unlike
// finger which checked for a Yggdrasil source. yggOnlyListener drops them.
func TestYggOnlyListenerDropsNonYgg(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	yl := yggOnlyListener{ln}
	defer yl.Close()
	got := make(chan net.Conn, 1)
	go func() {
		c, err := yl.Accept()
		if err == nil {
			got <- c
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 1)
	if _, err := c.Read(b); err == nil {
		t.Fatal("non-Yggdrasil client got data")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("non-Yggdrasil connection was not closed")
	}
	select {
	case <-got:
		t.Fatal("non-Yggdrasil connection accepted")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCleanLANPass(t *testing.T) {
	p := strings.Repeat("あ", 30) // 90 bytes
	got := cleanLANPass(p)
	if len(got) > 64 || !utf8.ValidString(got) || got != strings.Repeat("あ", 21) {
		t.Fatalf("got %q (%d bytes)", got, len(got))
	}
	// What lan.json would hold after a save/load must equal what is in use.
	b, _ := json.Marshal(lanCfg{Pass: got})
	var back lanCfg
	_ = json.Unmarshal(b, &back)
	if back.Pass != got {
		t.Fatal("password changes across save/load")
	}
	if cleanLANPass("  short  ") != "short" {
		t.Fatal("trim")
	}
}

func fakeDoH(t *testing.T, body string) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dns-json")
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// Regression: a domain with several Yggdrasil AAAA records verified only for
// the first one in the answer, and a SERVFAIL from the first DoH server was
// reported as "no AAAA" (un-verifying the contact) instead of trying the next.
func TestResolveYggFor(t *testing.T) {
	old := dohServers
	defer func() { dohServers = old }()
	two := `{"Status":0,"Answer":[{"type":5,"data":"x.example."},{"type":28,"data":"200::a"},{"type":28,"data":"2001:db8::1"},{"type":28,"data":"201::b"}]}`
	dohServers = []string{fakeDoH(t, `{"Status":2}`), fakeDoH(t, two)}
	ad, err := resolveYggFor("x.example", "201::b")
	if err != nil || ad != "201::b" {
		t.Fatalf("got %q %v", ad, err)
	}
	if ad, _ := resolveYgg("x.example"); ad != "200::a" {
		t.Fatalf("first: %q", ad)
	}
	dohServers = []string{fakeDoH(t, `{"Status":0,"Answer":[{"type":28,"data":"2001:db8::1"}]}`)}
	if _, err := resolveYggFor("x.example", "201::b"); !isNoAAAA(err) {
		t.Fatalf("want no-AAAA, got %v", err)
	}
	dohServers = []string{fakeDoH(t, `{"Status":2}`)}
	if _, err := resolveYggFor("x.example", "201::b"); err == nil || isNoAAAA(err) {
		t.Fatalf("SERVFAIL must be a lookup failure, got %v", err)
	}
}

func TestDomCheckRateLimit(t *testing.T) {
	if !domCheckStart("200::77") {
		t.Fatal("first check refused")
	}
	domCheckDone()
	if domCheckStart("200::77") {
		t.Fatal("second check within 30 s allowed")
	}
	if !domCheckStart("200::78") {
		t.Fatal("other peer refused")
	}
	domCheckDone()
}

func FuzzFingerQuery(f *testing.F) {
	for _, s := range []string{"\r\n", "/W\r\n", "/W alice\r\n", "alice@host\r\n", "/W @a@b", "\x00\xff@", strings.Repeat("a", 300)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		u, fwd := fingerQuery(s)
		if strings.Contains(s, "@") != fwd {
			t.Fatalf("%q: forward=%v", s, fwd)
		}
		if fwd && u != "" || strings.Contains(u, "@") || len(u) > len(s) {
			t.Fatalf("%q -> %q", s, u)
		}
	})
}
