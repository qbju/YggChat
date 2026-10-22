package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"testing"
	"time"

	"github.com/h6e/yggchat/move"
)

const (
	tMvRecv = "201:1111::1"
	tMvSend = "202:2222::2"
	tMvEvil = "203:3333::3"
)

func mvReset() {
	mv.mu.Lock()
	mv.priv, mv.id, mv.from, mv.chunks, mv.got, mv.done, mv.auth = nil, "", "", nil, 0, false, false
	mv.sendID, mv.sendAcks, mv.sendRes, mv.sendHello, mv.sendPeer, mv.sendKey = "", nil, nil, false, "", nil
	mv.mu.Unlock()
}

// mvRecvApp opens a receive screen and returns the node, its sent packets and the code's public key.
func mvRecvApp(t *testing.T) (*App, *fakePC, []byte) {
	t.Cleanup(mvReset)
	mvReset()
	a, pc := tAppPC(t, tMvRecv)
	k, code, err := move.NewReceiver(tMvRecv)
	if err != nil {
		t.Fatal(err)
	}
	_, rpk, err := move.ParseCode(code)
	if err != nil {
		t.Fatal(err)
	}
	mv.mu.Lock()
	mv.priv = k
	mv.mu.Unlock()
	return a, pc, rpk
}

func mvHdr(blob []byte, key []byte) wire {
	sum := sha256.Sum256(blob)
	idb := make([]byte, 8)
	rand.Read(idb)
	w := wire{T: "mh", ID: hex.EncodeToString(idb), Size: len(blob), N: (len(blob) + move.Chunk - 1) / move.Chunk, Sha: hex.EncodeToString(sum[:])}
	if key != nil {
		w.MA = move.HeaderMAC(key, w.ID, w.Size, w.N, w.Sha)
	}
	return w
}

func mvSlot() (id, from string, auth bool) {
	mv.mu.Lock()
	defer mv.mu.Unlock()
	return mv.id, mv.from, mv.auth
}

func mvChunks(a *App, from string, h wire, blob []byte) {
	for i := 0; i < h.N; i++ {
		end := min((i+1)*move.Chunk, len(blob))
		a.onMoveIn(from, wire{T: "mc", ID: h.ID, I: i, D: base64.StdEncoding.EncodeToString(blob[i*move.Chunk : end])})
	}
}

// The v0.19.3 hole: any mesh node could take the receive slot and hog it
// by trickling chunks. A sender holding the code now takes it back, and the
// squatter can neither get it again nor feed chunks into it.
func TestMoveSlotHijack(t *testing.T) {
	a, pc, rpk := mvRecvApp(t)
	key := move.AuthKey(rpk)
	junk := make([]byte, 5000)
	rand.Read(junk)

	// Squatter (no code) takes the free slot like an old sender would.
	evil := mvHdr(junk, nil)
	a.onMoveIn(tMvEvil, evil)
	if id, from, auth := mvSlot(); id != evil.ID || from != tMvEvil || auth {
		t.Fatalf("legacy header should get a free slot: %q %q %v", id, from, auth)
	}
	a.onMoveIn(tMvEvil, wire{T: "mc", ID: evil.ID, I: 0, D: base64.StdEncoding.EncodeToString(junk[:move.Chunk])})

	// A forged proof is dropped, not treated as legacy.
	forged := mvHdr(junk, nil)
	forged.MA = move.HeaderMAC(move.AuthKey(make([]byte, 32)), forged.ID, forged.Size, forged.N, forged.Sha)
	a.onMoveIn(tMvEvil, forged)
	if id, _, _ := mvSlot(); id != evil.ID {
		t.Fatal("forged proof changed the slot")
	}
	if len(pc.sent(tMvEvil, "mha")) != 1 {
		t.Fatal("forged header was answered")
	}

	// The real sender (holds the code) takes the slot over.
	blob := make([]byte, 3000) // undecryptable, so the import fails cleanly instead of restarting
	rand.Read(blob)
	real := mvHdr(blob, key)
	a.onMoveIn(tMvSend, real)
	if id, from, auth := mvSlot(); id != real.ID || from != tMvSend || !auth {
		t.Fatalf("proven sender did not take the slot: %q %q %v", id, from, auth)
	}
	ha := pc.sent(tMvSend, "mha")
	if len(ha) != 1 || !move.MACOK(move.ReplyMAC(key, real.ID), ha[0].MA) {
		t.Fatalf("mha without a valid proof: %+v", ha)
	}
	if ng := pc.sent(tMvEvil, "mng"); len(ng) != 1 || ng[0].ID != evil.ID {
		t.Fatalf("squatter not told: %+v", ng)
	}

	// The squatter can't take it back, with or without a new id, and its chunks don't count.
	a.onMoveIn(tMvEvil, mvHdr(junk, nil))
	a.onMoveIn(tMvEvil, evil)
	mvChunks(a, tMvEvil, real, junk[:len(blob)])
	if id, from, _ := mvSlot(); id != real.ID || from != tMvSend {
		t.Fatal("squatter got the slot back")
	}
	mv.mu.Lock()
	got := mv.got
	mv.mu.Unlock()
	if got != 0 {
		t.Fatalf("squatter's chunks counted: %d", got)
	}

	// The real chunks complete the transfer (and fail at decryption, as this blob is junk).
	mvChunks(a, tMvSend, real, blob)
	ng := pc.sent(tMvSend, "mng")
	if len(ng) != 1 || ng[0].Text != "復号できませんでした" {
		t.Fatalf("transfer not completed by the real sender: %+v", ng)
	}
}

