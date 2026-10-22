// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Group unsend (送信取り消し for everyone, v0.19.4). The author may unsend its
// own post; the host may delete anyone's post and a co-admin the posts of
// ordinary members (the same rule as removing members). The deletion goes to
// every member as a control packet and is kept as a tombstone, so a late
// copy, a Nostr box or a catch-up relay can't bring the post back:
//
//	{t:"del", id:<ctl id>, g:gid, ref:<post id>, by:<author>, dby:<deleter, when not the author>,
//	 pk:<deleter's key>, sg:<sig>, ts:<when deleted>}
//
//	sig = Ed25519(deleter's key, "ygg-gdel1\n"+gid+"\n"+postID+"\n"+author+"\n"+dby)  (dby "" for the author)
//
// The key is checked against the deleter's address, so a relaying member
// can't forge a deletion. A deletion without a signature (system Yggdrasil
// mode has no key) is taken only straight from the deleter (the source
// address is bound to its key) and is never relayed.
//
// The packet reuses "del" with a "g" field: v0.19.x nodes take it for a 1:1
// unsend of a message they don't have and ack it (the sender stops), but
// keep the post. Members who were away get it from the sender's control
// queue when they come back, or from any member answering their catch-up
// ask (ghq), which never serves a deleted post again.
package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/h6e/yggchat/mbox"
)

const (
	gdelMax     = 5000 // tombstones kept
	gdelPerSync = 64   // tombstones passed on per catch-up answer
)

// gTomb is a group-post tombstone (Store.GDel). Sg == "" means local only
// (deleted on this device, or an unsigned unsend): never passed on.
type gTomb struct {
	Exp int64  `json:"x"`
	At  int64  `json:"at,omitempty"` // when it was deleted
	Dby string `json:"d,omitempty"`  // deleter when not the author
	PK  string `json:"pk,omitempty"` // deleter's key
	Sg  string `json:"sg,omitempty"`
}

func gdelKey(gid, by, pid string) string { return gid + "/" + by + "/" + postID(pid) }

func gdelSigMsg(gid, pid, by, dby string) []byte {
	return []byte("ygg-gdel1\n" + gid + "\n" + postID(pid) + "\n" + by + "\n" + dby)
}

func verifySig(addr, pkHex, sg string, msg []byte) bool {
	pk, err := hex.DecodeString(pkHex)
	if err != nil || !mbox.AddrOK(addr, pk) {
		return false
	}
	sig, err := base64.RawStdEncoding.DecodeString(sg)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(pk), msg, sig)
}

// signDel signs a deletion by us ("" in system mode).
func (a *App) signDel(gid, pid, by string) (sg, pk string) {
	pk = a.myPK()
	if pk == "" {
		return "", ""
	}
	dby := a.addr
	if by == a.addr {
		dby = "" // the author's own unsend
	}
	priv := ed25519.PrivateKey(a.cfg.PrivateKey)
	return base64.RawStdEncoding.EncodeToString(ed25519.Sign(priv, gdelSigMsg(gid, pid, by, dby))), pk
}

// mayDelete: deleter may remove author's post in g.
func mayDelete(g Group, deleter, author string) bool {
	if deleter == author {
		return true
	}
	return canManage(g, deleter) && (deleter == g.Owner || !canManage(g, author))
}

// markGDelLocked records a tombstone; a signed one replaces a local one.
// Caller holds s.mu.
func (s *Store) markGDelLocked(gid, by, pid string, t gTomb) {
	now := time.Now().Unix()
	if s.GDel == nil {
		s.GDel = map[string]gTomb{}
	}
	k := gdelKey(gid, by, pid)
	if old, ok := s.GDel[k]; ok && old.Exp > now && (old.Sg != "" || t.Sg == "") {
		return
	}
	if len(s.GDel) >= gdelMax {
		for x, o := range s.GDel {
			if o.Exp <= now {
				delete(s.GDel, x)
			}
		}
		for x := range s.GDel {
			if len(s.GDel) < gdelMax*9/10 {
				break
			}
			delete(s.GDel, x)
		}
	}
	if t.At == 0 {
		t.At = now
	}
	t.Exp = now + tombKeep
	s.GDel[k] = t
	s.markGDelHLocked(k, now)
}

// The full tombstones go after 10 days (they are what is passed on), but an
// old peer (v0.19.3 and older keeps unsent posts) could still relay the post
// after that. So a short hash of every deleted post is kept for 90 days
// (bounded): a post on that list is never shown again here (v0.19.4).
const (
	gdelHKeep = 90 * 86400
	gdelHMax  = 10000
)

