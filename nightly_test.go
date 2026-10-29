// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/config"
)

// The identity reset's refusal must reach the UI (it answered {} before).
func TestIdentityResetRefusalReported(t *testing.T) {
	a := tApp(t, tMe)
	a.sys = true
	a.store.Contacts[tHost] = Contact{Addr: tHost}
	mux := http.NewServeMux()
	a.featureHandlers(mux)
	for _, body := range []string{`{"Confirm":true}`, `{}`} {
		r := httptest.NewRequest("POST", "/api/identity/reset", strings.NewReader(body))
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Errorf("%s: %d %q, want 400", body, w.Code, w.Body.String())
		}
	}
	if _, ok := a.store.Contacts[tHost]; !ok {
		t.Fatal("refused reset wiped the contacts")
	}
}

// A reset whose new key can't be written keeps the chats and the old key.
func TestIdentityResetKeepsChatsWhenCfgUnwritable(t *testing.T) {
	a := tApp(t, tMe)
	a.cfg = config.GenerateConfig()
	old := string(a.cfg.PrivateKey)
	a.cfgPath = filepath.Join(t.TempDir(), "missing-dir", "cfg.json")
	a.store.Contacts[tHost] = Contact{Addr: tHost}
	if err := a.resetIdentity(); err == nil {
		t.Fatal("reset succeeded without writing the config")
	}
	if string(a.cfg.PrivateKey) != old {
		t.Fatal("key changed in memory")
	}
	if _, ok := a.store.Contacts[tHost]; !ok {
		t.Fatal("contacts wiped")
	}
}

// Text sent after an unconfirmed /block cancels it.
func TestCLIConfirmCancelledBySend(t *testing.T) {
	c := &cli{out: bufio.NewWriter(io.Discard)}
	c.confirm = "/block"
	c.send("hello") // no chat open: nothing is sent
	if c.confirm != "" {
		t.Fatalf("confirm still %q", c.confirm)
	}
}

