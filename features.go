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
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
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
	var keys map[string]json.RawMessage
	_ = json.Unmarshal(b, &keys)
	_, hasCT := keys["ct"]
	_, hasKDF := keys["kdf"]
	err = nil
	if hasCT || hasKDF {
		// An encrypted store we can't parse would otherwise read as an empty
		// plaintext one (unknown fields are ignored) and be overwritten.
		err = errors.New("damaged encrypted store")
	} else {
		err = json.Unmarshal(b, s)
	}
	if err != nil {
		// A damaged file would be overwritten by the first save and every
		// chat lost: keep a copy and start from whatever could be read.
		bak := fmt.Sprintf("%s.corrupt-%d", s.path, time.Now().Unix())
		_ = writeAtomic(bak, b)
		log.Printf("store: %s is damaged (%v); saved a copy as %s", s.path, err, bak)
	}
}

func (s *Store) fix() {
	if s.Contacts == nil {
		s.Contacts = map[string]Contact{}
	}
	if s.Blocked == nil {
		s.Blocked = map[string]bool{}
	}
	if s.Groups == nil {
		s.Groups = map[string]Group{}
	}
	s.pruneSentCopies()
	s.dedupeGroupMsgs()
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
	if err := s.loadData(plain); err != nil {
		return err
	}
	s.key, s.salt, s.locked, s.env = key, s.env.Salt, false, nil
	s.replayJournalLocked()
	s.requeueAfterRestart()
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
	oldKey, oldSalt := s.key, s.salt
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
	if err := s.saveErr(); err != nil {
		s.key, s.salt = oldKey, oldSalt
		return fmt.Errorf("保存できませんでした: %v", err)
	}
	return nil
}

func (s *Store) lockNow() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.key == nil {
		return errors.New("パスコードが未設定です")
	}
	// Lock exactly what we hold now: re-reading the file after a failed
	// write would lock (and later unlock) an older history.
	b, err := s.writeSnapshotLocked()
	if err != nil {
		return fmt.Errorf("保存できませんでした: %v", err)
	}
	s.dirty = false
	var env envelope
	if err := json.Unmarshal(b, &env); err != nil || len(env.CT) == 0 {
		return errors.New("bad envelope")
	}
	s.env, s.locked, s.key = &env, true, nil
	s.resetData()
	s.Contacts, s.Blocked, s.Groups = map[string]Contact{}, map[string]bool{}, map[string]Group{}
	return nil
}

// resetData clears every saved (exported) field.
func (s *Store) resetData() {
	s.Nick, s.Bio, s.Avatar, s.Domain = "", "", "", ""
	s.Contacts, s.Messages, s.Blocked, s.Groups, s.Ctl = nil, nil, nil, nil, nil
	s.NostrOn, s.NostrRelays, s.NostrLegacy, s.PowStrict = false, nil, false, false
	s.Joins, s.Gone, s.Early, s.GDel = nil, nil, nil, nil
	s.GDelH, s.NRecall = nil, nil
	s.JGen = ""
}

// loadData replaces all saved fields with the JSON b (every field, so new
// ones can't be forgotten here).
func (s *Store) loadData(b []byte) error {
	s.resetData()
	err := json.Unmarshal(b, s)
	s.fix()
	return err
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
	a.udpWrite(b, &net.UDPAddr{IP: net.ParseIP(peer), Port: chatPort})
}

// udpWriteMu serialises datagram writes: the userspace stack's UDP
// endpoint is not safe for concurrent WriteTo (found with -race).
var udpWriteMu sync.Mutex

func (a *App) udpWrite(b []byte, dst *net.UDPAddr) {
	udpWriteMu.Lock()
	defer udpWriteMu.Unlock()
	_, err := a.udp.WriteTo(b, dst)
	if yggDbg {
		log.Printf("[yggdbg] out %s %s err=%v", dst.IP, dbgType(b), err)
	}
}

func (a *App) profWire() wire {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	return wire{T: "prof", Nick: a.store.Nick, Bio: a.store.Bio, Av: a.store.Avatar, Dom: a.store.Domain, PK: hex.EncodeToString(a.pub())}
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
	a.setTTLAt(peer, ttl, byMe, time.Now().Unix())
}

