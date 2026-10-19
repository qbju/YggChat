// YggChat: serverless P2P chat over Yggdrasil.
// Every node gets a global, stable IPv6 (200::/7) derived from its key,
// so peers dial each other directly. No NAT traversal, no server.
// Yggdrasil runs embedded in userspace (yggstack netstack): no TUN, no root.
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	gologme "github.com/gologme/log"
	"github.com/yggdrasil-network/yggdrasil-go/src/config"
	"github.com/yggdrasil-network/yggdrasil-go/src/core"
	"github.com/yggdrasil-network/yggstack/src/netstack"
)

//go:embed web
var webFS embed.FS

const chatPort = 7777

// Recommended peers: Japan/Asia first, then well-known EU nodes as backbone.
var defaultPeers = []string{
	"tls://yg-tyo.magicum.net:32333",
	"tls://133.18.201.69:54232",
	"tls://yg-sin.magicum.net:23900",
	"tls://ygg.mkg20001.io:443",
}

type Message struct {
	ID   string `json:"id"`
	Peer string `json:"peer"` // remote ygg address (conversation key)
	From string `json:"from"` // sender ygg address
	Nick string `json:"nick"`
	Text string `json:"text"`
	TS   int64  `json:"ts"`
	Out  bool   `json:"out"`
	// Status of outgoing messages: sending | sent | queued (auto-resent when the peer is reachable) | failed.
	Status string `json:"status,omitempty"`
}

type Contact struct {
	Addr string `json:"addr"`
	Nick string `json:"nick"`
}

type Store struct {
	mu       sync.Mutex
	path     string
	Nick     string             `json:"nick"`
	Contacts map[string]Contact `json:"contacts"`
	Messages []Message          `json:"messages"`
}

func (s *Store) save() {
	b, _ := json.Marshal(s)
	_ = writeAtomic(s.path, b)
}

// writeAtomic writes via a 0600 temp file + fsync + rename so a crash never
// leaves a half-written file behind.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	_ = os.Chmod(path, 0600)
	return os.Rename(tmp, path)
}

var msgIDRe = regexp.MustCompile(`^[0-9A-Za-z_-]{1,40}$`)

// cleanNick drops control/bidi characters and caps the length of a peer-supplied nick.
func cleanNick(n string) string {
	n = strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) || (r >= 0x200b && r <= 0x200f) || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
			return -1
		}
		return r
	}, n)
	n = strings.TrimSpace(n)
	if r := []rune(n); len(r) > 32 {
		n = string(r[:32])
	}
	return n
}

// localOnly rejects requests that could come from other web pages or DNS
// rebinding: Host must be loopback, a present Origin must match it, and
// POST bodies must be application/json (which forces a CORS preflight).
func localOnly(uiAddr string, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if ip := net.ParseIP(host); !(host == "localhost" || (ip != nil && ip.IsLoopback())) {
			http.Error(w, "forbidden host", 403)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			if o := r.Header.Get("Origin"); o != "" {
				u, err := url.Parse(o)
				if err != nil || u.Host != r.Host {
					http.Error(w, "forbidden origin", 403)
					return
				}
			}
			if r.Method != "GET" && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				http.Error(w, "content-type must be application/json", 415)
				return
			}
			if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
				http.Error(w, "forbidden", 403)
				return
			}
		}
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		h.ServeHTTP(w, r)
	})
}

type App struct {
	core     *core.Core
	ns       *netstack.YggdrasilNetstack
	addr     string
	store    *Store
	subMu    sync.Mutex
	subs     map[chan []byte]struct{}
	udp      net.PacketConn
	ackMu    sync.Mutex
	acks     map[string]chan struct{}
	flMu     sync.Mutex
	inflight map[string]bool
	cfg      *config.NodeConfig
	cfgPath  string
}

func (a *App) broadcast(kind string, v any) {
	b, _ := json.Marshal(map[string]any{"type": kind, "data": v})
	a.subMu.Lock()
	defer a.subMu.Unlock()
	for c := range a.subs {
		select {
		case c <- b:
		default:
		}
	}
}

