// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group hardening (v0.19.3 bug hunt). Members don't trust each other more
// than they must:
//
//   - A former member's "late" post (made before it left or was removed) is
//     taken only for a while after it dropped out and only so many of them,
//     so a removed member can't keep posting by backdating its posts.
//   - The host or admin who changes the member list says when ("ts" on gi and
//     gd); a member that learns a removal late drops what the removed member
//     posted after it, and never relays it.
//   - Relayed history ("ghs") is taken from an ordinary member only when we
//     asked it (ghq) a moment ago, only for posts by members (or former
//     members before they dropped out), and the group's timer applies to it.
//     A direct copy from the sender replaces a relayed version of the post.
//   - Without history sharing a member catching up gets only what was said
//     since it was added, whatever "since" it asks for.
//   - Strangers can't fill the store with invitations, split-list buffers or
//     rate-limit entries.
//   - Members removed by the host or an admin can't come back through an
//     invite link (a direct add or an approval lets them in again).
//   - A host that left and handed the group to an admin who was offline
//     re-sends the hand-over when that admin turns up.
package main

import (
	"sort"
	"sync"
	"time"
)

const (
	lateGrace     = 7 * 86400 // late posts from a former member are taken this long after it dropped out
	lateMax       = 30        // ... and at most this many per member (per run)
	maxVJump      = 64        // an admin's list may be at most this many versions ahead of ours
	maxInvites    = 50        // unanswered invitations from people we haven't accepted
	maxInvitesPer = 3         // ... from one of them
	histAskTTL    = 120       // seconds we take relayed history from a member we asked
	maxBan        = 256
	goneSlack     = 120 // clock slack when dropping posts made after a removal
	pendKeep      = 3 * 86400 // a full request list drops requests older than this
	leaveRepeat   = 3 * 86400 // our gl is repeated on reconnects for this long
)

var lateState = struct {
	sync.Mutex
	n map[string]int
}{n: map[string]int{}}

// lateOK reports whether a post by from (no longer a member of g) made at ts
// is a late copy from before it dropped out that we still take.
func lateOK(g Group, from string, ts int64) bool {
	at := goneAt(g, from)
	if at <= 0 || ts <= 0 || ts > at {
		return false
	}
	return time.Now().Unix()-at <= lateGrace
}

// lateTake counts one late post from a former member; false once it had lateMax.
func lateTake(gid, from string) bool {
	lateState.Lock()
	defer lateState.Unlock()
	k := gid + "|" + from
	if lateState.n[k] >= lateMax {
		return false
	}
	if len(lateState.n) > 4096 {
		lateState.n = map[string]int{}
	}
	lateState.n[k]++
	return true
}

// changeTime is the time of a member-list change as the manager who made it
// says ("ts" on gi/gd), when it is plausible; else 0 (= now).
func changeTime(ts int64) int64 {
	now := time.Now().Unix()
	if ts <= 0 || ts > now+300 || now-ts > goneKeep {
		return 0
	}
	return min(ts, now)
}

// noteAdded records when members not in before joined g (before empty: a group
// new to us, whose members were all there before us). Local only.
func noteAdded(g *Group, before []string, at int64) {
	now := time.Now().Unix()
	if at <= 0 || at > now {
		at = now
	}
	if len(before) > 0 {
		for _, m := range g.Members {
			if hasMember(Group{Members: before}, m) {
				continue
			}
			if g.AddAt == nil {
				g.AddAt = map[string]int64{}
			}
			g.AddAt[m] = at
		}
	}
	for m := range g.AddAt {
		if !hasMember(*g, m) {
			delete(g.AddAt, m)
		}
	}
	if len(g.AddAt) == 0 {
		g.AddAt = nil
	}
}

