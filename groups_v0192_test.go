package main

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tNew = "205:1:2:3:4:5:6:7"

func tJoined(a *App, from string, ms []string, v int, admins ...string) {
	a.onGroupCtl(from, wire{T: "gi", G: tgid, GN: "G", GM: ms, GO: tHost, GAD: admins, GV: v})
	g := a.store.Groups[tgid]
	g.Req = false
	a.store.Groups[tgid] = g
}

// #5: the host's list with the same version as an admin's change doesn't kick the admin's addition.
func TestVersionCollisionKeepsMembers(t *testing.T) {
	a := tApp(t, tMem) // added by the admin at V2
	tJoined(a, tAdm, []string{tHost, tAdm, tMe, tMem}, 2, tAdm)
	// the host missed V2 and makes its own V2 (adds tNew): the list lacks tMem
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tAdm, tMe, tNew}, GO: tHost, GAD: []string{tAdm}, GV: 2})
	g := a.store.Groups[tgid]
	if g.Left || !hasMember(g, tMem) || hasMember(g, tNew) || g.V != 2 {
		t.Fatalf("collision applied: %+v", g)
	}
	// the host's merged V3 is taken
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tAdm, tMe, tMem, tNew}, GO: tHost, GAD: []string{tAdm}, GV: 3})
	if g = a.store.Groups[tgid]; g.Left || !hasMember(g, tNew) || g.V != 3 {
		t.Fatalf("merged version not applied: %+v", g)
	}
}

// #5: the host merges an admin's same-version (or older) change into a new version,
// but never brings back someone it removed.
func TestOwnerMergesAdminChange(t *testing.T) {
	h := tApp(t, tHost)
	h.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tAdm, tMe}, tHost), Owner: tHost, Admins: []string{tAdm}, V: 2, Gone: []string{tNew}}
	// admin's V2 added tMem (host's V2 added tMe)
	h.onGroupCtl(tAdm, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tAdm, tMem}, GO: tHost, GAD: []string{tAdm}, GV: 2})
	g := h.store.Groups[tgid]
	if !hasMember(g, tMem) || !hasMember(g, tMe) || g.V != 3 {
		t.Fatalf("not merged: %+v", g)
	}
	// a stale admin list with a member the host removed doesn't bring them back
	h.onGroupCtl(tAdm, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tAdm, tMem, tNew}, GO: tHost, GAD: []string{tAdm}, GV: 1})
	if g = h.store.Groups[tgid]; hasMember(g, tNew) || g.V != 3 {
		t.Fatalf("removed member came back: %+v", g)
	}
	// same version, admin's list only lacks someone: the host re-issues its own as a new version
	h.onGroupCtl(tAdm, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tAdm, tMe}, GO: tHost, GAD: []string{tAdm}, GV: 3})
	if g = h.store.Groups[tgid]; !hasMember(g, tMem) || g.V != 4 {
		t.Fatalf("resync: %+v", g)
	}
	// host removes tMem: it's remembered
	h.store.mu.Lock()
	g = h.store.Groups[tgid]
	before := g.Members
	g.Members = cleanMembers([]string{tAdm, tMe}, tHost)
	trackGone(&g, before)
	h.store.Groups[tgid] = g
	h.store.mu.Unlock()
	if !hasMember(Group{Members: g.Gone}, tMem) {
		t.Fatalf("gone not tracked: %v", g.Gone)
	}
}

// #5: an admin's delta from an old version is merged by the host (stale approval).
func TestOwnerMergesStaleAdminDelta(t *testing.T) {
	h := tApp(t, tHost)
	h.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tAdm, tMe}, tHost), Owner: tHost, Admins: []string{tAdm}, V: 5}
	h.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 4, GA: packAddrs([]string{tNew}), GR: packAddrs([]string{tMe})})
	g := h.store.Groups[tgid]
	if !hasMember(g, tNew) || !hasMember(g, tMe) || g.V != 6 {
		t.Fatalf("stale delta: %+v", g)
	}
	// a repeat changes nothing
	h.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 4, GA: packAddrs([]string{tNew})})
	if g = h.store.Groups[tgid]; g.V != 6 {
		t.Fatalf("repeat bumped version: %+v", g)
	}
}