func (a *App) addMessage(m Message) {
	a.store.mu.Lock()
	a.store.Messages = append(a.store.Messages, m)
	if _, ok := a.store.Contacts[m.Peer]; !ok {
		a.store.Contacts[m.Peer] = Contact{Addr: m.Peer, Nick: m.Nick}
	} else if !m.Out && m.Nick != "" {
		c := a.store.Contacts[m.Peer]
		c.Nick = m.Nick
		a.store.Contacts[m.Peer] = c
	}
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("message", m)
}

// Wire format (shared with the browser/WASM node): one JSON datagram per UDP
// packet on port 7777. {"t":"msg","id","nick","text","ts"} -> {"t":"ack","id"}
type wire struct {
	T    string `json:"t"`
	ID   string `json:"id"`
	Nick string `json:"nick,omitempty"`
	Text string `json:"text,omitempty"`
	TS   int64  `json:"ts,omitempty"`
}

func (a *App) serveChat() {
	pc, err := a.ns.ListenUDP(&net.UDPAddr{IP: net.ParseIP(a.addr), Port: chatPort})
	if err != nil {
		log.Fatal("listen: ", err)
	}
	a.udp = pc
	seen := map[string]bool{}
	buf := make([]byte, 65535)
	for {
		n, raddr, err := pc.ReadFrom(buf)
		if err != nil {
			continue
		}
		var w wire
		if json.Unmarshal(buf[:n], &w) != nil {
			continue
		}
		// The source IP is cryptographically bound to the sender's key by Yggdrasil.
		from := raddr.(*net.UDPAddr).IP.String()
		switch w.T {
		case "hi":
			hb, _ := json.Marshal(wire{T: "hiack"})
			_, _ = pc.WriteTo(hb, raddr)
			go a.flush(from)
		case "hiack":
			go a.flush(from)
		case "ack":
			a.ackMu.Lock()
			if ch, ok := a.acks[w.ID]; ok {
				close(ch)
				delete(a.acks, w.ID)
			}
			a.ackMu.Unlock()
		case "msg":
			if w.Text == "" || len(w.Text) > 8000 || !msgIDRe.MatchString(w.ID) {
				continue
			}
			w.Nick = cleanNick(w.Nick)
			ack, _ := json.Marshal(wire{T: "ack", ID: w.ID})
			_, _ = pc.WriteTo(ack, raddr)
			key := from + "/" + w.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			a.addMessage(Message{ID: w.ID, Peer: from, From: from, Nick: w.Nick, Text: w.Text, TS: w.TS})
			go a.flush(from)
		}
	}
}

func (a *App) setStatus(peer, id, status string) {
	a.store.mu.Lock()
	for i := len(a.store.Messages) - 1; i >= 0; i-- {
		if m := &a.store.Messages[i]; m.ID == id && m.Peer == peer {
			m.Status = status
			break
		}
	}
	a.store.save()
	a.store.mu.Unlock()
	a.broadcast("status", map[string]string{"id": id, "peer": peer, "status": status})
}

// send stores the message as "sending" and delivers it in the background.
func (a *App) send(to, text string) (Message, error) {
	ip := net.ParseIP(to)
	if ip == nil || len(ip) != 16 || (ip[0]&0xfe) != 0x02 {
		return Message{}, fmt.Errorf("not a Yggdrasil address: %s", to)
	}
	if text == "" || len(text) > 8000 {
		return Message{}, fmt.Errorf("message empty or too long")
	}
	a.store.mu.Lock()
	nick := a.store.Nick
	a.store.mu.Unlock()
	m := Message{ID: fmt.Sprintf("%x", time.Now().UnixNano()), Peer: ip.String(), From: a.addr, Nick: nick, Text: text, TS: time.Now().Unix(), Out: true, Status: "sending"}
	a.addMessage(m)
	go a.deliver(m)
	return m, nil
}

