// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

// Regression tests for the vendored yggdrasil-go v0.5.15 / ironwood 0712ecc:
// the LAN (multicast) and listener password must not be bypassable with a
// low-order public key, and a rejected listener must not keep its port.
// On the old v0.5.14 copy TestYggLowOrderKeyPassword and
// TestYggInvalidListenerReleasesPort fail.

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"net/url"
	"os"
	"testing"
	"time"

	"filippo.io/edwards25519"
	"github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
)

func yggTestNode(t *testing.T) *core.Core {
	cfg := config.GenerateConfig()
	if err := cfg.GenerateSelfSignedCertificate(); err != nil {
		t.Fatal(err)
	}
	l := log.New(os.Stderr, "", log.Flags())
	if os.Getenv("YGGCHAT_TESTLOG") != "" {
		l.EnableLevel("info")
	}
	n, err := core.New(cfg.Certificate, l)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Stop)
	return n
}

func yggTestListen(t *testing.T, n *core.Core, q string) string {
	u, _ := url.Parse("tcp://127.0.0.1:0" + q)
	li, err := n.Listen(u, "")
	if err != nil {
		t.Fatal(err)
	}
	return li.Addr().String()
}

func yggUpPeers(n *core.Core) (keys []ed25519.PublicKey) {
	for _, p := range n.GetPeers() {
		// Key is only set once ironwood accepted the peer (handshake done)
		if p.Up && len(p.Key) == ed25519.PublicKeySize {
			keys = append(keys, p.Key)
		}
	}
	return
}

func yggWaitPeers(n *core.Core, want int, d time.Duration) int {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if len(yggUpPeers(n)) >= want {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	return len(yggUpPeers(n))
}

// Normal peers: a node must know the listener password.
func TestYggListenerPassword(t *testing.T) {
	for _, c := range []struct {
		name, q string
		ok      bool
	}{{"correct", "?password=s3cret", true}, {"wrong", "?password=nope", false}, {"none", "", false}} {
		t.Run(c.name, func(t *testing.T) {
			a, b := yggTestNode(t), yggTestNode(t)
			addr := yggTestListen(t, a, "?password=s3cret")
			u, _ := url.Parse("tcp://" + addr + c.q)
			if err := b.CallPeer(u, ""); err != nil {
				t.Logf("CallPeer: %v", err)
			}
			got := yggWaitPeers(a, 1, 3*time.Second)
			if (got == 1) != c.ok {
				t.Fatalf("peers=%d, want ok=%v", got, c.ok)
			}
		})
	}
}

// The handshake proves the password with an Ed25519 signature over
// blake2b(key=password, data=publicKey). For the identity point as public key,
// [S]B == R + [k]A holds for any message when R = [S]B, so a forged signature
// passes without knowing the password. ironwood 0712ecc rejects such keys.
func yggForgedHandshake(pub []byte) []byte {
	var sb [64]byte
	rand.Read(sb[:])
	s, _ := edwards25519.NewScalar().SetUniformBytes(sb[:])
	R := new(edwards25519.Point).ScalarBaseMult(s)
	sig := append(R.Bytes(), s.Bytes()...)
	bs := []byte{'m', 'e', 't', 'a', 0, 0}
	add := func(op uint16, v []byte) {
		bs = binary.BigEndian.AppendUint16(bs, op)
		bs = binary.BigEndian.AppendUint16(bs, uint16(len(v)))
		bs = append(bs, v...)
	}
	add(0, []byte{0, 0}) // major
	add(1, []byte{0, 5}) // minor
	add(2, pub)
	add(3, []byte{0})
	bs = append(bs, sig...)
	binary.BigEndian.PutUint16(bs[4:6], uint16(len(bs)-6))
	return bs
}

func TestYggLowOrderKeyPassword(t *testing.T) {
	a := yggTestNode(t)
	addr := yggTestListen(t, a, "?password=s3cret")
	identity := make([]byte, 32)
	identity[0] = 1 // encoding of the identity point (0,1)
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(yggForgedHandshake(identity)); err != nil {
		t.Fatal(err)
	}
	// read and discard the node's own handshake, then keep the link open
	go io.Copy(io.Discard, c)
	got := yggWaitPeers(a, 1, 3*time.Second)
	if got != 0 {
		t.Fatalf("VULNERABLE: peer with identity key and no password accepted (peers=%d, key=%x)", got, yggUpPeers(a)[0])
	}
	t.Log("identity-key peer refused")
}

// Issue #1389: an invalid listener option must not leave the port bound.
func TestYggInvalidListenerReleasesPort(t *testing.T) {
	a := yggTestNode(t)
	for _, q := range []string{"?priority=bad", "?password=" + string(yggLongPass())} {
		p, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := p.Addr().String()
		p.Close()
		u, _ := url.Parse("tcp://" + addr + q)
		if li, err := a.Listen(u, ""); err == nil {
			li.Cancel()
			t.Fatalf("%s: listener accepted", q[:10])
		}
		r, err := net.Listen("tcp", addr)
		if err != nil {
			t.Fatalf("%s: port still bound after rejected listener: %v", q[:10], err)
		}
		r.Close()
	}
}

func yggLongPass() []byte {
	b := make([]byte, 65)
	for i := range b {
		b[i] = 'x'
	}
	return b
}
