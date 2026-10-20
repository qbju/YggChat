// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import "testing"

func TestPeerViaSocks(t *testing.T) {
	old := socks
	defer func() { socks = old }()
	socks = socksCfg{On: true, Addr: "127.0.0.1:9050"}
	got := effectivePeers([]string{"tcp://a.example:1", "tls://[2001:db8::1]:443?key=ab", "quic://x:1", "wss://y/ygg", "socks://p:1/z:2"})
	want := []string{"socks://127.0.0.1:9050/a.example:1", "sockstls://127.0.0.1:9050/[2001:db8::1]:443?key=ab", "socks://p:1/z:2"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
	socks.On = false
	if p := effectivePeers([]string{"quic://x:1"}); len(p) != 1 {
		t.Fatal("off must not filter")
	}
}
