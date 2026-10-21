// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Large groups and co-admins (v0.19).
//
// Member limit: the host picks 24..1024 ("mx" in the settings). A member
// list of more than ~20 addresses no longer fits one 1280-byte datagram, so
// the group info is then sent as a header plus parts:
//
//	{"t":"gi","g","gn","go","gs","gv","gad","gc","gh"}   header, no "gm"
//	{"t":"gp","g","gv","gh","i","n","gmb"}               part i of n: up to
//	                                                     40 packed addresses
//
// gh is the hash of name+members; a member whose own list already has that
// hash needs no parts. Pre-v0.19 nodes ignore a gi without "gm", so groups
// that big need v0.19 everywhere (the UI says so).
//
// Membership changes are sent as deltas so a join in a group of 1000 is not
// 1000 full lists:
//
//	{"t":"gd","g","gv","ga","gr","gh"}   members added/removed (packed), new
//	                                     version and resulting hash
//
// gv is the member-list version: it goes up by one on every change; a list
// or delta older than ours is ignored. New members get the full info.
//
// Co-admins ("gad", at most 10, set by the host): they can let people in
// through their own invite link (their own token), approve requests and add
// or remove ordinary members. Settings, the password, admins and the host
// stay with the host. The host sends each admin the password hash:
//
//	{"t":"gk","g","pw"}                  host -> admin (pw = hash, "" = none)
//	{"t":"gkq","g"}                      admin -> host: please send gk
package main

import (
	"encoding/base64"
	"encoding/json"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxGroupCap     = 1024 // hard cap of any group
	defaultGroupMax = 24
	maxAdmins       = 10
	giMaxBytes      = 1180 // a classic gi (full list inline) must fit one datagram
	partAddrs       = 40   // addresses per gp / gd datagram (640 bytes packed)
	nostrGroupMax   = 64   // bigger groups don't park copies on Nostr
	maxStubs        = 50   // placeholder groups made from strangers' messages
	copyTries       = 3    // direct tries per member copy before it waits in the queue
	maxPartBufs     = 64   // split group infos being assembled at once
	maxPartBufsPer  = 4    // ... from one sender
)

// groupSem limits how many member copies are being delivered at once.
var groupSem = make(chan struct{}, 48)

// A copy gets only a few direct tries; then it waits in the queue (hello /
// flush retry it like any queued message), so offline members don't hold the
// slots other posts need (v0.19.2).
func (a *App) deliverCopy(m Message) {
	groupSem <- struct{}{}
	defer func() { <-groupSem }()
	a.deliverN(m, copyTries)
}

// pruneSentCopies drops delivered hidden group copies kept by versions before v0.19. Caller holds s.mu.
func (s *Store) pruneSentCopies() {
	kept := s.Messages[:0]
	for _, m := range s.Messages {
		if !(m.Hid && m.Status == "sent") {
			kept = append(kept, m)
		}
	}
	s.Messages = kept
}

var groupMaxChoices = []int{24, 32, 50, 64, 100, 128, 256, 512, 1024}

func groupMax(g Group) int {
	if g.Set.Max >= defaultGroupMax && g.Set.Max <= maxGroupCap {
		return g.Set.Max
	}
	return defaultGroupMax
}

func validGroupMax(n int) bool {
	for _, c := range groupMaxChoices {
		if c == n {
			return true
		}
	}
	return false
}

func isAdmin(g Group, addr string) bool {
	for _, x := range g.Admins {
		if x == addr {
			return true
		}
	}
	return false
}

// canManage: may let people in and add/remove ordinary members.
func canManage(g Group, addr string) bool {
	return g.Owner != "" && (addr == g.Owner || isAdmin(g, addr))
}

// cleanAdmins keeps distinct members that aren't the host (capped).
func cleanAdmins(in []string, g Group) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if a, ok := yggAddr(s); ok && !seen[a] && a != g.Owner && hasMember(g, a) {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	if len(out) > maxAdmins {
		out = out[:maxAdmins]
	}
	return out
}

func packAddrs(as []string) string {
	b := make([]byte, 0, 16*len(as))
	for _, a := range as {
		if ip := net.ParseIP(a).To16(); ip != nil {
			b = append(b, ip...)
		}
	}
	return base64.RawStdEncoding.EncodeToString(b)
}

func unpackAddrs(s string) []string {
	b, err := base64.RawStdEncoding.DecodeString(s)
	if err != nil || len(b)%16 != 0 || len(b) > 16*partAddrs {
		return nil
	}
	var out []string
	for i := 0; i < len(b); i += 16 {
		if a, ok := yggAddr(net.IP(b[i : i+16]).String()); ok {
			out = append(out, a)
		}
	}
	return out
}

