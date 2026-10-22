package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// fakeRelay is a tiny in-process Nostr relay (NIP-01 EVENT/REQ, NIP-09
// deletions by the event's own key; ignoreDel makes it a relay that keeps
// deleted events).
type fakeRelay struct {
	mu        sync.Mutex
	evs       map[string]map[string]any // id -> event
	dels      int
	ignoreDel bool
	srv       *httptest.Server
}

func newFakeRelay(t *testing.T) *fakeRelay {
	r := &fakeRelay{evs: map[string]map[string]any{}}
	r.srv = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *fakeRelay) url() string { return "ws" + strings.TrimPrefix(r.srv.URL, "http") }

func (r *fakeRelay) count(kind float64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.evs {
		if e["kind"] == kind {
			n++
		}
	}
	return n
}

func (r *fakeRelay) serve(w http.ResponseWriter, req *http.Request) {
	c, err := websocket.Accept(w, req, nil)
	if err != nil {
		return
	}
	defer c.Close(websocket.StatusNormalClosure, "")
	ctx := context.Background()
	for {
		_, b, err := c.Read(ctx)
		if err != nil {
			return
		}
		var m []json.RawMessage
		if json.Unmarshal(b, &m) != nil || len(m) < 2 {
			continue
		}
		var typ string
		json.Unmarshal(m[0], &typ)
		switch typ {
		case "EVENT":
			var e map[string]any
			json.Unmarshal(m[1], &e)
			id, _ := e["id"].(string)
			r.mu.Lock()
			if e["kind"] == float64(5) {
				r.dels++
				if !r.ignoreDel {
					tags, _ := e["tags"].([]any)
					for _, tg := range tags {
						if x, _ := tg.([]any); len(x) >= 2 && x[0] == "e" {
							if old, ok := r.evs[x[1].(string)]; ok && old["pubkey"] == e["pubkey"] {
								delete(r.evs, x[1].(string))
							}
						}
					}
				}
			} else {
				r.evs[id] = e
			}
			r.mu.Unlock()
			c.Write(ctx, websocket.MessageText, []byte(`["OK","`+id+`",true,""]`))
		case "REQ":
			var sub string
			json.Unmarshal(m[1], &sub)
			var f struct {
				P []string `json:"#p"`
			}
			if len(m) > 2 {
				json.Unmarshal(m[2], &f)
			}
			want := map[string]bool{}
			for _, p := range f.P {
				want[p] = true
			}
			r.mu.Lock()
			var out [][]byte
			for _, e := range r.evs {
				tags, _ := e["tags"].([]any)
				for _, tg := range tags {
					if x, _ := tg.([]any); len(x) >= 2 && x[0] == "p" && want[x[1].(string)] {
						eb, _ := json.Marshal(e)
						out = append(out, eb)
						break
					}
				}
			}
			r.mu.Unlock()
			for _, eb := range out {
				c.Write(ctx, websocket.MessageText, []byte(`["EVENT","`+sub+`",`+string(eb)+`]`))
			}
			c.Write(ctx, websocket.MessageText, []byte(`["EOSE","`+sub+`"]`))
		}
	}
}

func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	for i := 0; i < 250; i++ {
		if f() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timeout: " + what)
}

// nostrPair makes y and z contacts with known keys, both using relay r.
func nostrPair(t *testing.T, y, z *App, r *fakeRelay) {
	for _, p := range [][2]*App{{y, z}, {z, y}} {
		a, b := p[0], p[1]
		a.store.mu.Lock()
		a.store.Contacts[b.addr] = Contact{Addr: b.addr, Nick: "x", Shared: true, HasProf: true, PK: b.myPK()}
		a.store.NostrOn, a.store.NostrRelays = true, []string{r.url()}
		a.store.mu.Unlock()
	}
}

func hasMsg1(a *App, peer, id string) bool {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	for _, m := range a.store.Messages {
		if m.Peer == peer && m.ID == id {
			return true
		}
	}
	return false
}

func ctlFor(a *App, peer string) []ctlItem {
	return a.pendingCtl(peer)
}

