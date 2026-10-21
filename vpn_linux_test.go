//go:build linux

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"net"
	"os"
	"syscall"
	"testing"
	"time"
)

func sendFDs(t *testing.T, name string, fds ...int) byte {
	c, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, _, err := c.WriteMsgUnix([]byte{1}, syscall.UnixRights(fds...), nil); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	b := make([]byte, 1)
	n, _ := c.Read(b)
	if n == 0 {
		return 0
	}
	return b[0]
}

// The TUN hand-over takes exactly one fd from our own uid; extra fds must be
// closed, not leaked into the node process.
func TestVPNAcceptFD(t *testing.T) {
	name := "@yggchat-tun-test-" + time.Now().Format("150405.000000")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: name, Net: "unix"})
	if err != nil {
		t.Skip(err)
	}
	defer ln.Close()
	v := &vpnState{}
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				return
			}
			v.accept(c)
		}
	}()
	r1, w1, _ := os.Pipe()
	defer r1.Close()
	defer w1.Close()
	r2, w2, _ := os.Pipe()
	defer r2.Close()
	defer w2.Close()
	if got := sendFDs(t, name, int(r1.Fd()), int(r2.Fd())); got != 0 {
		t.Fatal("two fds accepted")
	}
	if v.cur() != nil {
		t.Fatal("tun set from a two-fd message")
	}
	if got := sendFDs(t, name, int(r1.Fd())); got != 1 {
		t.Fatal("one fd refused")
	}
	if v.cur() == nil {
		t.Fatal("tun not set")
	}
	v.cur().Close() // ends toCore
}
