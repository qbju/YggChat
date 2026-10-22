package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The CLI (and the desktop "already running" browser launch, which goes
// through apiUp) sent the key to whatever held the UI port. Now it checks
// /api/proof first; an impostor never sees the key.
func TestCLIChecksProofBeforeKey(t *testing.T) {
	uiKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	defer func() { uiKey = "" }()
	real := httptest.NewServer(keyGuard("127.0.0.1:7780", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })))
	defer real.Close()
	var leaked atomic.Bool
	fake := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get(uiKeyHeader)+r.URL.RawQuery, uiKey) {
			leaked.Store(true)
		}
		if r.URL.Path == "/api/proof" {
			w.Write([]byte(`{"mac":"` + strings.Repeat("ab", 32) + `"}`))
			return
		}
		w.WriteHeader(200) // claims to be up
	}))
	defer fake.Close()
	host := func(s *httptest.Server) string { return strings.TrimPrefix(s.URL, "http://") }
	proofMu.Lock()
	proofAt = map[string]time.Time{}
	proofMu.Unlock()

	if st := apiStatus(host(real)); st != 200 {
		t.Fatalf("real node: %d", st)
	}
	if st := apiStatus(host(fake)); st != apiNoProof {
		t.Fatalf("impostor: %d", st)
	}
	if _, err := cliHTTP.Get(fake.URL + "/api/state"); err == nil || !strings.Contains(err.Error(), "prove") {
		t.Fatalf("CLI request to impostor: %v", err)
	}
	if r, err := cliHTTP.Get(real.URL + "/api/state"); err != nil || r.StatusCode != 200 {
		t.Fatalf("CLI request to node: %v", err)
	}
	if leaked.Load() {
		t.Fatal("key sent to the impostor")
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if st := apiStatus(dead); st != 0 {
		t.Fatalf("nothing listening: %d", st)
	}
}

// Captured stdout (Android node.log, a service journal) must not get the key.
func TestShownUIURLKeyOnlyOnTerminal(t *testing.T) {
	uiKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	defer func() { uiKey = "" }()
	if s := shownUIURL("127.0.0.1:7780", "/d", false); strings.Contains(s, uiKey) || !strings.Contains(s, filepath.Join("/d", "ui-key")) {
		t.Fatal(s)
	}
	if s := shownUIURL("127.0.0.1:7780", "/d", true); !strings.Contains(s, "?k="+uiKey) {
		t.Fatal(s)
	}
}

// One peer could hold all 8 finger slots.
func TestFingerSlotsPerPeer(t *testing.T) {
	f := &fingerState{sem: make(chan struct{}, 8)}
	tcp := func(s string) net.Addr { return &net.TCPAddr{IP: net.ParseIP(s), Port: 1} }
	a := fingerPeerKey(tcp("201::1"))
	if !f.fingerTake(a) || !f.fingerTake(a) || f.fingerTake(a) {
		t.Fatal("per-peer cap")
	}
	// a 300::/64 subnet is one node
	if fingerPeerKey(tcp("301:1:2:3::1")) != fingerPeerKey(tcp("301:1:2:3::2")) || fingerPeerKey(tcp("301:1:2:3::1")) == fingerPeerKey(tcp("301:1:2:4::1")) {
		t.Fatal("subnet grouping")
	}
	n := 2
	for i := 0; i < 20; i++ {
		if f.fingerTake(fingerPeerKey(tcp(fmt.Sprintf("202::%x", i+1)))) {
			n++
		}
	}
	if n != 8 {
		t.Fatalf("slots in use: %d", n)
	}
	f.fingerDone(a)
	if !f.fingerTake(a) {
		t.Fatal("slot not given back")
	}
}