// #5: a member doesn't apply a different delta with its own version.
func TestMemberDeltaCollision(t *testing.T) {
	a := tApp(t, tMe)
	tJoined(a, tHost, []string{tHost, tAdm, tMe, tMem}, 3, tAdm)
	a.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 3, GR: packAddrs([]string{tMem})})
	if g := a.store.Groups[tgid]; !hasMember(g, tMem) || g.V != 3 {
		t.Fatalf("same-version delta applied: %+v", g)
	}
	a.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 4, GA: packAddrs([]string{tNew})})
	if g := a.store.Groups[tgid]; !hasMember(g, tNew) || g.V != 4 {
		t.Fatalf("newer delta not applied: %+v", g)
	}
}

// #6: messages for a group we left are acked; a re-add from someone who missed our gl doesn't pull us back.
func TestLeftGroupAckAndNoRejoin(t *testing.T) {
	a := tApp(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: []string{tHost, tMe}, Owner: tHost, Left: true, Quit: true, V: 2}
	if !a.onGroupMsg(tHost, wire{T: "msg", ID: "bbg0", G: tgid, Text: "x", TS: 1}, "") {
		t.Fatal("not acked")
	}
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe, tMem}, GO: tHost, GV: 5})
	if g := a.store.Groups[tgid]; !g.Left || g.Req {
		t.Fatalf("pulled back in: %+v", g)
	}
	// removed (not left ourselves): the host can still bring us back
	b := tApp(t, tMe)
	b.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: []string{tHost}, Owner: tHost, Left: true, V: 2}
	b.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe}, GO: tHost, GV: 3})
	if g := b.store.Groups[tgid]; g.Left || !g.Req {
		t.Fatalf("re-add after removal: %+v", g)
	}
}

// #6: deleting a left group keeps a hidden tombstone, so a late copy doesn't recreate it.
func TestLeftGroupTombstone(t *testing.T) {
	a := tApp(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: []string{tHost, tMe}, Owner: tHost, Left: true, Quit: true}
	a.store.Messages = []Message{{ID: "1", Peer: gKey(tgid), From: tHost, Text: "x"}}
	a.deleteGroupLocked(tgid)
	g, ok := a.store.Groups[tgid]
	if !ok || !g.Tomb || len(a.store.Messages) != 0 {
		t.Fatalf("tombstone: ok=%v %+v msgs=%d", ok, g, len(a.store.Messages))
	}
	if _, shown := a.contactsViewLocked()[gKey(tgid)]; shown {
		t.Fatal("tombstone shown")
	}
	if !a.onGroupMsg(tHost, wire{T: "msg", ID: "ccg0", G: tgid, Text: "late", TS: 1}, "") {
		t.Fatal("late copy not acked")
	}
	if g := a.store.Groups[tgid]; !g.Tomb || g.Stub || len(a.store.Messages) != 0 {
		t.Fatalf("late copy recreated it: %+v", g)
	}
	// an active group (not left) is still deleted outright
	a.store.Groups["1111222233334444"] = Group{ID: "1111222233334444", Members: []string{tHost, tMe}}
	a.deleteGroupLocked("1111222233334444")
	if _, ok := a.store.Groups["1111222233334444"]; ok {
		t.Fatal("active group kept")
	}
}

// #6: the host turns a gl into a new version.
func TestHostLeaveBumpsVersion(t *testing.T) {
	h := tApp(t, tHost)
	h.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tMe, tMem}, tHost), Owner: tHost, V: 4}
	h.onGroupCtl(tMem, wire{T: "gl", G: tgid})
	g := h.store.Groups[tgid]
	if hasMember(g, tMem) || g.V != 5 || !hasMember(Group{Members: g.Gone}, tMem) {
		t.Fatalf("leave: %+v", g)
	}
}