func (a *App) retry(peer, id string) (Message, error) {
	a.store.mu.Lock()
	var found *Message
	for i := range a.store.Messages {
		if a.store.Messages[i].ID == id && a.store.Messages[i].Peer == peer && a.store.Messages[i].Out {
			c := a.store.Messages[i]
			found = &c
		}
	}
	a.store.mu.Unlock()
	if found == nil {
		return Message{}, fmt.Errorf("no such message")
	}
	a.setStatus(peer, id, "sending")
	found.Status = "sending"
	go a.deliver(*found)
	return *found, nil
}

func (a *App) deliver(m Message) { a.deliverN(m, 8) }

// deliverN sends m up to tries times; on no ack the message goes back to the
// offline queue. Returns true when acked.
func (a *App) deliverN(m Message, tries int) bool {
	a.flMu.Lock()
	if a.inflight[m.ID] {
		a.flMu.Unlock()
		return false
	}
	a.inflight[m.ID] = true
	a.flMu.Unlock()
	defer func() { a.flMu.Lock(); delete(a.inflight, m.ID); a.flMu.Unlock() }()
	if m.Status != "sending" {
		a.setStatus(m.Peer, m.ID, "sending")
	}
	b, _ := json.Marshal(wire{T: "msg", ID: m.ID, Nick: m.Nick, Text: m.Text, TS: m.TS})
	ch := make(chan struct{})
	a.ackMu.Lock()
	a.acks[m.ID] = ch
	a.ackMu.Unlock()
	defer func() { a.ackMu.Lock(); delete(a.acks, m.ID); a.ackMu.Unlock() }()
	for a.udp == nil {
		time.Sleep(200 * time.Millisecond)
	}
	dst := &net.UDPAddr{IP: net.ParseIP(m.Peer), Port: chatPort}
	// First packet to a new key triggers a key lookup, so retry a few times.
	for i := 0; i < tries; i++ {
		_, _ = a.udp.WriteTo(b, dst)
		select {
		case <-ch:
			a.setStatus(m.Peer, m.ID, "sent")
			return true
		case <-time.After(2500 * time.Millisecond):
		}
	}
	a.setStatus(m.Peer, m.ID, "queued")
	return false
}

func (a *App) queued(peer string) []Message {
	a.store.mu.Lock()
	defer a.store.mu.Unlock()
	var out []Message
	for _, m := range a.store.Messages {
		if m.Out && m.Status == "queued" && (peer == "" || m.Peer == peer) {
			out = append(out, m)
		}
	}
	return out
}

// flush re-sends everything queued for peer, oldest first; stops at the first miss.
func (a *App) flush(peer string) {
	for _, m := range a.queued(peer) {
		if !a.deliverN(m, 4) {
			return
		}
	}
}

func (a *App) hello(peer string) {
	if a.udp == nil {
		return
	}
	b, _ := json.Marshal(wire{T: "hi"})
	_, _ = a.udp.WriteTo(b, &net.UDPAddr{IP: net.ParseIP(peer), Port: chatPort})
}

func (a *App) helloAll() {
	a.store.mu.Lock()
	var ps []string
	for p := range a.store.Contacts {
		ps = append(ps, p)
	}
	a.store.mu.Unlock()
	for _, p := range ps {
		a.hello(p)
	}
}

type peerView struct {
	URI     string `json:"uri"`
	Up      bool   `json:"up"`
	Inbound bool   `json:"inbound"`
	Latency int64  `json:"latency_ms"`
	Error   string `json:"error,omitempty"`
	Saved   bool   `json:"saved"`
}

func (a *App) peerList() []peerView {
	saved := map[string]bool{}
	for _, p := range a.cfg.Peers {
		saved[p] = true
	}
	seen := map[string]bool{}
	out := []peerView{}
	for _, p := range a.core.GetPeers() {
		v := peerView{URI: p.URI, Up: p.Up, Inbound: p.Inbound, Latency: p.Latency.Milliseconds(), Saved: saved[p.URI]}
		if p.LastError != nil && !p.Up {
			v.Error = p.LastError.Error()
		}
		seen[p.URI] = true
		out = append(out, v)
	}
	for _, p := range a.cfg.Peers {
		if !seen[p] {
			out = append(out, peerView{URI: p, Saved: true})
		}
	}
	return out
}