// Group placeholders (posts into unknown groups) and link joins from
// non-members share the message-request limit.
func TestGroupRequestsLimited(t *testing.T) {
	a := tApp(t, tMe)
	stored := 0
	for i := 0; i < 40; i++ {
		gid := fmt.Sprintf("4444%012x", i)
		if a.onGroupMsg(tMem, wire{T: "msg", ID: fmt.Sprintf("s%d", i), G: gid, Text: "spam", TS: time.Now().Unix()}, "") {
			stored++
		}
	}
	if stored != reqPerMin {
		t.Fatalf("placeholders from one stranger in a minute: %d, want %d", stored, reqPerMin)
	}
	// an accepted contact isn't limited
	a.store.Contacts[tHost] = Contact{Addr: tHost, Shared: true}
	for i := 0; i < 5; i++ {
		if !a.onGroupMsg(tHost, wire{T: "msg", ID: fmt.Sprintf("c%d", i), G: fmt.Sprintf("5555%012x", i), Text: "x", TS: time.Now().Unix()}, "") {
			t.Fatal("contact limited")
		}
	}

	h, pc := tAppPC(t, tHost)
	gid := "6666000066660000"
	h.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers([]string{tMe}, tHost), Owner: tHost, V: 1, Tok: "0011223344556677", Set: GroupSet{Approve: true}}
	for i := 0; i < 40; i++ {
		h.onGroupCtl(tMem, wire{T: "gj", G: gid, Tok: "0011223344556677", Nick: "j"})
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(pc.sent(tMem, "gjn")); n != reqPerMin {
		t.Fatalf("join requests answered: %d, want %d", n, reqPerMin)
	}
}

// Group posts with a hostile clock were stored as sent (0, or far future).
func TestGroupPostTimeClamped(t *testing.T) {
	gid := "7171000071710000"
	y, _ := tKeyApp(t)
	a := tApp(t, tMe)
	ms := cleanMembers([]string{y.addr, tMe}, tHost)
	a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, V: 2}
	now := time.Now().Unix()
	fut := now + 86400*365
	w := wire{T: "msg", ID: "fu1", G: gid, Text: "from the future", TS: fut, PK: y.myPK()}
	w.Sg = y.signPost(gid, "fu1", fut, w.Text)
	if !a.onGroupMsg(y.addr, w, "") {
		t.Fatal("refused")
	}
	if !a.onGroupMsg(y.addr, wire{T: "msg", ID: "ze1", G: gid, Text: "zero", TS: 0}, "") {
		t.Fatal("refused")
	}
	for _, m := range groupMsgsOf(a, gid) {
		if m.TS <= 0 || m.TS > time.Now().Unix()+600 {
			t.Fatalf("time kept: %+v", m)
		}
		if m.ID == "fu1" && m.Sg != "" {
			t.Fatal("signature kept with a changed time (relays would fail it)")
		}
	}
}

// Disk full: an incoming message whose save failed was acked anyway.
func TestNoAckWhenSaveFails(t *testing.T) {
	a, pc := contactApp(t)
	good := a.store.path
	a.store.path = filepath.Join(t.TempDir(), "missing", "c.json")
	from := net.ParseIP(tHost)
	pkt := wpkt(t, wire{T: "msg", ID: "df1", Text: "hello", TS: time.Now().Unix()})
	a.handlePacket(from, pkt)
	time.Sleep(100 * time.Millisecond)
	if len(pc.sent(tHost, "ack")) != 0 {
		t.Fatal("acked although not saved")
	}
	if len(msgsWith(a, tHost)) != 0 {
		t.Fatal("unsaved message kept")
	}
	// a 1:1 request from a stranger doesn't leave a contact behind either
	a.handlePacket(net.ParseIP(tMem), wpkt(t, wire{T: "msg", ID: "df2", Text: "hi", TS: time.Now().Unix()}))
	a.store.mu.Lock()
	_, ok := a.store.Contacts[tMem]
	a.store.mu.Unlock()
	if ok {
		t.Fatal("contact from an unsaved request kept")
	}
	a.store.path = good
	a.handlePacket(from, pkt) // the sender's retry
	time.Sleep(100 * time.Millisecond)
	if len(pc.sent(tHost, "ack")) != 1 || len(msgsWith(a, tHost)) != 1 {
		t.Fatalf("retry: acks %d, msgs %d", len(pc.sent(tHost, "ack")), len(msgsWith(a, tHost)))
	}
}