// A proven transfer can't be displaced by an unproven header from anyone,
// and an older (v0.19.3) sender still gets a free slot and an answer.
func TestMoveSlotLegacyCompat(t *testing.T) {
	a, pc, rpk := mvRecvApp(t)
	blob := make([]byte, 2000)
	rand.Read(blob)
	old := mvHdr(blob, nil)
	a.onMoveIn(tMvSend, old)
	ha := pc.sent(tMvSend, "mha")
	if len(ha) != 1 || ha[0].MA != "" {
		t.Fatalf("old sender not answered plainly: %+v", ha)
	}
	a.onMoveIn(tMvSend, old) // repeated header: answered again
	if len(pc.sent(tMvSend, "mha")) != 2 {
		t.Fatal("repeat not answered")
	}
	mvChunks(a, tMvSend, old, blob)
	if ng := pc.sent(tMvSend, "mng"); len(ng) != 1 {
		t.Fatalf("old sender's transfer not finished: %+v", ng)
	}

	// Now a proven transfer; a legacy header from another node is ignored.
	key := move.AuthKey(rpk)
	real := mvHdr(blob, key)
	a.onMoveIn(tMvSend, real)
	a.onMoveIn(tMvEvil, mvHdr(blob, nil))
	if id, from, auth := mvSlot(); id != real.ID || from != tMvSend || !auth {
		t.Fatal("legacy header displaced a proven transfer")
	}
	// The same sender can't downgrade its own proven slot either.
	a.onMoveIn(tMvSend, mvHdr(blob, nil))
	if id, _, _ := mvSlot(); id != real.ID {
		t.Fatal("unproven restart replaced a proven transfer")
	}
	// A stale slot (no progress for a minute) is free again.
	mv.mu.Lock()
	mv.at = time.Now().Add(-2 * time.Minute)
	mv.mu.Unlock()
	h := mvHdr(blob, nil)
	a.onMoveIn(tMvEvil, h)
	if id, _, _ := mvSlot(); id != h.ID {
		t.Fatal("stale slot not freed")
	}
}

// The sender takes answers only from the receiver in the code, and an mha
// with a wrong proof doesn't count.
func TestMoveSenderAcks(t *testing.T) {
	t.Cleanup(mvReset)
	mvReset()
	a, _ := tAppPC(t, tMvSend)
	rpk := make([]byte, 32)
	rand.Read(rpk)
	key := move.AuthKey(rpk)
	mv.mu.Lock()
	mv.sendID, mv.sendAcks, mv.sendPeer, mv.sendKey = "abcdef0123456789", map[int]bool{}, tMvRecv, key
	res := make(chan string, 1)
	mv.sendRes = res
	mv.mu.Unlock()
	a.onMoveAck(tMvEvil, wire{T: "mca", ID: "abcdef0123456789", I: 0})
	a.onMoveAck(tMvEvil, wire{T: "mok", ID: "abcdef0123456789"})
	a.onMoveAck(tMvRecv, wire{T: "mha", ID: "abcdef0123456789", MA: move.ReplyMAC(move.AuthKey(make([]byte, 32)), "abcdef0123456789")})
	mv.mu.Lock()
	hello, acks := mv.sendHello, len(mv.sendAcks)
	mv.mu.Unlock()
	if hello || acks != 0 || len(res) != 0 {
		t.Fatalf("spoofed answers counted: hello=%v acks=%d res=%d", hello, acks, len(res))
	}
	a.onMoveAck(tMvRecv, wire{T: "mha", ID: "abcdef0123456789", MA: move.ReplyMAC(key, "abcdef0123456789")})
	a.onMoveAck(tMvRecv, wire{T: "mca", ID: "abcdef0123456789", I: 3})
	a.onMoveAck(tMvRecv, wire{T: "mha", ID: "abcdef0123456789"}) // v0.19.3 receiver: plain mha
	mv.mu.Lock()
	hello, acks = mv.sendHello, len(mv.sendAcks)
	mv.mu.Unlock()
	if !hello || acks != 1 {
		t.Fatal("real answers not counted")
	}
}

// The header a new sender puts on the wire carries the proof (and older
// receivers just ignore the extra field).
func TestMoveSenderHeaderProof(t *testing.T) {
	t.Cleanup(mvReset)
	mvReset()
	a, pc := tAppPC(t, tMvSend)
	rpk := make([]byte, 32)
	rand.Read(rpk)
	blob := make([]byte, 1000)
	rand.Read(blob)
	go a.moveSend(tMvRecv, rpk, blob)
	deadline := time.Now().Add(3 * time.Second)
	for len(pc.sent(tMvRecv, "mh")) == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	mv.mu.Lock()
	mv.sendID = "" // cancel
	mv.mu.Unlock()
	hs := pc.sent(tMvRecv, "mh")
	if len(hs) == 0 {
		t.Fatal("no header sent")
	}
	h := hs[0]
	if !move.MACOK(move.HeaderMAC(move.AuthKey(rpk), h.ID, h.Size, h.N, h.Sha), h.MA) {
		t.Fatalf("header proof invalid: %+v", h)
	}
	time.Sleep(800 * time.Millisecond) // let the sender loop see the cancel
}