// #7: a stranger's message for an unknown group needs the request PoW (and PowStrict holds).
func TestStubNeedsRequestPow(t *testing.T) {
	a := tApp(t, tMe)
	a.store.PowStrict = true
	gid := "1111222233334444"
	if a.onGroupMsg(tMem, wire{T: "msg", ID: "aa1", G: gid, Text: "spam", TS: 1}, "") {
		t.Fatal("no-pow stub accepted while strict")
	}
	if _, ok := a.store.Groups[gid]; ok {
		t.Fatal("stub stored before the pow check")
	}
	w := wire{T: "msg", ID: "aa2g1", G: gid, Text: "hi", TS: 1}
	w.PN = powSolve(tMem+">g:"+gid, "aa2", 1, "hi", "", 3)
	for powBits(tMem+">g:"+gid, "aa2", 1, "hi", "", w.PN) >= powRequest { // make sure it's only 3..7 bits
		w.TS++
		w.PN = powSolve(tMem+">g:"+gid, "aa2", w.TS, "hi", "", 3)
	}
	if a.onGroupMsg(tMem, w, "") {
		t.Fatal("3-bit stub accepted")
	}
	w.PN = powSolve(tMem+">g:"+gid, "aa2", w.TS, "hi", "", powRequest)
	if !a.onGroupMsg(tMem, w, "") {
		t.Fatal("8-bit stub refused")
	}
	// the stub (unaccepted) keeps asking 8 bits of strangers
	w2 := wire{T: "msg", ID: "aa3g2", G: gid, Text: "more", TS: 2}
	if a.onGroupMsg(tMem, w2, "") {
		t.Fatal("no-pow message into a stub accepted while strict")
	}
	// not strict: legacy (no nonce) is allowed like a 1:1 request
	b := tApp(t, tMe)
	if !b.onGroupMsg(tMem, wire{T: "msg", ID: "aa4", G: gid, Text: "hi", TS: 1}, "") {
		t.Fatal("legacy stub refused while not strict")
	}
	// an accepted contact needs only the default
	c := tApp(t, tMe)
	c.store.PowStrict = true
	c.store.Contacts[tHost] = Contact{Addr: tHost, Shared: true}
	if !c.onGroupMsg(tHost, wire{T: "msg", ID: "aa5", G: gid, Text: "hi", TS: 1}, "") {
		t.Fatal("contact's group message refused")
	}
}

func TestStubCap(t *testing.T) {
	a := tApp(t, tMe)
	for i := 0; i < maxStubs; i++ {
		id := strings.Repeat("0", 12) + hex.EncodeToString([]byte{byte(i >> 8), byte(i)})
		a.store.Groups[id] = Group{ID: id, Stub: true, Req: true, Members: []string{tMem, tMe}}
	}
	if a.onGroupMsg(tMem, wire{T: "msg", ID: "aa1", G: "1111222233334444", Text: "x", TS: 1}, "") {
		t.Fatal("stub over the cap accepted")
	}
}

// #8: a long group text is solved once, the way deliverFile checks it.
func TestLongTextGroupPowReused(t *testing.T) {
	a := tApp(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: []string{tHost, tMe, tMem}, Owner: tMe}
	text := strings.Repeat("あいうえお", 600)
	id, ts := "abc", int64(100)
	copies := []Message{{ID: id + "g0", Peer: tHost, Text: text, TS: ts, Grp: tgid, Of: id, Hid: true}, {ID: id + "g2", Peer: tMem, Text: text, TS: ts, Grp: tgid, Of: id, Hid: true}}
	a.groupPowCopies(tgid, id, ts, text, "", 12, copies)
	s := sha256.Sum256([]byte(text))
	for _, c := range copies {
		if powBits(tMe+">g:"+tgid, id, ts, "", hex.EncodeToString(s[:]), c.Pn) < 12 {
			t.Fatalf("%s: nonce doesn't fit the file check", c.ID)
		}
	}
	short := []Message{{ID: id + "g0", Peer: tHost, Text: "hi", TS: ts, Grp: tgid, Of: id, Hid: true}}
	a.groupPowCopies(tgid, id, ts, "hi", "", 12, short)
	if powBits(tMe+">g:"+tgid, id, ts, "hi", "", short[0].Pn) < 12 {
		t.Fatal("short text nonce")
	}
}