// A 1:1 message parked on Nostr is recalled when unsent, and the unsend
// itself is parked, so a receiver that comes back while we're away gets it
// even from a relay that ignores deletions.
func TestNostrUnsendRecalls1to1(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		y, _ := tKeyApp(t)
		z, zpc := tKeyApp(t)
		r := newFakeRelay(t)
		r.ignoreDel = ignore
		nostrPair(t, y, z, r)
		m := Message{ID: "18f0000000000a01", Peer: z.addr, From: y.addr, Nick: "y", Text: "secret", TS: time.Now().Unix(), Out: true, Status: "queued"}
		y.addMessage(m)
		y.nostrPark(m)
		if r.count(1059) != 1 {
			t.Fatal("not parked")
		}
		y.store.mu.Lock()
		nev := len(y.store.Messages[len(y.store.Messages)-1].NEv)
		y.store.mu.Unlock()
		if nev != 1 {
			t.Fatal("box not recorded on the message")
		}
		rec := tPost(t, delMux(y), "/api/message/delete", `{"Peer":"`+z.addr+`","ID":"`+m.ID+`","Everyone":true}`)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
		waitFor(t, "recall published", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.dels >= 1 })
		if !ignore && r.count(1059) != 0 {
			t.Fatal("box still on the relay after unsend")
		}
		cs := ctlFor(y, z.addr)
		if len(cs) != 1 || !cs[0].Park {
			t.Fatalf("unsend not marked to park: %+v", cs)
		}
		y.parkCtl(z.addr) // what flushCtl does when z can't be reached
		cs = ctlFor(y, z.addr)
		if len(cs) != 1 || len(cs[0].NEv) != 1 {
			t.Fatalf("unsend not parked: %+v", cs)
		}
		z.nostrFetch()
		if hasMsg1(z, y.addr, m.ID) {
			t.Fatalf("ignoreDel=%v: unsent message shown", ignore)
		}
		z.store.mu.Lock()
		gone := z.store.isGoneLocked(y.addr, m.ID)
		z.store.mu.Unlock()
		if !gone {
			t.Fatal("unsend from the box not applied")
		}
		waitFor(t, "receiver acks the parked unsend", func() bool {
			for _, w := range zpc.sent(y.addr, "ack") {
				if w.ID == cs[0].W.ID {
					return true
				}
			}
			return false
		})
		// y hears the ack: its queue drains and the parked unsend is recalled
		y.lateAck(z.addr, cs[0].W.ID)
		if len(ctlFor(y, z.addr)) != 0 {
			t.Fatal("ctl not drained")
		}
		if !ignore {
			waitFor(t, "all boxes gone", func() bool { return r.count(1059) == 0 })
		}
		waitFor(t, "recall queue empty", func() bool {
			y.store.mu.Lock()
			defer y.store.mu.Unlock()
			return len(y.store.NRecall) == 0
		})
	}
}

// Group: the author's undelivered parked copy is recalled on unsend, the
// unsend is parked for that member only, and a member reading both boxes
// never shows the post.
func TestNostrUnsendRecallsGroup(t *testing.T) {
	for _, ignore := range []bool{false, true} {
		gid, y, z, _, _, _, _, _ := unsendGroup(t)
		r := newFakeRelay(t)
		r.ignoreDel = ignore
		nostrPair(t, y, z, r)
		vis, err := y.sendGroup(gid, "oops group", "")
		if err != nil {
			t.Fatal(err)
		}
		var cp Message
		y.store.mu.Lock()
		for i := range y.store.Messages {
			if x := &y.store.Messages[i]; x.Hid && x.Peer == z.addr && x.Of == vis.ID {
				x.Status = "queued" // z is away
				cp = *x
			}
		}
		y.store.mu.Unlock()
		y.nostrPark(cp)
		if r.count(1059) != 1 {
			t.Fatal("copy not parked")
		}
		rec := tPost(t, delMux(y), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
		if rec.Code != 200 {
			t.Fatal(rec.Code, rec.Body)
		}
		waitFor(t, "recall published", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.dels >= 1 })
		if !ignore && r.count(1059) != 0 {
			t.Fatal("parked copy still on the relay")
		}
		y.store.mu.Lock()
		for _, c := range y.store.Ctl {
			if c.W.G == gid && c.Park != (c.Peer == z.addr) {
				t.Errorf("park flag wrong for %s: %v", c.Peer, c.Park)
			}
		}
		y.store.mu.Unlock()
		y.parkCtl(z.addr)
		z.nostrFetch()
		if hasPost(z, gid, y.addr, vis.ID) {
			t.Fatalf("ignoreDel=%v: unsent group post shown", ignore)
		}
		if !z.groupGone(gid, y.addr, vis.ID) {
			t.Fatal("group unsend from the box not applied")
		}
	}
}

