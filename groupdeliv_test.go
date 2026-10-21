package main

import (
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"
)

// fakePC records what a node sends.
type fakePC struct {
	mu   sync.Mutex
	pkts []sentPkt
}

type sentPkt struct {
	to string
	w  wire
}

func (f *fakePC) ReadFrom(b []byte) (int, net.Addr, error) { select {} }
func (f *fakePC) WriteTo(b []byte, to net.Addr) (int, error) {
	var w wire
	_ = json.Unmarshal(b, &w)
	f.mu.Lock()
	f.pkts = append(f.pkts, sentPkt{to.(*net.UDPAddr).IP.String(), w})
	f.mu.Unlock()
	return len(b), nil
}
func (f *fakePC) Close() error                       { return nil }
func (f *fakePC) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (f *fakePC) SetDeadline(t time.Time) error      { return nil }
func (f *fakePC) SetReadDeadline(t time.Time) error  { return nil }
func (f *fakePC) SetWriteDeadline(t time.Time) error { return nil }

func (f *fakePC) sent(to, typ string) []wire {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []wire
	for _, p := range f.pkts {
		if p.to == to && p.w.T == typ {
			out = append(out, p.w)
		}
	}
	return out
}

func tAppPC(t *testing.T, me string) (*App, *fakePC) {
	a := tApp(t, tMe)
	a.addr = me
	pc := &fakePC{}
	a.udp = pc
	return a, pc
}

func groupMsgs(a *App) []Message {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []Message
	for _, m := range a.store.Messages {
		if m.Peer == gKey(tgid) && m.Sys == "" && !m.Hid {
			out = append(out, m)
		}
	}
	return out
}

// Bug 1: a post arrives as our member copy ("<id>g<i>"), relayed by another
// member with its own copy id or the bare id, and as host history: one message.
func TestGroupPostDedupeAcrossCopyIDs(t *testing.T) {
	a, _ := tAppPC(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem, tAdm}, tMe), Owner: tHost, Admins: []string{tAdm}}
	if !a.onGroupMsg(tHost, wire{T: "msg", ID: "18a2b3c4g1", G: tgid, Text: "hi", TS: 100}, "") {
		t.Fatal("not acked")
	}
	a.onGroupCtl(tMem, wire{T: "ghs", G: tgid, ID: "18a2b3c4g2", By: tHost, Text: "hi", TS: 100}) // old relayer: its copy id
	a.onGroupCtl(tAdm, wire{T: "ghs", G: tgid, ID: "18a2b3c4", By: tHost, Text: "hi", TS: 100})   // new relayer: post id
	if !a.onGroupMsg(tHost, wire{T: "msg", ID: "18a2b3c4g1", G: tgid, Text: "hi", TS: 100}, "") {
		t.Fatal("retry not acked")
	}
	ms := groupMsgs(a)
	if len(ms) != 1 || ms[0].ID != "18a2b3c4" {
		t.Fatalf("want one message stored under the post id, got %+v", ms)
	}
	// history first, then the late direct copy: still one
	a.onGroupCtl(tMem, wire{T: "ghs", G: tgid, ID: "19ffg0", By: tAdm, Text: "x", TS: 101})
	a.onGroupMsg(tAdm, wire{T: "msg", ID: "19ffg2", G: tgid, Text: "x", TS: 101}, "")
	if n := len(groupMsgs(a)); n != 2 {
		t.Fatalf("history+copy: %d messages", n)
	}
	// insert-time guard (two receive paths racing)
	a.addMessage(Message{ID: "19ffg7", Peer: gKey(tgid), From: tAdm, Text: "x", TS: 101})
	if n := len(groupMsgs(a)); n != 2 {
		t.Fatalf("addMessage stored a duplicate: %d", n)
	}
	// duplicates already in an older store collapse on load
	s := &Store{Messages: []Message{
		{ID: "aag0", Peer: gKey(tgid), From: tHost, Text: "a"},
		{ID: "aag2", Peer: gKey(tgid), From: tHost, Text: "a", Hist: true},
		{ID: "aa", Peer: gKey(tgid), From: tHost, Text: "a"},
		{ID: "aa", Peer: gKey(tgid), From: tMem, Text: "same id, other sender"},
		{ID: "bb", Peer: gKey(tgid), From: tMe, Out: true, Text: "ours"},
		{ID: "bbg1", Peer: tHost, From: tMe, Out: true, Hid: true, Grp: tgid, Of: "bb", Status: "queued"},
	}}
	s.fix()
	if len(s.Messages) != 4 {
		t.Fatalf("collapse: %+v", s.Messages)
	}
	// relays send the post id
	if postID("18a2b3c4g12") != "18a2b3c4" || !samePost("ab", "abg3") || samePost("ab", "abcg3") {
		t.Fatal("postID")
	}
}

