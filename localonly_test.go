// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOnlyRemoteAddr(t *testing.T) {
	h := localOnly("127.0.0.1:7780", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	for _, c := range []struct {
		remote, host string
		want         int
	}{
		{"127.0.0.1:5555", "127.0.0.1:7780", 204},
		{"[::1]:5555", "localhost:7780", 204},
		{"192.0.2.7:5555", "127.0.0.1:7780", 403}, // forged Host from outside
		{"[200::1]:5555", "127.0.0.1:7780", 403},
		{"127.0.0.1:5555", "evil.example:7780", 403},
	} {
		r := httptest.NewRequest("GET", "/api/state", nil)
		r.RemoteAddr, r.Host = c.remote, c.host
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != c.want {
			t.Errorf("%s Host %s: got %d want %d", c.remote, c.host, w.Code, c.want)
		}
	}
	for addr, ok := range map[string]bool{"127.0.0.1:7780": true, "[::1]:7780": true, "localhost:7780": true, "0.0.0.0:7780": false, ":7780": false, "[::]:7780": false, "192.168.1.2:7780": false} {
		if (checkUIAddr(addr) == nil) != ok {
			t.Errorf("checkUIAddr(%q) ok=%v", addr, !ok)
		}
	}
}
