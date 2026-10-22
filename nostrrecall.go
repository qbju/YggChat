// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Recalling Nostr boxes (v0.19.4). Every box we park records its event id,
// its throw-away key and the relays it went to (Message.NEv, ctlItem.NEv).
// When the message is unsent (1:1 or group), deleted here before it went
// out, or a parked control packet is acked, those boxes are recalled with a
// NIP-09 deletion signed by the box's own key. Store.NRecall keeps the
// deletions until every relay took them (or the box expired anyway), so a
// recall made offline or while a relay is down still happens later.
//
// An unsend (and an edit) of a message that went out as a box is parked as a
// box too when the peer can't be reached, so a receiver that only comes back
// while we're away still gets it (from relays that ignore deletions, the
// post itself could come too; the unsend then wins: it is applied first and
// leaves a tombstone). Old receivers (v0.19.3 and older) skip such boxes.
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/h6e/yggchat/mbox"
)

// nostrRef is one box we parked.
type nostrRef struct {
	I string   `json:"i"`           // event id
	K string   `json:"k"`           // its throw-away key (hex)
	R []string `json:"r,omitempty"` // relays it went to (in NRecall: still to delete from)
	X int64    `json:"x,omitempty"` // when relays drop it anyway
}

const nrecallMax = 2000

// relayPublishMany sends evs (id -> event JSON) to one relay over one
// connection and returns the ids it accepted.
func relayPublishMany(url string, evs map[string]string) map[string]bool {
	ok := map[string]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := relayDial(ctx, url)
	if err != nil {
		return ok
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	for _, ev := range evs {
		if c.Write(ctx, websocket.MessageText, []byte(`["EVENT",`+ev+`]`)) != nil {
			return ok
		}
	}
	for left := len(evs); left > 0; {
		_, b, err := c.Read(ctx)
		if err != nil {
			return ok
		}
		var m []json.RawMessage
		var id string
		if json.Unmarshal(b, &m) != nil || len(m) < 3 || string(m[0]) != `"OK"` || json.Unmarshal(m[1], &id) != nil {
			continue
		}
		if _, mine := evs[id]; mine {
			left--
			ok[id] = string(m[2]) == "true"
		}
	}
	return ok
}

// nostrPutWire seals w for peer and parks it on the relays. It returns the
// boxes made (nil when nothing was accepted).
func (a *App) nostrPutWire(peer string, w wire) []nostrRef {
	relays := a.nostrRelays()
	if relays == nil {
		return nil
	}
	a.store.mu.Lock()
	c := a.store.Contacts[peer]
	legacy := a.store.NostrLegacy
	a.store.mu.Unlock()
	pk, err := hex.DecodeString(c.PK)
	if err != nil || len(pk) != 32 {
		return nil
	}
	wb, _ := json.Marshal(w)
	var refs []nostrRef
	put := func(leg bool) {
		ev, id, nk, err := mbox.NostrWrapKey(pk, a.privKey(), wb, leg)
		if err != nil {
			return
		}
		if nostrPublish(relays, ev) {
			refs = append(refs, nostrRef{I: id, K: nk, R: append([]string{}, relays...), X: time.Now().Unix() + mbox.NostrTTL + 86400})
		}
	}
	put(false)
	if legacy {
		put(true) // a second box with the old static tag, for peers still on v0.12
	}
	return refs
}

// addRecallLocked queues refs for deletion. Caller holds s.mu.
func (s *Store) addRecallLocked(refs []nostrRef) {
	now := time.Now().Unix()
	kept := s.NRecall[:0]
	for _, r := range s.NRecall {
		if r.X > now && len(r.R) > 0 {
			kept = append(kept, r)
		}
	}
	for _, r := range refs {
		if r.I != "" && r.K != "" && len(r.R) > 0 && r.X > now {
			r.R = append([]string{}, r.R...)
			kept = append(kept, r)
		}
	}
	if len(kept) > nrecallMax {
		kept = append([]nostrRef{}, kept[len(kept)-nrecallMax:]...) // oldest go first
	}
	s.NRecall = kept
}

// nostrRecall deletes the boxes refs from their relays (now, and later
// again for relays that didn't take it).
func (a *App) nostrRecall(refs []nostrRef) {
	if len(refs) == 0 {
		return
	}
	a.store.mu.Lock()
	a.store.addRecallLocked(refs)
	a.store.save()
	a.store.mu.Unlock()
	recallAgain.Store(true)
	go a.runRecall()
}

var (
	recallMu    sync.Mutex
	recallAgain atomic.Bool // new deletions queued since the last pass started
)

// runRecall publishes the pending deletions, one connection per relay. One
// pass at a time; deletions queued meanwhile get another pass right after.
func (a *App) runRecall() {
	for {
		if !recallMu.TryLock() {
			return
		}
		recallAgain.Store(false)
		a.recallPass()
		recallMu.Unlock()
		if !recallAgain.Load() {
			return
		}
	}
}

func (a *App) recallPass() {
	a.store.mu.Lock()
	if a.store.locked || len(a.store.NRecall) == 0 {
		a.store.mu.Unlock()
		return
	}
	todo := append([]nostrRef{}, a.store.NRecall...)
	a.store.mu.Unlock()
	byRelay := map[string]map[string]string{} // relay -> deletion id -> deletion
	boxOf := map[string]string{}              // deletion id -> box id
	bad := map[string]bool{}
	for _, r := range todo {
		del, err := mbox.NostrDelete(r.K, r.I)
		var d struct{ ID string }
		if err != nil || json.Unmarshal([]byte(del), &d) != nil || d.ID == "" {
			bad[r.I] = true
			continue
		}
		boxOf[d.ID] = r.I
		for _, u := range r.R {
			if byRelay[u] == nil {
				byRelay[u] = map[string]string{}
			}
			byRelay[u][d.ID] = del
		}
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	done := map[string]map[string]bool{} // relay -> ids taken
	for u, evs := range byRelay {
		wg.Add(1)
		go func(u string, evs map[string]string) {
			defer wg.Done()
			got := map[string]bool{}
			for did, ok := range relayPublishMany(u, evs) {
				if ok {
					got[boxOf[did]] = true
				}
			}
			mu.Lock()
			done[u] = got
			mu.Unlock()
		}(u, evs)
	}
	wg.Wait()
	a.store.mu.Lock()
	now := time.Now().Unix()
	kept := a.store.NRecall[:0]
	for _, r := range a.store.NRecall {
		if bad[r.I] || r.X <= now {
			continue
		}
		var left []string
		for _, u := range r.R {
			if !done[u][r.I] {
				left = append(left, u)
			}
		}
		if len(left) > 0 {
			r.R = left
			kept = append(kept, r)
		}
	}
	changed := len(kept) != len(todo)
	a.store.NRecall = kept
	if changed || len(kept) > 0 {
		a.store.save()
	}
	a.store.mu.Unlock()
}

// parkCtl parks the pending control packets for peer that should go the way
// their message went (Nostr) and aren't parked yet.
func (a *App) parkCtl(peer string) {
	a.flMu.Lock()
	if a.ctlPark == nil {
		a.ctlPark = map[string]bool{}
	}
	if a.ctlPark[peer] {
		a.flMu.Unlock()
		return
	}
	a.ctlPark[peer] = true
	a.flMu.Unlock()
	defer func() { a.flMu.Lock(); delete(a.ctlPark, peer); a.flMu.Unlock() }()
	for _, c := range a.pendingCtl(peer) {
		if !c.Park || len(c.NEv) > 0 {
			continue
		}
		refs := a.nostrPutWire(peer, c.W)
		if len(refs) == 0 {
			continue
		}
		a.store.mu.Lock()
		found := false
		for i := range a.store.Ctl {
			if x := &a.store.Ctl[i]; x.Peer == peer && x.W.ID == c.W.ID {
				x.NEv, found = append(x.NEv, refs...), true
				break
			}
		}
		if found {
			a.store.save()
		}
		a.store.mu.Unlock()
		if !found {
			a.nostrRecall(refs) // acked meanwhile: the box isn't needed
		}
	}
}

// ctlRefs collects the parked boxes of control items.
func ctlRefs(cs []ctlItem) []nostrRef {
	var out []nostrRef
	for _, c := range cs {
		out = append(out, c.NEv...)
	}
	return out
}
