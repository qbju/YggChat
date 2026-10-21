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


type Group struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Members []string `json:"members"`         // Yggdrasil addresses, including ours
	Req     bool     `json:"req,omitempty"`   // invitation not accepted yet
	Left    bool     `json:"left,omitempty"`  // we left or were removed
	Owner   string   `json:"owner,omitempty"` // host (creator); "" for pre-v0.16 groups
	Tok     string   `json:"tok,omitempty"`   // invite-link token (host only, v0.17)
	Set     GroupSet `json:"set,omitempty"`   // host's settings (v0.18)
	PwH     string   `json:"pwh,omitempty"`   // join password hash (host only, v0.18)
	Pend    []Pend   `json:"pend,omitempty"`  // join requests waiting for approval (host/admin)
	Admins  []string `json:"admins,omitempty"` // co-admins (v0.19, set by the host)
	Stub    bool     `json:"stub,omitempty"`   // made from a message before any group info arrived
	V       int      `json:"v,omitempty"`      // member-list version (v0.19)
	SV      int      `json:"sv,omitempty"`     // version of the host's name/settings/admins (v0.19)
}

// GroupSet are the host's group settings (v0.18), sent in gi as "gs".
// Members keep the host's copy; each device enforces it on what it sends and
// stores (a member on an older version ignores it).
type GroupSet struct {
	NoFiles bool `json:"nf,omitempty"`  // photos may not be posted
	Approve bool `json:"ap,omitempty"`  // link joins wait for the host's approval
	Hist    bool `json:"hi,omitempty"`  // new members get the recent history from the host
	TTL     int  `json:"ttl,omitempty"` // disappearing messages (seconds)
	Pass    bool `json:"pw,omitempty"`  // the invite link needs a password
	Max     int  `json:"mx,omitempty"`  // member limit 24..1024 (v0.19; 0 = 24)
	PoW     int  `json:"pb,omitempty"`  // proof-of-work bits per post (v0.19; 0 = 3, -1 = none)
}

