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
	for _, c := range a.pendingCtl(peer) {
		if !a.sendCtl(c, 6) {
			return
		}
	}
}

func (a *App) sendCtl(c ctlItem, tries int) bool {
	ch := make(chan struct{})
	a.ackMu.Lock()
	a.acks[c.W.ID] = ch
	a.ackMu.Unlock()
	defer func() { a.ackMu.Lock(); delete(a.acks, c.W.ID); a.ackMu.Unlock() }()
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
	if idx >= 0 {
		switch w.T {
		case "edit":
			if t := strings.TrimSpace(w.Text); t != "" && len(w.Text) <= maxText {
				a.store.Messages[idx].Text = w.Text
				a.store.Messages[idx].Edited = true
				cp := a.store.Messages[idx]
				upd = &cp
			}
		case "del":
			a.store.Messages = append(a.store.Messages[:idx], a.store.Messages[idx+1:]...)
		}
		a.store.save()
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
		remote := m.Status == "sent" || m.Status == "sending" || m.Nostr
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
		ip := net.ParseIP(req.Peer)
		if ip == nil {
			http.Error(w, "bad request", 400)
			return
		}
		peer := ip.String()
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
		if req.Everyone && (m.Status == "sent" || m.Status == "sending" || m.Nostr) {
			a.queueCtl(peer, wire{T: "del", ID: ctlID(), Ref: m.ID})
		}
		a.broadcast("msg_del", map[string]string{"peer": peer, "id": m.ID})
		w.Write([]byte("{}"))
	})
}