func (a *App) upPeers() int {
	n := 0
	for _, p := range a.core.GetPeers() {
		if p.Up {
			n++
		}
	}
	return n
}

func (a *App) saveCfg() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	_ = writeAtomic(a.cfgPath, b)
}

func (a *App) setPeers(list []string) error {
	want := map[string]bool{}
	for _, p := range list {
		u, err := url.Parse(p)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("bad peer URI: %s", p)
		}
		want[p] = true
	}
	have := map[string]bool{}
	for _, p := range a.cfg.Peers {
		have[p] = true
	}
	for p := range have {
		if !want[p] {
			u, _ := url.Parse(p)
			_ = a.core.RemovePeer(u, "")
		}
	}
	for p := range want {
		if !have[p] {
			u, _ := url.Parse(p)
			_ = a.core.AddPeer(u, "")
		}
	}
	a.cfg.Peers = list
	a.saveCfg()
	return nil
}

var peerURIRe = regexp.MustCompile("`((?:tcp|tls|quic|ws|wss)://[^`\\s]+)`")

// publicPeers builds the list from the official github.com/yggdrasil-network/public-peers
// repo (one zip download), annotated with live status from the community crawler when reachable.
// Asia first, then by region; unreachable/flaky peers last.
func publicPeers() ([]map[string]any, error) {
	cl := &http.Client{Timeout: 20 * time.Second}
	r, err := cl.Get("https://codeload.github.com/yggdrasil-network/public-peers/zip/refs/heads/master")
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	r.Body.Close()
	if err != nil {
		return nil, err
	}
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		return nil, err
	}
	// Optional live status (crawler keyed by country file name).
	type st struct {
		Up     bool   `json:"up"`
		States string `json:"states"`
	}
	status := map[string]st{}
	if r2, err := (&http.Client{Timeout: 8 * time.Second}).Get("https://publicpeers.neilalexander.dev/publicnodes.json"); err == nil {
		var d map[string]map[string]st
		if json.NewDecoder(r2.Body).Decode(&d) == nil {
			for _, m := range d {
				for u, v := range m {
					status[u] = v
				}
			}
		}
		r2.Body.Close()
	}
	out := []map[string]any{}
	for _, f := range zr.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) != 3 || !strings.HasSuffix(parts[2], ".md") {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			continue
		}
		md, _ := io.ReadAll(rc)
		rc.Close()
		continent, country := parts[1], strings.TrimSuffix(parts[2], ".md")
		for _, m := range peerURIRe.FindAllStringSubmatch(string(md), -1) {
			uri := m[1]
			e := map[string]any{"uri": uri, "continent": continent, "region": country, "asia": continent == "asia"}
			if v, ok := status[uri]; ok {
				e["up"] = v.Up
				if len(v.States) > 0 {
					e["reliability"] = float64(strings.Count(v.States, "*")) / float64(len(v.States))
				}
			}
			out = append(out, e)
		}
	}
	score := func(e map[string]any) int {
		if up, ok := e["up"].(bool); ok && !up {
			return 2
		}
		if rel, ok := e["reliability"].(float64); ok && rel < 0.9 {
			return 1
		}
		return 0
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i]["asia"] != out[j]["asia"] {
			return out[i]["asia"].(bool)
		}
		if si, sj := score(out[i]), score(out[j]); si != sj {
			return si < sj
		}
		if out[i]["region"] != out[j]["region"] {
			return out[i]["region"].(string) < out[j]["region"].(string)
		}
		return out[i]["uri"].(string) < out[j]["uri"].(string)
	})
	return out, nil
}

