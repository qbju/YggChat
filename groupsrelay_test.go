package main

import (
	"strings"
	"testing"
	"time"
)

// A catch-up relay (ghs) carries a photo only as a "📷" line and a long text
// shortened (so unsigned). When the sender's own copy arrives later (a file
// transfer: fh), it must still come in and replace the relay; it used to be
// acked at the header as "already here", so the photo never arrived and the
// shortened text stayed marked 中継 (r2-matrix G4/G3n).
func TestRelayedPhotoAndLongTextReplaced(t *testing.T) {
	gid := "7171000071710000"
	y, _ := tKeyApp(t) // the poster
	z, _ := tKeyApp(t) // the receiver
	ms := cleanMembers([]string{y.addr, z.addr, tMem}, tHost)
	z.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, V: 2}

	// photo: relayed placeholder first
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "p1aa", By: y.addr, Text: "📷 cap", TS: 1000})
	got := groupMsgsOf(z, gid)
	if len(got) != 1 || got[0].Via != tMem || got[0].Img != "" {
		t.Fatalf("placeholder: %+v", got)
	}
	if z.hasGroupFile(gid, y.addr, "p1aag1", "image/jpeg") {
		t.Fatal("photo header would be acked as already here")
	}
	img := "data:image/png;base64,iVBORw0KGgo="
	if !z.onGroupMsg(y.addr, wire{T: "msg", G: gid, ID: "p1aag1", Nick: "y", Text: "cap", TS: 1000}, img) {
		t.Fatal("own photo copy not taken")
	}
	got = groupMsgsOf(z, gid)
	if len(got) != 1 || got[0].Img != img || got[0].Via != "" || got[0].Hist || got[0].Text != "cap" {
		t.Fatalf("photo not replaced: %+v", got)
	}
	if !z.hasGroupFile(gid, y.addr, "p1aag2", "image/jpeg") {
		t.Fatal("a resend of the photo would be transferred again")
	}

	// long text: shortened relay, then the full text as a text/plain transfer
	long := strings.Repeat("あ", 1500)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "t1bb", By: y.addr, Text: long[:600] + "…（省略）", TS: 1001})
	if z.hasGroupFile(gid, y.addr, "t1bbg1", textMime) {
		t.Fatal("long-text header would be acked as already here")
	}
	if !z.onGroupMsg(y.addr, wire{T: "msg", G: gid, ID: "t1bbg1", Nick: "y", Text: long, TS: 1001, checked: true}, "") {
		t.Fatal("own long text not taken")
	}
	got = groupMsgsOf(z, gid)
	if len(got) != 2 || got[1].Text != long || got[1].Via != "" || got[1].Hist {
		t.Fatalf("long text not replaced: %d %+v", len(got), got[len(got)-1].Via)
	}
}

// The info sent when a group is made can be lost (a member's route isn't
// there yet): the host sends it again on the member's hello until it hears
// from it about the group (r2-matrix G5/G3n "group reached all").
func TestGroupInfoResentUntilHeard(t *testing.T) {
	h, pc := tAppPC(t, tHost)
	g, err := h.createGroup("G", []string{tMem, tAdm})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(4500 * time.Millisecond) // the three quick rounds
	n0 := len(pc.sent(tMem, "gi"))
	if n0 == 0 {
		t.Fatal("no info sent")
	}
	inv.mu.Lock()
	inv.sent[g.ID+"|"+tMem] = time.Time{} // as if 10 s had passed
	inv.sent[g.ID+"|"+tAdm] = time.Time{}
	inv.mu.Unlock()
	invHeard(g.ID, tAdm) // tAdm asked something about the group: it has it
	h.pushInvites(tMem)
	h.pushInvites(tAdm)
	if n := len(pc.sent(tMem, "gi")); n != n0+1 {
		t.Fatalf("not re-sent on hello: %d -> %d", n0, n)
	}
	if n := len(pc.sent(tAdm, "gi")); n != n0 {
		t.Fatalf("re-sent to a member we heard from: %d", n)
	}
	h.pushInvites(tMem) // within 10 s: held back
	if n := len(pc.sent(tMem, "gi")); n != n0+1 {
		t.Fatalf("re-sent too often: %d", n)
	}
	invHeard(g.ID, tMem)
	if h.pushInvite(g.ID, tMem) {
		t.Fatal("still pending after hearing from it")
	}
}

// Catch-up also asks the host for the info: a removal made while we were
// away must reach us even if nobody posts (r2-matrix G5).
func TestSyncGroupAsksInfo(t *testing.T) {
	gid := "abab6666abab6666"
	b, pc := tAppPC(t, tMem)
	b.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tHost, tMem, tAdm}, ""), Owner: tHost, Admins: []string{tAdm}}
	b.syncGroup(gid)
	if len(pc.sent(tHost, "gq")) != 1 || len(pc.sent(tAdm, "gq")) != 1 {
		t.Fatalf("gq: host %d admin %d", len(pc.sent(tHost, "gq")), len(pc.sent(tAdm, "gq")))
	}
	time.Sleep(4500 * time.Millisecond)
	if len(pc.sent(tHost, "gq")) != 2 {
		t.Fatalf("second gq: %d", len(pc.sent(tHost, "gq")))
	}
}