// Bug 2: native nodes answer the catch-up request "ghq" (with post ids).
func TestNativeAnswersGhq(t *testing.T) {
	if !isGroupCtl("ghq") || !isGroupCtl("gq") || isGroupCtl("msg") {
		t.Fatal("ghq not dispatched to onGroupCtl")
	}
	a, pc := tAppPC(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost, Set: GroupSet{Hist: true}}
	a.onGroupMsg(tHost, wire{T: "msg", ID: "1234g2", G: tgid, Text: "missed", TS: time.Now().Unix()}, "")
	a.store.mu.Lock()
	a.store.Messages = append(a.store.Messages, Message{ID: "5678g0", Peer: gKey(tgid), From: tHost, Text: "old copy id", TS: time.Now().Unix()})
	a.store.mu.Unlock()
	a.onGroupCtl(tMem, wire{T: "ghq", G: tgid, TS: 1})
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) && len(pc.sent(tMem, "ghs")) < 2 {
		time.Sleep(100 * time.Millisecond)
	}
	got := pc.sent(tMem, "ghs")
	if len(got) < 2 {
		t.Fatalf("no history sent: %+v", got)
	}
	for _, w := range got {
		if w.ID != postID(w.ID) || w.By != tHost {
			t.Fatalf("history carries a copy id: %+v", w)
		}
	}
}

