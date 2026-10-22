// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

package main

import (
	"encoding/json"
	"net"
	"os"
	"sort"
	"testing"
	"time"
)

// TestShimDiff runs packet scenarios written by test/pwa/diff.js through the
// native handlePacket and writes the resulting state, so the same scenarios can
// be compared with the PWA shim. Skipped unless SHIMDIFF_IN/SHIMDIFF_OUT are set.
func TestShimDiff(t *testing.T) {
	in, outPath := os.Getenv("SHIMDIFF_IN"), os.Getenv("SHIMDIFF_OUT")
	if in == "" || outPath == "" {
		t.Skip("SHIMDIFF_IN / SHIMDIFF_OUT not set")
	}
	b, err := os.ReadFile(in)
	if err != nil {
		t.Fatal(err)
	}
	var scs []struct {
		Name  string          `json:"name"`
		Me    string          `json:"me"`
		Store json.RawMessage `json:"store"`
		Pkts  []struct {
			From string `json:"from"`
			JSON string `json:"json"`
		} `json:"pkts"`
	}
	if err := json.Unmarshal(b, &scs); err != nil {
		t.Fatal(err)
	}
	type res struct {
		Name  string     `json:"name"`
		Store any        `json:"store"`
		Sent  [][]string `json:"sent"`
	}
	var out []res
	for _, sc := range scs {
		a := tApp(t, sc.Me)
		pc := &fakePC{}
		a.udp = pc
		a.sys = true
		if err := json.Unmarshal(sc.Store, a.store); err != nil {
			t.Fatal(sc.Name, err)
		}
		a.store.fix()
		for _, p := range sc.Pkts {
			a.handlePacket(net.ParseIP(p.From), []byte(p.JSON))
		}
		time.Sleep(60 * time.Millisecond)
		a.store.mu.Lock()
		sb, _ := json.Marshal(a.store)
		a.store.mu.Unlock()
		var st any
		_ = json.Unmarshal(sb, &st)
		pc.mu.Lock()
		var sent [][]string
		for _, p := range pc.pkts {
			sent = append(sent, []string{p.to, p.w.T, p.w.ID})
		}
		pc.mu.Unlock()
		sort.Slice(sent, func(i, j int) bool { return sent[i][0]+sent[i][1]+sent[i][2] < sent[j][0]+sent[j][1]+sent[j][2] })
		out = append(out, res{sc.Name, st, sent})
	}
	ob, _ := json.Marshal(out)
	if err := os.WriteFile(outPath, ob, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A signed post relayed with a time outside the accepted window is stored at
// our time; it must not keep the signature (passed on with the other time, it
// fails every check and receivers drop it as forged). Found by TestShimDiff.
func TestHistoryClampedTimeDropsSig(t *testing.T) {
	gid := "5151000051510001"
	y, _ := tKeyApp(t) // the poster
	z, _ := tKeyApp(t) // the receiver
	ms := cleanMembers([]string{y.addr, z.addr, tMem}, tHost)
	z.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, V: 2}
	ts := time.Now().Unix() + 5000
	sg := y.signPost(gid, "fut1", ts, "hello")
	z.onGroupCtl(tHost, wire{T: "ghs", G: gid, ID: "fut1", By: y.addr, Text: "hello", TS: ts, Sg: sg, PK: y.myPK()})
	got := groupMsgsOf(z, gid)
	if len(got) != 1 || got[0].Sg != "" || got[0].TS >= ts {
		t.Fatalf("clamped relay kept its signature: %+v", got)
	}
	ok := y.signPost(gid, "now1", time.Now().Unix()-5, "fine")
	z.onGroupCtl(tHost, wire{T: "ghs", G: gid, ID: "now1", By: y.addr, Text: "fine", TS: time.Now().Unix() - 5, Sg: ok, PK: y.myPK()})
	if got := groupMsgsOf(z, gid); len(got) != 2 || got[1].Sg != ok {
		t.Fatalf("in-window signed relay lost its signature: %+v", got)
	}
}
