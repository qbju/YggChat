//go:build linux

// SPDX-License-Identifier: GPL-3.0-or-later
// Copyright (C) 2026 h6e and YggChat contributors

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
	_, oobn, flags, _, err := c.ReadMsgUnix(buf, oob)
	if err != nil {
		return
	}
	msgs, err := syscall.ParseSocketControlMessage(oob[:oobn])
	if err != nil {
		return
	}
	// Collect every passed fd so none leaks; use exactly one.
	var fds []int
	for i := range msgs {
		if got, err := syscall.ParseUnixRights(&msgs[i]); err == nil {
			fds = append(fds, got...)
		}
	}
	if len(fds) != 1 || flags&syscall.MSG_CTRUNC != 0 {
		for _, fd := range fds {
			syscall.Close(fd)
		}
		return
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
