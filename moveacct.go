// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Moving the account to another device (v0.15). See move/move.go for the
// crypto. Wire: {t:"mh",id,size,n,sha} header, {t:"mc",id,i,d} chunks acked
// with {t:"mca",id,i}; when the receiver has opened and checked the bundle it
// answers {t:"mok",id} (or {t:"mng",id,text}) and switches to the moved key.
package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"

	"github.com/h6e/yggchat/move"
)

type moveBundle struct {
	V     int             `json:"v"`
	Key   string          `json:"key"`   // Ed25519 private key, hex (64 bytes)
	Store json.RawMessage `json:"store"` // the Store as saved (messages optional)
}

var mv struct {
	mu sync.Mutex
	// receiving
	priv   *ecdh.PrivateKey
	id     string
	from   string
	hdr    wire
	chunks [][]byte
	got    int
	at     time.Time
	done   bool
	// sending
	sendID    string
	sendAcks  map[int]bool
	sendHello bool        // the receiver answered our header
	sendRes   chan string // "" = ok, else error text
}

func (a *App) moveBundle(history bool) ([]byte, error) {
	if a.sys {
		return nil, errors.New("端末のYggdrasilを使用中は鍵を移せません")
	}
	a.store.mu.Lock()
	if a.store.locked {
		a.store.mu.Unlock()
		return nil, errors.New("ロックを解除してください")
	}
	sb, err := json.Marshal(a.store)
	a.store.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !history {
		var m map[string]json.RawMessage
		json.Unmarshal(sb, &m)
		delete(m, "messages")
		delete(m, "ctl")
		sb, _ = json.Marshal(m)
	}
	return json.Marshal(moveBundle{V: 1, Key: hex.EncodeToString(a.cfg.PrivateKey), Store: sb})
}

// importBundle replaces our key and data with the bundle's and restarts.
func (a *App) importBundle(b []byte) error {
	if a.sys {
		return errors.New("端末のYggdrasilを使用中は鍵を受け取れません")
	}
	var mb moveBundle
	if json.Unmarshal(b, &mb) != nil || mb.V != 1 {
		return errors.New("対応していない移行データです（アプリを更新してください）")
	}
	key, err := hex.DecodeString(mb.Key)
	if err != nil || len(key) != ed25519.PrivateKeySize || string(ed25519.NewKeyFromSeed(key[:32])) != string(key) {
		return errors.New("鍵が壊れています")
	}
	var ns Store
	if json.Unmarshal(mb.Store, &ns) != nil {
		return errors.New("データが壊れています")
	}
	a.store.mu.Lock()
	if a.store.locked {
		a.store.mu.Unlock()
		return errors.New("ロックを解除してください")
	}
	s := a.store
	s.Nick, s.Bio, s.Avatar, s.Domain = ns.Nick, ns.Bio, ns.Avatar, ns.Domain
	s.Contacts, s.Messages, s.Blocked, s.Groups, s.Ctl = ns.Contacts, ns.Messages, ns.Blocked, ns.Groups, ns.Ctl
	s.NostrOn, s.NostrRelays, s.NostrLegacy = ns.NostrOn, ns.NostrRelays, ns.NostrLegacy
	if s.Contacts == nil {
		s.Contacts = map[string]Contact{}
	}
	if s.Nick == "" {
		s.Nick = "anon"
	}
	s.save()
	a.store.mu.Unlock()
	a.cfg.PrivateKey = key
	a.cfg.Certificate = nil
	a.saveCfg()
	go func() {
		time.Sleep(700 * time.Millisecond)
		restartSelf()
	}()
	return nil
}

// ---------- receiving ----------

