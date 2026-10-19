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
	"errors"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Every datagram must fit the 1280-byte IPv6 MTU without fragmentation:
// browsers (PWA) build raw IPv6 packets and cannot do fragments.
const (
	imgChunk   = 864        // raw bytes per chunk (1152 base64) → UDP payload < 1232
	imgMaxSize = 600 * 1024 // after the UI's compression
	imgMaxN    = (imgMaxSize + imgChunk - 1) / imgChunk
	capMax     = 800 // caption bytes (UTF-8): the header must also fit one packet
)

var mimeOK = map[string]bool{"image/jpeg": true, "image/png": true, "image/webp": true, "image/gif": true}
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
	return mime, raw, nil
}

type inFile struct {
	hdr    wire
	chunks [][]byte
	got    int
	at     time.Time
}

type fileState struct {
	mu   sync.Mutex
	in   map[string]*inFile      // from/id -> partial download
	acks map[string]map[int]bool // id -> chunk acks (outgoing)
}

func (a *App) files() *fileState {
	a.fsOnce.Do(func() { a.fs = &fileState{in: map[string]*inFile{}, acks: map[string]map[int]bool{}} })
	return a.fs
}

func (a *App) deliverImg(m Message, tries int) bool {
	mime, raw, err := parseImg(m.Img)
	if err != nil {
		a.setStatus(m.Peer, m.ID, "failed")
		return false
	}
	sum := sha256.Sum256(raw)
	n := (len(raw) + imgChunk - 1) / imgChunk
	hdr := wire{T: "fh", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS, TTL: m.TTL, Mime: mime, Size: len(raw), N: n, Sha: hex.EncodeToString(sum[:])}
	fs := a.files()
	fs.mu.Lock()
	fs.acks[m.ID] = map[int]bool{}
	fs.mu.Unlock()
	defer func() { fs.mu.Lock(); delete(fs.acks, m.ID); fs.mu.Unlock() }()
	ch := make(chan struct{})
	a.ackMu.Lock()
	a.acks[m.ID] = ch
	a.ackMu.Unlock()
	defer func() { a.ackMu.Lock(); delete(a.acks, m.ID); a.ackMu.Unlock() }()
	for a.udp == nil {
		time.Sleep(200 * time.Millisecond)
	}
	idle, last := 0, -1
	for idle < tries {
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
		case <-time.After(2500 * time.Millisecond):
		}
	}
	a.setStatus(m.Peer, m.ID, "queued") // images don't go through the mailbox
	return false
}

func (a *App) onFileAck(w wire) {
	fs := a.files()
	fs.mu.Lock()
	if m, ok := fs.acks[w.ID]; ok {
		m[w.I] = true
	}
	fs.mu.Unlock()
}

func (a *App) onFileHeader(from string, w wire) {
	if !msgIDRe.MatchString(w.ID) || !mimeOK[w.Mime] || w.N < 1 || w.N > imgMaxN || w.Size < 1 || w.Size > imgMaxSize ||
		(w.Size+imgChunk-1)/imgChunk != w.N || !shaRe.MatchString(w.Sha) || len(w.Text) > capMax || len(w.Nick) > 120 {
		return
	}
	if a.hasMsg(from, w.ID) {
		a.sendWire(from, wire{T: "ack", ID: w.ID})
		return
	}
	fs := a.files()
	fs.mu.Lock()
	defer fs.mu.Unlock()
	for k, f := range fs.in {
		if time.Since(f.at) > 3*time.Minute {
			delete(fs.in, k)
		}
	}
	key := from + "/" + w.ID
	if _, ok := fs.in[key]; ok || len(fs.in) >= 16 {
		return
	}
	fs.in[key] = &inFile{hdr: w, chunks: make([][]byte, w.N), at: time.Now()}
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
	if a.acceptMsg(from, h, "data:"+h.Mime+";base64,"+base64.StdEncoding.EncodeToString(raw)) {
		a.sendWire(from, wire{T: "ack", ID: w.ID})
	}
}

func (a *App) hasMsg(from, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if a.store.Messages[i].Peer == from && a.store.Messages[i].ID == id {
			return true
		}
	}
	return false
}
