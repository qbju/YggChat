// SPDX-License-Identifier: GPL-3.0-or-later
package move

import (
	"bytes"
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
