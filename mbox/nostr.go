// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package mbox

// Optional (deprecated, opt-in) offline delivery over public Nostr relays.
//
// The wire packet is sealed exactly like the old mailbox (anonymous NaCl box to
// the recipient's Ed25519->X25519 key, sender signed inside), then posted as a
// Nostr event signed by a throw-away secp256k1 key. The "p" tag is a hash of
// the recipient's key, not a Nostr identity, so a relay sees neither content,
// sender nor the Yggdrasil address; it does see the recipient tag, timing and
// the poster's IP. The throw-away key travels inside the box, so only the
// recipient can publish the NIP-09 deletion after reading. NIP-40 expiration
// makes well-behaved relays drop it after the TTL anyway.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
)

// NostrKind is the event kind used for boxes (NIP-59 gift-wrap kind; relays
// store it like any regular event).
const NostrKind = 1059

// NostrTTL is how long relays are asked to keep a box (NIP-40).
const NostrTTL = 7 * 24 * 3600

// DefaultRelays are used when the user enables the feature without editing the list.
var DefaultRelays = []string{"wss://nos.lol", "wss://relay.primal.net", "wss://nostr.mom"}

type Event struct {
	ID        string     `json:"id"`
	PubKey    string     `json:"pubkey"`
	CreatedAt int64      `json:"created_at"`
	Kind      int        `json:"kind"`
	Tags      [][]string `json:"tags"`
	Content   string     `json:"content"`
	Sig       string     `json:"sig"`
}

// NostrTag is the recipient tag (64 hex chars) derived from an Ed25519 key.
func NostrTag(pk []byte) string {
	h := sha256.Sum256([]byte("yggchat-nostr:" + hex.EncodeToString(pk)))
	return hex.EncodeToString(h[:])
}

func marshalNoEsc(v any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(b.Bytes(), "\n")
}

func signEvent(sk *btcec.PrivateKey, e *Event) error {
	e.PubKey = hex.EncodeToString(schnorr.SerializePubKey(sk.PubKey()))
	ser := marshalNoEsc([]any{0, e.PubKey, e.CreatedAt, e.Kind, e.Tags, e.Content})
	id := sha256.Sum256(ser)
	sig, err := schnorr.Sign(sk, id[:])
	if err != nil {
		return err
	}
	e.ID = hex.EncodeToString(id[:])
	e.Sig = hex.EncodeToString(sig.Serialize())
	return nil
}

// NostrWrap returns a signed Nostr event (JSON) carrying wb for recipient `to`.
func NostrWrap(to ed25519.PublicKey, priv ed25519.PrivateKey, wb []byte) (string, error) {
	sk, err := btcec.NewPrivateKey()
	if err != nil {
		return "", err
	}
	// Put the deletion key inside the box together with the packet.
	inner, _ := json.Marshal(struct {
		W  json.RawMessage `json:"w"`
		NK string          `json:"nk"`
	}{wb, hex.EncodeToString(sk.Serialize())})
	box, err := Seal(to, priv, inner)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	e := &Event{CreatedAt: now, Kind: NostrKind, Content: box, Tags: [][]string{
		{"p", NostrTag(to)},
		{"expiration", itoa(now + NostrTTL)},
	}}
	if err := signEvent(sk, e); err != nil {
		return "", err
	}
	return string(marshalNoEsc(e)), nil
}

// NostrOpen opens an event addressed to us. It returns the sender's address and
// key, the wire packet, and a signed NIP-09 deletion event (JSON) to publish.
func NostrOpen(evJSON string, priv ed25519.PrivateKey) (from string, pk ed25519.PublicKey, wb []byte, del string, err error) {
	var e Event
	if err = json.Unmarshal([]byte(evJSON), &e); err != nil || e.Kind != NostrKind {
		err = errors.New("not a yggchat event")
		return
	}
	from, pk, inner, err := Open(e.Content, priv)
	if err != nil {
		return
	}
	var in struct {
		W  json.RawMessage `json:"w"`
		NK string          `json:"nk"`
	}
	if err = json.Unmarshal(inner, &in); err != nil || len(in.W) == 0 {
		err = errors.New("bad inner")
		return
	}
	wb = in.W
	if kb, e2 := hex.DecodeString(in.NK); e2 == nil && len(kb) == 32 {
		sk, _ := btcec.PrivKeyFromBytes(kb)
		if hex.EncodeToString(schnorr.SerializePubKey(sk.PubKey())) == e.PubKey {
			d := &Event{CreatedAt: time.Now().Unix(), Kind: 5, Tags: [][]string{{"e", e.ID}, {"k", itoa(NostrKind)}}}
			if signEvent(sk, d) == nil {
				del = string(marshalNoEsc(d))
			}
		}
	}
	return
}

func itoa(n int64) string { return string(marshalNoEsc(n)) }
