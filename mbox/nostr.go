// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package mbox

// Optional (deprecated, opt-in) offline delivery over public Nostr relays.
//
// The wire packet is sealed exactly like the old mailbox (anonymous NaCl box to
// the recipient's Ed25519->X25519 key, sender signed inside), then posted as a
// Nostr event signed by a throw-away secp256k1 key, so a relay sees neither
// content, sender nor the Yggdrasil address.
//
// Since v0.13 the "p" tag is a pairwise, daily-rotating value:
// HMAC-SHA256(X25519(sender, recipient), "yggchat-nostr-v2|"+day). Only the
// two people in a conversation can compute it, so a stranger who merely knows
// an address can neither find nor flood that person's boxes, and the tag a
// relay sees changes every day and differs per contact. created_at is
// back-dated by a random 0-48 h (as NIP-59 does), the expiration is blurred
// too, and the sealed payload is padded to a fixed size bucket. The real send
// time travels inside the box. Relays still see the poster's and fetcher's
// IP addresses and, within one fetch, which tags one client asks for.
//
// The throw-away key travels inside the box, so only the recipient can
// publish the NIP-09 deletion after reading. NIP-40 expiration makes
// well-behaved relays drop it after the TTL anyway.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"strconv"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"golang.org/x/crypto/curve25519"
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

// NostrTag is the pre-v0.13 static recipient tag. Only used when the user turns
// on "旧式の置き配も通す" (send a second legacy box, fetch this tag); remove in v0.20.
func NostrTag(pk []byte) string {
	h := sha256.Sum256([]byte("yggchat-nostr:" + hex.EncodeToString(pk)))
	return hex.EncodeToString(h[:])
}

// pairSecret is the X25519 shared secret of our key and the peer's (symmetric).
func pairSecret(priv ed25519.PrivateKey, peer []byte) ([]byte, error) {
	xb, err := edPubToX(peer)
	if err != nil {
		return nil, err
	}
	return curve25519.X25519(edPrivToX(priv)[:], xb[:])
}

func day(t int64) int64 { return t / 86400 }

// NostrPairTag is the tag for boxes between us and peer on a given UTC day.
func NostrPairTag(priv ed25519.PrivateKey, peer []byte, d int64) (string, error) {
	sec, err := pairSecret(priv, peer)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, sec)
	m.Write([]byte("yggchat-nostr-v2|" + strconv.FormatInt(d, 10)))
	return hex.EncodeToString(m.Sum(nil)), nil
}

// NostrTags returns every tag a box for us may carry right now: each peer's
// pair tag for the TTL window (plus a day of clock skew either side) and the
// legacy static tag. Order is shuffled so it says nothing about the contact list.
func NostrTags(priv ed25519.PrivateKey, peers [][]byte, now int64, legacy bool) []string {
	out := []string{}
	if legacy {
		out = append(out, NostrTag(priv.Public().(ed25519.PublicKey)))
	}
	for _, pk := range peers {
		for d := day(now) - NostrTTL/86400 - 1; d <= day(now)+1; d++ {
			if t, err := NostrPairTag(priv, pk, d); err == nil {
				out = append(out, t)
			}
		}
	}
	for i := len(out) - 1; i > 0; i-- {
		j := randInt(int64(i + 1))
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// NostrSince is the earliest created_at a live box can carry (TTL + back-dating).
func NostrSince(now int64) int64 { return now - NostrTTL - nostrBackdate - 3600 }

const nostrBackdate = 2 * 24 * 3600

func randInt(n int64) int {
	v, err := rand.Int(rand.Reader, big.NewInt(n))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

// padTo returns the size bucket the sealed payload is padded up to.
func padTo(n int) int {
	for _, b := range []int{1024, 2048, 4096, 8192, 16384} {
		if n <= b {
			return b
		}
	}
	return (n + 16383) / 16384 * 16384
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
	return nostrWrap(to, priv, wb, false)
}

// NostrWrapLegacy is NostrWrap with the pre-v0.13 static tag and no
// back-dating, so v0.12 receivers find it. Opt-in; remove in v0.20.
func NostrWrapLegacy(to ed25519.PublicKey, priv ed25519.PrivateKey, wb []byte) (string, error) {
	return nostrWrap(to, priv, wb, true)
}

func nostrWrap(to ed25519.PublicKey, priv ed25519.PrivateKey, wb []byte, legacy bool) (string, error) {
	sk, err := btcec.NewPrivateKey()
	if err != nil {
		return "", err
	}
	// Put the deletion key inside the box together with the packet, padded so
	// the box size only reveals a coarse bucket.
	type innerT struct {
		W   json.RawMessage `json:"w"`
		NK  string          `json:"nk"`
		Pad string          `json:"pad,omitempty"`
	}
	in := innerT{W: wb, NK: hex.EncodeToString(sk.Serialize())}
	base, _ := json.Marshal(in)
	// Seal wraps it as {"pk":<64 hex>,"w":inner,"sig":<128 hex>}: 215 bytes;
	// the pad field itself costs 9 more.
	if n := 215 + len(base) + 9; padTo(n) > n {
		extra := padTo(n) - n
		in.Pad = string(bytes.Repeat([]byte{'0'}, extra))
	}
	inner, _ := json.Marshal(in)
	box, err := Seal(to, priv, inner)
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	tag, err := NostrPairTag(priv, to, day(now))
	if err != nil {
		return "", err
	}
	created := now - int64(randInt(nostrBackdate))
	if legacy {
		tag, created = NostrTag(to), now
	}
	e := &Event{CreatedAt: created, Kind: NostrKind, Content: box, Tags: [][]string{
		{"p", tag},
		{"expiration", itoa(now + NostrTTL - int64(randInt(12*3600)))},
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
