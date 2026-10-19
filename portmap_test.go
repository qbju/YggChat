// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import "testing"

func csum(p []byte) uint16 {
	var s uint32
	add := func(b []byte) {
		for i := 0; i+1 < len(b); i += 2 {
			s += uint32(b[i])<<8 | uint32(b[i+1])
		}
		if len(b)%2 == 1 {
			s += uint32(b[len(b)-1]) << 8
		}
	}
	add(p[8:40])
	l := len(p) - 40
	s += uint32(l) + 6
	add(p[40:])
	for s>>16 != 0 {
		s = s&0xffff + s>>16
	}
	return uint16(s)
}

func TestPortMap(t *testing.T) {
	p := make([]byte, 60+5)
	p[0] = 0x60
	p[6] = 6
	p[8], p[24] = 0x02, 0x02
	for i := 9; i < 40; i++ {
		p[i] = byte(i * 7)
	}
	p[8], p[24] = 0x02, 0x03
	p[40], p[41] = 0x9c, 0x41
	p[42], p[43] = 0, 79
	copy(p[60:], "hello")
	p[56], p[57] = 0, 0
	c := ^csum(p)
	p[56], p[57] = byte(c>>8), byte(c)
	portMap(p, 24, 42, 79, 7979)
	if p[42] != 7979>>8 || p[43] != 7979&0xff {
		t.Fatal("port not mapped")
	}
	if csum(p) != 0xffff {
		t.Fatalf("bad checksum %x", csum(p))
	}
}
