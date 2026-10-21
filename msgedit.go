// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Editing and unsending messages. Only the original sender can edit or
// delete-for-everyone, and the receiver checks that the control packet comes
// from the same Yggdrasil address (bound to the sender's key) as the message.
// Control packets {t:"edit"|"del", id:<ctl id>, ref:<msg id>, text} are acked
// like messages and kept in the store until acked, so they reach peers that
// are offline now (sent again with the offline queue).
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

type ctlItem struct {
	Peer string `json:"peer"`
	W    wire   `json:"w"`
}

const maxText = 8000

// Tombstones and early edits (see Store.Gone / Store.Early).
const (
	tombKeep  = 10 * 86400 // Nostr boxes live 7 days (+ clock skew)
	goneMax   = 20000
	earlyMax  = 500
	earlyPeer = 50
)

type earlyEdit struct {
	Text string `json:"text"`
	At   int64  `json:"at"`
}

// markGoneLocked records that peer's message id must not come back. Caller holds s.mu.
func (s *Store) markGoneLocked(peer, id string) {
	now := time.Now().Unix()
	if s.Gone == nil {
		s.Gone = map[string]int64{}
	}
	if len(s.Gone) >= goneMax {
		for k, t := range s.Gone {
			if t <= now {
				delete(s.Gone, k)
			}
		}
		for k := range s.Gone { // still full: forget arbitrary old entries
			if len(s.Gone) < goneMax*9/10 {
				break
			}
			delete(s.Gone, k)
		}
	}
	s.Gone[peer+"/"+id] = now + tombKeep
	delete(s.Early, peer+"/"+id)
}

// goneIfRecentLocked tombstones an incoming 1:1 message that is being
// removed here while a copy of it could still arrive. Caller holds s.mu.
func (s *Store) goneIfRecentLocked(m Message) {
	if m.Out || m.Sys != "" || m.Hid || isGroupKey(m.Peer) || m.TS < time.Now().Unix()-tombKeep {
		return
	}
	s.markGoneLocked(m.Peer, m.ID)
}

// isGoneLocked: caller holds s.mu.
func (s *Store) isGoneLocked(peer, id string) bool {
	t, ok := s.Gone[peer+"/"+id]
	return ok && t > time.Now().Unix()
}

// addEarlyLocked keeps an edit for a message we don't have yet. Caller holds s.mu.
func (s *Store) addEarlyLocked(peer, id, text string) {
	now := time.Now().Unix()
	if s.Early == nil {
		s.Early = map[string]earlyEdit{}
	}
	n := 0
	for k, e := range s.Early {
		if now-e.At > tombKeep {
			delete(s.Early, k)
		} else if strings.HasPrefix(k, peer+"/") {
			n++
		}
	}
	if _, ok := s.Early[peer+"/"+id]; !ok && (n >= earlyPeer || len(s.Early) >= earlyMax) {
		return
	}
	s.Early[peer+"/"+id] = earlyEdit{Text: text, At: now}
}

// takeEarlyLocked returns (and forgets) an early edit for peer's message id.
func (s *Store) takeEarlyLocked(peer, id string) (string, bool) {
	k := peer + "/" + id
	e, ok := s.Early[k]
	if ok {
		delete(s.Early, k)
	}
	return e.Text, ok
}

// stillOut reports whether our 1:1 message is still in the store (the user
// may have deleted it while a delivery was running).
func (a *App) stillOut(peer, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := &a.store.Messages[i]; m.ID == id && m.Peer == peer {
			return true
		}
	}
	return false
}

func ctlID() string { return fmt.Sprintf("c%x", time.Now().UnixNano()) }

// queueCtl stores a control packet for peer and tries to send it now.
func (a *App) queueCtl(peer string, w wire) {
	a.store.mu.Lock()
	// A newer edit of the same message replaces an unsent older one; a delete
	// replaces any pending edit.
	kept := a.store.Ctl[:0]
	for _, c := range a.store.Ctl {
		if c.Peer == peer && c.W.Ref == w.Ref {
			continue
		}
		kept = append(kept, c)
	}
	a.store.Ctl = append(kept, ctlItem{Peer: peer, W: w})
	a.store.save()
	a.store.mu.Unlock()
	go a.flushCtl(peer)
}

func (a *App) pendingCtl(peer string) []ctlItem {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []ctlItem
	for _, c := range a.store.Ctl {
		if c.Peer == peer {
			out = append(out, c)
		}
	}
	return out
}

func (a *App) flushCtl(peer string) {
	// One sender per peer: two would steal each other's acks.
	a.flMu.Lock()
	if a.ctlBusy == nil {
		a.ctlBusy = map[string]bool{}
	}
	if a.ctlBusy[peer] {
		a.flMu.Unlock()
		return
	}
	a.ctlBusy[peer] = true
	a.flMu.Unlock()
	defer func() { a.flMu.Lock(); delete(a.ctlBusy, peer); a.flMu.Unlock() }()
	for _, c := range a.pendingCtl(peer) {
		if !a.sendCtl(c, 6) {
			return
		}
	}
}

func (a *App) sendCtl(c ctlItem, tries int) bool {
	ch := a.waitAck(c.Peer, c.W.ID)
	defer a.doneAck(c.W.ID, ch)
	for i := 0; i < tries; i++ {
		a.sendWire(c.Peer, c.W)
		select {
		case <-ch:
			a.store.mu.Lock()
			kept := a.store.Ctl[:0]
			for _, x := range a.store.Ctl {
				if !(x.Peer == c.Peer && x.W.ID == c.W.ID) {
					kept = append(kept, x)
				}
			}
			a.store.Ctl = kept
			a.store.save()
			a.store.mu.Unlock()
			return true
		case <-time.After(2500 * time.Millisecond):
		}
	}
	return false
}

