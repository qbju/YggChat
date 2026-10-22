// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group info that may not have arrived (r2-matrix). The info a host or admin
// sends when it makes a group or adds members is plain datagrams, three
// rounds within 4 s. A member that just came online (or whose route is still
// being looked up) often misses all three and then never learns it is in the
// group: it doesn't take part in catch-up, and posts reach it only as
// message requests. So the manager remembers whom it hasn't heard from in
// that group yet and sends the info again a little later, and whenever that
// member's node says hello (it reached us). Hearing any packet about the
// group from the member settles it. Kept in memory only; at most a week.
package main

import (
	"sync"
	"time"
)

const (
	invKeep  = 7 * 86400 // seconds
	invMax   = 4096      // remembered (gid, member) pairs
	invEvery = 10 * time.Second
)

var invRetry = []time.Duration{10 * time.Second, 30 * time.Second, 90 * time.Second}

var inv = struct {
	mu   sync.Mutex
	at   map[string]int64     // gid|member -> when the info went out first
	sent map[string]time.Time // gid|member -> last push
}{at: map[string]int64{}, sent: map[string]time.Time{}}

// noteInvited remembers that members were just sent the info of group gid,
// and sends it again a few times while we haven't heard from them.
func (a *App) noteInvited(gid string, members []string) {
	now := time.Now()
	inv.mu.Lock()
	if len(inv.at) > invMax {
		inv.at, inv.sent = map[string]int64{}, map[string]time.Time{}
	}
	var ms []string
	for _, m := range members {
		if m == "" || m == a.addr {
			continue
		}
		k := gid + "|" + m
		if _, ok := inv.at[k]; !ok {
			inv.at[k] = now.Unix()
		}
		inv.sent[k] = now
		ms = append(ms, m)
	}
	inv.mu.Unlock()
	if len(ms) == 0 {
		return
	}
	go func() {
		for _, d := range invRetry {
			time.Sleep(d)
			left := 0
			for _, m := range ms {
				if a.pushInvite(gid, m) {
					left++
				}
			}
			if left == 0 {
				return
			}
		}
	}()
}

// invHeard: a packet about group gid came from peer, so it has the group.
func invHeard(gid, peer string) {
	inv.mu.Lock()
	k := gid + "|" + peer
	if _, ok := inv.at[k]; ok {
		delete(inv.at, k)
		delete(inv.sent, k)
	}
	inv.mu.Unlock()
}

// pushInvites sends peer the info of every group it may not have yet (it just
// said hello, so a route to it exists now).
func (a *App) pushInvites(peer string) {
	inv.mu.Lock()
	var gids []string
	for k := range inv.at {
		if len(k) > len(peer) && k[len(k)-len(peer)-1:] == "|"+peer {
			gids = append(gids, k[:len(k)-len(peer)-1])
		}
	}
	inv.mu.Unlock()
	for _, gid := range gids {
		a.pushInvite(gid, peer)
	}
}

// pushInvite re-sends the info of gid to member peer if we still haven't
// heard from it; false once there is nothing left to do for this pair.
func (a *App) pushInvite(gid, peer string) bool {
	k := gid + "|" + peer
	now := time.Now()
	inv.mu.Lock()
	at, ok := inv.at[k]
	if ok && now.Unix()-at > invKeep {
		delete(inv.at, k)
		delete(inv.sent, k)
		ok = false
	}
	if !ok || now.Sub(inv.sent[k]) < invEvery {
		inv.mu.Unlock()
		return ok
	}
	inv.sent[k] = now
	inv.mu.Unlock()
	a.store.mu.Lock()
	g, known := a.store.Groups[gid]
	still := known && !g.Left && !a.store.locked && canManage(g, a.addr) && hasMember(g, peer)
	a.store.mu.Unlock()
	if !still {
		invHeard(gid, peer)
		return false
	}
	for _, p := range a.infoPackets(g) {
		a.sendWire(peer, p)
	}
	return true
}
