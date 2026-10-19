// Android VPN mode: YggChat acts as the device's Yggdrasil (like the official
// app). The Java VpnService creates a TUN for 200::/7 and hands its fd to this
// process over an abstract unix socket; packets are pumped between the TUN and
// the embedded core. Chat itself then uses the OS stack, like system mode, but
// with YggChat's own key and address.
package main

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/yggdrasil-network/yggdrasil-go/src/ipv6rwc"
)

const vpnMTU = 1280

func vpnModeFile(dataDir string) string { return filepath.Join(dataDir, "vpn-mode") }

func vpnModeOn(dataDir string) bool {
	if runtime.GOOS != "android" && os.Getenv("YGGCHAT_VPN") != "1" {
		return false
	}
	_, err := os.Stat(vpnModeFile(dataDir))
	return err == nil || os.Getenv("YGGCHAT_VPN") == "1"
}

type vpnState struct {
	mu  sync.Mutex
	rwc *ipv6rwc.ReadWriteCloser
	tun *os.File
	at  time.Time
	in, out, werr int64 // packets tun→core, core→tun, write errors
	lastErr       string
}

func (a *App) startVPN() {
	a.vpnS = &vpnState{rwc: ipv6rwc.NewReadWriteCloser(a.core)}
	a.vpnS.rwc.SetMTU(vpnMTU)
	go a.vpnS.fromCore()
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: "@yggchat-tun", Net: "unix"})
	if err != nil {
		log.Println("vpn: listen:", err)
		return
	}
	go func() {
		for {
			c, err := ln.AcceptUnix()
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			a.vpnS.accept(c)
		}
	}()
}

func (v *vpnState) cur() *os.File {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.tun
}

// toCore: packets the phone sends to 200::/7 → Yggdrasil.
func (v *vpnState) toCore(f *os.File) {
	buf := make([]byte, 65535)
	for {
		n, err := f.Read(buf)
		if err != nil {
			v.mu.Lock()
			if v.tun == f {
				v.tun = nil
			}
			v.mu.Unlock()
			return
		}
		if n > 0 && buf[0]>>4 == 6 {
			v.mu.Lock()
			v.in++
			v.mu.Unlock()
			portMap(buf[:n], 8, 40, fingerAltPort, fingerPort) // reply from our finger listener
			if _, err := v.rwc.Write(buf[:n]); err != nil {
				v.mu.Lock()
				v.werr++
				v.lastErr = err.Error()
				v.mu.Unlock()
			}
		}
	}
}

// fromCore: packets arriving from Yggdrasil → the phone.
func (v *vpnState) fromCore() {
	buf := make([]byte, 65535)
	for {
		n, err := v.rwc.Read(buf)
		if err != nil {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if f := v.cur(); f != nil {
			portMap(buf[:n], 24, 42, fingerPort, fingerAltPort) // finger to us -> OS port 7979
			_, _ = f.Write(buf[:n])
			v.mu.Lock()
			v.out++
			v.mu.Unlock()
		}
	}
}

func (a *App) vpnHandlers(mux *http.ServeMux, dataDir string) {
	mux.HandleFunc("/api/vpn", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			var req struct{ On bool }
			if !readJSON(w, r, &req) {
				return
			}
			if req.On && runtime.GOOS != "android" {
				http.Error(w, "VPN mode is Android only", 400)
				return
			}
			if req.On == a.vpn {
				json.NewEncoder(w).Encode(map[string]any{"on": a.vpn, "restart": false})
				return
			}
			if req.On {
				_ = os.Remove(sysModeFile(dataDir))
				if err := writeAtomic(vpnModeFile(dataDir), []byte("1\n")); err != nil {
					http.Error(w, err.Error(), 500)
					return
				}
			} else {
				_ = os.Remove(vpnModeFile(dataDir))
			}
			json.NewEncoder(w).Encode(map[string]any{"on": req.On, "restart": true})
			go func() {
				time.Sleep(300 * time.Millisecond)
				restartSelf()
			}()
			return
		}
		res := map[string]any{"on": a.vpn, "tun": false, "addr": a.addr, "mtu": vpnMTU}
		if v := a.vpnS; v != nil {
			v.mu.Lock()
			res["tun"], res["in"], res["out"], res["werr"], res["err"] = v.tun != nil, v.in, v.out, v.werr, v.lastErr
			v.mu.Unlock()
		}
		json.NewEncoder(w).Encode(res)
	})
}

// portMap rewrites a TCP port in an IPv6 packet (no extension headers) when it
// equals from, fixing the checksum incrementally (RFC 1624). Android apps can't
// bind port 79, so the finger listener sits on 7979 and the pump maps it.
// addrOff selects which address must be ours (8 = src, 24 = dst); portOff is
// the byte offset of the TCP port (40 = src, 42 = dst).
func portMap(p []byte, addrOff, portOff int, from, to uint16) {
	if len(p) < 60 || p[0]>>4 != 6 || p[6] != 6 {
		return
	}
	if uint16(p[portOff])<<8|uint16(p[portOff+1]) != from {
		return
	}
	if (p[addrOff] & 0xfe) != 0x02 {
		return
	}
	p[portOff], p[portOff+1] = byte(to>>8), byte(to)
	sum := uint32(^(uint16(p[56])<<8 | uint16(p[57])))
	sum += uint32(^from) + uint32(to)
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	c := ^uint16(sum)
	p[56], p[57] = byte(c>>8), byte(c)
}
