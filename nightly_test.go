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