// Bug 3: a post made before its sender left still counts; later ones are acked
// and dropped; our own copies for a group we left are failed, not retried.
func TestLeaveOrphanedPosts(t *testing.T) {
	a, _ := tAppPC(t, tMe)
	a.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost}
	leftAt := time.Now().Unix() - 30
	a.onGroupCtl(tMem, wire{T: "gl", G: tgid, TS: leftAt})
	if g := a.store.Groups[tgid]; hasMember(g, tMem) || g.GoneAt[tMem] != leftAt {
		t.Fatalf("gl: %+v", g)
	}
	if !a.onGroupMsg(tMem, wire{T: "msg", ID: "aaaag2", G: tgid, Text: "before", TS: leftAt - 1}, "") {
		t.Fatal("late pre-leave post not acked")
	}
	if !a.onGroupMsg(tMem, wire{T: "msg", ID: "bbbbg2", G: tgid, Text: "after", TS: leftAt + 5}, "") {
		t.Fatal("post-leave post not acked (sender would retry forever)")
	}
	ms := groupMsgs(a)
	if len(ms) != 1 || ms[0].Text != "before" {
		t.Fatalf("want only the pre-leave post, got %+v", ms)
	}
	// a stranger (never a member) is still refused
	if a.onGroupMsg(tNew, wire{T: "msg", ID: "ccccg1", G: tgid, Text: "x", TS: leftAt - 1}, "") {
		t.Fatal("stranger acked")
	}
	// relayed history of a pre-leave post is taken too
	a.onGroupCtl(tHost, wire{T: "ghs", G: tgid, ID: "dddd", By: tMem, Text: "relayed", TS: leftAt - 2})
	if n := len(groupMsgs(a)); n != 2 {
		t.Fatalf("relayed pre-leave post: %d", n)
	}
	a.onGroupCtl(tHost, wire{T: "ghs", G: tgid, ID: "eeee", By: tMem, Text: "relayed late", TS: leftAt + 10})
	if n := len(groupMsgs(a)); n != 2 {
		t.Fatalf("relayed post-leave post taken: %d", n)
	}

	// sender side: we left with copies pending
	b, _ := tAppPC(t, tMe)
	b.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost, Left: true, Quit: true}
	b.store.Messages = []Message{
		{ID: "p1", Peer: gKey(tgid), From: tMe, Out: true, Text: "x", Status: "sending", Tot: 2},
		{ID: "p1g0", Peer: tHost, From: tMe, Out: true, Hid: true, Grp: tgid, Of: "p1", Text: "x", Status: "queued"},
		{ID: "p1g2", Peer: tMem, From: tMe, Out: true, Hid: true, Grp: tgid, Of: "p1", Text: "x", Status: "stored"},
		{ID: "d1", Peer: tHost, From: tMe, Out: true, Text: "direct", Status: "queued"},
	}
	if q := b.queued(""); len(q) != 1 || q[0].ID != "d1" {
		t.Fatalf("left-group copies still queued: %+v", q)
	}
	if !b.deliverN(b.store.Messages[1], 3) {
		t.Fatal("deliverN of a left copy should give up at once")
	}
	b.failLeftCopies(tgid)
	b.store.mu.Lock()
	for _, m := range b.store.Messages {
		if m.ID == "p1" && m.Status != "failed" {
			t.Fatalf("post status %q, want failed", m.Status)
		}
		if m.Hid && m.Status != "failed" {
			t.Fatalf("copy %s status %q", m.ID, m.Status)
		}
	}
	b.store.mu.Unlock()
	b.setStatus(tMem, "p1g2", "stored") // a retry finishing after we gave up
	b.store.mu.Lock()
	if b.store.Messages[2].Status != "failed" {
		t.Fatalf("late retry revived a failed copy: %q", b.store.Messages[2].Status)
	}
	b.store.requeueAfterRestart()
	for _, m := range b.store.Messages {
		if (m.Grp == tgid || m.Peer == gKey(tgid)) && m.Status != "failed" {
			t.Fatalf("restart revived %s: %q", m.ID, m.Status)
		}
		if m.ID == "d1" && m.Status != "queued" {
			t.Fatal("direct message touched")
		}
	}
	b.store.mu.Unlock()
	if _, err := b.retryGroup(tgid, "p1"); err == nil {
		t.Fatal("retry allowed after leaving")
	}
	// a delivered-to-someone post stays "sent"
	c, _ := tAppPC(t, tMe)
	c.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: []string{tMe}, Left: true, Quit: true}
	c.store.Messages = []Message{
		{ID: "p2", Peer: gKey(tgid), From: tMe, Out: true, Text: "x", Status: "sent", Dlv: 1},
		{ID: "p2g2", Peer: tMem, From: tMe, Out: true, Hid: true, Grp: tgid, Of: "p2", Text: "x", Status: "queued"},
	}
	c.failLeftCopies(tgid)
	if s := c.store.Messages[0].Status; s != "sent" {
		t.Fatalf("partly delivered post shows %q", s)
	}
}

