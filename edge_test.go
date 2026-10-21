// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"encoding/json"
	"net"
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