func (a *App) onMoveIn(from string, w wire) {
	mv.mu.Lock()
	defer mv.mu.Unlock()
	if mv.priv == nil || mv.done && w.ID == mv.id {
		if mv.done && w.ID == mv.id {
			a.sendWire(from, wire{T: "mok", ID: w.ID})
		}
		return
	}
	switch w.T {
	case "mh":
		if w.ID == mv.id && from == mv.from {
			a.sendWire(from, wire{T: "mha", ID: w.ID})
			return
		}
		if w.ID == mv.id || len(w.ID) < 8 || len(w.ID) > 40 || !shaRe.MatchString(w.Sha) || w.Size < 33 || w.Size > move.MaxSize || w.N != (w.Size+move.Chunk-1)/move.Chunk {
			return
		}
		if mv.id != "" && mv.from != from && time.Since(mv.at) < time.Minute {
			return // another transfer is running
		}
		mv.id, mv.from, mv.hdr, mv.chunks, mv.got, mv.at = w.ID, from, w, make([][]byte, w.N), 0, time.Now()
		a.sendWire(from, wire{T: "mha", ID: w.ID})
		a.broadcast("move", map[string]any{"role": "recv", "phase": "recv", "got": 0, "n": w.N, "from": from})
	case "mc":
		if w.ID != mv.id || from != mv.from || w.I < 0 || w.I >= len(mv.chunks) {
			return
		}
		if mv.chunks[w.I] == nil {
			b, err := base64.StdEncoding.DecodeString(w.D)
			if err != nil || len(b) == 0 || len(b) > move.Chunk {
				return
			}
			mv.chunks[w.I] = b
			mv.got++
			mv.at = time.Now()
			if mv.got%64 == 0 {
				a.broadcast("move", map[string]any{"role": "recv", "phase": "recv", "got": mv.got, "n": len(mv.chunks), "from": from})
			}
		}
		a.sendWire(from, wire{T: "mca", ID: w.ID, I: w.I})
		if mv.got < len(mv.chunks) {
			return
		}
		blob := make([]byte, 0, mv.hdr.Size)
		for _, c := range mv.chunks {
			blob = append(blob, c...)
		}
		sum := sha256.Sum256(blob)
		fail := func(msg string) {
			a.sendWire(from, wire{T: "mng", ID: w.ID, Text: msg})
			a.broadcast("move", map[string]any{"role": "recv", "phase": "error", "err": msg})
			mv.id, mv.chunks = "", nil
		}
		if len(blob) != mv.hdr.Size || hex.EncodeToString(sum[:]) != mv.hdr.Sha {
			fail("データが壊れていました")
			return
		}
		plain, err := move.Open(mv.priv, blob)
		if err != nil {
			fail(err.Error())
			return
		}
		a.broadcast("move", map[string]any{"role": "recv", "phase": "import", "got": mv.got, "n": len(mv.chunks)})
		if err := a.importBundle(plain); err != nil {
			fail(err.Error())
			return
		}
		mv.done, mv.chunks = true, nil
		for i := 0; i < 3; i++ {
			a.sendWire(from, wire{T: "mok", ID: w.ID})
		}
	}
}

// ---------- sending ----------

func (a *App) onMoveAck(from string, w wire) {
	mv.mu.Lock()
	defer mv.mu.Unlock()
	if w.ID == "" || w.ID != mv.sendID {
		return
	}
	switch w.T {
	case "mha":
		mv.sendHello = true
	case "mca":
		mv.sendHello = true
		mv.sendAcks[w.I] = true
	case "mok", "mng":
		if mv.sendRes != nil {
			res := ""
			if w.T == "mng" {
				res = "受け取り側: " + cleanText(w.Text, 200)
				if res == "受け取り側: " {
					res = "受け取り側で失敗しました"
				}
			}
			select {
			case mv.sendRes <- res:
			default:
			}
		}
	}
}

