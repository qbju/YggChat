// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group settings (v0.18). The host decides; members keep the copy that comes
// in the host's gi ("gs") and every device enforces it on its own side:
//
//	nf   photos off: senders can't attach, receivers drop photos
//	ap   link joins wait in the host's list until approved
//	hi   a new member gets the last messages from the host ("ghs")
//	ttl  disappearing messages for the whole group
//	pw   the invite link also needs a password (hash on the host only)
//
//	{"t":"gj","g","tok","nick","pw"}        join request (pw only when asked)
//	{"t":"gjn","g","text","k"}              refusal; k="pw" asks for the password,
//	                                        k="wait" means waiting for approval
//	{"t":"ghs","g","id","by","nick","text","ts","ttl"}  one history message
//
// History is relayed by the host, so members trust the host for it (the
// same trust they already give it for the member list). Each history
// message is one datagram, so long texts are shortened.
package main

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	mrand "math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	pwIter     = 100000
	histMax    = 30  // messages a new member gets
	histText   = 700 // bytes of text per history datagram
	maxPending = 20
)

var groupTTLs = map[int]bool{0: true, 300: true, 3600: true, 86400: true, 604800: true, 2419200: true}

func cleanSet(s GroupSet) GroupSet {
	if !groupTTLs[s.TTL] {
		s.TTL = 0
	}
	if !groupPowChoices[s.PoW] {
		s.PoW = 0
	}
	if !validGroupMax(s.Max) || s.Max == defaultGroupMax {
		s.Max = 0
	}
	return s
}

func ttlText(s int) string {
	switch s {
	case 0:
		return "オフ"
	case 300:
		return "5分"
	case 3600:
		return "1時間"
	case 86400:
		return "1日"
	case 604800:
		return "1週間"
	case 2419200:
		return "4週間"
	}
	return "オン"
}

func onOff(b bool) string {
	if b {
		return "オン"
	}
	return "オフ"
}

// setDiff describes what changed, as "…にしました" phrases.
func setDiff(a, b GroupSet) []string {
	var out []string
	if a.NoFiles != b.NoFiles {
		out = append(out, "写真の送信を"+onOff(!b.NoFiles)+"にしました")
	}
	if a.Approve != b.Approve {
		out = append(out, "参加の承認を"+onOff(b.Approve)+"にしました")
	}
	if a.Hist != b.Hist {
		out = append(out, "新メンバーへの履歴共有を"+onOff(b.Hist)+"にしました")
	}
	if a.TTL != b.TTL {
		out = append(out, "消えるメッセージを「"+ttlText(b.TTL)+"」にしました")
	}
	if a.Pass != b.Pass {
		out = append(out, "参加パスワードを"+onOff(b.Pass)+"にしました")
	}
	if groupMax(Group{Set: a}) != groupMax(Group{Set: b}) {
		out = append(out, "人数の上限を"+itoa(groupMax(Group{Set: b}))+"人にしました")
	}
	if groupPow(Group{Set: a}) != groupPow(Group{Set: b}) {
		if n := groupPow(Group{Set: b}); n == 0 {
			out = append(out, "投稿のPoWをなしにしました")
		} else {
			out = append(out, "投稿のPoWを"+itoa(n)+"ビットにしました")
		}
	}
	return out
}

// hashGroupPass: "p1:<salt hex>:<PBKDF2-SHA256 hex>" (same format in the PWA).
func hashGroupPass(p string) string {
	salt := make([]byte, 16)
	_, _ = rand.Read(salt)
	k, _ := pbkdf2.Key(sha256.New, p, salt, pwIter, 32)
	return "p1:" + hex.EncodeToString(salt) + ":" + hex.EncodeToString(k)
}

func checkGroupPass(h, p string) bool {
	parts := strings.Split(h, ":")
	if len(parts) != 3 || parts[0] != "p1" || p == "" {
		return false
	}
	salt, err1 := hex.DecodeString(parts[1])
	want, err2 := hex.DecodeString(parts[2])
	if err1 != nil || err2 != nil {
		return false
	}
	k, err := pbkdf2.Key(sha256.New, p, salt, pwIter, len(want))
	return err == nil && subtle.ConstantTimeCompare(k, want) == 1
}

// pwFail counts wrong passwords per address (in memory), 5 per 10 minutes.
type pwFail struct {
	n  int
	at time.Time
}

// pwGlobalOK caps password checks at 20 a minute overall (addresses are free to make).
var pwGlobal struct {
	sync.Mutex
	n  int
	at time.Time
}