// A device move whose key or data can't be written leaves this node as it
// was (the write error was ignored: moved history under the old key).
func TestMoveImportRollsBackOnWriteError(t *testing.T) {
	newKey := config.GenerateConfig().PrivateKey
	bundle := func() []byte {
		b, _ := json.Marshal(moveBundle{V: 1, Key: hex.EncodeToString(newKey), Store: json.RawMessage(`{"nick":"moved","contacts":{"200:9::9":{"addr":"200:9::9"}}}`)})
		return b
	}
	check := func(t *testing.T, a *App, oldKey string) {
		if string(a.cfg.PrivateKey) != oldKey {
			t.Fatal("key changed")
		}
		if a.store.Nick != "mine" {
			t.Fatalf("nick %q", a.store.Nick)
		}
		if _, ok := a.store.Contacts[tHost]; !ok {
			t.Fatal("own contact lost")
		}
		if _, ok := a.store.Contacts["200:9::9"]; ok {
			t.Fatal("moved contact kept")
		}
	}
	setup := func(t *testing.T) (*App, string) {
		a := tApp(t, tMe)
		a.cfg = config.GenerateConfig()
		a.cfgPath = filepath.Join(t.TempDir(), "cfg.json")
		a.store.Nick = "mine"
		a.store.Contacts[tHost] = Contact{Addr: tHost}
		return a, string(a.cfg.PrivateKey)
	}
	t.Run("cfg", func(t *testing.T) {
		a, old := setup(t)
		a.cfgPath = filepath.Join(t.TempDir(), "missing-dir", "cfg.json")
		if err := a.importBundle(bundle()); err == nil {
			t.Fatal("import succeeded without writing the key")
		}
		check(t, a, old)
	})
	t.Run("store", func(t *testing.T) {
		a, old := setup(t)
		a.store.path = filepath.Join(t.TempDir(), "missing-dir", "c.json")
		if err := a.importBundle(bundle()); err == nil {
			t.Fatal("import succeeded without writing the data")
		}
		check(t, a, old)
		b, err := os.ReadFile(a.cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var c config.NodeConfig
		if json.Unmarshal(b, &c) != nil || string(c.PrivateKey) != old {
			t.Fatal("old key not written back")
		}
	})
}

// A 1:1 photo/long text whose chunks were all acked but which the receiver
// didn't store (request limit, failed save) is sent again once, not only its
// header until the sender gives up.
func TestFileChunksResentWhenNotStored(t *testing.T) {
	old := fileRound
	fileRound = 40 * time.Millisecond
	t.Cleanup(func() { fileRound = old })
	a, pc := tAppPC(t, tMe)
	m := Message{ID: "m1abc", Peer: tHost, Out: true, Text: strings.Repeat("あ", 1000), TS: time.Now().Unix(), Status: "sending"}
	a.store.Messages = append(a.store.Messages, m)
	stop := make(chan struct{})
	go func() { // the receiver acks every chunk, never the message
		seen := 0
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			fc := pc.sent(tHost, "fc")
			for _, w := range fc[seen:] {
				a.onFileAck(tHost, wire{T: "fca", ID: w.ID, I: w.I})
			}
			seen = len(fc)
		}
	}()
	raw := []byte(m.Text)
	ok := a.deliverFile(m, textMime, raw, 3)
	close(stop)
	if ok {
		t.Fatal("delivered without an ack")
	}
	n := 0
	for _, w := range pc.sent(tHost, "fc") {
		if w.I == 0 {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("chunk 0 sent %d times, want 2 (once more after the receiver dropped it)", n)
	}
}

// flush doesn't hold back pending edits/unsends behind a message that is
// already being sent.
func TestFlushSendsCtlPastInflight(t *testing.T) {
	a, pc := tAppPC(t, tMe)
	a.store.Messages = append(a.store.Messages, Message{ID: "m1abc", Peer: tHost, Out: true, Text: "hi", TS: time.Now().Unix(), Status: "queued"})
	a.store.Ctl = []ctlItem{{Peer: tHost, W: wire{T: "edit", ID: "c1", Ref: "m0old", Text: "x"}}}
	a.flMu.Lock()
	a.inflight["m1abc"] = true
	a.flMu.Unlock()
	done := make(chan struct{})
	go func() { a.flush(tHost); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for len(pc.sent(tHost, "edit")) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("pending edit not sent")
		}
		time.Sleep(5 * time.Millisecond)
	}
	a.ackMu.Lock()
	if ch, ok := a.acks["c1"]; ok {
		close(ch)
		delete(a.acks, "c1")
	}
	a.ackMu.Unlock()
	<-done
}

// nightly 10-29: an identity reset while the history is locked used to
// write the new key and skip the wipe (a locked store isn't saved): after
// the restart and unlock the old chats were back under the new ID.
func TestIdentityResetRefusedWhileLocked(t *testing.T) {
	a := tApp(t, tMe)
	a.cfg = config.GenerateConfig()
	a.cfgPath = filepath.Join(t.TempDir(), "cfg.json")
	old := string(a.cfg.PrivateKey)
	a.store.Contacts[tHost] = Contact{Addr: tHost}
	if err := a.store.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	if err := a.store.lockNow(); err != nil {
		t.Fatal(err)
	}
	if err := a.resetIdentity(); err == nil {
		t.Fatal("reset went ahead while locked")
	}
	if string(a.cfg.PrivateKey) != old {
		t.Fatal("key changed while the chats stayed")
	}
	if _, err := os.Stat(a.cfgPath); err == nil {
		t.Fatal("new key written")
	}
	if err := a.store.unlock("pass1234"); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.store.Contacts[tHost]; !ok {
		t.Fatal("chats lost")
	}
}

// A reset whose wiped history can't be written puts the old key back
// (the chats would otherwise come back under the new ID).
func TestIdentityResetRollsBackWhenStoreUnwritable(t *testing.T) {
	a := tApp(t, tMe)
	a.cfg = config.GenerateConfig()
	a.cfgPath = filepath.Join(t.TempDir(), "cfg.json")
	if err := a.saveCfgErr(); err != nil {
		t.Fatal(err)
	}
	old := string(a.cfg.PrivateKey)
	a.store.Contacts[tHost] = Contact{Addr: tHost}
	a.store.path = filepath.Join(t.TempDir(), "missing-dir", "c.json")
	if err := a.resetIdentity(); err == nil {
		t.Fatal("reset reported success without writing the history")
	}
	if string(a.cfg.PrivateKey) != old {
		t.Fatal("key changed in memory")
	}
	b, _ := os.ReadFile(a.cfgPath)
	var c config.NodeConfig
	if json.Unmarshal(b, &c) != nil || string(c.PrivateKey) != old {
		t.Fatal("new key left on disk")
	}
	if _, ok := a.store.Contacts[tHost]; !ok {
		t.Fatal("contacts wiped")
	}
}

