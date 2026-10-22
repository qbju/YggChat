// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"fmt"
	"testing"
	"time"
)

// Copies for a member that never comes back piled up with every post.
func TestPruneStaleCopies(t *testing.T) {
	a := tApp(t, tMe)
	const dead, live = "209::d", "209::1"
	now := time.Now().Unix()
	add := func(id string, ts int64, reached bool, st string) {
		v := Message{ID: id, Peer: gKey(tgid), From: tMe, Out: true, Status: "queued", TS: ts}
		if reached {
			v.Status, v.Dlv = "sent", 1
		}
		a.store.Messages = append(a.store.Messages, v,
			Message{ID: id + "g0", Peer: dead, From: tMe, Out: true, Status: st, Hid: true, Grp: tgid, Of: id, TS: ts})
	}
	add("old", now-20*86400, true, "queued")        // reached, too old: goes
	add("oldnobody", now-20*86400, false, "queued") // nobody got it: kept
	add("oldfail", now-20*86400, false, "failed")   // failed for good, old: goes
	for i := 0; i < 150; i++ {
		add(fmt.Sprintf("p%03d", i), now-int64(150-i), true, "queued")
	}
	a.store.Messages = append(a.store.Messages, Message{ID: "x", Peer: live, From: tMe, Out: true, Status: "queued", TS: now - 30*86400}) // 1:1: untouched
	n := a.store.pruneStaleCopiesLocked(now)
	if n != 2+50 {
		t.Fatalf("dropped %d", n)
	}
	has := func(id string) bool {
		for _, m := range a.store.Messages {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if has("oldg0") || has("oldfailg0") || !has("oldnobodyg0") || !has("x") || has("p049g0") || !has("p050g0") || !has("p149g0") {
		t.Fatal("wrong copies kept")
	}
	// visible posts are never touched
	if !has("old") || !has("p000") {
		t.Fatal("visible post dropped")
	}
	if a.store.pruneStaleCopiesLocked(now) != 0 {
		t.Fatal("second pass dropped more")
	}
}