func (a *App) moveSend(peer string, blob []byte) {
	idb := make([]byte, 8)
	rand.Read(idb)
	id := hex.EncodeToString(idb)
	sum := sha256.Sum256(blob)
	n := (len(blob) + move.Chunk - 1) / move.Chunk
	res := make(chan string, 1)
	mv.mu.Lock()
	mv.sendID, mv.sendAcks, mv.sendRes, mv.sendHello = id, map[int]bool{}, res, false
	mv.mu.Unlock()
	finish := func(err string) {
		mv.mu.Lock()
		if mv.sendID == id {
			mv.sendID, mv.sendAcks, mv.sendRes = "", nil, nil
		}
		mv.mu.Unlock()
		if err == "" {
			a.broadcast("move", map[string]any{"role": "send", "phase": "done", "got": n, "n": n})
		} else {
			a.broadcast("move", map[string]any{"role": "send", "phase": "error", "err": err})
		}
	}
	for a.udp == nil {
		time.Sleep(200 * time.Millisecond)
	}
	hdr := wire{T: "mh", ID: id, Size: len(blob), N: n, Sha: hex.EncodeToString(sum[:])}
	lastProgress, last := time.Now(), -1
	// Until the receiver answers the header (mha), send only the header so
	// the first packets to a new address don't flood the key lookup; v0.15.0
	// receivers never answer it, so start the chunks anyway after 4 s. Then
	// grow the window from 32 chunks as acks come in.
	start, win := time.Now(), 32
	for {
		mv.mu.Lock()
		if mv.sendID != id {
			mv.mu.Unlock()
			finish("キャンセルしました")
			return
		}
		hello := mv.sendHello
		var todo []int
		for i := 0; i < n && len(todo) < win; i++ {
			if !mv.sendAcks[i] {
				todo = append(todo, i)
			}
		}
		got := len(mv.sendAcks)
		mv.mu.Unlock()
		if got != last {
			if last >= 0 && win < 256 {
				win *= 2
			}
			last, lastProgress = got, time.Now()
			a.broadcast("move", map[string]any{"role": "send", "phase": "send", "got": got, "n": n})
		} else if time.Since(lastProgress) > 90*time.Second {
			finish("相手に届きません。新しい端末で受け取り画面が開いているか確認してください")
			return
		}
		a.sendWire(peer, hdr)
		time.Sleep(30 * time.Millisecond)
		if !hello && time.Since(start) < 4*time.Second {
			todo = nil
		}
		for k, i := range todo {
			end := min((i+1)*move.Chunk, len(blob))
			a.sendWire(peer, wire{T: "mc", ID: id, I: i, D: base64.StdEncoding.EncodeToString(blob[i*move.Chunk : end])})
			if k%8 == 7 {
				time.Sleep(3 * time.Millisecond)
			}
		}
		select {
		case r := <-res:
			finish(r)
			return
		case <-time.After(700 * time.Millisecond):
		}
	}
}

func (a *App) moveHandlers(mux *http.ServeMux) {
	jsonOut := func(w http.ResponseWriter, v any) { json.NewEncoder(w).Encode(v) }
	mux.HandleFunc("/api/move/recv", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Stop bool }
		if !readJSON(w, r, &req) {
			return
		}
		mv.mu.Lock()
		defer mv.mu.Unlock()
		if req.Stop {
			if !mv.done {
				mv.priv, mv.id, mv.chunks = nil, "", nil
			}
			w.Write([]byte("{}"))
			return
		}
		if a.sys {
			http.Error(w, "端末のYggdrasilを使用中は鍵を受け取れません", 400)
			return
		}
		k, code, err := move.NewReceiver(a.addr)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		mv.priv, mv.id, mv.chunks, mv.done = k, "", nil, false
		jsonOut(w, map[string]any{"code": code})
	})
	mux.HandleFunc("/api/move/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code    string
			History bool
			Cancel  bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		if req.Cancel {
			mv.mu.Lock()
			mv.sendID = ""
			mv.mu.Unlock()
			w.Write([]byte("{}"))
			return
		}
		peer, rpk, err := move.ParseCode(req.Code)
		if err == nil && peer == a.addr {
			err = errors.New("これはこの端末自身のコードです。新しい端末に出ているコードを入れてください")
		}
		mv.mu.Lock()
		if err == nil && mv.sendID != "" {
			err = errors.New("送信中です")
		}
		mv.mu.Unlock()
		var blob []byte
		if err == nil {
			var b []byte
			if b, err = a.moveBundle(req.History); err == nil {
				blob, err = move.Seal(rpk, b)
			}
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		go a.moveSend(peer, blob)
		jsonOut(w, map[string]any{"size": len(blob)})
	})
	mux.HandleFunc("/api/move/export", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Pass    string
			History bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		b, err := a.moveBundle(req.History)
		if err == nil {
			b, err = move.SealPass(req.Pass, b)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		jsonOut(w, map[string]any{"data": base64.StdEncoding.EncodeToString(b)})
	})
	mux.HandleFunc("/api/move/import", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Pass, Data string }
		if r.Method != "POST" || json.NewDecoder(http.MaxBytesReader(w, r.Body, 100<<20)).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		blob, err := base64.StdEncoding.DecodeString(req.Data)
		if err != nil {
			http.Error(w, "ファイルが壊れています", 400)
			return
		}
		plain, err := move.OpenPass(req.Pass, blob)
		if err == nil {
			err = a.importBundle(plain)
		}
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		w.Write([]byte("{}"))
	})
}