func gdelHash(k string) string {
	h := sha256.Sum256([]byte("ygg-gdelh\n" + k))
	return hex.EncodeToString(h[:12])
}

// markGDelHLocked: caller holds s.mu.
func (s *Store) markGDelHLocked(k string, now int64) {
	if s.GDelH == nil {
		s.GDelH = map[string]int64{}
	}
	if len(s.GDelH) >= gdelHMax {
		for x, e := range s.GDelH {
			if e <= now {
				delete(s.GDelH, x)
			}
		}
		if len(s.GDelH) >= gdelHMax { // still full: the ones that go soonest make room
			type he struct {
				k string
				e int64
			}
			all := make([]he, 0, len(s.GDelH))
			for x, e := range s.GDelH {
				all = append(all, he{x, e})
			}
			sort.Slice(all, func(i, j int) bool { return all[i].e < all[j].e })
			for _, x := range all[:len(all)-gdelHMax*9/10] {
				delete(s.GDelH, x.k)
			}
		}
	}
	s.GDelH[gdelHash(k)] = now + gdelHKeep
}

// isGDelLocked: caller holds s.mu.
func (s *Store) isGDelLocked(gid, by, pid string) bool {
	now := time.Now().Unix()
	k := gdelKey(gid, by, pid)
	if t, ok := s.GDel[k]; ok && t.Exp > now {
		return true
	}
	e, ok := s.GDelH[gdelHash(k)]
	return ok && e > now
}

func (a *App) groupGone(gid, by, pid string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return a.store.isGDelLocked(gid, by, pid)
}

// dropGroupPostLocked removes author's post pid from the group chat (and,
// for our own post, the copies not delivered yet). It returns whether the
// post was shown, the Nostr boxes of the dropped copies (to recall) and the
// members whose copy went out as a box. Caller holds s.mu.
func (a *App) dropGroupPostLocked(gid, by, pid string) (found bool, refs []nostrRef, parked map[string]bool) {
	key := gKey(gid)
	kept := a.store.Messages[:0]
	for _, m := range a.store.Messages {
		if m.Peer == key && m.From == by && samePost(m.ID, pid) && m.Sys == "" {
			found = true
			continue
		}
		if by == a.addr && m.Hid && m.Grp == gid && m.Of == postID(pid) {
			// never goes out now; a box already parked is recalled (v0.19.4)
			refs = append(refs, m.NEv...)
			if m.Nostr {
				if parked == nil {
					parked = map[string]bool{}
				}
				parked[m.Peer] = true
			}
			continue
		}
		kept = append(kept, m)
	}
	a.store.Messages = kept
	return
}

// groupUnsend deletes post pid by author for everyone (API). Caller checked
// that the message exists.
func (a *App) groupUnsend(gid, by, pid string) error {
	pid = postID(pid)
	a.store.mu.Lock()
	g, ok := a.store.Groups[gid]
	if !ok || g.Left {
		a.store.mu.Unlock()
		return errors.New("退出したグループでは自分の端末からだけ削除できます")
	}
	if !mayDelete(g, a.addr, by) {
		a.store.mu.Unlock()
		return errors.New("他の人のメッセージを全員から削除できるのは、ホスト（と、一般メンバーの投稿なら管理者）だけです")
	}
	sg, pk := a.signDel(gid, pid, by)
	dby := ""
	if by != a.addr {
		dby = a.addr
	}
	now := time.Now().Unix()
	a.store.markGDelLocked(gid, by, pid, gTomb{At: now, Dby: dby, PK: pk, Sg: sg})
	_, refs, parked := a.dropGroupPostLocked(gid, by, pid)
	var to []string
	for _, m := range g.Members {
		if m != a.addr {
			to = append(to, m)
		}
	}
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("msg_del", map[string]string{"peer": gKey(gid), "id": pid})
	a.nostrRecall(refs)
	// A member whose copy went out as a Nostr box gets the unsend that way
	// too when it can't be reached (it may only come back while we're away).
	a.queueGroupCtl(to, wire{T: "del", G: gid, Ref: pid, By: by, Dby: dby, PK: pk, Sg: sg, TS: now}, parked)
	return nil
}

