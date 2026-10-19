// Store-and-forward mailbox: when a peer is unreachable, the message is sealed
// to the peer's key (NaCl anonymous box over X25519 converted from Ed25519)
// and parked at the relay for up to 7 days. Only the recipient can open it,
// and the sender identity is inside the box (signed), so the relay learns
// neither content nor sender.
package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/h6e/yggchat/mbox"
)

var mbClient = &http.Client{Timeout: 20 * time.Second}

func mbPost(op string, body any) ([]byte, error) {
	b, _ := json.Marshal(body)
	resp, err := mbClient.Post(mbox.Base+op, "application/json", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("mailbox %s: %d %s", op, resp.StatusCode, buf.String())
	}
	return buf.Bytes(), nil
}

func (a *App) privKey() ed25519.PrivateKey { return ed25519.PrivateKey(a.cfg.PrivateKey) }

func (a *App) mbAuth(op string) map[string]any {
	pk := a.core.PublicKey()
	tag := mbox.Tag(pk)
	ts := time.Now().Unix()
	sig := ed25519.Sign(a.privKey(), []byte(fmt.Sprintf("yggchat-mb-%s:%s:%d", op, tag, ts)))
	return map[string]any{"pk": hex.EncodeToString(pk), "ts": ts, "sig": hex.EncodeToString(sig)}
}

// mbPut parks m at the relay if we know the peer's full key.
func (a *App) mbPut(m Message) bool {
	a.store.mu.Lock()
	c := a.store.Contacts[m.Peer]
	a.store.mu.Unlock()
	pk, err := hex.DecodeString(c.PK)
	if err != nil || len(pk) != ed25519.PublicKeySize {
		return false
	}
	wb, _ := json.Marshal(wire{T: "msg", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL})
	b, err := mbox.Seal(pk, a.privKey(), wb)
	if err != nil {
		return false
	}
	if _, err := mbPost("put", map[string]any{"to": mbox.Tag(pk), "box": b}); err != nil {
		return false
	}
	return true
}

// mbFetch downloads, opens and deletes everything waiting for us.
func (a *App) mbFetch() {
	if a.store.isLocked() {
		return
	}
	body, err := mbPost("get", a.mbAuth("get"))
	if err != nil {
		return
	}
	var res struct {
		Boxes []struct{ K, Box string }
	}
	if json.Unmarshal(body, &res) != nil || len(res.Boxes) == 0 {
		return
	}
	var keys []string
	for _, bx := range res.Boxes {
		keys = append(keys, bx.K)
		from, pk, wb, err := mbox.Open(bx.Box, a.privKey())
		var w wire
		if err != nil || json.Unmarshal(wb, &w) != nil || w.T != "msg" {
			continue
		}
		a.learnPK(from, hex.EncodeToString(pk))
		if a.handleMsg(from, w) {
			a.sendWire(from, wire{T: "ack", ID: w.ID}) // tell the sender directly if it is online
		}
	}
	auth := a.mbAuth("del")
	auth["keys"] = keys
	_, _ = mbPost("del", auth)
}

func (a *App) mbLoop() {
	time.Sleep(3 * time.Second)
	for {
		a.mbFetch()
		time.Sleep(5 * time.Minute)
	}
}

// learnPK records a peer's full public key after checking it matches the address.
func (a *App) learnPK(from, pkHex string) {
	pk, err := hex.DecodeString(pkHex)
	if err != nil || !mbox.AddrOK(from, pk) {
		return
	}
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if c, ok := a.store.Contacts[from]; ok && c.PK != pkHex {
		c.PK = pkHex
		a.store.Contacts[from] = c
		a.store.save()
	}
}
