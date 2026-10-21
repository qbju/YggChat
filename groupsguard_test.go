package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const tX = "206:1:2:3:4:5:6:8"

func tMux(a *App) *http.ServeMux {
	mux := http.NewServeMux()
	a.groupHandlers(mux)
	a.groupSetHandlers(mux)
	a.inviteHandlers(mux)
	return mux
}

func tPost(t *testing.T, mux *http.ServeMux, ep, body string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", ep, strings.NewReader(body)))
	return rec
}

// waitSent polls until the node sent at least one typ packet to to.
func waitSent(pc *fakePC, to, typ string) []wire {
	for i := 0; i < 100; i++ {
		if ws := pc.sent(to, typ); len(ws) > 0 {
			return ws
		}
		time.Sleep(20 * time.Millisecond)
	}
	return nil
}

// Known leftover: after leaving, our own list still showed us (and the old members as if current).
func TestLeaverListDropsSelf(t *testing.T) {
	a, pc := tAppPC(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost, V: 3}
	if rec := tPost(t, tMux(a), "/api/group/leave", `{"ID":"`+tgid+`"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	g := a.store.Groups[tgid]
	if !g.Left || !g.Quit || hasMember(g, tMe) || !hasMember(g, tHost) {
		t.Fatalf("after leave: %+v", g)
	}
	// the gl still goes to everyone
	if waitSent(pc, tMem, "gl") == nil {
		t.Fatal("no gl to the other member")
	}
}

// Removed members could keep posting forever by backdating their posts
// before the removal ("late copies"); now only for a while and only so many.
func TestRemovedMemberBackdatedPosts(t *testing.T) {
	gid := "1111000011110000"
	a := tApp(t, tMe)
	now := time.Now().Unix()
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost}, tMe), Owner: tHost, GoneAt: map[string]int64{tMem: now - 60}}
	n := 0
	for i := 0; i < lateMax+10; i++ {
		a.onGroupMsg(tMem, wire{T: "msg", ID: fmt.Sprintf("a%dg1", i), G: gid, Text: "late", TS: now - 100}, "")
	}
	for _, m := range a.store.Messages {
		if m.Peer == gKey(gid) && m.From == tMem {
			n++
		}
	}
	if n != lateMax {
		t.Fatalf("late posts kept: %d, want %d", n, lateMax)
	}
	// long after the removal: nothing, whatever ts it claims
	b := tApp(t, tMe)
	b.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost}, tMe), Owner: tHost, GoneAt: map[string]int64{tMem: now - lateGrace - 10}}
	if !b.onGroupMsg(tMem, wire{T: "msg", ID: "bbg1", G: gid, Text: "old?", TS: now - lateGrace - 20}, "") {
		t.Fatal("not acked")
	}
	if len(b.store.Messages) != 0 {
		t.Fatalf("stored: %+v", b.store.Messages)
	}
}

// Known leftover: a removed member's post made while the host was cut off
// reached members who hadn't heard of the removal; once they learn it (with
// the removal time) the post goes and is never relayed.
func TestRemovalTimeDropsLaterPosts(t *testing.T) {
	gid := "2222000022220000"
	a, pc := tAppPC(t, tMe)
	now := time.Now().Unix()
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost, tMem, tX}, tMe), Owner: tHost, V: 4}
	a.onGroupMsg(tMem, wire{T: "msg", ID: "c1g0", G: gid, Text: "before", TS: now - 600}, "")
	a.onGroupMsg(tMem, wire{T: "msg", ID: "c2g0", G: gid, Text: "after removal", TS: now - 100}, "")
	if len(groupMsgsOf(a, gid)) != 2 {
		t.Fatal("setup")
	}
	// the host removed tMem 500 s ago (it was cut off; we hear it only now)
	a.onGroupCtl(tHost, wire{T: "gi", G: gid, GN: "G", GM: cleanMembers([]string{tHost, tX}, tMe), GO: tHost, GV: 5, TS: now - 500, GR: packAddrs([]string{tMem}), K: "kick"})
	ms := groupMsgsOf(a, gid)
	if len(ms) != 1 || ms[0].Text != "before" {
		t.Fatalf("after removal: %+v", ms)
	}
	// a late post from after the removal isn't taken either
	a.onGroupMsg(tMem, wire{T: "msg", ID: "c3g0", G: gid, Text: "later", TS: now - 50}, "")
	if len(groupMsgsOf(a, gid)) != 1 {
		t.Fatal("post after removal taken")
	}
	// and a catch-up answer never carries one (even if it got in some other way)
	a.store.Messages = append(a.store.Messages, Message{ID: "c4", Peer: gKey(gid), From: tMem, Text: "sneaked", TS: now - 10})
	a.onGroupCtl(tX, wire{T: "ghq", G: gid, TS: 1})
	time.Sleep(3500 * time.Millisecond)
	for _, w := range pc.sent(tX, "ghs") {
		if w.Text == "sneaked" || w.Text == "after removal" {
			t.Fatalf("relayed %q", w.Text)
		}
	}
	if len(pc.sent(tX, "ghs")) == 0 {
		t.Fatal("no catch-up answer")
	}
}

func groupMsgsOf(a *App, gid string) []Message {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []Message
	for _, m := range a.store.Messages {
		if m.Peer == gKey(gid) && m.Sys == "" && !m.Hid {
			out = append(out, m)
		}
	}
	return out
}

// Relayed history: only when asked (ordinary members), only posts by
// members, the group timer applies, and the sender's own copy wins.
func TestHistoryRelayRules(t *testing.T) {
	gid := "3333000033330000"
	a := tApp(t, tMe)
	now := time.Now().Unix()
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost, tMem, tX}, tMe), Owner: tHost}
	// unasked relay from an ordinary member: dropped
	a.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "d1", By: tX, Text: "unasked", TS: now})
	if len(groupMsgsOf(a, gid)) != 0 {
		t.Fatal("unasked relay taken")
	}
	noteHistAsk(gid, tMem)
	// a post by someone who was never a member: dropped
	a.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "d2", By: tNew, Text: "stranger", TS: now})
	if len(groupMsgsOf(a, gid)) != 0 {
		t.Fatal("made-up post by a stranger taken")
	}
	// the host may relay pre-join posts of people we never knew
	a.onGroupCtl(tHost, wire{T: "ghs", G: gid, ID: "d3", By: tNew, Text: "old member", TS: now - 50})
	// an asked member relays an altered post by tX
	a.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "d4", By: tX, Text: "forged", TS: now})
	ms := groupMsgsOf(a, gid)
	if len(ms) != 2 || ms[1].Via != tMem {
		t.Fatalf("relays: %+v", ms)
	}
	// tX's own copy replaces the relayed one
	if !a.onGroupMsg(tX, wire{T: "msg", ID: "d4g2", G: gid, Text: "real", TS: now}, "") {
		t.Fatal("direct copy not acked")
	}
	ms = groupMsgsOf(a, gid)
	if len(ms) != 2 || ms[1].Text != "real" || ms[1].Via != "" || ms[1].Hist {
		t.Fatalf("direct copy didn't win: %+v", ms)
	}
	// disappearing messages: the group's timer counts from the post time
	b := tApp(t, tMe)
	b.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost, Set: GroupSet{TTL: 3600}}
	b.onGroupCtl(tHost, wire{T: "ghs", G: gid, ID: "e1", By: tMem, Text: "expired", TS: now - 7200})
	b.onGroupCtl(tHost, wire{T: "ghs", G: gid, ID: "e2", By: tMem, Text: "live", TS: now - 600})
	ms = groupMsgsOf(b, gid)
	if len(ms) != 1 || ms[0].Exp != now-600+3600 {
		t.Fatalf("group TTL on history: %+v", ms)
	}
}

// With history sharing off, a member added later got everything by asking
// with ts=0; now only what was said since it was added.
func TestHistReqHistOffSinceAdded(t *testing.T) {
	gid := "4444000044440000"
	a, pc := tAppPC(t, tMe)
	now := time.Now().Unix()
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost}, tMe), Owner: tHost, V: 1}
	a.store.Messages = append(a.store.Messages, Message{ID: "f1", Peer: gKey(gid), From: tHost, Text: "secret before", TS: now - 1000})
	a.onGroupCtl(tHost, wire{T: "gi", G: gid, GN: "G", GM: cleanMembers([]string{tHost, tMem}, tMe), GO: tHost, GV: 2, TS: now - 10})
	if a.store.Groups[gid].AddAt[tMem] == 0 {
		t.Fatalf("add time not kept: %+v", a.store.Groups[gid])
	}
	a.store.Messages = append(a.store.Messages, Message{ID: "f2", Peer: gKey(gid), From: tHost, Text: "after", TS: now})
	a.onGroupCtl(tMem, wire{T: "ghq", G: gid, TS: 0})
	time.Sleep(3500 * time.Millisecond)
	got := pc.sent(tMem, "ghs")
	if len(got) == 0 {
		t.Fatal("no answer")
	}
	for _, w := range got {
		if w.Text == "secret before" {
			t.Fatal("history from before they were added leaked")
		}
	}
}

// An admin can't push the version out of reach (V+huge or overflow) or
// overfill the group; members ask the host instead.
func TestAdminVersionJumpAndLimit(t *testing.T) {
	gid := "5555000055550000"
	a, pc := tAppPC(t, tMe)
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost, tAdm}, tMe), Owner: tHost, Admins: []string{tAdm}, V: 3}
	a.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: cleanMembers([]string{tHost, tAdm, tMem}, tMe), GO: tHost, GAD: []string{tAdm}, GV: 1 << 62})
	g := a.store.Groups[gid]
	if g.V != 3 || hasMember(g, tMem) {
		t.Fatalf("huge admin version taken: %+v", g)
	}
	if waitSent(pc, tHost, "gq") == nil {
		t.Fatal("didn't ask the host")
	}
	a.onGroupCtl(tAdm, wire{T: "gd", G: gid, GV: 1 << 62, GA: packAddrs([]string{tMem})})
	if g := a.store.Groups[gid]; g.V != 3 || hasMember(g, tMem) {
		t.Fatalf("huge admin delta taken: %+v", g)
	}
	// over the limit (24 by default)
	big := manyAddrs(30)
	for i := 0; i < len(big); i += partAddrs {
		a.onGroupCtl(tAdm, wire{T: "gd", G: gid, GV: 4, GA: packAddrs(big[i:min(i+partAddrs, len(big))])})
	}
	if g := a.store.Groups[gid]; len(g.Members) > groupMax(g) {
		t.Fatalf("admin overfilled the group: %d", len(g.Members))
	}
	// a normal next version still works
	a.onGroupCtl(tAdm, wire{T: "gd", G: gid, GV: 4, GA: packAddrs([]string{tMem})})
	if g := a.store.Groups[gid]; g.V != 4 || !hasMember(g, tMem) {
		t.Fatalf("normal admin delta: %+v", g)
	}
	// the host's own word still goes (trusted), any version
	a.onGroupCtl(tHost, wire{T: "gi", G: gid, GN: "G", GM: cleanMembers([]string{tHost, tAdm}, tMe), GO: tHost, GAD: []string{tAdm}, GV: 500})
	if g := a.store.Groups[gid]; g.V != 500 || hasMember(g, tMem) {
		t.Fatalf("host list: %+v", g)
	}
}

// A host that left (no heir) can't keep changing the list with gd.
func TestLeftOwnerDeltaIgnored(t *testing.T) {
	gid := "6666000066660000"
	a := tApp(t, tMe)
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tMem}, tMe), Owner: tHost, V: 3}
	a.onGroupCtl(tHost, wire{T: "gd", G: gid, GV: 4, GA: packAddrs([]string{tHost, tX})})
	if g := a.store.Groups[gid]; g.V != 3 || hasMember(g, tX) {
		t.Fatalf("left owner changed the list: %+v", g)
	}
}

// Strangers could fill the store with invitations.
func TestInvitationSpamCapped(t *testing.T) {
	a := tApp(t, tMe)
	for i := 0; i < 10; i++ {
		gid := fmt.Sprintf("77770000%08x", i)
		a.onGroupCtl(tX, wire{T: "gi", G: gid, GN: "spam", GM: []string{tX, tMe}, GO: tX, GV: 1})
	}
	if n := len(a.store.Groups); n != maxInvitesPer {
		t.Fatalf("invitations from one stranger: %d", n)
	}
	for i := 0; i < 200; i++ {
		from := fmt.Sprintf("2%02x:%x::9", 0x10+i%200, i+1)
		gid := fmt.Sprintf("88880000%08x", i)
		a.onGroupCtl(from, wire{T: "gi", G: gid, GN: "spam", GM: []string{from, tMe}, GO: from, GV: 1})
	}
	if n := len(a.store.Groups); n > maxInvites {
		t.Fatalf("invitations kept: %d", n)
	}
	// an accepted contact's invitation still lands
	a.store.Contacts[tHost] = Contact{Addr: tHost, Shared: true}
	a.onGroupCtl(tHost, wire{T: "gi", G: "9999000099990000", GN: "ok", GM: []string{tHost, tMe}, GO: tHost, GV: 1})
	if _, ok := a.store.Groups["9999000099990000"]; !ok {
		t.Fatal("contact's invitation dropped")
	}
}

// Split-list buffers for made-up group ids were unbounded.
func TestPartBuffersBounded(t *testing.T) {
	a := tApp(t, tMe)
	gps.mu.Lock()
	gps.bufs = map[string]*partBuf{}
	gps.mu.Unlock()
	for i := 0; i < 500; i++ {
		from := tX
		if i%2 == 0 {
			from = fmt.Sprintf("2%02x:%x::7", 0x10+i%200, i+1)
		}
		a.onInfoPart(from, wire{T: "gp", G: fmt.Sprintf("abab0000%08x", i), GV: 1, GH: "x", I: 0, N: 3, GMB: packAddrs(manyAddrs(40))})
	}
	gps.mu.Lock()
	n, mine := len(gps.bufs), 0
	for k := range gps.bufs {
		if strings.HasSuffix(k, "|"+tX) {
			mine++
		}
	}
	gps.mu.Unlock()
	if n > maxPartBufs || mine > maxPartBufsPer {
		t.Fatalf("buffers: %d (from one sender %d)", n, mine)
	}
}

// Members removed by a manager can't come back through the invite link.
func TestRemovedMemberCantRejoinByLink(t *testing.T) {
	gid := "aaaa0000aaaa0000"
	h, pc := tAppPC(t, tHost)
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tMem, tAdm}, tHost), Owner: tHost, Admins: []string{tAdm}, V: 2, Tok: "0011223344556677"}
	mux := tMux(h)
	if rec := tPost(t, mux, "/api/group/update", `{"ID":"`+gid+`","Remove":["`+tMem+`"]}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	h.onGroupCtl(tMem, wire{T: "gj", G: gid, Tok: "0011223344556677", Nick: "back"})
	if hasMember(h.store.Groups[gid], tMem) {
		t.Fatal("rejoined by link after removal")
	}
	if ws := waitSent(pc, tMem, "gjn"); ws == nil || !strings.Contains(ws[0].Text, "外されて") {
		t.Fatalf("refusal: %+v", ws)
	}
	// the removal tells the admin it was a kick
	var gi *wire
	for _, w := range waitSent(pc, tAdm, "gi") {
		if w.K == "kick" {
			w := w
			gi = &w
		}
	}
	if gi == nil || len(unpackAddrs(gi.GR)) != 1 {
		t.Fatalf("no kick info to the admin: %+v", pc.sent(tAdm, "gi"))
	}
	ad := tApp(t, tAdm)
	ad.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tMem, tHost}, tAdm), Owner: tHost, Admins: []string{tAdm}, V: 2, Tok: "8899aabbccddeeff"}
	ad.onGroupCtl(tHost, *gi)
	if !banned(ad.store.Groups[gid], tMem) {
		t.Fatalf("admin didn't note the kick: %+v", ad.store.Groups[gid])
	}
	// a leave is not a kick
	h.onGroupCtl(tAdm, wire{T: "gl", G: gid})
	if banned(h.store.Groups[gid], tAdm) {
		t.Fatal("a leaver was banned")
	}
	// a direct add lets them back in
	if rec := tPost(t, mux, "/api/group/update", `{"ID":"`+gid+`","Add":["`+tMem+`"]}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	if g := h.store.Groups[gid]; !hasMember(g, tMem) || banned(g, tMem) {
		t.Fatalf("direct add: %+v", g)
	}
}

// Two admins removing different members from the same version: the host
// kept only one of the removals in classic (small) groups.
func TestConcurrentAdminRemovals(t *testing.T) {
	gid := "bbbb0000bbbb0000"
	const tAdm2 = "207:1:2:3:4:5:6:9"
	h := tApp(t, tHost)
	base := cleanMembers([]string{tAdm, tAdm2, tMem, tX, tMe}, tHost)
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: base, Owner: tHost, Admins: cleanAdmins([]string{tAdm, tAdm2}, Group{Owner: tHost, Members: base}), V: 1}
	without := func(x string) []string {
		var out []string
		for _, m := range base {
			if m != x {
				out = append(out, m)
			}
		}
		return out
	}
	ads := h.store.Groups[gid].Admins
	h.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: without(tMem), GO: tHost, GAD: ads, GV: 2, GR: packAddrs([]string{tMem}), K: "kick"})
	h.onGroupCtl(tAdm2, wire{T: "gi", G: gid, GN: "G", GM: without(tX), GO: tHost, GAD: ads, GV: 2, GR: packAddrs([]string{tX}), K: "kick"})
	g := h.store.Groups[gid]
	if hasMember(g, tMem) || hasMember(g, tX) || g.V != 3 || !hasMember(g, tMe) {
		t.Fatalf("merged: %+v", g)
	}
	if !banned(g, tMem) || !banned(g, tX) {
		t.Fatalf("kicks not noted: %+v", g.Ban)
	}
	// an add and a removal from the same base merge too
	h2 := tApp(t, tHost)
	h2.store.Groups[gid] = Group{ID: gid, Name: "G", Members: base, Owner: tHost, Admins: ads, V: 1}
	h2.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: append(append([]string{}, base...), tNew), GO: tHost, GAD: ads, GV: 2})
	h2.onGroupCtl(tAdm2, wire{T: "gi", G: gid, GN: "G", GM: without(tX), GO: tHost, GAD: ads, GV: 2, GR: packAddrs([]string{tX})})
	if g := h2.store.Groups[gid]; !hasMember(g, tNew) || hasMember(g, tX) || g.V != 3 {
		t.Fatalf("add+remove: %+v", g)
	}
	// an ordinary member's gr means nothing
	h3 := tApp(t, tHost)
	h3.store.Groups[gid] = Group{ID: gid, Name: "G", Members: base, Owner: tHost, Admins: ads, V: 1}
	h3.onGroupCtl(tMem, wire{T: "gi", G: gid, GN: "G", GM: without(tX), GO: tHost, GV: 1, GR: packAddrs([]string{tX}), K: "kick"})
	if g := h3.store.Groups[gid]; !hasMember(g, tX) || banned(g, tX) {
		t.Fatalf("member removed someone: %+v", g)
	}
}

// A host leaving while the first admin is offline: the heir is the admin
// heard from last, and the hand-over is re-sent when the heir turns up.
func TestHandOverToOfflineHeir(t *testing.T) {
	gid := "cccc0000cccc0000"
	const tAdm2 = "207:1:2:3:4:5:6:9"
	h, pc := tAppPC(t, tHost)
	ms := cleanMembers([]string{tAdm, tAdm2, tMem}, tHost)
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: cleanAdmins([]string{tAdm, tAdm2}, Group{Owner: tHost, Members: ms}), V: 5, SV: 2}
	h.store.Messages = append(h.store.Messages, Message{ID: "x1", Peer: gKey(gid), From: tAdm2, Text: "here", TS: time.Now().Unix()})
	if rec := tPost(t, tMux(h), "/api/group/leave", `{"ID":"`+gid+`"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	g := h.store.Groups[gid]
	if g.Owner != tAdm2 || !g.Hand || hasMember(g, tHost) {
		t.Fatalf("hand-over: %+v", g)
	}
	// the heir (offline until now) asks us for the history: it gets the hand-over, then our gl
	pc.mu.Lock()
	pc.pkts = nil
	pc.mu.Unlock()
	h.onGroupCtl(tAdm2, wire{T: "ghq", G: gid, TS: 1})
	gi := waitSent(pc, tAdm2, "gi")
	if gi == nil || gi[0].GO != tAdm2 || !hasMember(Group{Members: gi[0].GM}, tHost) {
		t.Fatalf("hand-over info: %+v", gi)
	}
	time.Sleep(3200 * time.Millisecond)
	if len(pc.sent(tAdm2, "gl")) == 0 {
		t.Fatal("no gl after the hand-over")
	}
	// the heir takes it: host, then without the old host
	heir := tApp(t, tAdm2)
	heir.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: cleanAdmins([]string{tAdm, tAdm2}, Group{Owner: tHost, Members: ms}), V: 5, SV: 2}
	heir.onGroupCtl(tHost, gi[0])
	heir.onGroupCtl(tHost, wire{T: "gl", G: gid, TS: time.Now().Unix()})
	if hg := heir.store.Groups[gid]; hg.Owner != tAdm2 || hasMember(hg, tHost) {
		t.Fatalf("heir: %+v", hg)
	}
	// anyone else just gets our gl
	h.onGroupCtl(tMem, wire{T: "gq", G: gid})
	if waitSent(pc, tMem, "gl") == nil {
		t.Fatal("non-heir notice")
	}
}