func pwGlobalOK() bool {
	pwGlobal.Lock()
	defer pwGlobal.Unlock()
	if time.Since(pwGlobal.at) > time.Minute {
		pwGlobal.n, pwGlobal.at = 0, time.Now()
	}
	pwGlobal.n++
	return pwGlobal.n <= 20
}

func (a *App) pwLimited(from string, failed bool) bool {
	a.pwMu.Lock()
	defer a.pwMu.Unlock()
	if a.pwFails == nil {
		a.pwFails = map[string]*pwFail{}
	}
	f := a.pwFails[from]
	if f == nil || time.Since(f.at) > 10*time.Minute {
		f = &pwFail{at: time.Now()}
		a.pwFails[from] = f
	}
	if failed {
		f.n++
	}
	return f.n >= 5
}

// admit adds from to group g on the host (after token, password and
// approval), tells everyone and shares the history if that is on.
// Caller holds a.store.mu; it is released here.
func (a *App) admit(g Group, from, nick, how string) {
	before := g.Members
	g.Members = cleanMembers(append(append([]string{}, g.Members...), from), a.addr)
	if g.Owner == a.addr {
		trackGone(&g, before)
	}
	noteGone(&g, before, 0)
	g.Pend = dropPend(g.Pend, from)
	unban(&g, from) // let in again (approved, or a direct add)
	bumpV(&g)
	a.store.Groups[g.ID] = g
	a.store.save()
	var hist []Message
	if g.Set.Hist {
		hist = a.historyLocked(g.ID)
	}
	a.store.mu.Unlock()
	a.broadcast("contact", groupView(g))
	if nick == "" {
		nick = a.nameOf(from)
	}
	a.groupSys(g, nick+" が"+how)
	a.sendMembers(g, []string{from}, nil, false)
	if len(hist) > 0 {
		go a.sendHistory(from, g.ID, hist)
	}
}

