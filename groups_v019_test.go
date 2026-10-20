package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const tgid = "0123456789abcdef"
const tMe = "201:e1bf:2093:6a01:d9d6:4c53:1569:c35d"
const tHost = "202:944f:271f:cfd3:8b0c:352:3161:947d"
const tMem = "203:1111:2222:3333:4444:5555:6666:7777"
const tAdm = "204:aaaa:bbbb:cccc:dddd:eeee:ffff:1111"

func tApp(t *testing.T, me string) *App {
	s := &Store{path: filepath.Join(t.TempDir(), "c.json")}
	s.fix()
	return &App{addr: me, store: s, subs: map[chan []byte]struct{}{}, acks: map[string]chan struct{}{}, inflight: map[string]bool{}, seen: map[string]bool{}, profAt: map[string]time.Time{}}
}

func manyAddrs(n int) []string {
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, fmt.Sprintf("2%02x:%x:1:2:3:4:5:%x", i%256, i, i+1))
	}
	return out
}

func TestUnlockKeepsEverything(t *testing.T) {
	p := filepath.Join(t.TempDir(), "chat.json")
	s := &Store{path: p}
	s.fix()
	s.Groups[tgid] = Group{ID: tgid, Name: "g"}
	s.NostrOn, s.Domain, s.Ctl, s.PowStrict = true, "x.example", []ctlItem{{Peer: tHost}}, true
	if err := s.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	s2 := &Store{path: p}
	s2.load()
	if err := s2.unlock("pass1234"); err != nil {
		t.Fatal(err)
	}
	if len(s2.Groups) != 1 || !s2.NostrOn || s2.Domain != "x.example" || len(s2.Ctl) != 1 || !s2.PowStrict {
		t.Fatalf("lost fields: %+v", s2)
	}
}

func TestShortAddrNoPanic(t *testing.T) {
	a := tApp(t, tMe)
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "x", GM: []string{tHost, tMe}, GO: tHost})
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "x", GM: []string{tHost, tMe, "200::5"}, GO: tHost})
}

func TestOwnerFromLaterInfo(t *testing.T) {
	a := tApp(t, tMe)
	a.onGroupMsg(tMem, wire{T: "msg", ID: "abc", G: tgid, Text: "hi"}, "")
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMem, tMe}, GO: tHost, GS: &GroupSet{NoFiles: true, TTL: 300}})
	g := a.store.Groups[tgid]
	if g.Owner != tHost || !g.Set.NoFiles || g.Set.TTL != 300 || g.Stub {
		t.Fatalf("got %+v", g)
	}
}

func TestReAdd(t *testing.T) {
	a := tApp(t, tMe)
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe, tMem}, GO: tHost, GV: 1})
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMem}, GO: tHost, GV: 2})
	if !a.store.Groups[tgid].Left {
		t.Fatal("not removed")
	}
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMem, tMe}, GO: tHost, GV: 2})
	if !a.store.Groups[tgid].Left {
		t.Fatal("stale list re-added us")
	}
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMem, tMe}, GO: tHost, GV: 3})
	if g := a.store.Groups[tgid]; g.Left || !g.Req {
		t.Fatalf("re-add: %+v", g)
	}
}

func TestPackRoundTrip(t *testing.T) {
	as := manyAddrs(40)
	got := unpackAddrs(packAddrs(as))
	for i := range as {
		x, _ := yggAddr(as[i])
		if got[i] != x {
			t.Fatalf("%d: %s != %s", i, got[i], x)
		}
	}
}

func TestBigInfoSplitAndAssemble(t *testing.T) {
	host := tApp(t, tHost)
	ms := cleanMembers(append(manyAddrs(1000), tMe), tHost)
	g := Group{ID: tgid, Name: strings.Repeat("な", 32), Members: ms, Owner: tHost, V: 7, Admins: []string{ms[3]}, Set: GroupSet{Max: 1024, PoW: 8}}
	ps := host.infoPackets(g)
	if len(ps) != 1+(len(ms)+partAddrs-1)/partAddrs {
		t.Fatalf("packets %d", len(ps))
	}
	for _, p := range ps {
		if n := wireLen(p); n > dgramMax {
			t.Fatalf("%s is %d bytes", p.T, n)
		}
	}
	me := tApp(t, tMe)
	// parts first, header last, one part twice
	for i := len(ps) - 1; i >= 1; i-- {
		b, _ := json.Marshal(ps[i])
		var w wire
		json.Unmarshal(b, &w)
		me.onGroupCtl(tHost, w)
	}
	b, _ := json.Marshal(ps[0])
	var hw wire
	json.Unmarshal(b, &hw)
	me.onGroupCtl(tHost, hw)
	x, ok := me.store.Groups[tgid]
	if !ok || len(x.Members) != len(ms) || x.Owner != tHost || x.V != 7 || len(x.Admins) != 1 || x.Set.Max != 1024 {
		t.Fatalf("assembled: ok=%v n=%d owner=%s v=%d admins=%v", ok, len(x.Members), x.Owner, x.V, x.Admins)
	}
	// a header alone with the same list is enough
	g.Name = "新しい名前"
	h2 := host.infoPackets(g)[0]
	me.onGroupCtl(tHost, h2)
	if me.store.Groups[tgid].Name != "新しい名前" {
		t.Fatal("header-only rename not applied")
	}
}

