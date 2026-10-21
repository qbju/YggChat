package main

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func wpkt(t *testing.T, w wire) []byte {
	b, err := json.Marshal(w)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func msgsWith(a *App, peer string) []Message {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []Message
	for _, m := range a.store.Messages {
		if m.Peer == peer && m.Sys == "" {
			out = append(out, m)
		}
	}
	return out
}

func contactApp(t *testing.T) (*App, *fakePC) {
	a, pc := tAppPC(t, tMe)
	a.sys = true
	a.store.Contacts[tHost] = Contact{Addr: tHost, Nick: "h", Shared: true, HasProf: true}
	return a, pc
}

// An unsend that overtakes its message (Nostr box fetched later, a retry
// after a lost ack) used to be acked and forgotten: the message then showed
// up anyway, although its sender had unsent it.
func TestUnsendBeforeMessage(t *testing.T) {
	a, pc := contactApp(t)
	from := net.ParseIP(tHost)
	a.handlePacket(from, wpkt(t, wire{T: "del", ID: "c1", Ref: "m1"}))
	a.handlePacket(from, wpkt(t, wire{T: "msg", ID: "m1", Text: "secret", TS: time.Now().Unix()}))
	if ms := msgsWith(a, tHost); len(ms) != 0 {
		t.Fatalf("unsent message appeared: %+v", ms)
	}
	if len(pc.sent(tHost, "ack")) != 2 {
		t.Fatalf("both must be acked so the sender stops: %+v", pc.sent(tHost, "ack"))
	}
	// Survives a restart (lock/unlock reload).
	a.store.mu.Lock()
	b, _ := json.Marshal(a.store)
	a.store.mu.Unlock()
	a.store.mu.Lock()
	_ = a.store.loadData(b)
	a.store.mu.Unlock()
	a.seen = map[string]bool{}
	a.handlePacket(from, wpkt(t, wire{T: "msg", ID: "m1", Text: "secret", TS: time.Now().Unix()}))
	if ms := msgsWith(a, tHost); len(ms) != 0 {
		t.Fatalf("unsent message appeared after reload: %+v", ms)
	}
}

func TestEditBeforeMessage(t *testing.T) {
	a, _ := contactApp(t)
	from := net.ParseIP(tHost)
	a.handlePacket(from, wpkt(t, wire{T: "edit", ID: "c1", Ref: "m1", Text: "fixed"}))
	a.handlePacket(from, wpkt(t, wire{T: "msg", ID: "m1", Text: "tpyo", TS: time.Now().Unix()}))
	ms := msgsWith(a, tHost)
	if len(ms) != 1 || ms[0].Text != "fixed" || !ms[0].Edited {
		t.Fatalf("early edit lost: %+v", ms)
	}
}

// Only the original sender can edit/unsend: another peer's ctl for the same id does nothing.
func TestEditOnlyBySender(t *testing.T) {
	a, _ := contactApp(t)
	a.handlePacket(net.ParseIP(tHost), wpkt(t, wire{T: "msg", ID: "m1", Text: "orig", TS: time.Now().Unix()}))
	a.handlePacket(net.ParseIP(tMem), wpkt(t, wire{T: "edit", ID: "c1", Ref: "m1", Text: "evil"}))
	a.handlePacket(net.ParseIP(tMem), wpkt(t, wire{T: "del", ID: "c2", Ref: "m1"}))
	ms := msgsWith(a, tHost)
	if len(ms) != 1 || ms[0].Text != "orig" {
		t.Fatalf("stranger changed a message: %+v", ms)
	}
	a.handlePacket(net.ParseIP(tHost), wpkt(t, wire{T: "msg", ID: "m2", Text: "x", TS: time.Now().Unix()}))
	if len(msgsWith(a, tHost)) != 2 {
		t.Fatal("a stranger's del must not tombstone the sender's ids")
	}
}

// Deleting a received message (or letting it expire) and then receiving a
// late copy (relay that ignored NIP-09, retry) must not bring it back.
func TestDeletedStaysDeleted(t *testing.T) {
	a, _ := contactApp(t)
	mux := http.NewServeMux()
	a.msgEditHandlers(mux)
	from := net.ParseIP(tHost)
	m := wire{T: "msg", ID: "m1", Text: "hi", TS: time.Now().Unix()}
	a.handlePacket(from, wpkt(t, m))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/message/delete", bytes.NewBufferString(`{"Peer":"`+tHost+`","ID":"m1"}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	a.seen = map[string]bool{} // restart
	a.handlePacket(from, wpkt(t, m))
	if len(msgsWith(a, tHost)) != 0 {
		t.Fatal("deleted message came back")
	}
}

// Unsending a message still "queued" (it may have arrived; only the ack was
// lost) must tell the peer.
func TestUnsendQueuedSendsDel(t *testing.T) {
	a, _ := contactApp(t)
	a.store.Messages = append(a.store.Messages, Message{ID: "m9", Peer: tHost, From: tMe, Text: "x", Out: true, Status: "queued", TS: time.Now().Unix()})
	mux := http.NewServeMux()
	a.msgEditHandlers(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("POST", "/api/message/delete", bytes.NewBufferString(`{"Peer":"`+tHost+`","ID":"m9","Everyone":true}`)))
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	a.store.mu.Lock()
	n := len(a.store.Ctl)
	a.store.mu.Unlock()
	if n != 1 {
		t.Fatalf("no unsend queued for a queued message (ctl=%d)", n)
	}
}

// A message deleted while it waits must not be parked on Nostr afterwards.
func TestNoParkAfterDelete(t *testing.T) {
	a, _ := contactApp(t)
	a.sys = false
	m := Message{ID: "m5", Peer: tHost, From: tMe, Text: "x", Out: true, Status: "queued"}
	a.nostrPark(m) // not in the store: must return before touching relays/keys (would panic without cfg)
}
