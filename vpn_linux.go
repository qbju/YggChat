//go:build linux

package main

import (
	"net"
	"os"
	"syscall"
	"time"
)

// accept takes a TUN fd from our own app (same uid only).
func (v *vpnState) accept(c *net.UnixConn) {
	defer c.Close()
	raw, err := c.SyscallConn()
	if err != nil {
		return
	}
	var cred *syscall.Ucred
	_ = raw.Control(func(fd uintptr) { cred, _ = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED) })
	if cred == nil || int(cred.Uid) != os.Getuid() {
		return
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	buf, oob := make([]byte, 16), make([]byte, syscall.CmsgSpace(4*4))
	_, oobn, _, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil || len(msgs) == 0 {
		return
	}
	fds, err := syscall.ParseUnixRights(&msgs[0])
	if err != nil || len(fds) == 0 {
		return
	}
	for _, extra := range fds[1:] {
		syscall.Close(extra)
	}
	f := os.NewFile(uintptr(fds[0]), "tun")
	v.mu.Lock()
	if v.tun != nil {
		v.tun.Close()
	}
	v.tun, v.at = f, time.Now()
	v.mu.Unlock()
	_, _ = c.Write([]byte{1})
	go v.toCore(f)
}

