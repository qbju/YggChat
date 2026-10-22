// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

// Store journal (r2): an incoming message used to rewrite (and fsync) the
// whole chat.json before it was acked. With a long history that is many MB
// per message: 50k messages = ~12 MB, ~35 ms of JSON under the store lock
// and 12 MB of flash writes for every message received.
//
// Now an incoming message is appended to <chat.json>.journal (one line,
// fsynced, encrypted like the store when a passcode is set) and acked; the
// full file is rewritten at most every journalEvery, or after journalMax
// entries / journalMaxBytes, and on shutdown. Any full save starts a new
// generation (Store.JGen) and drops the journal, so a journal line counts
// only while chat.json carries its generation: a crash between the two
// writes, or an older version rewriting chat.json without "jgen" (a
// downgrade), leaves stale lines that are ignored.

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	journalEvery    = 30 * time.Second
	journalMax      = 256
	journalMaxBytes = 4 << 20
)

type jrec struct {
	G string   `json:"g"`
	M *Message `json:"m,omitempty"`
	C *Contact `json:"c,omitempty"` // the message's contact entry when it changed (new request, nick)
	N []byte   `json:"n,omitempty"` // encrypted record: nonce
	X []byte   `json:"x,omitempty"` // ... and AES-GCM(json of {m,c})
}

func (s *Store) journalPath() string { return s.path + ".journal" }

func newJGen() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

const journalAD = "yggchat-journal-v1"

func jseal(key, plain []byte) (nonce, ct []byte, err error) {
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
	return nonce, g.Seal(nil, nonce, plain, []byte(journalAD)), nil
}

func jopen(key, nonce, ct []byte) ([]byte, error) {
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(blk)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, []byte(journalAD))
}

// writeSnapshotLocked writes the whole store as a new generation and drops
// the journal. Caller holds s.mu; the store is unlocked.
func (s *Store) writeSnapshotLocked() ([]byte, error) {
	old := s.JGen
	s.JGen = newJGen()
	b, err := s.encode()
	if err == nil {
		err = writeAtomic(s.path, b)
	}
	if err != nil {
		s.JGen = old // the journal still belongs to the file on disk
		return nil, err
	}
	s.jn, s.jbytes = 0, 0
	if s.jT != nil {
		s.jT.Stop()
		s.jT = nil
	}
	if err := os.Remove(s.journalPath()); err != nil && !os.IsNotExist(err) {
		log.Printf("store: journal: %v", err) // stale lines are ignored (other generation)
	}
	return b, nil
}

// journalLocked makes m (and its contact entry c, when it changed) durable
// without rewriting the store. Falls back to a full save when there is no
// generation yet or the journal can't be written. Caller holds s.mu.
func (s *Store) journalLocked(m Message, c *Contact) error {
	if s.locked {
		return errLocked
	}
	if s.JGen == "" || s.jn >= journalMax || s.jbytes >= journalMaxBytes {
		return s.saveErr()
	}
	rec := jrec{G: s.JGen}
	body, err := json.Marshal(jrec{M: &m, C: c})
	if err != nil {
		return s.saveErr()
	}
	if s.key != nil {
		if rec.N, rec.X, err = jseal(s.key, body); err != nil {
			return s.saveErr()
		}
	} else {
		rec.M, rec.C = &m, c
	}
	line, _ := json.Marshal(rec)
	line = append(line, '\n')
	p := s.journalPath()
	_, statErr := os.Stat(p)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err == nil {
		if _, err = f.Write(line); err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil && os.IsNotExist(statErr) {
		// A new file: its directory entry must be durable too.
		if d, derr := os.Open(filepath.Dir(p)); derr == nil {
			_ = d.Sync()
			d.Close()
		}
	}
	if err != nil {
		log.Printf("store: journal: %v; saving the whole store", err)
		return s.saveErr()
	}
	s.jn++
	s.jbytes += int64(len(line))
	s.dirty = true
	if s.jT == nil {
		s.jT = time.AfterFunc(journalEvery, func() {
			s.mu.Lock()
			s.jT = nil
			if s.dirty && !s.locked {
				s.save()
			}
			s.mu.Unlock()
		})
	}
	return nil
}

// replayJournalLocked adds what the journal holds for the current generation
// (messages not in the store yet, their contacts). Caller holds s.mu; the
// store is unlocked and loaded.
func (s *Store) replayJournalLocked() {
	if s.JGen == "" {
		return
	}
	b, err := os.ReadFile(s.journalPath())
	if err != nil {
		return
	}
	have := map[string]bool{}
	for _, m := range s.Messages {
		have[jkey(m)] = true
	}
	n := 0
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64<<10), 8<<20)
	for sc.Scan() {
		var r jrec
		if json.Unmarshal(sc.Bytes(), &r) != nil || r.G != s.JGen {
			continue // a torn last line, or an older generation
		}
		if len(r.X) > 0 {
			if s.key == nil {
				continue
			}
			plain, err := jopen(s.key, r.N, r.X)
			if err != nil {
				continue
			}
			var in jrec
			if json.Unmarshal(plain, &in) != nil {
				continue
			}
			r.M, r.C = in.M, in.C
		} else if s.key != nil {
			continue // never take plaintext lines into an encrypted store
		}
		if r.C != nil && r.C.Addr != "" {
			s.Contacts[r.C.Addr] = *r.C
		}
		if r.M == nil || have[jkey(*r.M)] {
			continue
		}
		m := *r.M
		have[jkey(m)] = true
		if !m.Out && m.Sys == "" && !isGroupKey(m.Peer) {
			s.insertByTimeLocked(m)
		} else {
			s.Messages = append(s.Messages, m)
		}
		n++
	}
	if n > 0 {
		log.Printf("store: %d message(s) restored from the journal", n)
	}
	if len(b) > 0 {
		s.save() // fold it in (or drop stale lines)
	}
}

// flushOnSignal writes pending changes (journal, debounced saves) before the
// process ends on SIGTERM/Ctrl-C (Android stopping the service, a desktop
// shutdown): the next start then needs no journal, and an older version
// started on this folder sees everything.
func flushOnSignal(s *Store) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		s.mu.Lock()
		if s.dirty && !s.locked {
			s.save()
		}
		os.Exit(0)
	}()
}

// jkey identifies a stored message the way addMessage dedupes it.
func jkey(m Message) string {
	if isGroupKey(m.Peer) && !m.Out && m.Sys == "" {
		return m.Peer + "|" + m.From + "|" + postID(m.ID)
	}
	return m.Peer + "|" + m.ID
}
