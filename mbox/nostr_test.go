// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package mbox

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2/schnorr"
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

// The sender can recall its box: the deletion is signed by the box's own key
// (the same key NostrOpen gives the recipient), so relays accept it (NIP-09).
func TestNostrDeleteBySender(t *testing.T) {
	bpub, bpriv, _ := ed25519.GenerateKey(rand.Reader)
	_, apriv, _ := ed25519.GenerateKey(rand.Reader)
	for _, legacy := range []bool{false, true} {
		ev, id, nk, err := NostrWrapKey(bpub, apriv, []byte(`{"t":"msg","id":"x1","text":"hi"}`), legacy)
		if err != nil {
			t.Fatal(err)
		}
		var e Event
		json.Unmarshal([]byte(ev), &e)
		if e.ID != id {
			t.Fatal("id mismatch")
		}
		del, err := NostrDelete(nk, id)
		if err != nil {
			t.Fatal(err)
		}
		var d Event
		json.Unmarshal([]byte(del), &d)
		if d.Kind != 5 || d.PubKey != e.PubKey || d.Tags[0][0] != "e" || d.Tags[0][1] != id {
			t.Fatalf("bad deletion %s", del)
		}
		if !checkSig(d) {
			t.Fatal("deletion signature invalid")
		}
		_, _, _, rdel, err := NostrOpen(ev, bpriv)
		var r Event
		json.Unmarshal([]byte(rdel), &r)
		if err != nil || r.PubKey != d.PubKey {
			t.Fatal("recipient's deletion uses another key")
		}
	}
	if _, err := NostrDelete("zz", "00"); err == nil {
		t.Fatal("bad key accepted")
	}
}

func checkSig(e Event) bool {
	pkb, err1 := hex.DecodeString(e.PubKey)
	sb, err2 := hex.DecodeString(e.Sig)
	if err1 != nil || err2 != nil {
		return false
	}
	pk, err := schnorr.ParsePubKey(pkb)
	if err != nil {
		return false
	}
	sig, err := schnorr.ParseSignature(sb)
	if err != nil {
		return false
	}
	ser := marshalNoEsc([]any{0, e.PubKey, e.CreatedAt, e.Kind, e.Tags, e.Content})
	h := sha256.Sum256(ser)
	return hex.EncodeToString(h[:]) == e.ID && sig.Verify(h[:], pk)
}
