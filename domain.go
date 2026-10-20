// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Verified domains (v0.14): a profile can carry a domain. The claim itself
// arrives over Yggdrasil from the peer's own key (address -> domain); we then
// check that the domain's AAAA record is that same address (domain -> address).
// Only when both agree does the UI show the domain with a check mark.
package main

import (
	"strings"
	"time"
)

func cleanDomain(s string) string {
	s = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
	if s == "" || len(s) > 253 || !domainRE.MatchString(s) {
		return ""
	}
	return s
}

func (a *App) verifyDomain(peer, dom string) {
	ad, err := resolveYgg(dom)
	ok := err == nil && ad == peer
	if err != nil && !strings.Contains(err.Error(), "AAAA") {
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
