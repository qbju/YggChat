package main

import (
	"testing"
	"time"
)

func pkt(proto byte, src, dst byte, sp, dp uint16) []byte {
	p := make([]byte, 60)
	p[0] = 0x60
	p[6] = proto
	p[8], p[23] = 0x02, src
	p[24], p[39] = 0x02, dst
	p[40], p[41], p[42], p[43] = byte(sp>>8), byte(sp), byte(dp>>8), byte(dp)
	return p
}

func TestVPNFW(t *testing.T) {
	fin := false
	f := &vpnFW{flows: map[flowKey]time.Time{}, finger: func() bool { return fin }}
	f.setCfg(vpnFWCfg{})
	if f.inbound(pkt(6, 9, 1, 40000, 22)) {
		t.Fatal("closed: ssh allowed")
	}
	if !f.inbound(pkt(17, 9, 1, 7777, chatPort)) {
		t.Fatal("chat blocked")
	}
	if f.inbound(pkt(6, 9, 1, 5000, fingerAltPort)) {
		t.Fatal("finger off but allowed")
	}
	fin = true
	if !f.inbound(pkt(6, 9, 1, 5000, fingerAltPort)) {
		t.Fatal("finger on but blocked")
	}
	f.outbound(pkt(6, 1, 9, 51000, 443))
	if !f.inbound(pkt(6, 9, 1, 443, 51000)) {
		t.Fatal("reply blocked")
	}
	if f.inbound(pkt(6, 8, 1, 443, 51000)) {
		t.Fatal("reply from other host allowed")
	}
	ping := pkt(58, 9, 1, 0, 0)
	ping[40] = 128
	if f.inbound(ping) {
		t.Fatal("ping allowed")
	}
	f.setCfg(vpnFWCfg{Mode: "list", Ports: []int{22}})
	if !f.inbound(pkt(6, 9, 1, 40000, 22)) {
		t.Fatal("list: 22 blocked")
	}
	f.setCfg(vpnFWCfg{Mode: "all"})
	if !f.inbound(ping) {
		t.Fatal("all: ping blocked")
	}
}
