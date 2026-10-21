// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"net"
	"testing"
	"time"
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