func dropPend(ps []Pend, addr string) []Pend {
	out := []Pend{}
	for _, p := range ps {
		if p.Addr != addr {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// historyLocked returns the last text messages of a group, oldest first. Caller holds a.store.mu.
func (a *App) historyLocked(gid string) []Message {
	var out []Message
	now := time.Now().Unix()
	for i := len(a.store.Messages) - 1; i >= 0 && len(out) < histMax; i-- {
		m := a.store.Messages[i]
		if m.Peer != gKey(gid) || m.Hid || m.Sys != "" || (m.Exp > 0 && m.Exp <= now) || (m.Text == "" && m.Img == "") {
			continue
		}
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func (a *App) sendHistory(to, gid string, hist []Message) {
	defer func() { recover() }() // a bad message must never take the node down
	time.Sleep(3 * time.Second) // let the gi land first
	now := time.Now().Unix()
	sigs := map[string][2]string{}
	a.store.mu.Lock()
	g := a.store.Groups[gid]
	for _, m := range hist {
		if sg, pk := a.sigForLocked(g, m); sg != "" && pk != "" {
			sigs[m.From+"|"+m.ID] = [2]string{sg, pk}
		}
	}
	a.store.mu.Unlock()
	ws := make([]wire, 0, len(hist))
	for _, m := range hist {
		text := m.Text
		if m.Img != "" {
			if text == "" {
				text = "📷 写真"
			} else {
				text = "📷 " + text
			}
		}
		// The post id, not our copy's "<post>g<i>": every member then has the same one to dedupe on (v0.19.2).
		w := wire{T: "ghs", G: gid, ID: postID(m.ID), By: m.From, Nick: m.Nick, Text: text, TS: m.TS}
		if s, ok := sigs[m.From+"|"+m.ID]; ok && m.Img == "" {
			w.Sg, w.PK = s[0], s[1] // the sender's proof, valid only for the whole text
		}
		if m.Exp > 0 {
			r := m.Exp - now - 20 // it takes a few seconds to send
			if r <= 0 {
				continue
			}
			w.TTL = int(r)
		}
		// Shorten until the datagram fits (JSON escapes can triple a text).
		if w.Sg != "" && wireLen(w) > dgramMax {
			w.Sg, w.PK = "", "" // it will be shortened: the signature no longer fits the text
		}
		for cut := min(len(text), histText); wireLen(w) > dgramMax && cut > 0; cut -= 60 {
			for cut > 0 && cut < len(text) && !utf8Start(text[cut]) {
				cut--
			}
			w.Text = text[:cut] + "…（省略）"
		}
		if wireLen(w) > dgramMax {
			continue
		}
		ws = append(ws, w)
	}
	for round := 0; round < 2; round++ {
		for _, w := range ws {
			a.sendWire(to, w)
			time.Sleep(60 * time.Millisecond)
		}
		time.Sleep(4 * time.Second)
	}
}

func utf8Start(b byte) bool { return b&0xc0 != 0x80 }

// onHistory stores one history message from the host. Caller holds a.store.mu; released here.
func (a *App) onHistory(from string, w wire, g Group, known bool) {
	by, ok := yggAddr(w.By)
	// Any member may relay what it has (it stands in as the mailbox for
	// members who were away); only the sender's own copy and the host's or
	// admins' are unmarked, the rest show who relayed them.
	if !known || g.Left || !hasMember(g, from) || !ok || w.Text == "" || len(w.Text) > 2000 || !msgIDRe.MatchString(w.ID) || w.TTL < 0 {
		a.store.mu.Unlock()
		return
	}
	// An ordinary member relays only when we asked it (ghq); the host and
	// admins also send history to new members unasked (v0.19.3).
	if !canManage(g, from) && !histAsked(g.ID, from) {
		a.store.mu.Unlock()
		return
	}
	if !hasMember(g, by) {
		at := goneAt(g, by)
		if at != 0 && (at < 0 || w.TS <= 0 || w.TS > at) {
			a.store.mu.Unlock() // posted after they left or were removed (v0.19.2)
			return
		}
		if at == 0 && !canManage(g, from) {
			// Someone who was never a member as far as we know: only the
			// host or an admin (history from before we joined) may relay
			// such a post, an ordinary member can't make up posts by
			// strangers (v0.19.3).
			a.store.mu.Unlock()
			return
		}
	}
	key := gKey(g.ID)
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Peer == key && m.From == by && samePost(m.ID, w.ID) {
			a.store.mu.Unlock()
			return
		}
	}
	knownSigner := g.Keys[by] != ""
	a.store.mu.Unlock()
	if by == a.addr {
		return
	}
	// The sender's signature (groupssig.go): a bad one is a forgery; a good
	// one is its own words, whoever relays it (v0.19.3).
	signed := false
	if w.Sg != "" {
		if !verifyPost(by, w.PK, w.Sg, g.ID, postID(w.ID), w.TS, w.Text) {
			return
		}
		signed = true
	}
	m := Message{ID: postID(w.ID), Peer: key, From: by, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, Hist: true}
	if signed {
		m.Sg = w.Sg
		if !knownSigner && hasMember(g, by) {
			a.noteSigner(g.ID, by, w.PK) // so we can pass the proof on too
		}
	}
	// Unsigned relays show who relayed them; the host's and admins' too when
	// the sender is known to sign (an unsigned post "by" it is not its own).
	if by != from && !signed && (!canManage(g, from) || knownSigner) {
		m.Via = from
	}
	if m.TS <= 0 || m.TS > time.Now().Unix()+300 {
		m.TS = time.Now().Unix()
	}
	// The group's timer applies to relayed posts too, counted from when they were posted (v0.19.3).
	exp, live := histExp(g, m.TS, w.TTL)
	if !live {
		return
	}
	m.Exp = exp
	if exp > 0 {
		m.TTL = int(exp - m.TS)
		if m.TTL <= 0 {
			m.TTL = int(exp - time.Now().Unix())
		}
	}
	a.addMessage(m)
}

func (a *App) groupSetHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/group/sync", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		if !readJSON(w, r, &req) {
			return
		}
		go a.syncGroup(req.ID)
		w.Write([]byte("{}"))
	})
	// The host answers a join request waiting for approval.
	mux.HandleFunc("/api/group/approve", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID   string
			Addr string
			OK   bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		addr, _ := yggAddr(req.Addr)
		a.store.mu.Lock()
		g, ok := a.store.Groups[req.ID]
		if !ok || g.Left || a.store.locked || !canManage(g, a.addr) {
			a.store.mu.Unlock()
			http.Error(w, "承認できるのはホストと副管理者だけです", 403)
			return
		}
		var p *Pend
		for i := range g.Pend {
			if g.Pend[i].Addr == addr {
				p = &g.Pend[i]
			}
		}
		if p == nil {
			a.store.mu.Unlock()
			http.Error(w, "その申請はありません", 400)
			return
		}
		pend := *p
		if !req.OK {
			g.Pend = dropPend(g.Pend, addr)
			a.store.Groups[g.ID] = g
			a.store.save()
			a.store.mu.Unlock()
			a.broadcast("contact", groupView(g))
			a.sendWire(addr, wire{T: "gjn", G: g.ID, Text: "参加は承認されませんでした"})
			json.NewEncoder(w).Encode(groupView(g))
			return
		}
		if len(g.Members) >= groupMax(g) {
			a.store.mu.Unlock()
			http.Error(w, "グループが満員です", 400)
			return
		}
		a.admit(g, addr, pend.Nick, "参加しました（承認）")
		a.store.mu.Lock()
		g = a.store.Groups[req.ID]
		a.store.mu.Unlock()
		json.NewEncoder(w).Encode(groupView(g))
	})
}

