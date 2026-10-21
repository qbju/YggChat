// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

// The local UI/API key. Loopback is shared by every program on the device
// (on Android: by every app), so the Host/Origin/peer checks in localOnly
// only stop web pages. Each request must also carry this key, either as the
// X-YggChat-Key header (CLI, Android service) or as an HttpOnly cookie that
// the UI receives by opening the launch URL /?k=<key> once.
// The key lives in <data>/ui-key (0600), so only the user/app that owns the
// data directory can read it.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const uiKeyHeader = "X-YggChat-Key"

var uiKey string

func loadUIKey(dataDir string) (string, error) {
	p := filepath.Join(dataDir, "ui-key")
	if b, err := os.ReadFile(p); err == nil {
		if k := strings.TrimSpace(string(b)); len(k) == 64 {
			if _, err := hex.DecodeString(k); err == nil {
				_ = os.Chmod(p, 0600)
				return k, nil
			}
		}
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	k := hex.EncodeToString(b)
	if err := writeAtomic(p, []byte(k+"\n")); err != nil {
		return "", err
	}
	return k, nil
}

// uiURL is the launch URL that hands the key to a browser or WebView.
func uiURL(addr string) string { return "http://" + addr + "/?k=" + uiKey }

func keyCookieName(addr string) string {
	_, port, _ := net.SplitHostPort(addr)
	return "yggchat_key_" + port // cookies ignore ports; keep instances apart
}

func keyOK(got string) bool {
	return uiKey != "" && len(got) == len(uiKey) && subtle.ConstantTimeCompare([]byte(got), []byte(uiKey)) == 1
}

const noKeyPage = `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<title>YggChat</title><body style="font-family:sans-serif;max-width:36em;margin:2em auto;padding:0 1em;line-height:1.6">
<h1>YggChat</h1><p>この画面を開くには、起動時に表示された <code>?k=</code> 付きのURLを開いてください。</p>
<p>To open the UI, use the URL with <code>?k=</code> that YggChat printed when it started.</p>`

// uiProof answers GET /api/proof?n=<nonce> with HMAC-SHA256(key, nonce)
// without needing the key. Another app (or local user) can bind the UI port
// first when our node isn't running (first start, restarts); a client that
// holds the key file (the Android service and WebView loader) checks the
// proof before sending the key, so an impostor on the port can't collect it.
func uiProof(nonce string) string {
	m := hmac.New(sha256.New, []byte(uiKey))
	m.Write([]byte("yggchat-ui-proof\n" + nonce))
	return hex.EncodeToString(m.Sum(nil))
}

func validNonce(n string) bool {
	if len(n) < 32 || len(n) > 128 {
		return false
	}
	_, err := hex.DecodeString(n)
	return err == nil
}

func keyGuard(addr string, h http.Handler) http.Handler {
	name := keyCookieName(addr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/proof" && r.Method == "GET" {
			n := r.URL.Query().Get("n")
			if uiKey == "" || !validNonce(n) {
				http.Error(w, "bad nonce", 400)
				return
			}
			w.Write([]byte(`{"mac":"` + uiProof(n) + `"}`))
			return
		}
		if k := r.URL.Query().Get("k"); k != "" {
			if !keyOK(k) {
				http.Error(w, "bad key", 403)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: name, Value: uiKey, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			q := r.URL.Query()
			q.Del("k")
			u := *r.URL
			u.RawQuery = q.Encode()
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, u.RequestURI(), http.StatusSeeOther)
			return
		}
		ok := keyOK(r.Header.Get(uiKeyHeader))
		if !ok {
			if c, err := r.Cookie(name); err == nil {
				ok = keyOK(c.Value)
			}
		}
		if !ok {
			if r.Method == "GET" && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.WriteHeader(401)
				w.Write([]byte(noKeyPage))
				return
			}
			http.Error(w, "unauthorized", 401)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// keyTransport adds the key to the CLI's own requests.
type keyTransport struct{}

func (keyTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set(uiKeyHeader, uiKey)
	return http.DefaultTransport.RoundTrip(r)
}
