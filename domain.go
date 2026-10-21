// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Verified domains (v0.14): a profile can carry a domain. The claim itself
// arrives over Yggdrasil from the peer's own key (address -> domain); we then
// check that the domain's AAAA record is that same address (domain -> address).
// Only when both agree does the UI show the domain with a check mark.
package main

import (
	"strings"
	"sync"
	"time"
)

func cleanDomain(s string) string {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if s == "" || len(s) > 253 || !domainRE.MatchString(s) {
		return ""
	}
	return s
}

// Lookups a peer's profile packets can trigger: a peer flipping its claimed
// domain on every prof made us run one DoH lookup per packet.
var domCheck struct {
	sync.Mutex
	last map[string]time.Time // peer -> last lookup started
	busy int
}

const domCheckEvery = 30 * time.Second
const domCheckMax = 4 // concurrent lookups

func domCheckStart(peer string) bool {
	domCheck.Lock()
	defer domCheck.Unlock()
	if domCheck.last == nil {
		domCheck.last = map[string]time.Time{}
	}
	now := time.Now()
	if t, ok := domCheck.last[peer]; ok && now.Sub(t) < domCheckEvery || domCheck.busy >= domCheckMax {
		return false
	}
	if len(domCheck.last) > 4096 {
		for p, t := range domCheck.last {
			if now.Sub(t) >= domCheckEvery {
				delete(domCheck.last, p)
			}
		}
	}
	domCheck.last[peer] = now
	domCheck.busy++
	return true
}

func domCheckDone() {
	domCheck.Lock()
	domCheck.busy--
	domCheck.Unlock()
}

func (a *App) verifyDomain(peer, dom string) {
	if !domCheckStart(peer) {
		return // checked again on a later profile (DomAt stays 0 / stale)
	}
	defer domCheckDone()
	ad, err := resolveYggFor(dom, peer)
	ok := err == nil && ad == peer
	if err != nil && !isNoAAAA(err) {
		return // lookup failed (offline, DoH blocked): keep the old result, retry next profile
	}
	a.store.mu.Lock()
	c, exists := a.store.Contacts[peer]
	if !exists || c.Dom != dom {
		a.store.mu.Unlock()
		return
	}
	c.DomOK, c.DomAt = ok, time.Now().Unix()
	a.store.Contacts[peer] = c
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("contact", c)
}
