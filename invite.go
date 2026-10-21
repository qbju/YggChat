// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group invite links (v0.17). The host keeps a random token per group; the
// link carries the host's address, the group id and that token:
//
//	code = base64url(host addr 16 B | group id 8 B | token 8 B)  (43 chars)
//	https://yggchat.surge.sh/#j=<code>   or   yggchat://join/<code>
//
// Opening it sends {"t":"gj","g","tok","nick"} to the host. If the token
// matches, the host adds the sender and pushes the group info as usual; else
// it answers {"t":"gjn","g","text"}. Only the host can let people in, so the
// host must be online. Resetting the link makes old links stop working.
package main

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

// joinWait is a pending invite-link join of ours. It is saved (v0.19.2), so
// a restart while waiting for approval still turns the host's gi into
// "joined" instead of a plain invitation.
type joinWait struct {
	Host string `json:"host"`
	Exp  int64  `json:"exp"`            // unix time it is forgotten
	Wait bool   `json:"wait,omitempty"` // the host said "wait for approval"
}

const (
	joinWaitTTL     = 10 * 60      // a join request is answered within seconds
	joinApproveWait = 24 * 60 * 60 // the host said "wait for approval"
)

// joinHostLocked returns the host our join of gid waits for. Caller holds a.store.mu.
func (a *App) joinHostLocked(gid string) (string, bool) {
	j, ok := a.store.Joins[gid]
	if !ok {
		return "", false
	}
	if j.Exp < time.Now().Unix() {
		delete(a.store.Joins, gid)
		return "", false
	}
	return j.Host, true
}

// setJoinLocked remembers a pending join for ttl seconds. Caller holds a.store.mu (and saves).
func (a *App) setJoinLocked(gid, host string, ttl int64) {
	now := time.Now().Unix()
	if a.store.Joins == nil {
		a.store.Joins = map[string]joinWait{}
	}
	for k, j := range a.store.Joins {
		if j.Exp < now {
			delete(a.store.Joins, k)
		}
	}
	a.store.Joins[gid] = joinWait{Host: host, Exp: now + ttl, Wait: ttl > joinWaitTTL}
	a.joinsChangedLocked()
}

// joinsViewLocked lists our pending joins for the UI. Caller holds a.store.mu.
func (a *App) joinsViewLocked() []map[string]any {
	now := time.Now().Unix()
	out := []map[string]any{}
	for gid, j := range a.store.Joins {
		if j.Exp < now {
			continue
		}
		v := map[string]any{"addr": gKey(gid), "host": j.Host, "exp": j.Exp, "wait": j.Wait}
		if c, ok := a.store.Contacts[j.Host]; ok && c.Nick != "" {
			v["hostnick"] = c.Nick
		}
		out = append(out, v)
	}
	sort.Slice(out, func(i, k int) bool { return out[i]["addr"].(string) < out[k]["addr"].(string) })
	return out
}

// joinsChangedLocked tells the UI the pending joins changed. Caller holds a.store.mu.
func (a *App) joinsChangedLocked() { a.broadcast("joins", a.joinsViewLocked()) }