// queueGroupCtl stores one control packet per member (each with its own id)
// and sends them, at most groupSem at once. park: members to park it for
// (see ctlItem.Park).
func (a *App) queueGroupCtl(peers []string, w wire, park map[string]bool) {
	if len(peers) == 0 {
		return
	}
	a.store.mu.Lock()
	old := time.Now().Unix() - tombKeep
	want := map[string]bool{}
	for _, p := range peers {
		want[p] = true
	}
	kept := a.store.Ctl[:0]
	var gone []ctlItem
	for _, c := range a.store.Ctl {
		if c.W.G != "" && c.W.TS > 0 && c.W.TS < old {
			gone = append(gone, c)
			continue // a member who never came back
		}
		if want[c.Peer] && c.W.Ref == w.Ref && c.W.G == w.G {
			gone = append(gone, c)
			continue
		}
		kept = append(kept, c)
	}
	base := ctlID()
	for i, p := range peers {
		x := w
		x.ID = base + "-" + strconv.Itoa(i)
		kept = append(kept, ctlItem{Peer: p, W: x, Park: park[p]})
	}
	a.store.Ctl = kept
	a.store.save()
	a.store.mu.Unlock()
	a.nostrRecall(ctlRefs(gone))
	for _, p := range peers {
		go func(p string) {
			groupSem <- struct{}{}
			defer func() { <-groupSem }()
			a.flushCtl(p)
		}(p)
	}
}

// onGroupDel applies an incoming group unsend (direct or relayed). Returns
// true to ack: anything well-formed is acked so the sender stops retrying,
// whether we apply it or not.
func (a *App) onGroupDel(from string, w wire) bool {
	if !validGID(w.G) || !validID(w.ID) || !validID(w.Ref) || !msgIDRe.MatchString(w.Ref) {
		return false
	}
	by, ok := yggAddr(w.By)
	if !ok {
		return false
	}
	deleter := by
	if w.Dby != "" {
		if deleter, ok = yggAddr(w.Dby); !ok {
			return false
		}
	}
	if w.Sg != "" {
		if !verifySig(deleter, w.PK, w.Sg, gdelSigMsg(w.G, w.Ref, by, w.Dby)) {
			return true // forged (or broken): ignored
		}
	} else if from != deleter {
		return true // unsigned: only the deleter itself can say so
	}
	pid := postID(w.Ref)
	a.store.mu.Lock()
	g, known := a.store.Groups[w.G]
	if a.store.locked || !known || g.Tomb || (from != deleter && !hasMember(g, from)) || !mayDelete(g, deleter, by) {
		a.store.mu.Unlock()
		return true
	}
	t := gTomb{Dby: w.Dby}
	if w.Sg != "" {
		t.Sg, t.PK = w.Sg, w.PK
	}
	if w.TS > 0 && w.TS <= time.Now().Unix()+600 {
		t.At = w.TS
	}
	a.store.markGDelLocked(w.G, by, pid, t)
	found, refs, _ := a.dropGroupPostLocked(w.G, by, pid)
	if found || len(refs) > 0 {
		a.store.save()
	} else {
		a.store.saveSoon()
	}
	a.store.mu.Unlock()
	a.nostrRecall(refs) // moderation of our own post: our boxes of it go too
	if found {
		a.broadcast("msg_del", map[string]string{"peer": gKey(w.G), "id": pid})
	}
	return true
}

// gdelWiresLocked are the signed tombstones of gid to pass on with a
// catch-up answer, newest first. Caller holds s.mu.
func (a *App) gdelWiresLocked(gid string) []wire {
	now := time.Now().Unix()
	type tw struct {
		at int64
		w  wire
	}
	var all []tw
	pre := gid + "/"
	for k, t := range a.store.GDel {
		if t.Sg == "" || t.Exp <= now || len(k) <= len(pre) || k[:len(pre)] != pre {
			continue
		}
		rest := k[len(pre):]
		i := len(rest) - 1
		for i >= 0 && rest[i] != '/' {
			i--
		}
		if i <= 0 {
			continue
		}
		all = append(all, tw{t.At, wire{T: "del", G: gid, By: rest[:i], Ref: rest[i+1:], Dby: t.Dby, PK: t.PK, Sg: t.Sg, TS: t.At}})
	}
	// newest first, capped
	for i := 1; i < len(all); i++ {
		for j := i; j > 0 && all[j].at > all[j-1].at; j-- {
			all[j], all[j-1] = all[j-1], all[j]
		}
	}
	if len(all) > gdelPerSync {
		all = all[:gdelPerSync]
	}
	out := make([]wire, len(all))
	for i, x := range all {
		out[i] = x.w
	}
	return out
}

// sendGDels passes tombstones on to a member catching up (twice, like history).
func (a *App) sendGDels(to string, ws []wire) {
	time.Sleep(2 * time.Second)
	for round := 0; round < 2; round++ {
		for i, w := range ws {
			w.ID = ctlID() + "-" + strconv.Itoa(i) // acked; nobody waits for it
			a.sendWire(to, w)
			time.Sleep(40 * time.Millisecond)
		}
		time.Sleep(4 * time.Second)
	}
}