func chunkAddrs(as []string) [][]string {
	var out [][]string
	for len(as) > partAddrs {
		out = append(out, as[:partAddrs])
		as = as[partAddrs:]
	}
	return append(out, as)
}

// infoPackets is the group info as datagrams: the classic gi when it fits,
// else a header (first) plus parts.
func (a *App) infoPackets(g Group) []wire {
	w := a.infoWire(g)
	if b, _ := json.Marshal(w); len(b) <= giMaxBytes {
		return []wire{w}
	}
	w.GM = nil
	w.GC = len(g.Members)
	w.GH = groupHash(g.Name, g.Members)
	out := []wire{w}
	parts := chunkAddrs(g.Members)
	for i, p := range parts {
		out = append(out, wire{T: "gp", G: g.ID, GV: g.V, GH: w.GH, I: i, N: len(parts), GMB: packAddrs(p)})
	}
	return out
}

func classicFits(ps []wire) bool { return len(ps) == 1 }

// sendPackets sends ps to every address in to, 3 rounds, paced for big lists.
func (a *App) sendPackets(to []string, ps []wire) {
	if len(ps) == 0 || len(to) == 0 {
		return
	}
	go func() {
		for i := 0; i < 3; i++ {
			n := 0
			for _, m := range to {
				if m == a.addr {
					continue
				}
				for _, p := range ps {
					a.sendWire(m, p)
					if n++; n%64 == 0 {
						time.Sleep(20 * time.Millisecond)
					}
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

// sendInfo pushes the group info (name, settings, admins) to every member.
// When the list is too big for one datagram only the header goes to the
// members (they ask with gq if their list differs); extra get everything.
func (a *App) sendInfo(g Group, extra ...string) {
	ps := a.infoPackets(g)
	if classicFits(ps) {
		a.sendPackets(append(append([]string{}, g.Members...), extra...), ps)
		return
	}
	a.sendPackets(g.Members, ps[:1])
	a.sendPackets(extra, ps)
}

// sendMembers tells everyone about a membership change made by us. kick:
// the removed were removed by a manager (not a leave), so the other managers
// refuse their link joins too. The classic gi also carries what was removed
// (gr), so a host merging two admins' changes made from the same version
// keeps both removals (v0.19.3).
func (a *App) sendMembers(g Group, added, removed []string, kick bool) {
	ps := a.infoPackets(g)
	if classicFits(ps) {
		if len(removed) > 0 {
			x := ps[0]
			x.GR = packAddrs(removed)
			if kick {
				x.K = "kick"
			}
			if wireLen(x) <= dgramMax {
				ps = []wire{x}
			}
		}
		a.sendPackets(append(append([]string{}, g.Members...), removed...), ps)
		return
	}
	var old []string
	for _, m := range g.Members {
		in := false
		for _, x := range added {
			in = in || x == m
		}
		if !in {
			old = append(old, m)
		}
	}
	gh := groupHash(g.Name, g.Members)
	var ds []wire
	for _, c := range chunkAddrs(added) {
		if len(c) > 0 {
			ds = append(ds, wire{T: "gd", G: g.ID, GV: g.V, GA: packAddrs(c), GH: gh, TS: g.VAt})
		}
	}
	for _, c := range chunkAddrs(removed) {
		if len(c) > 0 {
			d := wire{T: "gd", G: g.ID, GV: g.V, GR: packAddrs(c), GH: gh, TS: g.VAt}
			if kick {
				d.K = "kick"
			}
			ds = append(ds, d)
		}
	}
	a.sendPackets(append(old, removed...), ds)
	a.sendPackets(added, ps)
}

// ---- receiving the split info ----

type partBuf struct {
	hdr   *wire
	v     int
	gh    string
	n     int
	parts map[int][]string
	at    time.Time
}

type gpState struct {
	mu   sync.Mutex
	bufs map[string]*partBuf // gid|from
	qAt  map[string]time.Time
	rAt  map[string]time.Time
	lAt  map[string]time.Time // leave notices sent
}

var gps = &gpState{bufs: map[string]*partBuf{}, qAt: map[string]time.Time{}, rAt: map[string]time.Time{}, lAt: map[string]time.Time{}}

// leaveNotice re-sends gl to a peer that still treats us as a member of a
// group we left (it missed our gl), at most every 5 minutes (v0.19.2).
func (a *App) leaveNotice(peer, gid string) {
	gps.mu.Lock()
	k := gid + "|" + peer
	if time.Since(gps.lAt[k]) < 5*time.Minute {
		gps.mu.Unlock()
		return
	}
	if len(gps.lAt) > 4096 {
		gps.lAt = map[string]time.Time{}
	}
	gps.lAt[k] = time.Now()
	gps.mu.Unlock()
	a.sendWire(peer, wire{T: "gl", G: gid})
}

// askInfo sends gq to peer, at most every 15 s per group.
func (a *App) askInfo(peer, gid string) {
	gps.mu.Lock()
	k := gid + "|" + peer
	if time.Since(gps.qAt[k]) < 15*time.Second {
		gps.mu.Unlock()
		return
	}
	if len(gps.qAt) > 4096 {
		gps.qAt = map[string]time.Time{} // strangers can make keys up (v0.19.3)
	}
	gps.qAt[k] = time.Now()
	gps.mu.Unlock()
	a.sendWire(peer, wire{T: "gq", G: gid})
}

// answerInfo replies to gq with the full info, at most every 10 s per asker.
func (a *App) answerInfo(peer string, g Group) {
	gps.mu.Lock()
	k := g.ID + "|" + peer
	if time.Since(gps.rAt[k]) < 10*time.Second {
		gps.mu.Unlock()
		return
	}
	if len(gps.rAt) > 4096 {
		gps.rAt = map[string]time.Time{}
	}
	gps.rAt[k] = time.Now()
	gps.mu.Unlock()
	for _, p := range a.infoPackets(g) {
		a.sendWire(peer, p)
	}
}

func (a *App) partBufFor(from string, gid string, v int, gh string) *partBuf {
	k := gid + "|" + from
	b := gps.bufs[k]
	if b == nil || b.v != v || b.gh != gh || time.Since(b.at) > time.Minute {
		b = &partBuf{v: v, gh: gh, parts: map[int][]string{}, at: time.Now()}
		// Drop stale buffers, and keep their number bounded: anyone can send
		// parts for made-up group ids (v0.19.3).
		mine, oldK, oldMine := 0, "", ""
		var oldAt, oldMineAt time.Time
		for kk, x := range gps.bufs {
			if time.Since(x.at) > 2*time.Minute {
				delete(gps.bufs, kk)
				continue
			}
			if oldK == "" || x.at.Before(oldAt) {
				oldK, oldAt = kk, x.at
			}
			if strings.HasSuffix(kk, "|"+from) {
				mine++
				if oldMine == "" || x.at.Before(oldMineAt) {
					oldMine, oldMineAt = kk, x.at
				}
			}
		}
		if mine >= maxPartBufsPer && oldMine != "" {
			delete(gps.bufs, oldMine)
		} else if len(gps.bufs) >= maxPartBufs && oldK != "" {
			delete(gps.bufs, oldK)
		}
		gps.bufs[k] = b
	}
	return b
}

// onInfoHeader handles a gi without the inline list. No lock held.
func (a *App) onInfoHeader(from string, w wire) {
	if w.GC <= 0 || w.GC > maxGroupCap || w.GH == "" {
		return
	}
	a.store.mu.Lock()
	g, known := a.store.Groups[w.G]
	same := known && groupHash(cleanNickOr(w.GN), g.Members) == w.GH
	a.store.mu.Unlock()
	if same {
		w.GM = g.Members
		a.onGroupCtl(from, w)
		return
	}
	gps.mu.Lock()
	b := a.partBufFor(from, w.G, w.GV, w.GH)
	first := b.hdr == nil
	hw := w
	b.hdr = &hw
	full := a.partsDoneLocked(b)
	gps.mu.Unlock()
	if full != nil {
		a.onGroupCtl(from, *full)
		return
	}
	if !first {
		return // a repeat: the check below is already scheduled
	}
	// Parts usually follow right away; if not, ask for them.
	time.AfterFunc(5*time.Second, func() {
		gps.mu.Lock()
		k := w.G + "|" + from
		x := gps.bufs[k]
		missing := x == b && x.hdr != nil
		gps.mu.Unlock()
		if missing {
			a.askInfo(from, w.G)
		}
	})
}

func (a *App) onInfoPart(from string, w wire) {
	if !validGID(w.G) || w.N <= 0 || w.N > (maxGroupCap+partAddrs-1)/partAddrs || w.I < 0 || w.I >= w.N {
		return
	}
	as := unpackAddrs(w.GMB)
	if as == nil {
		return
	}
	gps.mu.Lock()
	b := a.partBufFor(from, w.G, w.GV, w.GH)
	if b.n == 0 {
		b.n = w.N
	}
	if b.n != w.N {
		gps.mu.Unlock()
		return
	}
	b.parts[w.I] = as
	full := a.partsDoneLocked(b)
	gps.mu.Unlock()
	if full != nil {
		a.onGroupCtl(from, *full)
	}
}

// partsDoneLocked returns the assembled gi once header and all parts are in. gps.mu held.
func (a *App) partsDoneLocked(b *partBuf) *wire {
	if b.hdr == nil || b.n == 0 || len(b.parts) < b.n {
		return nil
	}
	var ms []string
	for i := 0; i < b.n; i++ {
		ms = append(ms, b.parts[i]...)
	}
	w := *b.hdr
	if len(ms) != w.GC || groupHash(cleanNickOr(w.GN), ms) != w.GH {
		return nil
	}
	w.GM = ms
	w.GC = 0
	for k, x := range gps.bufs {
		if x == b {
			delete(gps.bufs, k)
		}
	}
	return &w
}

func cleanNickOr(n string) string {
	if n = cleanNick(n); n == "" {
		return "グループ"
	}
	return n
}

// onDelta applies a membership change from the host or an admin.
// Caller holds a.store.mu; released here.
func (a *App) onDelta(from string, w wire, g Group, known bool) {
	// A host that left (no heir) is still the Owner on file but no longer runs the group (v0.19.3).
	if !known || g.Left || !canManage(g, from) || !hasMember(g, from) || !hasMember(g, a.addr) {
		a.store.mu.Unlock()
		return
	}
	host := g.Owner == a.addr
	if !host && from != g.Owner && w.GV > g.V+maxVJump {
		// An admin far ahead of us: the host's list decides, so an admin
		// can't push the version out of reach (v0.19.3).
		a.store.mu.Unlock()
		a.askInfo(g.Owner, g.ID)
		return
	}
	// An admin's delta the host gets late or from an old base (v <= ours) is
	// merged into a new version: its additions (not the ones the host removed
	// since), and its removals only when it is the same version (v0.19.2).
	merge := host && w.GV <= g.V
	if w.GV < g.V && !merge {
		a.store.mu.Unlock()
		return
	}
	add, rm := unpackAddrs(w.GA), unpackAddrs(w.GR)
	if merge {
		add = mergeAdds(g, add)
		if w.GV < g.V {
			rm = nil
		}
	}
	byHost := from == g.Owner
	old := g
	var added, removed []string
	ms := append([]string{}, g.Members...)
	for _, x := range add {
		if !hasMember(Group{Members: ms}, x) && len(ms) < maxGroupCap {
			ms = append(ms, x)
			added = append(added, x)
		}
	}
	out := false
	kept := ms[:0:0]
	for _, m := range ms {
		drop := false
		for _, x := range rm {
			if x == m && (byHost || (m != g.Owner && !isAdmin(g, m))) {
				drop = true
			}
		}
		if drop {
			removed = append(removed, m)
			out = out || m == a.addr
		} else {
			kept = append(kept, m)
		}
	}
	if len(added) == 0 && len(removed) == 0 && w.GV <= g.V {
		a.store.mu.Unlock() // a repeat of what we have
		return
	}
	if !byHost && len(kept) > groupMax(g) {
		// An admin can't take the group over its limit; the host decides (v0.19.3).
		a.store.mu.Unlock()
		if !host {
			a.askInfo(g.Owner, g.ID)
		}
		return
	}
	if !host && w.GV == g.V {
		// Same version, different change: keep ours, let the host merge.
		a.store.mu.Unlock()
		if from == g.Owner {
			a.answerInfo(g.Owner, g)
		} else {
			a.askInfo(g.Owner, g.ID)
		}
		return
	}
	sort.Strings(kept)
	g.Members = kept
	g.Admins = cleanAdmins(g.Admins, g)
	at := int64(0)
	if merge {
		bumpV(&g)
	} else {
		g.V = w.GV
		at = changeTime(w.TS)
		g.VAt = at // 0 = unknown (an older sender): never pass on a stale time
	}
	if host {
		trackGone(&g, old.Members)
	}
	noteGone(&g, old.Members, at)
	dels := a.purgeGoneLocked(g, old.Members, at) // what they posted after the removal reached us late
	kick := w.K == "kick" || (merge && len(removed) > 0)
	if kick && canManage(g, a.addr) {
		addBan(&g, removed)
	}
	for _, x := range added {
		g.Pend = dropPend(g.Pend, x)
	}
	if out {
		g.Left = true
	}
	a.store.Groups[g.ID] = g
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("contact", groupView(g))
	a.broadcastDels(dels)
	if out {
		a.groupSys(g, "グループから外されました")
		go a.failLeftCopies(g.ID)
		return
	}
	a.memberSys(g, from, added, removed)
	if merge {
		a.sendMembers(g, added, removed, kick) // everyone (the admin too) gets the merged version
		return
	}
	// The host re-sends the classic list so pre-v0.19 members learn an admin's change.
	if a.addr == g.Owner && !byHost && (len(added) > 0 || len(removed) > 0) {
		if ps := a.infoPackets(g); classicFits(ps) {
			a.sendPackets(append(append([]string{}, g.Members...), removed...), ps)
		}
	}
	gid, v, gh := g.ID, w.GV, w.GH
	time.AfterFunc(6*time.Second, func() {
		a.store.mu.Lock()
		x, ok := a.store.Groups[gid]
		bad := ok && !x.Left && x.V == v && groupHash(x.Name, x.Members) != gh
		a.store.mu.Unlock()
		if bad {
			a.askInfo(from, gid)
		}
	})
}

// memberSys writes the "X added Y" lines (summarised for many).
func (a *App) memberSys(g Group, by string, added, removed []string) {
	who := a.gNameOf(g, by)
	if by == a.addr {
		who = "あなた"
	}
	line := func(list []string, verb string) {
		var others []string
		for _, m := range list {
			if m != a.addr && m != by {
				others = append(others, m)
			}
		}
		if len(others) > 5 {
			a.groupSys(g, who+" が "+itoa(len(others))+"人を"+verb+"ました")
			return
		}
		for _, m := range others {
			a.groupSys(g, who+" が "+a.gNameOf(g, m)+" を"+verb+"ました")
		}
	}
	line(added, "追加し")
	line(removed, "外し")
}

// adminSys writes the lines for admins added/removed.
func (a *App) adminSys(g Group, by string, old, now []string) {
	has := func(l []string, x string) bool {
		for _, y := range l {
			if y == x {
				return true
			}
		}
		return false
	}
	who := a.gNameOf(g, by)
	for _, x := range now {
		if !has(old, x) {
			a.groupSys(g, who+" が "+a.gNameOf(g, x)+" を副管理者にしました")
		}
	}
	for _, x := range old {
		if !has(now, x) && hasMember(g, x) {
			a.groupSys(g, who+" が "+a.gNameOf(g, x)+" を副管理者から外しました")
		}
	}
}

// sendKey gives admins the join-password hash.
func (a *App) sendKey(g Group, to []string) {
	w := wire{T: "gk", G: g.ID, Pw: g.PwH}
	if g.PwH == "" {
		w.K = "none"
	}
	a.sendPackets(to, []wire{w})
}

// onKey: an admin stores the host's password hash. Caller holds a.store.mu; released here.
func (a *App) onKey(from string, w wire, g Group, known bool) {
	if !known || g.Left || g.Owner == "" || from != g.Owner || !isAdmin(g, a.addr) || len(w.Pw) > 200 {
		a.store.mu.Unlock()
		return
	}
	if w.Pw == "" || w.K == "none" {
		g.PwH = ""
	} else {
		g.PwH = w.Pw
	}
	a.store.Groups[g.ID] = g
	a.store.save()
	a.store.mu.Unlock()
}

// waitAck registers a wait for the ack of id from peer.
func (a *App) waitAck(peer, id string) chan struct{} {
	ch := make(chan struct{})
	a.ackMu.Lock()
	if a.ackFrom == nil {
		a.ackFrom = map[string]string{}
	}
	a.acks[id] = ch
	a.ackFrom[id] = peer
	a.ackMu.Unlock()
	return ch
}

func (a *App) doneAck(id string, ch chan struct{}) {
	a.ackMu.Lock()
	if a.acks[id] == ch {
		delete(a.acks, id)
		delete(a.ackFrom, id)
	}
	a.ackMu.Unlock()
}

// dropCopy removes one hidden group copy and updates its group message's status.
func (a *App) dropCopy(peer, id string) {
	a.store.mu.Lock()
	grp, of := "", ""
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Hid && m.Peer == peer && m.ID == id {
			grp, of = m.Grp, m.Of
			a.store.Messages = append(a.store.Messages[:i], a.store.Messages[i+1:]...)
			a.store.saveSoon()
			break
		}
	}
	a.store.mu.Unlock()
	if grp != "" {
		a.groupStatus(grp, of)
	}
}
