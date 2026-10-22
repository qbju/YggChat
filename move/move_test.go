// SPDX-License-Identifier: GPL-3.0-or-later
package move

import (
	"bytes"
	"net"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	k, code, err := NewReceiver("200:1234::1")
	if err != nil {
		t.Fatal(err)
	}
	addr, pub, err := ParseCode("  " + code + "\n")
	if err != nil || addr != "200:1234::1" {
		t.Fatal(addr, err)
	}
	msg := bytes.Repeat([]byte("hello yggchat "), 5000)
	blob, err := Seal(pub, msg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Open(k, blob)
	if err != nil || !bytes.Equal(out, msg) {
		t.Fatal("open", err)
	}
	blob[40] ^= 1
	if _, err := Open(k, blob); err == nil {
		t.Fatal("tamper not detected")
	}
	k2, _, _ := NewReceiver("200::1")
	blob[40] ^= 1
	if _, err := Open(k2, blob); err == nil {
		t.Fatal("wrong key opened")
	}
	f, err := SealPass("pass1234", msg)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := OpenPass("pass1234", f); err != nil || !bytes.Equal(out, msg) {
		t.Fatal("pass", err)
	}
	if _, err := OpenPass("wrong", f); err == nil {
		t.Fatal("wrong pass")
	}
	if _, _, err := ParseCode("ygm1:1.2.3.4:xx"); err == nil {
		t.Fatal("bad code accepted")
	}
}

// Regression: SealPass accepted 4 bytes, i.e. "あい", for a file that holds
// the identity key and can be brute-forced offline.
func TestSealPassMinLength(t *testing.T) {
	for _, p := range []string{"あい", "abcd", "1234567", "  abcdefg  "} {
		if _, err := SealPass(p, []byte("x")); err == nil {
			t.Errorf("weak passphrase %q accepted", p)
		}
	}
	for _, p := range []string{"abcdefgh", "あいうえおかきく"} {
		if _, err := SealPass(p, []byte("x")); err != nil {
			t.Errorf("%q refused: %v", p, err)
		}
	}
}

func FuzzParseCode(f *testing.F) {
	f.Add("ygm1:200:1234::1:" + b64u.EncodeToString(make([]byte, 32)))
	f.Add("ygm1:1.2.3.4:xx")
	f.Add("ygm1::")
	f.Add("xx ygm1:[200::1]:AAAA")
	f.Fuzz(func(t *testing.T, s string) {
		addr, pub, err := ParseCode(s)
		if err != nil {
			return
		}
		ip := net.ParseIP(addr)
		if ip == nil || ip.To4() != nil || ip[0]&0xfe != 0x02 || len(pub) != 32 {
			t.Fatalf("accepted bad code %q -> %q %d", s, addr, len(pub))
		}
	})
}

func FuzzOpen(f *testing.F) {
	k, _, _ := NewReceiver("200::1")
	good, _ := Seal(k.PublicKey().Bytes(), []byte("hello"))
	f.Add(good)
	f.Add(make([]byte, 80))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = Open(k, b)
	})
}
