package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"image"
	"image/png"
	"net"
	"os"
	"testing"
	"time"
)

// pngWithSize builds a tiny valid-header PNG claiming w x h pixels.
func pngWithSize(w, h uint32) []byte {
	var b bytes.Buffer
	b.Write([]byte("\x89PNG\r\n\x1a\n"))
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6
	binary.Write(&b, binary.BigEndian, uint32(13))
	b.WriteString("IHDR")
	b.Write(ihdr)
	binary.Write(&b, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("IHDR"), ihdr...)))
	return b.Bytes()
}

func TestImageBombRefused(t *testing.T) {
	if err := imgDimsOK("image/png", pngWithSize(60000, 60000)); err == nil {
		t.Fatal("60000x60000 PNG accepted")
	}
	var ok bytes.Buffer
	png.Encode(&ok, image.NewRGBA(image.Rect(0, 0, 1600, 1200)))
	if err := imgDimsOK("image/png", ok.Bytes()); err != nil {
		t.Fatal(err)
	}
	// WebP VP8X claiming 16M x 16M
	wp := []byte("RIFF\x00\x00\x00\x00WEBPVP8X\x0a\x00\x00\x00\x00\x00\x00\x00\xff\xff\xff\xff\xff\xff")
	if w, h, good := webpSize(wp); !good || w != 1<<24 || h != 1<<24 {
		t.Fatalf("webp parse %d %d %v", w, h, good)
	}
	if imgDimsOK("image/webp", wp) == nil {
		t.Fatal("huge webp accepted")
	}
	if imgDimsOK("image/webp", []byte("RIFF")) == nil {
		t.Fatal("short webp accepted")
	}
	if _, _, err := parseImg("data:image/png;base64," + base64.StdEncoding.EncodeToString(pngWithSize(20000, 20000))); err == nil {
		t.Fatal("parseImg let a bomb through")
	}
}

// The bomb arriving over the wire is dropped (and acked) instead of reaching the UI.
func TestImageBombOverWire(t *testing.T) {
	a, pc := contactApp(t)
	raw := pngWithSize(50000, 50000)
	sum := sha256.Sum256(raw)
	from := net.ParseIP(tHost)
	a.handlePacket(from, wpkt(t, wire{T: "fh", ID: "f1", Mime: "image/png", Size: len(raw), N: 1, Sha: hex.EncodeToString(sum[:]), TS: time.Now().Unix()}))
	a.handlePacket(from, wpkt(t, wire{T: "fc", ID: "f1", I: 0, D: base64.StdEncoding.EncodeToString(raw)}))
	if len(msgsWith(a, tHost)) != 0 {
		t.Fatal("bomb stored")
	}
	if len(pc.sent(tHost, "ack")) != 1 {
		t.Fatal("bomb not acked")
	}
}

// One stranger used to be able to take all 16 partial-file slots and keep
// photos/long texts from everyone else out.
func TestFileSlotsPerPeer(t *testing.T) {
	a, _ := contactApp(t)
	a.store.PowStrict = false
	for i := 0; i < 40; i++ {
		ip := net.ParseIP(tMem)
		a.handlePacket(ip, wpkt(t, wire{T: "fh", ID: fmt.Sprintf("x%d", i), Mime: "image/png", Size: 100, N: 1, Sha: hex.EncodeToString(make([]byte, 32)), TS: 1, PN: powSolve(tMem+">"+tMe, fmt.Sprintf("x%d", i), 1, "", hex.EncodeToString(make([]byte, 32)), powRequest)}))
	}
	fs := a.files()
	fs.mu.Lock()
	n := len(fs.in)
	fs.mu.Unlock()
	if n != maxInFilesPeer {
		t.Fatalf("one stranger holds %d slots", n)
	}
	a.handlePacket(net.ParseIP(tHost), wpkt(t, wire{T: "fh", ID: "ok1", Mime: "image/png", Size: 100, N: 1, Sha: hex.EncodeToString(make([]byte, 32)), TS: 1}))
	fs.mu.Lock()
	_, ok := fs.in[tHost+"/ok1"]
	fs.mu.Unlock()
	if !ok {
		t.Fatal("contact's photo refused")
	}
}

// The same profile datagram (sent 3x, re-shared every 10 min, or flooded)
// must not rewrite the store each time.
func TestProfRepeatNoRewrite(t *testing.T) {
	a, _ := contactApp(t)
	p := wpkt(t, wire{T: "prof", Nick: "h", Bio: "b"})
	a.handlePacket(net.ParseIP(tHost), p)
	st1, err := os.Stat(a.store.path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	for i := 0; i < 50; i++ {
		a.handlePacket(net.ParseIP(tHost), p)
	}
	st2, _ := os.Stat(a.store.path)
	if !st2.ModTime().Equal(st1.ModTime()) {
		t.Fatal("store rewritten for an unchanged profile")
	}
}

func TestStrangerFloodLimited(t *testing.T) {
	a, _ := contactApp(t)
	reqMu.Lock()
	reqPeer, reqAll = map[string][]int64{}, nil
	reqMu.Unlock()
	from := net.ParseIP(tMem)
	for i := 0; i < 60; i++ {
		id := fmt.Sprintf("s%d", i)
		ts := time.Now().Unix()
		a.handlePacket(from, wpkt(t, wire{T: "msg", ID: id, Text: "spam", TS: ts, PN: powSolve(tMem+">"+tMe, id, ts, "spam", "", powRequest)}))
	}
	if n := len(msgsWith(a, tMem)); n != reqPerMin {
		t.Fatalf("stored %d requests from one stranger in a minute, want %d", n, reqPerMin)
	}
	// An accepted contact is not limited.
	for i := 0; i < 30; i++ {
		a.handlePacket(net.ParseIP(tHost), wpkt(t, wire{T: "msg", ID: fmt.Sprintf("c%d", i), Text: "x", TS: time.Now().Unix()}))
	}
	if n := len(msgsWith(a, tHost)); n != 30 {
		t.Fatalf("contact limited: %d", n)
	}
}

// Late (queued / Nostr) messages are stored in time order.
func TestIncomingStoredInTimeOrder(t *testing.T) {
	a, _ := contactApp(t)
	now := time.Now().Unix()
	from := net.ParseIP(tHost)
	for i, ts := range []int64{now - 10, now - 30, now - 20, now + 99999} {
		a.handlePacket(from, wpkt(t, wire{T: "msg", ID: fmt.Sprintf("o%d", i), Text: "x", TS: ts}))
	}
	ms := msgsWith(a, tHost)
	for i := 1; i < len(ms); i++ {
		if ms[i].TS < ms[i-1].TS {
			t.Fatalf("out of order: %+v", ms)
		}
	}
	if ms[len(ms)-1].TS > now+600 {
		t.Fatal("far-future timestamp kept")
	}
}
