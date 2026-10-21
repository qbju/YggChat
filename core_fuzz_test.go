package main

import (
	"encoding/json"
	"net"
	"runtime"
	"testing"
	"time"
)

// discardPC swallows everything a node sends.
type discardPC struct{}

func (discardPC) ReadFrom(b []byte) (int, net.Addr, error) { select {} }
func (discardPC) WriteTo(b []byte, to net.Addr) (int, error) {
	if len(b) > 65507 {
		panic("datagram too large")
	}
	return len(b), nil
}
func (discardPC) Close() error                       { return nil }
func (discardPC) LocalAddr() net.Addr                { return &net.UDPAddr{} }
func (discardPC) SetDeadline(t time.Time) error      { return nil }
func (discardPC) SetReadDeadline(t time.Time) error  { return nil }
func (discardPC) SetWriteDeadline(t time.Time) error { return nil }

func fuzzApp(t *testing.T) *App {
	a := tApp(t, tMe)
	a.sys = true // no embedded core: pub() is nil
	a.udp = discardPC{}
	return a
}

var fuzzSeeds = []string{
	`{"t":"msg","id":"abc","nick":"x","text":"hello","ts":1}`,
	`{"t":"msg","id":"abc","text":"hello","ts":1,"ttl":-5}`,
	`{"t":"msg","id":"abc","text":"hello","ts":-9223372036854775808,"ttl":99999999999}`,
	`{"t":"msg","id":"abc","text":"x","g":"0123456789abcdef","gh":"zz"}`,
	`{"t":"fh","id":"f1","mime":"image/png","size":10,"n":1,"sha":"` + "aa" + `"}`,
	`{"t":"fh","id":"f1","mime":"text/plain","size":5,"n":1,"sha":"2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"}`,
	`{"t":"fc","id":"f1","i":0,"d":"aGVsbG8="}`,
	`{"t":"fc","id":"f1","i":-1,"d":"!!!"}`,
	`{"t":"fca","id":"f1","i":999999999}`,
	`{"t":"pow","id":"abc","n":21}`,
	`{"t":"edit","id":"c1","ref":"abc","text":"changed"}`,
	`{"t":"del","id":"c2","ref":"abc"}`,
	`{"t":"prof","nick":"\u202eevil","bio":"b","av":"x","dom":"a.example","pk":"zz"}`,
	`{"t":"ttl","ttl":300}`,
	`{"t":"hi","pk":"00"}`,
	`{"t":"ack","id":"abc"}`,
	`{"t":"gi","g":"0123456789abcdef","gn":"n","gm":["` + tMe + `","` + tHost + `"],"go":"` + tHost + `","gv":1}`,
	`{"t":"gi","g":"0123456789abcdef","gc":99999999,"gh":"x"}`,
	`{"t":"gp","g":"0123456789abcdef","i":-1,"gmb":"####"}`,
	`{"t":"gd","g":"0123456789abcdef","gv":-1,"ga":"AAAA","gr":"%%"}`,
	`{"t":"ghs","g":"0123456789abcdef","id":"h1","by":"` + tMem + `","text":"t","ts":1}`,
	`{"t":"ghq","g":"0123456789abcdef","ts":-1}`,
	`{"t":"gj","g":"0123456789abcdef","tok":"x","nick":"n"}`,
	`{"t":"gjn","g":"0123456789abcdef","text":"no","k":"pw"}`,
	`{"t":"gl","g":"0123456789abcdef","ts":5}`,
	`{"t":"gq","g":"0123456789abcdef"}`,
	`{"t":"gk","g":"0123456789abcdef","pw":"p1:x:y"}`,
	`{"t":"gkq","g":"0123456789abcdef"}`,
	`{"t":"gi","g":"0123456789abcdef","gs":{"mx":-5,"pb":99,"ttl":-1}}`,
	`[]`, `null`, `{"t":5}`, `{"gm":null,"gs":null}`,
}