func (a *App) httpHandlers(mux *http.ServeMux) {
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	mux.HandleFunc("/api/state", func(w http.ResponseWriter, r *http.Request) {
		a.store.mu.Lock()
		defer a.store.mu.Unlock()
		peers := a.upPeers()
		json.NewEncoder(w).Encode(map[string]any{
			"addr": a.addr, "pubkey": hex.EncodeToString(a.core.PublicKey()),
			"nick": a.store.Nick, "contacts": a.store.Contacts, "messages": a.store.Messages, "peers": peers, "platform": "native",
		})
	})
	mux.HandleFunc("/api/send", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ To, Text, Retry string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		var m Message
		var err error
		if req.Retry != "" {
			ip := net.ParseIP(strings.TrimSpace(req.To))
			if ip == nil {
				http.Error(w, "bad address", 400)
				return
			}
			m, err = a.retry(ip.String(), req.Retry)
		} else {
			m, err = a.send(strings.TrimSpace(req.To), req.Text)
		}
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("/api/contact", func(w http.ResponseWriter, r *http.Request) {
		var c Contact
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&c) != nil || net.ParseIP(c.Addr) == nil {
			http.Error(w, "bad request", 400)
			return
		}
		c.Addr = net.ParseIP(c.Addr).String()
		a.store.mu.Lock()
		a.store.Contacts[c.Addr] = c
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact", c)
		json.NewEncoder(w).Encode(c)
	})
	mux.HandleFunc("/api/contact/delete", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Addr string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil || net.ParseIP(req.Addr) == nil {
			http.Error(w, "bad request", 400)
			return
		}
		addr := net.ParseIP(req.Addr).String()
		a.store.mu.Lock()
		delete(a.store.Contacts, addr)
		kept := a.store.Messages[:0]
		for _, m := range a.store.Messages {
			if m.Peer != addr {
				kept = append(kept, m)
			}
		}
		a.store.Messages = kept
		a.store.save()
		a.store.mu.Unlock()
		a.broadcast("contact_del", map[string]string{"addr": addr})
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/peers", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct {
				Add, Remove string
				Reset       bool
			}
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				http.Error(w, "bad request", 400)
				return
			}
			list := append([]string{}, a.cfg.Peers...)
			switch {
			case req.Reset:
				list = append([]string{}, defaultPeers...)
			case req.Add != "":
				add := strings.TrimSpace(req.Add)
				dup := false
				for _, p := range list {
					dup = dup || p == add
				}
				if !dup {
					list = append(list, add)
				}
			case req.Remove != "":
				n := list[:0]
				for _, p := range list {
					if p != req.Remove {
						n = append(n, p)
					}
				}
				list = n
			}
			if err := a.setPeers(list); err != nil {
				http.Error(w, err.Error(), 400)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"peers": a.peerList(), "recommended": defaultPeers, "up": a.upPeers()})
	})
	mux.HandleFunc("/api/peers/public", func(w http.ResponseWriter, r *http.Request) {
		list, err := publicPeers()
		if err != nil {
			http.Error(w, "public peer list unavailable: "+err.Error(), 502)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"peers": list})
	})
	mux.HandleFunc("/api/nick", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Nick string }
		if r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		a.store.mu.Lock()
		a.store.Nick = req.Nick
		a.store.save()
		a.store.mu.Unlock()
		w.Write([]byte("{}"))
	})
	mux.HandleFunc("/api/events", func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "no stream", 500)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		ch := make(chan []byte, 32)
		a.subMu.Lock()
		a.subs[ch] = struct{}{}
		a.subMu.Unlock()
		defer func() { a.subMu.Lock(); delete(a.subs, ch); a.subMu.Unlock() }()
		tick := time.NewTicker(20 * time.Second)
		defer tick.Stop()
		for {
			select {
			case b := <-ch:
				fmt.Fprintf(w, "data: %s\n\n", b)
				fl.Flush()
			case <-tick.C:
				fmt.Fprint(w, ": ping\n\n")
				fl.Flush()
			case <-r.Context().Done():
				return
			}
		}
	})
}

func loadConfig(path string) *config.NodeConfig {
	cfg := config.GenerateConfig()
	if f, err := os.Open(path); err == nil {
		defer f.Close()
		if _, err := cfg.ReadFrom(f); err != nil {
			log.Fatal("config: ", err)
		}
		return cfg
	}
	cfg.AdminListen = "none"
	cfg.Peers = defaultPeers
	cfg.Listen = []string{}
	b, _ := json.MarshalIndent(cfg, "", "  ")
	_ = writeAtomic(path, b)
	return cfg
}

