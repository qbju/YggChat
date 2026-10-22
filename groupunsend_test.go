package main

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

// unsendGroup is a group of real-key nodes: y (author), z (member), h (host),
// d (admin), plus the keyless member tMem used as a relayer.
func unsendGroup(t *testing.T) (gid string, y, z, h, d *App, ypc, zpc, hpc *fakePC) {
	gid = "6d6d00006d6d0000"
	y, ypc = tKeyApp(t)
	z, zpc = tKeyApp(t)
	h, hpc = tKeyApp(t)
	d, _ = tKeyApp(t)
	ms := cleanMembers([]string{y.addr, z.addr, h.addr, d.addr, tMem}, "")
	for _, a := range []*App{y, z, h, d} {
		a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: h.addr, Admins: []string{d.addr}, V: 3}
	}
	return
}

func firstCopy(t *testing.T, pc *fakePC, to string) wire {
	t.Helper()
	ws := waitSent(pc, to, "msg")
	if len(ws) == 0 {
		t.Fatal("no copy sent")
	}
	return ws[0]
}

func hasPost(a *App, gid, by, pid string) bool {
	for _, m := range groupMsgsOf(a, gid) {
		if m.From == by && samePost(m.ID, pid) {
			return true
		}
	}
	return false
}

func delMux(a *App) *http.ServeMux {
	mux := tMux(a)
	a.msgEditHandlers(mux)
	return mux
}

func TestGroupUnsendByAuthor(t *testing.T) {
	gid, y, z, _, _, ypc, zpc, _ := unsendGroup(t)
	vis, err := y.sendGroup(gid, "oops", "")
	if err != nil {
		t.Fatal(err)
	}
	cp := firstCopy(t, ypc, z.addr)
	if !z.onGroupMsg(y.addr, cp, "") || !hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("post not stored")
	}
	// y unsends it (API)
	rec := tPost(t, delMux(y), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if hasPost(y, gid, y.addr, vis.ID) {
		t.Fatal("still on the author's screen")
	}
	ws := waitSent(ypc, z.addr, "del")
	if len(ws) == 0 || ws[0].G != gid || ws[0].Ref != vis.ID || ws[0].By != y.addr || ws[0].Sg == "" || ws[0].Dby != "" {
		t.Fatalf("del packet: %+v", ws)
	}
	// queued for every other member (also the keyless relayer, who may be offline)
	y.store.mu.Lock()
	n := 0
	for _, c := range y.store.Ctl {
		if c.W.T == "del" && c.W.G == gid {
			n++
		}
	}
	y.store.mu.Unlock()
	if n == 0 {
		t.Fatal("no queued del (offline members would never get it)")
	}
	// z gets it through handlePacket and acks
	b, _ := json.Marshal(ws[0])
	z.handlePacket(net.ParseIP(y.addr), b)
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("not removed on z")
	}
	if acks := waitSent(zpc, y.addr, "ack"); len(acks) == 0 || acks[len(acks)-1].ID != ws[0].ID {
		t.Fatalf("no ack: %+v", acks)
	}
	// a late copy and a relay don't bring it back
	z.onGroupMsg(y.addr, cp, "")
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: vis.ID, By: y.addr, Text: cp.Text, TS: cp.TS, Sg: cp.Sg, PK: cp.PK})
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("resurrected")
	}
	// z passes the tombstone on when someone catches up
	z.onGroupCtl(tMem, wire{T: "ghq", G: gid, TS: 1})
	dws := waitSentT(zpc, tMem, "del", 5*time.Second)
	if len(dws) == 0 || dws[0].Sg != ws[0].Sg || dws[0].Ref != vis.ID {
		t.Fatalf("catch-up without tombstone: %+v", dws)
	}
}

func TestGroupUnsendBeforePostAndRelayed(t *testing.T) {
	gid, y, z, _, _, ypc, _, _ := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "early", "")
	cp := firstCopy(t, ypc, z.addr)
	tPost(t, delMux(y), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	del := waitSent(ypc, z.addr, "del")[0]
	// relayed by another member before the post arrives
	if !z.onGroupDel(tMem, del) {
		t.Fatal("not acked")
	}
	z.onGroupMsg(y.addr, cp, "")
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: vis.ID, By: y.addr, Text: cp.Text, TS: cp.TS})
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("post arrived after its unsend and stayed")
	}
}