// A made-up invitation for the group (its list without the host) blocked a
// link join: the host's answer was refused as "not a member".
func TestJoinOverFakeInvitation(t *testing.T) {
	gid := "dddd0000dddd0000"
	a := tApp(t, tMe)
	mux := tMux(a)
	a.onGroupCtl(tX, wire{T: "gi", G: gid, GN: "fake", GM: []string{tX, tMe}, GO: tX, GV: 9})
	if g := a.store.Groups[gid]; !g.Req {
		t.Fatalf("setup: %+v", g)
	}
	code := encodeJoin(tHost, gid, "0011223344556677")
	if rec := tPost(t, mux, "/api/group/join", `{"Code":"`+code+`"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	a.onGroupCtl(tHost, wire{T: "gi", G: gid, GN: "Real", GM: cleanMembers([]string{tHost}, tMe), GO: tHost, GV: 2})
	g := a.store.Groups[gid]
	if g.Owner != tHost || g.Name != "Real" || g.Req || hasMember(g, tX) {
		t.Fatalf("join: %+v", g)
	}
}

// Member limit 1024: a full list with 10 admins, a worst-case name and every
// setting still fits the datagrams; the 1025th is refused at the host.
func TestMaxGroupBoundary(t *testing.T) {
	h, pc := tAppPC(t, tHost)
	var ms []string
	for i := 0; len(ms) < maxGroupCap-2; i++ {
		ms = append(ms, fmt.Sprintf("2%02x:ffff:ffff:ffff:ffff:ffff:%04x:%04x", 0x10+i%200, i, 0xffff-i))
	}
	ms = cleanMembers(ms, tHost)
	if len(ms) != maxGroupCap-1 {
		t.Fatal(len(ms))
	}
	g := Group{ID: tgid, Name: strings.Repeat("<", 32), Members: ms, Owner: tHost, V: 1 << 40, SV: 1 << 40, VAt: time.Now().Unix(), Tok: "0011223344556677",
		Set: GroupSet{Max: maxGroupCap, PoW: 16, TTL: 2419200, NoFiles: true, Hist: true}}
	g.Admins = cleanAdmins(ms[len(ms)-maxAdmins:], g)
	h.store.Groups[tgid] = g
	h.onGroupCtl(tX, wire{T: "gj", G: tgid, Tok: "0011223344556677"})
	g = h.store.Groups[tgid]
	if len(g.Members) != maxGroupCap {
		t.Fatalf("1024th not admitted: %d", len(g.Members))
	}
	for _, p := range h.infoPackets(g) {
		if n := wireLen(p); n > dgramMax {
			t.Fatalf("%s is %d bytes", p.T, n)
		}
	}
	h.onGroupCtl(tNew, wire{T: "gj", G: tgid, Tok: "0011223344556677"})
	if hasMember(h.store.Groups[tgid], tNew) || waitSent(pc, tNew, "gjn") == nil {
		t.Fatal("1025th admitted")
	}
}

// While we wait for the host of a link we opened, someone else's info for
// the group (without the host) arrived first and the host's was refused.
func TestJoinHijackByOtherInfo(t *testing.T) {
	gid := "eeee0000eeee0000"
	a := tApp(t, tMe)
	code := encodeJoin(tHost, gid, "0011223344556677")
	if rec := tPost(t, tMux(a), "/api/group/join", `{"Code":"`+code+`"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	a.onGroupCtl(tX, wire{T: "gi", G: gid, GN: "fake", GM: []string{tX, tMe}, GO: tX, GV: 9})
	a.onGroupCtl(tHost, wire{T: "gi", G: gid, GN: "Real", GM: cleanMembers([]string{tHost, tMem}, tMe), GO: tHost, GV: 2})
	g := a.store.Groups[gid]
	if g.Owner != tHost || g.Name != "Real" || g.Req || hasMember(g, tX) || g.V != 2 {
		t.Fatalf("join: %+v", g)
	}
}

// A full request list stayed full for good; requests left for days make room.
func TestPendingListExpires(t *testing.T) {
	gid := "ffff0000ffff0000"
	h := tApp(t, tHost)
	old := time.Now().Unix() - pendKeep - 10
	var ps []Pend
	for i := 0; i < maxPending; i++ {
		ps = append(ps, Pend{Addr: fmt.Sprintf("2%02x::%x", 0x10+i, i+1), TS: old})
	}
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tMem}, tHost), Owner: tHost, Tok: "0011223344556677", Set: GroupSet{Approve: true}, Pend: ps}
	h.onGroupCtl(tX, wire{T: "gj", G: gid, Tok: "0011223344556677"})
	g := h.store.Groups[gid]
	if len(g.Pend) != 1 || g.Pend[0].Addr != tX {
		t.Fatalf("pending: %+v", g.Pend)
	}
}

