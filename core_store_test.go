package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A damaged chat.json used to be silently replaced by an empty store on the
// first save: every chat gone.
func TestCorruptStoreIsBackedUp(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chat.json")
	bad := []byte(`{"nick":"me","contacts":{"x":{"addr":"x"}},"messages":[{"id":"1","te`)
	if err := os.WriteFile(p, bad, 0600); err != nil {
		t.Fatal(err)
	}
	s := &Store{path: p}
	s.load()
	s.fix()
	s.save()
	m, _ := filepath.Glob(p + ".corrupt-*")
	if len(m) != 1 {
		t.Fatalf("no backup of the damaged store: %v", m)
	}
	if b, _ := os.ReadFile(m[0]); string(b) != string(bad) {
		t.Fatalf("backup differs")
	}
}

// Locking must encrypt the history we hold now, not whatever is on disk.
func TestLockNowUsesMemoryNotDisk(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chat.json")
	s := &Store{path: p}
	s.fix()
	if err := s.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.Messages = append(s.Messages, Message{ID: "new1", Peer: tHost, Text: "late"})
	s.dirty = true // e.g. a saveSoon still pending
	s.mu.Unlock()
	if err := s.lockNow(); err != nil {
		t.Fatal(err)
	}
	if err := s.unlock("pass1234"); err != nil {
		t.Fatal(err)
	}
	if len(s.Messages) != 1 || s.Messages[0].ID != "new1" {
		t.Fatalf("message lost across lock/unlock: %+v", s.Messages)
	}
}

// A failed write must not leave a passcode "set" in memory while the file
// on disk is still plaintext (or the old passcode).
func TestSetPassFailsLoudly(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "chat.json")
	s := &Store{path: p}
	s.fix()
	s.save()
	os.Chmod(dir, 0500)
	defer os.Chmod(dir, 0700)
	err := s.setPass("", "pass1234")
	if err == nil {
		t.Fatal("setPass reported success though nothing was written")
	}
	if s.key != nil {
		t.Fatal("key kept after a failed save")
	}
	if b, _ := os.ReadFile(p); strings.Contains(string(b), `"ct"`) {
		t.Fatal("file changed?")
	}
	if err := s.lockNow(); err == nil {
		t.Fatal("lock without passcode")
	}
}

func TestWrongPasscodeAndTamper(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chat.json")
	s := &Store{path: p}
	s.fix()
	s.Nick = "me"
	if err := s.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	if err := s.lockNow(); err != nil {
		t.Fatal(err)
	}
	if s.unlock("wrong") == nil {
		t.Fatal("wrong passcode unlocked")
	}
	if !s.isLocked() || s.Nick != "" {
		t.Fatal("state leaked after a wrong passcode")
	}
	// Flip a ciphertext byte on disk: a fresh load must refuse it.
	b, _ := os.ReadFile(p)
	i := strings.Index(string(b), `"ct":"`) + 10
	if b[i] == 'A' { // stay valid base64, change the bytes
		b[i] = 'B'
	} else {
		b[i] = 'A'
	}
	os.WriteFile(p, b, 0600)
	s2 := &Store{path: p}
	s2.load()
	if !s2.locked || s2.unlock("pass1234") == nil {
		t.Fatal("tampered store opened")
	}
	if err := s.unlock("pass1234"); err != nil || s.Nick != "me" {
		t.Fatalf("unlock: %v %q", err, s.Nick)
	}
}

// An envelope damaged so badly it doesn't parse must not be taken for an
// empty plaintext store and overwritten.
func TestBrokenEnvelopeKept(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "chat.json")
	s := &Store{path: p}
	s.fix()
	if err := s.setPass("", "pass1234"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(p)
	i := strings.Index(string(b), `"ct":"`) + 10
	b[i] = '!'
	os.WriteFile(p, b, 0600)
	s2 := &Store{path: p}
	s2.load()
	s2.fix()
	s2.save()
	m, _ := filepath.Glob(p + ".corrupt-*")
	if len(m) != 1 {
		t.Fatal("damaged encrypted store overwritten without a copy")
	}
}