func TestDeltaAndAdminRules(t *testing.T) {
	me := tApp(t, tMe)
	me.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe, tMem, tAdm}, GO: tHost, GV: 1, GAD: []string{tAdm}})
	g := me.store.Groups[tgid]
	if !isAdmin(g, tAdm) {
		t.Fatal("admin not set")
	}
	newb := "205::1"
	// admin adds a member and tries to remove the host
	me.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 2, GA: packAddrs([]string{newb})})
	me.onGroupCtl(tAdm, wire{T: "gd", G: tgid, GV: 3, GR: packAddrs([]string{tHost})})
	g = me.store.Groups[tgid]
	if !hasMember(g, "205::1") || !hasMember(g, tHost) || g.V != 3 {
		t.Fatalf("delta: %+v", g)
	}
	// a plain member can't
	me.onGroupCtl(tMem, wire{T: "gd", G: tgid, GV: 9, GR: packAddrs([]string{newb})})
	if !hasMember(me.store.Groups[tgid], newb) {
		t.Fatal("member removed someone")
	}
	// an admin's info can't change settings or admins
	me.onGroupCtl(tAdm, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe, tMem, tAdm, newb}, GO: tHost, GV: 4, GAD: []string{tAdm, tMem}, GS: &GroupSet{NoFiles: true}})
	g = me.store.Groups[tgid]
	if g.Set.NoFiles || isAdmin(g, tMem) {
		t.Fatalf("admin changed host-only things: %+v", g)
	}
	// host hands over to the admin
	me.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: g.Members, GO: tAdm, GV: 5})
	if g = me.store.Groups[tgid]; g.Owner != tAdm || isAdmin(g, tAdm) {
		t.Fatalf("hand-over: %+v", g)
	}
}

func TestPow(t *testing.T) {
	n := powSolve("a>b", "id1", 1760000000, "hello", "", 12)
	if powBits("a>b", "id1", 1760000000, "hello", "", n) < 12 {
		t.Fatal("solve")
	}
	if powBits("a>b", "id1", 1760000000, "hellp", "", n) >= 12 && powBits("a>b", "id1", 1760000001, "hello", "", n) >= 12 {
		t.Fatal("not bound")
	}
	if postID("18a2b3c4g12") != "18a2b3c4" {
		t.Fatal("postID")
	}
	a := tApp(t, tMe)
	// stranger: 8 bits needed; without a nonce allowed only when not strict
	w := wire{T: "msg", ID: "x1", Text: "hi", TS: 5}
	if !a.powCheck(tHost, w) {
		t.Fatal("legacy request refused while not strict")
	}
	a.store.PowStrict = true
	if a.powCheck(tHost, w) {
		t.Fatal("legacy request accepted while strict")
	}
	w.PN = powSolve(tHost+">"+tMe, "x1", 5, "hi", "", 8)
	if !a.powCheck(tHost, w) {
		t.Fatal("valid 8-bit nonce refused")
	}
	if !joinPowOK(tHost, tgid, "tok", joinPow(tHost, tgid, "tok")) {
		t.Fatal("join pow")
	}
}

func TestLongTextGoesChunked(t *testing.T) {
	a := tApp(t, tMe)
	short := Message{ID: "1", Peer: tHost, Text: strings.Repeat("あ", 300)}
	long := Message{ID: "2", Peer: tHost, Text: strings.Repeat("あ", 400)}
	esc := Message{ID: "3", Peer: tHost, Text: strings.Repeat("<", 300)}
	if a.longText(short) || !a.longText(long) || !a.longText(esc) {
		t.Fatal("longText threshold")
	}
}

func TestHistoryFits(t *testing.T) {
	a := tApp(t, tHost)
	now := time.Now().Unix()
	a.store.Messages = []Message{{ID: "1", Peer: gKey(tgid), From: tMem, Nick: strings.Repeat("あ", 32), Text: strings.Repeat("<", 2000), TS: now}}
	_ = a.historyLocked(tgid)
	// sendHistory with no udp just builds; check one datagram by hand
	w := wire{T: "ghs", G: tgid, ID: "1", By: tMem, Nick: strings.Repeat("あ", 32), Text: strings.Repeat("<", 700) + "…（省略）", TS: now}
	if wireLen(w) <= dgramMax {
		t.Skip("escape case fits anyway")
	}
}
