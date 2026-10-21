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
	if f.inbound(pkt(6, 9, 1, 5000, fingerPort)) {
		t.Fatal("finger off but allowed")
	}
	fin = true
	if !f.inbound(pkt(6, 9, 1, 5000, fingerPort)) {
		t.Fatal("finger on but blocked")
	}
	// The firewall sees packets before the 79->7979 mapping: a packet sent
	// straight to 7979 is not finger traffic.
	if f.inbound(pkt(6, 9, 1, 5000, fingerAltPort)) {
		t.Fatal("direct 7979 allowed")
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

// Regression: with the profile site on, a packet aimed straight at TCP 8080
// (where another app, e.g. a dev server, may listen) passed the closed
// firewall. Only port 80 (mapped to 8080 after the check) may pass.
func TestVPNFWSiteDirectAltPort(t *testing.T) {
	site := true
	f := &vpnFW{flows: map[flowKey]time.Time{}, site: func() bool { return site }}
	f.setCfg(vpnFWCfg{})
	if !f.inbound(pkt(6, 9, 1, 40000, sitePort)) {
		t.Fatal("site on: port 80 blocked")
	}
	if f.inbound(pkt(6, 9, 1, 40000, siteAltPort)) {
		t.Fatal("site on: direct 8080 allowed")
	}
	site = false
	if f.inbound(pkt(6, 9, 1, 40000, sitePort)) {
		t.Fatal("site off: port 80 allowed")
	}
	f.setCfg(vpnFWCfg{Mode: "list", Ports: []int{8080}})
	if !f.inbound(pkt(6, 9, 1, 40000, siteAltPort)) {
		t.Fatal("list 8080: blocked")
	}
}

func TestVPNFWICMP(t *testing.T) {
	f := &vpnFW{flows: map[flowKey]time.Time{}}
	f.setCfg(vpnFWCfg{})
	for typ, want := range map[byte]bool{1: true, 2: true, 3: true, 4: true, 128: false, 129: true, 130: false, 139: false, 143: false} {
		p := pkt(58, 9, 1, 0, 0)
		p[40] = typ
		if f.inbound(p) != want {
			t.Errorf("icmp type %d: got %v", typ, !want)
		}
	}
	frag := pkt(44, 9, 1, 0, 0)
	if f.inbound(frag) {
		t.Fatal("fragment allowed in closed mode")
	}
	if f.inbound(make([]byte, 39)) || f.inbound(nil) {
		t.Fatal("short packet allowed")
	}
}

func FuzzVPNFW(f *testing.F) {
	f.Add(pkt(6, 9, 1, 40000, 22), byte(0))
	f.Add(pkt(17, 9, 1, 7777, chatPort), byte(1))
	f.Add(pkt(58, 9, 1, 0, 0), byte(2))
	f.Add([]byte{0x60}, byte(0))
	f.Fuzz(func(t *testing.T, p []byte, mode byte) {
		fw := &vpnFW{flows: map[flowKey]time.Time{}, finger: func() bool { return mode&4 != 0 }, site: func() bool { return mode&8 != 0 }}
		fw.setCfg(vpnFWCfg{Mode: []string{"closed", "list", "all"}[int(mode)%3], Ports: []int{22, 8080}})
		q := append([]byte(nil), p...)
		fw.outbound(q)
		ok := fw.inbound(q)
		if fw.cfg.Mode == "closed" && ok && len(q) >= 44 && q[0]>>4 == 6 && q[6] == 6 {
			dp := uint16(q[42])<<8 | uint16(q[43])
			if dp == fingerAltPort || dp == siteAltPort || dp == 22 {
				// only possible as a reply to a flow the phone opened
				var k flowKey
				k.proto, k.rport, k.lpt = 6, uint16(q[40])<<8|uint16(q[41]), dp
				copy(k.remote[:], q[8:24])
				if _, had := fw.flows[k]; !had {
					t.Fatalf("closed mode let TCP %d in", dp)
				}
			}
		}
		portMap(q, 24, 42, fingerPort, fingerAltPort)
		portMap(q, 8, 40, siteAltPort, sitePort)
	})
}
