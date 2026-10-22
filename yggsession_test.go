package main

// Regression test for the Yggdrasil (ironwood) session fixes in
// third_party/ironwood/encrypted/session.go: a node that restarts with the
// same key while a peer keeps sending to it, over a slow link, must be
// reachable both ways again within seconds and stay reachable. Before the
// fix the handshake livelocked (same-second init seqs dropped, throwaway
// keys adopted, key rotations outrunning the traffic in flight) and
// datagrams were lost for minutes.

import (
	"crypto/ed25519"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Arceliar/ironwood/encrypted"
	"github.com/Arceliar/ironwood/types"
)

// lagConn delivers every write after a fixed delay, in order.
type lagConn struct {
	net.Conn
	lag  time.Duration
	q    chan lagMsg
	once sync.Once
}

type lagMsg struct {
	at time.Time
	b  []byte
}

func newLagConn(c net.Conn, d time.Duration) net.Conn {
	if d == 0 {
		return c
	}
	l := &lagConn{Conn: c, lag: d, q: make(chan lagMsg, 4096)}
	go func() {
		for m := range l.q {
			time.Sleep(time.Until(m.at))
			if _, err := l.Conn.Write(m.b); err != nil {
				return
			}
		}
	}()
	return l
}

func (l *lagConn) Write(b []byte) (int, error) {
	select {
	case l.q <- lagMsg{time.Now().Add(l.lag), append([]byte(nil), b...)}:
	default: // full: drop, like a congested link
	}
	return len(b), nil
}

func (l *lagConn) Close() error {
	l.once.Do(func() { close(l.q) })
	return l.Conn.Close()
}

type sessNode struct {
	pc   *encrypted.PacketConn
	got  sync.Map // payload -> time received
	recv atomic.Int64
}

func startSessNode(t *testing.T, priv ed25519.PrivateKey) *sessNode {
	pc, err := encrypted.NewPacketConn(priv)
	if err != nil {
		t.Fatal(err)
	}
	n := &sessNode{pc: pc}
	go func() {
		buf := make([]byte, 2048)
		for {
			k, _, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			n.got.Store(string(buf[:k]), time.Now())
			n.recv.Add(1)
		}
	}()
	return n
}

// link connects a and b through a pipe with one-way delay d on each side.
func sessLink(a, b *sessNode, d time.Duration) (net.Conn, net.Conn) {
	ca, cb := net.Pipe()
	la, lb := newLagConn(ca, d), newLagConn(cb, d)
	ka := a.pc.PrivateKey().Public().(ed25519.PublicKey)
	kb := b.pc.PrivateKey().Public().(ed25519.PublicKey)
	go func() { _ = a.pc.HandleConn(kb, la, 0) }()
	go func() { _ = b.pc.HandleConn(ka, lb, 0) }()
	return la, lb
}

func TestYggSessionRestartSameKey(t *testing.T) {
	if testing.Short() {
		t.Skip("live ironwood test (~25 s)")
	}
	_, privH, _ := ed25519.GenerateKey(nil)
	_, privX, _ := ed25519.GenerateKey(nil)
	pubR, privR, _ := ed25519.GenerateKey(nil)
	h, x := startSessNode(t, privH), startSessNode(t, privX)
	defer h.pc.Close()
	defer x.pc.Close()
	// X's link to the hub is slow (0.7 s each way, like an overloaded
	// host); YGGSESS_LAG_MS overrides it. Over ~1 s the peering itself
	// times out.
	lag := 700 * time.Millisecond
	if v := os.Getenv("YGGSESS_LAG_MS"); v != "" {
		ms, _ := strconv.Atoi(v)
		lag = time.Duration(ms) * time.Millisecond
	}
	sessLink(h, x, lag)
	r := startSessNode(t, privR)
	_, rConn := sessLink(h, r, 0)
	pubX := privX.Public().(ed25519.PublicKey)

	// X sends to R every 100 ms, R to X every 200 ms, all along.
	var seqX, seqR atomic.Int64
	var rMu sync.Mutex
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			_, _ = x.pc.WriteTo([]byte(fmt.Sprintf("x%d", seqX.Add(1))), types.Addr(pubR))
		}
	}()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(200 * time.Millisecond):
			}
			rMu.Lock()
			pc := r.pc
			rMu.Unlock()
			_, _ = pc.WriteTo([]byte(fmt.Sprintf("r%d", seqR.Add(1))), types.Addr(pubX))
		}
	}()
	waitGot := func(n *sessNode, prefix byte, after int64, within time.Duration) bool {
		end := time.Now().Add(within)
		for time.Now().Before(end) {
			ok := false
			n.got.Range(func(k, _ any) bool {
				s := k.(string)
				var v int64
				if s[0] == prefix && func() bool { _, e := fmt.Sscanf(s[1:], "%d", &v); return e == nil }() && v > after {
					ok = true
					return false
				}
				return true
			})
			if ok {
				return true
			}
			time.Sleep(100 * time.Millisecond)
		}
		return false
	}
	if !waitGot(r, 'x', 0, 15*time.Second) || !waitGot(x, 'r', 0, 15*time.Second) {
		t.Fatalf("no traffic before the restart (r got %d, x got %d, h peers %d, x peers %d)", r.recv.Load(), x.recv.Load(), len(h.pc.PacketConn.Debug.GetPeers()), len(x.pc.PacketConn.Debug.GetPeers()))
	}

	// R goes away for 4 s while X keeps sending, then comes back with the same key.
	_ = r.pc.Close()
	_ = rConn.Close()
	time.Sleep(4 * time.Second)
	r2 := startSessNode(t, privR)
	defer r2.pc.Close()
	rMu.Lock()
	r.pc = r2.pc
	rMu.Unlock()
	back := time.Now()
	sessLink(h, r2, 0)
	fromX, fromR := seqX.Load(), seqR.Load()
	okR := waitGot(r2, 'x', fromX, 10*time.Second)
	tR := time.Since(back)
	okX := waitGot(x, 'r', fromR, 10*time.Second)
	tX := time.Since(back)
	t.Logf("after restart: X->R %v (%v), R->X %v (%v)", okR, tR.Round(100*time.Millisecond), okX, tX.Round(100*time.Millisecond))
	if !okR || !okX {
		t.Fatalf("restarted node not reachable within 10 s (X->R %v, R->X %v)", okR, okX)
	}
	// And it stays reachable: most of X's next 5 s of traffic reaches R.
	time.Sleep(2 * time.Second)
	a0, n0 := r2.recv.Load(), seqX.Load()
	time.Sleep(5 * time.Second)
	a1, n1 := r2.recv.Load(), seqX.Load()
	// r2.recv also counts nothing else: only X sends to R.
	rate := float64(a1-a0) / float64(n1-n0)
	t.Logf("steady X->R delivery %.0f%% (%d/%d)", rate*100, a1-a0, n1-n0)
	if rate < 0.8 {
		t.Fatalf("X->R delivery after the restart only %.0f%%: the session keeps re-keying", rate*100)
	}
}
