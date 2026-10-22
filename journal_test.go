// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

const tStranger = "205:1:2:3:4:5:6:7"

func jMsg(t *testing.T, a *App, from, id, text string) {
	t.Helper()
	w := wire{T: "msg", ID: id, Text: text, TS: time.Now().Unix(), Nick: "s"}
	if c, ok := a.store.Contacts[from]; !ok || c.Req {
		w.PN = powSolve(powScope(from, w, a.addr), id, w.TS, text, "", powRequest)
	}
	a.handlePacket(net.ParseIP(from), wpkt(t, w))
}

func reload(t *testing.T, a *App, pass string) *Store {
	t.Helper()
	s := &Store{path: a.store.path}
	s.load()
	if s.locked {
		if err := s.unlock(pass); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s.fix()
	s.replayJournalLocked()
	return s
}

func hasMsg(s *Store, peer, id string) int {
	n := 0
	for _, m := range s.Messages {
		if m.Peer == peer && m.ID == id {
			n++
		}
	}
	return n
}

// An incoming message is appended to the journal and acked, not written by
// rewriting chat.json; a restart (even after a crash) brings it back with its
// contact entry.
func TestJournalIncomingNoRewrite(t *testing.T) {
	a, pc := contactApp(t)
	a.store.mu.Lock()
	a.store.save()
	a.store.mu.Unlock()
	before, _ := os.ReadFile(a.store.path)
	jMsg(t, a, tHost, "m1", "hello")
	jMsg(t, a, tStranger, "r1", "a request")
	after, _ := os.ReadFile(a.store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("chat.json rewritten for an incoming message")
	}
	if len(pc.sent(tHost, "ack")) != 1 || len(pc.sent(tStranger, "ack")) != 1 {
		t.Fatal("not acked")
	}
	s := reload(t, a, "")
	if hasMsg(s, tHost, "m1") != 1 || hasMsg(s, tStranger, "r1") != 1 {
		t.Fatalf("lost after restart: %+v", s.Messages)
	}
	if c, ok := s.Contacts[tStranger]; !ok || !c.Req {
		t.Fatalf("request contact lost: %+v", s.Contacts)
	}
	// The reload folded the journal in.
	if _, err := os.Stat(a.store.path + ".journal"); !os.IsNotExist(err) {
		t.Fatal("journal kept after it was folded in")
	}
	s2 := reload(t, a, "")
	if hasMsg(s2, tHost, "m1") != 1 {
		t.Fatal("duplicated or lost on second load")
	}
}

// With a passcode the journal is encrypted, and replayed on unlock.
func TestJournalEncrypted(t *testing.T) {
	a, _ := contactApp(t)
	if err := a.store.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	jMsg(t, a, tHost, "m1", "top secret words")
	j, err := os.ReadFile(a.store.path + ".journal")
	if err != nil || len(j) == 0 {
		t.Fatal("no journal")
	}
	if strings.Contains(string(j), "secret") || strings.Contains(string(j), tHost) {
		t.Fatal("journal holds plaintext")
	}
	s := reload(t, a, "pass1234")
	if hasMsg(s, tHost, "m1") != 1 {
		t.Fatal("lost after unlock")
	}
	// Locking writes everything and drops the journal.
	jMsg(t, a, tHost, "m2", "more")
	if err := a.store.lockNow(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(a.store.path + ".journal"); !os.IsNotExist(err) {
		t.Fatal("journal left after lock")
	}
	s = reload(t, a, "pass1234")
	if hasMsg(s, tHost, "m2") != 1 {
		t.Fatal("lost after lock/unlock")
	}
}

// Lines of another generation are ignored: a crash between the full save and
// dropping the journal, or an older version that rewrote chat.json without
// "jgen" (downgrade), must not bring back anything.
func TestJournalStaleGeneration(t *testing.T) {
	a, _ := contactApp(t)
	a.store.mu.Lock()
	a.store.save()
	a.store.mu.Unlock()
	jMsg(t, a, tHost, "m1", "x")
	j, _ := os.ReadFile(a.store.path + ".journal")
	// The user deletes it; a full save happens; the old journal survives (crash).
	a.store.mu.Lock()
	a.store.Messages = nil
	a.store.save()
	a.store.mu.Unlock()
	_ = os.WriteFile(a.store.path+".journal", j, 0600)
	if s := reload(t, a, ""); hasMsg(s, tHost, "m1") != 0 {
		t.Fatal("stale journal line brought a deleted message back")
	}
	// Downgrade: an old version writes chat.json without jgen.
	a.store.mu.Lock()
	b, _ := a.store.encode()
	a.store.mu.Unlock()
	b = bytes.Replace(b, []byte(`"jgen":"`+a.store.JGen+`"`), []byte(`"x":""`), 1)
	_ = os.WriteFile(a.store.path, b, 0600)
	_ = os.WriteFile(a.store.path+".journal", j, 0600)
	if s := reload(t, a, ""); hasMsg(s, tHost, "m1") != 0 {
		t.Fatal("journal applied to a file without generation")
	}
}

// A torn last line (power loss mid-append) is skipped, the rest is kept.
func TestJournalTornLine(t *testing.T) {
	a, _ := contactApp(t)
	a.store.mu.Lock()
	a.store.save()
	a.store.mu.Unlock()
	jMsg(t, a, tHost, "m1", "x")
	f, _ := os.OpenFile(a.store.path+".journal", os.O_WRONLY|os.O_APPEND, 0600)
	f.Write([]byte(`{"g":"` + a.store.JGen + `","m":{"id":"m2","pe`))
	f.Close()
	s := reload(t, a, "")
	if hasMsg(s, tHost, "m1") != 1 || hasMsg(s, tHost, "m2") != 0 {
		t.Fatal("torn line handling")
	}
}

// The journal is folded into chat.json after journalMax entries.
func TestJournalCompacts(t *testing.T) {
	a, _ := contactApp(t)
	a.store.mu.Lock()
	a.store.save()
	a.store.mu.Unlock()
	for i := 0; i <= journalMax; i++ {
		jMsg(t, a, tHost, fmt.Sprintf("m%d", i), "x")
	}
	a.store.mu.Lock()
	jn := a.store.jn
	a.store.mu.Unlock()
	if jn > 1 {
		t.Fatalf("journal not compacted: %d entries", jn)
	}
	s := &Store{path: a.store.path}
	s.load()
	s.fix()
	if len(s.Messages) < journalMax {
		t.Fatalf("compacted file lacks messages: %d", len(s.Messages))
	}
}

// A group post (and every retried duplicate of it) rewrote the whole store
// in onGroupMsg before it was even stored.
func TestGroupPostNoRewrite(t *testing.T) {
	a, _ := tAppPC(t, tMe)
	a.sys = true
	a.store.Contacts[tHost] = Contact{Addr: tHost, Nick: "h", Shared: true, HasProf: true}
	tJoined(a, tHost, []string{tHost, tMe}, 1)
	a.store.mu.Lock()
	a.store.save()
	a.store.mu.Unlock()
	before, _ := os.ReadFile(a.store.path)
	w := wire{T: "msg", ID: "p1g0", G: tgid, Text: "hi all", TS: time.Now().Unix()}
	w.PN = powSolve(powScope(tHost, w, a.addr), "p1", w.TS, w.Text, "", 3)
	for i := 0; i < 3; i++ {
		a.handlePacket(net.ParseIP(tHost), wpkt(t, w))
	}
	after, _ := os.ReadFile(a.store.path)
	if !bytes.Equal(before, after) {
		t.Fatal("chat.json rewritten for a group post")
	}
	s := reload(t, a, "")
	if hasMsg(s, gKey(tgid), "p1") != 1 {
		t.Fatalf("group post lost/duplicated: %+v", s.Messages)
	}
}