// After the 10-day tombstone is gone, a relay of the post (an old peer keeps
// unsent posts) still can't bring it back: the 90-day hash list holds it.
func TestGroupTombHashOutlivesTomb(t *testing.T) {
	gid, y, z, _, _, ypc, _, _ := unsendGroup(t)
	vis, _ := y.sendGroup(gid, "old secret", "")
	cps := waitSentT(ypc, z.addr, "msg", 30*time.Second) // copy slots may still be busy with earlier tests' retries
	if len(cps) == 0 {
		t.Fatal("no copy sent")
	}
	cp := cps[0]
	z.onGroupMsg(y.addr, cp, "")
	tPost(t, delMux(y), "/api/message/delete", `{"Peer":"g:`+gid+`","ID":"`+vis.ID+`","Everyone":true}`)
	dels := waitSentT(ypc, z.addr, "del", 30*time.Second)
	if len(dels) == 0 {
		t.Fatal("no del sent")
	}
	z.onGroupDel(y.addr, dels[0])
	// 11 days later: the full tombstone expired (and is no longer passed on)
	z.store.mu.Lock()
	for k, tb := range z.store.GDel {
		tb.Exp = time.Now().Unix() - 86400
		z.store.GDel[k] = tb
	}
	nw := len(z.gdelWiresLocked(gid))
	z.store.mu.Unlock()
	if nw != 0 {
		t.Fatal("expired tombstone still passed on")
	}
	noteHistAsk(gid, tMem)
	z.onGroupCtl(tMem, wire{T: "ghs", G: gid, ID: vis.ID, By: y.addr, Text: cp.Text, TS: cp.TS, Sg: cp.Sg, PK: cp.PK})
	if hasPost(z, gid, y.addr, vis.ID) {
		t.Fatal("unsent post resurrected by a relay after the tombstone expired")
	}
	// and the list stays bounded
	z.store.mu.Lock()
	for i := 0; i < gdelHMax+50; i++ {
		z.store.markGDelHLocked(gid+"/x/"+itoa(i), time.Now().Unix())
	}
	n := len(z.store.GDelH)
	z.store.mu.Unlock()
	if n > gdelHMax {
		t.Fatalf("hash list unbounded: %d", n)
	}
}

// A recall that can't reach a relay stays queued (and persisted) and is
// retried later; expired boxes are dropped; the queue is bounded.
func TestNostrRecallRetried(t *testing.T) {
	y, _ := tKeyApp(t)
	r := newFakeRelay(t)
	down := "ws://127.0.0.1:1"
	ref := nostrRef{I: strings.Repeat("ab", 32), K: strings.Repeat("11", 32), R: []string{down, r.url()}, X: time.Now().Unix() + 3600}
	y.nostrRecall([]nostrRef{ref, {I: strings.Repeat("cd", 32), K: strings.Repeat("22", 32), R: []string{down}, X: time.Now().Unix() - 1}})
	waitFor(t, "live relay took it", func() bool { r.mu.Lock(); defer r.mu.Unlock(); return r.dels == 1 })
	waitFor(t, "down relay still pending", func() bool {
		y.store.mu.Lock()
		defer y.store.mu.Unlock()
		return len(y.store.NRecall) == 1 && len(y.store.NRecall[0].R) == 1 && y.store.NRecall[0].R[0] == down
	})
	y.store.mu.Lock()
	y.store.NRecall[0].R = []string{r.url()} // the relay is back (same box)
	y.store.mu.Unlock()
	y.runRecall()
	y.store.mu.Lock()
	n := len(y.store.NRecall)
	var many []nostrRef
	for i := 0; i < nrecallMax+10; i++ {
		many = append(many, nostrRef{I: "x" + itoa(i), K: "k", R: []string{down}, X: time.Now().Unix() + 60})
	}
	y.store.addRecallLocked(many)
	m := len(y.store.NRecall)
	y.store.mu.Unlock()
	if n != 0 || m != nrecallMax {
		t.Fatalf("pending after retry %d, bounded %d", n, m)
	}
}
