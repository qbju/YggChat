// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group chat (v0.13.5, provisional). There is no server: a group is an id, a
// name and a member list that every member keeps. A group message is sent to
// each member separately as an ordinary acked "msg" carrying the group id (G)
// and a short hash of the member list (GH); each copy has its own wire id, so
// the existing queue, retry and Nostr parking work per member unchanged.
// The copies are stored as hidden outgoing messages; the visible message in
// the "g:<id>" conversation shows the combined status.
//
//	{"t":"gq","g"}                 ask a member for the group info
//	{"t":"gi","g","gn","gm":[...]} group info (name, members incl. the sender)
//	{"t":"gl","g"}                 the sender left the group
//
// Only members may change the info; a member who receives a list without
// themselves is out. Since v0.16 the creator is the group's host ("go" in
// gi): only the host's info changes the member list (others may rename).
// Groups made before v0.16 have no host and keep the old rule.
// Photos go to each member as an image (fh/fc) carrying g/gh, like msg.
// No edits or timers yet.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

const maxGroupMembers = 20

type Group struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`         // Yggdrasil addresses, including ours
	Req     bool     `json:"req,omitempty"`   // invitation not accepted yet
	Left    bool     `json:"left,omitempty"`  // we left or were removed
	Owner   string   `json:"owner,omitempty"` // host (creator); "" for pre-v0.16 groups
}

func gKey(id string) string { return "g:" + id }

func isGroupKey(k string) bool { return strings.HasPrefix(k, "g:") }

func validGID(id string) bool {
	if len(id) != 16 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func yggAddr(s string) (string, bool) {
	ip := net.ParseIP(strings.TrimSpace(s))
	if ip == nil || len(ip) != 16 || (ip[0]&0xfe) != 0x02 {
		return "", false
	}
	return ip.String(), true
}

// cleanMembers keeps distinct valid addresses (capped), always including must.
func cleanMembers(in []string, must string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range append([]string{must}, in...) {
		if a, ok := yggAddr(s); ok && !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	if len(out) > maxGroupMembers {
		out = out[:maxGroupMembers]
	}
	sort.Strings(out)
	return out
}

func groupHash(name string, members []string) string {
	ms := append([]string{}, members...)
	sort.Strings(ms)
	h := sha256.Sum256([]byte(name + "\n" + strings.Join(ms, ",")))
	return hex.EncodeToString(h[:4])
}

func hasMember(g Group, addr string) bool {
	for _, m := range g.Members {
		if m == addr {
			return true
		}
	}
	return false
}

// groupView is how a group appears in the UI's contact list.
func groupView(g Group) map[string]any {
	return map[string]any{"addr": gKey(g.ID), "nick": g.Name, "avatar": "👥", "group": true, "members": g.Members, "req": g.Req, "left": g.Left, "owner": g.Owner}
}

// contactsViewLocked merges groups into the contact map for the UI. Caller holds a.store.mu.
func (a *App) contactsViewLocked() map[string]any {
	out := map[string]any{}
	for k, c := range a.store.Contacts {
		out[k] = c
	}
	for _, g := range a.store.Groups {
		out[gKey(g.ID)] = groupView(g)
	}
	return out
}

// visibleMessagesLocked hides the per-member copies of group messages. Caller holds a.store.mu.
func (a *App) visibleMessagesLocked() []Message {
	out := make([]Message, 0, len(a.store.Messages))
	for _, m := range a.store.Messages {
		if !m.Hid {
			out = append(out, m)
		}
	}
	return out
}

func (a *App) groupSys(g Group, text string) {
	a.addMessage(Message{ID: sysID(), Peer: gKey(g.ID), From: a.addr, TS: time.Now().Unix(), Sys: text, Out: true})
}

func (a *App) infoWire(g Group) wire {
	return wire{T: "gi", G: g.ID, GN: g.Name, GM: g.Members, GO: g.Owner}
}

// sendInfo pushes the group info to every other member (datagrams, repeated).
func (a *App) sendInfo(g Group, extra ...string) {
	w := a.infoWire(g)
	to := append(append([]string{}, g.Members...), extra...)
	go func() {
		for i := 0; i < 3; i++ {
			for _, m := range to {
				if m != a.addr {
					a.sendWire(m, w)
				}
			}
			time.Sleep(2 * time.Second)
		}
	}()
}

func (a *App) createGroup(name string, members []string) (Group, error) {
	name = cleanNick(name)
	if name == "" {
		name = "グループ"
	}
	ms := cleanMembers(members, a.addr)
	if len(ms) < 2 {
		return Group{}, fmt.Errorf("メンバーを1人以上選んでください")
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	g := Group{ID: hex.EncodeToString(b), Name: name, Members: ms, Owner: a.addr}
	a.store.mu.Lock()
	if a.store.locked {
		a.store.mu.Unlock()
		return Group{}, errLocked
	}
	a.store.Groups[g.ID] = g
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("contact", groupView(g))
	a.groupSys(g, "グループ「"+g.Name+"」を作りました")
	a.sendInfo(g)
	return g, nil
}

// sendGroup stores the visible message and one hidden copy per member, then delivers them.
func (a *App) sendGroup(gid, text, img string) (Message, error) {
	if (text == "" && img == "") || len(text) > 4000 {
		return Message{}, fmt.Errorf("message empty or too long")
	}
	if img != "" {
		if _, _, err := parseImg(img); err != nil {
			return Message{}, err
		}
		if len(text) > capMax {
			return Message{}, fmt.Errorf("caption too long (max %d bytes, about 260 Japanese characters)", capMax)
		}
	}
	a.store.mu.Lock()
	g, ok := a.store.Groups[gid]
	if !ok || g.Left || a.store.locked {
		a.store.mu.Unlock()
		return Message{}, fmt.Errorf("このグループには送れません")
	}
	if g.Req {
		g.Req = false
		a.store.Groups[gid] = g
		defer a.broadcast("contact", groupView(g))
	}
	nick := a.store.Nick
	now := time.Now()
	id := fmt.Sprintf("%x", now.UnixNano())
	vis := Message{ID: id, Peer: gKey(gid), From: a.addr, Nick: nick, Text: text, TS: now.Unix(), Out: true, Status: "sending", Img: img}
	var copies []Message
	for i, mem := range g.Members {
		if mem == a.addr {
			continue
		}
		// The photo itself stays on the visible message only (GImg), so a
		// group of 20 doesn't store it 20 times.
		copies = append(copies, Message{ID: fmt.Sprintf("%sg%d", id, i), Peer: mem, From: a.addr, Nick: nick, Text: text, TS: vis.TS, Out: true, Status: "sending", Grp: gid, Of: id, Hid: true, GImg: img != ""})
	}
	if len(copies) == 0 {
		vis.Status = "sent"
	}
	a.store.Messages = append(a.store.Messages, copies...)
	a.store.mu.Unlock()
	a.addMessage(vis)
	for _, c := range copies {
		go a.deliver(c)
	}
	return vis, nil
}

// groupStatus recomputes the visible status of group message id from its copies.
func (a *App) groupStatus(gid, id string) {
	a.store.mu.Lock()
	n, sent, sending, stored := 0, 0, 0, 0
	for _, m := range a.store.Messages {
		if m.Hid && m.Of == id && m.Grp == gid {
			n++
			switch m.Status {
			case "sent":
				sent++
			case "sending":
				sending++
			case "stored":
				stored++
			}
		}
	}
	st := "queued"
	switch {
	case n == 0 || sent == n:
		st = "sent"
	case sending > 0:
		st = "sending"
	case sent+stored == n:
		st = "stored"
	}
	changed := false
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := &a.store.Messages[i]; m.ID == id && m.Peer == gKey(gid) {
			changed = m.Status != st
			m.Status = st
			break
		}
	}
	if changed {
		a.store.save()
	}
	a.store.mu.Unlock()
	if changed {
		a.broadcast("status", map[string]string{"id": id, "peer": gKey(gid), "status": st})
	}
}

func (a *App) retryGroup(gid, id string) (Message, error) {
	a.store.mu.Lock()
	var vis *Message
	var copies []Message
	for i := range a.store.Messages {
		m := a.store.Messages[i]
		if m.ID == id && m.Peer == gKey(gid) && m.Out {
			vis = &m
		}
		if m.Hid && m.Grp == gid && m.Of == id && m.Status != "sent" {
			copies = append(copies, m)
		}
	}
	a.store.mu.Unlock()
	if vis == nil {
		return Message{}, fmt.Errorf("no such message")
	}
	for _, c := range copies {
		c.Status = "sending"
		a.setStatus(c.Peer, c.ID, "sending")
		go a.deliver(c)
	}
	vis.Status = "sending"
	return *vis, nil
}

// onGroupMsg stores an incoming group message. Returns true when it should be acked.
func (a *App) onGroupMsg(from string, w wire, img string) bool {
	if !validGID(w.G) || (w.Text == "" && img == "") || len(w.Text) > 8000 || !msgIDRe.MatchString(w.ID) {
		return false
	}
	a.store.mu.Lock()
	if a.store.locked || a.store.Blocked[from] {
		a.store.mu.Unlock()
		return false
	}
	g, known := a.store.Groups[w.G]
	ask := false
	if !known {
		// First we hear of it: keep it as an invitation until we know more.
		c, ok := a.store.Contacts[from]
		g = Group{ID: w.G, Name: "グループ", Members: cleanMembers([]string{from}, a.addr), Req: !(ok && c.Shared && !c.Req)}
		a.store.Groups[w.G] = g
		ask = true
	} else if g.Left || !hasMember(g, from) {
		a.store.mu.Unlock()
		a.sendWire(from, wire{T: "gq", G: w.G})
		return false
	} else if w.GH != "" && w.GH != groupHash(g.Name, g.Members) {
		ask = true
	}
	key := gKey(w.G)
	dup := false
	for i := len(a.store.Messages) - 1; i >= 0 && !dup; i-- {
		m := a.store.Messages[i]
		dup = m.Peer == key && m.From == from && m.ID == w.ID
	}
	a.store.save()
	a.store.mu.Unlock()
	if !known {
		a.broadcast("contact", groupView(g))
	}
	if ask {
		a.sendWire(from, wire{T: "gq", G: w.G})
	}
	if dup {
		return true
	}
	a.addMessage(Message{ID: w.ID, Peer: key, From: from, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, Img: img})
	return true
}

// groupImg returns the photo of visible group message id (hidden copies don't keep it).
func (a *App) groupImg(gid, id string) string {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.ID == id && m.Peer == gKey(gid) {
			return m.Img
		}
	}
	return ""
}

func (a *App) hasGroupMsg(gid, from, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Peer == gKey(gid) && m.From == from && m.ID == id {
			return true
		}
	}
	return false
}

func (a *App) onGroupCtl(from string, w wire) {
	if !validGID(w.G) {
		return
	}
	a.store.mu.Lock()
	if a.store.locked {
		a.store.mu.Unlock()
		return
	}
	g, known := a.store.Groups[w.G]
	switch w.T {
	case "gq":
		a.store.mu.Unlock()
		if known && !g.Left && hasMember(g, from) && hasMember(g, a.addr) {
			a.sendWire(from, a.infoWire(g))
		}
		return
	case "gl":
		if !known || !hasMember(g, from) {
			a.store.mu.Unlock()
			return
		}
		ms := []string{}
		for _, m := range g.Members {
			if m != from {
				ms = append(ms, m)
			}
		}
		g.Members = ms
		a.store.Groups[w.G] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		a.groupSys(g, a.nameOf(from)+" が退出しました")
		return
	case "gi":
		ms := cleanMembers(w.GM, from)
		if len(w.GM) == 0 || (known && !g.Left && !hasMember(g, from)) {
			a.store.mu.Unlock()
			return
		}
		if known && g.Left {
			a.store.mu.Unlock()
			return
		}
		in := false
		for _, m := range ms {
			in = in || m == a.addr
		}
		name := cleanNick(w.GN)
		if name == "" {
			name = "グループ"
		}
		if known && g.Owner != "" && from != g.Owner {
			// Not the host: only the host changes who is in the group.
			ms, in = g.Members, true
		}
		if !known {
			if !in {
				a.store.mu.Unlock()
				return
			}
			c, ok := a.store.Contacts[from]
			g = Group{ID: w.G, Req: !(ok && c.Shared && !c.Req)}
			if o, ok := yggAddr(w.GO); ok && hasMember(Group{Members: ms}, o) {
				g.Owner = o
			}
		}
		old := g
		g.Name, g.Members = name, ms
		if !in {
			g.Left = true
		}
		a.store.Groups[w.G] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		switch {
		case !in:
			a.groupSys(g, "グループから外されました")
		case known && old.Name != g.Name && old.Name != "グループ":
			a.groupSys(g, a.nameOf(from)+" がグループ名を「"+g.Name+"」にしました")
		}
		if known && in {
			for _, m := range g.Members {
				if !hasMember(old, m) && m != a.addr {
					a.groupSys(g, a.nameOf(from)+" が "+a.nameOf(m)+" を追加しました")
				}
			}
			for _, m := range old.Members {
				if !hasMember(g, m) && m != from {
					a.groupSys(g, a.nameOf(from)+" が "+a.nameOf(m)+" を外しました")
				}
			}
		}
		return
	}
	a.store.mu.Unlock()
}

func (a *App) nameOf(addr string) string {
	if addr == a.addr {
		return "あなた"
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if c, ok := a.store.Contacts[addr]; ok && c.Nick != "" {
		return c.Nick
	}
	return addr[:9] + "…"
}

func (a *App) groupHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/group/create", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Name    string
			Members []string
		}
		if !readJSON(w, r, &req) {
			return
		}
		g, err := a.createGroup(req.Name, req.Members)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		json.NewEncoder(w).Encode(groupView(g))
	})
	mux.HandleFunc("/api/group/update", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     string
			Name   *string
			Add    []string
			Remove []string
		}
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		g, ok := a.store.Groups[req.ID]
		if !ok || g.Left || a.store.locked {
			a.store.mu.Unlock()
			http.Error(w, "no such group", 400)
			return
		}
		old := g
		if req.Name != nil {
			if n := cleanNick(*req.Name); n != "" {
				g.Name = n
			}
		}
		if (len(req.Add) > 0 || len(req.Remove) > 0) && g.Owner != "" && g.Owner != a.addr {
			a.store.mu.Unlock()
			http.Error(w, "メンバーを追加・削除できるのはホストだけです", 403)
			return
		}
		if len(req.Remove) > 0 && g.Owner == "" {
			a.store.mu.Unlock()
			http.Error(w, "このグループにはホストがいないので外せません（v0.16.0より前に作ったグループ）", 400)
			return
		}
		g.Members = cleanMembers(append(append([]string{}, g.Members...), req.Add...), a.addr)
		var removed []string
		if len(req.Remove) > 0 {
			kept := []string{}
			for _, m := range g.Members {
				drop := false
				for _, r := range req.Remove {
					if x, ok := yggAddr(r); ok && x == m && m != a.addr {
						drop = true
					}
				}
				if drop {
					removed = append(removed, m)
				} else {
					kept = append(kept, m)
				}
			}
			g.Members = kept
		}
		g.Req = false
		a.store.Groups[g.ID] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		if g.Name != old.Name {
			a.groupSys(g, "グループ名を「"+g.Name+"」にしました")
		}
		for _, m := range g.Members {
			if !hasMember(old, m) {
				a.groupSys(g, a.nameOf(m)+" を追加しました")
			}
		}
		for _, m := range removed {
			a.groupSys(g, a.nameOf(m)+" を外しました")
		}
		a.sendInfo(g, removed...) // the removed hear it too, so they know they're out
		json.NewEncoder(w).Encode(groupView(g))
	})
	mux.HandleFunc("/api/group/leave", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ ID string }
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		g, ok := a.store.Groups[req.ID]
		if !ok || a.store.locked {
			a.store.mu.Unlock()
			http.Error(w, "no such group", 400)
			return
		}
		was := g.Left
		g.Left, g.Req = true, false
		a.store.Groups[g.ID] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		if !was {
			lw := wire{T: "gl", G: g.ID}
			go func() {
				for i := 0; i < 3; i++ {
					for _, m := range g.Members {
						if m != a.addr {
							a.sendWire(m, lw)
						}
					}
					time.Sleep(2 * time.Second)
				}
			}()
			a.groupSys(g, "グループを退出しました")
		}
		json.NewEncoder(w).Encode(groupView(g))
	})
}

// deleteGroupLocked removes a group and its messages (visible and hidden). Caller holds a.store.mu.
func (a *App) deleteGroupLocked(gid string) {
	delete(a.store.Groups, gid)
	kept := a.store.Messages[:0]
	for _, m := range a.store.Messages {
		if m.Peer != gKey(gid) && m.Grp != gid {
			kept = append(kept, m)
		}
	}
	a.store.Messages = kept
}

// msgWire is the "msg" datagram for m (with the group fields for a group copy).
func (a *App) msgWire(m Message) wire {
	w := wire{T: "msg", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL}
	if m.Grp != "" {
		w.TTL = 0
		w.G = m.Grp
		a.store.mu.Lock()
		if g, ok := a.store.Groups[m.Grp]; ok {
			w.GH = groupHash(g.Name, g.Members)
		}
		a.store.mu.Unlock()
	}
	return w
}
