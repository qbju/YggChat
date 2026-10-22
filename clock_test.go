// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"fmt"
	"testing"
)

func withMono(t *testing.T, now *int64) {
	old := monoMs
	monoMs = func() int64 { return *now }
	t.Cleanup(func() { monoMs = old })
}

// A clock set back by a day left the request limiter full of "future"
// entries: no message request got through for a day.
func TestRequestLimiterSurvivesClockSetBack(t *testing.T) {
	now := int64(10 * 86400000)
	withMono(t, &now)
	reqMu.Lock()
	reqPeer, reqAll = map[string][]int64{}, nil
	reqMu.Unlock()
	for i := 0; i < reqAllMin; i++ {
		strangerOK(fmt.Sprintf("201::%x", i))
	}
	if strangerOK("201::ffff") {
		t.Fatal("limit not reached")
	}
	now -= 86400000 // clock jumps back a day
	if !strangerOK("201::fffe") {
		t.Fatal("requests still refused after the clock was set back")
	}
}

// histAsked must be false for a peer we never asked, also right after start
// (the monotonic clock starts at 0), and expire after histAskTTL.
func TestHistAskedMonotonic(t *testing.T) {
	now := int64(5)
	withMono(t, &now)
	if histAsked("g1", "200::1") {
		t.Fatal("never asked")
	}
	noteHistAsk("g1", "200::1")
	if !histAsked("g1", "200::1") {
		t.Fatal("just asked")
	}
	now += histAskTTL*1000 + 1
	if histAsked("g1", "200::1") {
		t.Fatal("should have expired")
	}
}