// Catch-up (v0.19.1): a member who opens a group or comes back online asks a
// few other members for the recent messages; whoever is online answers, so
// any member stands in as the group's mailbox.
var (
	histReqMu   sync.Mutex
	histReqSeen = map[string]int64{}
)

func (a *App) onHistReq(from string, w wire, g Group, known bool) {
	if !known || g.Left || !hasMember(g, from) || !hasMember(g, a.addr) || from == a.addr {
		a.store.mu.Unlock()
		if known && !g.Left && !hasMember(g, from) {
			a.goneNotice(from, g) // a removed member catching up: tell it it is out
		} else if known && g.Left {
			a.leftNotice(from, g) // our heir catching up gets the hand-over (v0.19.3)
		}
		return
	}
	// Without history sharing: only what was said since they were added,
	// whatever "since" they ask for (v0.19.3).
	since := w.TS
	if t := g.AddAt[from]; !g.Set.Hist && t > since {
		since = t
	}
	hist := a.historyLocked(g.ID)
	a.store.mu.Unlock()
	now := time.Now().Unix()
	histReqMu.Lock()
	k := from + "/" + g.ID
	if t := histReqSeen[k]; now-t < 20 {
		histReqMu.Unlock()
		return
	}
	histReqSeen[k] = now
	if len(histReqSeen) > 4096 {
		histReqSeen = map[string]int64{}
	}
	histReqMu.Unlock()
	out := hist[:0:0]
	for _, m := range hist {
		// Without history sharing, only what was said since they were in the group.
		if m.From == from || (!g.Set.Hist && m.TS < since) {
			continue
		}
		// Never relay what a removed member posted after its removal.
		if at := goneAt(g, m.From); m.From != a.addr && !hasMember(g, m.From) && at > 0 && m.TS > at+goneSlack {
			continue
		}
		out = append(out, m)
	}
	if len(out) > 0 {
		go a.sendHistory(from, g.ID, out)
	}
}

// syncGroup asks the host, the admins and a few other members for what we missed.
func (a *App) syncGroup(gid string) {
	a.store.mu.Lock()
	g, ok := a.store.Groups[gid]
	if !ok || g.Left || g.Req || a.store.locked {
		a.store.mu.Unlock()
		return
	}
	since := time.Now().Unix()
	for _, m := range a.store.Messages {
		if m.Peer == gKey(gid) && m.TS > 0 && m.TS < since {
			since = m.TS
			break
		}
	}
	a.store.mu.Unlock()
	ask := []string{}
	add := func(x string) {
		if x != "" && x != a.addr && hasMember(g, x) && !hasMember(Group{Members: ask}, x) && len(ask) < 8 {
			ask = append(ask, x)
		}
	}
	add(g.Owner)
	for _, x := range g.Admins {
		add(x)
	}
	for _, i := range mrand.Perm(len(g.Members)) {
		add(g.Members[i])
	}
	for _, x := range ask {
		noteHistAsk(gid, x) // their answer (ghs) is taken for a while
		a.sendWire(x, wire{T: "ghq", G: gid, TS: since})
	}
}

func (a *App) syncAllGroups() {
	a.store.mu.Lock()
	ids := []string{}
	var gone []Group
	for id, g := range a.store.Groups {
		ids = append(ids, id)
		if g.Left && g.Quit && g.QuitAt > 0 && time.Now().Unix()-g.QuitAt < leaveRepeat {
			gone = append(gone, g)
		}
	}
	a.store.mu.Unlock()
	// Groups we left lately: our gl again (it may all have been lost while
	// we were cut off; a member that has it ignores it), v0.19.3.
	for _, g := range gone {
		go a.repeatLeave(g)
	}
	for _, id := range ids {
		a.syncGroup(id)
		time.Sleep(200 * time.Millisecond)
	}
}
