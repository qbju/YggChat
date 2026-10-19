package main

import (
	"encoding/hex"

	"github.com/h6e/yggchat/mbox"
)

// learnPK records a peer's full public key after checking it matches the address.
// (Kept for a future self-hosted mailbox node; the Cloudflare mailbox was removed in v0.7.)
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
