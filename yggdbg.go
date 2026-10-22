package main

// Diagnostics: YGGCHAT_YGGDBG=1 logs the Yggdrasil path/session/peer state
// once a second (when it changes) and the type of every chat datagram in and
// out, to debug delivery between nodes (r3: lost datagrams after a restart).
// Off by default; it logs peer addresses, never message contents.

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/address"
)

var yggDbg = os.Getenv("YGGCHAT_YGGDBG") != ""

func dbgType(b []byte) string {
	var w struct {
		T  string `json:"t"`
		ID string `json:"id"`
		G  string `json:"g"`
	}
	_ = json.Unmarshal(b, &w)
	s := w.T
	if w.ID != "" {
		s += " id=" + w.ID
	}
	if w.G != "" && len(w.G) > 6 {
		s += " g=" + w.G[:6]
	}
	return s
}

func (a *App) dbgLoop() {
	if !yggDbg || a.core == nil {
		return
	}
	last := ""
	for range time.Tick(time.Second) {
		var parts []string
		for _, p := range a.core.GetPaths() {
			parts = append(parts, fmt.Sprintf("path %s seq=%d %v", net.IP(address.AddrForKey(p.Key)[:]), p.Sequence, p.Path))
		}
		for _, s := range a.core.GetSessions() {
			parts = append(parts, fmt.Sprintf("sess %s up=%ds", net.IP(address.AddrForKey(s.Key)[:]), int(s.Uptime.Seconds())))
		}
		for _, p := range a.core.GetPeers() {
			k := hex.EncodeToString(p.Key)
			if len(k) > 8 {
				k = k[:8]
			}
			parts = append(parts, fmt.Sprintf("peer %s up=%v port=%d", k, p.Up, p.Port))
		}
		sort.Strings(parts)
		// uptime changes every second: compare without it
		cmp := strings.Join(parts, "|")
		key := ""
		for _, p := range parts {
			if i := strings.Index(p, " up="); i > 0 && strings.HasPrefix(p, "sess") {
				p = p[:i]
			}
			key += p + "|"
		}
		if key != last {
			last = key
			log.Printf("[yggdbg] state: %s", cmp)
		}
	}
}
