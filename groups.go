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
	ID      string            `json:"id"`
	Name    string            `json:"name"`
	Members []string          `json:"members"`          // Yggdrasil addresses, including ours
	Req     bool              `json:"req,omitempty"`    // invitation not accepted yet
	Left    bool              `json:"left,omitempty"`   // we left or were removed
	Owner   string            `json:"owner,omitempty"`  // host (creator); "" for pre-v0.16 groups
	Tok     string            `json:"tok,omitempty"`    // invite-link token (host only, v0.17)
	Set     GroupSet          `json:"set,omitempty"`    // host's settings (v0.18)
	PwH     string            `json:"pwh,omitempty"`    // join password hash (host only, v0.18)
	Pend    []Pend            `json:"pend,omitempty"`   // join requests waiting for approval (host/admin)
	Admins  []string          `json:"admins,omitempty"` // co-admins (v0.19, set by the host)
	Stub    bool              `json:"stub,omitempty"`   // made from a message before any group info arrived
	V       int               `json:"v,omitempty"`      // member-list version (v0.19)
	SV      int               `json:"sv,omitempty"`     // version of the host's name/settings/admins (v0.19)
	Quit    bool              `json:"quit,omitempty"`   // we left ourselves (not removed), v0.19.2
	Tomb    bool              `json:"tomb,omitempty"`   // deleted after leaving: hidden, kept so late copies don't bring it back
	Gone    []string          `json:"gone,omitempty"`   // host only: members recently removed or left (merge guard)
	GoneAt  map[string]int64  `json:"gat,omitempty"`    // every node, local: when members dropped out (late posts from before are kept)
	AddAt   map[string]int64  `json:"gaa,omitempty"`    // every node, local: when members were added after we knew the group (v0.19.3)
	VAt     int64             `json:"vat,omitempty"`    // when the current member-list version was made (from gi/gd "ts", v0.19.3)
	By      string            `json:"by,omitempty"`     // local: who sent us the invitation (caps invitation spam)
	Ban     []string          `json:"ban,omitempty"`    // host/admin, local: removed by a manager; their link joins are refused
	Hand    bool              `json:"hand,omitempty"`   // we left as host and handed the group to Owner (re-sent if they missed it)
	Keys    map[string]string `json:"keys,omitempty"`   // local: members whose last post was signed -> their key (groupssig.go)
	QuitAt  int64             `json:"qat,omitempty"`    // when we left (our gl is repeated for a while, v0.19.3)
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