type Pend struct {
	Addr string `json:"addr"`
	Nick string `json:"nick,omitempty"`
	TS   int64  `json:"ts"`
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
	if len(out) > maxGroupCap {
		out = out[:maxGroupCap]
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
	v := map[string]any{"addr": gKey(g.ID), "nick": g.Name, "avatar": "👥", "group": true, "members": g.Members, "req": g.Req, "left": g.Left, "owner": g.Owner, "set": g.Set, "admins": g.Admins, "max": groupMax(g)}
	if g.Set.TTL > 0 {
		v["ttl"] = g.Set.TTL
	}
	if len(g.Pend) > 0 {
		v["pend"] = g.Pend
	}
	return v
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
	w := wire{T: "gi", G: g.ID, GN: g.Name, GM: g.Members, GO: g.Owner, GV: g.V, GSV: g.SV, GAD: g.Admins}
	if g.Owner != "" {
		set := g.Set
		w.GS = &set
	}
	return w
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
	g := Group{ID: hex.EncodeToString(b), Name: name, Members: ms, Owner: a.addr, V: 1}
	g.Set.Hist = true // new members and members who were away catch up (v0.19.1)
	for _, c := range groupMaxChoices {
		if c >= len(ms) {
			if c > defaultGroupMax {
				g.Set.Max = c
			}
			break
		}
	}
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
	a.sendPackets(g.Members, a.infoPackets(g))
	return g, nil
}

// sendGroup stores the visible message and one hidden copy per member, then delivers them.
func (a *App) sendGroup(gid, text, img string) (Message, error) {
	if (text == "" && img == "") || len(text) > maxSendText {
		return Message{}, fmt.Errorf("message empty or too long")
	}
	if img != "" {
		if _, _, err := parseImg(img); err != nil {
			return Message{}, err
		}
		if escLen(text) > capMax {
			return Message{}, fmt.Errorf("写真の説明が長すぎます（日本語でおよそ260文字まで）")
		}
	}
	a.store.mu.Lock()
	g, ok := a.store.Groups[gid]
	if !ok || g.Left || a.store.locked {
		a.store.mu.Unlock()
		return Message{}, fmt.Errorf("このグループには送れません")
	}
	if img != "" && g.Set.NoFiles {
		a.store.mu.Unlock()
		return Message{}, fmt.Errorf("このグループでは写真を送れません（ホストの設定）")
	}
	if g.Req {
		g.Req = false
		a.store.Groups[gid] = g
		defer a.broadcast("contact", groupView(g))
	}
	nick := a.store.Nick
	now := time.Now()
	id := fmt.Sprintf("%x", now.UnixNano())
	ttl := g.Set.TTL
	var exp int64
	if ttl > 0 {
		exp = now.Unix() + int64(ttl)
	}
	vis := Message{ID: id, Peer: gKey(gid), From: a.addr, Nick: nick, Text: text, TS: now.Unix(), Out: true, Status: "sending", Img: img, TTL: ttl, Exp: exp}
	var copies []Message
	for i, mem := range g.Members {
		if mem == a.addr {
			continue
		}
		// The photo itself stays on the visible message only (GImg), so a
		// group of 20 doesn't store it 20 times.
		copies = append(copies, Message{ID: fmt.Sprintf("%sg%d", id, i), Peer: mem, From: a.addr, Nick: nick, Text: text, TS: vis.TS, Out: true, Status: "sending", Grp: gid, Of: id, Hid: true, GImg: img != "", TTL: ttl, Exp: exp})
	}
	if len(copies) == 0 {
		vis.Status = "sent"
	}
	a.store.Messages = append(a.store.Messages, copies...)
	need := groupPow(g)
	a.store.mu.Unlock()
	if need > 0 && len(copies) > 0 {
		// One nonce for the post, shared by every member copy.
		sha := ""
		if img != "" {
			if _, raw, err := parseImg(img); err == nil {
				s := sha256.Sum256(raw)
				sha = hex.EncodeToString(s[:])
			}
		}
		pn := powSolve(a.addr+">g:"+gid, id, vis.TS, text, sha, need)
		a.store.mu.Lock()
		for i := len(a.store.Messages) - 1; i >= 0; i-- {
			if x := &a.store.Messages[i]; x.Hid && x.Of == id && x.Grp == gid {
				x.Pn = pn
			}
		}
		a.store.mu.Unlock()
		for i := range copies {
			copies[i].Pn = pn
		}
	}
	a.addMessage(vis)
	for _, c := range copies {
		go a.deliverCopy(c)
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
	// Delivered to anyone counts as sent: the rest are retried on their own
	// (v0.19.1, user's request), so "waiting for them to come online" only
	// shows while nobody has it yet.
	dlv := 0
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := &a.store.Messages[i]; m.ID == id && m.Peer == gKey(gid) {
			dlv = m.Dlv
			break
		}
	}
	st := "queued"
	switch {
	case n == 0 || sent > 0 || dlv > 0:
		st = "sent"
	case sending > 0:
		st = "sending"
	case stored > 0:
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
		a.store.saveSoon()
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
	if len(copies) == 0 {
		a.groupStatus(gid, id)
		return *vis, nil
	}
	for _, c := range copies {
		c.Status = "sending"
		a.setStatus(c.Peer, c.ID, "sending")
		go a.deliverCopy(c)
	}
	vis.Status = "sending"
	return *vis, nil
}

// onGroupMsg stores an incoming group message. Returns true when it should be acked.
func (a *App) onGroupMsg(from string, w wire, img string) bool {
	if !validGID(w.G) || (w.Text == "" && img == "") || len(w.Text) > maxRecvText || !msgIDRe.MatchString(w.ID) {
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
		g = Group{ID: w.G, Name: "グループ", Members: cleanMembers([]string{from}, a.addr), Req: !(ok && c.Shared && !c.Req), Stub: true}
		a.store.Groups[w.G] = g
		ask = true
	} else if g.Left || !hasMember(g, from) {
		a.store.mu.Unlock()
		a.askInfo(from, w.G)
		if !g.Left && g.Owner != "" && g.Owner != from && g.Owner != a.addr {
			a.askInfo(g.Owner, w.G) // we may have missed a change: the host knows the list
		}
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
	// Photos are dropped (but acked, so the sender stops) when the host turned them off.
	drop := img != "" && g.Set.NoFiles && w.Text == ""
	if img != "" && g.Set.NoFiles {
		img = ""
	}
	ttl := w.TTL
	if g.Set.TTL > 0 && (ttl <= 0 || ttl > g.Set.TTL) {
		ttl = g.Set.TTL
	}
	if ttl < 0 || ttl > 4*7*86400 {
		ttl = 0
	}
	a.store.save()
	a.store.mu.Unlock()
	if !known {
		a.broadcast("contact", groupView(g))
	}
	if ask {
		a.askInfo(from, w.G)
		if known && g.Owner != "" && g.Owner != from && g.Owner != a.addr {
			a.askInfo(g.Owner, w.G)
		}
	}
	if dup || drop {
		return true
	}
	if !a.powCheck(from, w) {
		return false
	}
	m := Message{ID: w.ID, Peer: key, From: from, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, Img: img, TTL: ttl}
	if ttl > 0 {
		m.Exp = time.Now().Unix() + int64(ttl)
	}
	a.addMessage(m)
	return true
}

// groupNoFiles reports whether photos are off in group gid.
func (a *App) groupNoFiles(gid string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	g, ok := a.store.Groups[gid]
	return ok && g.Set.NoFiles
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
	case "gj":
		a.onJoin(from, w, g, known)
		return
	case "gjn":
		host, waiting := a.joins[w.G]
		if waiting && host == from && w.K != "wait" {
			delete(a.joins, w.G) // "wait": approval pending, keep listening for the host's gi
		}
		a.store.mu.Unlock()
		if waiting && host == from {
			a.broadcast("notice", map[string]string{"text": cleanText(w.Text, 200), "k": w.K, "g": w.G})
		}
		return
	case "ghs":
		a.onHistory(from, w, g, known)
		return
	case "ghq":
		a.onHistReq(from, w, g, known)
		return
	case "gd":
		a.onDelta(from, w, g, known)
		return
	case "gk":
		a.onKey(from, w, g, known)
		return
	case "gkq":
		a.store.mu.Unlock()
		if known && !g.Left && g.Owner == a.addr && isAdmin(g, from) {
			a.sendKey(g, []string{from})
		}
		return
	case "gp":
		a.store.mu.Unlock()
		a.onInfoPart(from, w)
		return
	case "gq":
		a.store.mu.Unlock()
		if known && !g.Left && hasMember(g, from) && hasMember(g, a.addr) {
			a.answerInfo(from, g)
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
		g.Admins = cleanAdmins(g.Admins, g)
		a.store.Groups[w.G] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		a.groupSys(g, a.nameOf(from)+" が退出しました")
		return
	case "gi":
		if len(w.GM) == 0 && w.GC > 0 {
			a.store.mu.Unlock()
			a.onInfoHeader(from, w) // split list (v0.19)
			return
		}
		ms := cleanMembers(w.GM, from)
		stubReq := g.Req
		if known && g.Stub && !g.Left {
			known = false // a placeholder from a message: take this info as the first one
		}
		if len(w.GM) == 0 || (known && !g.Left && !hasMember(g, from)) {
			a.store.mu.Unlock()
			return
		}
		in := false
		for _, m := range ms {
			in = in || m == a.addr
		}
		// The host's settings/name/admins carry their own version (gsv): a late
		// repeat of older info (it is sent 3 times) must not undo a change.
		staleSet := known && g.Owner != "" && w.GSV < g.SV
		rejoined := false
		if known && g.Left {
			// Only the host (or an admin) adding us back with a newer list brings us back in.
			if !in || !canManage(g, from) || (w.GV != 0 && w.GV <= g.V) {
				a.store.mu.Unlock()
				return
			}
			g.Left, g.Req, rejoined = false, true, true
		}
		name := cleanNickOr(w.GN)
		byHost := known && g.Owner != "" && from == g.Owner
		if known && g.Owner != "" {
			switch {
			case byHost && (w.GV == 0 || w.GV >= g.V):
				// the host's list (a pre-v0.19 host sends no version)
			case !byHost && isAdmin(g, from) && w.GV > g.V:
				// an admin's newer list; the host and admins stay in it
				for _, x := range append([]string{g.Owner}, g.Admins...) {
					if hasMember(g, x) && !hasMember(Group{Members: ms}, x) {
						ms = append(ms, x)
					}
				}
				sort.Strings(ms)
				in = hasMember(Group{Members: ms}, a.addr)
			default:
				// Not the host (or an older list): members stay as they are.
				ms, in = g.Members, true
			}
		}
		if !known {
			if !in {
				a.store.mu.Unlock()
				return
			}
			c, ok := a.store.Contacts[from]
			wasStub := g.Stub
			g = Group{ID: w.G, Req: !(ok && c.Shared && !c.Req)}
			if wasStub {
				g.Req = stubReq
			}
			if o, ok := yggAddr(w.GO); ok && hasMember(Group{Members: ms}, o) {
				g.Owner = o
				g.Admins = cleanAdmins(w.GAD, Group{Owner: o, Members: ms})
			}
			if a.joins[w.G] == from && canManage(g, from) {
				g.Req = false // we asked to join through this host's/admin's link
			}
		}
		if a.joins[w.G] == from && g.Owner != "" && canManage(g, from) && in {
			delete(a.joins, w.G)
		}
		old := g
		g.Members = ms
		if !staleSet {
			g.Name = name
		}
		if w.GV > g.V && (byHost || isAdmin(old, from) || !known) {
			g.V = w.GV
		}
		if !in {
			g.Left = true
		}
		setChanged, handed := false, ""
		if g.Owner != "" && from == g.Owner && !staleSet {
			if w.GSV > g.SV {
				g.SV = w.GSV
			}
			if o, ok := yggAddr(w.GO); ok && o != g.Owner && hasMember(g, o) {
				g.Owner = o // the host handed the group over
				handed = o
			}
			if w.GS != nil {
				ns := cleanSet(*w.GS)
				setChanged = known && ns != g.Set
				g.Set = ns
			}
			g.Admins = cleanAdmins(w.GAD, g)
		} else {
			g.Admins = cleanAdmins(g.Admins, g)
		}
		lostAdmin := isAdmin(old, a.addr) && !isAdmin(g, a.addr)
		if (lostAdmin || !isAdmin(g, a.addr)) && g.Owner != a.addr {
			g.Tok, g.Pend = "", nil
			if g.Owner != a.addr {
				g.PwH = ""
			}
		}
		for _, m := range g.Members {
			g.Pend = dropPend(g.Pend, m)
		}
		needKey := isAdmin(g, a.addr) && g.Set.Pass && g.PwH == ""
		a.store.Groups[w.G] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		if needKey {
			a.sendWire(g.Owner, wire{T: "gkq", G: g.ID})
		}
		switch {
		case rejoined:
			a.groupSys(g, a.nameOf(from)+" があなたをグループに戻しました")
		case !in:
			a.groupSys(g, "グループから外されました")
		case known && old.Name != g.Name && old.Name != "グループ":
			a.groupSys(g, a.nameOf(from)+" がグループ名を「"+g.Name+"」にしました")
		}
		if setChanged && in {
			for _, t := range setDiff(old.Set, g.Set) {
				a.groupSys(g, a.nameOf(from)+" が"+t)
			}
		}
		if handed != "" {
			a.groupSys(g, a.nameOf(from)+" が "+a.nameOf(handed)+" をホストにしました")
		}
		if known && in {
			var added, removed []string
			for _, m := range g.Members {
				if !hasMember(old, m) && m != a.addr {
					added = append(added, m)
				}
			}
			for _, m := range old.Members {
				if !hasMember(g, m) && m != from {
					removed = append(removed, m)
				}
			}
			a.memberSys(g, from, added, removed)
			a.adminSys(g, from, old.Admins, g.Admins)
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
	if len(addr) > 9 {
		return addr[:9] + "…"
	}
	return addr
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
			Set    *GroupSet
			Pass   *string   // join password ("" removes it)
			Admins *[]string // co-admins (host only, v0.19)
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
		fail := func(code int, msg string) {
			a.store.mu.Unlock()
			http.Error(w, msg, code)
		}
		old := g
		host := g.Owner != "" && g.Owner == a.addr
		if req.Name != nil {
			if n := cleanNick(*req.Name); n != "" {
				g.Name = n
			}
		}
		if (len(req.Add) > 0 || len(req.Remove) > 0) && g.Owner != "" && !canManage(g, a.addr) {
			fail(403, "メンバーを追加・削除できるのはホストと副管理者だけです")
			return
		}
		if (req.Set != nil || req.Pass != nil || req.Admins != nil) && !host {
			fail(403, "設定を変えられるのはホストだけです")
			return
		}
		if len(req.Remove) > 0 && g.Owner == "" {
			fail(400, "このグループにはホストがいないので外せません（v0.16.0より前に作ったグループ）")
			return
		}
		if req.Set != nil {
			ns := cleanSet(*req.Set)
			if ns.Max != 0 && !validGroupMax(ns.Max) {
				fail(400, "人数の上限が不正です")
				return
			}
			if ns.Max == defaultGroupMax {
				ns.Max = 0
			}
			if groupMax(Group{Set: ns}) < len(g.Members) {
				fail(400, fmt.Sprintf("今のメンバー（%d人）より少ない上限にはできません", len(g.Members)))
				return
			}
			g.Set = ns
			g.Set.Pass = g.PwH != ""
		}
		pwChanged := false
		if req.Pass != nil {
			if p := *req.Pass; p == "" {
				g.PwH = ""
			} else if n := len([]rune(p)); n < 4 || n > 64 {
				fail(400, "パスワードは4〜64文字にしてください")
				return
			} else {
				g.PwH = hashGroupPass(p)
			}
			g.Set.Pass = g.PwH != ""
			pwChanged = true
		}
		var added, removed []string
		for _, x := range req.Add {
			if m, ok := yggAddr(x); ok && !hasMember(g, m) && !hasMember(Group{Members: added}, m) {
				added = append(added, m)
			}
		}
		if len(g.Members)+len(added) > groupMax(g) {
			fail(400, fmt.Sprintf("このグループの上限は%d人です（グループ設定で変えられます）", groupMax(g)))
			return
		}
		g.Members = cleanMembers(append(append([]string{}, g.Members...), added...), a.addr)
		if len(req.Remove) > 0 {
			kept := []string{}
			for _, m := range g.Members {
				drop := false
				for _, r := range req.Remove {
					if x, ok := yggAddr(r); ok && x == m && m != a.addr {
						drop = true
					}
				}
				if drop && !host && (m == g.Owner || isAdmin(g, m)) {
					a.store.mu.Unlock()
					http.Error(w, "ホストと副管理者は外せません", 403)
					return
				}
				if drop {
					removed = append(removed, m)
				} else {
					kept = append(kept, m)
				}
			}
			g.Members = kept
		}
		if req.Admins != nil {
			ad := cleanAdmins(*req.Admins, g)
			if len(*req.Admins) > maxAdmins {
				fail(400, fmt.Sprintf("副管理者は%d人までです", maxAdmins))
				return
			}
			g.Admins = ad
		}
		g.Admins = cleanAdmins(g.Admins, g)
		if len(added) > 0 || len(removed) > 0 {
			g.V++
		}
		if host && (req.Set != nil || req.Pass != nil || req.Admins != nil || g.Name != old.Name) {
			g.SV++ // a late repeat of the old info can't undo this
		}
		for _, m := range added {
			g.Pend = dropPend(g.Pend, m)
		}
		g.Req = false
		a.store.Groups[g.ID] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		if g.Name != old.Name {
			a.groupSys(g, "グループ名を「"+g.Name+"」にしました")
		}
		for _, t := range setDiff(old.Set, g.Set) {
			a.groupSys(g, "あなたが"+t)
		}
		if req.Pass != nil && *req.Pass != "" && old.PwH != "" {
			a.groupSys(g, "あなたが参加パスワードを変更しました")
		}
		a.memberSys(g, a.addr, added, removed)
		a.adminSys(g, a.addr, old.Admins, g.Admins)
		if len(added) > 0 || len(removed) > 0 {
			a.sendMembers(g, added, removed) // the removed hear it too, so they know they're out
		}
		if len(added) > 0 && g.Set.Hist {
			a.store.mu.Lock()
			hist := a.historyLocked(g.ID)
			a.store.mu.Unlock()
			for _, m := range added {
				if len(hist) > 0 {
					go a.sendHistory(m, g.ID, hist)
				}
			}
		}
		if g.Name != old.Name || req.Set != nil || pwChanged || req.Admins != nil {
			a.sendInfo(g)
		}
		if host {
			var newAd []string
			for _, x := range g.Admins {
				if !isAdmin(old, x) || pwChanged {
					newAd = append(newAd, x)
				}
			}
			if len(newAd) > 0 {
				a.sendKey(g, newAd)
			}
		}
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
		// A leaving host hands the group to the first co-admin, so it can still be run.
		heir := ""
		if !was && g.Owner == a.addr && len(g.Admins) > 0 {
			heir = g.Admins[0]
			g.Owner = heir
			g.Admins = cleanAdmins(g.Admins, g)
			g.V++
			g.SV++
		}
		g.Left, g.Req = true, false
		a.store.Groups[g.ID] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		if heir != "" {
			a.sendInfo(g) // carries the new host ("go") from us, the old host
			a.groupSys(g, a.nameOf(heir)+" をホストにしました")
		}
		if !was {
			lw := wire{T: "gl", G: g.ID}
			go func() {
				if heir != "" {
					time.Sleep(5 * time.Second) // let the hand-over land first
				}
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
	w := a.msgWireBare(m)
	a.withPow(m, &w, "")
	return w
}

// msgWireBare is msgWire without the proof of work.
func (a *App) msgWireBare(m Message) wire {
	w := wire{T: "msg", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL}
	if m.Grp != "" {
		w.G = m.Grp
		a.store.mu.Lock()
		if g, ok := a.store.Groups[m.Grp]; ok {
			w.GH = groupHash(g.Name, g.Members)
		}
		a.store.mu.Unlock()
	}
	return w
}