// A member that missed the old host's hand-over (but got its gl) took
// nothing from the new host: the group had no host for it.
func TestMissedHandOverHeirClaims(t *testing.T) {
	gid := "abcd0000abcd0000"
	a := tApp(t, tMe)
	ms := cleanMembers([]string{tAdm, tMem}, tMe) // the host already left
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 6, SV: 2}
	// an ordinary member can't claim it
	a.onGroupCtl(tMem, wire{T: "gi", G: gid, GN: "G", GM: ms, GO: tMem, GV: 6, GSV: 9, GS: &GroupSet{NoFiles: true}})
	if g := a.store.Groups[gid]; g.Owner != tHost || g.Set.NoFiles {
		t.Fatalf("member claimed: %+v", g)
	}
	a.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: ms, GO: tAdm, GV: 6, GSV: 3, GS: &GroupSet{NoFiles: true}})
	g := a.store.Groups[gid]
	if g.Owner != tAdm || !g.Set.NoFiles || g.SV != 3 {
		t.Fatalf("heir's claim: %+v", g)
	}
	// while the host is still a member, an admin can't
	b := tApp(t, tMe)
	ms2 := cleanMembers([]string{tAdm, tMem, tHost}, tMe)
	b.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms2, Owner: tHost, Admins: []string{tAdm}, V: 6, SV: 2}
	b.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: ms2, GO: tAdm, GV: 6, GSV: 3})
	if g := b.store.Groups[gid]; g.Owner != tHost {
		t.Fatalf("admin took a live host's group: %+v", g)
	}
}