func main() {
	if runtime.GOOS == "android" {
		// Pure-Go resolver has no /etc/resolv.conf on Android: use public DNS.
		net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "udp", "1.1.1.1:53")
		}}
	}
	home, _ := os.UserHomeDir()
	dataDir := flag.String("data", filepath.Join(home, ".yggchat"), "data directory (keys, contacts, history)")
	uiAddr := flag.String("ui", "127.0.0.1:7780", "local web UI address")
	peers := flag.String("peers", "", "comma-separated Yggdrasil peer URIs (overrides config)")
	listen := flag.String("listen", "", "comma-separated Yggdrasil listen URIs, e.g. tls://0.0.0.0:12345")
	flag.Parse()
	_ = os.MkdirAll(*dataDir, 0700)
	_ = os.Chmod(*dataDir, 0700)
	for _, f := range []string{"node.json", "chat.json"} {
		_ = os.Chmod(filepath.Join(*dataDir, f), 0600)
	}

	cfg := loadConfig(filepath.Join(*dataDir, "node.json"))
	if *peers != "" {
		cfg.Peers = strings.Split(*peers, ",")
	}
	if *listen != "" {
		cfg.Listen = strings.Split(*listen, ",")
	}

	logger := gologme.New(os.Stderr, "[ygg] ", gologme.LstdFlags)
	logger.EnableLevel("error")
	logger.EnableLevel("warn")
	opts := []core.SetupOption{}
	for _, l := range cfg.Listen {
		opts = append(opts, core.ListenAddress(l))
	}
	for _, p := range cfg.Peers {
		opts = append(opts, core.Peer{URI: p})
	}
	c, err := core.New(cfg.Certificate, logger, opts...)
	if err != nil {
		log.Fatal(err)
	}
	ns, err := netstack.CreateYggdrasilNetstack(c)
	if err != nil {
		log.Fatal(err)
	}

	st := &Store{path: filepath.Join(*dataDir, "chat.json"), Contacts: map[string]Contact{}}
	if b, err := os.ReadFile(st.path); err == nil {
		_ = json.Unmarshal(b, st)
		if st.Contacts == nil {
			st.Contacts = map[string]Contact{}
		}
	}
	if st.Nick == "" {
		st.Nick = "anon"
	}

	a := &App{core: c, ns: ns, addr: c.Address().String(), store: st, subs: map[chan []byte]struct{}{}, acks: map[string]chan struct{}{}, inflight: map[string]bool{}, cfg: cfg, cfgPath: filepath.Join(*dataDir, "node.json")}
	// Messages left "sending" by a previous run never got an ack.
	for i := range st.Messages {
		if st.Messages[i].Status == "sending" || st.Messages[i].Status == "failed" {
			st.Messages[i].Status = "queued"
		}
	}
	go func() {
		last := -1
		for range time.Tick(3 * time.Second) {
			if n := a.upPeers(); n != last {
				if n > 0 && last <= 0 {
					go func() { time.Sleep(1500 * time.Millisecond); a.helloAll() }()
				}
				last = n
				a.broadcast("peers", map[string]int{"up": n})
			}
		}
	}()
	go a.serveChat()
	// Offline queue: probe peers we owe messages to every 20 s, and greet every
	// contact every 2 min so they can flush messages queued for us.
	go func() {
		tick := 0
		for range time.Tick(20 * time.Second) {
			tick++
			if a.upPeers() == 0 {
				continue
			}
			if tick%6 == 0 {
				a.helloAll()
				continue
			}
			seen := map[string]bool{}
			for _, m := range a.queued("") {
				if !seen[m.Peer] {
					seen[m.Peer] = true
					a.hello(m.Peer)
				}
			}
		}
	}()

	mux := http.NewServeMux()
	a.httpHandlers(mux)
	fmt.Printf("YggChat\n  your address: %s\n  open UI:      http://%s/\n", a.addr, *uiAddr)
	log.Fatal(http.ListenAndServe(*uiAddr, localOnly(*uiAddr, mux)))
}
