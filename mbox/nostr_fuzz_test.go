// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package mbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
)

// FuzzNostrOpen: events from a relay are untrusted (any JSON, any content).
// NostrOpen must never panic, and only a box sealed to us by a valid key opens.
func FuzzNostrOpen(f *testing.F) {
	apub, apriv, _ := ed25519.GenerateKey(rand.Reader)
	_, bpriv, _ := ed25519.GenerateKey(rand.Reader)
	ev, err := NostrWrap(apub, bpriv, []byte(`{"t":"msg","id":"x","text":"hi"}`))
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{ev, `{}`, `null`, `[]`, `{"kind":1059,"content":"!!!"}`, `{"kind":1059,"content":""}`, `{"kind":1059,"content":"AAAA"}`, `{"kind":"x"}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		from, pk, wb, _, err := NostrOpen(s, apriv)
		if err != nil {
			return
		}
		if !AddrOK(from, pk) || !json.Valid(wb) {
			t.Fatalf("opened with a bad sender/body: %q %x %q", from, pk, wb)
		}
	})
}