func newGroupToken() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func encodeJoin(host, gid, tok string) string {
	b := make([]byte, 0, 32)
	b = append(b, net.ParseIP(host).To16()...)
	g, _ := hex.DecodeString(gid)
	t, _ := hex.DecodeString(tok)
	b = append(append(b, g...), t...)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeJoin(code string) (host, gid, tok string, err error) {
	b, err := base64.RawURLEncoding.DecodeString(code)
	if err != nil || len(b) != 32 {
		return "", "", "", fmt.Errorf("招待リンクが壊れています")
	}
	h, ok := yggAddr(net.IP(b[:16]).String())
	if !ok {
		return "", "", "", fmt.Errorf("招待リンクのアドレスが不正です")
	}
	return h, hex.EncodeToString(b[16:24]), hex.EncodeToString(b[24:]), nil
}

// onJoin runs on the host for a gj. Caller holds a.store.mu; it is released here.
func (a *App) onJoin(from string, w wire, g Group, known bool) {
	no := func(text string) {
		a.store.mu.Unlock()
		a.sendWire(from, wire{T: "gjn", G: w.G, Text: text})
	}
	if !known || g.Left || !canManage(g, a.addr) || g.Tok == "" || len(w.Tok) != len(g.Tok) || subtle.ConstantTimeCompare([]byte(w.Tok), []byte(g.Tok)) != 1 {
		no("この招待リンクは使えません（リセットされたか、グループがなくなりました）")
		return
	}
	if hasMember(g, from) {
		a.store.mu.Unlock()
		a.answerInfo(from, g)
		return
	}
	if len(g.Members) >= groupMax(g) {
		no(fmt.Sprintf("グループが満員です（最大%d人）", groupMax(g)))
		return
	}
	if a.store.Blocked[from] {
		no("この招待リンクは使えません")
		return
	}
	nick := cleanNick(w.Nick)
	if (w.PN != "" || a.store.PowStrict) && !joinPowOK(from, w.G, w.Tok, w.PN) {
		no("参加にはYggChat v0.19.0以上が必要です（PoW）")
		return
	}
	if g.Set.Pass && g.PwH == "" {
		// an admin that hasn't got the password hash from the host yet
		owner := g.Owner
		no("いまは参加を受け付けられません。少ししてからもう一度開いてね")
		a.sendWire(owner, wire{T: "gkq", G: w.G})
		return
	}
	if g.PwH != "" {
		if a.pwLimited(from, false) {
			no("パスワードを何度も間違えたので、しばらく参加できません")
			return
		}
		good := false
		if w.Pw != "" {
			if !pwGlobalOK() {
				no("いま参加が混み合っています。少ししてからもう一度開いてね")
				return
			}
			// PBKDF2 is slow: check without holding the store.
			h := g.PwH
			a.store.mu.Unlock()
			good = checkGroupPass(h, w.Pw)
			a.store.mu.Lock()
			var ok bool
			if g, ok = a.store.Groups[w.G]; !ok || g.Left || !canManage(g, a.addr) || a.store.locked {
				a.store.mu.Unlock()
				return
			}
			if hasMember(g, from) {
				a.store.mu.Unlock()
				a.answerInfo(from, g)
				return
			}
		}
		if !good {
			text := "このグループに入るにはパスワードが必要です"
			if w.Pw != "" {
				a.pwLimited(from, true)
				text = "パスワードが違います"
			}
			a.store.mu.Unlock()
			a.sendWire(from, wire{T: "gjn", G: w.G, K: "pw", Text: text})
			return
		}
	}
	if g.Set.Approve {
		fresh := true
		for _, p := range g.Pend {
			fresh = fresh && p.Addr != from
		}
		if fresh {
			if len(g.Pend) >= maxPending {
				no("参加申請がいっぱいです。しばらくしてからもう一度開いてね")
				return
			}
			g.Pend = append(g.Pend, Pend{Addr: from, Nick: nick, TS: time.Now().Unix()})
			a.store.Groups[g.ID] = g
			a.store.save()
		}
		a.store.mu.Unlock()
		a.sendWire(from, wire{T: "gjn", G: w.G, K: "wait", Text: "ホストの承認待ちです。承認されると自動で参加します"})
		if fresh {
			a.broadcast("contact", groupView(g))
			who := nick
			if who == "" {
				who = a.nameOf(from)
			}
			a.groupSys(g, who+" が参加を申請しています")
			a.broadcast("notice", map[string]string{"text": "「" + g.Name + "」に参加申請があります"})
		}
		return
	}
	a.admit(g, from, nick, "招待リンクから参加しました")
}

func (a *App) inviteHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/group/invite", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID    string
			Reset bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		g, ok := a.store.Groups[req.ID]
		if !ok || g.Left || a.store.locked || !canManage(g, a.addr) {
			a.store.mu.Unlock()
			http.Error(w, "招待リンクを作れるのはホストと副管理者だけです", 403)
			return
		}
		if g.Tok == "" || req.Reset {
			g.Tok = newGroupToken()
			a.store.Groups[g.ID] = g
			a.store.save()
		}
		code := encodeJoin(a.addr, g.ID, g.Tok)
		a.store.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]string{"code": code, "link": "https://yggchat.surge.sh/#j=" + code})
	})
	mux.HandleFunc("/api/group/join", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code, Pw, ID string
			Cancel       bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Cancel {
			// Stop waiting for this join (the host's pending entry stays until it answers).
			gid := strings.TrimPrefix(req.ID, "g:")
			if req.Code != "" {
				if _, g2, _, err := decodeJoin(req.Code); err == nil {
					gid = g2
				}
			}
			a.store.mu.Lock()
			if _, ok := a.store.Joins[gid]; ok {
				delete(a.store.Joins, gid)
				a.store.save()
				a.joinsChangedLocked()
			}
			a.store.mu.Unlock()
			w.Write([]byte("{}"))
			return
		}
		host, gid, tok, err := decodeJoin(req.Code)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if host == a.addr {
			http.Error(w, "自分のグループの招待リンクです", 400)
			return
		}
		a.store.mu.Lock()
		if a.store.locked {
			a.store.mu.Unlock()
			http.Error(w, "locked", 400)
			return
		}
		if g, ok := a.store.Groups[gid]; ok && !g.Left && hasMember(g, a.addr) && !g.Req {
			a.store.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"already": true, "addr": gKey(gid)})
			return
		}
		if g, ok := a.store.Groups[gid]; ok && g.Left {
			delete(a.store.Groups, gid) // joining again starts fresh
		}
		a.setJoinLocked(gid, host, joinWaitTTL)
		a.store.save()
		nick := a.store.Nick
		a.store.mu.Unlock()
		jw := wire{T: "gj", G: gid, Tok: tok, Nick: nick, Pw: req.Pw, PN: joinPow(a.addr, gid, tok)}
		go func() {
			a.hello(host) // the first packets to a new peer often go nowhere while its route is looked up
			for _, d := range []time.Duration{1, 2, 2, 3, 4, 5, 6} {
				time.Sleep(d * time.Second)
				a.store.mu.Lock()
				_, waiting := a.joinHostLocked(gid)
				a.store.mu.Unlock()
				if !waiting {
					return
				}
				a.sendWire(host, jw)
			}
		}()
		json.NewEncoder(w).Encode(map[string]any{"addr": gKey(gid)})
	})
}
