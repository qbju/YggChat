// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"
)

// bigApp loads a store with n messages spread over 200 contacts and 30 groups.
func bigApp(t testing.TB, n int) (*App, *fakePC, []string) {
	a := tApp(t.(*testing.T), tMe)
	pc := &fakePC{}
	a.udp = pc
	a.sys = true
	var cs []string
	for i := 0; i < 200; i++ {
		ad := fmt.Sprintf("201:%x:1:2:3:4:5:%x", i, i+1)
		cs = append(cs, ad)
		a.store.Contacts[ad] = Contact{Addr: ad, Nick: fmt.Sprint("p", i), HasProf: true, Shared: true}
	}
	var gs []string
	for g := 0; g < 30; g++ {
		id := fmt.Sprintf("%016x", g+1)
		ms := append([]string{tMe}, cs[g:g+20]...)
		a.store.Groups[id] = Group{ID: id, Name: "g", Members: ms, Owner: cs[g], V: 3}
		gs = append(gs, id)
	}
	now := time.Now().Unix() - int64(n)*10
	for i := 0; i < n; i++ {
		ts := now + int64(i)*10
		if i%3 == 0 {
			g := gs[i%30]
			a.store.Messages = append(a.store.Messages, Message{ID: fmt.Sprintf("%016x", i), Peer: "g:" + g, From: a.store.Groups[g].Members[1+i%20], Text: "group message text text text text", TS: ts})
		} else {
			p := cs[i%200]
			a.store.Messages = append(a.store.Messages, Message{ID: fmt.Sprintf("%016x", i), Peer: p, From: p, Text: "hello message lorem ipsum dolor sit amet", TS: ts})
		}
	}
	a.store.save()
	return a, pc, cs
}

func TestBigStoreIncoming(t *testing.T) {
	if os.Getenv("YGG_BIG") == "" {
		t.Skip("set YGG_BIG=1")
	}
	a, _, cs := bigApp(t, 50000)
	st := time.Now()
	for i := 0; i < 50; i++ {
		p := cs[i%200]
		a.handlePacket(net.ParseIP(p), wpkt(t, wire{T: "msg", ID: fmt.Sprintf("in%d", i), Text: "new", TS: time.Now().Unix()}))
	}
	t.Logf("50 incoming 1:1: %v per msg", time.Since(st)/50)
}

func TestBigStoreGroupAcks(t *testing.T) {
	if os.Getenv("YGG_BIG") == "" {
		t.Skip("set YGG_BIG=1")
	}
	a, _, _ := bigApp(t, 50000)
	gid := "00000000000000ff"
	ms := append([]string{tMe}, manyAddrs(1023)...)
	a.store.Groups[gid] = Group{ID: gid, Name: "big", Members: ms, Owner: tMe, V: 1, Set: GroupSet{Max: 1024, PoW: -1}}
	// copies as sendGroup makes them
	id := "post1"
	a.store.Messages = append(a.store.Messages, Message{ID: id, Peer: gKey(gid), From: tMe, Out: true, Status: "sending", Text: "x", TS: time.Now().Unix()})
	for i, m := range ms[1:] {
		a.store.Messages = append(a.store.Messages, Message{ID: fmt.Sprintf("%sg%d", id, i), Peer: m, From: tMe, Out: true, Status: "sending", Hid: true, Grp: gid, Of: id, Text: "x"})
	}
	st := time.Now()
	for i, m := range ms[1:] {
		a.setStatus(m, fmt.Sprintf("%sg%d", id, i), "sent")
	}
	t.Logf("1023 copy acks: %v total", time.Since(st))
}
