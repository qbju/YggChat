// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Native side of the optional Nostr offline delivery (see mbox/nostr.go).
package main

import (
	"context"
	"crypto/ed25519"
	"net/http"
	"net/url"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/h6e/yggchat/mbox"
)

// nostrRelays returns the relays to use, or nil when the feature is off.
func (a *App) nostrRelays() []string {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.locked || !a.store.NostrOn || a.sys {
		return nil
	}
	if len(a.store.NostrRelays) == 0 {
		return mbox.DefaultRelays
	}
	return append([]string{}, a.store.NostrRelays...)
}

func relayDial(ctx context.Context, url string) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, url, nil)
	if err == nil {
		c.SetReadLimit(1 << 20)
	}
	return c, err
}

// relayPublish sends ev to one relay and waits for its OK.
func relayPublish(url, ev string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := relayDial(ctx, url)
	if err != nil {
		return false
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	if c.Write(ctx, websocket.MessageText, []byte(`["EVENT",`+ev+`]`)) != nil {
		return false
	}
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return false
		}
		var m []json.RawMessage
		if json.Unmarshal(b, &m) == nil && len(m) >= 3 && string(m[0]) == `"OK"` {
			return string(m[2]) == "true"
		}
	}
}

// nostrPublish posts ev to every relay in parallel; true if any accepted it.
func nostrPublish(relays []string, ev string) bool {
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := false
	for _, r := range relays {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			if relayPublish(r, ev) {
				mu.Lock()
				ok = true
				mu.Unlock()
			}
		}(r)
	}
	wg.Wait()
	return ok
}

// relayFetch returns the raw events addressed to tag on one relay.
func relayFetch(url, tag string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, err := relayDial(ctx, url)
	if err != nil {
		return nil
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	since := time.Now().Unix() - mbox.NostrTTL
	req := `["REQ","yc",{"kinds":[` + itoa(mbox.NostrKind) + `],"#p":["` + tag + `"],"since":` + itoa(int(since)) + `,"limit":200}]`
	if c.Write(ctx, websocket.MessageText, []byte(req)) != nil {
		return nil
	}
	var out []string
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return out
		}
		var m []json.RawMessage
		if json.Unmarshal(b, &m) != nil || len(m) < 2 {
			continue
		}
		switch string(m[0]) {
		case `"EVENT"`:
			if len(m) >= 3 {
				out = append(out, string(m[2]))
			}
		case `"EOSE"`, `"CLOSED"`:
			_ = c.Write(ctx, websocket.MessageText, []byte(`["CLOSE","yc"]`))
			return out
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// nostrPut parks m on the relays if the feature is on and we know the peer's key.
func (a *App) nostrPut(m Message) bool {
	relays := a.nostrRelays()
	if relays == nil || m.Img != "" {
		return false
	}
	a.store.mu.Lock()
	c := a.store.Contacts[m.Peer]
	a.store.mu.Unlock()
	pk, err := hex.DecodeString(c.PK)
	if err != nil || len(pk) != 32 {
		return false
	}
	wb, _ := json.Marshal(wire{T: "msg", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL})
	ev, err := mbox.NostrWrap(pk, a.privKey(), wb)
	if err != nil {
		return false
	}
	return nostrPublish(relays, ev)
}

var nostrSeen sync.Map // event id -> struct{}

// nostrFetch pulls, opens and deletes everything waiting for us.
func (a *App) nostrFetch() {
	relays := a.nostrRelays()
	if relays == nil {
		return
	}
	tag := mbox.NostrTag(a.pub())
	var mu sync.Mutex
	var wg sync.WaitGroup
	evs := map[string]string{}
	for _, r := range relays {
		wg.Add(1)
		go func(r string) {
			defer wg.Done()
			for _, ev := range relayFetch(r, tag) {
				var e struct{ ID string }
				if json.Unmarshal([]byte(ev), &e) == nil && e.ID != "" {
					mu.Lock()
					evs[e.ID] = ev
					mu.Unlock()
				}
			}
		}(r)
	}
	wg.Wait()
	for id, ev := range evs {
		if _, done := nostrSeen.Load(id); done {
			continue
		}
		from, pk, wb, del, err := mbox.NostrOpen(ev, a.privKey())
		var w wire
		if err != nil || json.Unmarshal(wb, &w) != nil || w.T != "msg" || strings.TrimSpace(from) == "" {
			continue
		}
		a.learnPK(from, hex.EncodeToString(pk))
		if a.handleMsg(from, w) {
			nostrSeen.Store(id, struct{}{})
			go a.sendWire(from, wire{T: "ack", ID: w.ID}) // tell the sender directly if it is online
			if del != "" {
				go nostrPublish(relays, del)
			}
		}
	}
}

func (a *App) nostrLoop() {
	time.Sleep(5 * time.Second)
	for {
		a.nostrFetch()
		time.Sleep(3 * time.Minute)
	}
}

func (a *App) privKey() ed25519.PrivateKey { return ed25519.PrivateKey(a.cfg.PrivateKey) }

// nostrStateLocked: caller holds a.store.mu.
func (a *App) nostrStateLocked() map[string]any {
	relays := a.store.NostrRelays
	if len(relays) == 0 {
		relays = mbox.DefaultRelays
	}
	return map[string]any{"on": a.store.NostrOn, "relays": relays, "defaults": mbox.DefaultRelays}
}

// cleanRelays keeps up to 8 distinct wss:// URLs.
func cleanRelays(in []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, r := range in {
		r = strings.TrimSpace(r)
		u, err := url.Parse(r)
		if err != nil || u.Scheme != "wss" || u.Host == "" || len(r) > 200 || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
		if len(out) == 8 {
			break
		}
	}
	return out
}

func (a *App) nostrHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/api/nostr", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			On     *bool
			Relays []string
			Reset  bool
		}
		if !readJSON(w, r, &req) {
			return
		}
		a.store.mu.Lock()
		turnedOn := false
		if req.On != nil {
			turnedOn = *req.On && !a.store.NostrOn
			a.store.NostrOn = *req.On
		}
		if req.Reset {
			a.store.NostrRelays = nil
		} else if req.Relays != nil {
			a.store.NostrRelays = cleanRelays(req.Relays)
		}
		a.store.save()
		res := a.nostrStateLocked()
		a.store.mu.Unlock()
		if turnedOn {
			go a.nostrFetch()
			go func() {
				for _, m := range a.queued("") {
					if !m.Nostr {
						a.nostrPark(m)
					}
				}
			}()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	})
}