// #10: deleting or blocking a contact keeps our pending group copies to them.
func TestDeleteContactKeepsGroupCopies(t *testing.T) {
	for _, ep := range []string{"/api/contact/delete", "/api/block"} {
		a := tApp(t, tMe)
		mux := http.NewServeMux()
		a.httpHandlers(mux)
		a.featureHandlers(mux)
		a.store.Contacts[tMem] = Contact{Addr: tMem}
		a.store.Messages = []Message{
			{ID: "x", Peer: gKey(tgid), Out: true, Status: "queued"},
			{ID: "xg1", Peer: tMem, Out: true, Status: "queued", Hid: true, Grp: tgid, Of: "x"},
			{ID: "d1", Peer: tMem, Text: "dm"},
		}
		body := `{"Addr":"` + tMem + `","Block":true}`
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("POST", ep, strings.NewReader(body)))
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", ep, rec.Code, rec.Body)
		}
		ms := a.store.Messages
		if len(ms) != 2 || ms[1].ID != "xg1" {
			t.Fatalf("%s kept %+v", ep, ms)
		}
	}
}

// #13: an older (parked) message doesn't undo a newer timer.
func TestParkedTTLOrder(t *testing.T) {
	a := tApp(t, tMe)
	a.store.Contacts[tHost] = Contact{Addr: tHost, Shared: true, HasProf: true}
	a.acceptMsg(tHost, wire{T: "msg", ID: "m2", Text: "new", TS: 200, TTL: 300}, "")
	a.acceptMsg(tHost, wire{T: "msg", ID: "m1", Text: "old", TS: 100, TTL: 0}, "")
	if c := a.store.Contacts[tHost]; c.TTL != 300 {
		t.Fatalf("older message changed the timer: %+v", c)
	}
	a.acceptMsg(tHost, wire{T: "msg", ID: "m3", Text: "newer", TS: 300, TTL: 0}, "")
	if c := a.store.Contacts[tHost]; c.TTL != 0 {
		t.Fatalf("newer message didn't: %+v", c)
	}
}

// #14: a pending join survives a restart and expires.
func TestJoinPersisted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	s := &Store{path: p}
	s.fix()
	a := &App{addr: tMe, store: s}
	s.mu.Lock()
	a.setJoinLocked(tgid, tHost, joinWaitTTL)
	a.setJoinLocked("1111222233334444", tHost, -1)
	s.save()
	s.mu.Unlock()
	s2 := &Store{path: p}
	s2.load()
	s2.fix()
	b := &App{addr: tMe, store: s2, subs: map[chan []byte]struct{}{}}
	if h, ok := b.joinHostLocked(tgid); !ok || h != tHost {
		t.Fatalf("join lost: %v %v", h, ok)
	}
	if _, ok := b.joinHostLocked("1111222233334444"); ok {
		t.Fatal("expired join kept")
	}
	// the host's gi lands as "joined", not as an invitation
	b.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe}, GO: tHost, GV: 2})
	if g := b.store.Groups[tgid]; g.Req || g.Left {
		t.Fatalf("join after restart became an invite: %+v", g)
	}
	if _, ok := b.joinHostLocked(tgid); ok {
		t.Fatal("join not cleared")
	}
	_ = time.Now
}