// purgeGoneLocked drops the posts members of before that are no longer in g
// made after they dropped out (GoneAt: by the manager's clock when it told
// us, else when we learnt it). They reached us while we hadn't heard of the
// removal yet. Caller holds a.store.mu.
func (a *App) purgeGoneLocked(g Group, before []string) []map[string]string {
	gone := map[string]int64{}
	for _, m := range before {
		if t := g.GoneAt[m]; !hasMember(g, m) && m != a.addr && t > 0 {
			gone[m] = t
		}
	}
	if len(gone) == 0 {
		return nil
	}
	key := gKey(g.ID)
	var del []map[string]string
	kept := a.store.Messages[:0]
	for _, m := range a.store.Messages {
		if t, ok := gone[m.From]; ok && m.Peer == key && !m.Out && m.Sys == "" && m.TS > t+goneSlack {
			del = append(del, map[string]string{"peer": key, "id": m.ID})
			continue
		}
		kept = append(kept, m)
	}
	a.store.Messages = kept
	return del
}

func (a *App) broadcastDels(del []map[string]string) {
	for _, d := range del {
		a.broadcast("msg_del", d)
	}
}

// noteGoneEach lowers the drop-out times of members of before that left g
// to the times the host/admin gave per member ("gt"). Their list version may
// fold several changes (a merge), so its own time can be later than a
// removal. Local only.
func noteGoneEach(g *Group, before []string, gt map[string]int64) {
	for m, ts := range gt {
		t := changeTime(ts)
		if t <= 0 || hasMember(*g, m) || !hasMember(Group{Members: before}, m) {
			continue
		}
		if g.GoneAt == nil {
			g.GoneAt = map[string]int64{}
		}
		if x, ok := g.GoneAt[m]; !ok || t < x {
			g.GoneAt[m] = t
		}
	}
}

// goneTimes is what the host/an admin puts in its info as "gt": when the
// members that dropped out most recently did (at most 8, within goneKeep).
func goneTimes(g Group) map[string]int64 {
	if len(g.GoneAt) == 0 || !(g.Owner != "") {
		return nil
	}
	type e struct {
		m string
		t int64
	}
	var es []e
	now := time.Now().Unix()
	for m, t := range g.GoneAt {
		if !hasMember(g, m) && t > 0 && now-t <= goneKeep {
			es = append(es, e{m, t})
		}
	}
	sort.Slice(es, func(i, j int) bool { return es[i].t > es[j].t })
	if len(es) > 8 {
		es = es[:8]
	}
	if len(es) == 0 {
		return nil
	}
	out := map[string]int64{}
	for _, x := range es {
		out[x.m] = x.t
	}
	return out
}

// ---- history relays ----

var histAsk = struct {
	sync.Mutex
	at map[string]int64
}{at: map[string]int64{}}

func noteHistAsk(gid, peer string) {
	histAsk.Lock()
	defer histAsk.Unlock()
	if len(histAsk.at) > 4096 {
		histAsk.at = map[string]int64{}
	}
	histAsk.at[gid+"|"+peer] = time.Now().Unix()
}

func histAsked(gid, peer string) bool {
	histAsk.Lock()
	defer histAsk.Unlock()
	return time.Now().Unix()-histAsk.at[gid+"|"+peer] <= histAskTTL
}

// histExp is when a relayed post expires: its own timer, and the group's
// counted from when it was posted. ok=false: it already has.
func histExp(g Group, ts int64, ttl int) (exp int64, ok bool) {
	now := time.Now().Unix()
	if ttl > 0 {
		exp = now + int64(ttl)
	}
	if g.Set.TTL > 0 {
		e := ts + int64(g.Set.TTL)
		if ts <= 0 {
			e = now + int64(g.Set.TTL)
		}
		if exp == 0 || e < exp {
			exp = e
		}
	}
	return exp, exp == 0 || exp > now
}

// ---- invitations ----

// inviteRoomLocked reports whether we take one more invitation from from
// (someone we haven't accepted). Caller holds a.store.mu.
func (a *App) inviteRoomLocked(from string) bool {
	all, by := 0, 0
	for _, x := range a.store.Groups {
		if x.Req && !x.Left && !x.Tomb {
			all++
			if x.By == from {
				by++
			}
		}
	}
	return all < maxInvites && by < maxInvitesPer
}

// ---- bans ----

func banned(g Group, addr string) bool { return hasMember(Group{Members: g.Ban}, addr) }

