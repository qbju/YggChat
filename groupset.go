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
	"net/http"
	"strings"
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
	g.Members = cleanMembers(append(append([]string{}, g.Members...), from), a.addr)
	g.Pend = dropPend(g.Pend, from)
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
	a.sendInfo(g)
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
	time.Sleep(3 * time.Second) // let the gi land first
	now := time.Now().Unix()
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
		if len(text) > histText {
			cut := histText
			for cut > 0 && !utf8Start(text[cut]) {
				cut--
			}
			text = text[:cut] + "…（省略）"
		}
		w := wire{T: "ghs", G: gid, ID: m.ID, By: m.From, Nick: m.Nick, Text: text, TS: m.TS}
		if m.Exp > 0 {
			w.TTL = int(m.Exp - now)
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
	if !known || g.Left || g.Owner == "" || g.Owner != from || !g.Set.Hist || !ok || w.Text == "" || len(w.Text) > 2000 || !msgIDRe.MatchString(w.ID) || w.TTL < 0 {
		a.store.mu.Unlock()
		return
	}
	key := gKey(g.ID)
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Peer == key && m.From == by && m.ID == w.ID {
			a.store.mu.Unlock()
			return
		}
	}
	a.store.mu.Unlock()
	if by == a.addr {
		return
	}
	m := Message{ID: w.ID, Peer: key, From: by, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, Hist: true}
	if w.TTL > 0 {
		m.Exp = time.Now().Unix() + int64(w.TTL)
	}
	a.addMessage(m)
}

func (a *App) groupSetHandlers(mux *http.ServeMux) {
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
		if !ok || g.Left || a.store.locked || g.Owner != a.addr {
			a.store.mu.Unlock()
			http.Error(w, "承認できるのはホストだけです", 403)
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
		if len(g.Members) >= maxGroupMembers {
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
