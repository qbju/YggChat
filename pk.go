// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"encoding/hex"

	"github.com/h6e/yggchat/mbox"
)

// learnPK records a peer's full public key after checking it matches the address.
// (Used to seal optional Nostr offline boxes.)
func (a *App) learnPK(from, pkHex string) {
	pk, err := hex.DecodeString(pkHex)
	if err != nil || !mbox.AddrOK(from, pk) {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if c, ok := a.store.Contacts[from]; ok && c.PK != pkHex {
		c.PK = pkHex
		a.store.Contacts[from] = c
		a.store.save()
	}
}

// askPK greets a contact whose full key we don't know yet; the hi/hiack carry
// both keys (needed for sealing Nostr offline boxes).
func (a *App) askPK(peer string) {
	a.store.mu.Lock()
	c, ok := a.store.Contacts[peer]
	a.store.mu.Unlock()
	if ok && c.PK == "" {
		go a.hello(peer)
	}
}
