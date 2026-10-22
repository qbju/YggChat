package main

import (
	"testing"
	"time"
)

// Password + approval group: after "password needed", joining again with the
// password must not be undone by the first request. Its retries kept going,
// the host answered them "password needed", which dropped our new join, and
// the approval then arrived as a plain invitation.
func TestJoinRetryOldAttemptStops(t *testing.T) {
	a, pc := tAppPC(t, tMe)
	a.sys = true // no embedded node: hello() sends no key
	mux := tMux(a)
	code := encodeJoin(tHost, tgid, "0011223344556677")
	if rec := tPost(t, mux, "/api/group/join", `{"Code":"`+code+`"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	noPw := func() int {
		n := 0
		for _, w := range pc.sent(tHost, "gj") {
			if w.Pw == "" {
				n++
			}
		}
		return n
	}
	if waitSent(pc, tHost, "gj") == nil {
		t.Fatal("no join request")
	}
	// the host asks for the password; we join again with it
	a.onGroupCtl(tHost, wire{T: "gjn", G: tgid, K: "pw", Text: joinPwNeeded})
	if rec := tPost(t, mux, "/api/group/join", `{"Code":"`+code+`","Pw":"secret"}`); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body)
	}
	before := noPw()
	// a late "password needed" for the old request must not drop the new join
	a.onGroupCtl(tHost, wire{T: "gjn", G: tgid, K: "pw", Text: joinPwNeeded})
	a.store.mu.Lock()
	_, waiting := a.joinHostLocked(tgid)
	a.store.mu.Unlock()
	if !waiting {
		t.Fatal("stale 'password needed' dropped the new join")
	}
	time.Sleep(3500 * time.Millisecond)
	if n := noPw(); n != before {
		t.Fatalf("old attempt kept retrying: %d -> %d", before, n)
	}
	// host: wait for approval (a stale "password needed" after it changes nothing), then approves
	a.onGroupCtl(tHost, wire{T: "gjn", G: tgid, K: "wait", Text: "wait"})
	a.onGroupCtl(tHost, wire{T: "gjn", G: tgid, K: "pw", Text: joinPwNeeded})
	a.onGroupCtl(tHost, wire{T: "gi", G: tgid, GN: "G", GM: []string{tHost, tMe}, GO: tHost, GV: 2})
	if g := a.store.Groups[tgid]; g.Req || g.Left || !hasMember(g, tMe) {
		t.Fatalf("approval became an invitation: %+v", g)
	}
	// a wrong password still ends the wait
	a.store.mu.Lock()
	a.setJoinLocked("1111222233334444", tHost, joinWaitTTL)
	a.store.mu.Unlock()
	joinStart(a, "1111222233334444", true)
	a.onGroupCtl(tHost, wire{T: "gjn", G: "1111222233334444", K: "pw", Text: "パスワードが違います"})
	a.store.mu.Lock()
	_, waiting = a.joinHostLocked("1111222233334444")
	a.store.mu.Unlock()
	if waiting {
		t.Fatal("wrong password did not end the join")
	}
}
