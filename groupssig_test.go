package main

import (
	"crypto/ed25519"
	"net"
	"testing"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
)

// tKeyApp is a test node with a real key (so it signs) and its address.
func tKeyApp(t *testing.T) (*App, *fakePC) {
	cfg := config.GenerateConfig()
	pub := ed25519.PrivateKey(cfg.PrivateKey).Public().(ed25519.PublicKey)
	addr := net.IP(address.AddrForKey(pub)[:]).String()
	a, pc := tAppPC(t, addr)
	a.cfg = cfg
	return a, pc
}

// A relaying member could make up a post "by" another member; signed posts
// make relays checkable: a forged signature is dropped, a good one shows
// unmarked, and an unsigned relay of a signer's post is marked even from an admin.
func TestSignedRelays(t *testing.T) {
	gid := "5151000051510000"
	y, ypc := tKeyApp(t) // the poster
	z, _ := tKeyApp(t)   // the receiver
	ms := cleanMembers([]string{y.addr, z.addr, tMem, tAdm}, tHost)
	for _, a := range []*App{y, z} {
		a.store.Groups[gid] = Group{ID: gid, Name: "G", Members: ms, Owner: tHost, Admins: []string{tAdm}, V: 2}
	}
	vis, err := y.sendGroup(gid, "hello from y", "")
	if err != nil || vis.Sg == "" {
		t.Fatalf("not signed: %v %+v", err, vis)
	}
	var cp *wire
	for i := 0; i < 100 && cp == nil; i++ {
		if ws := ypc.sent(z.addr, "msg"); len(ws) > 0 {
			cp = &ws[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	if cp == nil || cp.Sg == "" || cp.PK == "" {
		t.Fatalf("copy without signature: %+v", cp)
	}
	// z gets y's post relayed by tMem (asked), the true one
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: postID(cp.ID), By: y.addr, Text: cp.Text, TS: cp.TS, Sg: cp.Sg, PK: cp.PK})
	got := groupMsgsOf(z, gid)
	if len(got) != 1 || got[0].Via != "" || got[0].Sg == "" {
		t.Fatalf("signed relay: %+v", got)
	}
	// the same signature on altered text: dropped
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "abc123", By: y.addr, Text: "altered", TS: cp.TS, Sg: cp.Sg, PK: cp.PK})
	// a made-up key for y: dropped
	other, _ := tKeyApp(t)
	fake := other.signPost(gid, "abc124", cp.TS, "fake")
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: "abc124", By: y.addr, Text: "fake", TS: cp.TS, Sg: fake, PK: other.myPK()})
	if n := len(groupMsgsOf(z, gid)); n != 1 {
		t.Fatalf("forged relays taken: %d", n)
	}
	// y's direct post marks it as a signer: an admin's unsigned relay "by" y is marked
	z.onGroupMsg(y.addr, *cp, "")
	if z.store.Groups[gid].Keys[y.addr] == "" {
		t.Fatal("signer not noted")
	}
	z.onGroupCtl(tAdm, wire{T: "ghs", G: gid, ID: "abc125", By: y.addr, Text: "unsigned", TS: cp.TS})
	got = groupMsgsOf(z, gid)
	if len(got) != 2 || got[1].Via != tAdm {
		t.Fatalf("admin's unsigned relay of a signer: %+v", got)
	}
	// and z passes y's proof on when it relays
	z.store.mu.Lock()
	sg, pk := z.sigForLocked(z.store.Groups[gid], got[0])
	z.store.mu.Unlock()
	if sg == "" || pk != cp.PK {
		t.Fatalf("relay proof: %q %q", sg, pk)
	}
	// on the wire: a catch-up answer carries it
	zz, zpc := tKeyApp(t)
	zz.store.Groups[gid] = Group{ID: gid, Name: "G", Members: cleanMembers(ms, zz.addr), Owner: tHost, V: 2, Keys: map[string]string{y.addr: cp.PK}}
	zz.store.Messages = append(zz.store.Messages, Message{ID: postID(cp.ID), Peer: gKey(gid), From: y.addr, Text: cp.Text, TS: cp.TS, Sg: cp.Sg})
	zz.onGroupCtl(tMem, wire{T: "ghq", G: gid, TS: 1})
	ws := waitSentT(zpc, tMem, "ghs", 5*time.Second)
	if len(ws) == 0 || ws[0].Sg != cp.Sg || ws[0].PK != cp.PK {
		t.Fatalf("ghs without proof: %+v", ws)
	}
}

func waitSentT(pc *fakePC, to, typ string, d time.Duration) []wire {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if ws := pc.sent(to, typ); len(ws) > 0 {
			return ws
		}
		time.Sleep(50 * time.Millisecond)
	}
	return nil
}
