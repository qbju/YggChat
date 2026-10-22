// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Chat UDP on the OS stack (system / VPN mode). The node trusts a datagram's
// source address as the sender's identity, which only holds for packets that
// really came out of Yggdrasil. The socket listens on [::]:7777, so without a
// check anyone on the same Wi-Fi/LAN could send UDP to the device's LAN
// address with a forged 200::/7 source (IPv6 has no reverse-path filter by
// default), and a local app could do the same over loopback with
// IPV6_FREEBIND: forged messages, acks, group control from any contact.
//
//	VPN mode:    we inject every mesh packet into the TUN ourselves, so a
//	             datagram is accepted only if the pump just delivered it.
//	system mode: the packet must be addressed to our Yggdrasil address and,
//	             when the interface can be found, arrive on the interface that
//	             holds it (the Yggdrasil TUN).
package main

import (
	"hash/fnv"
	"log"
	"net"
	"sync"
	"time"

	"golang.org/x/net/ipv6"
)

type osChatConn struct {
	*net.UDPConn
	p    *ipv6.PacketConn
	a    *App
	cmOK bool // the OS gives us destination address + interface per packet
}

func (a *App) listenOSChat() (net.PacketConn, error) {
	c, err := net.ListenUDP("udp6", &net.UDPAddr{Port: chatPort})
	if err != nil {
		return nil, err
	}
	oc := &osChatConn{UDPConn: c, p: ipv6.NewPacketConn(c), a: a}
	oc.cmOK = oc.p.SetControlMessage(ipv6.FlagDst|ipv6.FlagInterface, true) == nil
	if !oc.cmOK && !a.vpn {
		log.Println("chat: no IPv6 packet info on this OS; can't tell Yggdrasil packets from LAN ones")
	}
	return oc, nil
}

func (c *osChatConn) ReadFrom(b []byte) (int, net.Addr, error) {
	for {
		n, cm, src, err := c.p.ReadFrom(b)
		if err != nil {
			return n, src, err
		}
		if c.fromYgg(b[:n], cm, src) {
			return n, src, nil
		}
	}
}

func (c *osChatConn) fromYgg(b []byte, cm *ipv6.ControlMessage, src net.Addr) bool {
	ua, _ := src.(*net.UDPAddr)
	if ua == nil || !isYggIP(ua.IP) {
		return false
	}
	if c.a.vpn {
		if v := c.a.vpnS; v != nil {
			return v.tookChat(ua.IP, ua.Port, b)
		}
		return false
	}
	if !c.cmOK || cm == nil {
		return true // no packet info (e.g. Windows): as before
	}
	own := net.ParseIP(c.a.myAddr())
	if own == nil || !cm.Dst.Equal(own) {
		return false
	}
	if idx := yggIfIndex(own); idx > 0 && cm.IfIndex != idx {
		return false
	}
	return true
}

// ---------- VPN mode: datagrams the pump delivered ----------

type chatKey struct {
	src  [16]byte
	port uint16
	n    int
	sum  uint64
}

type chatSeen struct {
	n int
	t time.Time
}

func chatKeyOf(src net.IP, port int, payload []byte) chatKey {
	var k chatKey
	copy(k.src[:], src.To16())
	k.port, k.n = uint16(port), len(payload)
	h := fnv.New64a()
	h.Write(payload)
	k.sum = h.Sum64()
	return k
}

// noteChat records a UDP datagram for the chat port that fromCore is about
// to write into the TUN (IPv6 without extension headers).
func (v *vpnState) noteChat(p []byte) {
	if len(p) < 48 || p[0]>>4 != 6 || p[6] != 17 || uint16(p[42])<<8|uint16(p[43]) != chatPort {
		return
	}
	end := 40 + int(uint16(p[44])<<8|uint16(p[45]))
	if end < 48 || end > len(p) {
		return
	}
	k := chatKeyOf(net.IP(p[8:24]), int(uint16(p[40])<<8|uint16(p[41])), p[48:end])
	now := time.Now()
	v.chatMu.Lock()
	if v.chat == nil {
		v.chat = map[chatKey]chatSeen{}
	}
	if len(v.chat) >= 4096 {
		for k, s := range v.chat {
			if now.Sub(s.t) > 10*time.Second {
				delete(v.chat, k)
			}
		}
		if len(v.chat) >= 4096 {
			v.chat = map[chatKey]chatSeen{} // flood: start over
		}
	}
	s := v.chat[k]
	v.chat[k] = chatSeen{n: s.n + 1, t: now}
	v.chatMu.Unlock()
}

// tookChat reports (and consumes) whether the pump delivered this datagram.
func (v *vpnState) tookChat(src net.IP, port int, payload []byte) bool {
	k := chatKeyOf(src, port, payload)
	v.chatMu.Lock()
	defer v.chatMu.Unlock()
	s, ok := v.chat[k]
	if !ok || time.Since(s.t) > 10*time.Second {
		delete(v.chat, k)
		return false
	}
	if s.n <= 1 {
		delete(v.chat, k)
	} else {
		s.n--
		v.chat[k] = s
	}
	return true
}

// ---------- system mode: which interface holds our address ----------

var yggIf struct {
	sync.Mutex
	addr string
	idx  int
	at   time.Time
}

// yggIfIndex returns the index of the interface that has own, or 0 when it
// can't be found (interface listing is not allowed on newer Android).
func yggIfIndex(own net.IP) int {
	yggIf.Lock()
	defer yggIf.Unlock()
	if yggIf.addr == own.String() && time.Since(yggIf.at) < 30*time.Second {
		return yggIf.idx
	}
	yggIf.addr, yggIf.idx, yggIf.at = own.String(), 0, time.Now()
	ifs, err := net.Interfaces()
	if err != nil {
		return 0
	}
	for _, ifc := range ifs {
		as, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range as {
			if n, ok := a.(*net.IPNet); ok && n.IP.Equal(own) {
				yggIf.idx = ifc.Index
				return ifc.Index
			}
		}
	}
	return 0
}
