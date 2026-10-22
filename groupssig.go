// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Signed group posts (v0.19.3). A relayed post ("ghs") names its sender in
// "by", which the relaying member could make up. Native nodes now sign
// their text posts with their Yggdrasil key, so a relay can carry the proof:
//
//	msg/fh (group copy) + {"sg": sig, "pk": sender's key}
//	ghs                 + {"sg", "pk"}   (only when the text is not shortened)
//
//	sig = Ed25519(key, "ygg-gpost1\n"+gid+"\n"+postID+"\n"+ts+"\n"+hex(sha256(text)))
//
// The key is checked against the sender's address, so no one else can sign
// for it. A relayed post with a valid signature is the sender's own words
// and shows unmarked; a bad signature is dropped; an unsigned one shows who
// relayed it, also from the host or an admin when we know the sender signs
// (its last post to us was signed). Older nodes and the PWA ignore the
// fields (the PWA can pass them on later). Photos are not signed.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strconv"

	"github.com/h6e/yggchat/mbox"
)

func postSigMsg(gid, pid string, ts int64, text string) []byte {
	h := sha256.Sum256([]byte(text))
	return []byte("ygg-gpost1\n" + gid + "\n" + pid + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(h[:]))
}

// signPost signs a text post of ours ("" when we can't: system Yggdrasil
// mode doesn't give us the key).
func (a *App) signPost(gid, pid string, ts int64, text string) string {
	if a.sys || a.cfg == nil || text == "" {
		return ""
	}
	priv := ed25519.PrivateKey(a.cfg.PrivateKey)
	if len(priv) != ed25519.PrivateKeySize {
		return ""
	}
	return base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, postSigMsg(gid, pid, ts, text)))
}

// myPK is our key as hex for the "pk" field ("" in system mode).
func (a *App) myPK() string {
	if a.sys || a.cfg == nil || len(a.cfg.PrivateKey) != ed25519.PrivateKeySize {
		return ""
	}
	return hex.EncodeToString(ed25519.PrivateKey(a.cfg.PrivateKey).Public().(ed25519.PublicKey))
}

// verifyPost checks that addr (whose key is pkHex) signed this post.
func verifyPost(addr, pkHex, sg, gid, pid string, ts int64, text string) bool {
	pk, err := hex.DecodeString(pkHex)
	if err != nil || !mbox.AddrOK(addr, pk) {
		return false
	}
	sig, err := base64.RawStdEncoding.DecodeString(sg)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pk), postSigMsg(gid, pid, ts, text), sig)
}

// noteSigner remembers whether from's latest direct post in gid was signed
// (pk = its key) or not (pk = ""). Members only; kept in Group.Keys.
func (a *App) noteSigner(gid, from, pk string) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	g, ok := a.store.Groups[gid]
	if !ok || !hasMember(g, from) || g.Keys[from] == pk {
		return
	}
	if pk == "" {
		delete(g.Keys, from)
	} else {
		if g.Keys == nil {
			g.Keys = map[string]string{}
		}
		g.Keys[from] = pk
	}
	for m := range g.Keys {
		if !hasMember(g, m) {
			delete(g.Keys, m)
		}
	}
	a.store.Groups[gid] = g
	a.store.saveSoon()
}

// sigFor is the signature and key to put on a relay of m ("" when unknown).
// Caller holds a.store.mu.
func (a *App) sigForLocked(g Group, m Message) (sg, pk string) {
	if m.Sg == "" {
		return "", ""
	}
	if m.From == a.addr {
		return m.Sg, a.myPK()
	}
	if k := g.Keys[m.From]; k != "" {
		return m.Sg, k
	}
	return "", ""
}
