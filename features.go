// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// v0.5 features: passcode-encrypted history, disappearing messages,
// message requests with profile sharing on accept, blocking, identity reset.
package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"golang.org/x/crypto/scrypt"
)

// ---------- encrypted store ----------

type envelope struct {
	V     int    `json:"v"`
	KDF   string `json:"kdf"`
	Salt  []byte `json:"salt"`
	Nonce []byte `json:"nonce"`
	CT    []byte `json:"ct"`
}

func deriveKey(pass string, salt []byte) ([]byte, error) {
	return scrypt.Key([]byte(pass), salt, 1<<15, 8, 1, 32)
}

func seal(key, plain []byte) (nonce, ct []byte, err error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, plain, []byte("yggchat-store-v1")), nil
}

func open_(key, nonce, ct []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, []byte("yggchat-store-v1"))
}

// encode returns the bytes to write for the store: plaintext JSON, or an
// AES-256-GCM envelope when a passcode is set. Caller holds s.mu.
func (s *Store) encode() ([]byte, error) {
	b, err := json.Marshal(s)
	if err != nil || s.key == nil {
		return b, err
	}
	nonce, ct, err := seal(s.key, b)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{V: 1, KDF: "scrypt-32768-8-1", Salt: s.salt, Nonce: nonce, CT: ct})
}

// load reads the store file; an encrypted file leaves the store locked.
func (s *Store) load() {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var env envelope
	if json.Unmarshal(b, &env) == nil && len(env.CT) > 0 {
		s.locked, s.env = true, &env
		return
	}
	_ = json.Unmarshal(b, s)
}

func (s *Store) fix() {
	if s.Contacts == nil {
		s.Contacts = map[string]Contact{}
	}
	if s.Blocked == nil {
		s.Blocked = map[string]bool{}
	}
	if s.Nick == "" {
		s.Nick = "anon"
	}
}

var errLocked = errors.New("locked")

func (s *Store) unlock(pass string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.locked {
		return nil
	}
	key, err := deriveKey(pass, s.env.Salt)
	if err != nil {
		return err
	}
	plain, err := open_(key, s.env.Nonce, s.env.CT)
	if err != nil {
		return errors.New("パスコードが違います")
	}
	var fresh Store
	if err := json.Unmarshal(plain, &fresh); err != nil {
		return err
	}
	s.Nick, s.Bio, s.Avatar, s.Contacts, s.Messages, s.Blocked = fresh.Nick, fresh.Bio, fresh.Avatar, fresh.Contacts, fresh.Messages, fresh.Blocked
	s.fix()
	s.key, s.salt, s.locked, s.env = key, s.env.Salt, false, nil
	for i := range s.Messages {
		if s.Messages[i].Status == "sending" || s.Messages[i].Status == "failed" {
			s.Messages[i].Status = "queued"
		}
	}
	return nil
}

func (s *Store) setPass(old, nw string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.locked {
		return errLocked
	}
	if s.key != nil {
		k, err := deriveKey(old, s.salt)
		if err != nil || subtle.ConstantTimeCompare(k, s.key) != 1 {
			return errors.New("現在のパスコードが違います")
		}
	}
	if nw == "" {
		s.key, s.salt = nil, nil
	} else {
		if len([]rune(nw)) < 4 {
			return errors.New("パスコードは4文字以上にしてください")
		}
		salt := make([]byte, 16)
		if _, err := rand.Read(salt); err != nil {
			return err
		}
		k, err := deriveKey(nw, salt)
		if err != nil {
			return err
		}
		s.key, s.salt = k, salt
	}
	s.save()
	return nil
}

func (s *Store) lockNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil {
		return errors.New("パスコードが未設定です")
	}
	s.save()
	b, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil {
		return err
	}
	s.env, s.locked, s.key = &env, true, nil
	s.Contacts, s.Messages, s.Blocked = map[string]Contact{}, nil, map[string]bool{}
	s.Nick, s.Bio, s.Avatar = "", "", ""
	return nil
}

