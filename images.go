// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

// Image messages: the UI compresses the picture, the node sends it as a
// header ({t:"fh"}) plus base64 chunks ({t:"fc"}) that the receiver acks one
// by one ({t:"fca"}); once all chunks arrive and the SHA-256 matches, the
// receiver stores the message and sends the normal {t:"ack"}.
package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Every datagram must fit the 1280-byte IPv6 MTU without fragmentation:
// browsers (PWA) build raw IPv6 packets and cannot do fragments.
const (
	imgChunk   = 864        // raw bytes per chunk (1152 base64) → UDP payload < 1232
	imgMaxSize = 600 * 1024 // after the UI's compression
	imgMaxN    = (imgMaxSize + imgChunk - 1) / imgChunk
	capMax     = 800 // caption bytes (UTF-8): the header must also fit one packet
)

var mimeOK = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true, textMime: true}

// textMime: a text too long for one datagram goes the same way as a photo
// (header + chunks), as UTF-8 bytes (v0.19).
const textMime = "text/plain"

const dgramMax = 1200 // any single JSON datagram we send

func wireLen(w wire) int { b, _ := json.Marshal(w); return len(b) }

// escLen is how many bytes s takes inside a JSON string (Go escapes <>& etc.).
func escLen(s string) int { b, _ := json.Marshal(s); return len(b) - 2 }

// longText: m's text has to go chunked.
func (a *App) longText(m Message) bool {
	return m.Img == "" && !m.GImg && wireLen(a.msgWireBare(m)) > dgramMax-40 // room for the nonce
}

const (
	maxSendText = 12288 // bytes we let the UI send (about 4096 Japanese characters)
	maxRecvText = 16384
)

var shaRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// parseImg checks a data: URL and returns its mime type and bytes.
func parseImg(u string) (string, []byte, error) {
	rest, ok := strings.CutPrefix(u, "data:")
	if !ok {
		return "", nil, errors.New("bad image")
	}
	mime, b64, ok := strings.Cut(rest, ";base64,")
	if !ok || !mimeOK[mime] {
		return "", nil, errors.New("unsupported image type")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw) == 0 {
		return "", nil, errors.New("bad image data")
	}
	if len(raw) > imgMaxSize {
		return "", nil, errors.New("image too large (max 600KB after compression)")
	}
	if err := imgDimsOK(mime, raw); err != nil {
		return "", nil, err
	}
	return mime, raw, nil
}

const (
	maxInFiles     = 32 // partial incoming files kept at once (≤ 600 KB each)
	maxInFilesPeer = 4  // of which one peer may hold
	maxInFilesReq  = 12 // and all strangers (not accepted contacts) together
)

type inFile struct {
	hdr    wire
	chunks [][]byte
	got    int
	at     time.Time
	// from someone we haven't accepted (their slots are capped together)
	stranger bool
}

type fileState struct {
	mu   sync.Mutex
	in   map[string]*inFile      // from/id -> partial download
	acks map[string]map[int]bool // id -> chunk acks (outgoing)
	// id -> receiver: only its chunk acks count
	ackFrom map[string]string
}

func (a *App) files() *fileState {
	a.fsOnce.Do(func() {
		a.fs = &fileState{in: map[string]*inFile{}, acks: map[string]map[int]bool{}, ackFrom: map[string]string{}}
	})
	return a.fs
}

func (a *App) deliverImg(m Message, tries int) bool {
	mime, raw, err := parseImg(m.Img)
	if err != nil {
		a.setStatus(m.Peer, m.ID, "failed")
		return false
	}
	return a.deliverFile(m, mime, raw, tries)
}

// fileRound is how long deliverFile waits for acks before the next round.
var fileRound = 2500 * time.Millisecond