// onCtl applies an incoming edit/del from peer. Returns true to ack.
func (a *App) onCtl(from string, w wire) bool {
	if !validID(w.ID) || !validID(w.Ref) {
		return false
	}
	a.store.mu.Lock()
	var upd *Message
	idx := -1
	for i := range a.store.Messages {
		m := &a.store.Messages[i]
		if m.Peer == from && m.ID == w.Ref && !m.Out && m.From == from {
			idx = i
			break
		}
	}
	okText := strings.TrimSpace(w.Text) != "" && len(w.Text) <= maxText
	if idx >= 0 {
		switch w.T {
		case "edit":
			if okText {
				a.store.Messages[idx].Text = w.Text
				a.store.Messages[idx].Edited = true
				cp := a.store.Messages[idx]
				upd = &cp
			}
		case "del":
			a.store.Messages = append(a.store.Messages[:idx], a.store.Messages[idx+1:]...)
		}
	}
	// The message itself may still be on its way (retry, offline queue,
	// Nostr box): remember the unsend / edit so it applies when it lands.
	// Only for contacts: a stranger can't have a message here to change, and
	// must not be able to fill the store (or make it rewrite) at will.
	_, known := a.store.Contacts[from]
	switch {
	case !known:
	case w.T == "del":
		a.store.markGoneLocked(from, w.Ref)
	case w.T == "edit" && idx < 0 && okText && !a.store.isGoneLocked(from, w.Ref):
		a.store.addEarlyLocked(from, w.Ref, w.Text)
	}
	if idx >= 0 {
		a.store.save()
	} else if known {
		a.store.saveSoon()
	}
	a.store.mu.Unlock()
	if idx >= 0 {
		if upd != nil {
			a.broadcast("msg_upd", upd)
		} else if w.T == "del" {
			a.broadcast("msg_del", map[string]string{"peer": from, "id": w.Ref})
		}
	}
	return true // ack even if unknown, so the sender stops retrying
}

func validID(s string) bool {
	if len(s) == 0 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

func (a *App) msgEditHandlers(mux *http.ServeMux) {
	// POST {Peer, ID, Text}: edit one of our own messages.
	mux.HandleFunc("/api/message/edit", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Peer, ID, Text string }
		if !readJSON(w, r, &req) {
			return
		}
		ip := net.ParseIP(req.Peer)
		if ip == nil || strings.TrimSpace(req.Text) == "" || len(req.Text) > maxText {
			http.Error(w, "bad request", 400)
			return
		}
		if wireLen(wire{T: "edit", ID: ctlID(), Ref: req.ID, Text: req.Text, TS: time.Now().Unix()}) > dgramMax {
			http.Error(w, "長すぎて編集できません（日本語でおよそ350文字まで）。送り直してね", 400)
			return
		}
		peer := ip.String()
		a.store.mu.Lock()
		var m *Message
		for i := range a.store.Messages {
			if x := &a.store.Messages[i]; x.Peer == peer && x.ID == req.ID && x.Out && x.Sys == "" {
				m = x
				break
			}
		}
		if m == nil || m.Img != "" {
			a.store.mu.Unlock()
			http.Error(w, "編集できるのは自分の文章メッセージだけです", 400)
			return
		}
		m.Text = req.Text
		// A "queued" message may have reached them already (lost ack).
		remote := m.Status != "failed"
		if remote {
			m.Edited = true
		}
		cp := *m
		a.store.save()
		a.store.mu.Unlock()
		if remote {
			a.queueCtl(peer, wire{T: "edit", ID: ctlID(), Ref: cp.ID, Text: cp.Text})
		}
		a.broadcast("msg_upd", cp)
		json.NewEncoder(w).Encode(cp)
	})
	// POST {Peer, ID, Everyone}: delete locally; Everyone also unsends our own message.
	mux.HandleFunc("/api/message/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Peer, ID string
			Everyone bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		peer := req.Peer
		if isGroupKey(peer) {
			// Group messages can only be removed from this device for now.
			req.Everyone = false
		} else if ip := net.ParseIP(req.Peer); ip != nil {
			peer = ip.String()
		} else {
			http.Error(w, "bad request", 400)
			return
		}
		a.store.mu.Lock()
		idx := -1
		for i := range a.store.Messages {
			if x := a.store.Messages[i]; x.Peer == peer && x.ID == req.ID {
				idx = i
				break
			}
		}
		if idx < 0 {
			a.store.mu.Unlock()
			http.Error(w, "not found", 404)
			return
		}
		m := a.store.Messages[idx]
		if req.Everyone && !m.Out {
			a.store.mu.Unlock()
			http.Error(w, "相手のメッセージは自分の端末からだけ削除できます", 400)
			return
		}
		a.store.Messages = append(a.store.Messages[:idx], a.store.Messages[idx+1:]...)
		if !m.Out && !isGroupKey(peer) && m.Sys == "" {
			a.store.markGoneLocked(peer, m.ID) // a late copy must not bring it back
		}
		// Drop pending edits for it; an unsent (queued) message just never goes out.
		kept := a.store.Ctl[:0]
		for _, c := range a.store.Ctl {
			if !(c.Peer == peer && c.W.Ref == m.ID) {
				kept = append(kept, c)
			}
		}
		a.store.Ctl = kept
		a.store.save()
		a.store.mu.Unlock()
		// "queued" may still have reached them (a lost ack, a retry or a Nostr
		// box in flight), so everything but a failed send gets the unsend.
		if req.Everyone && m.Sys == "" && m.Status != "failed" {
			a.queueCtl(peer, wire{T: "del", ID: ctlID(), Ref: m.ID})
		}
		a.broadcast("msg_del", map[string]string{"peer": peer, "id": m.ID})
		w.Write([]byte("{}"))
	})
}