func addBan(g *Group, addrs []string) {
	for _, m := range addrs {
		if !banned(*g, m) {
			g.Ban = append(g.Ban, m)
		}
	}
	if len(g.Ban) > maxBan {
		g.Ban = g.Ban[len(g.Ban)-maxBan:]
	}
}

func unban(g *Group, addr string) {
	var keep []string
	for _, m := range g.Ban {
		if m != addr {
			keep = append(keep, m)
		}
	}
	g.Ban = keep
}

// ---- host hand-over ----

// pickHeir chooses the co-admin a leaving host hands the group to: the one
// we heard from last in this group (most likely online), else the first.
// Caller holds a.store.mu.
func (a *App) pickHeirLocked(g Group) string {
	if len(g.Admins) == 0 {
		return ""
	}
	key := gKey(g.ID)
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Peer == key && !m.Out && isAdmin(g, m.From) {
			return m.From
		}
	}
	ad := append([]string{}, g.Admins...)
	sort.Strings(ad)
	return ad[0]
}

var handAt = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

// handNotice re-sends our hand-over (the info naming the heir as host, from
// us, the old host) and then our leave to the heir, which missed them while
// it was offline. At most once a minute.
func (a *App) handNotice(heir string, g Group) {
	if !g.Hand || !g.Quit || g.Owner != heir {
		return
	}
	handAt.Lock()
	k := g.ID + "|" + heir
	if time.Since(handAt.at[k]) < time.Minute {
		handAt.Unlock()
		return
	}
	if len(handAt.at) > 1024 {
		handAt.at = map[string]time.Time{}
	}
	handAt.at[k] = time.Now()
	handAt.Unlock()
	full := g
	full.Members = cleanMembers(g.Members, a.addr) // the hand-over info lists us; the gl then takes us out
	for _, p := range a.infoPackets(full) {
		a.sendWire(heir, p)
	}
	go func() {
		time.Sleep(3 * time.Second)
		a.sendWire(heir, wire{T: "gl", G: g.ID, TS: time.Now().Unix()})
	}()
}

// repeatLeave sends our gl for group g we left again to everyone (the heir
// gets the hand-over first).
func (a *App) repeatLeave(g Group) {
	lw := wire{T: "gl", G: g.ID, TS: g.QuitAt}
	for _, m := range g.Members {
		if m == a.addr {
			continue
		}
		if g.Hand && m == g.Owner {
			a.handNotice(m, g)
			continue
		}
		a.sendWire(m, lw)
	}
}

// leftNotice answers a packet about a group we are out of: the heir gets our
// hand-over again, anyone else our leave.
func (a *App) leftNotice(peer string, g Group) {
	if !g.Quit {
		return
	}
	if g.Hand && peer == g.Owner {
		a.handNotice(peer, g)
		return
	}
	a.leaveNoticeAt(peer, g.ID, g.QuitAt)
}

// replaceRelayed swaps a relayed version of a post (history from someone
// else, maybe altered) for the sender's own copy.
func (a *App) replaceRelayed(key, from string, w wire) {
	a.store.mu.Lock()
	var upd *Message
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		m := &a.store.Messages[i]
		if m.Peer == key && m.From == from && !m.Out && samePost(m.ID, w.ID) {
			m.Text, m.Via, m.Hist, m.Sg = w.Text, "", false, ""
			if w.Sg != "" && verifyPost(from, w.PK, w.Sg, w.G, postID(w.ID), w.TS, w.Text) {
				m.Sg = w.Sg
			}
			if n := cleanNick(w.Nick); n != "" {
				m.Nick = n
			}
			if w.TS > 0 && w.TS <= time.Now().Unix()+300 {
				m.TS = w.TS
			}
			cp := *m
			upd = &cp
			break
		}
	}
	if upd != nil {
		a.store.save()
	}
	a.store.mu.Unlock()
	if upd != nil {
		// via/hist are spelled out: the UI merges the update into what it has.
		v := map[string]any{"id": upd.ID, "peer": upd.Peer, "from": upd.From, "nick": upd.Nick, "text": upd.Text, "ts": upd.TS, "via": "", "hist": false}
		a.broadcast("msg_upd", v)
	}
}