func (a *App) deliverFile(m Message, mime string, raw []byte, tries int) bool {
	sum := sha256.Sum256(raw)
	n := (len(raw) + imgChunk - 1) / imgChunk
	hdr := wire{T: "fh", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL, Mime: mime, Size: len(raw), N: n, Sha: hex.EncodeToString(sum[:])}
	if mime == textMime {
		hdr.Text, m.Text = "", "" // the text is the payload
	}
	if m.Grp != "" {
		hdr.G = m.Grp
		a.store.mu.Lock()
		if g, ok := a.store.Groups[m.Grp]; ok {
			hdr.GH = groupHash(g.Name, g.Members)
		}
		a.store.mu.Unlock()
		if m.Sg != "" {
			hdr.Sg, hdr.PK = m.Sg, a.myPK() // signed group post (groupssig.go)
		}
	}
	a.withPow(m, &hdr, hdr.Sha)
	asked := powAsked(m.Peer, m.ID)
	fs := a.files()
	fs.mu.Lock()
	fs.acks[m.ID] = map[int]bool{}
	fs.ackFrom[m.ID] = m.Peer
	fs.mu.Unlock()
	defer func() { fs.mu.Lock(); delete(fs.acks, m.ID); delete(fs.ackFrom, m.ID); fs.mu.Unlock() }()
	ch := a.waitAck(m.Peer, m.ID)
	defer a.doneAck(m.ID, ch)
	for a.udp == nil {
		time.Sleep(200 * time.Millisecond)
	}
	idle, last, redo := 0, -1, false
	for idle < tries {
		if last >= 0 && !m.Hid && !a.stillOut(m.Peer, m.ID) {
			return true // deleted meanwhile
		}
		if x := powAsked(m.Peer, m.ID); x > asked {
			asked, m.Pn = x, ""
			a.withPow(m, &hdr, hdr.Sha)
		}
		a.sendWire(m.Peer, hdr)
		fs.mu.Lock()
		done := len(fs.acks[m.ID])
		var todo []int
		for i := 0; i < n; i++ {
			if !fs.acks[m.ID][i] {
				todo = append(todo, i)
			}
		}
		fs.mu.Unlock()
		if done == last {
			idle++
		} else {
			idle, last = 0, done
		}
		time.Sleep(30 * time.Millisecond) // let the header land first
		for _, i := range todo {
			end := min((i+1)*imgChunk, len(raw))
			a.sendWire(m.Peer, wire{T: "fc", ID: m.ID, I: i, D: base64.StdEncoding.EncodeToString(raw[i*imgChunk : end])})
			if i%8 == 7 {
				time.Sleep(3 * time.Millisecond)
			}
		}
		select {
		case <-ch:
			a.setStatus(m.Peer, m.ID, "sent")
			return true
		case <-time.After(fileRound):
		}
		if !redo && last == n && idle >= 1 {
			// Every chunk was acked but the message wasn't: the receiver had
			// the whole file and couldn't store it (request limit, a failed
			// save), so it dropped it. The header we keep resending opens a
			// new download there, but only the header went out until we gave
			// up. Send the chunks once more.
			redo = true
			fs.mu.Lock()
			fs.acks[m.ID] = map[int]bool{}
			fs.mu.Unlock()
		}
	}
	a.setStatus(m.Peer, m.ID, "queued") // images don't go through the mailbox
	if mime == textMime {
		m.Text = string(raw)
		go a.nostrPark(m) // a long text can (one Nostr event holds it)
	}
	return false
}

func (a *App) onFileAck(from string, w wire) {
	fs := a.files()
	fs.mu.Lock()
	if m, ok := fs.acks[w.ID]; ok && fs.ackFrom[w.ID] == from && w.I >= 0 && w.I < imgMaxN {
		m[w.I] = true
	}
	fs.mu.Unlock()
}

