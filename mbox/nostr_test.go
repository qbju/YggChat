// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package mbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestNostrPairTagAndPadding(t *testing.T) {
	apub, apriv, _ := ed25519.GenerateKey(rand.Reader)
	bpub, bpriv, _ := ed25519.GenerateKey(rand.Reader)
	_, cpriv, _ := ed25519.GenerateKey(rand.Reader)
	d := time.Now().Unix() / 86400
	ab, _ := NostrPairTag(apriv, bpub, d)
	ba, _ := NostrPairTag(bpriv, apub, d)
	cb, _ := NostrPairTag(cpriv, bpub, d)
	ab2, _ := NostrPairTag(apriv, bpub, d+1)
	if ab != ba || ab == cb || ab == ab2 {
		t.Fatal("pair tag not symmetric / not pairwise / not rotating")
	}
	sizes := map[int]bool{}
	for _, txt := range []string{"a", "こんにちは、これは少し長めのテキストです", strings.Repeat("あ", 100)} {
		wb, _ := json.Marshal(map[string]any{"t": "msg", "id": "x", "text": txt})
		ev, err := NostrWrap(bpub, apriv, wb)
		if err != nil {
			t.Fatal(err)
		}
		var e Event
		json.Unmarshal([]byte(ev), &e)
		if e.Tags[0][1] != ab {
			t.Fatal("event not tagged with pair tag")
		}
		if e.CreatedAt > time.Now().Unix() {
			t.Fatal("future created_at")
		}
		raw, _ := base64.StdEncoding.DecodeString(e.Content)
		sizes[len(raw)] = true
		_, pk, w, del, err := NostrOpen(ev, bpriv)
		if err != nil || string(pk) != string(apub) || string(w) != string(wb) || del == "" {
			t.Fatal("open failed", err)
		}
		found := false
		for _, tg := range NostrTags(bpriv, [][]byte{apub}, time.Now().Unix(), true) {
			found = found || tg == ab
		}
		if !found {
			t.Fatal("recipient does not fetch the tag")
		}
	}
	if len(sizes) != 1 {
		t.Fatalf("payload sizes leak: %v", sizes)
	}
}