// Bug 4: a member removed while away learns it from the host when it posts or
// asks; the other members ack and drop its later posts.
func TestRemovedMemberIsTold(t *testing.T) {
	h, pc := tAppPC(t, tHost)
	h.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tMe, tMem}, tHost), Owner: tHost, V: 3}
	// the host removes tMem (as /api/group/update does)
	h.store.mu.Lock()
	g := h.store.Groups[tgid]
	before := g.Members
	g.Members = cleanMembers([]string{tMe}, tHost)
	g.V++
	trackGone(&g, before)
	noteGone(&g, before, 0)
	h.store.Groups[tgid] = g
	h.store.mu.Unlock()
	if !h.onGroupMsg(tMem, wire{T: "msg", ID: "eeeeg0", G: tgid, Text: "into the void", TS: time.Now().Unix() + 2}, "") {
		t.Fatal("removed member's post not acked")
	}
	if len(groupMsgs(h)) != 0 {
		t.Fatal("post after removal stored")
	}
	gi := pc.sent(tMem, "gi")
	if len(gi) == 0 || hasMember(Group{Members: gi[0].GM}, tMem) {
		t.Fatalf("removed member not told: %+v", gi)
	}
	// rate limited
	h.onGroupMsg(tMem, wire{T: "msg", ID: "ffffg0", G: tgid, Text: "again", TS: time.Now().Unix() + 3}, "")
	if n := len(pc.sent(tMem, "gi")); n != len(gi) {
		t.Fatalf("notice not rate limited: %d", n)
	}
	// its gq / ghq get the answer too (after the limit)
	goneNoticeMu.Lock()
	delete(goneNoticeAt, tgid+"|"+tMem)
	goneNoticeMu.Unlock()
	h.onGroupCtl(tMem, wire{T: "gq", G: tgid})
	if n := len(pc.sent(tMem, "gi")); n <= len(gi) {
		t.Fatal("gq from removed member unanswered")
	}
	// a pre-v0.19.2 host store (Gone without times) still answers
	goneNoticeMu.Lock()
	goneNoticeAt = map[string]time.Time{}
	goneNoticeMu.Unlock()
	h2, pc2 := tAppPC(t, tHost)
	h2.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tMe}, tHost), Owner: tHost, Gone: []string{tMem}}
	h2.onGroupCtl(tMem, wire{T: "ghq", G: tgid, TS: 1})
	if len(pc2.sent(tMem, "gi")) == 0 {
		t.Fatal("ghq from removed member unanswered")
	}

	// the removed member: takes the host's list, shows it, fails its pending copies
	m, _ := tAppPC(t, tMem)
	m.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tMe, tHost}, tMem), Owner: tHost, V: 3}
	m.store.Messages = []Message{
		{ID: "q1", Peer: gKey(tgid), From: tMem, Out: true, Text: "x", Status: "sending", Tot: 2},
		{ID: "q1g0", Peer: tMe, From: tMem, Out: true, Hid: true, Grp: tgid, Of: "q1", Text: "x", Status: "queued"},
		{ID: "q1g1", Peer: tHost, From: tMem, Out: true, Hid: true, Grp: tgid, Of: "q1", Text: "x", Status: "queued"},
	}
	m.onGroupCtl(tHost, gi[0])
	if g := m.store.Groups[tgid]; !g.Left || g.Quit {
		t.Fatalf("removed member still in: %+v", g)
	}
	sys := false
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		m.store.mu.Lock()
		failed := m.store.Messages[0].Status == "failed"
		for _, x := range m.store.Messages {
			sys = sys || x.Sys == "グループから外されました"
		}
		m.store.mu.Unlock()
		if failed && sys {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sys || m.store.Messages[0].Status != "failed" {
		t.Fatalf("removed member UI: sys=%v status=%q", sys, m.store.Messages[0].Status)
	}

	// another member (not the host) acks and drops the removed member's later posts
	o, _ := tAppPC(t, tMe)
	o.store.Groups[tgid] = Group{ID: tgid, Name: "G", Members: cleanMembers([]string{tHost, tMem}, tMe), Owner: tHost, V: 3}
	o.onGroupCtl(tHost, wire{T: "gd", G: tgid, GV: 4, GR: packAddrs([]string{tMem})})
	if g := o.store.Groups[tgid]; hasMember(g, tMem) || g.GoneAt[tMem] == 0 {
		t.Fatalf("delta: %+v", g)
	}
	if !o.onGroupMsg(tMem, wire{T: "msg", ID: "abcdg1", G: tgid, Text: "late", TS: time.Now().Unix() + 5}, "") || len(groupMsgs(o)) != 0 {
		t.Fatal("member did not ack+drop the removed member's post")
	}
}