func (a *App) onFileHeader(from string, w wire) {
	if !msgIDRe.MatchString(w.ID) || !mimeOK[w.Mime] || w.N < 1 || w.N > imgMaxN || w.Size < 1 || w.Size > imgMaxSize ||
		(w.Size+imgChunk-1)/imgChunk != w.N || !shaRe.MatchString(w.Sha) || len(w.Text) > capMax || len(w.Nick) > 120 {
		return
	}
	if a.hasMsg(from, w.ID) || (w.G != "" && validGID(w.G) && (a.hasGroupFile(w.G, from, w.ID, w.Mime) || a.groupGone(w.G, from, w.ID) || (w.Text == "" && w.Mime != textMime && a.groupNoFiles(w.G)))) {
		a.sendWire(from, wire{T: "ack", ID: w.ID})
		return
	}
	if w.G != "" && validGID(w.G) {
		if left, quit := a.groupLeft(w.G); left {
			a.sendWire(from, wire{T: "ack", ID: w.ID}) // we're out: the sender can stop (v0.19.2)
			if quit {
				a.leaveNotice(from, w.G)
			}
			return
		}
	}
	if !a.powCheck(from, w) {
		return // told the sender how much work we want
	}
	if w.Mime == textMime && (w.Size > maxRecvText || w.Text != "") {
		return
	}
	a.store.mu.Lock()
	c, ok := a.store.Contacts[from]
	stranger := !ok || c.Req
	a.store.mu.Unlock()
	fs := a.files()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for k, f := range fs.in {
		if time.Since(f.at) > 3*time.Minute {
			delete(fs.in, k)
		}
	}
	key := from + "/" + w.ID
	if _, ok := fs.in[key]; ok || len(fs.in) >= maxInFiles {
		return
	}
	// One peer may only hold a few slots, so a stranger can't keep every
	// slot busy (refreshing them with a chunk now and then) and block photos
	// and long texts from everyone else.
	n, ns := 0, 0
	for k, f := range fs.in {
		if strings.HasPrefix(k, from+"/") {
			n++
		}
		if f.stranger {
			ns++
		}
	}
	if n >= maxInFilesPeer || (stranger && ns >= maxInFilesReq) {
		return
	}
	fs.in[key] = &inFile{hdr: w, chunks: make([][]byte, w.N), at: time.Now(), stranger: stranger}
}

func (a *App) onFileChunk(from string, w wire) {
	fs := a.files()
	key := from + "/" + w.ID
	fs.mu.Lock()
	f, ok := fs.in[key]
	if !ok || w.I < 0 || w.I >= f.hdr.N {
		fs.mu.Unlock()
		return
	}
	if f.chunks[w.I] == nil {
		b, err := base64.StdEncoding.DecodeString(w.D)
		if err != nil || len(b) == 0 || len(b) > imgChunk {
			fs.mu.Unlock()
			return
		}
		f.chunks[w.I] = b
		f.got++
		f.at = time.Now()
	}
	complete := f.got == f.hdr.N
	if complete {
		delete(fs.in, key)
	}
	fs.mu.Unlock()
	a.sendWire(from, wire{T: "fca", ID: w.ID, I: w.I})
	if !complete {
		return
	}
	var raw []byte
	for _, c := range f.chunks {
		raw = append(raw, c...)
	}
	sum := sha256.Sum256(raw)
	if len(raw) != f.hdr.Size || hex.EncodeToString(sum[:]) != f.hdr.Sha {
		return
	}
	h := f.hdr
	h.T = "msg"
	h.checked = true // the header's proof of work was checked
	img := "data:" + h.Mime + ";base64," + base64.StdEncoding.EncodeToString(raw)
	if h.Mime == textMime {
		if !utf8.Valid(raw) {
			return
		}
		h.Text, img = string(raw), ""
	} else if imgDimsOK(h.Mime, raw) != nil {
		// A decompression bomb would crash the UI: drop it, but ack so the
		// sender stops resending it.
		a.sendWire(from, wire{T: "ack", ID: w.ID})
		return
	}
	if h.G != "" {
		if a.onGroupMsg(from, h, img) {
			a.sendWire(from, wire{T: "ack", ID: w.ID})
		}
		return
	}
	if a.acceptMsg(from, h, img) {
		a.sendWire(from, wire{T: "ack", ID: w.ID})
	}
}

func (a *App) hasMsg(from, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	if a.store.isGoneLocked(from, id) {
		return true // unsent / deleted: ack and drop
	}
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if a.store.Messages[i].Peer == from && a.store.Messages[i].ID == id {
			return true
		}
	}
	return false
}