// A passcode-protected (unlocked) reset writes the wiped history.
func TestIdentityResetWipesEncryptedHistory(t *testing.T) {
	restarted := make(chan bool, 1)
	defer func(f func()) { restartAfterReset = f }(restartAfterReset)
	restartAfterReset = func() { restarted <- true }
	a := tApp(t, tMe)
	a.cfg = config.GenerateConfig()
	a.cfgPath = filepath.Join(t.TempDir(), "cfg.json")
	a.store.Contacts[tHost] = Contact{Addr: tHost}
	if err := a.store.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	if err := a.resetIdentity(); err != nil {
		t.Fatal(err)
	}
	<-restarted
	s2 := &Store{path: a.store.path}
	s2.load()
	if err := s2.unlock("pass1234"); err != nil {
		t.Fatal(err)
	}
	if len(s2.Contacts) != 0 {
		t.Fatal("old chats still on disk")
	}
}

// nightly 10-29: the CLI added our sent message from the /api/send reply and
// again from its "message" event when the reply was read first, so /hist
// showed it twice (and a later status updated both lines).
func TestCLISentMessageNotDoubled(t *testing.T) {
	old := uiKey
	uiKey = strings.Repeat("ab", 32)
	defer func() { uiKey = old }()
	m := Message{ID: "18e2f650d5ea4abe", Peer: tHost, From: tMe, Text: "hi", TS: time.Now().Unix(), Out: true, Status: "sending"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/proof" {
			w.Write([]byte(`{"mac":"` + uiProof(r.URL.Query().Get("n")) + `"}`))
			return
		}
		json.NewEncoder(w).Encode(m)
	}))
	defer srv.Close()
	c := &cli{base: srv.URL, out: bufio.NewWriter(io.Discard), st: cliState{Contacts: map[string]cliContact{tHost: {Addr: tHost}}}, gdone: map[string]bool{}, cur: tHost}
	c.send("hi") // the reply lands first...
	ev, _ := json.Marshal(map[string]any{"type": "message", "data": m})
	c.onEvent(ev) // ...then the event for the same message
	n := 0
	for _, x := range c.st.Messages {
		if x.ID == m.ID {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("message held %d times, want 1", n)
	}
}

// nightly 10-29: an edit/unsend queued while flushCtl was already sending an
// older one to that peer waited for the next hello (up to 20 s, with the
// peer online): the running flush only sent what it had listed at its start.
func TestFlushCtlSendsCtlQueuedMeanwhile(t *testing.T) {
	a, pc := tAppPC(t, tMe)
	a.store.Ctl = []ctlItem{{Peer: tHost, W: wire{T: "edit", ID: "c1", Ref: "m0old", Text: "x"}}}
	ack := func(id string) {
		a.ackMu.Lock()
		if ch, ok := a.acks[id]; ok {
			close(ch)
			delete(a.acks, id)
		}
		a.ackMu.Unlock()
	}
	waitSent := func(id string) bool {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			for _, w := range pc.sent(tHost, "edit") {
				if w.ID == id {
					return true
				}
			}
			time.Sleep(5 * time.Millisecond)
		}
		return false
	}
	done := make(chan struct{})
	go func() { a.flushCtl(tHost); close(done) }()
	if !waitSent("c1") {
		t.Fatal("first edit not sent")
	}
	a.queueCtl(tHost, wire{T: "edit", ID: "c2", Ref: "m1new", Text: "y"}, false) // its own flush finds the peer busy
	time.Sleep(100 * time.Millisecond)
	ack("c1")
	if !waitSent("c2") {
		t.Fatal("edit queued during the flush waited for the next hello")
	}
	ack("c2")
	<-done
}
