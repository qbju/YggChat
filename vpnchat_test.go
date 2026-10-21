// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"net"
	"testing"

	"golang.org/x/net/ipv6"
)

func udpPkt(src net.IP, sport uint16, payload string) []byte {
	p := make([]byte, 48+len(payload))
	p[0], p[6] = 0x60, 17
	copy(p[8:24], src.To16())
	copy(p[24:40], net.ParseIP("201::1").To16())
	p[40], p[41] = byte(sport>>8), byte(sport)
	p[42], p[43] = byte(chatPort>>8), byte(chatPort&0xff)
	l := 8 + len(payload)
	p[44], p[45] = byte(l>>8), byte(l)
	copy(p[48:], payload)
	return p
}

// Regression: in VPN/system mode the chat socket ([::]:7777 on the OS stack)
// trusted any datagram with a 200::/7 source, including ones forged on the
// LAN or over loopback. VPN mode now only accepts what the TUN pump delivered.
func TestVPNChatOnlyFromPump(t *testing.T) {
	v := &vpnState{}
	friend := net.ParseIP("202::5")
	v.noteChat(udpPkt(friend, 7777, `{"t":"msg","id":"a"}`))
	a := &App{vpn: true, vpnS: v}
	c := &osChatConn{a: a}
	src := &net.UDPAddr{IP: friend, Port: 7777}
	if !c.fromYgg([]byte(`{"t":"msg","id":"a"}`), nil, src) {
		t.Fatal("delivered datagram refused")
	}
	if c.fromYgg([]byte(`{"t":"msg","id":"a"}`), nil, src) {
		t.Fatal("same datagram accepted twice (replay)")
	}
	if c.fromYgg([]byte(`{"t":"msg","id":"forged"}`), nil, src) {
		t.Fatal("forged datagram accepted")
	}
	v.noteChat(udpPkt(friend, 7777, "x"))
	if c.fromYgg([]byte("x"), nil, &net.UDPAddr{IP: friend, Port: 7778}) {
		t.Fatal("other source port accepted")
	}
	if c.fromYgg([]byte("x"), nil, &net.UDPAddr{IP: net.ParseIP("192.0.2.1"), Port: 7777}) {
		t.Fatal("non-Ygg source accepted")
	}
	// UDP length shorter than the packet (padding) must still match
	p := udpPkt(friend, 9, "yy")
	p = append(p, 0, 0, 0)
	v.noteChat(p)
	if !c.fromYgg([]byte("yy"), nil, &net.UDPAddr{IP: friend, Port: 9}) {
		t.Fatal("padded packet not matched")
	}
}

func TestSysChatDst(t *testing.T) {
	a := &App{sys: true, addr: "201::1"}
	c := &osChatConn{a: a, cmOK: true}
	src := &net.UDPAddr{IP: net.ParseIP("202::5"), Port: 7777}
	if c.fromYgg(nil, &ipv6.ControlMessage{Dst: net.ParseIP("2001:db8::7")}, src) {
		t.Fatal("datagram to the LAN address accepted")
	}
	if !c.fromYgg(nil, &ipv6.ControlMessage{Dst: net.ParseIP("201::1")}, src) {
		t.Fatal("datagram to our Ygg address refused")
	}
}