func (s *Store) isLocked() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.locked
}

// ---------- wire helpers ----------

func (a *App) sendWire(peer string, w wire) {
	if a.udp == nil {
		return
	}
	b, _ := json.Marshal(w)
	_, _ = a.udp.WriteTo(b, &net.UDPAddr{IP: net.ParseIP(peer), Port: chatPort})
}

func (a *App) profWire() wire {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return wire{T: "prof", Nick: a.store.Nick, Bio: a.store.Bio, Av: a.store.Avatar, PK: hex.EncodeToString(a.pub())}
}

// sendProf shares our profile with peer (a few times, it is a datagram).
func (a *App) sendProf(peer string) {
	w := a.profWire()
	go func() {
		for i := 0; i < 3; i++ {
			a.sendWire(peer, w)
			time.Sleep(1500 * time.Millisecond)
		}
	}()
}

func (a *App) profAll() {
	a.store.mu.Lock()
	var ps []string
	for p, c := range a.store.Contacts {
		if c.Shared {
			ps = append(ps, p)
		}
	}
	a.store.mu.Unlock()
	for _, p := range ps {
		a.sendProf(p)
	}
}

func sysID() string { return fmt.Sprintf("s%x", time.Now().UnixNano()) }

// setTTL changes a conversation's disappearing timer and logs a system line.
// Caller must NOT hold store.mu.
func (a *App) setTTL(peer string, ttl int, byMe bool) {
	if ttl < 0 || ttl > 4*7*86400 {
		return
	}
	a.store.mu.Lock()
	c, ok := a.store.Contacts[peer]
	if !ok || c.TTL == ttl {
		a.store.mu.Unlock()
		return
	}
	c.TTL = ttl
	a.store.Contacts[peer] = c
	m := Message{ID: sysID(), Peer: peer, Sys: fmt.Sprintf("ttl:%d", ttl), TS: time.Now().Unix(), Out: byMe}
	a.store.Messages = append(a.store.Messages, m)
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("contact", c)
	a.broadcast("message", m)
	if byMe {
		go func() {
			for i := 0; i < 3; i++ {
				a.sendWire(peer, wire{T: "ttl", TTL: ttl})
				time.Sleep(1500 * time.Millisecond)
			}
		}()
	}
}

// sweep deletes expired disappearing messages.
func (a *App) sweep() {
	for range time.Tick(2 * time.Second) {
		now := time.Now().Unix()
		a.store.mu.Lock()
		if a.store.locked {
			a.store.mu.Unlock()
			continue
		}
		var gone []map[string]string
		kept := a.store.Messages[:0]
		for _, m := range a.store.Messages {
			if m.Exp > 0 && m.Exp <= now {
				gone = append(gone, map[string]string{"peer": m.Peer, "id": m.ID})
				continue
			}
			kept = append(kept, m)
		}
		a.store.Messages = kept
		if len(gone) > 0 {
			a.store.save()
		}
		a.store.mu.Unlock()
		for _, g := range gone {
			a.broadcast("msg_del", g)
		}
	}
}

// ---------- incoming control packets ----------

// handleCtl processes prof/ttl packets. Returns true if handled.
func (a *App) handleCtl(from string, w wire) bool {
	switch w.T {
	case "prof":
		a.store.mu.Lock()
		c, ok := a.store.Contacts[from]
		if !ok || a.store.Blocked[from] {
			a.store.mu.Unlock()
			return true
		}
		if n := cleanNick(w.Nick); n != "" && !c.Renamed {
			c.Nick = n
		}
		c.Bio = cleanText(w.Bio, 140)
		c.Avatar = cleanText(w.Av, 8)
		c.HasProf = true
		a.store.Contacts[from] = c
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", c)
		return true
	case "ttl":
		a.store.mu.Lock()
		blocked := a.store.Blocked[from]
		a.store.mu.Unlock()
		if !blocked {
			a.setTTL(from, w.TTL, false)
		}
		return true
	}
	return false
}

