package main

import (
	"strings"
	"testing"
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

