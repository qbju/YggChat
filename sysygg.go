// System Yggdrasil mode: instead of the embedded node, use a Yggdrasil that is
// already running on the device as a VPN/TUN (yggdrasil-android, the desktop
// yggdrasil service). The chat identity is then that node's address; YggChat
// holds no Yggdrasil key, so the full public key (and Nostr boxes) is unavailable.
package main

import (
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var sysMu sync.RWMutex

func sysModeFile(dataDir string) string { return filepath.Join(dataDir, "system-ygg") }

func sysModeOn(dataDir string) bool {
	if os.Getenv("YGGCHAT_SYSTEM") == "1" {
		return true
	}
	_, err := os.Stat(sysModeFile(dataDir))
	return err == nil
}

// detectSysAddr asks the kernel which source address it would use to reach
// 200::/7. That works without listing interfaces (Android 11+ forbids that).
func detectSysAddr() string {
	c, err := net.DialUDP("udp6", nil, &net.UDPAddr{IP: net.ParseIP("200::1"), Port: chatPort})
	if err != nil {
		return ""
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if len(ip) == 16 && (ip[0]&0xfe) == 0x02 {
		return ip.String()
	}
	return ""
}

func (a *App) myAddr() string {
	sysMu.RLock()
	defer sysMu.RUnlock()
	return a.addr
}

// watchSys keeps a.addr in step with the system node (VPN may start later).
func (a *App) watchSys() {
	for {
		ad := detectSysAddr()
		sysMu.Lock()
		changed := ad != a.addr
		a.addr = ad
		sysMu.Unlock()
		if changed {
			up := 0
			if ad != "" {
				up = 1
				go a.helloAll()
			}
			a.broadcast("peers", map[string]any{"up": up, "total": 1})
		}
		time.Sleep(5 * time.Second)
	}
}

func (a *App) pub() []byte {
	if a.sys {
		return nil
	}
	return a.core.PublicKey()
}

func (a *App) systemHandlers(mux *http.ServeMux, dataDir string) {
	mux.HandleFunc("/api/system", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct{ On bool }
			if !readJSON(w, r, &req) {
				return
			}
			if req.On {
				_ = os.Remove(vpnModeFile(dataDir))
				if err := writeAtomic(sysModeFile(dataDir), []byte("1\n")); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else {
				_ = os.Remove(sysModeFile(dataDir))
			}
			json.NewEncoder(w).Encode(map[string]any{"on": req.On, "restart": true})
			go func() {
				time.Sleep(300 * time.Millisecond)
				restartSelf()
			}()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"on": a.sys, "detected": detectSysAddr()})
	})
}
