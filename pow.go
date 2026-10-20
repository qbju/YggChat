// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Light proof of work on posts (v0.19). A message carries a nonce "pn" such
// that
//
//	sha256("ygg-pow1\n" + scope + "\n" + id + "\n" + ts + "\n" + hex(sha256(text + "\x00" + sha)) + "\n" + pn)
//
// starts with enough zero bits. scope is "<from>><to>" for a 1:1 message,
// "<from>>g:<group id>" for a group post (one nonce for every member copy:
// id is the post's id without the copy suffix) and "<from>>j:<group id>"
// for an invite-link join (id = token). sha is the photo's hash (fh).
//
//	3 bits  ordinary messages (default)
//	8 bits  message requests (to someone who hasn't accepted us) and joins
//	group   the host's choice: none, 3 .. 16 bits ("pb" in the settings)
//
// A receiver that wants more answers {"t":"pow","id","n":bits} instead of
// the ack and the sender recomputes. Messages without a nonce (pre-v0.19)
// are still taken where 3 bits or less is asked, until v0.24.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"math/bits"
	"strconv"
	"strings"
	"sync"
)

const (
	powDefault = 3
	powRequest = 8
	powMax     = 20
)

var groupPowChoices = map[int]bool{-1: true, 0: true, 6: true, 8: true, 10: true, 12: true, 16: true}

// groupPow is the bits a group asks for (Set.PoW: 0 = default 3, -1 = none).
func groupPow(g Group) int {
	switch {
	case g.Set.PoW < 0:
		return 0
	case g.Set.PoW == 0:
		return powDefault
	}
	return g.Set.PoW
}

func powPrefix(scope, id string, ts int64, text, sha string) []byte {
	c := sha256.Sum256([]byte(text + "\x00" + sha))
	return []byte("ygg-pow1\n" + scope + "\n" + id + "\n" + strconv.FormatInt(ts, 10) + "\n" + hex.EncodeToString(c[:]) + "\n")
}

func zeroBits(h [32]byte) int {
	n := 0
	for _, b := range h {
		if b == 0 {
			n += 8
			continue
		}
		return n + bits.LeadingZeros8(b)
	}
	return n
}

// powBits returns how many zero bits nonce gives (-1 when there's none).
func powBits(scope, id string, ts int64, text, sha, nonce string) int {
	if nonce == "" || len(nonce) > 20 {
		return -1
	}
	return zeroBits(sha256.Sum256(append(powPrefix(scope, id, ts, text, sha), nonce...)))
}

func powSolve(scope, id string, ts int64, text, sha string, need int) string {
	if need <= 0 {
		return ""
	}
	p := powPrefix(scope, id, ts, text, sha)
	buf := make([]byte, 0, len(p)+20)
	for n := uint64(0); ; n++ {
		buf = strconv.AppendUint(append(buf[:0], p...), n, 36)
		if zeroBits(sha256.Sum256(buf)) >= need {
			return string(buf[len(p):])
		}
	}
}

// postID is a group copy's id without its "g<i>" suffix (ids are hex).
func postID(id string) string {
	if i := strings.LastIndexByte(id, 'g'); i > 0 {
		return id[:i]
	}
	return id
}

func powScope(from string, w wire, to string) string {
	if w.G != "" {
		return from + ">g:" + w.G
	}
	return from + ">" + to
}

// powNeedState remembers higher demands from receivers ("pow" replies).
type powNeedState struct {
	mu   sync.Mutex
	need map[string]int // peer/id -> bits
}

var powNeeds = &powNeedState{need: map[string]int{}}

func (a *App) onPowNeed(from string, w wire) {
	if w.N <= 0 || w.N > powMax || !msgIDRe.MatchString(w.ID) {
		return
	}
	powNeeds.mu.Lock()
	if len(powNeeds.need) > 4096 {
		powNeeds.need = map[string]int{}
	}
	k := from + "/" + w.ID
	if w.N > powNeeds.need[k] {
		powNeeds.need[k] = w.N
	}
	powNeeds.mu.Unlock()
}

func powAsked(peer, id string) int {
	powNeeds.mu.Lock()
	defer powNeeds.mu.Unlock()
	return powNeeds.need[peer+"/"+id]
}

// sendBits is what we put on m for its receiver.
func (a *App) sendBits(m Message) int {
	need := powDefault
	a.store.mu.Lock()
	if m.Grp != "" {
		need = groupPow(a.store.Groups[m.Grp])
	} else if c, ok := a.store.Contacts[m.Peer]; !ok || !c.HasProf {
		need = powRequest // they haven't accepted us (no profile from them yet)
	}
	a.store.mu.Unlock()
	if x := powAsked(m.Peer, m.ID); x > need {
		need = x
	}
	return need
}

// withPow fills w.PN for m (reusing the stored nonce when it's enough).
func (a *App) withPow(m Message, w *wire, sha string) {
	need := a.sendBits(m)
	if need <= 0 {
		return
	}
	scope := powScope(a.addr, *w, m.Peer)
	id := m.ID
	if m.Grp != "" {
		id = postID(m.ID)
	}
	if m.Pn != "" && powBits(scope, id, m.TS, m.Text, sha, m.Pn) >= need {
		w.PN = m.Pn
		return
	}
	w.PN = powSolve(scope, id, m.TS, m.Text, sha, need)
	a.store.mu.Lock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if x := &a.store.Messages[i]; x.ID == m.ID && x.Peer == m.Peer {
			x.Pn = w.PN
			a.store.saveSoon()
			break
		}
	}
	a.store.mu.Unlock()
}

// recvBits is what we ask of a message from `from` (caller holds a.store.mu).
func (a *App) recvBitsLocked(from string, w wire) (need int, legacyOK bool) {
	if w.G != "" {
		g, ok := a.store.Groups[w.G]
		if !ok {
			return powRequest, false
		}
		return groupPow(g), g.Set.PoW == 0
	}
	if c, ok := a.store.Contacts[from]; ok && !c.Req {
		return powDefault, true
	}
	return powRequest, !a.store.PowStrict
}

// powCheck verifies w's nonce; on too little work it tells the sender and returns false.
func (a *App) powCheck(from string, w wire) bool {
	if w.checked {
		return true
	}
	a.store.mu.Lock()
	need, legacyOK := a.recvBitsLocked(from, w)
	a.store.mu.Unlock()
	if need <= 0 {
		return true
	}
	if w.PN == "" && legacyOK {
		return true // pre-v0.19 sender
	}
	id := w.ID
	if w.G != "" {
		id = postID(w.ID)
	}
	if powBits(powScope(from, w, a.addr), id, w.TS, w.Text, w.Sha, w.PN) >= need {
		return true
	}
	a.sendWire(from, wire{T: "pow", ID: w.ID, N: need})
	return false
}

func joinPow(from, gid, tok string) string {
	return powSolve(from+">j:"+gid, tok, 0, "", "", powRequest)
}

func joinPowOK(from, gid, tok, nonce string) bool {
	return powBits(from+">j:"+gid, tok, 0, "", "", nonce) >= powRequest
}

func (a *App) powHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/pow", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Strict *bool }
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		if req.Strict != nil && !a.store.locked {
			a.store.PowStrict = *req.Strict
			a.store.save()
		}
		st := a.store.PowStrict
		a.store.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"strict": st})
	})
}
