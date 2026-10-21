package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// apiPaths finds every /api endpoint registered anywhere in the package, so
// a new one can't slip in without the key check.
func apiPaths(t *testing.T) []string {
	re := regexp.MustCompile(`HandleFunc\("(/api/[^"]*)"`)
	fs, _ := filepath.Glob("*.go")
	var out []string
	for _, f := range fs {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		for _, m := range re.FindAllStringSubmatch(string(b), -1) {
			out = append(out, m[1])
		}
	}
	if len(out) < 30 {
		t.Fatalf("found only %d endpoints", len(out))
	}
	return out
}

func TestEveryAPINeedsKey(t *testing.T) {
	uiKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	defer func() { uiKey = "" }()
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) })
	h := localOnly("127.0.0.1:7780", keyGuard("127.0.0.1:7780", inner))
	ck := &http.Cookie{Name: keyCookieName("127.0.0.1:7780"), Value: uiKey}
	for _, p := range apiPaths(t) {
		for _, method := range []string{"GET", "POST"} {
			try := func(name string, want int, mod func(r *http.Request)) {
				called = false
				r := httptest.NewRequest(method, p, strings.NewReader("{}"))
				r.RemoteAddr, r.Host = "127.0.0.1:5000", "127.0.0.1:7780"
				r.Header.Set("Content-Type", "application/json")
				mod(r)
				w := httptest.NewRecorder()
				h.ServeHTTP(w, r)
				if w.Code != want || (want != 204 && called) {
					t.Errorf("%s %s %s: %d (handler ran: %v), want %d", method, p, name, w.Code, called, want)
				}
			}
			try("no key", 401, func(r *http.Request) {})
			try("wrong key", 401, func(r *http.Request) { r.Header.Set(uiKeyHeader, strings.Repeat("0", 64)) })
			try("cookie, other origin", 403, func(r *http.Request) {
				r.AddCookie(ck)
				r.Header.Set("Origin", "http://127.0.0.1:8000")
			})
			try("cookie, same-site page (img/form)", 403, func(r *http.Request) {
				r.AddCookie(ck)
				r.Header.Set("Sec-Fetch-Site", "same-site")
			})
			try("cookie, rebinding host", 403, func(r *http.Request) {
				r.AddCookie(ck)
				r.Host = "attacker.example:7780"
			})
			try("cookie", 204, func(r *http.Request) {
				r.AddCookie(ck)
				r.Header.Set("Sec-Fetch-Site", "same-origin")
				r.Header.Set("Origin", "http://127.0.0.1:7780")
			})
			if method == "POST" {
				try("form post", 415, func(r *http.Request) {
					r.AddCookie(ck)
					r.Header.Set("Content-Type", "text/plain")
				})
			}
		}
	}
}