// FuzzHandlePacket feeds arbitrary datagrams from two remote addresses (a
// stranger and an accepted contact) into the node. It must never panic.
func FuzzHandlePacket(f *testing.F) {
	for _, s := range fuzzSeeds {
		f.Add([]byte(s), false)
		f.Add([]byte(s), true)
	}
	f.Fuzz(func(t *testing.T, pkt []byte, contact bool) {
		var probe struct{ T string }
		if json.Unmarshal(pkt, &probe) == nil {
			switch probe.T {
			case "fq", "fr", "nq", "nr", "mh", "mc", "mha", "mca", "mok", "mng":
				return // finger/LAN/move need their own state (other areas)
			}
		}
		a := fuzzApp(t)
		from := net.ParseIP(tHost)
		if contact {
			a.store.Contacts[tHost] = Contact{Addr: tHost, Nick: "h", Shared: true, HasProf: true}
			a.store.Groups[tgid] = Group{ID: tgid, Name: "g", Owner: tHost, Members: []string{tMe, tHost, tMem}, V: 1}
		}
		a.handlePacket(from, pkt)
		a.handlePacket(from, pkt) // replays too
	})
}

// A packet must not make the node hold on to huge allocations.
func TestHandlePacketAllocBound(t *testing.T) {
	a := fuzzApp(t)
	var big []byte
	big = append(big, `{"t":"fh","id":"f1","mime":"image/png","size":614400,"n":712,"sha":"`...)
	for i := 0; i < 64; i++ {
		big = append(big, 'a')
	}
	big = append(big, `"}`...)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	for i := 0; i < 200; i++ {
		a.handlePacket(net.ParseIP(tHost), []byte(string(big)))
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	if d := int64(after.HeapAlloc) - int64(before.HeapAlloc); d > 8<<20 {
		t.Fatalf("200 headers kept %d bytes", d)
	}
}

var fuzzTypes = []string{"msg", "fh", "fc", "fca", "pow", "ack", "edit", "del", "prof", "ttl", "hi", "hiack",
	"gq", "gi", "gl", "gj", "gjn", "ghs", "ghq", "gd", "gp", "gk", "gkq"}

// FuzzWireFields builds well-formed packets with hostile field values, which
// reach much deeper into the handlers than random bytes.
func FuzzWireFields(f *testing.F) {
	f.Add(uint8(0), "abc", "hello", int64(1), 0, 0, 0, "", "", "", true, uint8(0))
	f.Add(uint8(1), "f1", "", int64(1), 10, 1, 0, "image/png", "aGVsbG8=", "", false, uint8(3))
	f.Add(uint8(13), "x", "n", int64(-1), -1, 99999, -5, "", "", tgid, true, uint8(2))
	f.Fuzz(func(t *testing.T, ti uint8, id, text string, ts int64, size, n, i int, mime, d, g string, contact bool, gmN uint8) {
		w := wire{T: fuzzTypes[int(ti)%len(fuzzTypes)], ID: id, Text: text, Nick: text, TS: ts, TTL: i, Size: size, N: n, I: i,
			Mime: mime, D: d, Sha: d, Ref: id, G: g, GN: text, GO: tHost, GH: d, Tok: id, By: tMem, GV: n, GSV: i, GC: size,
			GMB: d, GA: d, GR: d, PN: id, K: mime, Pw: d, Bio: text, Av: text, Dom: mime, PK: d}
		for k := 0; k < int(gmN%40); k++ {
			w.GM = append(w.GM, []string{tMe, tHost, tMem, tAdm, text, ""}[k%6])
		}
		if gmN%3 == 1 {
			w.GAD = w.GM
		}
		if gmN%5 == 2 {
			w.GS = &GroupSet{}
			_ = json.Unmarshal([]byte(`{"mx":`+itoa(n)+`,"pb":`+itoa(i)+`,"ttl":`+itoa(size)+`}`), w.GS)
		}
		b, err := json.Marshal(w)
		if err != nil {
			return
		}
		a := fuzzApp(t)
		if contact {
			a.store.Contacts[tHost] = Contact{Addr: tHost, Nick: "h", Shared: true, HasProf: true}
			a.store.Groups[tgid] = Group{ID: tgid, Name: "g", Owner: tHost, Members: []string{tMe, tHost, tMem}, V: 1}
		}
		a.handlePacket(net.ParseIP(tHost), b)
		a.handlePacket(net.ParseIP(tHost), b)
		a.handlePacket(net.ParseIP(tMem), b)
	})
}