func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if (r < 0x20 && r != '\n') || (r >= 0x7f && r <= 0x9f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, s)
	if r := []rune(strings.TrimSpace(s)); len(r) > max {
		s = string(r[:max])
	}
	return strings.TrimSpace(s)
}

// ---------- identity reset ----------

func (a *App) resetIdentity() error {
	if a.sys {
		return fmt.Errorf("端末のYggdrasilを使用中はIDを作り直せません（Yggdrasilアプリ側で鍵を再生成してください）")
	}
	nc := config.GenerateConfig()
	a.cfg.PrivateKey = nc.PrivateKey
	a.cfg.Certificate = nil
	a.saveCfg()
	a.store.mu.Lock()
	a.store.Contacts = map[string]Contact{}
	a.store.Messages = nil
	a.store.save()
	a.store.mu.Unlock()
	go func() {
		time.Sleep(300 * time.Millisecond)
		restartSelf()
	}()
	return nil
}

// restartSelf re-execs the binary. Under the Android service (which restarts
// the node when it exits) it just exits.
func restartSelf() {
	if os.Getenv("YGGCHAT_SUPERVISED") == "1" {
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err == nil {
		cmd := exec.Command(exe, os.Args[1:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		_ = cmd.Start()
	}
	os.Exit(0)
}

// listenRetry waits for the UI port to free up (after a restart).
func listenRetry(addr string) (net.Listener, error) {
	var err error
	for i := 0; i < 50; i++ {
		var ln net.Listener
		if ln, err = net.Listen("tcp", addr); err == nil {
			return ln, nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return nil, err
}

// ---------- HTTP ----------

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(v) != nil {
		http.Error(w, "bad request", 400)
		return false
	}
	return true
}

func (a *App) featureHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/unlock", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Pass string }
		if !readJSON(w, r, &req) {
			return
		}
		if err := a.store.unlock(req.Pass); err != nil {
			time.Sleep(700 * time.Millisecond) // slow down guessing
			http.Error(w, err.Error(), 403)
			return
		}
		go func() { time.Sleep(500 * time.Millisecond); a.helloAll() }()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/passcode", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Old, New string }
		if !readJSON(w, r, &req) {
			return
		}
		if err := a.store.setPass(req.Old, req.New); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/lock", func(w http.ResponseWriter, r *http.Request) {
		var req struct{}
		if !readJSON(w, r, &req) {
			return
		}
		if err := a.store.lockNow(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		a.broadcast("locked", map[string]bool{"locked": true})
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/profile", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Nick, Bio, Avatar string }
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		a.store.Nick = cleanNick(req.Nick)
		if a.store.Nick == "" {
			a.store.Nick = "anon"
		}
		a.store.Bio = cleanText(req.Bio, 140)
		a.store.Avatar = cleanText(req.Avatar, 8)
		a.store.save()
		a.store.mu.Unlock()
		a.profAll()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/block", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Addr  string
			Block bool
		}
		if !readJSON(w, r, &req) || net.ParseIP(req.Addr) == nil {
			return
		}
		addr := net.ParseIP(req.Addr).String()
		a.store.mu.Lock()
		if req.Block {
			a.store.Blocked[addr] = true
			delete(a.store.Contacts, addr)
			kept := a.store.Messages[:0]
			for _, m := range a.store.Messages {
				if m.Peer != addr {
					kept = append(kept, m)
				}
			}
			a.store.Messages = kept
		} else {
			delete(a.store.Blocked, addr)
		}
		a.store.save()
		a.store.mu.Unlock()
		if req.Block {
			a.broadcast("contact_del", map[string]string{"addr": addr})
		}
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/identity/reset", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Confirm bool }
		if !readJSON(w, r, &req) || !req.Confirm {
			return
		}
		_ = a.resetIdentity()
		w.Write([]byte("{}"))
	})
}