// Chaos 6d: the host (cut off) removed C, an admin let D in meanwhile, and the
// host merged both into a later version. The merged list's own time is after
// C's post; the per-member time ("gt") still drops it.
func TestMergedVersionKeepsRemovalTime(t *testing.T) {
	gid := "abab1111abab1111"
	now := time.Now().Unix()
	h := tApp(t, tHost)
	ms := cleanMembers([]string{tAdm, tMem, tX}, tHost)
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 3, VAt: now, GoneAt: map[string]int64{tMem: now - 500}}
	h.store.Groups[gid] = func() Group { g := h.store.Groups[gid]; g.Members = cleanMembers([]string{tAdm, tX, tNew}, tHost); return g }()
	ps := h.infoPackets(h.store.Groups[gid])
	if len(ps) != 1 || ps[0].GT[tMem] != now-500 {
		t.Fatalf("gt not sent: %+v", ps)
	}
	b := tApp(t, tX)
	b.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 2}
	b.onGroupMsg(tMem, wire{T: "msg", ID: "aa1g0", G: gid, Text: "while host dark", TS: now - 300}, "")
	b.onGroupMsg(tMem, wire{T: "msg", ID: "aa2g0", G: gid, Text: "before removal", TS: now - 900}, "")
	b.onGroupCtl(tHost, ps[0])
	got := groupMsgsOf(b, gid)
	if len(got) != 1 || got[0].Text != "before removal" {
		t.Fatalf("after merged list: %+v", got)
	}
	// a member's gt means nothing
	c := tApp(t, tX)
	c.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 2}
	c.onGroupMsg(tAdm, wire{T: "msg", ID: "bb1g0", G: gid, Text: "admin post", TS: now - 300}, "")
	c.onGroupCtl(tMem, wire{T: "gi", G: gid, GN: "G", GM: ms, GO: tHost, GV: 2, GT: map[string]int64{tAdm: now - 1000}})
	if len(groupMsgsOf(c, gid)) != 1 {
		t.Fatal("member's gt purged posts")
	}
}

// An admin adding a removed member back clears the host's bar too.
func TestBanClearedWhenAddedBack(t *testing.T) {
	gid := "abab2222abab2222"
	h := tApp(t, tHost)
	ms := cleanMembers([]string{tAdm, tX}, tHost)
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 3, Ban: []string{tMem}, Gone: []string{tMem}}
	h.onGroupCtl(tAdm, wire{T: "gi", G: gid, GN: "G", GM: append(append([]string{}, ms...), tMem), GO: tHost, GAD: []string{tAdm}, GV: 4})
	if g := h.store.Groups[gid]; !hasMember(g, tMem) || banned(g, tMem) {
		t.Fatalf("re-add by admin: %+v", g)
	}
}
