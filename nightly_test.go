// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

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