// setTTLAt is setTTL for a change made at time at (a message's timestamp).
func (a *App) setTTLAt(peer string, ttl int, byMe bool, at int64) {
	if ttl < 0 || ttl > 4*7*86400 {
		return
	}
	a.store.mu.Lock()
	c, ok := a.store.Contacts[peer]
	if !ok || c.TTL == ttl {
		a.store.mu.Unlock()
		return
	}
	c.TTL, c.TTLAt = ttl, at
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
				a.store.goneIfRecentLocked(m) // a late copy must not bring it back
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
		old := c
		if n := cleanNick(w.Nick); n != "" && !c.Renamed {
			c.Nick = n
		}
		c.Bio = cleanText(w.Bio, 140)
		c.Avatar = cleanText(w.Av, 8)
		c.HasProf = true
		dom := cleanDomain(w.Dom)
		recheck := dom != "" && (dom != c.Dom || time.Since(time.Unix(c.DomAt, 0)) > 12*time.Hour)
		if dom != c.Dom {
			c.Dom, c.DomOK, c.DomAt = dom, false, 0
		}
		if c == old {
			// The same profile again (it is sent 3 times, and re-shared):
			// no rewrite of the whole store per datagram.
			a.store.mu.Unlock()
			if recheck && domCheckOK(from) {
				go a.verifyDomain(from, dom)
			}
			return true
		}
		a.store.Contacts[from] = c
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", c)
		if recheck && domCheckOK(from) {
			go a.verifyDomain(from, dom)
		}
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

var (
	domCheckMu sync.Mutex
	domCheckAt = map[string]time.Time{}
)

// domCheckOK limits domain look-ups to one per peer per 30 s: a peer that
// sends a new domain in every datagram would otherwise make us fire DNS
// queries as fast as it can send.
func domCheckOK(peer string) bool {
	domCheckMu.Lock()
	defer domCheckMu.Unlock()
	if t, ok := domCheckAt[peer]; ok && time.Since(t) < 30*time.Second {
		return false
	}
	if len(domCheckAt) > 4096 {
		domCheckAt = map[string]time.Time{}
	}
	domCheckAt[peer] = time.Now()
	return true
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
	// Hold the store for the whole reset: a lock (/api/lock) between the
	// lock guard and here made the wipe below a no-op save (a locked store
	// isn't written), the new key went to disk anyway and, after the
	// restart and unlock, the old encrypted chats came back under the new ID.
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.locked {
		return fmt.Errorf("ロック中はIDを作り直せません（ロックを解除してからやり直してください）")
	}
	nc := config.GenerateConfig()
	oldKey, oldCert := a.cfg.PrivateKey, a.cfg.Certificate
	a.cfg.PrivateKey = nc.PrivateKey
	a.cfg.Certificate = nil
	if err := a.saveCfgErr(); err != nil {
		// The new key isn't on disk: wiping the chats now would restart
		// with the old address and nothing left.
		a.cfg.PrivateKey, a.cfg.Certificate = oldKey, oldCert
		return fmt.Errorf("設定を保存できませんでした: %v", err)
	}
	s := a.store
	type kept struct {
		contacts map[string]Contact
		msgs     []Message
		groups   map[string]Group
		ctl      []ctlItem
		domain   string
		blocked  map[string]bool
	}
	prev := kept{s.Contacts, s.Messages, s.Groups, s.Ctl, s.Domain, s.Blocked}
	prevJ, prevG, prevE, prevD, prevDH := s.Joins, s.Gone, s.Early, s.GDel, s.GDelH
	// Everything tied to the old address goes: chats, groups (we were host
	// or member under the old address), pending edits, our verified domain.
	s.Contacts = map[string]Contact{}
	s.Messages = nil
	s.Groups = map[string]Group{}
	s.Ctl = nil
	s.Domain = ""
	s.Blocked = map[string]bool{}
	s.Joins, s.Gone, s.Early, s.GDel = nil, nil, nil, nil
	s.GDelH = nil // (pending recalls stay: they only delete boxes we parked)
	if err := s.saveErr(); err != nil {
		// The wipe isn't on disk (disk full): the old chats would come back
		// under the new key after the restart. Keep everything as it was.
		s.Contacts, s.Messages, s.Groups, s.Ctl, s.Domain, s.Blocked = prev.contacts, prev.msgs, prev.groups, prev.ctl, prev.domain, prev.blocked
		s.Joins, s.Gone, s.Early, s.GDel, s.GDelH = prevJ, prevG, prevE, prevD, prevDH
		s.dirty = false
		a.cfg.PrivateKey, a.cfg.Certificate = oldKey, oldCert
		if cerr := a.saveCfgErr(); cerr != nil {
			log.Printf("identity reset: could not put the old key back: %v", cerr)
			return fmt.Errorf("履歴を保存できず、元の鍵も書き戻せませんでした: %v / %v", err, cerr)
		}
		return fmt.Errorf("履歴を保存できませんでした: %v", err)
	}
	go func() {
		time.Sleep(300 * time.Millisecond)
		restartAfterReset()
	}()
	return nil
}

// restartAfterReset is restartSelf (a variable so tests can see a reset
// finish without the process exiting).
var restartAfterReset = restartSelf

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
		// The UI is already open and reconnects by itself: no second tab.
		cmd.Env = append(os.Environ(), "YGGCHAT_NO_OPEN=1")
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
		var req struct {
			Nick, Bio, Avatar string
			Domain            *string
		}
		if !readJSON(w, r, &req) {
			return
		}
		var dom string
		if req.Domain != nil {
			if dom = cleanDomain(*req.Domain); dom == "" && strings.TrimSpace(*req.Domain) != "" {
				http.Error(w, "ドメイン名ではありません", 400)
				return
			}
			if dom != "" {
				ad, err := resolveYggFor(dom, a.addr) // any of its Ygg AAAA records
				if err != nil {
					http.Error(w, err.Error(), 400)
					return
				}
				if ad != a.addr {
					http.Error(w, dom+" のAAAAは "+ad+" で、あなたのアドレスではありません", 400)
					return
				}
			}
		}
		a.store.mu.Lock()
		if req.Domain != nil {
			a.store.Domain = dom
		}
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
		if !readJSON(w, r, &req) {
			return
		}
		if net.ParseIP(req.Addr) == nil {
			http.Error(w, "bad address", 400)
			return
		}
		addr := net.ParseIP(req.Addr).String()
		a.store.mu.Lock()
		if req.Block {
			a.store.Blocked[addr] = true
			delete(a.store.Contacts, addr)
			kept := a.store.Messages[:0]
			for _, m := range a.store.Messages {
				if m.Peer != addr || m.Hid { // pending group copies to them still count for the group (v0.19.2)
					kept = append(kept, m)
				} else {
					a.store.goneIfRecentLocked(m)
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
		if !readJSON(w, r, &req) {
			return
		}
		if !req.Confirm {
			http.Error(w, "bad request", 400)
			return
		}
		// The refusal (system Yggdrasil mode) must reach the UI: it used to
		// answer {} and the UI said 初期化しました / 再起動中 although the ID
		// (and, after a device move, the moved identity) was still here.
		if err := a.resetIdentity(); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Write([]byte("{}"))
	})
}