func TestGroupUnsendForgedRejected(t *testing.T) {
	gid, y, z, _, d, ypc, _, _ := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "keep me", "")
	cp := firstCopy(t, ypc, z.addr)
	z.onGroupMsg(y.addr, cp, "")
	evil, _ := tKeyApp(t)
	sg, pk := evil.signDel(gid, vis.ID, y.addr)
	// (1) someone else's key claiming to be y, relayed
	z.onGroupDel(tMem, wire{T: "del", ID: "c1", G: gid, Ref: vis.ID, By: y.addr, PK: pk, Sg: sg})
	// (2) unsigned, relayed by a member
	z.onGroupDel(tMem, wire{T: "del", ID: "c2", G: gid, Ref: vis.ID, By: y.addr})
	// (3) unsigned, straight from a member who is not the author
	z.onGroupDel(tMem, wire{T: "del", ID: "c3", G: gid, Ref: vis.ID, By: y.addr, Dby: tMem})
	// (4) a valid signature of an ordinary member deleting y's post (no right to)
	m2, _ := tKeyApp(t)
	for _, a := range []*App{z} {
		g := a.store.Groups[gid]
		g.Members = cleanMembers(append(g.Members, m2.addr), "")
		a.store.Groups[gid] = g
	}
	sg2, pk2 := m2.signDel(gid, vis.ID, y.addr)
	z.onGroupDel(m2.addr, wire{T: "del", ID: "c4", G: gid, Ref: vis.ID, By: y.addr, Dby: m2.addr, PK: pk2, Sg: sg2})
	// (5) y's signature replayed for another post id
	ysg, ypk := y.signDel(gid, "ffff1", y.addr)
	z.onGroupDel(tMem, wire{T: "del", ID: "c5", G: gid, Ref: vis.ID, By: y.addr, PK: ypk, Sg: ysg})
	// (6) the admin deleting the host's post
	if !hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("forged unsend applied")
	}
	z.store.Messages = append(z.store.Messages, Message{ID: "aaa1", Peer: gKey(gid), From: z.store.Groups[gid].Owner, Text: "host post", TS: 1})
	asg, apk := d.signDel(gid, "aaa1", z.store.Groups[gid].Owner)
	z.onGroupDel(d.addr, wire{T: "del", ID: "c6", G: gid, Ref: "aaa1", By: z.store.Groups[gid].Owner, Dby: d.addr, PK: apk, Sg: asg})
	if !hasPost(z, gid, z.store.Groups[gid].Owner, "aaa1") {
		t.Fatal("admin deleted the host's post")
	}
	// but an unsigned unsend straight from the author works (system mode)
	z.onGroupDel(y.addr, wire{T: "del", ID: "c7", G: gid, Ref: vis.ID, By: y.addr})
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("author's direct unsigned unsend ignored")
	}
	// ... and it is not passed on (unsigned)
	z.store.mu.Lock()
	n := len(z.gdelWiresLocked(gid))
	z.store.mu.Unlock()
	if n != 0 {
		t.Fatal("unsigned tombstone relayed")
	}
}

func TestGroupModeration(t *testing.T) {
	gid, y, z, h, d, ypc, _, hpc := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "spam", "")
	cp := firstCopy(t, ypc, z.addr)
	for _, a := range []*App{z, h, d} {
		a.onGroupMsg(y.addr, cp, "")
	}
	// an ordinary member can't delete y's post for everyone
	rec := tPost(t, delMux(z), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	if rec.Code != 400 || !hasPost(z, gid, y.addr, vis.ID) {
		t.Fatalf("member moderated: %d", rec.Code)
	}
	// the host can
	rec = tPost(t, delMux(h), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	if rec.Code != 200 {
		t.Fatalf("host delete: %d %s", rec.Code, rec.Body)
	}
	del := waitSent(hpc, z.addr, "del")
	if len(del) == 0 || del[0].Dby != h.addr || del[0].By != y.addr {
		t.Fatalf("moderation packet: %+v", del)
	}
	z.onGroupDel(h.addr, del[0])
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("host deletion not applied")
	}
	// relayed by a member it still verifies (d never got it directly)
	d.onGroupDel(tMem, del[0])
	if hasPost(d, gid, y.addr, vis.ID) {
		t.Fatal("relayed host deletion not applied")
	}
	// the author's own screen and its pending copies go too
	y.onGroupDel(h.addr, waitSent(hpc, y.addr, "del")[0])
	if hasPost(y, gid, y.addr, vis.ID) {
		t.Fatal("author still sees it")
	}
	y.store.mu.Lock()
	for _, m := range y.store.Messages {
		if m.Hid && m.Of == vis.ID {
			t.Errorf("pending copy kept: %+v", m)
		}
	}
	y.store.mu.Unlock()
}

// Deleting someone's group post on this device only used to come back with
// the next catch-up (ghs relay).
func TestGroupLocalDeleteStaysDeleted(t *testing.T) {
	gid, y, z, _, _, ypc, _, _ := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "hide me", "")
	cp := firstCopy(t, ypc, z.addr)
	z.onGroupMsg(y.addr, cp, "")
	rec := tPost(t, delMux(z), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`"}`)
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: vis.ID, By: y.addr, Text: cp.Text, TS: cp.TS, Sg: cp.Sg, PK: cp.PK})
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("locally deleted post came back via catch-up")
	}
}

// A v0.19.3 node takes the group del for a 1:1 unsend of an unknown message:
// it acks (the sender stops) and keeps the post.
func TestGroupDelOldPeerAcks(t *testing.T) {
	gid, y, z, _, _, ypc, _, _ := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "x", "")
	cp := firstCopy(t, ypc, z.addr)
	z.onGroupMsg(y.addr, cp, "")
	tPost(t, delMux(y), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	del := waitSent(ypc, z.addr, "del")[0]
	if !z.onCtl(y.addr, del) { // the old code path
		t.Fatal("old path wouldn't ack")
	}
	if !hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("old path removed a group post")
	}
	if wireLen(del) > dgramMax {
		t.Fatalf("del too big: %d", wireLen(del))
	}
}