// sameMembers reports whether two member lists hold the same addresses.
func sameMembers(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	x, y := append([]string{}, a...), append([]string{}, b...)
	sort.Strings(x)
	sort.Strings(y)
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

const maxGone = 256

// trackGone keeps the host's list of members recently removed or left (Gone),
// so a late or colliding list from an admin can't bring them back when the
// host merges it (v0.19.2). before is the member list before the change.
func trackGone(g *Group, before []string) {
	for _, m := range before {
		if !hasMember(*g, m) && !hasMember(Group{Members: g.Gone}, m) {
			g.Gone = append(g.Gone, m)
		}
	}
	var keep []string
	for _, m := range g.Gone {
		if !hasMember(*g, m) {
			keep = append(keep, m)
		}
	}
	if len(keep) > maxGone {
		keep = keep[len(keep)-maxGone:]
	}
	g.Gone = keep
}

// mergeAdds returns the members of list that the host doesn't have and
// didn't remove recently (an admin's change it missed), within the limit.
func mergeAdds(g Group, list []string) []string {
	var out []string
	for _, m := range list {
		if !hasMember(g, m) && !hasMember(Group{Members: g.Gone}, m) && !hasMember(Group{Members: out}, m) && len(g.Members)+len(out) < groupMax(g) {
			out = append(out, m)
		}
	}
	return out
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
		if !g.Tomb {
			out[gKey(g.ID)] = groupView(g)
		}
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
	w := wire{T: "gi", G: g.ID, GN: g.Name, GM: g.Members, GO: g.Owner, GV: g.V, GSV: g.SV, GAD: g.Admins, TS: g.VAt}
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
	g := Group{ID: hex.EncodeToString(b), Name: name, Members: ms, Owner: a.addr, V: 1, VAt: time.Now().Unix()}
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
	a.noteInvited(g.ID, g.Members) // sent again while we don't hear from them (groupinv.go)
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
	if img == "" {
		vis.Sg = a.signPost(gid, id, vis.TS, text) // relays can then prove it is ours (v0.19.3)
	}
	var copies []Message
	for i, mem := range g.Members {
		if mem == a.addr {
			continue
		}
		// The photo itself stays on the visible message only (GImg), so a
		// group of 20 doesn't store it 20 times.
		copies = append(copies, Message{ID: fmt.Sprintf("%sg%d", id, i), Peer: mem, From: a.addr, Nick: nick, Text: text, TS: vis.TS, Out: true, Status: "sending", Grp: gid, Of: id, Hid: true, GImg: img != "", TTL: ttl, Exp: exp, Sg: vis.Sg})
	}
	vis.Tot = len(copies)
	if len(copies) == 0 {
		vis.Status = "sent"
	}
	a.store.Messages = append(a.store.Messages, copies...)
	need := groupPow(g)
	a.store.mu.Unlock()
	if need > 0 && len(copies) > 0 {
		a.groupPowCopies(gid, id, vis.TS, text, img, need, copies)
		byID := map[string]string{}
		for _, c := range copies {
			byID[c.ID] = c.Pn
		}
		a.store.mu.Lock()
		for i := len(a.store.Messages) - 1; i >= 0; i-- {
			if x := &a.store.Messages[i]; x.Hid && x.Of == id && x.Grp == gid {
				if pn, ok := byID[x.ID]; ok {
					x.Pn = pn
				}
			}
		}
		a.store.mu.Unlock()
	}
	a.addMessage(vis)
	for _, c := range copies {
		go a.deliverCopy(c)
	}
	return vis, nil
}

// groupPowCopies sets one shared nonce on the copies of post id. A photo is
// bound by its hash; a long text goes as a file (text "", sha of the UTF-8
// text), so it is solved the way deliverFile checks it and not once per copy
// (v0.19.2). Copy ids differ in length, so a text right at the limit may go
// both ways: then each way gets its own nonce.
func (a *App) groupPowCopies(gid, id string, ts int64, text, img string, need int, copies []Message) {
	scope := a.addr + ">g:" + gid
	pns := map[bool]string{} // long text -> nonce
	for i := range copies {
		long := img == "" && a.longText(copies[i])
		pn, ok := pns[long]
		if !ok {
			t, sha := text, ""
			switch {
			case img != "":
				if _, raw, err := parseImg(img); err == nil {
					s := sha256.Sum256(raw)
					sha = hex.EncodeToString(s[:])
				}
			case long:
				s := sha256.Sum256([]byte(text))
				t, sha = "", hex.EncodeToString(s[:])
			}
			pn = powSolve(scope, id, ts, t, sha, need)
			pns[long] = pn
		}
		copies[i].Pn = pn
	}
}

// groupStatus recomputes the visible status of group message id from its copies.
func (a *App) groupStatus(gid, id string) {
	a.store.mu.Lock()
	n, sent, sending, stored, failed := 0, 0, 0, 0, 0
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
			case "failed":
				failed++ // we left the group before it got there
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
	case failed == n:
		st = "failed"
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
	if changed || dlv > 0 {
		a.broadcast("status", map[string]any{"id": id, "peer": gKey(gid), "status": st, "dlv": dlv})
	}
}

func (a *App) retryGroup(gid, id string) (Message, error) {
	a.store.mu.Lock()
	if a.groupOutLocked(gid) {
		a.store.mu.Unlock()
		return Message{}, fmt.Errorf("退出したグループには再送できません")
	}
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
	ask, late := false, false
	if !known {
		// A group we've never heard of is a message request: it needs the
		// request proof of work (and PowStrict) before anything is stored,
		// and only so many placeholders are kept (v0.19.2).
		stubs := 0
		for _, x := range a.store.Groups {
			if x.Stub && !x.Left {
				stubs++
			}
		}
		c, ok := a.store.Contacts[from]
		accepted := ok && c.Shared && !c.Req
		a.store.mu.Unlock()
		if stubs >= maxStubs || !a.powCheck(from, w) {
			return false
		}
		// Same request limit as 1:1 message requests: each placeholder is
		// stored and rewrites the history file (v0.19.3).
		if !accepted && !strangerOK(from) {
			return false
		}
		a.store.mu.Lock()
		if a.store.locked {
			a.store.mu.Unlock()
			return false
		}
		g, known = a.store.Groups[w.G]
		if known {
			a.store.mu.Unlock()
			return a.onGroupMsg(from, w, img) // it appeared meanwhile
		}
		// First we hear of it: keep it as an invitation until we know more.
		c, ok = a.store.Contacts[from]
		g = Group{ID: w.G, Name: "グループ", Members: cleanMembers([]string{from}, a.addr), Req: !(ok && c.Shared && !c.Req), Stub: true}
		a.store.Groups[w.G] = g
		ask = true
	} else if g.Left {
		// We're out: ack and drop so the sender stops retrying; if we left
		// ourselves and they missed it, tell them again (v0.19.2).
		a.store.mu.Unlock()
		a.leftNotice(from, g)
		return true
	} else if !hasMember(g, from) {
		at := goneAt(g, from)
		if at == 0 {
			a.store.mu.Unlock()
			a.askInfo(from, w.G)
			if g.Owner != "" && g.Owner != from && g.Owner != a.addr {
				a.askInfo(g.Owner, w.G) // we may have missed a change: the host knows the list
			}
			return false
		}
		// A member who left or was removed. A post made before that (a late
		// copy) is still theirs to make; a later one is acked and dropped so
		// they stop retrying, and the host tells them they are out (v0.19.2).
		// Late copies are taken only for a while and only so many, so a
		// removed member can't go on posting by backdating (v0.19.3).
		if !lateOK(g, from, w.TS) {
			a.store.mu.Unlock()
			a.goneNotice(from, g)
			return true
		}
		late = true
	} else if w.GH != "" && w.GH != groupHash(g.Name, g.Members) {
		ask = true
	}
	key := gKey(w.G)
	dup, relayed := false, false
	for i := len(a.store.Messages) - 1; i >= 0 && !dup; i-- {
		m := a.store.Messages[i]
		dup = m.Peer == key && m.From == from && samePost(m.ID, w.ID)
		// We hold it as someone's relay: the sender's own copy is the real one (v0.19.3).
		// A relayed photo is only a "📷" text line, so the photo itself replaces it too.
		relayed = dup && !m.Out && (m.Via != "" || m.Hist) && (m.Text != w.Text || m.Via != "" || (img != "" && m.Img == ""))
	}
	if late && !dup && !lateTake(w.G, from) {
		a.store.mu.Unlock()
		return true // too many late posts from a former member
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
	if relayed && !drop {
		if !a.powCheck(from, w) {
			return false
		}
		a.replaceRelayed(key, from, w, img)
		if w.Sg != "" && verifyPost(from, w.PK, w.Sg, w.G, postID(w.ID), w.TS, w.Text) {
			a.noteSigner(w.G, from, w.PK)
		}
		return true
	}
	if dup || drop {
		return true
	}
	if !a.powCheck(from, w) {
		return false
	}
	// Stored under the post id (copies are "<post>g<i>"), so relays dedupe on it.
	m := Message{ID: postID(w.ID), Peer: key, From: from, Nick: cleanNick(w.Nick), Text: w.Text, TS: w.TS, Img: img, TTL: ttl}
	if now := time.Now().Unix(); m.TS <= 0 || m.TS > now+600 {
		m.TS = now // a wrong (or hostile) clock must not pin it far in the past/future (v0.19.3)
	}
	if img == "" {
		// A signed text post: keep the signature to pass on with relays; the
		// sender is known to sign while its posts are signed (v0.19.3).
		pk := ""
		if w.Sg != "" && verifyPost(from, w.PK, w.Sg, w.G, postID(w.ID), w.TS, w.Text) {
			pk = w.PK
			if m.TS == w.TS {
				m.Sg = w.Sg // (a clamped time no longer matches what was signed)
			}
		}
		a.noteSigner(w.G, from, pk)
	}
	if ttl > 0 {
		m.Exp = time.Now().Unix() + int64(ttl)
	}
	return a.addMessageErr(m) == nil // not saved: no ack, the sender retries (v0.19.3)
}

// groupLeft reports whether we are out of group gid, and whether we left ourselves.
func (a *App) groupLeft(gid string) (left, quit bool) {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	g, ok := a.store.Groups[gid]
	return ok && g.Left, ok && g.Left && g.Quit
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
		if m := a.store.Messages[i]; m.Peer == gKey(gid) && m.From == from && samePost(m.ID, id) {
			return true
		}
	}
	return false
}

// hasGroupFile reports whether the post a file header (photo or long text)
// belongs to is here already. A relay of it doesn't count: history carries a
// photo only as a "📷" line and a long text shortened (and so unsigned), so
// the sender's own copy still has to come in and replace it.
func (a *App) hasGroupFile(gid, from, id, mime string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		m := a.store.Messages[i]
		if m.Peer != gKey(gid) || m.From != from || !samePost(m.ID, id) {
			continue
		}
		if !m.Out && (m.Hist || m.Via != "") && ((mime == textMime) || m.Img == "") {
			return false
		}
		return true
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
		host, waiting := a.joinHostLocked(w.G)
		if waiting && host == from {
			if w.K == "wait" {
				a.setJoinLocked(w.G, host, joinApproveWait) // approval pending: keep listening for the host's gi
			} else {
				delete(a.store.Joins, w.G)
				a.joinsChangedLocked()
			}
			a.store.save()
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
		} else if known && !g.Left && !hasMember(g, from) {
			a.goneNotice(from, g) // removed while away: the host's list tells it (v0.19.2)
		} else if known && g.Left {
			a.leftNotice(from, g) // they missed our leave (or, as our heir, the hand-over)
		}
		return
	case "gl":
		if !known || g.Left || !hasMember(g, from) {
			a.store.mu.Unlock()
			return
		}
		before := g.Members
		ms := []string{}
		for _, m := range g.Members {
			if m != from {
				ms = append(ms, m)
			}
		}
		g.Members = ms
		g.Admins = cleanAdmins(g.Admins, g)
		noteGone(&g, before, w.TS) // gl carries when they left (v0.19.2); posts from before still count
		host := g.Owner == a.addr
		if host {
			// The host makes the leave a new version, so members who missed
			// the gl (or got a re-add that crossed it) follow (v0.19.2).
			bumpV(&g)
			trackGone(&g, before)
		}
		a.store.Groups[w.G] = g
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", groupView(g))
		a.groupSys(g, a.gNameOf(g, from)+" が退出しました")
		if host {
			a.sendMembers(g, nil, []string{from}, false)
		}
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
		if known && g.Req && !g.Left {
			// An unanswered invitation while we wait for the host whose link we
			// opened: another member (or anyone who saw the group id) may have
			// sent it first, maybe without the host in it; the host's own info
			// starts the group afresh (v0.19.3).
			if jh, ok := a.joinHostLocked(w.G); ok && jh == from {
				known = false
			}
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
			if g.Quit {
				// We left ourselves: a list with us in it is from someone who
				// missed our gl, not an invitation (v0.19.2). Join by link to come back.
				a.store.mu.Unlock()
				if in {
					a.leftNotice(from, g)
				}
				return
			}
			// Only the host (or an admin) adding us back with a newer list brings us back in.
			if !in || !canManage(g, from) || (w.GV != 0 && w.GV <= g.V) {
				a.store.mu.Unlock()
				return
			}
			g.Left, g.Req, g.Tomb, rejoined = false, true, false, true
		}
		name := cleanNickOr(w.GN)
		byHost := known && g.Owner != "" && from == g.Owner
		collide, hint, stale, resync, askHost := false, false, false, false, false
		var merged, unmerged []string
		adopted := !known // the list came from the host/an admin and replaces ours
		if known && g.Owner != "" {
			switch {
			case w.GV != 0 && w.GV == g.V && g.Owner != a.addr && !sameMembers(ms, g.Members) && !(byHost && !in && hasMember(Group{Members: unpackAddrs(w.GR)}, a.addr)):
				// (Unless the host's list says it removed us ("gr"): its merge
				// keeps that, so it is taken even at the same version, v0.19.3.
				// Without gr a list lacking us may just predate an admin adding us.)
				// Same version, different list: two changes were made from the
				// same base (the host missed an admin's, or an admin worked from
				// an old list). Keep ours; the host merges them into a new
				// version (v0.19.2).
				ms, in, collide = g.Members, true, true
			case byHost && (w.GV == 0 || w.GV >= g.V):
				// the host's list (a pre-v0.19 host sends no version)
				adopted = !sameMembers(ms, g.Members)
			case !byHost && isAdmin(g, from) && w.GV > g.V && (w.GV > g.V+maxVJump || len(ms) > groupMax(g)) && g.Owner != a.addr:
				// An admin's list far ahead of ours or over the limit: an
				// admin can't brick the versions or overfill the group; the
				// host's list decides (v0.19.3).
				ms, in, askHost = g.Members, true, true
			case !byHost && isAdmin(g, from) && w.GV > g.V && w.GV <= g.V+maxVJump && len(ms) <= groupMax(g):
				// an admin's newer list; the host and admins stay in it
				for _, x := range append([]string{g.Owner}, g.Admins...) {
					if hasMember(g, x) && !hasMember(Group{Members: ms}, x) {
						ms = append(ms, x)
					}
				}
				sort.Strings(ms)
				in = hasMember(Group{Members: ms}, a.addr)
				adopted = true
			default:
				// Not the host (or an older list): members stay as they are.
				if g.Owner == a.addr && w.GV != 0 && !sameMembers(ms, g.Members) {
					switch {
					case isAdmin(g, from):
						merged = mergeAdds(g, ms) // an admin's change we missed
						if w.GV == g.V {
							// Its removals too, when it worked from the same base
							// (two admins removing at once, v0.19.3).
							for _, x := range unpackAddrs(w.GR) {
								if hasMember(g, x) && x != g.Owner && !isAdmin(g, x) && x != a.addr && !hasMember(Group{Members: ms}, x) {
									unmerged = append(unmerged, x)
								}
							}
						}
						if len(merged) == 0 && len(unmerged) == 0 {
							// nothing to add (e.g. the admin removed someone at our
							// version): re-issue ours as a new version so all agree
							resync, stale = w.GV == g.V, w.GV < g.V
						}
					case w.GV >= g.V:
						hint = true // a member holds an admin's list we lack: ask the admins
					default:
						stale = true // they missed changes: send them ours
					}
				}
				ms, in = g.Members, true
				if len(merged) > 0 || len(unmerged) > 0 {
					var keep []string
					for _, m := range g.Members {
						if !hasMember(Group{Members: unmerged}, m) {
							keep = append(keep, m)
						}
					}
					ms = cleanMembers(append(keep, merged...), a.addr)
				}
			}
		}
		fresh, viaLink := !known, false
		if !known {
			if !in {
				a.store.mu.Unlock()
				return
			}
			c, ok := a.store.Contacts[from]
			jh, joining := a.joinHostLocked(w.G)
			joining = joining && jh == from
			if accepted := ok && c.Shared && !c.Req; !accepted && !joining && !g.Stub && (!a.inviteRoomLocked(from) || !strangerOK(from)) {
				a.store.mu.Unlock() // a stranger flooding us with invitations (v0.19.3)
				return
			}
			wasStub := g.Stub
			g = Group{ID: w.G, Req: !(ok && c.Shared && !c.Req), By: from}
			if wasStub {
				g.Req = stubReq
			}
			if o, ok := yggAddr(w.GO); ok && hasMember(Group{Members: ms}, o) {
				g.Owner = o
				g.Admins = cleanAdmins(w.GAD, Group{Owner: o, Members: ms})
			}
			if jh, ok := a.joinHostLocked(w.G); ok && jh == from && canManage(g, from) {
				g.Req = false // we asked to join through this host's/admin's link
				viaLink = true
			}
		}
		if jh, ok := a.joinHostLocked(w.G); ok && jh == from && g.Owner != "" && canManage(g, from) && in {
			delete(a.store.Joins, w.G)
			a.joinsChangedLocked()
		}
		old := g
		g.Members = ms
		// The manager who made this list says when; a member removed then
		// can't have posted after it (v0.19.3).
		at := int64(0)
		if adopted && known {
			at = changeTime(w.TS)
		}
		noteGone(&g, old.Members, at)
		if adopted && known {
			noteGoneEach(&g, old.Members, w.GT)
		}
		dels := a.purgeGoneLocked(g, old.Members)
		if !staleSet {
			g.Name = name
		}
		if w.GV > g.V && (byHost || isAdmin(old, from) || !known) && !askHost {
			g.V = w.GV
			g.VAt = changeTime(w.TS) // 0 = unknown (an older sender): never pass on a stale time

		}
		if len(merged) > 0 || len(unmerged) > 0 || resync {
			g.V = old.V // the host re-issues the merged list as a new version
			bumpV(&g)
		}
		if w.K == "kick" && canManage(old, from) && canManage(g, a.addr) {
			for _, x := range unpackAddrs(w.GR) {
				if !hasMember(g, x) {
					addBan(&g, []string{x}) // removed by a manager: no coming back by link
				}
			}
		}
		if len(unmerged) > 0 {
			addBan(&g, unmerged)
		}
		if g.Owner == a.addr {
			trackGone(&g, old.Members)
		}
		if !in {
			g.Left = true
		}
		setChanged, handed, claimed := false, "", false
		if known && g.Owner != "" && from != g.Owner && !hasMember(g, g.Owner) && isAdmin(old, from) && hasMember(g, from) && w.GSV > g.SV {
			// The host left (it is no longer a member) and we missed its
			// hand-over: the admin it named says so itself (v0.19.3). While
			// the host is still in the group nobody else can claim it.
			if o, ok := yggAddr(w.GO); ok && o == from {
				g.Owner, claimed = from, true
			}
		}
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
		a.broadcastDels(dels)
		if askHost {
			a.askInfo(g.Owner, g.ID)
		}
		if needKey {
			a.sendWire(g.Owner, wire{T: "gkq", G: g.ID})
		}
		switch {
		case collide && from == g.Owner:
			a.answerInfo(g.Owner, g) // show the host the list it conflicts with
		case collide:
			a.askInfo(g.Owner, g.ID)
		case hint:
			for _, x := range g.Admins {
				a.askInfo(x, g.ID)
			}
		case stale:
			a.answerInfo(from, g)
		case resync:
			a.sendInfo(g, from)
		case len(merged) > 0 || len(unmerged) > 0:
			a.sendMembers(g, merged, unmerged, len(unmerged) > 0)
			if g.Set.Hist {
				a.store.mu.Lock()
				hist := a.historyLocked(g.ID)
				a.store.mu.Unlock()
				for _, m := range merged {
					if len(hist) > 0 {
						go a.sendHistory(m, g.ID, hist)
					}
				}
			}
		}
		switch {
		case fresh && in && viaLink:
			a.groupSys(g, "あなたが参加しました") // the UI puts history before this line
		case fresh && in:
			a.groupSys(g, a.gNameOf(g, from)+" があなたを追加しました")
		case rejoined:
			a.groupSys(g, a.gNameOf(g, from)+" があなたをグループに戻しました")
		case !in:
			a.groupSys(g, "グループから外されました")
			go a.failLeftCopies(g.ID) // never retried now
		case known && old.Name != g.Name && old.Name != "グループ":
			a.groupSys(g, a.gNameOf(g, from)+" がグループ名を「"+g.Name+"」にしました")
		}
		if setChanged && in {
			for _, t := range setDiff(old.Set, g.Set) {
				a.groupSys(g, a.gNameOf(g, from)+" が"+t)
			}
		}
		if handed != "" {
			a.groupSys(g, a.gNameOf(g, from)+" が "+a.gNameOf(g, handed)+" をホストにしました")
		}
		if claimed {
			a.groupSys(g, a.gNameOf(g, from)+" がホストになりました")
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

// gNameOf names a member in a group's system lines: their contact name, else
// the name on their group messages or join request, else a short address
// (which the UI turns into a name once it learns one).
func (a *App) gNameOf(g Group, addr string) string {
	if addr == a.addr {
		return "あなた"
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if c, ok := a.store.Contacts[addr]; ok && c.Nick != "" {
		return c.Nick
	}
	key := gKey(g.ID)
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := a.store.Messages[i]; m.Peer == key && m.From == addr && m.Nick != "" && !m.Hid {
			return m.Nick
		}
	}
	for _, p := range g.Pend {
		if p.Addr == addr && p.Nick != "" {
			return p.Nick
		}
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
			bumpV(&g)
			if host {
				trackGone(&g, old.Members)
			}
			noteGone(&g, old.Members, 0)
			addBan(&g, removed) // removed by us: their link joins are refused (v0.19.3)
			for _, m := range added {
				unban(&g, m)
			}
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
			a.sendMembers(g, added, removed, len(removed) > 0) // the removed hear it too, so they know they're out
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
		// A leaving host hands the group to a co-admin (the one most likely
		// online), so it can still be run.
		heir := ""
		if !was && g.Owner == a.addr && len(g.Admins) > 0 {
			heir = a.pickHeirLocked(g)
			g.Owner = heir
			g.Admins = cleanAdmins(g.Admins, g)
			bumpV(&g)
			g.SV++
			g.Hand = true // re-sent to the heir if it missed it (v0.19.3)
		}
		full := g // the list as it was, for the hand-over and our gl
		if !was {
			// We are out: our own list no longer shows us (it stays as the
			// group was when we left), v0.19.3.
			var ms []string
			for _, m := range g.Members {
				if m != a.addr {
					ms = append(ms, m)
				}
			}
			g.Members = ms
			g.Admins = cleanAdmins(g.Admins, g)
		}
		if !was {
			g.QuitAt = time.Now().Unix()
		}
		g.Left, g.Req, g.Quit = true, false, true
		a.store.Groups[g.ID] = g
		a.store.save()
		pend := a.pendingCopiesLocked(g.ID)
		a.store.mu.Unlock()
		leftAt := time.Now().Unix()
		a.broadcast("contact", groupView(g))
		if heir != "" {
			a.sendInfo(full) // carries the new host ("go") from us, the old host
			a.groupSys(g, a.gNameOf(g, heir)+" をホストにしました")
		}
		if !was {
			// gl says when we left: members keep our posts from before (v0.19.2).
			lw := wire{T: "gl", G: g.ID, TS: leftAt}
			go func() {
				// Posts still on their way get one last push first (a pre-v0.19.2
				// member drops them once it has our gl); what is left is failed.
				a.flushBeforeLeave(g.ID, pend, 8*time.Second)
				a.failLeftCopies(g.ID)
				if heir != "" {
					time.Sleep(5 * time.Second) // let the hand-over land first
				}
				for i := 0; i < 3; i++ {
					for _, m := range full.Members {
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

// bumpV makes a new member-list version (host/admin side).
func bumpV(g *Group) {
	g.V++
	g.VAt = time.Now().Unix()
}

// deleteGroupLocked removes a group and its messages (visible and hidden). Caller holds a.store.mu.
// A group we left stays as a hidden tombstone (v0.19.2): a copy still queued
// somewhere must not bring it back as a new invitation.
func (a *App) deleteGroupLocked(gid string) {
	if g, ok := a.store.Groups[gid]; ok && g.Left {
		g.Tomb, g.Tok, g.PwH, g.Pend = true, "", "", nil
		a.store.Groups[gid] = g
	} else {
		delete(a.store.Groups, gid)
	}
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
		if m.Sg != "" {
			w.Sg, w.PK = m.Sg, a.myPK()
		}
		a.store.mu.Lock()
		if g, ok := a.store.Groups[m.Grp]; ok {
			w.GH = groupHash(g.Name, g.Members)
		}
		a.store.mu.Unlock()
	}
	return w
}
